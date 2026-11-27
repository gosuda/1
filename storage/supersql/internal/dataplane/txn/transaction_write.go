package txn

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sort"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func (transaction *Transaction) target(ctx context.Context, target storage.ObjectRef) (transactionTarget, error) {
	for _, cached := range transaction.targets {
		if cached.layout.Target == target {
			return cached, nil
		}
	}
	if err := transaction.lockWriteIntent(ctx, target); err != nil {
		return transactionTarget{}, err
	}
	normalizer, err := transaction.resolver.ResolveMutation(ctx, target)
	if err != nil {
		return transactionTarget{}, err
	}
	if normalizer == nil {
		return transactionTarget{}, storage.ErrInvalidTarget
	}
	return transaction.cacheTarget(ctx, target, normalizer, normalizer.RetainedBytes())
}

// AdmitCreatedTarget transfers a new relation's immutable policy to this
// transaction. The schema owner must first protect namespace/identity absence and
// stage its catalog definition in this statement. Admission rolls back with that
// statement or savepoint; it never authorizes an existing published relation.
// retainedBytes admits the policy and its compiled programs until rollback or
// transaction completion; the caller supplies their bounded retained footprint.
func (transaction *Transaction) AdmitCreatedTarget(ctx context.Context, normalizer *RowNormalizer, retainedBytes uint64) error {
	operation, finish, _, err := transaction.enter(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if err := transaction.active(operation); err != nil {
		return err
	}
	if transaction.mode != ReadWrite {
		return ErrReadOnlyTransaction
	}
	if normalizer == nil || transaction.statementDepth == 0 {
		return storage.ErrInvalidTarget
	}
	if retainedBytes < normalizer.RetainedBytes() || retainedBytes > transaction.metadataLimit-transaction.metadataUsed {
		return errTransactionBudget
	}
	for _, cached := range transaction.targets {
		if cached.layout.Target.ObjectID != normalizer.target.ObjectID {
			continue
		}
		if cached.layout.Target == normalizer.target {
			// A generation rewrite resolves its new physical target through the
			// plan resolver before the schema owner stages admission; admitting
			// the identical target again is a no-op.
			return nil
		}
		return storage.ErrInvalidTarget
	}
	if err := transaction.lockWriteIntent(operation, normalizer.target); err != nil {
		return err
	}
	_, err = transaction.cacheTarget(operation, normalizer.target, normalizer, retainedBytes)
	return err
}

func (transaction *Transaction) cacheTarget(ctx context.Context, target storage.ObjectRef, normalizer *RowNormalizer, retainedBytes uint64) (transactionTarget, error) {
	if normalizer == nil || normalizer.target != target {
		return transactionTarget{}, storage.ErrInvalidTarget
	}
	memory := overlayMemory{limit: transaction.metadataLimit - transaction.metadataUsed}
	if err := memory.add(retainedBytes, 1); err != nil {
		return transactionTarget{}, errTransactionBudget
	}
	if err := memory.add(2, uint64(unsafe.Sizeof(transactionTarget{}))); err != nil {
		return transactionTarget{}, errTransactionBudget
	}
	if err := memory.add(uint64(len(normalizer.columns)), uint64(unsafe.Sizeof(storage.ColumnLayout{}))); err != nil {
		return transactionTarget{}, errTransactionBudget
	}
	layout := storage.ObjectLayout{Target: target, Columns: make([]storage.ColumnLayout, len(normalizer.columns))}
	for i, rule := range normalizer.columns {
		column := rule.Layout
		owned, used, err := model.CopyTypeLayout(ctx, model.TypeLayout{Type: column.Type, Children: column.Children}, memory.limit-memory.used)
		if err != nil {
			return transactionTarget{}, errors.Join(errTransactionBudget, err)
		}
		memory.used += used
		column.Children = owned.Children
		if column.HasMissingValue {
			value, used, err := model.CopyValue(ctx, column.MissingValue, memory.limit-memory.used)
			if err != nil {
				return transactionTarget{}, errors.Join(errTransactionBudget, err)
			}
			memory.used += used
			column.MissingValue = value
		}
		layout.Columns[i] = column
	}
	cached := transactionTarget{normalizer: normalizer, layout: layout, bytes: memory.used - 2*uint64(unsafe.Sizeof(transactionTarget{}))}
	transaction.metadataUsed += memory.used
	transaction.targets = append(transaction.targets, cached)
	return cached, nil
}

func copyTransactionMutation(ctx context.Context, source storage.Mutation, budget uint64) (storage.Mutation, error) {
	memory := overlayMemory{limit: budget}
	if err := memory.add(uint64(len(source.Key))+uint64(len(source.EndKey)), 1); err != nil {
		return storage.Mutation{}, errTransactionBudget
	}
	if err := memory.add(uint64(len(source.ColumnIDs)), 4); err != nil {
		return storage.Mutation{}, errTransactionBudget
	}
	if err := memory.add(uint64(len(source.Values)), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
		return storage.Mutation{}, errTransactionBudget
	}
	owned := source
	owned.Key, owned.EndKey = bytes.Clone(source.Key), bytes.Clone(source.EndKey)
	owned.ColumnIDs = slices.Clone(source.ColumnIDs)
	owned.Values = make([]model.Value, len(source.Values))
	for i, value := range source.Values {
		copy, used, err := model.CopyValue(ctx, value, memory.limit-memory.used)
		if err != nil {
			return storage.Mutation{}, errors.Join(errTransactionBudget, err)
		}
		memory.used += used
		owned.Values[i] = copy
	}
	return owned, nil
}

func (transaction *Transaction) previous(ctx context.Context, target transactionTarget, key []byte) (*model.Row, error) {
	if !transaction.reserveResource() {
		return nil, errTransactionBudget
	}
	defer transaction.resources.Add(-1)
	result, err := transaction.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	reader := result.reader
	columns := make([]model.ColumnID, len(target.layout.Columns))
	for i, column := range target.layout.Columns {
		columns[i] = column.ID
	}
	row, getErr := reader.Get(ctx, storage.ReadRequest{Target: target.layout.Target, Key: key, Layout: target.layout.Columns, ColumnIDs: columns})
	closeErr := reader.Close()
	if errors.Is(getErr, storage.ErrRowNotFound) {
		return nil, closeErr
	}
	if err := errors.Join(getErr, closeErr); err != nil {
		return nil, err
	}
	return &row, nil
}

func (transaction *Transaction) ApplyMutations(ctx context.Context, mutations []storage.Mutation) error {
	operation, finish, _, err := transaction.enter(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if err := transaction.active(operation); err != nil {
		return err
	}
	if transaction.mode != ReadWrite {
		return ErrReadOnlyTransaction
	}
	// Recursive expression writes share the root statement's rollback boundary.
	position, err := transaction.checkpoint()
	if err != nil {
		return transactionError(err)
	}
	if transaction.statementDepth >= 64 {
		return errTransactionBudget
	}
	transaction.statementDepth++
	defer func() { transaction.statementDepth-- }()
	for _, mutation := range mutations {
		if err = transaction.active(operation); err != nil {
			break
		}
		err = transaction.applyOne(operation, mutation)
		if err != nil {
			break
		}
	}
	if err == nil {
		err = transaction.active(operation)
	}
	if err == nil {
		return nil
	}
	return transaction.rollbackStatement(operation, position, err)
}

func (transaction *Transaction) applyOne(ctx context.Context, borrowed storage.Mutation) error {
	budget := (transaction.scratchBytes - transaction.scratchUsed) / 2
	if budget < 1024 {
		return errTransactionBudget
	}
	transaction.scratchUsed += budget
	defer func() { transaction.scratchUsed -= budget }()
	mutation, err := copyTransactionMutation(ctx, borrowed, budget)
	if err != nil {
		return err
	}
	if err := mutation.ValidateShape(); err != nil {
		return err
	}
	target, err := transaction.target(ctx, mutation.Target)
	if err != nil {
		return err
	}
	if mutation.Op == storage.MutationDeleteRange {
		if resolver, ok := transaction.resolver.(DeleteRangeResolver); ok {
			required, resolveErr := resolver.RequiresDeleteRangeExpansion(ctx, mutation.Target)
			if resolveErr != nil {
				return resolveErr
			}
			if required {
				return ErrDeleteRangeExpansion
			}
		}
	}
	request := pointLock(mutation.Target, mutation.Key)
	if mutation.Op == storage.MutationDeleteRange {
		request.Upper = lockBound{Key: mutation.EndKey}
	}
	if _, err := transaction.manager.locks.acquire(ctx, transaction.owner, request, lockUpdate, lockWait); err != nil {
		return transactionError(err)
	}
	var previous *model.Row
	if mutation.Op == storage.MutationDeleteRange {
		if err := transaction.enumerateRange(ctx, target, mutation, budget); err != nil {
			return err
		}
	} else {
		previous, err = transaction.previous(ctx, target, mutation.Key)
		if err != nil {
			return err
		}
	}
	// The immutable resolver's own operation bound cannot exceed this transaction's
	// statement allowance, including recursive default evaluation depth.
	normalizer := *target.normalizer
	normalizer.maxBytes = min(normalizer.maxBytes, budget)
	normalized, err := normalizer.normalizeOwned(ctx, mutation, previous)
	if err != nil {
		return err
	}
	if err := transaction.validateMutationKey(ctx, target.normalizer, normalized, budget); err != nil {
		return err
	}
	if normalized.Op == storage.MutationUpsert {
		if err := target.normalizer.persistableMutation(&normalized); err != nil {
			return err
		}
	}
	return transaction.stageNormalized(ctx, normalized)
}

func (transaction *Transaction) stageNormalized(ctx context.Context, normalized storage.Mutation) error {
	if transaction.ordinal == ^uint64(0) {
		return errTransactionBudget
	}
	transaction.ordinal++
	normalized.Ordinal = transaction.ordinal
	if err := transaction.overlay.StageOwned(ctx, []storage.Mutation{normalized}); err != nil {
		return err
	}
	return transaction.log.append(ctx, []storage.Mutation{normalized})
}

func (transaction *Transaction) encodeMutationKey(ctx context.Context, normalizer *RowNormalizer, mutation storage.Mutation, budget uint64) ([]byte, error) {
	keys := normalizer.keyLayout
	if len(keys) == 0 {
		return nil, storage.ErrInvalidLayout
	}
	used, err := mutationMemory(mutation, budget)
	if err != nil {
		return nil, err
	}
	memory := overlayMemory{limit: budget, used: used}
	first := sort.Search(len(mutation.ColumnIDs), func(i int) bool { return mutation.ColumnIDs[i] >= keys[0].ColumnID })
	contiguous := first+len(keys) <= len(mutation.ColumnIDs)
	for i := 0; contiguous && i < len(keys); i++ {
		contiguous = mutation.ColumnIDs[first+i] == keys[i].ColumnID
	}
	var values []model.Value
	if contiguous {
		values = mutation.Values[first : first+len(keys)]
	} else {
		if err := memory.add(uint64(len(keys)), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
			return nil, err
		}
		values = make([]model.Value, len(keys))
		for i, key := range keys {
			position := sort.Search(len(mutation.ColumnIDs), func(j int) bool { return mutation.ColumnIDs[j] >= key.ColumnID })
			if position == len(mutation.ColumnIDs) || mutation.ColumnIDs[position] != key.ColumnID {
				return nil, storage.ErrInvalidLayout
			}
			values[i] = mutation.Values[position]
		}
	}
	encoded, err := transaction.encoder.EncodePrefix(ctx, keys, values, memory.limit-memory.used)
	if err != nil {
		return nil, err
	}
	if uint64(cap(encoded)) > memory.limit-memory.used {
		return nil, errTransactionBudget
	}
	return encoded, nil
}

func (transaction *Transaction) validateMutationKey(ctx context.Context, normalizer *RowNormalizer, mutation storage.Mutation, budget uint64) error {
	if mutation.Op != storage.MutationUpsert || len(normalizer.keyLayout) == 0 {
		return nil
	}
	encoded, err := transaction.encodeMutationKey(ctx, normalizer, mutation, budget)
	if err != nil {
		return err
	}
	if !bytes.Equal(encoded, mutation.Key) {
		return storage.ErrInvalidMutation
	}
	return nil
}

package txn

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// InsertPreparation borrows values only for PrepareInsert. MaxBytes and Allocator
// are required so candidate memory remains admitted after preparation returns.
type InsertPreparation struct {
	Target      storage.ObjectRef
	ColumnIDs   []model.ColumnID
	Values      []model.Value
	Allocator   columnar.Allocator
	MaxBytes    uint64
	Expressions model.ExpressionRuntime
}

// InsertCandidate is a sealed complete row. Copies share its single-use identity
// and Close state. The caller must Close it, including after successful apply.
type InsertCandidate struct{ state *rowCandidateState }

type rowCandidateState struct {
	mu              sync.Mutex
	transaction     *Transaction
	mutation        storage.Mutation
	originalKey     []byte
	operation       storage.MutationOp
	claim           columnar.Allocation
	applied, closed bool
}

func (candidate *InsertCandidate) Close() error {
	if candidate == nil || candidate.state == nil {
		return nil
	}
	return candidate.state.close()
}

func (state *rowCandidateState) close() error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil
	}
	state.closed = true
	state.transaction = nil
	state.mutation = storage.Mutation{}
	state.originalKey = nil
	if state.claim != nil {
		state.claim.Release()
		state.claim = nil
	}
	return nil
}

// CopyRow returns independent payloads; the caller owns and accounts for used
// bytes until the returned row is discarded. It remains valid after Close.
func (candidate *InsertCandidate) CopyRow(ctx context.Context, maxBytes uint64) (model.Row, uint64, error) {
	if candidate == nil || candidate.state == nil {
		return model.Row{}, 0, storage.ErrInvalidMutation
	}
	return candidate.state.copyRow(ctx, maxBytes)
}

func (state *rowCandidateState) copyRow(ctx context.Context, maxBytes uint64) (model.Row, uint64, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return model.Row{}, 0, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return model.Row{}, 0, err
	}
	memory := overlayMemory{limit: maxBytes}
	if err := memory.add(uint64(len(state.mutation.Key)), 1); err != nil {
		return model.Row{}, 0, err
	}
	if err := memory.add(uint64(len(state.mutation.Values)), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
		return model.Row{}, 0, err
	}
	row := model.Row{Key: bytes.Clone(state.mutation.Key), Values: make([]model.Value, len(state.mutation.Values))}
	for i, value := range state.mutation.Values {
		owned, used, err := model.CopyValue(ctx, value, memory.limit-memory.used)
		if err != nil {
			return model.Row{}, 0, err
		}
		memory.used += used
		row.Values[i] = owned
	}
	return row, memory.used, nil
}

func (transaction *Transaction) PrepareInsert(ctx context.Context, preparation InsertPreparation) (*InsertCandidate, error) {
	state, err := transaction.prepareRow(ctx, preparation, nil)
	if err != nil {
		return nil, err
	}
	return &InsertCandidate{state: state}, nil
}

func (transaction *Transaction) prepareRow(ctx context.Context, preparation InsertPreparation, previous *model.Row) (state *rowCandidateState, err error) {
	operation, finish, _, err := transaction.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	if err := transaction.active(operation); err != nil {
		return nil, err
	}
	if transaction.mode != ReadWrite {
		return nil, ErrReadOnlyTransaction
	}
	if preparation.Allocator == nil {
		return nil, storage.ErrInvalidMutation
	}
	budget := min(preparation.MaxBytes, (transaction.scratchBytes-transaction.scratchUsed)/2)
	header := uint64(unsafe.Sizeof(InsertCandidate{})) + uint64(unsafe.Sizeof(rowCandidateState{}))
	if budget <= header || transaction.statementDepth >= 64 {
		return nil, errTransactionBudget
	}
	claim, err := preparation.Allocator.Reserve(budget)
	if err != nil {
		return nil, err
	}
	defer func() {
		if claim != nil {
			claim.Release()
		}
	}()
	transaction.scratchUsed += budget
	defer func() { transaction.scratchUsed -= budget }()
	position, err := transaction.checkpoint()
	if err != nil {
		return nil, transactionError(err)
	}
	defer func() {
		if err != nil {
			err = transaction.rollbackStatement(operation, position, err)
		}
	}()
	transaction.statementDepth++
	defer func() { transaction.statementDepth-- }()
	target, err := transaction.target(operation, preparation.Target)
	if err != nil {
		return nil, err
	}
	if len(target.normalizer.keyLayout) == 0 {
		return nil, storage.ErrInvalidLayout
	}
	// Reserve both completion workspace and key-encoding workspace before either
	// phase allocates; completed payloads coexist with the encoder's scratch.
	work := (budget - header) / 2
	input := storage.Mutation{Target: preparation.Target, Op: storage.MutationInsert, ColumnIDs: preparation.ColumnIDs, Values: preparation.Values}
	if previous != nil {
		physicalColumns := target.normalizer.physicalColumnCount()
		if previous.Deleted || len(previous.Values) != len(target.normalizer.columns) && len(previous.Values) != physicalColumns {
			return nil, storage.ErrInvalidLayout
		}
		input.Op, input.Key = storage.MutationUpdate, previous.Key
	}
	mutation, err := copyTransactionMutation(operation, input, work)
	if err != nil {
		return nil, err
	}
	used, err := mutationMemory(mutation, work)
	if err != nil {
		return nil, err
	}
	normalizer := *target.normalizer
	normalizer.maxBytes = min(normalizer.maxBytes, work)
	normalizer.expressions = preparation.Expressions
	if used > normalizer.maxBytes {
		return nil, errTransactionBudget
	}
	var originalKey []byte
	if previous == nil {
		for _, rule := range normalizer.columns {
			if !rule.SystemIdentity {
				continue
			}
			if transaction.rowIdentitySerial == math.MaxUint64 {
				return nil, errTransactionBudget
			}
			transaction.rowIdentitySerial++
			copy(normalizer.identity[:16], transaction.id[:])
			binary.BigEndian.PutUint64(normalizer.identity[16:], transaction.rowIdentitySerial)
			normalizer.hasIdentity = true
			break
		}
		mutation, err = normalizer.completeOwned(operation, mutation, nil, overlayMemory{limit: normalizer.maxBytes, used: used})
	} else {
		old, copyErr := copyTransactionMutation(operation, storage.Mutation{Values: previous.Values}, normalizer.maxBytes-used)
		if copyErr != nil {
			return nil, copyErr
		}
		// The original key is owned by the candidate; OLD borrows it only until
		// completion has transferred its selected, independently copied values.
		originalKey = mutation.Key
		mutation, err = normalizer.normalizeOwned(operation, mutation, &model.Row{Key: originalKey, Values: old.Values})
	}
	if err != nil {
		return nil, err
	}
	mutation.Key, err = transaction.encodeMutationKey(operation, target.normalizer, mutation, work)
	if err != nil {
		return nil, err
	}
	if err = mutation.ValidateShape(); err != nil {
		return nil, err
	}
	if err = transaction.active(operation); err != nil {
		return nil, err
	}
	used, err = mutationMemory(mutation, budget-header)
	if err != nil {
		return nil, err
	}
	if uint64(cap(originalKey)) > budget-header-used {
		return nil, errTransactionBudget
	}
	used += uint64(cap(originalKey))
	if claim != nil {
		if err = claim.Resize(header + used); err != nil {
			return nil, err
		}
	}
	state = &rowCandidateState{transaction: transaction, mutation: mutation, originalKey: originalKey, operation: input.Op, claim: claim}
	claim = nil
	return state, nil
}

func (transaction *Transaction) ApplyPreparedInsert(ctx context.Context, candidate *InsertCandidate) error {
	if candidate == nil || candidate.state == nil || candidate.state.operation != storage.MutationInsert {
		return storage.ErrInvalidMutation
	}
	return transaction.applyPreparedRow(ctx, candidate.state)
}

func (transaction *Transaction) applyPreparedRow(ctx context.Context, state *rowCandidateState) (err error) {
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
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return storage.ErrClosed
	}
	if state.transaction != transaction || state.applied {
		return storage.ErrInvalidMutation
	}
	budget := (transaction.scratchBytes - transaction.scratchUsed) / 2
	if budget < 1024 || transaction.statementDepth >= 64 {
		return errTransactionBudget
	}
	transaction.scratchUsed += budget
	defer func() { transaction.scratchUsed -= budget }()
	position, err := transaction.checkpoint()
	if err != nil {
		return transactionError(err)
	}
	defer func() {
		if err != nil {
			err = transaction.rollbackStatement(operation, position, err)
		}
	}()
	transaction.statementDepth++
	defer func() { transaction.statementDepth-- }()
	target, err := transaction.target(operation, state.mutation.Target)
	if err != nil {
		return err
	}
	originalKey := state.mutation.Key
	if state.operation == storage.MutationUpdate {
		originalKey = state.originalKey
	}
	first, second := originalKey, state.mutation.Key
	moved := !bytes.Equal(first, second)
	if bytes.Compare(first, second) > 0 {
		first, second = second, first
	}
	if _, err = transaction.manager.locks.acquire(operation, transaction.owner, pointLock(state.mutation.Target, first), lockUpdate, lockWait); err != nil {
		return transactionError(err)
	}
	if moved {
		if _, err = transaction.manager.locks.acquire(operation, transaction.owner, pointLock(state.mutation.Target, second), lockUpdate, lockWait); err != nil {
			return transactionError(err)
		}
	}
	previous, err := transaction.previous(operation, target, originalKey)
	if err != nil {
		return err
	}
	if state.operation == storage.MutationInsert {
		if previous != nil {
			return &model.Error{Code: "23505", Message: "row already exists", Cause: storage.ErrInvalidMutation}
		}
	} else if previous == nil {
		return storage.ErrRowNotFound
	}
	if moved {
		existing, lookupErr := transaction.previous(operation, target, state.mutation.Key)
		if lookupErr != nil {
			return lookupErr
		}
		if existing != nil {
			return &model.Error{Code: "23505", Message: "row already exists", Cause: storage.ErrInvalidMutation}
		}
	}
	// The overlay takes ownership while the candidate remains available for
	// RETURNING/EXCLUDED. Never expose or transfer the sealed candidate's slices.
	mutation, err := copyTransactionMutation(operation, state.mutation, budget)
	if err != nil {
		return err
	}
	if err = transaction.validateMutationKey(operation, target.normalizer, mutation, budget); err != nil {
		return err
	}
	if err = target.normalizer.persistableMutation(&mutation); err != nil {
		return err
	}
	if moved {
		used, memoryErr := mutationMemory(mutation, budget)
		if memoryErr != nil {
			return memoryErr
		}
		tombstone, copyErr := copyTransactionMutation(operation, storage.Mutation{Target: mutation.Target, Op: storage.MutationDelete, Key: originalKey}, budget-used)
		if copyErr != nil {
			return copyErr
		}
		if err = transaction.stageNormalized(operation, tombstone); err != nil {
			return err
		}
	}
	if err = transaction.stageNormalized(operation, mutation); err != nil {
		return err
	}
	if err = transaction.active(operation); err != nil {
		return err
	}
	state.applied = true
	return nil
}

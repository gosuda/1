package schema

import (
	"bytes"
	"context"
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	errIndexOwnerCorrupt    = &model.Error{Code: "XX001", Message: "index entry ownership is inconsistent", Cause: storage.ErrCorruptStorage}
	errIndexUniqueViolation = &model.Error{Code: "23505", Message: "duplicate key violates unique index", Cause: storage.ErrInvalidMutation}
)

// FindConflict borrows a normalized candidate and returns an owned base-row key.
// Empty arbiters select all public unique indexes; primary arbitration belongs
// to the caller. Update locks remain in the caller's active statement.
// maxBytes admits index workspace and the returned owner, not transaction storage.
func (s *IndexSet) FindConflict(ctx context.Context, transaction txn.LockingMutationApplier, candidate *model.Row, arbiters []model.ConflictArbiter, maxBytes uint64) (*model.ConflictOwner, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || transaction == nil || candidate == nil || candidate.Deleted || len(candidate.Key) == 0 {
		return nil, storage.ErrInvalidMutation
	}
	if err := s.ValidateArbiters(ctx, arbiters); err != nil {
		return nil, err
	}
	memory := indexSetBudget{remaining: maxBytes}
	if err := memory.charge(1, uint64(unsafe.Sizeof(model.ConflictOwner{}))); err != nil {
		return nil, err
	}
	workspaceAdmitted := false
	for i := range s.indexes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		binding := &s.indexes[i]
		if !binding.plan.Valid() || !binding.plan.isUnique() || binding.plan.deferred || binding.plan.exclusion || binding.plan.State() != model.StatePublic {
			continue
		}
		selected := len(arbiters) == 0
		for _, arbiter := range arbiters {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !arbiter.Primary && arbiter.Index == binding.target.ObjectID {
				selected = true
				break
			}
		}
		if !selected {
			continue
		}
		if !workspaceAdmitted {
			if err := memory.charge(1, s.WorkspaceBytes()); err != nil {
				return nil, err
			}
			workspaceAdmitted = true
		}
		entry, err := binding.plan.Entry(ctx, candidate)
		if err != nil {
			return nil, err
		}
		if entry == nil {
			continue
		}
		// Entry supplies a base-key disambiguator exactly when NULL values do
		// not participate in uniqueness. A primary collision is not this arbiter.
		if len(entry.Values[len(binding.plan.keys)].BytesVal) != 0 {
			continue
		}
		acquired, err := transaction.LockRow(ctx, binding.target, entry.Key, txn.UpdateLock, txn.WaitForRowLock)
		if err != nil {
			return nil, err
		}
		if !acquired {
			return nil, storage.ErrInvalidMutation
		}
		owner, err := binding.readOwner(ctx, transaction, entry.Key, s.limits.EntryBytes)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if owner == nil {
			continue
		}
		if err := memory.charge(uint64(len(owner)), 1); err != nil {
			return nil, err
		}
		result := &model.ConflictOwner{
			Arbiter: model.ConflictArbiter{
				Index:      binding.target.ObjectID,
				Version:    binding.target.DescriptorVersion,
				Generation: binding.target.Generation,
			},
			Key: bytes.Clone(owner),
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return result, nil
	}
	return nil, ctx.Err()
}

// ValidateArbiters verifies that an inferred conflict target names current
// public unique indexes of this table.
func (s *IndexSet) ValidateArbiters(ctx context.Context, arbiters []model.ConflictArbiter) error {
	if s == nil {
		return errIndexSetDescriptor
	}
	for _, arbiter := range arbiters {
		if err := ctx.Err(); err != nil {
			return err
		}
		if arbiter.Primary {
			if arbiter.Index != 0 {
				return errIndexSetDescriptor
			}
			continue
		}
		binding, found := s.index(arbiter.Index)
		if !found || !binding.plan.Valid() || !binding.plan.isUnique() || binding.plan.deferred || binding.plan.exclusion || binding.plan.State() != model.StatePublic ||
			binding.target.DescriptorVersion != arbiter.Version || binding.target.Generation != arbiter.Generation {
			return errIndexSetDescriptor
		}
	}
	return nil
}

// MapParentArbiters translates a partition parent's unique-index identities to
// the cloned indexes maintained by this table.
func (s *IndexSet) MapParentArbiters(arbiters []model.ConflictArbiter) ([]model.ConflictArbiter, error) {
	if s == nil {
		return nil, errIndexSetDescriptor
	}
	result := make([]model.ConflictArbiter, len(arbiters))
	for i, arbiter := range arbiters {
		result[i] = arbiter
		if arbiter.Primary {
			continue
		}
		var selected *indexBinding
		for j := range s.indexes {
			if s.indexes[j].plan.parentIndex != arbiter.Index {
				continue
			}
			if selected != nil {
				return nil, errIndexSetDescriptor
			}
			selected = &s.indexes[j]
			if !selected.plan.Valid() {
				return nil, errIndexSetDescriptor
			}
		}
		if selected == nil {
			return nil, errIndexSetDescriptor
		}
		result[i] = model.ConflictArbiter{
			Index: selected.target.ObjectID, Version: selected.target.DescriptorVersion, Generation: selected.target.Generation,
		}
	}
	return result, nil
}

// Expand requires the caller's active statement. Any error must escape that
// statement so its base writes and already-staged index writes roll back together.
func (s *IndexSet) Expand(ctx context.Context, transaction txn.MutationApplier, old, current *model.Row) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || transaction == nil {
		return storage.ErrInvalidMutation
	}
	for i := range s.indexes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.indexes[i].expand(ctx, transaction, old, current, s.limits.EntryBytes); err != nil {
			return err
		}
	}
	if s.shadow != nil {
		return s.shadow.Expand(ctx, transaction, old, current)
	}
	return ctx.Err()
}

// ExpandSelected stages only the named indexes from this set. Index IDs must be
// strictly increasing and every ID must belong to the set.
func (s *IndexSet) ExpandSelected(ctx context.Context, transaction txn.MutationApplier, current *model.Row, indexes []model.ObjectID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || transaction == nil || current == nil || current.Deleted || len(indexes) == 0 {
		return storage.ErrInvalidMutation
	}
	position := 0
	for i, id := range indexes {
		if id == 0 || i > 0 && indexes[i-1] >= id {
			return errIndexSetDescriptor
		}
		for position < len(s.indexes) && s.indexes[position].target.ObjectID < id {
			position++
		}
		if position >= len(s.indexes) || s.indexes[position].target.ObjectID != id {
			return errIndexSetDescriptor
		}
		if err := s.indexes[position].expand(ctx, transaction, nil, current, s.limits.EntryBytes); err != nil {
			return err
		}
		position++
	}
	return ctx.Err()
}

func (b *indexBinding) expand(ctx context.Context, transaction txn.MutationApplier, old, current *model.Row, entryBytes uint64) error {
	if !b.plan.WriteMaintained() {
		return nil
	}
	if b.plan.State() == model.StateAbsent {
		return nil
	}
	previous, err := b.plan.Entry(ctx, old)
	if err != nil {
		return err
	}
	var next *IndexEntry
	switch b.plan.State() {
	case model.StateDeleteOnly, model.StateDropDeleteOnly:
	case model.StateWriteOnly, model.StatePublic, model.StateDropWriteOnly:
		next, err = b.plan.Entry(ctx, current)
		if err != nil {
			return err
		}
	default:
		return errIndexSetDescriptor
	}
	if previous == nil && next == nil {
		return nil
	}
	if previous != nil && next != nil && bytes.Equal(previous.Key, next.Key) {
		owner, err := b.readOwner(ctx, transaction, previous.Key, entryBytes)
		if err != nil {
			return err
		}
		if owner != nil && !bytes.Equal(owner, old.Key) {
			return errIndexOwnerCorrupt
		}
		op := storage.MutationInsert
		if owner != nil {
			op = storage.MutationUpdate
		}
		mutation := indexEntryMutation(b.target, op, next)
		return transaction.ApplyMutations(ctx, []storage.Mutation{mutation})
	}
	var mutations [2]storage.Mutation
	count := 0
	if previous != nil {
		owner, err := b.readOwner(ctx, transaction, previous.Key, entryBytes)
		if err != nil {
			return err
		}
		if owner != nil {
			if !bytes.Equal(owner, old.Key) {
				return errIndexOwnerCorrupt
			}
			mutations[count] = storage.Mutation{Op: storage.MutationDelete, Target: b.target, Key: previous.Key}
			count++
		}
	}
	if next != nil {
		owner, err := b.readOwner(ctx, transaction, next.Key, entryBytes)
		if err != nil {
			return err
		}
		alreadyIndexed := owner != nil && bytes.Equal(owner, current.Key)
		if b.plan.pendingKind == model.ConstraintPrimaryKey && b.plan.State() == model.StateWriteOnly {
			for _, value := range next.Values[:len(b.plan.keys)] {
				if value.IsNull {
					return &model.Error{Code: "23502", Message: "null value violates primary key constraint " + b.plan.pendingName}
				}
			}
		}
		if b.plan.pendingKind != 0 && b.plan.State() == model.StateWriteOnly && !alreadyIndexed {
			participates := true
			for _, value := range next.Values[:len(b.plan.keys)] {
				if value.IsNull && !b.plan.nullsNotDistinct {
					participates = false
					break
				}
			}
			if participates {
				constrained, ok := transaction.(txn.ConstraintTransaction)
				if !ok {
					return errIndexSetDescriptor
				}
				prefixCount, countErr := countIndexPrefix(ctx, constrained, b.target, b.plan.layout.Columns, b.plan.keyLayout, b.ownerLayout[0].ID, next.Values[:len(b.plan.keys)], entryBytes)
				if countErr != nil {
					return countErr
				}
				if prefixCount != 0 {
					return errIndexUniqueViolation
				}
			}
		}
		op := storage.MutationInsert
		if owner != nil {
			if !bytes.Equal(owner, current.Key) {
				if b.plan.isUnique() {
					return errIndexUniqueViolation
				}
				return errIndexOwnerCorrupt
			}
			op = storage.MutationUpdate
		}
		mutations[count] = indexEntryMutation(b.target, op, next)
		count++
	}
	if count == 0 {
		return nil
	}
	return transaction.ApplyMutations(ctx, mutations[:count])
}

func indexEntryMutation(target storage.ObjectRef, op storage.MutationOp, entry *IndexEntry) storage.Mutation {
	return storage.Mutation{Op: op, Target: target, Key: entry.Key, ColumnIDs: entry.ColumnIDs, Values: entry.Values}
}

// A nil owner denotes only an absent row. Empty, NULL, deleted or malformed
// owners are corruption, never permission to replace another base row's entry.
func (b *indexBinding) readOwner(ctx context.Context, transaction txn.MutationApplier, key []byte, entryBytes uint64) ([]byte, error) {
	row, err := transaction.Get(ctx, storage.ReadRequest{Target: b.target, Key: key, ColumnIDs: b.ownerIDs[:], Layout: b.ownerLayout[:]})
	if err != nil {
		if indexRowAbsent(err) {
			return nil, nil
		}
		return nil, err
	}
	if row.Deleted || !bytes.Equal(row.Key, key) || len(row.Values) != 1 {
		return nil, errIndexOwnerCorrupt
	}
	value := row.Values[0]
	if value.Type != model.TypeBytes || value.IsNull || len(value.BytesVal) == 0 {
		return nil, errIndexOwnerCorrupt
	}
	memory := indexSetBudget{remaining: entryBytes}
	if err := memory.charge(uint64(len(row.Key)), 1); err != nil {
		return nil, err
	}
	if err := memory.charge(1, uint64(unsafe.Sizeof(model.Value{}))); err != nil {
		return nil, err
	}
	if _, err := model.ValueBytes(ctx, value, memory.remaining); err != nil {
		if errors.Is(err, model.ErrValueBudget) {
			return nil, errIndexSetLimit
		}
		return nil, err
	}
	return value.BytesVal, nil
}

// allCauses reports whether err and every error joined into or wrapped by it
// satisfies pred. Joined errors require every cause to satisfy pred; a
// non-joined error is unwrapped and its cause visited. Nil and empty join sets
// fail.
func allCauses(err error, pred func(error) bool) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !allCauses(cause, pred) {
				return false
			}
		}
		return true
	}
	if pred(err) {
		return true
	}
	if cause := errors.Unwrap(err); cause != nil {
		return allCauses(cause, pred)
	}
	return false
}

func indexRowAbsent(err error) bool {
	return allCauses(err, func(err error) bool { return err == storage.ErrRowNotFound })
}

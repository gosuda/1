package catalog

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)


// LockDescriptor excludes readers before DDL changes state outside the catalog
// transaction. Namespace/absence dependencies remain the caller's responsibility.
func (s *Store) LockDescriptor(ctx context.Context, transaction txn.LockingMutationApplier, id model.ObjectID) error {
	if transaction == nil || id == 0 || id >= NamespaceObjectID {
		return model.ErrInvalidDescriptor
	}
	key, err := s.encoded(ctx, numberKey(uint64(id)))
	if err != nil {
		return err
	}
	locked, err := transaction.LockRow(ctx, target(DescriptorObjectID), key, txn.UpdateLock, txn.WaitForRowLock)
	if err != nil {
		return err
	}
	if !locked {
		return &model.Error{Code: "55000", Message: "catalog descriptor lock was not granted"}
	}
	return nil
}

// LockDescriptorPairSkipLocked atomically admits work that depends on a schema
// and its relation. A false result retains neither descriptor lock.
func (s *Store) LockDescriptorPairSkipLocked(ctx context.Context, transaction *txn.Transaction, schema, relation model.ObjectID) (bool, error) {
	if s == nil || transaction == nil || schema == 0 || relation == 0 || schema >= NamespaceObjectID || relation >= NamespaceObjectID || schema == relation {
		return false, model.ErrInvalidDescriptor
	}
	schemaKey, err := s.encoded(ctx, numberKey(uint64(schema)))
	if err != nil {
		return false, err
	}
	relationKey, err := s.encoded(ctx, numberKey(uint64(relation)))
	if err != nil {
		return false, err
	}
	requests := [2]txn.RowLockRequest{
		{Target: target(DescriptorObjectID), Key: schemaKey, Strength: txn.ShareLock},
		{Target: target(DescriptorObjectID), Key: relationKey, Strength: txn.UpdateLock},
	}
	return transaction.LockRows(ctx, requests[:], txn.SkipLockedRow)
}

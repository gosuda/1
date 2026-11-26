package bind

import (
	"slices"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) bindTableLock(statement *zpgparse.LockStmt) (Command, []ResultColumn, error) {
	if b.catalog == nil {
		return nil, nil, bindError("55000", "catalog snapshot is not configured", 0)
	}
	if statement == nil || len(statement.Relations) == 0 || statement.Mode < int32(txn.AccessShareTableLock) || statement.Mode > int32(txn.AccessExclusiveTableLock) {
		return nil, nil, bindError("42601", "invalid LOCK TABLE command", 0)
	}
	if uint64(len(statement.Relations)) > uint64(b.options.Limits.MaxOperands) {
		return nil, nil, bindError("54000", "LOCK TABLE relation limit exceeded", 0)
	}
	mode := txn.TableLockMode(statement.Mode)
	command := TableLockCommand{Nowait: statement.Nowait}
	seen := make(map[model.ObjectID]struct{}, len(statement.Relations))
	for _, relationNode := range statement.Relations {
		relation := relationNode.GetRangeVar()
		if relation == nil || relation.Relname == "" || relation.Catalogname != "" || relation.Alias != nil {
			return nil, nil, bindError("42601", "invalid LOCK TABLE relation", 0)
		}
		position := int(relation.Location) + 1
		parts := []string{relation.Relname}
		if relation.Schemaname != "" {
			parts = []string{relation.Schemaname, relation.Relname}
		}
		root, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectTable, model.ObjectView, model.ObjectMaterializedView}, "42P01", position)
		if err != nil {
			return nil, nil, err
		}
		if root.Kind() != model.ObjectTable {
			return nil, nil, bindError("42809", "LOCK TABLE target is not a table", position)
		}
		descriptors := []*model.Descriptor{root}
		metadata, ok := root.Table()
		if !ok {
			return nil, nil, bindError("XX000", "table descriptor has no table metadata", position)
		}
		if relation.Inh && metadata.InheritanceChildren != 0 {
			descriptors, err = b.inheritanceDescriptors(root, position)
			if err != nil {
				return nil, nil, err
			}
		}
		for _, descriptor := range descriptors {
			if _, duplicate := seen[descriptor.ID()]; duplicate {
				continue
			}
			physical := descriptor.Physical()
			target := storage.ObjectRef{ObjectID: descriptor.ID(), Generation: physical.Generation, DescriptorVersion: descriptor.Version(), EncodingVersion: physical.EncodingVersion}
			if err := target.Validate(); err != nil {
				return nil, nil, bindError("55000", "LOCK TABLE target has no physical identity", position)
			}
			if uint64(len(command.Requests)) >= uint64(b.options.Limits.MaxOperands) {
				return nil, nil, bindError("54000", "LOCK TABLE relation limit exceeded", position)
			}
			seen[descriptor.ID()] = struct{}{}
			b.dependency(descriptor, 0)
			command.Requests = append(command.Requests, txn.TableLockRequest{Target: target, Mode: mode})
		}
	}
	slices.SortFunc(command.Requests, func(left, right txn.TableLockRequest) int {
		return compareObjectID(left.Target.ObjectID, right.Target.ObjectID)
	})
	return command, nil, nil
}

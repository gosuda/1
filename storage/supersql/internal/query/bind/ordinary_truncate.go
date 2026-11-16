package bind

import (
	"context"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type BoundTruncate struct {
	tables           []*model.Descriptor
	sequences        []*BoundSequenceDefinition
	commands         []*BoundObjectCommand
	dependencies     []model.Dependency
	scannedSchemas   []model.ObjectID
	scannedSequences bool
	warnings         []model.Error
}

func (t *BoundTruncate) Tables() []*model.Descriptor           { return slices.Clone(t.tables) }
func (t *BoundTruncate) Sequences() []*BoundSequenceDefinition { return slices.Clone(t.sequences) }
func (t *BoundTruncate) Commands() []*BoundObjectCommand       { return slices.Clone(t.commands) }
func (t *BoundTruncate) Dependencies() []model.Dependency      { return slices.Clone(t.dependencies) }
func (t *BoundTruncate) ScannedSchemas() []model.ObjectID      { return slices.Clone(t.scannedSchemas) }
func (t *BoundTruncate) SequencesScanned() bool                { return t.scannedSequences }
func (t *BoundTruncate) Warnings() []model.Error               { return slices.Clone(t.warnings) }

func BindTruncateTransactional(ctx context.Context, statement *zpgparse.TruncateStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundTruncate, error) {
	maximum := uint64(options.Bind.Limits.MaxOperands)
	if statement == nil || snapshot == nil || len(statement.Relations) == 0 || uint64(len(statement.Relations)) > maximum {
		return nil, bindError("42601", "invalid TRUNCATE command", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_TruncateStmt{TruncateStmt: statement}})
	if err != nil {
		return nil, err
	}
	seen := make(map[model.ObjectID]bool, len(statement.Relations))
	roots := make([]*model.Descriptor, 0, len(statement.Relations))
	for _, relationNode := range statement.Relations {
		relation := relationNode.GetRangeVar()
		if relation == nil || relation.Relname == "" || relation.Catalogname != "" || relation.Alias != nil {
			return nil, bindError("42601", "invalid TRUNCATE relation", 0)
		}
		position := int(relation.Location) + 1
		parts := []string{relation.Relname}
		if relation.Schemaname != "" {
			parts = []string{relation.Schemaname, relation.Relname}
		}
		root, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectTable}, "42P01", position)
		if err != nil {
			return nil, err
		}
		metadata, ok := root.Table()
		if !ok || root.State() != model.StatePublic {
			return nil, model.ErrInvalidDescriptor
		}
		members := []*model.Descriptor{root}
		if relation.Inh && metadata.InheritanceChildren != 0 {
			members, err = b.inheritanceDescriptors(root, position)
			if err != nil {
				return nil, err
			}
		} else if !relation.Inh && metadata.PartitionStrategy != 0 {
			return nil, bindError("42809", "cannot truncate only a partitioned table", position)
		}
		for _, member := range members {
			if seen[member.ID()] {
				continue
			}
			if uint64(len(roots)) >= maximum {
				return nil, bindError("54000", "TRUNCATE relation limit exceeded", position)
			}
			seen[member.ID()] = true
			roots = append(roots, member)
			b.dependency(member, 0)
		}
	}
	tables, err := BindTruncateClosure(ctx, b, snapshot, roots, statement.Behavior == zpgparse.DropBehavior_DROP_CASCADE, int(min(maximum, uint64(^uint(0)>>1))))
	if err != nil {
		return nil, err
	}
	if len(tables) == 0 {
		return nil, model.ErrDescriptorLimit
	}
	targets := make(map[model.ObjectID]bool, len(tables))
	var persistentUpdates, temporaryUpdates []ObjectMutation
	for _, table := range tables {
		if table == nil || table.Kind() != model.ObjectTable || table.State() != model.StatePublic {
			return nil, model.ErrInvalidDescriptor
		}
		targets[table.ID()] = true
		spec, err := table.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if spec.Table == nil || spec.Version == ^uint64(0) || spec.Physical.Generation == 0 || spec.Physical.Generation == ^uint64(0) {
			return nil, model.ErrDescriptorLimit
		}
		sourceIndexes, err := onlineSourceIndexes(ctx, b, snapshot, options, spec.Table.Indexes)
		if err != nil {
			return nil, err
		}
		spec.Version++
		spec.Physical.Generation++
		target, err := rebindTargetTableExpressions(ctx, table, &spec, snapshot, options)
		if err != nil {
			return nil, err
		}
		targetIndexes, err := onlineTargetIndexes(ctx, snapshot, options, sourceIndexes, target, 0)
		if err != nil {
			return nil, err
		}
		if truncateUpdateLimitExceeded(options.Bind.Limits.MaxOperands, len(persistentUpdates), len(temporaryUpdates), len(sourceIndexes)) {
			return nil, bindError("54000", "TRUNCATE descriptor update limit exceeded", 0)
		}
		temporary, err := temporaryDescriptor(ctx, table, options.Descriptors)
		if err != nil {
			return nil, err
		}
		group := &persistentUpdates
		if temporary {
			group = &temporaryUpdates
		}
		*group = append(*group, ObjectMutation{Base: table, Next: target})
		for i := range sourceIndexes {
			*group = append(*group, ObjectMutation{Base: sourceIndexes[i], Next: targetIndexes[i]})
		}
	}
	var sequences []*BoundSequenceDefinition
	if statement.RestartSeqs {
		sequences, err = truncateOwnedSequences(ctx, b, snapshot, targets, options)
		if err != nil {
			return nil, err
		}
	}
	scannedSchemas, err := truncateScannedSchemas(ctx, snapshot, maximum)
	if err != nil {
		return nil, err
	}
	dependencies := slices.Clone(b.dependencies)
	warnings := slices.Clone(b.warnings)
	var commands []*BoundObjectCommand
	if len(persistentUpdates) != 0 {
		commands = append(commands, &BoundObjectCommand{updates: persistentUpdates, tag: "TRUNCATE"})
	}
	if len(temporaryUpdates) != 0 {
		commands = append(commands, &BoundObjectCommand{updates: temporaryUpdates, tag: "TRUNCATE"})
	}
	return &BoundTruncate{
		tables: slices.Clone(tables), sequences: sequences, commands: commands,
		dependencies: dependencies, scannedSchemas: scannedSchemas,
		scannedSequences: statement.RestartSeqs, warnings: warnings,
	}, ctx.Err()
}

func truncateUpdateLimitExceeded(maximum uint32, persistent, temporary, indexes int) bool {
	remaining := uint64(maximum)
	if uint64(persistent) > remaining {
		return true
	}
	remaining -= uint64(persistent)
	if uint64(temporary) > remaining {
		return true
	}
	remaining -= uint64(temporary)
	return uint64(indexes) >= remaining
}

func truncateScannedSchemas(ctx context.Context, snapshot CatalogSnapshot, maximum uint64) ([]model.ObjectID, error) {
	var schemas []model.ObjectID
	if err := snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(published catalog.PublishedDescriptor) error {
		descriptor := published.Descriptor
		if descriptor == nil || descriptor.State() != model.StatePublic || descriptor.Kind() != model.ObjectSchema {
			return model.ErrInvalidDescriptor
		}
		if uint64(len(schemas)) >= maximum {
			return bindError("54000", "TRUNCATE schema scan limit exceeded", 0)
		}
		schemas = append(schemas, descriptor.ID())
		return nil
	}); err != nil {
		return nil, err
	}
	slices.Sort(schemas)
	return schemas, ctx.Err()
}

func truncateOwnedSequences(ctx context.Context, b *binder, snapshot CatalogSnapshot, targets map[model.ObjectID]bool, options CreateDeclarationOptions) ([]*BoundSequenceDefinition, error) {
	var schemas []*model.Descriptor
	var scanned uint64
	if err := snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(published catalog.PublishedDescriptor) error {
		if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
			return nil
		}
		scanned++
		if scanned > uint64(options.Bind.Limits.MaxOperands) {
			return bindError("54000", "TRUNCATE sequence scan limit exceeded", 0)
		}
		schemas = append(schemas, published.Descriptor)
		return nil
	}); err != nil {
		return nil, err
	}
	var sequences []*BoundSequenceDefinition
	for _, schema := range schemas {
		if err := snapshot.ScanNamespace(ctx, schema.ID(), model.ObjectSequence, func(published catalog.PublishedDescriptor) error {
			descriptor := published.Descriptor
			if descriptor == nil || descriptor.State() != model.StatePublic {
				return nil
			}
			scanned++
			if scanned > uint64(options.Bind.Limits.MaxOperands) {
				return bindError("54000", "TRUNCATE sequence scan limit exceeded", 0)
			}
			metadata, ok := descriptor.Sequence()
			if !ok {
				return model.ErrInvalidDescriptor
			}
			b.dependency(descriptor, 0)
			if !targets[metadata.OwnerTable] {
				return nil
			}
			if uint64(len(sequences)) >= uint64(options.Bind.Limits.MaxOperands) {
				return bindError("54000", "TRUNCATE owned sequence limit exceeded", 0)
			}
			parent, err := snapshot.Descriptor(ctx, descriptor.Name().Parent)
			if err != nil {
				return err
			}
			if parent.Descriptor.Kind() != model.ObjectSchema {
				return model.ErrInvalidDescriptor
			}
			relation := &zpgparse.RangeVar{Schemaname: parent.Descriptor.Name().Name, Relname: descriptor.Name().Name, Inh: true, Relpersistence: metadata.Persistence, Location: -1}
			restart := &zpgparse.DefElem{Defname: "restart", Location: -1, Defaction: zpgparse.DefElemAction_DEFELEM_UNSPEC}
			bound, err := BindAlterSequence(ctx, &zpgparse.AlterSeqStmt{
				Sequence: relation,
				Options: []*zpgparse.Node{{Node: &zpgparse.Node_DefElem{DefElem: restart}}},
			}, snapshot, options)
			if err != nil {
				return err
			}
			sequences = append(sequences, bound)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(sequences, func(left, right *BoundSequenceDefinition) int {
		return compareObjectID(left.Base().ID(), right.Base().ID())
	})
	return sequences, ctx.Err()
}

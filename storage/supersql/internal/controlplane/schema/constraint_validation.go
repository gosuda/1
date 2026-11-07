package schema

import (
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type immediateConstraintDeferral struct{}

func (immediateConstraintDeferral) DeferConstraint(context.Context, txn.DeferredConstraint) (bool, error) {
	return false, nil
}

// ValidateConstraint scans the transaction-visible table and validates one
// target constraint before its descriptor can be published.
func (p *IndexPolicyProvider) ValidateConstraint(ctx context.Context, transaction txn.ConstraintTransaction, snapshot *catalog.CatalogSnapshot, sourceVersion uint64, source, target *model.Descriptor, constraintID model.ObjectID) error {
	return p.ValidateConstraintWithIndexes(ctx, transaction, snapshot, sourceVersion, source, target, constraintID, nil)
}

// ValidateConstraintWithIndexes also admits unpublished target index
// descriptors owned by the same atomic schema change.
func (p *IndexPolicyProvider) ValidateConstraintWithIndexes(ctx context.Context, transaction txn.ConstraintTransaction, snapshot *catalog.CatalogSnapshot, sourceVersion uint64, source, target *model.Descriptor, constraintID model.ObjectID, indexes []*model.Descriptor) error {
	if p == nil || transaction == nil || snapshot == nil || sourceVersion == 0 || source == nil || target == nil || constraintID == 0 || source.ID() != target.ID() || source.Kind() != model.ObjectTable || target.Kind() != model.ObjectTable || source.Physical() != target.Physical() {
		return errRelationalPolicy
	}
	targetSpec, err := target.SpecWithin(ctx, model.DescriptorLimits{MaxBytes: p.limits.MetadataBytes, MaxItems: int(p.limits.MetadataBytes), MaxDepth: 64})
	if err != nil || targetSpec.Table == nil {
		return errors.Join(errRelationalPolicy, err)
	}
	position := slices.IndexFunc(targetSpec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool { return constraint.ID == constraintID })
	if position < 0 {
		return errRelationalPolicy
	}
	constraint := targetSpec.Table.Constraints[position]
	planningSpec := targetSpec
	planningSpec.Version = sourceVersion
	planning, err := model.NewDescriptor(ctx, planningSpec, model.DescriptorLimits{MaxBytes: p.limits.MetadataBytes, MaxItems: int(p.limits.MetadataBytes), MaxDepth: 64})
	if err != nil {
		return err
	}
	encoder, err := p.keyEncoder(ctx, snapshot)
	if err != nil {
		return err
	}
	var indexExpressions IndexExpressions
	if p.expressions != nil {
		indexExpressions, err = p.expressions(ctx, snapshot)
		if err != nil {
			return err
		}
	}
	localSet := &IndexSet{table: planning, target: lifecycleTarget(planning), limits: p.limits}
	if constraint.Index != 0 {
		localSet, err = p.validationIndexSet(ctx, snapshot, planning, constraint.Index, constraint.Deferrable, encoder, indexExpressions, indexes)
		if err != nil {
			return err
		}
	}
	local, _, err := newRelationalTable(ctx, localSet, targetSpec.Table, p.limits.MetadataBytes)
	if err != nil {
		return err
	}
	relations := &relationalSet{tables: []relationalTable{local}, encoder: encoder, maxBytes: p.limits.EntryBytes}
	if constraint.Kind == model.ConstraintForeignKey {
		published, lookupErr := snapshot.Descriptor(ctx, constraint.ReferencedObject)
		if lookupErr != nil || published.Descriptor == nil {
			return errors.Join(errRelationalPolicy, lookupErr)
		}
		parentSpec, specErr := published.Descriptor.SpecWithin(ctx, model.DescriptorLimits{MaxBytes: p.limits.MetadataBytes, MaxItems: int(p.limits.MetadataBytes), MaxDepth: 64})
		if specErr != nil || parentSpec.Table == nil {
			return errors.Join(errRelationalPolicy, specErr)
		}
		parentSet, setErr := p.validationIndexSet(ctx, snapshot, published.Descriptor, constraint.ReferencedIndex, false, encoder, indexExpressions, indexes)
		if setErr != nil {
			return setErr
		}
		parent, _, tableErr := newRelationalTable(ctx, parentSet, parentSpec.Table, p.limits.MetadataBytes)
		if tableErr != nil {
			return tableErr
		}
		if constraint.ReferencedObject == source.ID() {
			relations.tables[0] = parent
		} else {
			relations.tables = append(relations.tables, parent)
			slices.SortFunc(relations.tables, func(left, right relationalTable) int {
				if left.target.ObjectID < right.target.ObjectID {
					return -1
				}
				if left.target.ObjectID > right.target.ObjectID {
					return 1
				}
				return 0
			})
		}
	}
	var normalizer *txn.RowNormalizer
	if constraint.Kind == model.ConstraintCheck || constraint.Kind == model.ConstraintNotNull {
		var expressions ColumnExpressions
		if p.columns != nil {
			expressions, err = p.columns(ctx, snapshot)
			if err != nil {
				return err
			}
		}
		policySpec := planningSpec
		policyTable := *planningSpec.Table
		policyTable.Constraints = []model.ConstraintDescriptor{constraint}
		policyTable.Triggers = nil
		for i := range policyTable.Columns {
			policyTable.Columns[i].Nullable = true
			if constraint.Kind == model.ConstraintNotNull && slices.Contains(constraint.Columns, policyTable.Columns[i].ID) {
				policyTable.Columns[i].Nullable = false
			}
		}
		policySpec.Table = &policyTable
		policyDescriptor, policyErr := model.NewDescriptor(ctx, policySpec, model.DescriptorLimits{MaxBytes: p.limits.MetadataBytes, MaxItems: int(p.limits.MetadataBytes), MaxDepth: 64})
		if policyErr != nil {
			return policyErr
		}
		normalizer, err = NewTablePolicy(ctx, TablePolicyOptions{Descriptor: policyDescriptor, Encoder: encoder, Expressions: expressions, MetadataBytes: p.limits.MetadataBytes, RowBytes: p.limits.EntryBytes, MaxColumns: p.limits.MaxColumns})
		if err != nil {
			return err
		}
	}
	execution := relationalExecution{transaction: transaction, deferral: immediateConstraintDeferral{}, set: relations, maxBytes: p.limits.EntryBytes}
	return scanRelationalRows(ctx, transaction, &local, encoder, p.limits.EntryBytes, func(row *model.Row) error {
		switch constraint.Kind {
		case model.ConstraintCheck, model.ConstraintNotNull:
			return normalizer.ValidateRow(ctx, row)
		case model.ConstraintUnique, model.ConstraintPrimaryKey, model.ConstraintExclude:
			return execution.checkIndexConstraint(ctx, &local, constraint, row)
		case model.ConstraintForeignKey:
			return execution.checkForeignKey(ctx, &local, constraint, row)
		default:
			return errRelationalPolicy
		}
	})
}

func (p *IndexPolicyProvider) validationIndexSet(ctx context.Context, snapshot *catalog.CatalogSnapshot, table *model.Descriptor, indexID model.ObjectID, deferred bool, encoder storage.KeyEncoder, expressions IndexExpressions, candidates []*model.Descriptor) (*IndexSet, error) {
	var index *model.Descriptor
	for _, candidate := range candidates {
		if candidate != nil && candidate.ID() == indexID {
			if index != nil {
				return nil, errRelationalPolicy
			}
			index = candidate
		}
	}
	if index == nil {
		published, err := snapshot.Descriptor(ctx, indexID)
		if err != nil || published.Descriptor == nil {
			return nil, errors.Join(errRelationalPolicy, err)
		}
		index = published.Descriptor
	}
	limits := model.DescriptorLimits{MaxBytes: p.limits.MetadataBytes, MaxItems: int(p.limits.MetadataBytes), MaxDepth: 64}
	tableSpec, err := table.SpecWithin(ctx, limits)
	if err != nil || tableSpec.Table == nil || !slices.Contains(tableSpec.Table.Indexes, indexID) {
		return nil, errors.Join(errRelationalPolicy, err)
	}
	if len(tableSpec.Table.Indexes) != 1 {
		tableSpec.Table.Indexes = []model.ObjectID{indexID}
		tableSpec.Table.Constraints = slices.DeleteFunc(tableSpec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool {
			switch constraint.Kind {
			case model.ConstraintPrimaryKey, model.ConstraintUnique, model.ConstraintExclude:
				return constraint.Index != indexID
			default:
				return false
			}
		})
		table, err = model.NewDescriptor(ctx, tableSpec, limits)
		if err != nil {
			return nil, err
		}
	}
	plan, err := NewIndexPlan(ctx, table, index, encoder, expressions, p.limits)
	if err != nil {
		return nil, err
	}
	set, err := NewIndexSet(ctx, table, []*IndexPlan{plan}, p.limits)
	if err != nil {
		return nil, err
	}
	if len(set.indexes) != 1 {
		return nil, errRelationalPolicy
	}
	set.indexes[0].plan.deferred = deferred
	return set, nil
}

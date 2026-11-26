package modify

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/distinctset"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	logicalpartition "github.com/gosuda/supersql-alpha/internal/query/partition"
)

func (c *cursor) configureConflict(ctx context.Context, table *model.TableMetadata) error {
	o := &c.options
	plan := *o.Conflict
	if plan.Action != ir.ConflictNothing && plan.Action != ir.ConflictUpdate {
		return runtime.ErrInvalidConfig
	}
	missingUpdate := len(plan.Arbiters) == 0 || (plan.Assignment == nil && len(plan.AssignmentColumns) != 0)
	if plan.Action == ir.ConflictUpdate && missingUpdate {
		return runtime.ErrInvalidConfig
	}
	extraUpdate := plan.Predicate != nil || plan.Assignment != nil || len(plan.AssignmentColumns) != 0
	if plan.Action == ir.ConflictNothing && extraUpdate {
		return runtime.ErrInvalidConfig
	}
	needsLookup := len(plan.Arbiters) == 0 && len(table.Indexes) != 0
	for i, arbiter := range plan.Arbiters {
		if slices.Contains(plan.Arbiters[:i], arbiter) {
			return runtime.ErrInvalidConfig
		}
		if arbiter.Primary {
			if arbiter.Index != 0 {
				return runtime.ErrInvalidConfig
			}
		} else {
			if arbiter.Index == 0 || arbiter.Version == 0 || arbiter.Generation == 0 || !slices.Contains(table.Indexes, arbiter.Index) {
				return runtime.ErrInvalidConfig
			}
			needsLookup = true
		}
	}
	if needsLookup && o.Conflicts == nil {
		return ErrUnsupported
	}
	if validator, ok := o.Conflicts.(interface {
		ValidateArbiters(context.Context, []model.ConflictArbiter) error
	}); ok {
		if err := validator.ValidateArbiters(ctx, plan.Arbiters); err != nil {
			return err
		}
	}
	plan.Arbiters = slices.Clone(plan.Arbiters)
	plan.AssignmentColumns = slices.Clone(plan.AssignmentColumns)
	o.Conflict = &plan
	var err error
	if plan.Predicate != nil {
		layouts := plan.Predicate.OutputLayouts()
		if len(layouts) != 1 || layouts[0].Type.Storage != model.TypeBoolean {
			return storage.ErrInvalidLayout
		}
		c.conflictState.predicate, err = expr.NewState(plan.Predicate, c.stateOptions())
		if err != nil {
			return err
		}
	}
	if plan.Action == ir.ConflictNothing {
		return nil
	}
	if plan.Assignment != nil {
		layouts := plan.Assignment.OutputLayouts()
		if len(layouts) != len(plan.AssignmentColumns) {
			return storage.ErrInvalidLayout
		}
		c.conflictState.columns = make([]int, len(layouts))
		for i, column := range plan.AssignmentColumns {
			position := slices.Index(c.columnIDs, column)
			if position < 0 || slices.Contains(plan.AssignmentColumns[:i], column) || !model.TypeLayoutsEqual(layouts[i], c.tableLayouts[position]) {
				return storage.ErrInvalidLayout
			}
			c.conflictState.columns[i] = position
		}
		c.conflictState.assignment, err = expr.NewState(plan.Assignment, c.stateOptions())
		if err != nil {
			return err
		}
	}
	imageLayouts := c.visibleLayouts
	if len(c.viewMutationSteps) != 0 {
		imageLayouts = c.viewMutationSteps[len(c.viewMutationSteps)-1].layouts
	}
	composite := model.TypeLayout{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, Children: imageLayouts}
	c.conflictInput, err = columnar.NewBuilder([]model.TypeLayout{composite, composite}, columnar.BuilderOptions{MaxRows: 1, MaxBytes: o.BatchBytes, Allocator: o.Query.Allocator(runtime.WorkMemory)})
	if err != nil {
		return err
	}
	keyLayouts := make([]model.TypeLayout, len(c.keyColumns))
	for i, column := range c.keyColumns {
		keyLayouts[i] = c.tableLayouts[column]
	}
	c.mutatedTargets, err = distinctset.New(ctx, distinctset.Options{Query: o.Query, Encoder: o.Encoder, Layouts: keyLayouts, MemoryBytes: o.WorkBytes / 16, WorkBytes: o.WorkBytes / 2, MaxKeyBytes: o.MaxRecordBytes, MaxKeys: o.MaxRows * 2, MaxLevels: 64})
	return err
}

func (c *cursor) recordConflictIdentity(ctx context.Context, row []model.Value) error {
	for i, column := range c.keyColumns {
		c.keyValues[i] = row[column]
	}
	inserted, err := c.mutatedTargets.Insert(ctx, c.keyValues)
	if err != nil {
		return err
	}
	if !inserted {
		return &model.Error{Code: "21000", Message: "ON CONFLICT DO UPDATE command cannot affect row a second time"}
	}
	return nil
}

func (c *cursor) conflictOwner(ctx context.Context, proposed *model.Row, route *partitionChild) (*model.Row, error) {
	o := &c.options
	target, columnIDs, layout := c.target, c.columnIDs, o.Layout
	conflicts, arbiters := o.Conflicts, o.Conflict.Arbiters
	if route != nil {
		var err error
		if o.PartitionTarget != 0 {
			conflicts, arbiters, err = o.Partitions.conflictFrom(route, o.PartitionTarget, arbiters)
		} else {
			conflicts, arbiters, err = o.Partitions.conflict(route, arbiters)
		}
		if err != nil {
			return nil, err
		}
		target, columnIDs, layout = route.target, route.columnIDs, route.layout
	}
	if conflicts == nil && slices.ContainsFunc(arbiters, func(arbiter model.ConflictArbiter) bool { return !arbiter.Primary }) {
		return nil, ErrUnsupported
	}
	get := func(key []byte) (model.Row, error) {
		return o.Transaction.Get(ctx, storage.ReadRequest{Target: target, Key: key, ColumnIDs: columnIDs, Layout: layout})
	}
	if _, err := o.Transaction.LockRow(ctx, target, proposed.Key, txn.UpdateLock, txn.WaitForRowLock); err != nil {
		return nil, err
	}
	primary := len(arbiters) == 0
	for _, arbiter := range arbiters {
		primary = primary || arbiter.Primary
	}
	var primaryOwner *model.Row
	if primary {
		row, err := get(proposed.Key)
		if err == nil {
			if row.Deleted || len(row.Values) != len(columnIDs) || !bytes.Equal(row.Key, proposed.Key) {
				return nil, storage.ErrInvalidLayout
			}
			primaryOwner = &row
		}
		if err != nil && !errors.Is(err, storage.ErrRowNotFound) {
			return nil, err
		}
	}
	if conflicts == nil {
		return primaryOwner, nil
	}
	maximum := o.WorkBytes
	if o.Expansion != nil {
		bytes := o.Expansion.WorkspaceBytes()
		if maximum > ^uint64(0)-bytes {
			return nil, resource("admit conflict lookup workspace")
		}
		maximum += bytes
	}
	if o.Partitions != nil {
		bytes := o.Partitions.WorkspaceBytes()
		if maximum > ^uint64(0)-bytes {
			return nil, resource("admit conflict lookup workspace")
		}
		maximum += bytes
	}
	owner, err := conflicts.FindConflict(ctx, o.Transaction, proposed, arbiters, maximum)
	if err != nil {
		return nil, err
	}
	if primaryOwner != nil || owner == nil {
		return primaryOwner, nil
	}
	if _, err := o.Transaction.LockRow(ctx, target, owner.Key, txn.UpdateLock, txn.WaitForRowLock); err != nil {
		return nil, err
	}
	row, err := get(owner.Key)
	if errors.Is(err, storage.ErrRowNotFound) {
		return nil, &model.Error{Code: "XX001", Message: "unique index owner is missing"}
	}
	if err != nil {
		return nil, err
	}
	if row.Deleted || len(row.Values) != len(columnIDs) || !bytes.Equal(row.Key, owner.Key) {
		return nil, storage.ErrInvalidLayout
	}
	return &row, nil
}

func (c *cursor) insert(ctx context.Context, frozen []model.Value, assignment *expr.State, columns []int, columnIDs []model.ColumnID) (affected bool, err error) {
	o := &c.options
	assigned, err := c.assignments(ctx, frozen[:len(o.InputLayouts)], assignment, columns)
	if err != nil {
		return false, err
	}
	if logicalMutationTarget(o.TargetKind) {
		proposed := model.Row{Values: make([]model.Value, len(c.tableLayouts))}
		defer clear(proposed.Values)
		for i, layout := range c.tableLayouts {
			proposed.Values[i] = model.NullVal(layout.Type.Storage)
		}
		for i, column := range c.assignmentColumns {
			proposed.Values[column] = assigned[i]
		}
		replacement, proceed, err := o.Triggers.BeforeRow(ctx, c.descriptor, model.TriggerInsert, nil, &proposed, o.MaxRecordBytes*8)
		if err != nil || !proceed {
			return false, err
		}
		if replacement == nil || len(replacement.Values) != len(proposed.Values) {
			return false, storage.ErrInvalidLayout
		}
		defer clear(replacement.Values)
		if err := c.finishMutation(ctx, frozen, storage.MutationInsert, nil, nil, replacement); err != nil {
			return false, err
		}
		return true, nil
	}
	var candidate *txn.InsertCandidate
	var proposed model.Row
	var route *partitionChild
	if o.Partitions != nil {
		candidate, route, proposed, err = o.Partitions.PrepareInsert(ctx, o.Transaction, columnIDs, assigned, o.Query.Allocator(runtime.WorkMemory), o.MaxRecordBytes*8, partitionExpressions(*o))
	} else {
		candidate, err = o.Transaction.PrepareInsert(ctx, txn.InsertPreparation{Target: c.target, ColumnIDs: columnIDs, Values: assigned, Allocator: o.Query.Allocator(runtime.WorkMemory), MaxBytes: o.MaxRecordBytes * 8, Expressions: partitionExpressions(*o)})
		if err == nil {
			proposed, _, err = candidate.CopyRow(ctx, o.MaxRecordBytes*8)
		}
	}
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, candidate.Close()) }()
	defer func() { clear(proposed.Values); proposed = model.Row{} }()
	if o.PartitionTarget != 0 && !o.Partitions.contains(route, o.PartitionTarget) {
		return false, &model.Error{Code: "23514", Message: "row violates partition constraint", Table: c.descriptor.Name().Name}
	}
	if o.Triggers != nil {
		triggerDescriptor := c.descriptor
		if route != nil {
			triggerDescriptor = route.descriptor
		}
		replacement, proceed, triggerErr := o.Triggers.BeforeRow(ctx, triggerDescriptor, model.TriggerInsert, nil, &proposed, o.MaxRecordBytes*8)
		if triggerErr != nil {
			return false, triggerErr
		}
		if !proceed {
			return false, nil
		}
		if replacement == nil || len(replacement.Values) != len(proposed.Values) {
			return false, storage.ErrInvalidLayout
		}
		if route != nil {
			selected, routeErr := o.Partitions.Route(ctx, replacement.Values, partitionExpressions(*o))
			if routeErr != nil {
				return false, routeErr
			}
			if selected != route {
				return false, logicalpartition.ErrNoPartition
			}
		}
		if err := candidate.Close(); err != nil {
			return false, err
		}
		clear(proposed.Values)
		target, ids := c.target, c.columnIDs
		if route != nil {
			target = route.target
			ids, err = o.Partitions.mapColumns(route, c.columnIDs)
			if err != nil {
				return false, err
			}
		}
		candidate, err = o.Transaction.PrepareInsert(ctx, txn.InsertPreparation{Target: target, ColumnIDs: ids, Values: replacement.Values, Allocator: o.Query.Allocator(runtime.WorkMemory), MaxBytes: o.MaxRecordBytes * 8, Expressions: partitionExpressions(*o)})
		if err != nil {
			return false, err
		}
		proposed, _, err = candidate.CopyRow(ctx, o.MaxRecordBytes*8)
		if err != nil {
			return false, err
		}
	}
	if o.Conflict != nil {
		owner, err := c.conflictOwner(ctx, &proposed, route)
		if err != nil {
			return false, err
		}
		if owner != nil {
			defer func() { clear(owner.Values); *owner = model.Row{} }()
			if o.Conflict.Action == ir.ConflictNothing {
				return false, nil
			}
			return c.updateConflict(ctx, frozen, route, owner, &proposed)
		}
		if o.Conflict.Action == ir.ConflictUpdate {
			if err := c.recordConflictIdentity(ctx, proposed.Values); err != nil {
				return false, err
			}
		}
	}
	if err := o.Transaction.ApplyPreparedInsert(ctx, candidate); err != nil {
		return false, err
	}
	if err := c.finishPartitionMutation(ctx, frozen, storage.MutationInsert, route, nil, nil, &proposed); err != nil {
		return false, err
	}
	return true, nil
}

func (c *cursor) updateConflict(ctx context.Context, frozen []model.Value, source *partitionChild, old, proposed *model.Row) (bool, error) {
	oldValues := c.visibleRow(old.Values, c.oldImage)
	proposedValues := c.visibleRow(proposed.Values, c.newImage)
	if len(c.viewMutationSteps) != 0 {
		projectedOld, _, err := c.projectViewRow(ctx, old, false, true, 0)
		if err != nil {
			return false, err
		}
		projectedProposed, _, err := c.projectViewRow(ctx, proposed, false, true, 1)
		if err != nil {
			return false, err
		}
		oldValues, proposedValues = projectedOld.Values, projectedProposed.Values
	}
	images := []model.Value{{Type: model.TypeComposite, ArrayVal: oldValues}, {Type: model.TypeComposite, ArrayVal: proposedValues}}
	defer clear(c.oldImage)
	defer clear(c.newImage)
	// Action programs see live OLD and sealed EXCLUDED, not the source snapshot.
	sourceInput := c.assignmentInput
	c.assignmentInput = c.conflictInput
	defer func() { c.assignmentInput = sourceInput }()
	if c.conflictState.predicate != nil {
		selected, err := c.mergePredicate(ctx, c.conflictState.predicate, images)
		if err != nil || !selected {
			return false, err
		}
	}
	if err := c.recordConflictIdentity(ctx, old.Values); err != nil {
		return false, err
	}
	assigned, err := c.assignments(ctx, images, c.conflictState.assignment, c.conflictState.columns)
	if err != nil {
		return false, err
	}
	if err := c.applyAssigned(ctx, frozen, storage.MutationUpdate, source, old.Values, old.Key, assigned, c.options.Conflict.AssignmentColumns); err != nil {
		return false, err
	}
	return true, nil
}

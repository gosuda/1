package exec

import (
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	exmodify "github.com/gosuda/supersql-alpha/internal/query/exec/modify"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/partition"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) modify(id physical.NodeID, node physical.NodeSpec, children []storage.BatchCursor) (storage.BatchCursor, error) {
	graph, _ := c.plan.Graph(node.Logical.Graph)
	relation, _ := graph.Rel(node.Logical.Rel)
	options := c.portal.env.Modify
	ref := storage.ObjectRef{ObjectID: relation.Object, DescriptorVersion: relation.DescriptorVersion, Generation: relation.Generation, EncodingVersion: relation.EncodingVersion}
	if c.portal.env.MutationTargets == nil {
		return nil, ErrInvalid
	}
	target, err := c.portal.env.MutationTargets.ResolveMutationTarget(c.portal.ctx, ref)
	if err != nil {
		return nil, err
	}
	descriptor := target.Descriptor
	if _, err := target.ResolveMutationTarget(c.portal.ctx, ref); err != nil {
		return nil, err
	}
	options.Conflict = nil
	options.Conflicts = nil
	options.Merge = nil
	switch relation.ModifyKind {
	case ir.Insert:
		options.Operation = storage.MutationInsert
	case ir.Update:
		options.Operation = storage.MutationUpdate
	case ir.Delete:
		options.Operation = storage.MutationDelete
	case ir.Merge:
		options.Operation = 0
	default:
		return nil, ErrInvalid
	}
	claim, err := c.portal.env.Query.Allocator(runtime.WorkMemory).Reserve(0)
	if err != nil {
		return nil, err
	}
	defer claim.Release()
	spec, err := descriptor.SpecWithin(c.portal.ctx, model.DescriptorLimits{MaxBytes: c.portal.env.Limits.MetadataBytes, MaxItems: c.portal.env.Limits.Expressions.MaxRegisters, MaxDepth: 64})
	if err != nil {
		return nil, err
	}
	options.Partitions = nil
	options.PartitionTarget = 0
	routing := target
	if target.PartitionRouting != nil {
		routing = *target.PartitionRouting
	}
	if routing.Descriptor == nil {
		return nil, ErrInvalid
	}
	routingTable, hasRoutingTable := routing.Descriptor.Table()
	if hasRoutingTable && routingTable.PartitionStrategy != 0 {
		children := make([]exmodify.PartitionChild, len(routing.PartitionChildren))
		for i, child := range routing.PartitionChildren {
			children[i] = compilePartitionChild(child)
		}
		partitionPlan, err := exmodify.NewPartitionPlan(c.portal.ctx, exmodify.PartitionPlanOptions{
			Root: routing.Descriptor, Input: descriptor, Children: children, Expressions: routing.PartitionExpressions,
			Encoder: c.portal.env.Keys, Builtin: c.portal.env.Builtin,
			Limits: partition.Limits{MaxChildren: int(c.portal.env.Limits.Expressions.MaxRegisters), MaxKeys: int(c.portal.env.Limits.Expressions.MaxRegisters), MaxBytes: c.portal.env.Limits.MetadataBytes},
		})
		if err != nil {
			return nil, err
		}
		if err := c.charge(partitionPlan.OwnedBytes()); err != nil {
			return nil, err
		}
		options.Partitions = partitionPlan
		if descriptor.ID() != routing.Descriptor.ID() {
			options.PartitionTarget = descriptor.ID()
		}
	}
	if spec.Table != nil && spec.Table.PartitionStrategy == 0 && relation.ModifyKind != ir.Insert && len(target.PartitionChildren) != 0 {
		inheritancePlan, err := exmodify.NewInheritancePlan(c.portal.ctx, compilePartitionChild(target))
		if err != nil {
			return nil, err
		}
		if err := c.charge(inheritancePlan.OwnedBytes()); err != nil {
			return nil, err
		}
		options.Partitions = inheritancePlan
	}
	var columns []model.ColumnDescriptor
	var primaryKey []model.KeyElement
	switch relation.ModifyTarget {
	case ir.PhysicalModifyTarget, ir.AutomaticViewModifyTarget:
		if spec.Name.Kind != model.ObjectTable && spec.Name.Kind != model.ObjectMaterializedView || spec.Table == nil {
			return nil, ErrInvalid
		}
		columns, primaryKey = spec.Table.Columns, spec.Table.PrimaryKey
	case ir.LogicalViewModifyTarget, ir.AutomaticViewTriggerModifyTarget:
		if spec.Name.Kind != model.ObjectView || spec.View == nil {
			return nil, ErrInvalid
		}
		columns = spec.View.Columns
		if !logicalViewTriggerCapable(graph, node.Logical.Rel, relation, spec.View.Triggers) {
			return nil, &RequiredExecutionError{Node: id, Algorithm: node.Algorithm, Reason: "applicable INSTEAD OF view trigger"}
		}
	default:
		return nil, ErrInvalid
	}
	metadata := uint64(len(columns))*uint64(unsafe.Sizeof(storage.ColumnLayout{})) + uint64(len(primaryKey))*(uint64(unsafe.Sizeof(storage.KeyColumn{}))+uint64(unsafe.Sizeof(int(0))))
	metadata += uint64(len(node.Output))*uint64(unsafe.Sizeof(ir.ScalarID(0))) + 2*uint64(unsafe.Sizeof(physical.Slot{}))
	if err = c.charge(metadata); err != nil {
		return nil, err
	}
	options.Query = c.portal.env.Query
	options.Descriptor = descriptor
	options.TargetKind = relation.ModifyTarget
	if relation.ModifyTarget == ir.PhysicalModifyTarget || relation.ModifyTarget == ir.AutomaticViewModifyTarget {
		options.Expansion = target.Expansion
	} else {
		options.Expansion = nil
	}
	options.Source = c.portal.env.Reader
	options.Transaction = &c.portal.mutations
	options.ConstraintTransaction = &c.portal.mutations
	options.Encoder = c.portal.env.Keys
	options.Parameters = c.portal.env.Parameters
	options.Subqueries = c
	options.Sequences = c.portal.env.Sequences
	options.Relations = c.portal.env.Relations
	options.Temporal = c.portal.env.Temporal
	options.Settings = c.portal.env.Settings
	options.TextSearch = c.portal.env.TextSearch
	options.Advisory = c.portal.env.Advisory
	options.Native = c.portal.env.Native
	options.Triggers = c.portal.env.Triggers
	options.BatchRows = c.portal.env.Limits.BatchRows
	options.BatchBytes = c.portal.env.Limits.BatchBytes
	options.Layout = make([]storage.ColumnLayout, len(columns))
	for i, column := range columns {
		options.Layout[i] = storage.ColumnLayout{ID: column.ID, Type: column.Layout.Type, Children: column.Layout.Children, Nullable: column.Nullable}
		if column.ConstantDefault != nil {
			options.Layout[i].HasMissingValue = true
			options.Layout[i].MissingValue = *column.ConstantDefault
		}
	}
	options.KeyLayout = make([]storage.KeyColumn, len(primaryKey))
	for i, key := range primaryKey {
		options.KeyLayout[i] = storage.KeyColumn{ColumnID: key.Column, Type: key.Type, Descending: key.Descending, NullsFirst: key.NullsFirst}
		for _, column := range options.Layout {
			if column.ID == key.Column {
				options.KeyLayout[i].Children = column.Children
				break
			}
		}
	}
	keyBytes, err := storage.KeyLayoutBytes(c.portal.ctx, options.KeyLayout, c.portal.env.Limits.MetadataBytes-c.used)
	if err != nil {
		return nil, err
	}
	if err := c.charge(keyBytes); err != nil {
		return nil, err
	}
	options.KeyLayout, _, err = storage.CopyKeyLayout(c.portal.ctx, options.KeyLayout, keyBytes)
	if err != nil {
		return nil, err
	}
	child, _ := c.plan.Node(node.Children[0])
	if err = c.charge(uint64(child.OutputCount()) * uint64(unsafe.Sizeof(physical.Slot{}))); err != nil {
		return nil, err
	}
	slots := make([]physical.Slot, child.OutputCount())
	for i := range slots {
		slots[i], _ = child.Output(uint32(i))
	}
	options.InputLayouts, err = c.layouts(slots)
	if err != nil {
		return nil, err
	}
	options.SourceObjectColumn = 0
	if relation.SourceObject != 0 {
		for i, slot := range slots {
			if slot.Graph == node.Logical.Graph && slot.ID == relation.SourceObject {
				options.SourceObjectColumn = uint32(i + 1)
				break
			}
		}
		if options.SourceObjectColumn == 0 {
			return nil, ErrInvalid
		}
	}
	options.OldKeyColumns = nil
	options.LogicalOldColumns = nil
	if options.Operation != storage.MutationInsert {
		if relation.ModifyTarget == ir.LogicalViewModifyTarget || relation.ModifyTarget == ir.AutomaticViewTriggerModifyTarget {
			options.LogicalOldColumns, err = c.logicalOldColumns(graph, slots, relation.Object, columns)
		} else {
			options.OldKeyColumns, err = c.originalKeyColumns(node, slots, options.KeyLayout)
		}
		if err != nil {
			return nil, err
		}
	}
	options.UpdatedColumns = nil
	if relation.UpdatedColumnCount() != 0 {
		if err = c.charge(uint64(relation.UpdatedColumnCount()) * uint64(unsafe.Sizeof(model.ColumnID(0)))); err != nil {
			return nil, err
		}
		options.UpdatedColumns = make([]model.ColumnID, relation.UpdatedColumnCount())
		for i := range options.UpdatedColumns {
			column, ok := graph.UpdatedColumn(node.Logical.Rel, uint32(i))
			if !ok {
				return nil, ErrInvalid
			}
			options.UpdatedColumns[i] = column
		}
	}
	options.Assignment = nil
	options.AssignmentColumns = nil
	if relation.AssignmentCount() != 0 {
		if err = c.charge(uint64(relation.AssignmentCount()) * (uint64(unsafe.Sizeof(ir.ScalarID(0))) + uint64(unsafe.Sizeof(model.ColumnID(0))))); err != nil {
			return nil, err
		}
		roots := make([]ir.ScalarID, relation.AssignmentCount())
		options.AssignmentColumns = make([]model.ColumnID, len(roots))
		for i := range roots {
			assignment, _ := graph.Assignment(node.Logical.Rel, uint32(i))
			info, _ := graph.Slot(assignment.Slot)
			if info.Object != relation.Object || info.Column == 0 {
				return nil, ErrInvalid
			}
			roots[i] = assignment.Expr
			options.AssignmentColumns[i] = info.Column
		}
		options.Assignment, err = c.program(node.Logical.Graph, roots, slots)
		if err != nil {
			return nil, err
		}
	}
	options.ViewMutationSteps, err = c.viewMutationPlan(node, graph, relation)
	if err != nil {
		return nil, err
	}
	if relation.ModifyKind == ir.Merge {
		options.Merge, err = c.mergePlan(node, graph, relation, slots)
		if err != nil {
			return nil, err
		}
	}
	if relation.Conflict != ir.NoConflict && (relation.ModifyTarget == ir.PhysicalModifyTarget || relation.ModifyTarget == ir.AutomaticViewModifyTarget) {
		options.Conflict, err = c.conflictPlan(node, graph, relation)
		if err != nil {
			return nil, err
		}
		if lookup, ok := target.Expansion.(exmodify.ConflictLookup); ok {
			options.Conflicts = lookup
		}
		needsLookup := relation.ConflictArbiterCount() == 0 && len(spec.Table.Indexes) != 0
		for _, arbiter := range options.Conflict.Arbiters {
			needsLookup = needsLookup || !arbiter.Primary
		}
		if needsLookup && options.Conflicts == nil {
			return nil, &RequiredExecutionError{Node: id, Algorithm: node.Algorithm, Reason: "unique conflict owner lookup"}
		}
	}
	options.Returning = nil
	options.ReturningInputLayouts = nil
	options.ReturningSourceColumns = nil
	options.ReturningTableOIDs = 0
	if relation.BindingCount() != 0 {
		roots := make([]ir.ScalarID, len(node.Output))
		for i, slot := range node.Output {
			for j := range relation.BindingCount() {
				binding, _ := graph.Binding(node.Logical.Rel, j)
				if binding.Slot == slot.ID {
					roots[i] = binding.Expr
					break
				}
			}
			if roots[i] == 0 {
				return nil, ErrInvalid
			}
		}
		options.ReturningSourceColumns, err = c.returningSourceColumns(graph, roots, slots)
		if err != nil {
			return nil, err
		}
		imageCount := 2
		if relation.ModifyKind == ir.Merge {
			imageCount = 4
		}
		oidCount := 0
		if relation.OldTableOID != 0 {
			oidCount = 2
			if relation.ModifyKind == ir.Merge {
				oidCount = 3
			}
		}
		options.ReturningTableOIDs = oidCount
		if err := c.charge(uint64(imageCount+oidCount+len(options.ReturningSourceColumns)) * uint64(unsafe.Sizeof(physical.Slot{}))); err != nil {
			return nil, err
		}
		images := make([]physical.Slot, imageCount+oidCount+len(options.ReturningSourceColumns))
		images[0] = physical.Slot{Graph: node.Logical.Graph, ID: relation.OldRow}
		images[1] = physical.Slot{Graph: node.Logical.Graph, ID: relation.NewRow}
		if relation.ModifyKind == ir.Merge {
			images[2] = physical.Slot{Graph: node.Logical.Graph, ID: relation.MergeResultRow}
			images[3] = physical.Slot{Graph: node.Logical.Graph, ID: relation.MergeActionSlot}
		}
		if oidCount != 0 {
			images[imageCount] = physical.Slot{Graph: node.Logical.Graph, ID: relation.OldTableOID}
			images[imageCount+1] = physical.Slot{Graph: node.Logical.Graph, ID: relation.NewTableOID}
			if oidCount == 3 {
				images[imageCount+2] = physical.Slot{Graph: node.Logical.Graph, ID: relation.MergeTableOID}
			}
		}
		for i, column := range options.ReturningSourceColumns {
			images[i+imageCount+oidCount] = slots[column]
		}
		for i := range images {
			info, ok := graph.Slot(images[i].ID)
			if !ok {
				return nil, ErrInvalid
			}
			images[i].Layout = info.Layout
			images[i].Nullable = info.Nullable
		}
		options.ReturningInputLayouts, err = c.layouts(images)
		if err != nil {
			return nil, err
		}
		options.Returning, err = c.program(node.Logical.Graph, roots, images)
		if err != nil {
			return nil, err
		}
	}
	if c.preparing {
		return nil, nil
	}
	cursor, err := exmodify.New(c.portal.ctx, children[0], options)
	if err == nil && id == c.plan.Root() {
		c.portal.mutation = cursor
	}
	return cursor, err
}

func compilePartitionChild(target MutationTarget) exmodify.PartitionChild {
	child := exmodify.PartitionChild{Descriptor: target.Descriptor, Expansion: target.Expansion, Expressions: target.PartitionExpressions}
	child.Children = make([]exmodify.PartitionChild, len(target.PartitionChildren))
	for i, nested := range target.PartitionChildren {
		child.Children[i] = compilePartitionChild(nested)
	}
	return child
}

func logicalViewTriggerCapable(graph *ir.Graph, relationID ir.RelID, relation ir.Rel, triggers []model.TriggerDescriptor) bool {
	events := make([]string, 0, 3)
	if relation.ModifyKind == ir.Merge {
		for i := range relation.ActionCount() {
			action, ok := graph.MergeAction(relationID, i)
			if !ok {
				return false
			}
			event := map[ir.ModifyKind]string{ir.Insert: "insert", ir.Update: "update", ir.Delete: "delete"}[action.Action]
			if event != "" && !slices.Contains(events, event) {
				events = append(events, event)
			}
		}
	} else {
		event := map[ir.ModifyKind]string{ir.Insert: "insert", ir.Update: "update", ir.Delete: "delete"}[relation.ModifyKind]
		if event == "" {
			return false
		}
		events = append(events, event)
	}
	for _, event := range events {
		if !slices.ContainsFunc(triggers, func(trigger model.TriggerDescriptor) bool {
			return trigger.EnableMode.Fires(false) && trigger.Timing == "instead" && trigger.ForEachRow && slices.Contains(trigger.Events, event)
		}) {
			return false
		}
	}
	return true
}

func (c *invocation) logicalOldColumns(graph *ir.Graph, slots []physical.Slot, object model.ObjectID, columns []model.ColumnDescriptor) ([]int, error) {
	result := make([]int, len(columns))
	for i, column := range columns {
		result[i] = -1
		for j, slot := range slots {
			info, ok := graph.Slot(slot.ID)
			if ok && info.Object == object && info.Column == column.ID {
				result[i] = j
				break
			}
		}
		if result[i] < 0 {
			return nil, ErrInvalid
		}
	}
	return result, nil
}

func (c *invocation) originalKeyColumns(node physical.NodeSpec, slots []physical.Slot, keys []storage.KeyColumn) ([]int, error) {
	graph, _ := c.plan.Graph(node.Logical.Graph)
	relation, _ := graph.Rel(node.Logical.Rel)
	explicit := relation.TargetKeyCount() != 0
	if explicit && int(relation.TargetKeyCount()) != len(keys) {
		return nil, ErrInvalid
	}
	if relation.ModifyKind == ir.Merge && !explicit {
		return nil, ErrInvalid
	}
	columns := make([]int, len(keys))
	for i, key := range keys {
		columns[i] = -1
		var targetKey model.SlotID
		if explicit {
			var ok bool
			targetKey, ok = graph.TargetKey(node.Logical.Rel, uint32(i))
			if !ok {
				return nil, ErrInvalid
			}
		}
		for j, slot := range slots {
			matches := slot.ID == targetKey
			if !explicit {
				info, _ := graph.Slot(slot.ID)
				matches = info.Object == relation.Object && info.Column == key.ColumnID
			}
			if matches {
				columns[i] = j
				break
			}
		}
		if columns[i] < 0 {
			return nil, &RequiredExecutionError{Node: c.current, Algorithm: node.Algorithm, Reason: "mutation input omits original key column"}
		}
	}
	return columns, nil
}

func (c *invocation) conflictPlan(node physical.NodeSpec, graph *ir.Graph, relation ir.Rel) (*exmodify.ConflictPlan, error) {
	bytes := uint64(relation.ConflictArbiterCount()) * uint64(unsafe.Sizeof(model.ConflictArbiter{}))
	bytes += uint64(relation.ConflictAssignmentCount()) * (uint64(unsafe.Sizeof(ir.ScalarID(0))) + uint64(unsafe.Sizeof(model.ColumnID(0))))
	bytes += 2 * uint64(unsafe.Sizeof(physical.Slot{}))
	if err := c.charge(bytes); err != nil {
		return nil, err
	}
	plan := &exmodify.ConflictPlan{Action: relation.Conflict, Arbiters: make([]model.ConflictArbiter, relation.ConflictArbiterCount())}
	for i := range plan.Arbiters {
		var ok bool
		plan.Arbiters[i], ok = graph.ConflictArbiter(node.Logical.Rel, uint32(i))
		if !ok {
			return nil, ErrInvalid
		}
	}
	if relation.Conflict == ir.ConflictNothing {
		return plan, nil
	}
	images := make([]physical.Slot, 2)
	for i, slot := range []model.SlotID{relation.OldRow, relation.ExcludedRow} {
		info, ok := graph.Slot(slot)
		if !ok {
			return nil, ErrInvalid
		}
		images[i] = physical.Slot{Graph: node.Logical.Graph, ID: slot, Layout: info.Layout, Nullable: info.Nullable}
	}
	var err error
	if relation.ConflictPredicate != 0 {
		plan.Predicate, err = c.program(node.Logical.Graph, []ir.ScalarID{relation.ConflictPredicate}, images)
		if err != nil {
			return nil, err
		}
	}
	if relation.ConflictAssignmentCount() == 0 {
		return plan, nil
	}
	roots := make([]ir.ScalarID, relation.ConflictAssignmentCount())
	plan.AssignmentColumns = make([]model.ColumnID, len(roots))
	for i := range roots {
		assignment, ok := graph.ConflictAssignment(node.Logical.Rel, uint32(i))
		if !ok {
			return nil, ErrInvalid
		}
		info, ok := graph.Slot(assignment.Slot)
		if !ok || info.Object != relation.Object || info.Column == 0 {
			return nil, ErrInvalid
		}
		roots[i], plan.AssignmentColumns[i] = assignment.Expr, info.Column
	}
	plan.Assignment, err = c.program(node.Logical.Graph, roots, images)
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func (c *invocation) viewMutationPlan(node physical.NodeSpec, graph *ir.Graph, relation ir.Rel) ([]exmodify.ViewMutationStep, error) {
	if relation.ViewMutationStepCount() == 0 {
		return nil, nil
	}
	bytes := uint64(relation.ViewMutationStepCount()) * uint64(unsafe.Sizeof(exmodify.ViewMutationStep{}))
	if err := c.charge(bytes); err != nil {
		return nil, err
	}
	steps := make([]exmodify.ViewMutationStep, relation.ViewMutationStepCount())
	for i := range steps {
		step, ok := graph.ViewMutationStep(node.Logical.Rel, uint32(i))
		if !ok {
			return nil, ErrInvalid
		}
		inputs := make([]physical.Slot, step.InputCount())
		inputColumns := make([]model.ColumnID, len(inputs))
		for j := range inputs {
			slot, found := graph.ViewMutationInput(node.Logical.Rel, uint32(i), uint32(j))
			info, infoFound := graph.Slot(slot)
			if !found || !infoFound || info.Column == 0 {
				return nil, ErrInvalid
			}
			inputs[j] = physical.Slot{Graph: node.Logical.Graph, ID: slot, Layout: info.Layout, Nullable: info.Nullable}
			inputColumns[j] = info.Column
		}
		projections := make([]ir.ScalarID, step.ProjectionCount())
		for j := range projections {
			var found bool
			projections[j], found = graph.ViewMutationProjection(node.Logical.Rel, uint32(i), uint32(j))
			if !found {
				return nil, ErrInvalid
			}
		}
		outputColumns := make([]model.ColumnID, step.OutputColumnCount())
		for j := range outputColumns {
			var found bool
			outputColumns[j], found = graph.ViewMutationOutputColumn(node.Logical.Rel, uint32(i), uint32(j))
			if !found {
				return nil, ErrInvalid
			}
		}
		projection, err := c.program(node.Logical.Graph, projections, inputs)
		if err != nil {
			return nil, err
		}
		var predicate *expr.Program
		if step.Check {
			predicate, err = c.program(node.Logical.Graph, []ir.ScalarID{step.Predicate}, inputs)
			if err != nil {
				return nil, err
			}
		}
		steps[i] = exmodify.ViewMutationStep{View: step.View, InputColumns: inputColumns, OutputColumns: outputColumns, Projection: projection, Predicate: predicate, Check: step.Check}
	}
	return steps, nil
}

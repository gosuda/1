package bind

import (
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type mutationTarget struct {
	descriptor          *model.Descriptor
	table               *model.TableMetadata
	qualifier           relationQualifier
	view                bool
	executionDescriptor *model.Descriptor
	executionTable      *model.TableMetadata
	viewLayers          []viewMutationLayer
	columnMap           map[model.ColumnID]model.ColumnID
	logicalTrigger      bool
}

// statementKind classifies the query statements this binder executes. Utility
// commands, object DDL, and statistics commands belong to the session's command
// paths and classify as statementNone.
type statementKind uint8

const (
	statementNone statementKind = iota
	statementSelect
	statementInsert
	statementUpdate
	statementDelete
	statementMerge
	statementCall
	statementCopy
)

func classifyStatement(node *zpgparse.Node) statementKind {
	if node == nil {
		return statementNone
	}
	switch {
	case node.GetSelectStmt() != nil:
		return statementSelect
	case node.GetInsertStmt() != nil:
		return statementInsert
	case node.GetUpdateStmt() != nil:
		return statementUpdate
	case node.GetDeleteStmt() != nil:
		return statementDelete
	case node.GetMergeStmt() != nil:
		return statementMerge
	case node.GetCallStmt() != nil:
		return statementCall
	case node.GetCopyStmt() != nil:
		return statementCopy
	}
	return statementNone
}

// IsStatement reports whether this binder executes the node as a query
// statement. Routine body compilers preview-bind only the statements binding
// owns; every other node is a command executed by the session, which binds it
// there against the services it needs.
func IsStatement(node *zpgparse.Node) bool { return classifyStatement(node) != statementNone }

func (b *binder) statement(node *zpgparse.Node, s *scope) (relation, error) {
	switch classifyStatement(node) {
	case statementSelect:
		return b.selectStatement(node.GetSelectStmt(), s, nil)
	case statementInsert:
		return b.insert(node.GetInsertStmt(), s)
	case statementUpdate:
		return b.update(node.GetUpdateStmt(), s)
	case statementDelete:
		return b.delete(node.GetDeleteStmt(), s)
	case statementMerge:
		return b.merge(node.GetMergeStmt(), s)
	case statementCall:
		return b.callStatement(node.GetCallStmt(), s)
	case statementCopy:
		return b.copyStatement(node.GetCopyStmt(), s)
	}
	return relation{}, missing("utility command binding", 0)
}
func (b *binder) modificationTarget(n *zpgparse.RangeVar) (mutationTarget, error) {
	if n == nil {
		return mutationTarget{}, bindError("42601", "missing modification target", 0)
	}
	if n.Catalogname != "" {
		return mutationTarget{}, bindError("0A000", "cross-database reference is not supported", int(n.Location)+1)
	}
	position := int(n.Location) + 1
	var d *model.Descriptor
	namespace := ""
	resolveSchema := func(schema string) (bool, error) {
		if _, logical, err := b.catalogRange(schema, n.Relname, position); err != nil {
			return false, err
		} else if logical {
			return false, bindError("42501", "permission denied for native catalog relation", position)
		}
		if schema == "pg_catalog" || schema == "information_schema" {
			return false, nil
		}
		descriptor, parent, found, err := b.resolveRangeInSchema(schema, n.Relname)
		if err != nil || !found {
			return false, err
		}
		if descriptor.Kind() == model.ObjectMaterializedView && b.options.MaterializedViewTarget == 0 {
			return false, nil
		}
		d, namespace = descriptor, parent.Name().Name
		return true, nil
	}
	if n.Schemaname != "" {
		found, err := resolveSchema(n.Schemaname)
		if err != nil {
			return mutationTarget{}, err
		}
		if !found {
			return mutationTarget{}, bindError("42P01", "object does not exist", position)
		}
	} else {
		if !slices.Contains(b.options.SearchPath, "pg_catalog") {
			if _, logical, err := b.catalogRange("pg_catalog", n.Relname, position); err != nil {
				return mutationTarget{}, err
			} else if logical {
				return mutationTarget{}, bindError("42501", "permission denied for native catalog relation", position)
			}
		}
		for _, schema := range b.options.SearchPath {
			found, err := resolveSchema(schema)
			if err != nil {
				return mutationTarget{}, err
			}
			if found {
				break
			}
		}
		if d == nil {
			return mutationTarget{}, bindError("42P01", "object does not exist", position)
		}
	}
	if d.Kind() == model.ObjectMaterializedView && d.ID() != b.options.MaterializedViewTarget {
		return mutationTarget{}, bindError("42809", "materialized view cannot be modified directly", int(n.Location)+1)
	}
	spec, err := b.descriptorSpec(d)
	if err != nil {
		return mutationTarget{}, err
	}
	var table *model.TableMetadata
	// A materialized view stores its rows in its own backing table, so it is a
	// physical modification target like a table: only a plain view routes
	// through the automatic-update mapping or INSTEAD OF triggers.
	view := spec.Table == nil && spec.View != nil
	switch {
	case spec.Table != nil:
		table = spec.Table
	case spec.View != nil:
		columns := slices.Clone(spec.View.Columns)
		for i := range columns {
			columns[i].Nullable = true
		}
		table = &model.TableMetadata{Columns: columns, Triggers: spec.View.Triggers}
	default:
		return mutationTarget{}, bindError("42809", "modification target is not a table or view", int(n.Location)+1)
	}
	for _, column := range table.Columns {
		if column.State != model.StatePublic {
			continue
		}
		if column.IdentitySequence != 0 {
			published, err := b.catalog.Descriptor(b.ctx, column.IdentitySequence)
			if err != nil {
				return mutationTarget{}, err
			}
			sequence := published.Descriptor
			metadata, ok := sequence.Sequence()
			if !ok || sequence.State() != model.StatePublic || metadata.OwnerTable != d.ID() || metadata.OwnerColumn != column.ID || metadata.Type.Storage != model.TypeInt64 {
				return mutationTarget{}, model.ErrInvalidDescriptor
			}
			switch metadata.Type.ID {
			case 20, 21, 23:
			default:
				return mutationTarget{}, model.ErrInvalidDescriptor
			}
			b.dependency(sequence, 0)
			b.dependency(d, column.ID)
		}
	}
	qualifier := relationQualifier{namespace: namespace, name: d.Name().Name}
	if n.Alias != nil {
		qualifier = relationQualifier{name: n.Alias.Aliasname}
	}
	return mutationTarget{descriptor: d, table: table, qualifier: qualifier, view: view}, nil
}
func (b *binder) modification(s *scope, target mutationTarget, input relation, kind ir.ModifyKind, assignments []ir.Binding, updated []model.ColumnID, clause *zpgparse.ReturningClause, conflict *zpgparse.OnConflictClause) (relation, error) {
	d := target.executionDescriptor
	if d == nil {
		d = target.descriptor
	}
	spec, columns, err := b.returning(s, target, kind, clause)
	if err != nil {
		return relation{}, err
	}
	if conflict != nil {
		if err := b.bindConflict(s, target, conflict, &spec); err != nil {
			return relation{}, err
		}
	}
	viewSteps, viewEffects, err := b.bindViewMutationSteps(s, target)
	if err != nil {
		return relation{}, err
	}
	effects := input.effects.Union(spec.Effects).Union(viewEffects).Union(ir.Effects{Volatility: ir.Volatile, MayError: true})
	spec.Kind = ir.Modify
	spec.Scope = s.id
	spec.Object = d.ID()
	spec.DescriptorVersion = d.Version()
	spec.ModifyTarget = ir.PhysicalModifyTarget
	spec.Generation = d.Physical().Generation
	spec.EncodingVersion = d.Physical().EncodingVersion
	if target.logicalTrigger {
		spec.ModifyTarget = ir.LogicalViewModifyTarget
		spec.Generation, spec.EncodingVersion = 0, 0
	}
	if target.automatic() {
		spec.LogicalView = target.descriptor.ID()
		spec.ViewMutationSteps = viewSteps
		spec.ModifyTarget = ir.AutomaticViewModifyTarget
		if target.logicalTrigger {
			spec.ModifyTarget = ir.AutomaticViewTriggerModifyTarget
		}
	}
	spec.ModifyKind = kind
	spec.Effects = effects
	spec.Inputs = []ir.RelID{input.id}
	spec.Assignments = assignments
	spec.UpdatedColumns = append(spec.UpdatedColumns, updated...)
	if kind != ir.Insert {
		for i := len(input.columns) - 1; i >= 0; i-- {
			column := input.columns[i]
			if column.internal && column.Name == "" && column.Layout.Type.Storage == model.TypeInt64 {
				spec.SourceObject = column.Slot
				break
			}
		}
	}
	id, err := b.builder.AddRel(b.ctx, spec)
	return relation{id: id, columns: columns, effects: effects}, err
}
func publicColumns(table *model.TableMetadata) []model.ColumnDescriptor {
	columns := make([]model.ColumnDescriptor, 0, len(table.Columns))
	for _, column := range table.Columns {
		if column.State == model.StatePublic && !column.SystemIdentity {
			columns = append(columns, column)
		}
	}
	return columns
}
func (b *binder) insert(n *zpgparse.InsertStmt, s *scope) (relation, error) {
	if n.WithClause != nil {
		if err := b.with(n.WithClause, s); err != nil {
			return relation{}, err
		}
	}
	target, err := b.modificationTarget(n.Relation)
	if err != nil {
		return relation{}, err
	}
	d, table := target.descriptor, target.table
	target, err = b.prepareMutationTarget(target, []string{"insert"})
	if err != nil {
		return relation{}, err
	}
	columns := publicColumns(table)
	targets := columns
	paths := make([][]*zpgparse.Node, len(targets))
	if len(n.Cols) > 0 {
		targets = nil
		paths = nil
		var seen []*zpgparse.ResTarget
		for _, node := range n.Cols {
			target := node.GetResTarget()
			if target == nil {
				return relation{}, bindError("42601", "invalid INSERT column", 0)
			}
			duplicate := slices.ContainsFunc(seen, func(previous *zpgparse.ResTarget) bool {
				if previous.Name != target.Name || len(previous.Indirection) != len(target.Indirection) {
					return false
				}
				for i := range previous.Indirection {
					if !equivalentAST(previous.Indirection[i].ProtoReflect(), target.Indirection[i].ProtoReflect()) {
						return false
					}
				}
				return true
			})
			if duplicate {
				return relation{}, bindError("42701", "column specified more than once", int(target.Location)+1)
			}
			seen = append(seen, target)
			found := false
			for _, c := range columns {
				if c.Name == target.Name {
					targets = append(targets, c)
					paths = append(paths, target.Indirection)
					found = true
					break
				}
			}
			if !found {
				return relation{}, bindError("42703", "INSERT column does not exist", int(target.Location)+1)
			}
		}
	}
	var input relation
	if n.SelectStmt == nil {
		id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: s.id, Rows: 1}})
		if err != nil {
			return relation{}, err
		}
		input.id = id
		targets = nil
		paths = nil
	} else {
		sourceTargets := slices.Clone(targets)
		for i := range sourceTargets {
			sourceTargets[i] = target.assignmentColumn(sourceTargets[i])
			if len(paths[i]) == 0 {
				continue
			}
			sourceTargets[i].Layout, err = b.assignmentTargetLayout(sourceTargets[i].Layout, paths[i], int(n.Cols[i].GetResTarget().Location)+1)
			if err != nil {
				return input, err
			}
			sourceTargets[i].Nullable = true
		}
		input, err = b.insertSource(n.SelectStmt.GetSelectStmt(), s, sourceTargets, len(n.Cols) != 0, n.Override)
		if err != nil {
			return input, err
		}
		targets = targets[:len(input.columns)]
		paths = paths[:len(input.columns)]
	}
	assignments := make([]ir.Binding, 0, len(columns))
	var identityError error
	owners, err := target.automaticInsertOwners(columns, func(column model.ColumnID) bool {
		return slices.ContainsFunc(targets, func(supplied model.ColumnDescriptor) bool {
			return supplied.ID == column
		})
	})
	if err != nil {
		return input, err
	}
	for _, c := range columns {
		if target.automatic() {
			executionColumn, writable := target.executionColumn(c.ID)
			if !writable {
				if slices.ContainsFunc(targets, func(supplied model.ColumnDescriptor) bool { return supplied.ID == c.ID }) {
					b.deferWriteError(bindError("0A000", "cannot insert into a non-updatable view column", 0))
				}
				continue
			}
			if owners[executionColumn.ID] != c.ID {
				continue
			}
			c = target.assignmentColumn(c)
		}
		if c.Generated != nil {
			if slices.ContainsFunc(targets, func(t model.ColumnDescriptor) bool { return t.ID == c.ID }) && !simpleInsertValues(n.SelectStmt.GetSelectStmt()) {
				b.deferWriteError(bindError("428C9", "cannot insert a non-DEFAULT value into generated column", 0))
			}
			continue
		}
		matches := make([]int, 0, 1)
		direct := -1
		for i, target := range targets {
			if target.ID != c.ID {
				continue
			}
			matches = append(matches, i)
			if len(paths[i]) == 0 {
				if direct >= 0 || len(matches) > 1 {
					return input, bindError("42701", "column specified more than once", 0)
				}
				direct = i
			} else if direct >= 0 {
				return input, bindError("42701", "column specified more than once", 0)
			}
		}
		supplied := len(matches) != 0
		if supplied && c.IdentitySequence != 0 && n.Override == zpgparse.OverridingKind_OVERRIDING_USER_VALUE {
			// PostgreSQL still type-checks the source expression here. OVERRIDING
			// USER VALUE changes which value is USED (the generated one), not
			// whether the source is assignable to the column, so a mismatched
			// source type must still raise 42804. Coerce before discarding,
			// matching HEAD's behaviour.
			for _, index := range append([]int(nil), matches...) {
				value, err := b.reference(s, input.columns[index], 0)
				if err != nil {
					return input, err
				}
				if _, err := b.coerceColumnAssignment(value, s, c); err != nil {
					return input, err
				}
			}
			supplied = false
			matches = nil
			direct = -1
		}
		if supplied && c.IdentityAlways && n.Override != zpgparse.OverridingKind_OVERRIDING_SYSTEM_VALUE && !simpleInsertValues(n.SelectStmt.GetSelectStmt()) {
			identityError = bindError("428C9", "cannot insert a non-DEFAULT value into identity column", 0)
		}
		var e expression
		if direct >= 0 {
			e, err = b.reference(s, input.columns[direct], 0)
			if err == nil {
				e, err = b.coerceColumnAssignment(e, s, c)
			}
		} else {
			e, err = b.columnDefault(c, s)
			if err == nil {
				e, err = b.coerceColumnAssignment(e, s, c)
			}
			for _, match := range matches {
				if err != nil {
					break
				}
				value, referenceErr := b.reference(s, input.columns[match], 0)
				if referenceErr != nil {
					err = referenceErr
					break
				}
				e, err = b.storeAssignment(s, e, paths[match], value, int(n.Relation.Location)+1)
			}
		}
		if err != nil {
			return input, err
		}
		destinationDescriptor, destinationColumn := d, c.ID
		if target.automatic() {
			executionColumn, _ := target.executionColumn(c.ID)
			destinationDescriptor, destinationColumn = target.executionDescriptor, executionColumn.ID
		}
		slot, err := b.slot(s, c.Layout, c.Nullable, destinationDescriptor.ID(), destinationColumn)
		if err != nil {
			return input, err
		}
		assignments = append(assignments, ir.Binding{Slot: slot, Expr: e.id})
		input.effects = input.effects.Union(e.effects)
		b.dependency(d, c.ID)
		b.dependency(destinationDescriptor, destinationColumn)
	}
	if target.automatic() {
		for _, column := range publicColumns(target.executionTable) {
			assigned := slices.ContainsFunc(assignments, func(binding ir.Binding) bool {
				info, ok := b.builder.Slot(binding.Slot)
				return ok && info.Object == target.executionDescriptor.ID() && info.Column == column.ID
			})
			if assigned || column.Generated != nil {
				continue
			}
			value, defaultErr := b.columnDefault(column, s)
			if defaultErr != nil {
				return input, defaultErr
			}
			value, defaultErr = b.coerceColumnAssignment(value, s, column)
			if defaultErr != nil {
				return input, defaultErr
			}
			slot, slotErr := b.slot(s, column.Layout, column.Nullable, target.executionDescriptor.ID(), column.ID)
			if slotErr != nil {
				return input, slotErr
			}
			assignments = append(assignments, ir.Binding{Slot: slot, Expr: value.id})
			input.effects = input.effects.Union(value.effects)
			b.dependency(target.executionDescriptor, column.ID)
		}
	}
	b.deferWriteError(identityError)
	return b.modification(s, target, input, ir.Insert, assignments, nil, n.ReturningClause, n.OnConflictClause)
}
func (b *binder) columnDefault(c model.ColumnDescriptor, s *scope) (expression, error) {
	if c.Generated != nil {
		return expression{}, missing("generated scalar default", 0)
	}
	if c.IdentitySequence != 0 {
		published, err := b.catalog.Descriptor(b.ctx, c.IdentitySequence)
		if err != nil {
			return expression{}, err
		}
		b.dependency(published.Descriptor, 0)
		argument, err := b.addScalar(s, expression{typ: b.primitive(2205), value: model.Int64Val(int64(published.Descriptor.OID()))}, ir.Scalar{Kind: ir.Const}, nil)
		if err != nil {
			return expression{}, err
		}
		call, found, err := b.builtinFunction("nextval", callArguments{values: []expression{argument}}, s, 0, false)
		if err != nil {
			return expression{}, err
		}
		if !found {
			return expression{}, bindError("42883", "identity sequence function does not exist", 0)
		}
		return b.invoke(s, call.signature, 0, call.arguments, ir.Call, 0, 0)
	}
	if c.Default != nil {
		return b.routineDefault(c.Default, c.Layout, s, 0)
	}
	value := model.NullVal(c.Layout.Type.Storage)
	if c.ConstantDefault != nil {
		value = *c.ConstantDefault
	}
	return b.addScalar(s, expression{typ: c.Layout, nullable: value.IsNull, value: value}, ir.Scalar{Kind: ir.Const}, nil)
}
func (b *binder) update(n *zpgparse.UpdateStmt, s *scope) (relation, error) {
	if n.WithClause != nil {
		if err := b.with(n.WithClause, s); err != nil {
			return relation{}, err
		}
	}
	target, err := b.modificationTarget(n.Relation)
	if err != nil {
		return relation{}, err
	}
	target, err = b.prepareMutationTarget(target, []string{"update"})
	if err != nil {
		return relation{}, err
	}
	previousProvenance := b.mutationProvenance
	if target.automatic() {
		b.mutationProvenance = target.executionDescriptor.ID()
	}
	input, err := b.from(&zpgparse.Node{Node: &zpgparse.Node_RangeVar{RangeVar: n.Relation}}, s)
	b.mutationProvenance = previousProvenance
	if err != nil {
		return input, err
	}
	for _, node := range n.FromClause {
		right, err := b.from(node, s)
		if err != nil {
			return input, err
		}
		input, err = b.cross(input, right, s)
		if err != nil {
			return input, err
		}
	}
	s.columns = input.columns
	s.namespaces = input.namespaces
	if n.WhereClause != nil {
		predicate, err := b.scalar(n.WhereClause, s)
		if err != nil {
			return input, err
		}
		predicate, err = b.coerce(s, predicate, b.primitive(16), ir.ImplicitCast)
		if err != nil {
			return input, err
		}
		input, err = b.filter(input, s, predicate)
		if err != nil {
			return input, err
		}
	}
	assignments, updated, effects, err := b.updateAssignments(n.TargetList, s, target)
	if err != nil {
		return input, err
	}
	input.effects = input.effects.Union(effects)
	return b.modification(s, target, input, ir.Update, assignments, updated, n.ReturningClause, nil)
}

func (b *binder) updateAssignments(nodes []*zpgparse.Node, s *scope, mutation mutationTarget) ([]ir.Binding, []model.ColumnID, ir.Effects, error) {
	d, table := mutation.descriptor, mutation.table
	var assignments []ir.Binding
	var updated []model.ColumnID
	var effects ir.Effects
	var identityError error
	written := make(map[model.ColumnID]bool)
	seen := make(map[model.ColumnID]bool)
	var tuple expression
	var tupleSource *zpgparse.Node
	tupleCount, tupleNext := 0, 0
	for _, node := range nodes {
		target := node.GetResTarget()
		if target == nil {
			return nil, nil, effects, bindError("42601", "invalid UPDATE assignment", 0)
		}
		position := int(target.Location) + 1
		index := slices.IndexFunc(table.Columns, func(c model.ColumnDescriptor) bool {
			return c.Name == target.Name && c.State == model.StatePublic && !c.SystemIdentity
		})
		if index < 0 {
			return nil, nil, effects, bindError("42703", "UPDATE column does not exist", position)
		}
		c := table.Columns[index]
		if seen[c.ID] {
			return nil, nil, effects, bindError("42701", "multiple assignments to same column", position)
		}
		seen[c.ID] = true
		assignmentColumn := mutation.assignmentColumn(c)

		var value expression
		multi := target.Val.GetMultiAssignRef()
		if multi == nil {
			if tupleNext != 0 {
				return nil, nil, effects, bindError("42601", "incomplete multiple assignment", position)
			}
			var err error
			value, err = b.assignmentInput(target.Val, s, assignmentColumn)
			if err != nil {
				return nil, nil, effects, err
			}
		} else {
			count, ordinal := int(multi.Ncolumns), int(multi.Colno)
			if count < 1 || ordinal < 1 || ordinal > count || multi.Source == nil {
				return nil, nil, effects, bindError("42601", "invalid multiple assignment", position)
			}
			if ordinal == 1 {
				if tupleNext != 0 {
					return nil, nil, effects, bindError("42601", "overlapping multiple assignment", position)
				}
				var err error
				tuple, err = b.multiAssignmentTuple(multi.Source, count, s, position)
				if err != nil {
					return nil, nil, effects, err
				}
				tupleSource, tupleCount, tupleNext = multi.Source, count, 1
			}
			if tupleNext != ordinal || tupleCount != count || (multi.Source != tupleSource && !equivalentAST(multi.Source.ProtoReflect(), tupleSource.ProtoReflect())) {
				return nil, nil, effects, bindError("42601", "invalid multiple assignment sequence", position)
			}
			var err error
			value, err = b.compositeFieldAt(s, tuple, ordinal-1, position)
			if err != nil {
				return nil, nil, effects, err
			}
			tupleNext++
			if tupleNext > tupleCount {
				tuple, tupleSource, tupleCount, tupleNext = expression{}, nil, 0, 0
			}
		}
		destinationDescriptor, destinationColumn := d, c.ID
		if mutation.automatic() {
			executionColumn, writable := mutation.executionColumn(c.ID)
			if !writable {
				b.deferWriteError(bindError("0A000", "cannot update a non-updatable view column", position))
				continue
			}
			destinationDescriptor, destinationColumn = mutation.executionDescriptor, executionColumn.ID
			if written[destinationColumn] {
				return nil, nil, effects, bindError("42701", "multiple assignments to same base column", position)
			}
			written[destinationColumn] = true
		}
		if !slices.Contains(updated, destinationColumn) {
			updated = append(updated, destinationColumn)
		}

		if assignmentColumn.Generated != nil {
			if target.Val.GetSetToDefault() == nil {
				b.deferWriteError(bindError("428C9", "cannot assign a non-DEFAULT value to generated column", position))
			}
			b.dependency(destinationDescriptor, destinationColumn)
			continue
		}
		if assignmentColumn.IdentityAlways && target.Val.GetSetToDefault() == nil && identityError == nil {
			identityError = bindError("428C9", "identity column can only be updated to DEFAULT", position)
		}
		var err error
		if len(target.Indirection) == 0 {
			value, err = b.coerceColumnAssignment(value, s, assignmentColumn)
		} else {
			base, baseErr := b.assignmentBase(s, mutation, c, position)
			if baseErr != nil {
				return nil, nil, effects, baseErr
			}
			value, err = b.storeAssignment(s, base, target.Indirection, value, position)
		}
		if err != nil {
			return nil, nil, effects, err
		}
		slot, err := b.slot(s, assignmentColumn.Layout, assignmentColumn.Nullable, destinationDescriptor.ID(), destinationColumn)
		if err != nil {
			return nil, nil, effects, err
		}
		assignments = append(assignments, ir.Binding{Slot: slot, Expr: value.id})
		effects = effects.Union(value.effects)
		b.dependency(d, c.ID)
		b.dependency(destinationDescriptor, destinationColumn)
	}
	if tupleNext != 0 {
		return nil, nil, effects, bindError("42601", "incomplete multiple assignment", 0)
	}
	b.deferWriteError(identityError)
	return assignments, updated, effects, nil
}

func (b *binder) multiAssignmentTuple(source *zpgparse.Node, count int, s *scope, position int) (expression, error) {
	if sublink := source.GetSubLink(); sublink != nil && sublink.SubLinkType == zpgparse.SubLinkType_EXPR_SUBLINK {
		child, err := b.newScope(s)
		if err != nil {
			return expression{}, err
		}
		query := sublink.Subselect.GetSelectStmt()
		if query == nil {
			return expression{}, bindError("42601", "multiple assignment subquery must be SELECT", position)
		}
		rel, err := b.selectStatement(query, child, nil)
		if err != nil {
			return expression{}, err
		}
		if len(rel.columns) != count {
			return expression{}, bindError("42601", "number of columns does not match number of values", position)
		}
		layout := b.primitive(2249)
		layout.Children = make([]model.TypeLayout, count)
		fields := slices.Clone(rel.columns)
		for i, column := range rel.columns {
			layout.Children[i] = column.Layout
		}
		value := expression{
			typ: layout, position: position, effects: rel.effects.Union(ir.Effects{MayError: true}),
			reads: b.captures[child.id], record: recordDeclared, recordFields: fields,
		}
		value, err = b.addScalar(s, value, ir.Scalar{Kind: ir.Subquery, Relation: rel.id, SubqueryKind: ir.TupleSubquery}, nil)
		if err != nil {
			return expression{}, err
		}
		return b.once(s, value)
	}
	value, err := b.scalar(source, s)
	if err != nil {
		return expression{}, err
	}
	if value.typ.Type.Storage != model.TypeComposite || len(value.typ.Children) != count {
		return expression{}, bindError("42804", "multiple assignment source has incompatible row type", position)
	}
	return b.once(s, value)
}

func (b *binder) assignmentBase(s *scope, mutation mutationTarget, descriptor model.ColumnDescriptor, position int) (expression, error) {
	column, err := s.lookup([]string{mutation.qualifier.name, descriptor.Name}, position)
	if err != nil {
		return expression{}, err
	}
	return b.reference(s, column, position)
}

func (b *binder) storeAssignment(s *scope, base expression, path []*zpgparse.Node, value expression, position int) (expression, error) {
	if len(path) == 0 {
		converted, err := b.coerce(s, value, base.typ, ir.AssignmentCast)
		if err != nil {
			return expression{}, err
		}
		return b.relabelCollation(s, converted, base.typ.Type.Collation)
	}
	if base.typ.Type.ID == 3802 {
		return b.jsonbStoreAssignment(s, base, path, value, position)
	}
	if field := path[0].GetString_(); field != nil {
		index, err := b.compositeFieldIndex(base, field.Sval, position)
		if err != nil {
			return expression{}, err
		}
		current, err := b.compositeFieldAt(s, base, index, position)
		if err != nil {
			return expression{}, err
		}
		replacement, err := b.storeAssignment(s, current, path[1:], value, position)
		if err != nil {
			return expression{}, err
		}
		result := expression{
			typ: base.typ, position: position, record: base.record,
			recordFields: base.recordFields,
		}
		return b.addScalar(s, result, ir.Scalar{Kind: ir.FieldStore, FieldIndex: uint32(index)}, []expression{base, replacement})
	}
	end := 0
	for end < len(path) && path[end].GetAIndices() != nil {
		end++
	}
	if end == 0 {
		return expression{}, bindError("42601", "invalid assignment indirection", position)
	}
	current, payload, operands, err := b.arraySubscriptPlan(base, path[:end], s)
	if err != nil {
		return expression{}, err
	}
	replacement, err := b.storeAssignment(s, current, path[end:], value, position)
	if err != nil {
		return expression{}, err
	}
	slice := payload.Kind == ir.ArraySlice
	payload.Kind = ir.ArrayStore
	payload.SliceAssignment = slice
	if !slice {
		payload.Subscripts.Dimensions = uint8(end)
	}
	operands = append(operands, replacement)
	result := expression{
		typ: base.typ, position: position, effects: ir.Effects{MayError: true},
		record: base.record, recordFields: base.recordFields,
	}
	return b.addScalar(s, result, payload, operands)
}

func (b *binder) assignmentTargetLayout(layout model.TypeLayout, path []*zpgparse.Node, position int) (model.TypeLayout, error) {
	if layout.Type.ID == 3802 {
		for _, node := range path {
			index := node.GetAIndices()
			if index == nil || index.IsSlice || index.Uidx == nil || index.Lidx != nil {
				return model.TypeLayout{}, bindError("42804", "jsonb assignment target requires element subscripts", position)
			}
		}
		return layout, nil
	}
	for start := 0; start < len(path); {
		if field := path[start].GetString_(); field != nil {
			source := expression{typ: layout}
			if len(layout.FieldNames) == len(layout.Children) && len(layout.FieldNames) != 0 {
				source.record = recordDeclared
				source.recordFields = make([]column, len(layout.Children))
				for i := range layout.Children {
					source.recordFields[i] = column{ResultColumn: ResultColumn{Name: layout.FieldNames[i], Layout: layout.Children[i], Nullable: true}}
				}
			}
			index, err := b.compositeFieldIndex(source, field.Sval, position)
			if err != nil {
				return model.TypeLayout{}, err
			}
			layout = layout.Children[index]
			start++
			continue
		}
		end, slice := start, false
		for end < len(path) {
			index := path[end].GetAIndices()
			if index == nil {
				break
			}
			slice = slice || index.IsSlice
			end++
		}
		if end == start || layout.Type.Storage != model.TypeArray || len(layout.Children) != 1 {
			return model.TypeLayout{}, bindError("42804", "assignment target is not subscriptable", position)
		}
		if end-start > 6 {
			return model.TypeLayout{}, bindError("54000", "number of array dimensions exceeds the maximum allowed (6)", position)
		}
		if !slice {
			layout = layout.Children[0]
		}
		start = end
	}
	return layout, nil
}
func (b *binder) delete(n *zpgparse.DeleteStmt, s *scope) (relation, error) {
	if n.WithClause != nil {
		if err := b.with(n.WithClause, s); err != nil {
			return relation{}, err
		}
	}
	target, err := b.modificationTarget(n.Relation)
	if err != nil {
		return relation{}, err
	}
	target, err = b.prepareMutationTarget(target, []string{"delete"})
	if err != nil {
		return relation{}, err
	}
	previousProvenance := b.mutationProvenance
	if target.automatic() {
		b.mutationProvenance = target.executionDescriptor.ID()
	}
	input, err := b.from(&zpgparse.Node{Node: &zpgparse.Node_RangeVar{RangeVar: n.Relation}}, s)
	b.mutationProvenance = previousProvenance
	if err != nil {
		return input, err
	}
	for _, node := range n.UsingClause {
		right, err := b.from(node, s)
		if err != nil {
			return input, err
		}
		input, err = b.cross(input, right, s)
		if err != nil {
			return input, err
		}
	}
	s.columns = input.columns
	s.namespaces = input.namespaces
	if n.WhereClause != nil {
		predicate, err := b.scalar(n.WhereClause, s)
		if err != nil {
			return input, err
		}
		predicate, err = b.coerce(s, predicate, b.primitive(16), ir.ImplicitCast)
		if err != nil {
			return input, err
		}
		input, err = b.filter(input, s, predicate)
		if err != nil {
			return input, err
		}
	}
	return b.modification(s, target, input, ir.Delete, nil, nil, n.ReturningClause, nil)
}

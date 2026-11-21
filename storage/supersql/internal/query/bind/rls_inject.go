package bind

import (
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// applyScanRLS wraps a physical table scan in a security-barrier Filter
// enforcing all applicable Row-Level Security USING policies for the current
// role and command. Wrapping the scan directly (before any JOIN) preserves
// outer-join semantics automatically and covers every query form uniformly.
func (b *binder) applyScanRLS(scan relation, descriptor, namespace *model.Descriptor, table *model.TableMetadata, s *scope, position int) (relation, error) {
	if table == nil || !table.RowSecurity || descriptor == nil {
		return scan, nil
	}
	roles, err := b.cachedRoles()
	if err != nil {
		return relation{}, err
	}
	if !rlsApplies(roles, b.options.CurrentRole, table) {
		return scan, nil
	}
	if b.rlsStack == nil {
		b.rlsStack = make(map[model.ObjectID]bool)
	}
	if b.rlsStack[descriptor.ID()] {
		return relation{}, bindError("42P17", `infinite recursion detected in policy for relation "`+descriptor.Name().Name+`"`, position)
	}
	b.rlsStack[descriptor.ID()] = true
	defer delete(b.rlsStack, descriptor.ID())

	// Build an isolated policy scope exposing only the target table's columns
	// under its canonical table name so policy expressions cannot accidentally
	// capture columns from enclosing queries or aliases.
	tableName := descriptor.Name().Name
	schemaName := ""
	if namespace != nil {
		schemaName = namespace.Name().Name
	}
	policyCols := make([]column, 0, len(scan.columns))
	for _, col := range scan.columns {
		if col.internal {
			continue
		}
		c := col
		c.qualifier = tableName
		c.namespace = schemaName
		c.hidden = false
		policyCols = append(policyCols, c)
	}
	policyScope := &scope{
		id:      s.id,
		columns: policyCols,
		namespaces: []relationNamespace{{
			qualifier:     relationQualifier{namespace: schemaName, name: tableName},
			descriptor:    descriptor,
			columns:       policyCols,
			systemColumns: true,
		}},
		ctes: make(map[string]cteBinding),
	}

	// Determine which USING command sets apply to this scan.
	commands := []string{"SELECT"}
	if b.rlsTargetID == descriptor.ID() && b.rlsTargetCommand != "" {
		switch b.rlsTargetCommand {
		case "UPDATE":
			commands = []string{"UPDATE"}
			if tableHasCommandUsingPolicy(table.Policies, "SELECT") {
				commands = append(commands, "SELECT")
			}
		case "DELETE":
			commands = []string{"DELETE"}
			if tableHasCommandUsingPolicy(table.Policies, "SELECT") {
				commands = append(commands, "SELECT")
			}
		case "MERGE":
			commands = []string{"SELECT"}
			if b.rlsMergeHasUpdate && tableHasCommandUsingPolicy(table.Policies, "UPDATE") {
				commands = append(commands, "UPDATE")
			}
			if b.rlsMergeHasDelete && tableHasCommandUsingPolicy(table.Policies, "DELETE") {
				commands = append(commands, "DELETE")
			}
		}
	}

	// Save and clear outer DML target context before binding policy expressions
	// so any tables referenced inside policy subqueries use SELECT policies.
	savedTargetID, savedTargetCmd := b.rlsTargetID, b.rlsTargetCommand
	savedMergeUpdate, savedMergeDelete := b.rlsMergeHasUpdate, b.rlsMergeHasDelete
	b.rlsTargetID, b.rlsTargetCommand, b.rlsMergeHasUpdate, b.rlsMergeHasDelete = 0, "", false, false
	defer func() {
		b.rlsTargetID, b.rlsTargetCommand = savedTargetID, savedTargetCmd
		b.rlsMergeHasUpdate, b.rlsMergeHasDelete = savedMergeUpdate, savedMergeDelete
	}()

	var combined expression
	for _, cmd := range commands {
		pred, err := b.bindRLSUsingPredicate(policyScope, roles, table.Policies, cmd, position)
		if err != nil {
			return relation{}, err
		}
		if combined.id == 0 {
			combined = pred
		} else {
			combined, err = b.combineBool(policyScope, ir.And, combined, pred)
			if err != nil {
				return relation{}, err
			}
		}
	}
	if combined.id == 0 {
		return scan, nil
	}
	effects := scan.effects.Union(combined.effects)
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{
		Rel: ir.Rel{
			Kind:            ir.Filter,
			Scope:           s.id,
			Predicate:       combined.id,
			Effects:         effects,
			SecurityBarrier: true,
		},
		Inputs:  []ir.RelID{scan.id},
		Outputs: outputSlots(scan.columns),
	})
	if err != nil {
		return relation{}, err
	}
	scan.id = id
	scan.effects = effects
	return scan, nil
}

// applyModifyRLS attaches per-branch RLS WITH CHECK predicates (and ON CONFLICT
// DO UPDATE USING predicates) directly onto an ir.Modify RelSpec.
func (b *binder) applyModifyRLS(s *scope, target mutationTarget, kind ir.ModifyKind, spec *ir.RelSpec, mergeHasInsert, mergeHasUpdate bool) error {
	d := target.executionDescriptor
	table := target.executionTable
	if d == nil {
		d = target.descriptor
		table = target.table
	}
	if d == nil || table == nil || !table.RowSecurity {
		return nil
	}
	// If modifying through a view without security_invoker, evaluate RLS under
	// the view owner's role; otherwise evaluate under the caller's role.
	effectiveRole := b.options.CurrentRole
	if target.view && target.descriptor != nil {
		if vmeta, ok := target.descriptor.View(); ok && !vmeta.SecurityInvoker {
			effectiveRole = vmeta.OwnerName
		}
	}
	savedRole := b.options.CurrentRole
	b.options.CurrentRole = effectiveRole
	defer func() { b.options.CurrentRole = savedRole }()

	roles, err := b.cachedRoles()
	if err != nil {
		return err
	}
	if !rlsApplies(roles, b.options.CurrentRole, table) {
		return nil
	}
	if b.rlsStack == nil {
		b.rlsStack = make(map[model.ObjectID]bool)
	}
	if b.rlsStack[d.ID()] {
		return bindError("42P17", `infinite recursion detected in policy for relation "`+d.Name().Name+`"`, 0)
	}
	b.rlsStack[d.ID()] = true
	defer delete(b.rlsStack, d.ID())

	needInsertCheck := kind == ir.Insert || (kind == ir.Merge && mergeHasInsert)
	needUpdateCheck := kind == ir.Update || (kind == ir.Insert && spec.Conflict == ir.ConflictUpdate) || (kind == ir.Merge && mergeHasUpdate)
	needConflictUsing := kind == ir.Insert && spec.Conflict == ir.ConflictUpdate

	if !needInsertCheck && !needUpdateCheck && !needConflictUsing {
		return nil
	}

	schemaName := ""
	if d.Name().Parent != 0 && b.catalog != nil {
		if pub, err := b.catalog.Descriptor(b.ctx, d.Name().Parent); err == nil && pub.Descriptor != nil {
			schemaName = pub.Descriptor.Name().Name
		}
	}

	if needInsertCheck || needUpdateCheck {
		columns := publicColumns(table)
		layout := model.TypeLayout{
			Type:     model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite},
			Children: make([]model.TypeLayout, len(columns)),
		}
		for i, c := range columns {
			layout.Children[i] = c.Layout
		}
		rlsRow, err := b.slot(s, layout, false, d.ID(), 0)
		if err != nil {
			return err
		}
		spec.RLSRow = rlsRow
		checkScope := b.rowImagePolicyScope(s.id, d, schemaName, columns, rlsRow, layout, false)

		if needInsertCheck {
			insertCheck, err := b.bindRLSWithCheckPredicate(checkScope, roles, table.Policies, "INSERT", 0)
			if err != nil {
				return err
			}
			spec.RLSInsertCheck = insertCheck.id
			spec.Effects = spec.Effects.Union(insertCheck.effects)
		}
		if needUpdateCheck {
			updateCheck, err := b.bindRLSWithCheckPredicate(checkScope, roles, table.Policies, "UPDATE", 0)
			if err != nil {
				return err
			}
			spec.RLSUpdateCheck = updateCheck.id
			spec.Effects = spec.Effects.Union(updateCheck.effects)
		}
	}

	if needConflictUsing && spec.OldRow != 0 {
		oldInfo, ok := b.builder.Slot(spec.OldRow)
		if !ok {
			return model.ErrInvalidDescriptor
		}
		// Build a scope reading target table columns from spec.OldRow.
		oldColumns := publicColumns(target.table)
		oldLayout := model.TypeLayout{
			Type:     model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite},
			Children: make([]model.TypeLayout, len(oldColumns)),
		}
		for i, c := range oldColumns {
			oldLayout.Children[i] = c.Layout
		}
		oldScope := b.rowImagePolicyScope(s.id, d, schemaName, oldColumns, spec.OldRow, oldLayout, oldInfo.Nullable)
		commands := []string{"UPDATE"}
		if tableHasCommandUsingPolicy(table.Policies, "SELECT") {
			commands = append(commands, "SELECT")
		}
		var conflictUsing expression
		for _, cmd := range commands {
			pred, err := b.bindRLSUsingPredicate(oldScope, roles, table.Policies, cmd, 0)
			if err != nil {
				return err
			}
			if conflictUsing.id == 0 {
				conflictUsing = pred
			} else {
				conflictUsing, err = b.combineBool(oldScope, ir.And, conflictUsing, pred)
				if err != nil {
					return err
				}
			}
		}
		if conflictUsing.id != 0 {
			if spec.ConflictPredicate != 0 {
				existingScalar, ok := b.builder.Scalar(spec.ConflictPredicate)
				if !ok {
					return model.ErrInvalidDescriptor
				}
				existingExpr := expression{
					id:       spec.ConflictPredicate,
					typ:      b.primitive(16),
					nullable: existingScalar.Nullable,
					effects:  existingScalar.Effects,
				}
				merged, err := b.combineBool(oldScope, ir.And, conflictUsing, existingExpr)
				if err != nil {
					return err
				}
				spec.ConflictPredicate = merged.id
				spec.Effects = spec.Effects.Union(merged.effects)
			} else {
				spec.ConflictPredicate = conflictUsing.id
				spec.Effects = spec.Effects.Union(conflictUsing.effects)
			}
		}
	}
	return nil
}

func (b *binder) rowImagePolicyScope(scopeID ir.ScopeID, descriptor *model.Descriptor, schemaName string, columns []model.ColumnDescriptor, rowSlot model.SlotID, layout model.TypeLayout, rowNullable bool) *scope {
	tableName := descriptor.Name().Name
	qualifier := relationQualifier{namespace: schemaName, name: tableName}
	ns := relationNamespace{
		qualifier:     qualifier,
		descriptor:    descriptor,
		systemColumns: true,
	}
	scopeCols := make([]column, 0, len(columns))
	for i, c := range columns {
		field := rowField{row: rowSlot, layout: layout, index: uint32(i), nullable: rowNullable}
		col := column{
			ResultColumn: ResultColumn{Name: c.Name, Layout: c.Layout, Nullable: c.Nullable || rowNullable},
			qualifier:    tableName,
			namespace:    schemaName,
			field:        &field,
		}
		scopeCols = append(scopeCols, col)
		ns.columns = append(ns.columns, col)
	}
	ns.row = &column{
		ResultColumn: ResultColumn{Slot: rowSlot, Layout: layout, Nullable: rowNullable},
		record:       recordDeclared,
		recordFields: ns.columns,
	}
	return &scope{
		id:         scopeID,
		columns:    scopeCols,
		namespaces: []relationNamespace{ns},
		ctes:       make(map[string]cteBinding),
	}
}

func tableHasCommandUsingPolicy(policies []model.PolicyDescriptor, cmd string) bool {
	return slices.ContainsFunc(policies, func(p model.PolicyDescriptor) bool {
		return p.Command == cmd
	})
}

func (b *binder) bindRLSUsingPredicate(s *scope, roles map[string]model.RoleMetadata, policies []model.PolicyDescriptor, cmd string, position int) (expression, error) {
	var permissive []expression
	var restrictive []expression
	for _, policy := range policies {
		if policy.Command != "ALL" && policy.Command != cmd {
			continue
		}
		if !policyMatchesRole(roles, b.options.CurrentRole, policy.Roles) {
			continue
		}
		if policy.Using == nil {
			continue
		}
		expr, err := b.bindStoredPolicyExpr(s, policy.Using, position)
		if err != nil {
			return expression{}, err
		}
		if policy.Permissive {
			permissive = append(permissive, expr)
		} else {
			restrictive = append(restrictive, expr)
		}
	}
	return b.combineRLSPredicates(s, permissive, restrictive)
}

func (b *binder) bindRLSWithCheckPredicate(s *scope, roles map[string]model.RoleMetadata, policies []model.PolicyDescriptor, cmd string, position int) (expression, error) {
	var permissive []expression
	var restrictive []expression
	for _, policy := range policies {
		if policy.Command != "ALL" && policy.Command != cmd {
			continue
		}
		if !policyMatchesRole(roles, b.options.CurrentRole, policy.Roles) {
			continue
		}
		// PostgreSQL rule: for ALL/UPDATE policies without explicit WITH CHECK,
		// USING serves as WITH CHECK. For INSERT policies, only WITH CHECK applies.
		def := policy.WithCheck
		if def == nil && (policy.Command == "ALL" || policy.Command == "UPDATE") {
			def = policy.Using
		}
		if def == nil {
			continue
		}
		expr, err := b.bindStoredPolicyExpr(s, def, position)
		if err != nil {
			return expression{}, err
		}
		if policy.Permissive {
			permissive = append(permissive, expr)
		} else {
			restrictive = append(restrictive, expr)
		}
	}
	return b.combineRLSPredicates(s, permissive, restrictive)
}

func (b *binder) bindStoredPolicyExpr(s *scope, def *model.ExpressionDefinition, position int) (expression, error) {
	if def == nil {
		return b.addScalar(s, expression{typ: b.primitive(16), value: model.BoolVal(true)}, ir.Scalar{Kind: ir.Const}, nil)
	}
	var node *zpgparse.Node
	if len(def.Encoding) > 0 {
		decoded := &zpgparse.Node{}
		if err := proto.Unmarshal(def.Encoding, decoded); err == nil && decoded.Node != nil {
			node = decoded
		}
	}
	if node == nil {
		if def.SQL == "" {
			return expression{}, model.ErrInvalidDescriptor
		}
		parsed, err := zpgparse.Parse("SELECT (" + def.SQL + ")")
		if err != nil || parsed == nil || len(parsed.Stmts) != 1 || parsed.Stmts[0].Stmt == nil {
			return expression{}, model.ErrInvalidDescriptor
		}
		sel := parsed.Stmts[0].Stmt.GetSelectStmt()
		if sel == nil || len(sel.TargetList) != 1 || sel.TargetList[0].GetResTarget() == nil {
			return expression{}, model.ErrInvalidDescriptor
		}
		node = sel.TargetList[0].GetResTarget().Val
	}
	if err := ValidateStatementFeatures(b.ctx, node); err != nil {
		return expression{}, err
	}
	bound, err := b.scalar(node, s)
	if err != nil {
		return expression{}, err
	}
	return b.coerce(s, bound, b.primitive(16), ir.ImplicitCast)
}

func (b *binder) combineRLSPredicates(s *scope, permissive, restrictive []expression) (expression, error) {
	if len(permissive) == 0 {
		return b.addScalar(s, expression{typ: b.primitive(16), value: model.BoolVal(false)}, ir.Scalar{Kind: ir.Const}, nil)
	}
	combined := permissive[0]
	for _, next := range permissive[1:] {
		var err error
		combined, err = b.combineBool(s, ir.Or, combined, next)
		if err != nil {
			return expression{}, err
		}
	}
	for _, next := range restrictive {
		var err error
		combined, err = b.combineBool(s, ir.And, combined, next)
		if err != nil {
			return expression{}, err
		}
	}
	return combined, nil
}

func (b *binder) combineBool(s *scope, op ir.BoolOp, left, right expression) (expression, error) {
	return b.addScalar(
		s,
		expression{
			typ:      b.primitive(16),
			nullable: left.nullable || right.nullable,
			effects:  left.effects.Union(right.effects),
		},
		ir.Scalar{Kind: ir.Bool, BoolOp: op},
		[]expression{left, right},
	)
}

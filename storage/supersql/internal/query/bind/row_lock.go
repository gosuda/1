package bind

import (
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type selectedRowLock struct {
	namespace relationNamespace
	strength  ir.LockStrength
	wait      ir.LockWait
}

type rowLockPolicy struct {
	strength ir.LockStrength
	wait     ir.LockWait
}

type namedRowLockPolicy struct {
	qualifier relationQualifier
	policy    rowLockPolicy
	position  int
}

func strongerLockWait(left, right ir.LockWait) ir.LockWait {
	rank := func(wait ir.LockWait) uint8 {
		switch wait {
		case ir.Wait:
			return 0
		case ir.SkipLocked:
			return 1
		case ir.NoWait:
			return 2
		default:
			return 0
		}
	}
	if rank(right) > rank(left) {
		return right
	}
	return left
}

func mergeRowLockPolicy(left, right rowLockPolicy) rowLockPolicy {
	if right.strength > left.strength {
		left.strength = right.strength
	}
	left.wait = strongerLockWait(left.wait, right.wait)
	return left
}

func lockingStrength(value zpgparse.LockClauseStrength) (ir.LockStrength, error) {
	switch value {
	case zpgparse.LockClauseStrength_LCS_FORKEYSHARE:
		return ir.KeyShare, nil
	case zpgparse.LockClauseStrength_LCS_FORSHARE:
		return ir.Share, nil
	case zpgparse.LockClauseStrength_LCS_FORNOKEYUPDATE:
		return ir.NoKeyUpdate, nil
	case zpgparse.LockClauseStrength_LCS_FORUPDATE:
		return ir.UpdateLock, nil
	default:
		return 0, bindError("42601", "invalid row locking strength", 0)
	}
}

func lockingWait(value zpgparse.LockWaitPolicy) (ir.LockWait, error) {
	switch value {
	case zpgparse.LockWaitPolicy_LOCK_WAIT_POLICY_UNDEFINED, zpgparse.LockWaitPolicy_LockWaitBlock:
		return ir.Wait, nil
	case zpgparse.LockWaitPolicy_LockWaitError:
		return ir.NoWait, nil
	case zpgparse.LockWaitPolicy_LockWaitSkip:
		return ir.SkipLocked, nil
	default:
		return 0, bindError("42601", "invalid row locking wait policy", 0)
	}
}

func rowLockClausePolicies(clauses []*zpgparse.Node, pushed []rowLockPolicy) ([]rowLockPolicy, []namedRowLockPolicy, error) {
	all := slices.Clone(pushed)
	var named []namedRowLockPolicy
	for _, node := range clauses {
		clause := node.GetLockingClause()
		if clause == nil {
			return nil, nil, bindError("42601", "invalid row locking clause", 0)
		}
		strength, err := lockingStrength(clause.Strength)
		if err != nil {
			return nil, nil, err
		}
		wait, err := lockingWait(clause.WaitPolicy)
		if err != nil {
			return nil, nil, err
		}
		policy := rowLockPolicy{strength: strength, wait: wait}
		if len(clause.LockedRels) == 0 {
			all = append(all, policy)
			continue
		}
		for _, locked := range clause.LockedRels {
			rangeVar := locked.GetRangeVar()
			if rangeVar == nil || rangeVar.Relname == "" || rangeVar.Catalogname != "" {
				return nil, nil, bindError("42601", "invalid row locking relation", 0)
			}
			named = append(named, namedRowLockPolicy{
				qualifier: relationQualifier{namespace: rangeVar.Schemaname, name: rangeVar.Relname},
				policy:    policy,
				position:  int(rangeVar.Location) + 1,
			})
		}
	}
	return all, named, nil
}

func (b *binder) prepareRowLockPropagation(from []*zpgparse.Node, clauses []*zpgparse.Node, pushed []rowLockPolicy) error {
	all, named, err := rowLockClausePolicies(clauses, pushed)
	if err != nil {
		return err
	}
	for _, node := range from {
		b.propagateRowLocks(node, all, named)
	}
	return nil
}

func (b *binder) propagateRowLocks(node *zpgparse.Node, all []rowLockPolicy, named []namedRowLockPolicy) {
	if node == nil {
		return
	}
	if rangeVar := node.GetRangeVar(); rangeVar != nil {
		qualifier := relationQualifier{namespace: rangeVar.Schemaname, name: rangeVar.Relname}
		if rangeVar.Alias != nil {
			qualifier = relationQualifier{name: rangeVar.Alias.Aliasname}
		}
		policies := slices.Clone(all)
		for _, candidate := range named {
			namespaceMatches := candidate.qualifier.namespace == "" || candidate.qualifier.namespace == qualifier.namespace
			if rangeVar.Alias == nil && qualifier.namespace == "" {
				namespaceMatches = true
			}
			if candidate.qualifier.name == qualifier.name && namespaceMatches {
				policies = append(policies, candidate.policy)
			}
		}
		if len(policies) != 0 {
			if b.rangeRowLocks == nil {
				b.rangeRowLocks = make(map[*zpgparse.RangeVar][]rowLockPolicy)
			}
			b.rangeRowLocks[rangeVar] = append(b.rangeRowLocks[rangeVar], policies...)
		}
		return
	}
	if subquery := node.GetRangeSubselect(); subquery != nil {
		selectStatement := subquery.Subquery.GetSelectStmt()
		if selectStatement == nil {
			return
		}
		policies := slices.Clone(all)
		if subquery.Alias != nil {
			qualifier := relationQualifier{name: subquery.Alias.Aliasname}
			for _, candidate := range named {
				if candidate.qualifier.name == qualifier.name && (candidate.qualifier.namespace == "" || candidate.qualifier.namespace == qualifier.namespace) {
					policies = append(policies, candidate.policy)
				}
			}
		}
		if len(policies) != 0 {
			if b.pushedRowLocks == nil {
				b.pushedRowLocks = make(map[*zpgparse.SelectStmt][]rowLockPolicy)
			}
			b.pushedRowLocks[selectStatement] = append(b.pushedRowLocks[selectStatement], policies...)
		}
		return
	}
	if join := node.GetJoinExpr(); join != nil {
		childNamed := named
		if join.Alias != nil {
			childNamed = nil
		}
		b.propagateRowLocks(join.Larg, all, childNamed)
		b.propagateRowLocks(join.Rarg, all, childNamed)
	}
}

func selectLockNamespace(namespaces []relationNamespace, qualifier relationQualifier, position int) (relationNamespace, error) {
	var result relationNamespace
	found := false
	for _, namespace := range namespaces {
		if namespace.qualifier.name != qualifier.name || qualifier.namespace != "" && namespace.qualifier.namespace != qualifier.namespace {
			continue
		}
		if found {
			return relationNamespace{}, bindError("42P09", "row locking relation is ambiguous", position)
		}
		result, found = namespace, true
	}
	if !found {
		return relationNamespace{}, bindError("42P01", "row locking relation does not exist", position)
	}
	return result, nil
}

func appendSelectedRowLock(result []selectedRowLock, namespace relationNamespace, policy rowLockPolicy) ([]selectedRowLock, error) {
	if namespace.occurrence == 0 || namespace.descriptor == nil || namespace.descriptor.Kind() != model.ObjectTable {
		return nil, bindError("42809", "row locking requires a table", 0)
	}
	if namespace.identity[0].Slot == 0 {
		return nil, bindError("XX000", "row identity metadata is unavailable", 0)
	}
	if slices.ContainsFunc(namespace.identity[:], func(column column) bool { return column.Nullable }) {
		return nil, bindError("0A000", "row locking cannot be applied to the nullable side of an outer join", 0)
	}
	for i := range result {
		if result[i].namespace.occurrence != namespace.occurrence {
			continue
		}
		merged := mergeRowLockPolicy(
			rowLockPolicy{strength: result[i].strength, wait: result[i].wait},
			policy,
		)
		result[i].strength, result[i].wait = merged.strength, merged.wait
		return result, nil
	}
	return append(result, selectedRowLock{namespace: namespace, strength: policy.strength, wait: policy.wait}), nil
}

func (b *binder) selectedRowLocks(clauses []*zpgparse.Node, namespaces, targets []relationNamespace, pushed []rowLockPolicy) ([]selectedRowLock, error) {
	all, named, err := rowLockClausePolicies(clauses, pushed)
	if err != nil {
		return nil, err
	}
	var result []selectedRowLock
	for _, policy := range all {
		for _, namespace := range targets {
			if namespace.subquery || namespace.descriptor == nil || namespace.descriptor.Kind() != model.ObjectTable {
				continue
			}
			result, err = appendSelectedRowLock(result, namespace, policy)
			if err != nil {
				return nil, err
			}
		}
	}
	for _, candidate := range named {
		namespace, err := selectLockNamespace(namespaces, candidate.qualifier, candidate.position)
		if err != nil {
			return nil, err
		}
		if namespace.subquery {
			continue
		}
		result, err = appendSelectedRowLock(result, namespace, candidate.policy)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (b *binder) lockRows(input relation, s *scope, clauses []*zpgparse.Node, pushed []rowLockPolicy) (relation, error) {
	selected, err := b.selectedRowLocks(clauses, input.namespaces, s.rowLockTargets, pushed)
	if err != nil {
		return input, err
	}
	if len(selected) == 0 {
		return input, nil
	}
	columns := slices.Clone(input.columns)
	bindings := make([]ir.Binding, 0, len(columns)+len(selected))
	for _, source := range columns {
		reference, err := b.reference(s, source, 0)
		if err != nil {
			return input, err
		}
		bindings = append(bindings, ir.Binding{Slot: source.Slot, Expr: reference.id})
	}
	locks := make([]ir.RowLockSpec, 0, len(selected))
	effects := input.effects
	for _, selected := range selected {
		table, ok := selected.namespace.descriptor.Table()
		if !ok || len(table.PrimaryKey) == 0 {
			return input, bindError("XX000", "row locking table has no physical primary key", 0)
		}
		arguments := make([]expression, len(table.PrimaryKey))
		layout := model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: -1}, Children: make([]model.TypeLayout, len(table.PrimaryKey)), FieldNames: make([]string, len(table.PrimaryKey))}
		keys := make([]ir.RowLockKey, len(table.PrimaryKey))
		for i, key := range table.PrimaryKey {
			if key.Column == 0 || key.Expression != nil {
				return input, bindError("XX000", "row locking primary key metadata is invalid", 0)
			}
			descriptorIndex := slices.IndexFunc(table.Columns, func(candidate model.ColumnDescriptor) bool { return candidate.ID == key.Column })
			if descriptorIndex < 0 {
				return input, bindError("XX000", "row locking primary key column is missing", 0)
			}
			descriptorColumn := table.Columns[descriptorIndex]
			physicalIndex := slices.IndexFunc(selected.namespace.physical, func(candidate column) bool {
				return candidate.Name == descriptorColumn.Name && sameType(candidate.Layout, descriptorColumn.Layout)
			})
			if physicalIndex < 0 {
				return input, bindError("XX000", "row locking primary key value is unavailable", 0)
			}
			arguments[i], err = b.reference(s, selected.namespace.physical[physicalIndex], 0)
			if err != nil {
				return input, err
			}
			layout.Children[i], layout.FieldNames[i] = descriptorColumn.Layout, descriptorColumn.Name
			keys[i] = ir.RowLockKey{Descending: key.Descending, NullsFirst: key.NullsFirst}
		}
		row, err := b.addScalar(s, expression{typ: layout, position: 0}, ir.Scalar{Kind: ir.Row}, arguments)
		if err != nil {
			return input, err
		}
		slot, err := b.slot(s, layout, false, 0, 0)
		if err != nil {
			return input, err
		}
		columns = append(columns, column{ResultColumn: ResultColumn{Layout: layout, Slot: slot}, hidden: true, internal: true})
		bindings = append(bindings, ir.Binding{Slot: slot, Expr: row.id})
		identity := selected.namespace.identity
		locks = append(locks, ir.RowLockSpec{RowLock: ir.RowLock{
			Object: selected.namespace.descriptor.ID(), Occurrence: selected.namespace.occurrence, ObjectSlot: identity[0].Slot,
			DescriptorVersionSlot: identity[1].Slot, GenerationSlot: identity[2].Slot,
			EncodingVersionSlot: identity[3].Slot, RowSlot: slot, Strength: selected.strength, Wait: selected.wait,
		}, Keys: keys})
	}
	project, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: s.id, Effects: effects}, Inputs: []ir.RelID{input.id}, Outputs: outputSlots(columns), Bindings: bindings})
	if err != nil {
		return input, err
	}
	effects.Volatility, effects.MayError = ir.Volatile, true
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.LockRows, Scope: s.id, Effects: effects}, Inputs: []ir.RelID{project}, Outputs: outputSlots(columns), Locks: locks})
	return relation{id: id, columns: columns, effects: effects, namespaces: input.namespaces}, err
}

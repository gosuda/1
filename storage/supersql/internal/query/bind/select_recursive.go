package bind

import (
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type recursiveClauses struct {
	columns   []column
	recursion ir.RecursionSpec
}


func (b *binder) recursiveCTE(c *zpgparse.CommonTableExpr, s *scope) error {
	pos := int(c.Location) + 1
	query := c.Ctequery.GetSelectStmt()
	if query == nil || query.Op != zpgparse.SetOperation_SETOP_UNION {
		return bindError("42P19", "recursive query must have non-recursive term UNION recursive term", pos)
	}
	if query.Larg == nil || query.Rarg == nil {
		return bindError("42601", "recursive UNION needs two inputs", pos)
	}
	if referencesCTE(&zpgparse.Node{Node: &zpgparse.Node_SelectStmt{SelectStmt: query.Larg}}, c.Ctename) {
		return bindError("42P19", "recursive reference must not appear within the non-recursive term", pos)
	}
	if c.SearchClause != nil || c.CycleClause != nil {
		leftSet := query.Larg.Op != zpgparse.SetOperation_SETOP_NONE && query.Larg.Op != zpgparse.SetOperation_SET_OPERATION_UNDEFINED
		if leftSet || len(query.Larg.ValuesLists) != 0 {
			return bindError("0A000", "with a SEARCH or CYCLE clause, the left side of the UNION must be a SELECT", pos)
		}
		rightSet := query.Rarg.Op != zpgparse.SetOperation_SETOP_NONE && query.Rarg.Op != zpgparse.SetOperation_SET_OPERATION_UNDEFINED
		if rightSet || len(query.Rarg.ValuesLists) != 0 {
			return bindError("42601", "with a SEARCH or CYCLE clause, the right side of the UNION must be a SELECT", pos)
		}
	}
	isolated := &scope{id: s.id, parent: s.parent, ctes: s.ctes}
	anchor, err := b.selectStatement(query.Larg, isolated, nil)
	if err != nil {
		return err
	}
	if len(c.Aliascolnames) > len(anchor.columns) {
		return bindError("42P10", "too many CTE column aliases", pos)
	}
	baseColumns := slices.Clone(anchor.columns)
	types := make([]model.TypeLayout, len(baseColumns))
	for i := range baseColumns {
		if i < len(c.Aliascolnames) {
			baseColumns[i].Name = c.Aliascolnames[i].GetString_().GetSval()
		}
		baseColumns[i].Nullable = true
		types[i] = baseColumns[i].Layout
		slot, err := b.slot(s, baseColumns[i].Layout, true, 0, 0)
		if err != nil {
			return err
		}
		baseColumns[i].Slot = slot
	}
	generated, err := b.bindRecursiveClauses(c, s, baseColumns)
	if err != nil {
		return err
	}
	columns := append(slices.Clone(baseColumns), generated.columns...)
	// Recursive calls may introduce any effect; no optimizer may move this CTE
	// using only the anchor's effect summary.
	effects := ir.Effects{Volatility: ir.Volatile, MayError: true}
	id, err := b.builder.DeclareCTE(b.ctx, ir.CTESpec{Scope: s.id, Outputs: outputSlots(columns), Materialization: ir.MaterializeAlways, Recursive: true, Effects: effects})
	if err != nil {
		return err
	}
	termColumns := slices.Clone(columns)
	for i := len(baseColumns); i < len(termColumns); i++ {
		// SEARCH/CYCLE working columns exist in the feedback row but are not in
		// the source query's namespace; PostgreSQL's rewrite consumes them.
		termColumns[i].internal = true
	}
	s.ctes[c.Ctename] = cteBinding{id: id, columns: termColumns, effects: effects, recursive: true}
	termScope := &scope{id: s.id, parent: s.parent, ctes: s.ctes}
	term, err := b.selectStatement(query.Rarg, termScope, nil)
	if err != nil {
		return err
	}
	if len(term.columns) != len(baseColumns) {
		return bindError("42601", "recursive UNION column count mismatch", pos)
	}
	for i, column := range term.columns {
		if !sameType(column.Layout, types[i]) {
			return bindError("42804", "recursive term type differs from non-recursive term", pos)
		}
	}
	relID, err := b.builder.AddRel(b.ctx, ir.RelSpec{
		Rel:           ir.Rel{Kind: ir.RecursiveUnion, Scope: s.id, CTE: id, All: query.All, Effects: effects},
		Inputs:        []ir.RelID{anchor.id, term.id},
		Outputs:       outputSlots(columns),
		RecursionSpec: generated.recursion,
	})
	if err != nil {
		return err
	}
	body, err := b.selectTail(query, s, relation{id: relID, columns: columns, effects: effects})
	if err != nil {
		return err
	}
	if err := b.builder.BindCTE(b.ctx, id, body.id); err != nil {
		return err
	}
	s.ctes[c.Ctename] = cteBinding{id: id, columns: body.columns, effects: body.effects}
	return nil
}


func (b *binder) bindRecursiveClauses(c *zpgparse.CommonTableExpr, s *scope, base []column) (recursiveClauses, error) {
	var result recursiveClauses
	search, cycle := c.SearchClause, c.CycleClause
	if search == nil && cycle == nil {
		return result, nil
	}
	if search != nil {
		position := int(search.Location) + 1
		keys, err := b.recursiveClauseKeys(search.SearchColList, base, "search", position)
		if err != nil {
			return result, err
		}
		if recursiveColumnNamed(base, search.SearchSeqColumn) {
			return result, bindError("42601", "search sequence column name already used in WITH query column list", position)
		}
		layout := recursivePathLayout(keys)
		mode := ir.DepthFirst
		if search.SearchBreadthFirst {
			mode = ir.BreadthFirst
			layout = recursiveBreadthLayout(b.primitive(20), keys)
		}
		slot, err := b.slot(s, layout, false, 0, 0)
		if err != nil {
			return result, err
		}
		result.columns = append(result.columns, column{ResultColumn: ResultColumn{Name: search.SearchSeqColumn, Layout: layout, Slot: slot}})
		result.recursion.Search = mode
		result.recursion.SearchSlot = slot
		result.recursion.SearchKeys = outputSlots(keys)
	}
	if cycle == nil {
		return result, nil
	}
	position := int(cycle.Location) + 1
	keys, err := b.recursiveClauseKeys(cycle.CycleColList, base, "cycle", position)
	if err != nil {
		return result, err
	}
	if recursiveColumnNamed(base, cycle.CycleMarkColumn) {
		return result, bindError("42601", "cycle mark column name already used in WITH query column list", position)
	}
	if recursiveColumnNamed(base, cycle.CyclePathColumn) {
		return result, bindError("42601", "cycle path column name already used in WITH query column list", position)
	}
	if cycle.CycleMarkColumn == cycle.CyclePathColumn {
		return result, bindError("42601", "cycle mark column name and cycle path column name are the same", position)
	}
	if search != nil && search.SearchSeqColumn == cycle.CycleMarkColumn {
		return result, bindError("42601", "search sequence column name and cycle mark column name are the same", int(search.Location)+1)
	}
	if search != nil && search.SearchSeqColumn == cycle.CyclePathColumn {
		return result, bindError("42601", "search sequence column name and cycle path column name are the same", int(search.Location)+1)
	}
	constantScope := &scope{id: s.id, ctes: s.ctes}
	value, err := b.scalar(cycle.CycleMarkValue, constantScope)
	if err != nil {
		return result, err
	}
	defaultValue, err := b.scalar(cycle.CycleMarkDefault, constantScope)
	if err != nil {
		return result, err
	}
	values := []expression{value, defaultValue}
	markLayout, err := b.commonType(values)
	if err != nil {
		return result, err
	}
	markCollation, err := b.commonCollation(values)
	if err != nil {
		return result, err
	}
	// PostgreSQL's transformCycleClause calls select_common_collation with
	// nones=true, so an indeterminate pair (both marks are plain string
	// literals) falls back to the database default rather than an unresolved
	// collation, which a recursive key cannot carry. That fallback applies only
	// where a collation is representable: select_common_collation yields
	// InvalidOid for non-collatable inputs, and a boolean or integer mark must
	// keep collation 0 or the relabel check rejects the operand metadata.
	if markCollation.id == 0 && b.collatable(markLayout.Type) {
		markCollation = collationState{strength: collationImplicit, id: b.options.DefaultCollation}
	}
	markLayout.Type.Collation = markCollation.id
	value, err = b.coerceCommon(constantScope, value, markLayout)
	if err != nil {
		return result, err
	}
	defaultValue, err = b.coerceCommon(constantScope, defaultValue, markLayout)
	if err != nil {
		return result, err
	}
	continued, err := b.operator(constantScope, "<>", []expression{defaultValue, value}, position)
	if err != nil {
		return result, err
	}
	markSlot, err := b.slot(s, markLayout, value.nullable || defaultValue.nullable, 0, 0)
	if err != nil {
		return result, err
	}
	pathLayout := recursivePathLayout(keys)
	pathSlot, err := b.slot(s, pathLayout, false, 0, 0)
	if err != nil {
		return result, err
	}
	result.columns = append(result.columns,
		column{ResultColumn: ResultColumn{Name: cycle.CycleMarkColumn, Layout: markLayout, Nullable: value.nullable || defaultValue.nullable, Slot: markSlot}, collation: markCollation},
		column{ResultColumn: ResultColumn{Name: cycle.CyclePathColumn, Layout: pathLayout, Slot: pathSlot}},
	)
	result.recursion.CycleMark = markSlot
	result.recursion.CyclePath = pathSlot
	result.recursion.CycleValue = value.id
	result.recursion.CycleDefault = defaultValue.id
	result.recursion.CycleContinue = continued.id
	result.recursion.CycleKeys = outputSlots(keys)
	return result, nil
}

func (b *binder) recursiveClauseKeys(nodes []*zpgparse.Node, columns []column, kind string, position int) ([]column, error) {
	names, err := names(nodes)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, bindError("42601", kind+" column list must not be empty", position)
	}
	keys := make([]column, len(names))
	for i, name := range names {
		if slices.Contains(names[:i], name) {
			return nil, bindError("42701", kind+" column specified more than once", position)
		}
		found := false
		for _, candidate := range columns {
			if candidate.Name != name {
				continue
			}
			keys[i], found = candidate, true
			break
		}
		if !found {
			return nil, bindError("42601", kind+" column not in WITH query column list", position)
		}
		if err := b.requireKeyCollation(expression{typ: keys[i].Layout, collation: keys[i].collation, position: position}); err != nil {
			return nil, err
		}
		if kind == "cycle" {
			operand := expression{typ: keys[i].Layout, nullable: keys[i].Nullable, collation: keys[i].collation, position: position}
			comparison, err := b.resolveOperator("=", []expression{operand, operand}, position)
			if err != nil {
				return nil, err
			}
			signature, err := b.specialize(comparison.signature, []expression{operand, operand}, position)
			if err != nil {
				return nil, err
			}
			if signature.Result != 16 || signature.ReturnsSet {
				return nil, bindError("42804", "cycle column equality operator must return boolean", position)
			}
		}
	}
	return keys, nil
}

func recursiveColumnNamed(columns []column, name string) bool {
	for _, candidate := range columns {
		if candidate.Name == name {
			return true
		}
	}
	return false
}

func recursiveRecordLayout(keys []column) model.TypeLayout {
	children := make([]model.TypeLayout, len(keys))
	for i := range keys {
		children[i] = keys[i].Layout
	}
	return model.TypeLayout{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, Children: children}
}

func recursivePathLayout(keys []column) model.TypeLayout {
	return model.TypeLayout{
		Type:     model.Type{ID: 2287, Modifier: -1, Storage: model.TypeArray},
		Children: []model.TypeLayout{recursiveRecordLayout(keys)},
	}
}

func recursiveBreadthLayout(depth model.TypeLayout, keys []column) model.TypeLayout {
	children := make([]model.TypeLayout, len(keys)+1)
	children[0] = depth
	for i := range keys {
		children[i+1] = keys[i].Layout
	}
	return model.TypeLayout{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, Children: children}
}

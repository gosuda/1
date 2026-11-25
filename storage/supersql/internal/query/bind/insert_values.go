package bind

import (
	"fmt"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type insertTargetContext struct {
	columns []model.ColumnDescriptor
	exact   bool
}

func (target *insertTargetContext) checkWidth(width int) error {
	if width > len(target.columns) || target.exact && width != len(target.columns) {
		return bindError("42601", "INSERT column count does not match source", 0)
	}
	return nil
}

func (target *insertTargetContext) inferOutputs(statement *zpgparse.SelectStmt, values []expression, visible int, keys []tailTarget) ([]bool, error) {
	if target == nil {
		return nil, nil
	}
	if err := target.checkWidth(visible); err != nil {
		return nil, err
	}
	if len(statement.DistinctClause) != 0 && (statement.DistinctClause[0] == nil || statement.DistinctClause[0].Node == nil) {
		return nil, nil
	}
	var allowed []bool
	for i := range visible {
		if values[i].typ.Type.ID != 0 || values[i].inferred != 0 {
			continue
		}
		if allowed == nil {
			allowed = make([]bool, len(values))
		}
		allowed[i] = true
	}
	if allowed == nil {
		return nil, nil
	}
	// ORDER BY and DISTINCT ON need the SELECT's own comparison type before
	// assignment. They cannot inherit a destination type from their consumer.
	for _, key := range keys {
		allowed[key.index] = false
	}
	return allowed, nil
}

func simpleInsertValues(statement *zpgparse.SelectStmt) bool {
	if statement == nil || len(statement.ValuesLists) == 0 || statement.IntoClause != nil {
		return false
	}
	ordered := len(statement.SortClause) != 0 || statement.LimitOffset != nil || statement.LimitCount != nil
	decorated := len(statement.LockingClause) != 0 || statement.WithClause != nil
	setOperation := statement.Op != zpgparse.SetOperation_SETOP_NONE && statement.Op != zpgparse.SetOperation_SET_OPERATION_UNDEFINED
	return !ordered && !decorated && !setOperation
}

func (b *binder) insertSource(statement *zpgparse.SelectStmt, s *scope, targets []model.ColumnDescriptor, explicit bool, override zpgparse.OverridingKind) (relation, error) {
	if simpleInsertValues(statement) {
		return b.insertValues(statement.ValuesLists, selectScope(s), targets, explicit, override)
	}
	destination := &insertTargetContext{columns: targets, exact: explicit}
	input, err := b.selectStatement(statement, s, destination)
	if err != nil {
		return input, err
	}
	if err := destination.checkWidth(len(input.columns)); err != nil {
		return input, err
	}
	return input, nil
}

// INSERT VALUES coerces each cell before constructing a relation. It does not
// choose a common source type or collation across different input rows.
func (b *binder) insertValues(rows []*zpgparse.Node, s *scope, targets []model.ColumnDescriptor, explicit bool, override zpgparse.OverridingKind) (relation, error) {
	if err := b.enter(); err != nil {
		return relation{}, err
	}
	defer func() { b.depth-- }()
	first := rows[0].GetList()
	if first == nil || len(first.Items) == 0 {
		return relation{}, bindError("42601", "INSERT VALUES requires a nonempty row", 0)
	}
	width := len(first.Items)
	if width > len(targets) || explicit && width != len(targets) {
		return relation{}, bindError("42601", "INSERT column count does not match source", 0)
	}
	if uint64(width) > uint64(b.options.Limits.MaxOperands)/uint64(len(rows)) {
		return relation{}, bindError("54000", "VALUES operand limit exceeded", 0)
	}
	ids := make([]ir.ScalarID, 0, len(rows)*width)
	nullable := make([]bool, width)
	bound := make([]expression, width)
	effects := ir.Effects{}
	for _, row := range rows {
		list := row.GetList()
		if list == nil || len(list.Items) != width {
			return relation{}, bindError("42601", "VALUES lists must all be the same length", 0)
		}
		for i, node := range list.Items {
			if targets[i].Generated != nil && node.GetSetToDefault() == nil {
				b.deferWriteError(bindError("428C9", "cannot insert a non-DEFAULT value into generated column", 0))
			}
			var value expression
			var err error
			if targets[i].IdentitySequence != 0 && override == zpgparse.OverridingKind_OVERRIDING_USER_VALUE && node.GetSetToDefault() != nil {
				// The assignment generates this value; the VALUES cell is unused.
				value, err = b.addScalar(s, expression{typ: targets[i].Layout, nullable: true, value: model.NullVal(targets[i].Layout.Type.Storage)}, ir.Scalar{Kind: ir.Const}, nil)
			} else {
				value, err = b.assignmentInput(node, s, targets[i])
			}
			if err != nil {
				return relation{}, err
			}
			bound[i] = value
		}
		for i, input := range bound {
			value, err := b.coerceColumnAssignment(input, s, targets[i])
			if err != nil {
				return relation{}, err
			}
			if len(rows) == 1 && targets[i].IdentitySequence != 0 && override == zpgparse.OverridingKind_OVERRIDING_USER_VALUE {
				// PostgreSQL removes a single-row target expression, but retains
				// expressions in a multi-row VALUES source, including volatile ones.
				value, err = b.addScalar(s, expression{typ: targets[i].Layout, nullable: true, value: model.NullVal(targets[i].Layout.Type.Storage)}, ir.Scalar{Kind: ir.Const}, nil)
				if err != nil {
					return relation{}, err
				}
			}
			bound[i] = expression{}
			ids = append(ids, value.id)
			nullable[i] = nullable[i] || value.nullable
			effects = effects.Union(value.effects)
		}
	}
	if override != zpgparse.OverridingKind_OVERRIDING_SYSTEM_VALUE && override != zpgparse.OverridingKind_OVERRIDING_USER_VALUE {
		for _, row := range rows {
			for i, node := range row.GetList().Items {
				if targets[i].IdentityAlways && node.GetSetToDefault() == nil {
					b.deferWriteError(bindError("428C9", "cannot insert a non-DEFAULT value into identity column", 0))
				}
			}
		}
	}
	columns := make([]column, width)
	for i := range columns {
		slot, err := b.slot(s, targets[i].Layout, nullable[i], 0, 0)
		if err != nil {
			return relation{}, err
		}
		columns[i] = column{ResultColumn: ResultColumn{Name: fmt.Sprintf("column%d", i+1), Layout: targets[i].Layout, Nullable: nullable[i], Slot: slot}}
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: s.id, Rows: uint64(len(rows)), Effects: effects}, Outputs: outputSlots(columns), Expressions: ids})
	return relation{id: id, columns: columns, effects: effects}, err
}

func (b *binder) columnAssignment(node *zpgparse.Node, s *scope, column model.ColumnDescriptor) (expression, error) {
	value, err := b.assignmentInput(node, s, column)
	if err != nil {
		return value, err
	}
	return b.coerceColumnAssignment(value, s, column)
}

func (b *binder) assignmentInput(node *zpgparse.Node, s *scope, column model.ColumnDescriptor) (expression, error) {
	var value expression
	var err error
	if requested := node.GetSetToDefault(); requested != nil {
		if column.Generated != nil {
			return b.addScalar(s, expression{typ: column.Layout, nullable: true, value: model.NullVal(column.Layout.Type.Storage)}, ir.Scalar{Kind: ir.Const}, nil)
		}
		value, err = b.columnDefault(column, s)
		value.position = int(requested.Location) + 1
	} else {
		value, err = b.scalar(node, s)
	}
	return value, err
}

func (b *binder) coerceColumnAssignment(value expression, s *scope, column model.ColumnDescriptor) (expression, error) {
	value, err := b.coerce(s, value, column.Layout, ir.AssignmentCast)
	if err != nil {
		return value, err
	}
	return b.relabelCollation(s, value, column.Layout.Type.Collation)
}

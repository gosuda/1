package bind

import (
	"math"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type boundJSONTableColumn struct {
	spec     ir.JSONTableColumn
	name     string
	internal column
	target   model.TypeLayout
	onEmpty *zpgparse.JsonBehavior
	onError *zpgparse.JsonBehavior
}

type jsonTableBinding struct {
	binder      *binder
	scope       *scope
	position    int
	expressions []expression
	columns     []boundJSONTableColumn
	specs       []ir.JSONTableColumn
	names       map[string]struct{}
	pathNameID  uint32
}

func (b *binder) jsonTable(n *zpgparse.JsonTable, s *scope) (relation, error) {
	position := int(n.Location) + 1
	if n.Pathspec == nil || n.Pathspec.String_ == nil {
		return relation{}, bindError("42601", "JSON_TABLE requires a root path", position)
	}
	arguments, format, encoding, err := b.jsonPathArguments(n.ContextItem, n.Pathspec.String_, n.Passing, s, position)
	if err != nil {
		return relation{}, err
	}
	runtimeArguments := make([]expression, 0, 1+4*len(n.Passing))
	runtimeArguments = append(runtimeArguments, arguments[0])
	runtimeArguments = append(runtimeArguments, arguments[2:]...)
	rootPathName := n.Pathspec.Name
	if rootPathName == "" {
		rootPathName = "json_table_path_0"
	}
	binding := jsonTableBinding{
		binder: b, scope: s, position: position,
		expressions: append(runtimeArguments, arguments[1]),
		names: map[string]struct{}{rootPathName: {}}, pathNameID: 1,
	}
	if err := binding.bindColumns(n.Columns, 0); err != nil {
		return relation{}, err
	}
	if len(binding.columns) == 0 || len(binding.columns) > math.MaxUint16 || len(runtimeArguments) > math.MaxUint16 {
		return relation{}, bindError("54000", "JSON_TABLE column or argument limit exceeded", position)
	}
	tableFlags := ir.SQLJSONFlags(0)
	if n.OnError == nil || n.OnError.Btype == zpgparse.JsonBehaviorType_JSON_BEHAVIOR_EMPTY ||
		n.OnError.Btype == zpgparse.JsonBehaviorType_JSON_BEHAVIOR_EMPTY_ARRAY {
		tableFlags |= ir.SQLJSONCaptureError
	} else if n.OnError.Btype != zpgparse.JsonBehaviorType_JSON_BEHAVIOR_ERROR {
		return relation{}, bindError("42601", "JSON_TABLE supports only EMPTY or ERROR ON ERROR", position)
	}
	tableFlags |= ir.SQLJSONUseTZ
	rowLayout := b.primitive(2249)
	rowLayout.Children = make([]model.TypeLayout, len(binding.columns))
	internalColumns := make([]column, len(binding.columns))
	for i := range binding.columns {
		internalColumns[i] = binding.columns[i].internal
		rowLayout.Children[i] = binding.columns[i].internal.Layout
	}
	rowResult, err := b.layout(rowLayout)
	if err != nil {
		return relation{}, err
	}
	volatility := ir.Immutable
	for index := len(runtimeArguments); index < len(binding.expressions); index++ {
		pathArguments := make([]expression, 0, len(arguments))
		pathArguments = append(pathArguments, arguments[0], binding.expressions[index])
		pathArguments = append(pathArguments, arguments[2:]...)
		pathVolatility, err := b.sqlJSONPathVolatility(pathArguments)
		if err != nil {
			return relation{}, err
		}
		if pathVolatility > volatility {
			volatility = pathVolatility
		}
	}
	expressionIDs := make([]ir.ScalarID, len(binding.expressions))
	effects := ir.Effects{MayError: true, Volatility: volatility}
	for i, expression := range binding.expressions {
		expressionIDs[i] = expression.id
		effects = effects.Union(expression.effects)
	}
	rootOrdinal := uint32(len(runtimeArguments) + 1)
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{
		Rel: ir.Rel{
			Kind: ir.Unnest, Scope: s.id, Span: sourceSpan(position), Effects: effects,
			RowResult: rowResult, JSONTableRoot: rootOrdinal, JSONTableArguments: uint16(len(runtimeArguments)),
			JSONTablePassing: uint16(len(n.Passing)), JSONTableFormat: format, JSONTableEncoding: encoding, JSONTableFlags: tableFlags,
		},
		Expressions: expressionIDs, Outputs: outputSlots(internalColumns), JSONTableColumns: binding.specs,
	})
	if err != nil {
		return relation{}, err
	}
	projected, err := binding.project(relation{id: id, columns: internalColumns, effects: effects})
	if err != nil {
		return relation{}, err
	}
	projected.columns, err = aliasOwnedColumns(projected.columns, n.Alias, "")
	if err != nil {
		return relation{}, err
	}
	qualifier := ""
	if n.Alias != nil {
		qualifier = n.Alias.Aliasname
	}
	return b.nameRelation(projected, s, relationQualifier{name: qualifier}, nil)
}

func (state *jsonTableBinding) bindColumns(nodes []*zpgparse.Node, parent uint16) error {
	ordinality := false
	for _, node := range nodes {
		if err := state.binder.ctx.Err(); err != nil {
			return err
		}
		columnNode := node.GetJsonTableColumn()
		if columnNode == nil {
			return bindError("42601", "invalid JSON_TABLE column", state.position)
		}
		if len(state.specs) >= math.MaxUint16 {
			return bindError("54000", "JSON_TABLE column limit exceeded", state.position)
		}
		if columnNode.Coltype == zpgparse.JsonTableColumnType_JTC_NESTED {
			pathName := ""
			if columnNode.Pathspec != nil {
				pathName = columnNode.Pathspec.Name
			}
			if pathName == "" {
				pathName = "json_table_path_" + strconv.FormatUint(uint64(state.pathNameID), 10)
				state.pathNameID++
			}
			if _, duplicate := state.names[pathName]; duplicate {
				return bindError("42712", "duplicate JSON_TABLE column or path name", int(columnNode.Location)+1)
			}
			state.names[pathName] = struct{}{}
			path, err := state.path(columnNode.Pathspec, "", int(columnNode.Location)+1)
			if err != nil {
				return err
			}
			spec := state.baseSpec(ir.JSONTableNested, parent)
			spec.Path = path
			index := uint16(len(state.specs) + 1)
			state.specs = append(state.specs, spec)
			if len(columnNode.Columns) == 0 {
				return bindError("42601", "nested JSON_TABLE path requires columns", int(columnNode.Location)+1)
			}
			if err := state.bindColumns(columnNode.Columns, index); err != nil {
				return err
			}
			continue
		}
		name := columnNode.Name
		if name == "" {
			return bindError("42601", "JSON_TABLE output column requires a name", int(columnNode.Location)+1)
		}
		if _, duplicate := state.names[name]; duplicate {
			return bindError("42712", "duplicate JSON_TABLE column or path name", int(columnNode.Location)+1)
		}
		state.names[name] = struct{}{}
		if columnNode.Coltype == zpgparse.JsonTableColumnType_JTC_FOR_ORDINALITY {
			if ordinality {
				return bindError("42601", "only one FOR ORDINALITY column is allowed per JSON_TABLE column list", int(columnNode.Location)+1)
			}
			ordinality = true
		}
		if columnNode.Coltype == zpgparse.JsonTableColumnType_JTC_FOR_ORDINALITY {
			if err := state.ordinality(columnNode, parent); err != nil {
				return err
			}
			continue
		}
		if err := state.valueColumn(columnNode, parent); err != nil {
			return err
		}
	}
	return nil
}

func (state *jsonTableBinding) baseSpec(kind ir.JSONTableColumnKind, parent uint16) ir.JSONTableColumn {
	return ir.JSONTableColumn{
		Kind: kind, Parent: parent,
		JSONFormat: ir.SQLJSONDefaultFormat, JSONEncoding: ir.SQLJSONDefaultEncoding,
		JSONWrapper: ir.SQLJSONUnspecifiedWrapper, JSONQuotes: ir.SQLJSONUnspecifiedQuotes,
	}
}

func (state *jsonTableBinding) ordinality(node *zpgparse.JsonTableColumn, parent uint16) error {
	target := state.binder.primitive(20)
	layout, err := state.binder.layout(target)
	if err != nil {
		return err
	}
	slot, err := state.binder.slot(state.scope, target, true, 0, 0)
	if err != nil {
		return err
	}
	spec := state.baseSpec(ir.JSONTableOrdinality, parent)
	spec.Output, spec.Layout = uint16(len(state.columns)+1), layout
	state.specs = append(state.specs, spec)
	state.columns = append(state.columns, boundJSONTableColumn{
		spec: spec, name: node.Name, target: target,
		internal: column{ResultColumn: ResultColumn{Name: node.Name, Layout: target, Nullable: true, Slot: slot}},
	})
	return nil
}

func (state *jsonTableBinding) valueColumn(node *zpgparse.JsonTableColumn, parent uint16) error {
	position := int(node.Location) + 1
	target, err := state.binder.typeName(node.TypeName)
	if err != nil {
		return err
	}
	var kind ir.JSONTableColumnKind
	switch node.Coltype {
	case zpgparse.JsonTableColumnType_JTC_REGULAR:
		kind = ir.JSONTableValue
	case zpgparse.JsonTableColumnType_JTC_EXISTS:
		kind = ir.JSONTableExists
	case zpgparse.JsonTableColumnType_JTC_FORMATTED:
		kind = ir.JSONTableQuery
	default:
		return bindError("42601", "invalid JSON_TABLE column kind", position)
	}
	path, err := state.path(node.Pathspec, node.Name, position)
	if err != nil {
		return err
	}
	format, encoding, err := sqlJSONFormat(node.Format)
	if err != nil {
		return err
	}
	wrapper, err := sqlJSONWrapper(node.Wrapper)
	if err != nil {
		return err
	}
	quotes, err := sqlJSONQuotes(node.Quotes)
	if err != nil {
		return err
	}
	if kind == ir.JSONTableValue && (target.Type.Storage == model.TypeArray || target.Type.Storage == model.TypeComposite ||
		target.Type.Storage == model.TypeJSONB || wrapper != ir.SQLJSONUnspecifiedWrapper || quotes != ir.SQLJSONUnspecifiedQuotes) {
		kind = ir.JSONTableQuery
	}
	if kind == ir.JSONTableQuery && quotes == ir.SQLJSONOmitQuotes &&
		(wrapper == ir.SQLJSONConditionalWrapper || wrapper == ir.SQLJSONUnconditionalWrapper) {
		return bindError("42601", "OMIT QUOTES cannot be used with JSON_TABLE WITH WRAPPER", position)
	}
	flags := ir.SQLJSONFlags(0)
	if kind != ir.JSONTableExists && (node.OnEmpty == nil || node.OnEmpty.Btype != zpgparse.JsonBehaviorType_JSON_BEHAVIOR_ERROR) {
		flags |= ir.SQLJSONCaptureEmpty
	}
	if node.OnError == nil || node.OnError.Btype != zpgparse.JsonBehaviorType_JSON_BEHAVIOR_ERROR {
		flags |= ir.SQLJSONCaptureError
	}
	targetID, err := state.binder.layout(target)
	if err != nil {
		return err
	}
	status := state.binder.primitive(2249)
	status.Children = []model.TypeLayout{state.binder.primitive(23), target}
	slot, err := state.binder.slot(state.scope, status, true, 0, 0)
	if err != nil {
		return err
	}
	spec := state.baseSpec(kind, parent)
	spec.Output, spec.Path, spec.Layout = uint16(len(state.columns)+1), path, targetID
	spec.JSONFormat, spec.JSONEncoding = format, encoding
	spec.JSONWrapper, spec.JSONQuotes, spec.JSONFlags = wrapper, quotes, flags
	state.specs = append(state.specs, spec)
	state.columns = append(state.columns, boundJSONTableColumn{
		spec: spec, name: node.Name, target: target, onEmpty: node.OnEmpty, onError: node.OnError,
		internal: column{ResultColumn: ResultColumn{Name: node.Name, Layout: status, Nullable: true, Slot: slot}},
	})
	return nil
}

func (state *jsonTableBinding) path(spec *zpgparse.JsonTablePathSpec, defaultName string, position int) (uint32, error) {
	var node *zpgparse.Node
	if spec != nil {
		node = spec.String_
	}
	if node == nil {
		if defaultName == "" {
			return 0, bindError("42601", "JSON_TABLE path is missing", position)
		}
		node = zpgparse.MakeAConstStrNode("$."+strconv.Quote(defaultName), int32(position-1))
	}
	path, err := state.binder.scalar(node, state.scope)
	if err != nil {
		return 0, err
	}
	path, err = state.binder.coerce(state.scope, path, state.binder.primitive(4072), ir.AssignmentCast)
	if err != nil {
		return 0, err
	}
	if len(state.expressions) >= math.MaxUint32 {
		return 0, bindError("54000", "JSON_TABLE expression limit exceeded", position)
	}
	state.expressions = append(state.expressions, path)
	return uint32(len(state.expressions)), nil
}

func (state *jsonTableBinding) project(input relation) (relation, error) {
	bindings := make([]ir.Binding, len(state.columns))
	columns := make([]column, len(state.columns))
	effects := input.effects
	for i, item := range state.columns {
		value, err := state.binder.reference(state.scope, item.internal, state.position)
		if err != nil {
			return relation{}, err
		}
		if item.spec.Kind != ir.JSONTableOrdinality {
			status, err := state.binder.compositeFieldAt(state.scope, value, 0, state.position)
			if err != nil {
				return relation{}, err
			}
			value, err = state.binder.compositeFieldAt(state.scope, value, 1, state.position)
			if err != nil {
				return relation{}, err
			}
			operation := ir.SQLJSONValue
			if item.spec.Kind == ir.JSONTableExists {
				operation = ir.SQLJSONExists
			} else if item.spec.Kind == ir.JSONTableQuery {
				operation = ir.SQLJSONQuery
			}
			if item.spec.JSONFlags&ir.SQLJSONCaptureEmpty != 0 {
				branch, err := state.binder.sqlJSONBehavior(item.onEmpty, operation, item.target, state.scope, state.position, false)
				if err != nil {
					return relation{}, err
				}
				value, err = state.binder.sqlJSONStatusCase(status, 1, branch, value, state.scope, state.position)
				if err != nil {
					return relation{}, err
				}
			}
			if item.spec.JSONFlags&ir.SQLJSONCaptureError != 0 {
				branch, err := state.binder.sqlJSONBehavior(item.onError, operation, item.target, state.scope, state.position, true)
				if err != nil {
					return relation{}, err
				}
				value, err = state.binder.sqlJSONStatusCase(status, 2, branch, value, state.scope, state.position)
				if err != nil {
					return relation{}, err
				}
			}
		}
		slot, err := state.binder.slot(state.scope, item.target, value.nullable, 0, 0)
		if err != nil {
			return relation{}, err
		}
		columns[i] = column{ResultColumn: ResultColumn{Name: item.name, Layout: item.target, Nullable: value.nullable, Slot: slot}}
		bindings[i] = ir.Binding{Slot: slot, Expr: value.id}
		effects = effects.Union(value.effects)
	}
	id, err := state.binder.builder.AddRel(state.binder.ctx, ir.RelSpec{
		Rel: ir.Rel{Kind: ir.Project, Scope: state.scope.id, Effects: effects},
		Inputs: []ir.RelID{input.id}, Outputs: outputSlots(columns), Bindings: bindings,
	})
	if err != nil {
		return relation{}, err
	}
	return relation{id: id, columns: columns, effects: effects}, nil
}

package bind

import (
	"slices"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
	qcopy "github.com/gosuda/supersql-alpha/internal/query/copy"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type copyOptionState struct {
	options qcopy.Options
	seen    map[string]bool
	lists   map[string][]string
	explicitDelimiter, explicitNull, explicitQuote, explicitEscape bool
	explicitError, explicitReject, explicitLog                   bool
}

func (b *binder) copyStatement(statement *zpgparse.CopyStmt, s *scope) (relation, error) {
	if statement == nil || statement.IsProgram || statement.Filename != "" {
		return relation{}, bindError("42601", "invalid caller-stream COPY statement", 0)
	}
	if statement.IsFrom {
		return b.copyFrom(statement, s)
	}
	return b.copyTo(statement, s)
}

func (b *binder) copyFrom(statement *zpgparse.CopyStmt, s *scope) (relation, error) {
	if statement.Relation == nil || statement.Query != nil {
		return relation{}, bindError("42601", "COPY FROM requires a relation", 0)
	}
	target, err := b.modificationTarget(statement.Relation)
	if err != nil {
		return relation{}, err
	}
	if target.view {
		return relation{}, bindError("42809", "cannot copy to view", int(statement.Relation.Location)+1)
	}
	all := publicColumns(target.table)
	insertable := make([]model.ColumnDescriptor, 0, len(all))
	for _, column := range all {
		if column.Generated == nil {
			insertable = append(insertable, column)
		}
	}
	supplied, err := copyNamedColumns(insertable, statement.Attlist, "COPY FROM")
	if err != nil {
		return relation{}, err
	}
	state, err := b.copyOptions(statement, supplied)
	if err != nil {
		return relation{}, err
	}
	raw, err := b.copyRawSource(s, target, supplied)
	if err != nil {
		return relation{}, err
	}
	input, err := b.copyNormalizeInput(s, target, raw, insertable, supplied)
	if err != nil {
		return relation{}, err
	}
	s.columns = input.columns
	s.namespaces = input.namespaces
	if statement.WhereClause != nil {
		predicate, err := b.restrictedScalar(statement.WhereClause, s, "COPY WHERE")
		if err != nil {
			return relation{}, err
		}
		predicate, err = b.coerce(s, predicate, b.primitive(16), ir.ImplicitCast)
		if err != nil {
			return relation{}, err
		}
		input, err = b.filter(input, s, predicate)
		if err != nil {
			return relation{}, err
		}
	}
	assignments := make([]ir.Binding, 0, len(insertable))
	for i, descriptor := range insertable {
		value, err := b.reference(s, input.columns[i], 0)
		if err != nil {
			return relation{}, err
		}
		slot, err := b.slot(s, descriptor.Layout, descriptor.Nullable, target.descriptor.ID(), descriptor.ID)
		if err != nil {
			return relation{}, err
		}
		assignments = append(assignments, ir.Binding{Slot: slot, Expr: value.id})
		b.dependency(target.descriptor, descriptor.ID)
	}
	columns := copyPlanColumns(supplied, state)
	plan, err := qcopy.NewPlan(qcopy.From, columns, state.options)
	if err != nil {
		return relation{}, err
	}
	b.copy = plan
	return b.modification(s, target, input, ir.Insert, assignments, nil, nil, nil)
}

func (b *binder) copyRawSource(s *scope, target mutationTarget, supplied []model.ColumnDescriptor) (relation, error) {
	columns := make([]column, 0, 2*len(supplied))
	layout := b.primitive(2249)
	layout.Children = make([]model.TypeLayout, 0, 2*len(supplied))
	layout.FieldNames = make([]string, 0, 2*len(supplied))
	for _, descriptor := range supplied {
		slot, err := b.slot(s, descriptor.Layout, true, 0, 0)
		if err != nil {
			return relation{}, err
		}
		columns = append(columns, column{ResultColumn: ResultColumn{Name: descriptor.Name, Layout: descriptor.Layout, Nullable: true, Slot: slot}, qualifier: target.qualifier.name, namespace: target.qualifier.namespace})
		layout.Children = append(layout.Children, descriptor.Layout)
		layout.FieldNames = append(layout.FieldNames, descriptor.Name)
	}
	boolean := b.primitive(16)
	for i, descriptor := range supplied {
		slot, err := b.slot(s, boolean, true, 0, 0)
		if err != nil {
			return relation{}, err
		}
		columns = append(columns, column{ResultColumn: ResultColumn{Name: "copy_default_" + strconv.Itoa(i), Layout: boolean, Nullable: true, Slot: slot}, hidden: true, internal: true})
		layout.Children = append(layout.Children, boolean)
		layout.FieldNames = append(layout.FieldNames, "copy_default_"+descriptor.Name)
	}
	rowLayout, err := b.layout(layout)
	if err != nil {
		return relation{}, err
	}
	effects := ir.Effects{Volatility: ir.Volatile, MayError: true}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{
		Kind: ir.Unnest, Scope: s.id, Effects: effects, RowFunction: qcopy.RowSourceFunction, RowResult: rowLayout,
	}, Outputs: outputSlots(columns)})
	return relation{id: id, columns: columns, effects: effects}, err
}

func (b *binder) copyNormalizeInput(s *scope, target mutationTarget, raw relation, all, supplied []model.ColumnDescriptor) (relation, error) {
	columns := make([]column, 0, len(all))
	bindings := make([]ir.Binding, 0, len(all))
	effects := raw.effects
	for _, descriptor := range all {
		index := slices.IndexFunc(supplied, func(column model.ColumnDescriptor) bool { return column.ID == descriptor.ID })
		var value expression
		var err error
		if index < 0 {
			value, err = b.columnDefault(descriptor, s)
		} else {
			actual, referenceErr := b.reference(s, raw.columns[index], 0)
			if referenceErr != nil {
				return relation{}, referenceErr
			}
			useDefault, referenceErr := b.reference(s, raw.columns[len(supplied)+index], 0)
			if referenceErr != nil {
				return relation{}, referenceErr
			}
			fallback, defaultErr := b.columnDefault(descriptor, s)
			if defaultErr != nil {
				return relation{}, defaultErr
			}
			value, err = b.addScalar(s, expression{typ: descriptor.Layout, nullable: actual.nullable || fallback.nullable}, ir.Scalar{Kind: ir.Case}, []expression{useDefault, fallback, actual})
		}
		if err != nil {
			return relation{}, err
		}
		value, err = b.coerceColumnAssignment(value, s, descriptor)
		if err != nil {
			return relation{}, err
		}
		slot, err := b.slot(s, descriptor.Layout, true, target.descriptor.ID(), descriptor.ID)
		if err != nil {
			return relation{}, err
		}
		columns = append(columns, column{ResultColumn: ResultColumn{Name: descriptor.Name, Layout: descriptor.Layout, Nullable: true, Slot: slot}, qualifier: target.qualifier.name, namespace: target.qualifier.namespace})
		bindings = append(bindings, ir.Binding{Slot: slot, Expr: value.id})
		effects = effects.Union(value.effects)
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: s.id, Effects: effects}, Inputs: []ir.RelID{raw.id}, Outputs: outputSlots(columns), Bindings: bindings})
	return relation{id: id, columns: columns, effects: effects}, err
}

func (b *binder) copyTo(statement *zpgparse.CopyStmt, s *scope) (relation, error) {
	if statement.WhereClause != nil {
		return relation{}, bindError("42601", "WHERE is not allowed with COPY TO", 0)
	}
	var input relation
	var err error
	switch {
	case statement.Query != nil && statement.Relation == nil && len(statement.Attlist) == 0:
		input, err = b.statement(statement.Query, s)
	case statement.Relation != nil && statement.Query == nil:
		input, err = b.from(&zpgparse.Node{Node: &zpgparse.Node_RangeVar{RangeVar: statement.Relation}}, s)
		if err == nil {
			input, err = b.copyOutputColumns(input, statement.Attlist, s)
		}
	default:
		return relation{}, bindError("42601", "invalid COPY TO source", 0)
	}
	if err != nil {
		return relation{}, err
	}
	optionColumns := make([]model.ColumnDescriptor, len(input.columns))
	columns := make([]qcopy.Column, len(input.columns))
	for i, column := range input.columns {
		optionColumns[i].Name = column.Name
		columns[i] = qcopy.Column{Name: column.Name, Layout: column.Layout, Nullable: column.Nullable}
	}
	state, err := b.copyOptions(statement, optionColumns)
	if err != nil {
		return relation{}, err
	}
	columns = applyCopyColumnOptions(columns, state)
	plan, err := qcopy.NewPlan(qcopy.To, columns, state.options)
	if err != nil {
		return relation{}, err
	}
	b.copy = plan
	return input, nil
}

func (b *binder) copyOutputColumns(input relation, namesAST []*zpgparse.Node, s *scope) (relation, error) {
	available := make([]column, 0, len(input.columns))
	for _, column := range input.columns {
		if column.visibleToStar(relationQualifier{}) {
			available = append(available, column)
		}
	}
	selected := available
	if len(namesAST) != 0 {
		names, err := names(namesAST)
		if err != nil {
			return relation{}, err
		}
		selected = nil
		seen := make(map[string]bool, len(names))
		for _, name := range names {
			if seen[name] {
				return relation{}, bindError("42701", "column specified more than once", 0)
			}
			seen[name] = true
			index := slices.IndexFunc(available, func(column column) bool { return column.Name == name })
			if index < 0 {
				return relation{}, bindError("42703", "COPY column does not exist", 0)
			}
			selected = append(selected, available[index])
		}
	}
	s.columns = input.columns
	columns := make([]column, len(selected))
	bindings := make([]ir.Binding, len(selected))
	for i, source := range selected {
		value, err := b.reference(s, source, 0)
		if err != nil {
			return relation{}, err
		}
		slot, err := b.slot(s, source.Layout, source.Nullable, 0, 0)
		if err != nil {
			return relation{}, err
		}
		columns[i] = source
		columns[i].Slot, columns[i].qualifier, columns[i].namespace = slot, "", ""
		bindings[i] = ir.Binding{Slot: slot, Expr: value.id}
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: s.id, Effects: input.effects}, Inputs: []ir.RelID{input.id}, Outputs: outputSlots(columns), Bindings: bindings})
	return relation{id: id, columns: columns, effects: input.effects}, err
}

func copyNamedColumns(available []model.ColumnDescriptor, list []*zpgparse.Node, operation string) ([]model.ColumnDescriptor, error) {
	if len(list) == 0 {
		return slices.Clone(available), nil
	}
	names, err := names(list)
	if err != nil {
		return nil, err
	}
	result := make([]model.ColumnDescriptor, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			return nil, bindError("42701", "column specified more than once", 0)
		}
		seen[name] = true
		index := slices.IndexFunc(available, func(column model.ColumnDescriptor) bool { return column.Name == name })
		if index < 0 {
			return nil, bindError("42703", operation+" column does not exist or is generated", 0)
		}
		result = append(result, available[index])
	}
	return result, nil
}

func (b *binder) copyOptions(statement *zpgparse.CopyStmt, columns []model.ColumnDescriptor) (copyOptionState, error) {
	state := copyOptionState{
		options: qcopy.Options{Format: qcopy.Text, Delimiter: '\t', Null: `\N`, Quote: '"', Escape: '"'},
		seen: make(map[string]bool), lists: make(map[string][]string),
	}
	for _, node := range statement.Options {
		option := node.GetDefElem()
		if option == nil || option.Defnamespace != "" || option.Defaction != zpgparse.DefElemAction_DEFELEM_UNSPEC {
			return state, bindError("42601", "invalid COPY option", 0)
		}
		name := strings.ToLower(option.Defname)
		if state.seen[name] {
			return state, bindError("42601", "COPY option specified more than once: "+name, int(option.Location)+1)
		}
		state.seen[name] = true
		position := int(option.Location) + 1
		switch name {
		case "format":
			value, err := copyStringOption(option)
			if err != nil { return state, err }
			switch strings.ToLower(value) {
			case "text": state.options.Format = qcopy.Text
			case "csv": state.options.Format = qcopy.CSV
			case "binary": state.options.Format = qcopy.Binary
			default: return state, bindError("22023", "COPY format must be text, csv, or binary", position)
			}
		case "freeze":
			value, err := copyBooleanOption(option)
			if err != nil { return state, err }
			if value { return state, unsupported(PhysicalStorage, position) }
		case "delimiter":
			value, err := copyByteOption(option, "delimiter")
			if err != nil { return state, err }
			state.options.Delimiter, state.explicitDelimiter = value, true
		case "null":
			value, err := copyStringOption(option)
			if err != nil { return state, err }
			state.options.Null, state.explicitNull = value, true
		case "default":
			value, err := copyStringOption(option)
			if err != nil { return state, err }
			state.options.Default, state.options.HasDefault = value, true
		case "header":
			if value, ok := copyRawString(option.Arg); ok && strings.EqualFold(value, "match") {
				state.options.Header = qcopy.HeaderMatch
			} else {
				value, err := copyBooleanOption(option)
				if err != nil { return state, err }
				if value { state.options.Header = qcopy.Header }
			}
		case "quote":
			value, err := copyByteOption(option, "quote")
			if err != nil { return state, err }
			state.options.Quote, state.explicitQuote = value, true
		case "escape":
			value, err := copyByteOption(option, "escape")
			if err != nil { return state, err }
			state.options.Escape, state.explicitEscape = value, true
		case "force_quote", "force_not_null", "force_null":
			value, err := copyColumnList(option, columns)
			if err != nil { return state, err }
			state.lists[name] = value
		case "encoding":
			value, err := copyStringOption(option)
			if err != nil { return state, err }
			value = strings.ReplaceAll(strings.ToUpper(value), "-", "")
			if value != "UTF8" && value != "UTF_8" { return state, unsupported(LocaleProviders, position) }
		case "on_error":
			value, err := copyStringOption(option)
			if err != nil { return state, err }
			state.explicitError = true
			switch strings.ToLower(value) {
			case "stop": state.options.ErrorMode = qcopy.StopOnError
			case "ignore": state.options.ErrorMode = qcopy.IgnoreConversionErrors
			default: return state, bindError("22023", "COPY ON_ERROR must be stop or ignore", position)
			}
		case "reject_limit":
			value, err := copyIntegerOption(option)
			if err != nil || value <= 0 { return state, bindError("22023", "COPY REJECT_LIMIT must be greater than zero", position) }
			state.options.RejectLimit, state.explicitReject = uint64(value), true
		case "log_verbosity":
			value, err := copyStringOption(option)
			if err != nil { return state, err }
			state.explicitLog = true
			switch strings.ToLower(value) {
			case "default": state.options.LogVerbosity = qcopy.LogDefault
			case "verbose": state.options.LogVerbosity = qcopy.LogVerbose
			case "silent": state.options.LogVerbosity = qcopy.LogSilent
			default: return state, bindError("22023", "COPY LOG_VERBOSITY must be default, verbose, or silent", position)
			}
		default:
			return state, bindError("22023", "COPY option not recognized: "+name, position)
		}
	}
	if state.options.Format == qcopy.CSV {
		if !state.explicitDelimiter { state.options.Delimiter = ',' }
		if !state.explicitNull { state.options.Null = "" }
		if !state.explicitQuote { state.options.Quote = '"' }
		if !state.explicitEscape { state.options.Escape = state.options.Quote }
	}
	if err := validateCopyOptions(statement.IsFrom, &state); err != nil {
		return state, err
	}
	return state, nil
}

func validateCopyOptions(from bool, state *copyOptionState) error {
	options := state.options
	binaryOnly := state.explicitDelimiter || state.explicitNull || options.HasDefault || options.Header != qcopy.NoHeader || state.explicitQuote || state.explicitEscape || len(state.lists) != 0 || state.explicitError || state.explicitReject || state.explicitLog || state.seen["encoding"]
	if options.Format == qcopy.Binary && binaryOnly {
		return bindError("22023", "COPY binary format does not allow text or CSV options", 0)
	}
	if options.Format != qcopy.CSV && (state.explicitQuote || state.explicitEscape || state.seen["force_quote"] || state.seen["force_not_null"] || state.seen["force_null"]) {
		return bindError("22023", "COPY quote and FORCE options require CSV format", 0)
	}
	if (from && state.seen["force_quote"]) || (!from && (state.seen["force_not_null"] || state.seen["force_null"])) {
		return bindError("22023", "COPY FORCE option is not valid for this direction", 0)
	}
	if !from && (state.seen["freeze"] || options.HasDefault || state.explicitError || state.explicitReject || state.explicitLog || options.Header == qcopy.HeaderMatch) {
		return bindError("22023", "COPY option is only valid with COPY FROM", 0)
	}
	if (state.explicitReject && options.ErrorMode != qcopy.IgnoreConversionErrors) || (state.explicitLog && options.ErrorMode != qcopy.IgnoreConversionErrors) {
		return bindError("22023", "COPY REJECT_LIMIT and LOG_VERBOSITY require ON_ERROR ignore", 0)
	}
	if (strings.IndexByte(options.Null, options.Delimiter) >= 0 || options.HasDefault && strings.IndexByte(options.Default, options.Delimiter) >= 0) && options.Format != qcopy.Binary {
		return bindError("22023", "COPY null and default representations must not contain the delimiter", 0)
	}
	return nil
}

func copyStringOption(option *zpgparse.DefElem) (string, error) {
	if value, ok := copyRawString(option.Arg); ok {
		return value, nil
	}
	return "", bindError("22023", "COPY option requires a string value", int(option.Location)+1)
}

func copyRawString(node *zpgparse.Node) (string, bool) {
	if node == nil { return "true", true }
	if value := node.GetString_(); value != nil { return value.Sval, true }
	if value := node.GetBoolean(); value != nil {
		if value.Boolval { return "true", true }
		return "false", true
	}
	if value := node.GetAConst(); value != nil && !value.Isnull {
		if text := value.GetSval(); text != nil { return text.Sval, true }
		if integer := value.GetIval(); integer != nil { return strconv.FormatInt(int64(integer.Ival), 10), true }
	}
	return "", false
}

func copyBooleanOption(option *zpgparse.DefElem) (bool, error) {
	value, ok := copyRawString(option.Arg)
	if !ok { return false, bindError("22023", "COPY option requires a boolean", int(option.Location)+1) }
	switch strings.ToLower(value) {
	case "true", "on", "yes", "1": return true, nil
	case "false", "off", "no", "0": return false, nil
	default: return false, bindError("22023", "COPY option requires a boolean", int(option.Location)+1)
	}
}

func copyByteOption(option *zpgparse.DefElem, name string) (byte, error) {
	value, err := copyStringOption(option)
	if err != nil { return 0, err }
	if len(value) != 1 || value[0] == 0 || value[0] >= 0x80 || value[0] == '\r' || value[0] == '\n' {
		return 0, bindError("22023", "COPY "+name+" must be one single-byte character", int(option.Location)+1)
	}
	return value[0], nil
}

func copyIntegerOption(option *zpgparse.DefElem) (int64, error) {
	if option.Arg != nil {
		if integer := option.Arg.GetInteger(); integer != nil { return int64(integer.Ival), nil }
		if constant := option.Arg.GetAConst(); constant != nil && constant.GetIval() != nil { return int64(constant.GetIval().Ival), nil }
		if value, ok := copyRawString(option.Arg); ok { return strconv.ParseInt(value, 10, 64) }
	}
	return 0, bindError("22023", "COPY option requires an integer", int(option.Location)+1)
}

func copyColumnList(option *zpgparse.DefElem, columns []model.ColumnDescriptor) ([]string, error) {
	if option.Arg == nil { return nil, bindError("22023", "COPY FORCE option requires columns", int(option.Location)+1) }
	if option.Arg.GetAStar() != nil {
		result := make([]string, len(columns))
		for i := range columns { result[i] = columns[i].Name }
		return result, nil
	}
	list := option.Arg.GetList()
	if list == nil { return nil, bindError("22023", "COPY FORCE option requires columns", int(option.Location)+1) }
	result, err := names(list.Items)
	if err != nil { return nil, err }
	seen := make(map[string]bool, len(result))
	for _, name := range result {
		if seen[name] { return nil, bindError("42701", "COPY FORCE column specified more than once", int(option.Location)+1) }
		seen[name] = true
		if slices.IndexFunc(columns, func(column model.ColumnDescriptor) bool { return column.Name == name }) < 0 {
			return nil, bindError("42703", "COPY FORCE column is not in the COPY column list", int(option.Location)+1)
		}
	}
	return result, nil
}

func copyPlanColumns(columns []model.ColumnDescriptor, state copyOptionState) []qcopy.Column {
	result := make([]qcopy.Column, len(columns))
	for i, column := range columns {
		result[i] = qcopy.Column{Name: column.Name, Layout: column.Layout, Nullable: column.Nullable}
	}
	return applyCopyColumnOptions(result, state)
}

func applyCopyColumnOptions(columns []qcopy.Column, state copyOptionState) []qcopy.Column {
	for i := range columns {
		columns[i].ForceQuote = slices.Contains(state.lists["force_quote"], columns[i].Name)
		columns[i].ForceNotNull = slices.Contains(state.lists["force_not_null"], columns[i].Name)
		columns[i].ForceNull = slices.Contains(state.lists["force_null"], columns[i].Name)
	}
	return columns
}


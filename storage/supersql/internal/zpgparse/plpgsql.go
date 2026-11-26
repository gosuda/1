package zpgparse

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"github.com/gosuda/supersql-alpha/internal/zpgparse/internal/neoparse"
	pgproto "github.com/gosuda/supersql-alpha/internal/zpgparse/internal/pgproto"
	"github.com/gosuda/supersql-alpha/internal/zpgparse/internal/plpgsql"
)

type PLpgSQLExpression = plpgsql.Expression
type PLpgSQLDatum = plpgsql.Datum
type PLpgSQLDatumKind = plpgsql.DatumKind
type PLpgSQLFunction = plpgsql.Function
type PLpgSQLParameter = plpgsql.Parameter
type PLpgSQLConfig = plpgsql.Config
type PLpgSQLStatement = plpgsql.Statement
type PLpgSQLBlock = plpgsql.Block
type PLpgSQLAssign = plpgsql.Assign
type PLpgSQLIf = plpgsql.If
type PLpgSQLElsif = plpgsql.Elsif
type PLpgSQLCase = plpgsql.Case
type PLpgSQLWhen = plpgsql.When
type PLpgSQLLoop = plpgsql.Loop
type PLpgSQLExit = plpgsql.Exit
type PLpgSQLReturn = plpgsql.Return
type PLpgSQLRaise = plpgsql.Raise
type PLpgSQLRaiseOption = plpgsql.RaiseOption
type PLpgSQLAssert = plpgsql.Assert
type PLpgSQLExecute = plpgsql.Execute
type PLpgSQLGetDiagnostics = plpgsql.GetDiagnostics
type PLpgSQLOpen = plpgsql.Open
type PLpgSQLFetch = plpgsql.Fetch
type PLpgSQLClose = plpgsql.Close
type PLpgSQLPerform = plpgsql.Perform
type PLpgSQLCall = plpgsql.Call
type PLpgSQLTransaction = plpgsql.Transaction

const (
	PLpgSQLScalar      = plpgsql.Scalar
	PLpgSQLRow         = plpgsql.Row
	PLpgSQLRecord      = plpgsql.Record
	PLpgSQLRecordField = plpgsql.RecordField
	PLpgSQLPromise     = plpgsql.Promise
)

// ParsePLpgSQL exposes the typed compiler tree used by routine execution.
func ParsePLpgSQL(source string, config PLpgSQLConfig) (*PLpgSQLFunction, error) {
	function, err := plpgsql.Parse(source, config, neoparse.ParseMode)
	if err == nil {
		return function, nil
	}
	var parseError *plpgsql.Error
	if errors.As(err, &parseError) {
		return nil, &Error{Message: parseError.Message, Lineno: parseError.Line, Cursorpos: parseError.Cursorpos}
	}
	return nil, err
}

func ParsePLpgSQLFunction(statement *CreateFunctionStmt) (*PLpgSQLFunction, error) {
	return parsePlpgsqlFunction(statement)
}

func ParsePLpgSQLDO(statement *DoStmt) (*PLpgSQLFunction, error) {
	if statement == nil {
		return nil, fmt.Errorf("missing DO statement")
	}
	body, language, present := plpgsqlOptions(statement.Args)
	if language != "plpgsql" {
		return nil, fmt.Errorf("unsupported procedural language %q", language)
	}
	if !present {
		return nil, fmt.Errorf("no inline code specified")
	}
	return ParsePLpgSQL(body, PLpgSQLConfig{Inline: true, ReturnType: "void", Name: "inline_code_block"})
}

// ParsePlPgSqlToJSON parses CREATE FUNCTION, CREATE PROCEDURE, and DO bodies.
func ParsePlPgSqlToJSON(input string) (string, error) {
	tree, err := Parse(input)
	if err != nil {
		return "", err
	}
	var functions []*plpgsql.Function
	visit := func(message protoreflect.Message) bool {
		if err != nil {
			return false
		}
		var function *plpgsql.Function
		switch node := message.Interface().(type) {
		case *pgproto.CreateFunctionStmt:
			function, err = parsePlpgsqlFunction(node)
		case *pgproto.DoStmt:
			body, language, present := plpgsqlOptions(node.Args)
			if language != "plpgsql" {
				function = &plpgsql.Function{}
			} else if !present {
				err = fmt.Errorf("no inline code specified")
			} else {
				function, err = plpgsql.Parse(body, plpgsql.Config{Inline: true, ReturnType: "void", Name: "inline_code_block"}, neoparse.ParseMode)
			}
		default:
			return true
		}
		if err == nil {
			functions = append(functions, function)
		}
		return err == nil
	}
	for _, statement := range tree.Stmts {
		walkPlpgsqlTree(statement.Stmt.ProtoReflect(), visit)
		if err != nil {
			break
		}
	}
	if err != nil {
		var parseError *plpgsql.Error
		if errors.As(err, &parseError) {
			return "", &Error{Message: parseError.Message, Lineno: parseError.Line, Cursorpos: parseError.Cursorpos}
		}
		return "", err
	}
	return plpgsql.ToJSON(functions), nil
}

func plpgsqlOptions(options []*pgproto.Node) (body, language string, present bool) {
	language = "plpgsql"
	for _, option := range options {
		def := option.GetDefElem()
		if def == nil {
			continue
		}
		switch def.Defname {
		case "language":
			language = def.Arg.GetString_().GetSval()
		case "as":
			if list := def.Arg.GetList(); list != nil {
				for _, item := range list.Items {
					body = item.GetString_().GetSval()
					present = true
				}
			} else if value := def.Arg.GetString_(); value != nil {
				body, present = value.Sval, true
			}
		}
	}
	return body, language, present
}

// plpgsqlTypeName renders a parsed TypeName as the reference string the
// plpgsql compiler resolves later: every qualified part survives, delimited
// names are re-quoted, and %TYPE rides as a suffix like a DECLARE reference.
func plpgsqlTypeName(typ *pgproto.TypeName) string {
	if typ == nil || len(typ.Names) == 0 {
		return ""
	}
	names := make([]string, len(typ.Names))
	for i, node := range typ.Names {
		names[i] = node.GetString_().GetSval()
	}
	// A scalar's pg_catalog qualification is noise for the plpgsql datum
	// dump: int reads as int4 either way, and the resolved name matches the
	// reference corpus. %TYPE references keep the schema: it qualifies the
	// relation whose column type is copied.
	if !typ.PctType && len(names) == 2 && names[0] == "pg_catalog" {
		names = names[1:]
	}
	parts := make([]string, len(names))
	for i, part := range names {
		if !plpgsqlPlainIdentifier(part) {
			part = `"` + strings.ReplaceAll(part, `"`, `""`) + `"`
		}
		parts[i] = part
	}
	name := strings.Join(parts, ".")
	if len(typ.ArrayBounds) > 0 {
		name = "_" + name
	}
	if typ.PctType {
		name += "%TYPE"
	}
	return name
}

// plpgsqlPlainIdentifier reports whether name needs no quoting: only folded
// names may stay bare because catalog lookups treat a delimited spelling as
// an exact match and never apply builtin aliases.
func plpgsqlPlainIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r == '_', r == '$':
		case r >= '0' && r <= '9' && i != 0:
		default:
			return false
		}
	}
	return true
}
func parsePlpgsqlFunction(node *pgproto.CreateFunctionStmt) (*plpgsql.Function, error) {
	body, language, present := plpgsqlOptions(node.Options)
	if language != "plpgsql" {
		return &plpgsql.Function{}, nil
	}
	if !present {
		return nil, fmt.Errorf("no function body specified")
	}

	config := plpgsql.Config{Procedure: node.IsProcedure, ReturnType: plpgsqlTypeName(node.ReturnType), ReturnsSet: node.ReturnType.GetSetof()}
	if len(node.Funcname) > 0 {
		config.Name = node.Funcname[0].GetString_().GetSval()
	}
	outputs := 0
	outputType := ""
	for _, parameter := range node.Parameters {
		arg := parameter.GetFunctionParameter()
		if arg == nil {
			return nil, fmt.Errorf("invalid function parameter")
		}

		output := arg.Mode == pgproto.FunctionParameterMode_FUNC_PARAM_OUT || arg.Mode == pgproto.FunctionParameterMode_FUNC_PARAM_INOUT || arg.Mode == pgproto.FunctionParameterMode_FUNC_PARAM_TABLE
		input := arg.Mode != pgproto.FunctionParameterMode_FUNC_PARAM_OUT && arg.Mode != pgproto.FunctionParameterMode_FUNC_PARAM_TABLE
		typ := plpgsqlTypeName(arg.ArgType)
		if arg.Mode == pgproto.FunctionParameterMode_FUNC_PARAM_VARIADIC && typ != "anyarray" && typ != "anycompatiblearray" && typ != "any" {
			return nil, fmt.Errorf("VARIADIC parameter must be an array")
		}
		if output {
			outputs++
			outputType = typ
		}
		if arg.Mode == pgproto.FunctionParameterMode_FUNC_PARAM_TABLE {
			config.ReturnsSet = true
		}
		config.Parameters = append(config.Parameters, plpgsql.Parameter{Name: arg.Name, Type: typ, Input: input, Output: output})
	}
	if outputs > 1 || outputs > 0 && node.IsProcedure {
		outputType = "record"
	}
	if config.ReturnType == "" {
		config.ReturnType = outputType
		if config.ReturnType == "" && node.IsProcedure {
			config.ReturnType = "void"
		}
		if config.ReturnType == "" {
			return nil, fmt.Errorf("function result type must be specified")
		}
	} else if outputs > 0 && config.ReturnType != outputType {
		return nil, fmt.Errorf("function result type must be %s because of OUT parameters", outputType)
	}
	return plpgsql.Parse(body, config, neoparse.ParseMode)
}
func walkPlpgsqlTree(m protoreflect.Message, visit func(protoreflect.Message) bool) {
	if m == nil || !m.IsValid() {
		return
	}
	m = utilityUnwrap(m)
	name := string(m.Descriptor().Name())
	if name == "RawStmt" {
		if f := utilityField(m, "stmt"); f != nil && m.Has(f) {
			walkPlpgsqlTree(m.Get(f).Message(), visit)
		}
		return
	}
	visit(m)
	if name == "CreateFunctionStmt" || name == "DoStmt" {
		return
	}
	fields := m.Descriptor().Fields()
	for i := range fields.Len() {
		f := fields.Get(i)
		if f.IsMap() {
			continue
		}
		if f.Kind() != protoreflect.MessageKind && f.Kind() != protoreflect.GroupKind {
			continue
		}
		if f.IsList() {
			l := m.Get(f).List()
			for j := range l.Len() {
				if mm, ok := l.Get(j).Interface().(protoreflect.Message); ok {
					walkPlpgsqlTree(mm, visit)
				}
			}
		} else if m.Has(f) {
			walkPlpgsqlTree(m.Get(f).Message(), visit)
		}
	}
}

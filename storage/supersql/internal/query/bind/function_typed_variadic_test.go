package bind_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestTypedVariadicArgumentsReachKernelAsOneArray(t *testing.T) {
	for _, tc := range []struct {
		name, sql, want string
		null            bool
	}{
		{"expanded", `SELECT json_extract_path_text('{"a":{"b":"value"}}','a','b')`, "value", false},
		{"explicit", `SELECT json_extract_path_text('{"a":{"b":"value"}}',VARIADIC ARRAY['a','b'])`, "value", false},
		{"implicit array coercion", `SELECT json_extract_path_text('{"a":{"b":"value"}}',VARIADIC ARRAY['a','b']::varchar[])`, "value", false},
		{"unknown array literal", `SELECT json_extract_path_text('{"a":{"b":"value"}}',VARIADIC '{a,b}')`, "value", false},
		{"empty array", `SELECT json_extract_path_text('{"a":1}',VARIADIC ARRAY[]::text[])`, `{"a":1}`, false},
		{"null array", `SELECT json_extract_path_text('{"a":1}',VARIADIC NULL::text[])`, "", true},
		{"unknown null array", `SELECT json_extract_path_text('{"a":1}',VARIADIC NULL)`, "", true},
		{"null element", `SELECT json_extract_path_text('{"a":1}',NULL)`, "", true},
		{"named", `SELECT json_extract_path_text(from_json=>'{"a":"value"}',VARIADIC path_elems=>ARRAY['a'])`, "value", false},
		{"mixed", `SELECT json_extract_path_text('{"a":"value"}',VARIADIC path_elems=>ARRAY['a'])`, "value", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statement, err := bindSQL(t, tc.sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, call, signature := variadicRoot(t, statement)
			if signature.ParameterCount() != 2 || call.OperandCount() != 2 || signature.VariadicArray {
				t.Fatalf("typed variadic call must pass two ordinary arguments: call=%+v signature=%+v", call, signature)
			}
			array, _ := statement.Graph().SignatureParameter(call.Signature, 1)
			typ, _ := statement.Graph().Type(array)
			child, _ := statement.Graph().LayoutChild(array, 0)
			element, _ := statement.Graph().Type(child)
			if typ.ID != 1009 || typ.Storage != model.TypeArray || element.ID != 25 {
				t.Fatalf("final argument = %+v of %+v, want text[]", typ, element)
			}
			values, err := evaluateRecordExpression(t, statement, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := values[0]; got.IsNull != tc.null || !tc.null && got.StrVal != tc.want {
				t.Fatalf("result = %+v, want %q (NULL=%v)", got, tc.want, tc.null)
			}
		})
	}
}

func TestTypedVariadicPacksMaximumSuppliedArguments(t *testing.T) {
	arguments := make([]string, 99)
	for i := range arguments {
		arguments[i] = "'" + strconv.Itoa(i) + "'"
	}
	statement, err := bindSQL(t, `SELECT json_extract_path('{}',`+strings.Join(arguments, ",")+`)`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	root, call, signature := variadicRoot(t, statement)
	if call.OperandCount() != 2 || signature.ParameterCount() != 2 {
		t.Fatalf("packed call=%+v signature=%+v", call, signature)
	}
	array, _ := statement.Graph().Operand(root, 1)
	packed, _ := statement.Graph().Scalar(array)
	if packed.Kind != ir.Array || packed.OperandCount() != 99 {
		t.Fatalf("packed path = %+v, want 99 elements", packed)
	}
	for i := range arguments {
		operand, _ := statement.Graph().Operand(array, uint32(i))
		value, err := statement.Graph().CloneConstant(t.Context(), operand, 1024)
		if err != nil || value.IsNull || value.StrVal != strconv.Itoa(i) {
			t.Fatalf("element %d = %+v, %v", i, value, err)
		}
	}
}

func TestTypedVariadicInfersScalarAndArrayParameters(t *testing.T) {
	for _, tc := range []struct {
		sql  string
		path model.TypeID
	}{
		{`SELECT json_extract_path($1,$2,$2)`, 25},
		{`SELECT json_extract_path($1,VARIADIC $2)`, 1009},
		{`SELECT json_extract_path(from_json=>$1,VARIADIC path_elems=>$2)`, 1009},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			statement, err := bindSQL(t, tc.sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			parameters := statement.Parameters()
			if len(parameters) != 2 || parameters[0].Type.ID != 114 || parameters[1].Type.ID != tc.path {
				t.Fatalf("parameters=%+v, want json and type %d", parameters, tc.path)
			}
		})
	}
}

func TestTypedVariadicRejectsInvalidCalls(t *testing.T) {
	for _, tc := range []struct{ name, sql, code string }{
		{"zero expanded elements", `SELECT json_extract_path('{}')`, "42883"},
		{"array without marker", `SELECT json_extract_path('{}',ARRAY['a'])`, "42883"},
		{"incompatible element", `SELECT json_extract_path('{}',1)`, "42883"},
		{"incompatible explicit array", `SELECT json_extract_path('{}',VARIADIC ARRAY[1])`, "42883"},
		{"expanded named arguments", `SELECT json_extract_path(from_json=>'{}',path_elems=>'a')`, "42883"},
		{"marker on wrong named parameter", `SELECT json_extract_path(path_elems=>ARRAY['a'],VARIADIC from_json=>'{}')`, "42883"},
		{"duplicate argument name", `SELECT json_extract_path(from_json=>'{}',VARIADIC from_json=>ARRAY['a'])`, "42601"},
		{"too many supplied arguments", `SELECT json_extract_path('{}',` + strings.Repeat("'a',", 99) + `'a')`, "54023"},
		{"explicit marker with expanded elements", `SELECT json_extract_path('{}','a',VARIADIC ARRAY['b'])`, "42883"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindSQL(t, tc.sql, nil, nil)
			requireCode(t, err, tc.code)
		})
	}
}

func TestTypedVariadicCoercesPrefixBeforeArray(t *testing.T) {
	const sql = `SELECT json_extract_path('{',VARIADIC '{')`
	_, err := bindSQL(t, sql, nil, nil)
	requireCode(t, err, "22P02")
	var sqlError *model.Error
	if !errors.As(err, &sqlError) || sqlError.Position != strings.Index(sql, "'{'")+1 {
		t.Fatalf("error = %v, want invalid JSON prefix position", err)
	}
}

func TestTypedVariadicCandidatePrefersMatchingFixedOverload(t *testing.T) {
	for _, tc := range []struct {
		sql      string
		function model.FunctionID
		argument model.TypeID
	}{
		{`SELECT jsonb_delete('{}','a')`, 3302, 25},
		{`SELECT jsonb_delete('{}',NULL)`, 3302, 25},
		{`SELECT jsonb_delete('{}',1)`, 3303, 23},
		{`SELECT jsonb_delete('{}','a','b')`, 3343, 1009},
		{`SELECT jsonb_delete('{}',VARIADIC ARRAY['a'])`, 3343, 1009},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			statement, err := bindSQL(t, tc.sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, call, signature := variadicRoot(t, statement)
			layout, _ := statement.Graph().SignatureParameter(call.Signature, 1)
			typ, _ := statement.Graph().Type(layout)
			if call.Function != tc.function || typ.ID != tc.argument || signature.VariadicArray {
				t.Fatalf("function=%d argument=%+v signature=%+v, want function %d argument %d", call.Function, typ, signature, tc.function, tc.argument)
			}
		})
	}
}

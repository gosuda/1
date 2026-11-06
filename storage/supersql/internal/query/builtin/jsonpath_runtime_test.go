package builtin_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

const jsonPathTestMemory = uint64(64 << 20)

func jsonPathBudget(output int64) builtin.CallBudget {
	return builtin.CallBudget{OutputBytes: output, MemoryBytes: jsonPathTestMemory}
}

func jsonPathValue(t *testing.T, source string) model.Value {
	t.Helper()
	call, err := builtin.Resolve(4001, builtin.ResolveOptions{})
	if err != nil { t.Fatal(err) }
	value, err := call.Call(t.Context(), []model.Value{model.StringVal(source)}, jsonPathBudget(1<<20))
	if err != nil { t.Fatal(err) }
	return value
}

func callJSONPath(t *testing.T, id model.FunctionID, document, path, variables string, silent bool) model.Value {
	t.Helper()
	call, err := builtin.Resolve(id, builtin.ResolveOptions{})
	if err != nil { t.Fatal(err) }
	arguments := []model.Value{{Type: model.TypeJSONB, StrVal: document}, jsonPathValue(t, path)}
	if call.ArgumentCount() == 4 {
		arguments = append(arguments, model.Value{Type: model.TypeJSONB, StrVal: variables}, model.BoolVal(silent))
	}
	value, err := call.Call(t.Context(), arguments, jsonPathBudget(1<<20))
	if err != nil { t.Fatal(err) }
	return value
}

func TestJSONPathCanonicalAndBinaryProtocolRoundTrip(t *testing.T) {
	input := jsonPathValue(t, `strict $.a[0, last - 1].**{2 to 5}?(@ like_regex "^x" flag "im")`)
	if len(input.BytesVal) < 2 || input.BytesVal[0] != 1 { t.Fatalf("jsonpath wire frame = %x", input.BytesVal) }
	output, err := builtin.Resolve(4003, builtin.ResolveOptions{})
	if err != nil { t.Fatal(err) }
	text, err := output.Call(t.Context(), []model.Value{input}, jsonPathBudget(1<<20))
	if err != nil { t.Fatal(err) }
	const want = `strict $."a"[0, last - 1].**{2 to 5}?(@ like_regex "^x" flag "im")`
	if text.StrVal != want { t.Fatalf("jsonpath output = %q; want %q", text.StrVal, want) }
	received, err := builtin.ReceiveJSONPath(t.Context(), input.BytesVal, jsonPathBudget(1<<20))
	if err != nil || !reflect.DeepEqual(received.BytesVal, input.BytesVal) { t.Fatalf("binary roundtrip = %x, %v", received.BytesVal, err) }
	bad := append([]byte(nil), input.BytesVal...); bad[0] = 2
	_, err = builtin.ReceiveJSONPath(t.Context(), bad, jsonPathBudget(1<<20))
	if err == nil { t.Fatal("unsupported binary version was accepted") }
}

func TestJSONPathTraversalPredicatesVariablesAndArithmetic(t *testing.T) {
	const document = `{"a":[{"n":1,"s":"Abc"},{"n":2,"s":"axis"},{"n":3,"s":null}],"deep":{"a":{"n":4}}}`
	cases := []struct{ name, path, variables, want string }{
		{"filter_and_variable", `$.a[*]?(@.n >= $min).n`, `{"min":2}`, `[2, 3]`},
		{"range_and_last", `$.a[1 to last].n`, `{}`, `[2, 3]`},
		{"recursive_descent", `$.**.n`, `{}`, `[1, 2, 3, 1, 2, 3, 4]`},
		{"three_valued_unknown", `($.missing == 1) is unknown`, `{}`, `[true]`},
		{"arithmetic", `($.a[2].n * 4 - 2) / 2`, `{}`, `[5.0000000000000000]`},
		{"like_regex", `$.a[*]?(@.s like_regex "^a" flag "i").n`, `{}`, `[1, 2]`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			value := callJSONPath(t, 4007, document, test.path, test.variables, false)
			if value.StrVal != test.want { t.Fatalf("result = %s; want %s", value.StrVal, test.want) }
		})
	}
}

func TestJSONPathStrictLaxAndSilentErrorBoundaries(t *testing.T) {
	lax := callJSONPath(t, 4007, `{"a":[{"n":1}]}`, `$.a.n`, `{}`, false)
	if lax.StrVal != `[1]` { t.Fatalf("lax unwrapping = %s", lax.StrVal) }
	call, err := builtin.Resolve(4007, builtin.ResolveOptions{})
	if err != nil { t.Fatal(err) }
	arguments := []model.Value{{Type: model.TypeJSONB, StrVal: `{"a":1}`}, jsonPathValue(t, `strict $.missing`), {Type: model.TypeJSONB, StrVal: `{}`}, model.BoolVal(false)}
	_, err = call.Call(t.Context(), arguments, jsonPathBudget(1<<20))
	var sql *model.Error
	if !errors.As(err, &sql) || sql.Code != "2203A" { t.Fatalf("strict missing member error = %v", err) }
	arguments[3] = model.BoolVal(true)
	suppressed, err := call.Call(t.Context(), arguments, jsonPathBudget(1<<20))
	if err != nil || suppressed.StrVal != `[]` { t.Fatalf("silent result = %v, %v", suppressed, err) }
}

func TestJSONPathMethodsAndKeyValueIdentifiers(t *testing.T) {
	cases := []struct{ document, path, want string }{
		{`[-2.7,2.1]`, `$[*].abs().floor()`, `[2, 2]`},
		{`["42.5","on"]`, `$[0].decimal(4,1)`, `[42.5]`},
		{`["42.5","on"]`, `$[1].boolean()`, `[true]`},
		{`{"a":1,"b":[2]}`, `$.keyvalue()`, `[{"id": 0, "key": "a", "value": 1}, {"id": 0, "key": "b", "value": [2]}]`},
		{`"10-03-2017 12:34"`, `$.datetime("dd-mm-yyyy HH24:MI").type()`, `["timestamp without time zone"]`},
	}
	for _, test := range cases {
		value := callJSONPath(t, 4007, test.document, test.path, `{}`, false)
		if value.StrVal != test.want { t.Errorf("%s on %s = %s; want %s", test.path, test.document, value.StrVal, test.want) }
	}
}

func TestJSONPathRowsStreamsAndCloses(t *testing.T) {
	jsonb := model.TypeLayout{Type: model.Type{ID: 3802, Modifier: -1, Storage: model.TypeJSONB}}
	path := model.TypeLayout{Type: model.Type{ID: 4072, Modifier: -1, Storage: model.TypeBytes}}
	boolean := model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}
	plan, err := builtin.ResolveJSONPathRows(t.Context(), 4006, []model.TypeLayout{jsonb, path, jsonb, boolean}, jsonb, builtin.ResolveOptions{}, 1<<20)
	if err != nil { t.Fatal(err) }
	arguments := []model.Value{{Type: model.TypeJSONB, StrVal: `[1,2,3]`}, jsonPathValue(t, `$[*]?(@ > 1)`), {Type: model.TypeJSONB, StrVal: `{}`}, model.BoolVal(false)}
	iterator, err := plan.Open(t.Context(), arguments, builtin.RowSourceOptions{MaxInputBytes: 1 << 20, MaxRowBytes: 1 << 20, MaxMemoryBytes: jsonPathTestMemory})
	if err != nil { t.Fatal(err) }
	defer func() { if err := iterator.Close(); err != nil { t.Error(err) } }()
	var got []string
	for {
		row, ok, err := iterator.Next(t.Context())
		if err != nil { t.Fatal(err) }
		if !ok { break }
		got = append(got, row[0].StrVal)
	}
	if !reflect.DeepEqual(got, []string{"2", "3"}) { t.Fatalf("streamed rows = %v", got) }
}

func TestJSONPathRowsHonorsPreCanceledContext(t *testing.T) {
	jsonb := model.TypeLayout{Type: model.Type{ID: 3802, Modifier: -1, Storage: model.TypeJSONB}}
	path := model.TypeLayout{Type: model.Type{ID: 4072, Modifier: -1, Storage: model.TypeBytes}}
	boolean := model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}
	plan, err := builtin.ResolveJSONPathRows(t.Context(), 4006, []model.TypeLayout{jsonb, path, jsonb, boolean}, jsonb, builtin.ResolveOptions{}, 1<<20)
	if err != nil { t.Fatal(err) }
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = plan.Open(ctx, []model.Value{{Type: model.TypeJSONB, StrVal: `[]`}, jsonPathValue(t, `$[*]`), {Type: model.TypeJSONB, StrVal: `{}`}, model.BoolVal(false)}, builtin.RowSourceOptions{MaxInputBytes: 1 << 20, MaxRowBytes: 1 << 20, MaxMemoryBytes: jsonPathTestMemory})
	if !errors.Is(err, context.Canceled) { t.Fatalf("pre-canceled open error = %v", err) }
}

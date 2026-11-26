package builtin_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func sqlJSONLayout(id model.TypeID, storage model.DataType) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: id, Modifier: -1, Storage: storage}}
}

func sqlJSONStatusLayout(target model.TypeLayout) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, Children: []model.TypeLayout{sqlJSONLayout(23, model.TypeInt64), target}}
}

func sqlJSONTestSpec(kind builtin.SQLJSONKind) builtin.SQLJSONPlanSpec {
	return builtin.SQLJSONPlanSpec{
		Kind: kind, InputFormat: builtin.SQLJSONDefaultInput, InputEncoding: builtin.SQLJSONDefaultEncoding,
		OutputFormat: builtin.SQLJSONDefaultInput, OutputEncoding: builtin.SQLJSONDefaultEncoding,
		Wrapper: builtin.SQLJSONUnspecifiedWrapper, Quotes: builtin.SQLJSONUnspecifiedQuotes,
		ItemType: builtin.SQLJSONAnyItem, UseTZ: true,
	}
}

func sqlJSONTableTestColumn(kind builtin.JSONTableColumnKind) builtin.JSONTableColumnSpec {
	return builtin.JSONTableColumnSpec{
		Kind: kind, OutputFormat: builtin.SQLJSONDefaultInput, OutputEncoding: builtin.SQLJSONDefaultEncoding,
		Wrapper: builtin.SQLJSONUnspecifiedWrapper, Quotes: builtin.SQLJSONUnspecifiedQuotes,
	}
}

func TestSQLJSONQueryScalarQuoteBehavior(t *testing.T) {
	jsonb := sqlJSONLayout(3802, model.TypeJSONB)
	path := sqlJSONLayout(4072, model.TypeBytes)
	text := sqlJSONLayout(25, model.TypeString)
	result := sqlJSONStatusLayout(text)
	for _, test := range []struct {
		name string
		document string
		quotes builtin.SQLJSONQuotes
		want string
	}{
		{"numeric", `12.3`, builtin.SQLJSONUnspecifiedQuotes, `12.3`},
		{"keep string quotes", `"aaa"`, builtin.SQLJSONKeepQuotes, `"aaa"`},
		{"omit string quotes", `"aaa"`, builtin.SQLJSONOmitQuotes, `aaa`},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := sqlJSONTestSpec(builtin.SQLJSONQueryKind)
			spec.Quotes = test.quotes
			plan, err := builtin.ResolveSQLJSON(t.Context(), spec, []model.TypeLayout{jsonb, path}, result, builtin.ResolveOptions{}, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			value, err := plan.Call(t.Context(), []model.Value{{Type: model.TypeJSONB, StrVal: test.document}, jsonPathValue(t, `$`)}, jsonPathBudget(1<<20))
			if err != nil || value.IsNull || len(value.ArrayVal) != 2 || value.ArrayVal[0].IntVal != 0 || value.ArrayVal[1].StrVal != test.want {
				t.Fatalf("query result=%v error=%v; want %q", value, err, test.want)
			}
		})
	}
}

func TestSQLJSONQueryDecodesArrayReturningType(t *testing.T) {
	jsonb := sqlJSONLayout(3802, model.TypeJSONB)
	path := sqlJSONLayout(4072, model.TypeBytes)
	integer := sqlJSONLayout(23, model.TypeInt64)
	array := model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	plan, err := builtin.ResolveSQLJSON(t.Context(), sqlJSONTestSpec(builtin.SQLJSONQueryKind), []model.TypeLayout{jsonb, path}, sqlJSONStatusLayout(array), builtin.ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	value, err := plan.Call(t.Context(), []model.Value{{Type: model.TypeJSONB, StrVal: `[1,2]`}, jsonPathValue(t, `$`)}, jsonPathBudget(1<<20))
	if err != nil || value.ArrayVal[0].IntVal != 0 {
		t.Fatalf("array query result=%v error=%v", value, err)
	}
	arrayValue := value.ArrayVal[1]
	if len(arrayValue.Dimensions) != 1 || arrayValue.Dimensions[0].LowerBound != 1 || arrayValue.Dimensions[0].Length != 2 ||
		len(arrayValue.ArrayVal) != 2 || arrayValue.ArrayVal[0].IntVal != 1 || arrayValue.ArrayVal[1].IntVal != 2 {
		t.Fatalf("array query value=%v", arrayValue)
	}
}

func TestSQLJSONTypedPassingUsesTimezoneEnabledComparison(t *testing.T) {
	jsonb := sqlJSONLayout(3802, model.TypeJSONB)
	path := sqlJSONLayout(4072, model.TypeBytes)
	text := sqlJSONLayout(25, model.TypeString)
	integer := sqlJSONLayout(23, model.TypeInt64)
	date := sqlJSONLayout(1082, model.TypeTimestamp)
	timestamptz := sqlJSONLayout(1184, model.TypeTimestamp)
	boolean := sqlJSONLayout(16, model.TypeBoolean)
	arguments := []model.TypeLayout{jsonb, path, text, integer, integer, date, text, integer, integer, timestamptz}
	values := []model.Value{
		{Type: model.TypeJSONB, StrVal: `null`}, jsonPathValue(t, `$d == $z`),
		model.StringVal("d"), model.Int64Val(int64(builtin.SQLJSONDefaultInput)), model.Int64Val(int64(builtin.SQLJSONDefaultEncoding)), {Type: model.TypeTimestamp, IntVal: 0},
		model.StringVal("z"), model.Int64Val(int64(builtin.SQLJSONDefaultInput)), model.Int64Val(int64(builtin.SQLJSONDefaultEncoding)), {Type: model.TypeTimestamp, IntVal: 0},
	}
	options := builtin.ResolveOptions{TimeZone: builtin.TimeZoneContext{Name: "UTC", Fixed: true}}
	for _, useTZ := range []bool{false, true} {
		spec := sqlJSONTestSpec(builtin.SQLJSONExistsKind)
		spec.Passing, spec.UseTZ = 2, useTZ
		plan, err := builtin.ResolveSQLJSON(t.Context(), spec, arguments, sqlJSONStatusLayout(boolean), options, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		value, err := plan.Call(t.Context(), values, jsonPathBudget(1<<20))
		if !useTZ {
			var sql *model.Error
			if !errors.As(err, &sql) || sql.Code != "0A000" {
				t.Fatalf("timezone-disabled error=%v", err)
			}
			continue
		}
		if err != nil || value.ArrayVal[0].IntVal != 0 || value.ArrayVal[1].IntVal != 1 {
			t.Fatalf("timezone-enabled result=%v error=%v", value, err)
		}
	}
}

func TestJSONTableNestedRowsCancellationAndResourceErrors(t *testing.T) {
	jsonb := sqlJSONLayout(3802, model.TypeJSONB)
	integer := sqlJSONLayout(23, model.TypeInt64)
	status := sqlJSONStatusLayout(integer)
	result := model.TypeLayout{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, Children: []model.TypeLayout{integer, status}}
	tableSpec := builtin.JSONTableSpec{
		Root: jsonPathValue(t, `$[*]`).BytesVal,
		InputFormat: builtin.SQLJSONDefaultInput, InputEncoding: builtin.SQLJSONDefaultEncoding,
		EmptyOnError: true, UseTZ: true,
		Columns: []builtin.JSONTableColumnSpec{
			sqlJSONTableTestColumn(builtin.JSONTableOrdinality),
			sqlJSONTableTestColumn(builtin.JSONTableNested),
			sqlJSONTableTestColumn(builtin.JSONTableValue),
		},
	}
	tableSpec.Columns[0].Output = 1
	tableSpec.Columns[1].Path = jsonPathValue(t, `$.a[*]`).BytesVal
	tableSpec.Columns[2].Parent, tableSpec.Columns[2].Output = 2, 2
	tableSpec.Columns[2].Path = jsonPathValue(t, `$`).BytesVal
	tableSpec.Columns[2].CaptureEmpty, tableSpec.Columns[2].CaptureError = true, true
	plan, err := builtin.ResolveJSONTable(t.Context(), tableSpec, []model.TypeLayout{jsonb}, result, builtin.ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	options := builtin.RowSourceOptions{MaxInputBytes: 1 << 20, MaxRowBytes: 1 << 20, MaxMemoryBytes: 1 << 20}
	iterator, err := plan.Open(t.Context(), []model.Value{{Type: model.TypeJSONB, StrVal: `[{"a":[10,11]},{"a":[]},{"a":[20]}]`}}, options)
	if err != nil {
		t.Fatal(err)
	}
	var rows [][2]int64
	for {
		row, ok, err := iterator.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		value := int64(-1)
		if !row[1].IsNull {
			value = row[1].ArrayVal[1].IntVal
		}
		rows = append(rows, [2]int64{row[0].IntVal, value})
	}
	if err := iterator.Close(); err != nil {
		t.Fatal(err)
	}
	if want := [][2]int64{{1, 10}, {1, 11}, {2, -1}, {3, 20}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows=%v; want %v", rows, want)
	}

	iterator, err = plan.Open(t.Context(), []model.Value{{Type: model.TypeJSONB, StrVal: `[{"a":[1]}]`}}, options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = iterator.Next(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled next error=%v", err)
	}
	_ = iterator.Close()

	_, err = plan.Open(t.Context(), []model.Value{{Type: model.TypeJSONB, StrVal: `[{"a":[1]}]`}}, builtin.RowSourceOptions{MaxInputBytes: 1 << 20, MaxRowBytes: 1 << 20, MaxMemoryBytes: 1})
	var sql *model.Error
	if !errors.As(err, &sql) || sql.Code != "54000" {
		t.Fatalf("resource error=%v", err)
	}
}

package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLSpecializedIntegerArgumentsDoNotUseArithmeticVectorKernels(t *testing.T) {
	graph := sqlGraph(t, `SELECT money(x), factorial(x::bigint), encode(int4send(x), 'hex') FROM (VALUES (3), (4)) AS v(x)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{790, 1700, 25}, [][]model.Value{
		{model.Int64Val(300), {Type: model.TypeNumeric, StrVal: "6"}, model.StringVal("00000003")},
		{model.Int64Val(400), {Type: model.TypeNumeric, StrVal: "24"}, model.StringVal("00000004")},
	})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLNegativeInt4LiteralsPreserveOIDBitPatterns(t *testing.T) {
	graph := sqlGraph(t, `SELECT x::oid, x::oid::integer, (ARRAY[x]::oid[])::text, (ARRAY[x]::oid[]::integer[])::text FROM (VALUES (-1), (-2147483648)) AS v(x)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{26, 23, 25, 25}, [][]model.Value{
		{model.Int64Val(4294967295), model.Int64Val(-1), model.StringVal("{4294967295}"), model.StringVal("{-1}")},
		{model.Int64Val(2147483648), model.Int64Val(-2147483648), model.StringVal("{2147483648}"), model.StringVal("{-2147483648}")},
	})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLNumericInfinityBinaryHeaders(t *testing.T) {
	graph := sqlGraph(t, `SELECT encode(numeric_send(x), 'hex') FROM (VALUES ('Infinity'::numeric), ('-Infinity'::numeric), ('NaN'::numeric)) AS v(x)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{
		{model.StringVal("00000000d0000020")},
		{model.StringVal("00000000f0000020")},
		{model.StringVal("00000000c0000000")},
	})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLJSONBConstructorsKeepWholeRowNamesAcrossMaterialization(t *testing.T) {
	graph := sqlGraph(t, `WITH c AS MATERIALIZED (SELECT t AS r FROM (VALUES (1,'a')) AS t(x,y)) SELECT jsonb_build_array(), jsonb_build_object(), jsonb_build_array(r, ARRAY[r,r]) FROM c`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{3802, 3802, 3802}, [][]model.Value{{
		{Type: model.TypeJSONB, StrVal: "[]"},
		{Type: model.TypeJSONB, StrVal: "{}"},
		{Type: model.TypeJSONB, StrVal: `[{"x": 1, "y": "a"}, [{"x": 1, "y": "a"}, {"x": 1, "y": "a"}]]`},
	}})
	aggregateProjectionReleased(t, env.Query)
}

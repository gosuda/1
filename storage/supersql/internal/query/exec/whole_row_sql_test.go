package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func TestSQLWholeRowFieldNamesSurviveDerivedProjection(t *testing.T) {
	graph := sqlGraph(t, `SELECT (s.r).a,(s.r).b FROM (SELECT v AS r FROM (VALUES(1,2))v(a,b))s`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23}, [][]model.Value{{model.Int64Val(1), model.Int64Val(2)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLWholeRowColumnNameTakesPrecedence(t *testing.T) {
	graph := sqlGraph(t, `SELECT v FROM (VALUES(7))v(v)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23}, [][]model.Value{{model.Int64Val(7)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLWholeRowOuterJoinDistinguishesAbsentFromAllNull(t *testing.T) {
	graph := sqlGraph(t, `SELECT l.n,num_nonnulls(r),num_nulls(r),r::text FROM (VALUES(1),(2))l(n) LEFT JOIN LATERAL (SELECT NULL::integer AS x WHERE l.n=1)r ON true ORDER BY l.n`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 23, 25}, [][]model.Value{{i(1), i(1), i(0), model.StringVal("()")}, {i(2), i(0), i(1), model.NullVal(model.TypeString)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLWholeRowCapturesOuterRelation(t *testing.T) {
	graph := sqlGraph(t, `SELECT v.a,s.r::text FROM (VALUES(1),(2))v(a) CROSS JOIN LATERAL (SELECT v AS r)s ORDER BY v.a`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 25}, [][]model.Value{{model.Int64Val(1), model.StringVal("(1)")}, {model.Int64Val(2), model.StringVal("(2)")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLWholeRowZeroColumnFunctionRetainsRows(t *testing.T) {
	record := model.TypeLayout{Type: model.Type{ID: 90000, Storage: model.TypeComposite, Modifier: -1}}
	array := model.TypeLayout{Type: model.Type{ID: 90001, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{record}}
	snapshot := make(lateralReplayCatalog, 2)
	for i, spec := range []model.DescriptorSpec{
		{ID: 10, OID: 10010, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "public"}, Schema: &model.SchemaMetadata{Owner: 10}},
		{ID: 30, OID: 90000, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "empty_record"}, Type: &model.TypeMetadata{Kind: model.TypeKindComposite, Layout: record, Array: 31}},
	} {
		var err error
		snapshot[i], err = model.NewDescriptor(t.Context(), spec, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20})
		if err != nil {
			t.Fatal(err)
		}
	}
	graph := sqlGraphWithCatalog(t, `SELECT num_nonnulls(u),u::text FROM unnest($1)u`, snapshot, []model.TypeLayout{array})
	env := statefulEnvironment(t)
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: array, Value: model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}}, ArrayVal: []model.Value{{Type: model.TypeComposite}, model.NullVal(model.TypeComposite)}}}}
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 25}, [][]model.Value{{model.Int64Val(1), model.StringVal("()")}, {model.Int64Val(1), model.StringVal("()")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLWholeRowNestedLateralNullExtension(t *testing.T) {
	graph := sqlGraph(t, `SELECT l.n,num_nonnulls(j.r),num_nonnulls(j),j::text FROM (VALUES(1),(2),(3))l(n) LEFT JOIN LATERAL (SELECT r FROM (VALUES(NULL::integer))r(x) WHERE l.n<3)j ON l.n=1 ORDER BY l.n`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	n := model.NullVal(model.TypeString)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 23, 25}, [][]model.Value{{i(1), i(1), i(1), model.StringVal(`("()")`)}, {i(2), i(0), i(0), n}, {i(3), i(0), i(0), n}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLWholeRowFullJoinNullsEachAbsentSide(t *testing.T) {
	graph := sqlGraph(t, `SELECT l.n,num_nonnulls(l),num_nonnulls(r) FROM (VALUES(1),(2))l(n) FULL JOIN (VALUES(1),(3))r(n) ON l.n=r.n ORDER BY l.n NULLS LAST,r.n`, nil)
	env := setWindowEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 23}, [][]model.Value{{i(1), i(1), i(1)}, {i(2), i(1), i(0)}, {model.NullVal(model.TypeInt64), i(0), i(1)}})
	aggregateProjectionReleased(t, env.Query)
}

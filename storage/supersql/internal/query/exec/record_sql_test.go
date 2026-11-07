package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLRecordOutputFeedsTextQuoteFormatAndArrays(t *testing.T) {
	graph := sqlGraph(t, `SELECT ROW(1,'x')::text,quote_literal(ROW(1,'x')),format('%s',ROW(1,'x')),array_to_string(ARRAY[ROW(1,'x'),NULL::record,ROW(2,'y')],';','null')`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 25}, [][]model.Value{{s("(1,x)"), s("'(1,x)'"), s("(1,x)"), s("(1,x);null;(2,y)")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRecordAndRowNullComparisonsRemainDistinct(t *testing.T) {
	graph := sqlGraph(t, `SELECT record_eq(ROW(1,NULL::text),ROW(1,NULL::text)),ROW(1,NULL::text)=ROW(1,NULL::text),record_gt(ROW(1,NULL::text),ROW(1,'a'::text)),btrecordcmp(ROW(2,1),ROW(1,'x'::text))`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16, 16, 16, 23}, [][]model.Value{{model.BoolVal(true), model.NullVal(model.TypeBoolean), model.BoolVal(true), model.Int64Val(1)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRecordComparatorsFeedArraySearchAndBuckets(t *testing.T) {
	graph := sqlGraph(t, `SELECT array_position(ARRAY[ROW(1,NULL::text),ROW(2,'x'::text)],ROW(1,NULL::text)),width_bucket(ROW(2,'y'::text),ARRAY[ROW(1,'x'::text),ROW(2,'y'::text),ROW(2,'z'::text)])`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23}, [][]model.Value{{model.Int64Val(1), model.Int64Val(2)}})
	aggregateProjectionReleased(t, env.Query)
}

func recordSQLCatalog(t *testing.T) lateralReplayCatalog {
	t.Helper()
	integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	label := model.TypeLayout{Type: model.Type{ID: 1043, Storage: model.TypeString, Modifier: 7, Collation: 950}}
	integers := model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{integer}}
	record := model.TypeLayout{Type: model.Type{ID: 90000, Storage: model.TypeComposite, Modifier: -1}, Children: []model.TypeLayout{label, integers}}
	array := model.TypeLayout{Type: model.Type{ID: 90001, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{record}}
	specs := []model.DescriptorSpec{
		{ID: 10, OID: 10010, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "public"}, Schema: &model.SchemaMetadata{Owner: 10}},
		{ID: 30, OID: 90000, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "pair"}, Type: &model.TypeMetadata{Kind: model.TypeKindComposite, Layout: record, Array: 31, Fields: []model.ColumnDescriptor{{ID: 1, Name: "label", State: model.StatePublic, Layout: label}, {ID: 2, Name: "numbers", State: model.StatePublic, Layout: integers}}}},
		{ID: 31, OID: 90001, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "_pair"}, Type: &model.TypeMetadata{Kind: model.TypeKindArray, Layout: array, Base: 30, BaseLayout: record}},
	}
	snapshot := make(lateralReplayCatalog, len(specs))
	for i, spec := range specs {
		var err error
		snapshot[i], err = model.NewDescriptor(t.Context(), spec, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20})
		if err != nil {
			t.Fatal(err)
		}
	}
	return snapshot
}

func TestSQLNamedRecordLiteralsCastsAndArrayInput(t *testing.T) {
	graph := sqlGraphWithCatalog(t, `SELECT '(abc,"{1,2}")'::pair::text,(ROW('abcdef',ARRAY[3::smallint,4::smallint])::pair)::text,('{"(abc,)","(,)",NULL}'::pair[])[1]::text`, recordSQLCatalog(t), nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s := model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25}, [][]model.Value{{s(`(abc,"{1,2}")`), s(`(abc,"{3,4}")`), s("(abc,)")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRowEqualityConstantFalseSkipsLaterDivision(t *testing.T) {
	graph := sqlGraph(t, `SELECT ROW(1,1/0)=ROW(2,3)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{16}, [][]model.Value{{model.BoolVal(false)}})
	aggregateProjectionReleased(t, env.Query)
}

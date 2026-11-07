package exec_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func typmodNumericLayout(modifier int32) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: 1700, Modifier: modifier, Storage: model.TypeNumeric}}
}

func typmodCharacterLayout(oid model.TypeID, modifier int32, collation model.CollationID) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: oid, Modifier: modifier, Collation: collation, Storage: model.TypeString}}
}

func typmodNumericValue(value string) model.Value {
	return model.Value{Type: model.TypeNumeric, StrVal: value}
}

func TestSQLNumericTypmodRounding(t *testing.T) {
	n := typmodNumericValue
	for _, scenario := range []struct {
		name, sql string
		modifier  int32
		want      [][]model.Value
	}{
		{"half away from zero", `SELECT x::numeric(4,2) FROM (VALUES(1.235::numeric),((-1.235)::numeric),(0.004::numeric),(NULL::numeric)) v(x)`, 262150, [][]model.Value{{n("1.24")}, {n("-1.24")}, {n("0.00")}, {model.NullVal(model.TypeNumeric)}}},
		{"single modifier defaults to zero scale", `SELECT 12.5::numeric(3)`, 196612, [][]model.Value{{n("13")}}},
		{"negative scale", `SELECT x::numeric(2,-3) FROM (VALUES(14500::numeric),((-14500)::numeric),(99499::numeric)) v(x)`, 133121, [][]model.Value{{n("15000")}, {n("-15000")}, {n("99000")}}},
		{"scale exceeds precision", `SELECT x::numeric(2,4) FROM (VALUES(0.00125::numeric),((-0.00125)::numeric),(0.00994::numeric)) v(x)`, 131080, [][]model.Value{{n("0.0013")}, {n("-0.0013")}, {n("0.0099")}}},
		{"narrowing existing numeric typmod", `SELECT (12.345::numeric(5,3))::numeric(4,2)`, 262150, [][]model.Value{{n("12.35")}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			assertCollationSQL(t, scenario.sql, []model.TypeLayout{typmodNumericLayout(scenario.modifier)}, scenario.want)
		})
	}
}

func TestSQLNumericTypmodOverflowAfterRounding(t *testing.T) {
	for _, scenario := range []struct{ name, sql string }{
		{"positive scale carry", `SELECT x::numeric(4,2) FROM (VALUES(99.995::numeric)) v(x)`},
		{"negative scale carry", `SELECT x::numeric(2,-3) FROM (VALUES(99500::numeric)) v(x)`},
		{"fractional precision carry", `SELECT x::numeric(2,4) FROM (VALUES(0.00995::numeric)) v(x)`},
		{"filter activates overflow", `SELECT sum(x::numeric(3,1)) FILTER(WHERE active) FROM (VALUES(false,'bad'::text),(true,'999')) v(active,x)`},
		{"limit includes overflow", `SELECT x::numeric(3,1) FROM (VALUES('1.25'::text),('999')) v(x) LIMIT 2`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env, dir := groupingSetsEnvironment(t, false)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertArrayAggregateFailure(t, portal, "22003")
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLCharacterTypmodCountsCharacters(t *testing.T) {
	s := model.StringVal
	assertCollationSQL(t, `SELECT 'é界🙂z'::varchar(3), 'é界🙂z'::char(2), 'é'::char(3),
		''::varchar(2), ''::char(2), NULL::varchar(2), NULL::char(2),
		('é界🙂z'::text)::varchar(2), ('é界🙂z'::text)::char(2)`,
		[]model.TypeLayout{
			typmodCharacterLayout(1043, 7, 100), typmodCharacterLayout(1042, 6, 100), typmodCharacterLayout(1042, 7, 100),
			typmodCharacterLayout(1043, 6, 100), typmodCharacterLayout(1042, 6, 100), typmodCharacterLayout(1043, 6, 100), typmodCharacterLayout(1042, 6, 100),
			typmodCharacterLayout(1043, 6, 100), typmodCharacterLayout(1042, 6, 100),
		},
		[][]model.Value{{s("é界🙂"), s("é界"), s("é  "), s(""), s("  "), model.NullVal(model.TypeString), model.NullVal(model.TypeString), s("é界"), s("é界")}})
}

func TestSQLCharacterTypmodCastChains(t *testing.T) {
	s := model.StringVal
	boolean := model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}
	assertCollationSQL(t, `SELECT true::char(3), false::varchar(4), true::text::char(6),
		'yes'::varchar(3)::boolean, 'f'::char(4)::text::boolean,
		'é'::char(4)::text, 'é '::varchar(4)::text,
		('abcdef'::varchar(5))::varchar(2), ('é'::char(2))::char(4)`,
		[]model.TypeLayout{
			typmodCharacterLayout(1042, 7, 100), typmodCharacterLayout(1043, 8, 100), typmodCharacterLayout(1042, 10, 100),
			boolean, boolean, collationTextLayout(100), collationTextLayout(100),
			typmodCharacterLayout(1043, 6, 100), typmodCharacterLayout(1042, 8, 100),
		}, [][]model.Value{{s("tru"), s("fals"), s("true  "), model.BoolVal(true), model.BoolVal(false), s("é"), s("é "), s("ab"), s("é   ")}})
}

func TestSQLCharacterTypmodTrailingSpaceComparisons(t *testing.T) {
	boolean := model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}
	assertCollationSQL(t, `SELECT 'a'::char(1)='a'::char(4), 'a'::char(4)<>'a '::char(2),
		'a'::char(4)<'b'::char(1), 'a'::char(4)<='a'::char(1),
		'b'::char(4)>'a'::char(1), 'a'::char(1)>='a'::char(4),
		'a'::text='a '::text, 'a'::varchar(4)='a '::varchar(4),
		'a'::char(4)::text='a'::text, NULL::char(2)='a'::char(1)`,
		[]model.TypeLayout{boolean, boolean, boolean, boolean, boolean, boolean, boolean, boolean, boolean, boolean},
		[][]model.Value{{model.BoolVal(true), model.BoolVal(false), model.BoolVal(true), model.BoolVal(true), model.BoolVal(true), model.BoolVal(true), model.BoolVal(false), model.BoolVal(false), model.BoolVal(true), model.NullVal(model.TypeBoolean)}})
}

func TestSQLTypmodCommonResultsDoNotNarrowOtherBranches(t *testing.T) {
	s, n := model.StringVal, typmodNumericValue
	assertCollationSQL(t, `SELECT CASE WHEN choose THEN a::varchar(1) ELSE b::varchar(4) END,
		coalesce(c::varchar(1),b::varchar(4)),
		CASE WHEN choose THEN x::numeric(3,1) ELSE y::numeric(5,2) END,
		coalesce(z::numeric(3,1),y::numeric(5,2))
		FROM (VALUES(true,'abc'::text,'wxyz'::text,NULL::text,1.25::numeric,123.456::numeric,NULL::numeric),
		(false,'abc','wxyz','q',1.25,123.456,2.25)) v(choose,a,b,c,x,y,z)`,
		[]model.TypeLayout{typmodCharacterLayout(1043, -1, 100), typmodCharacterLayout(1043, -1, 100), typmodNumericLayout(-1), typmodNumericLayout(-1)},
		[][]model.Value{{s("a"), s("wxyz"), n("1.3"), n("123.46")}, {s("wxyz"), s("q"), n("123.46"), n("2.3")}})
}

func TestSQLTypmodMasksInactiveConversions(t *testing.T) {
	n := typmodNumericValue
	for _, scenario := range []struct {
		name, sql string
		modifier  int32
		want      [][]model.Value
	}{
		{"case", `SELECT CASE WHEN active THEN x::numeric(3,1) ELSE 0::numeric(3,1) END FROM (VALUES(false,'bad'::text),(false,'999'),(true,'1.25'),(true,NULL)) v(active,x)`, 196613, [][]model.Value{{n("0.0")}, {n("0.0")}, {n("1.3")}, {model.NullVal(model.TypeNumeric)}}},
		{"coalesce", `SELECT coalesce(x::numeric(3,1),y::numeric(3,1)) FROM (VALUES('1.25'::text,'bad'::text),('2.25','999'),(NULL,'3.25'),(NULL,NULL)) v(x,y)`, 196613, [][]model.Value{{n("1.3")}, {n("2.3")}, {n("3.3")}, {model.NullVal(model.TypeNumeric)}}},
		{"limit", `SELECT x::numeric(3,1) FROM (VALUES('1.25'::text),('999'),('bad')) v(x) LIMIT 1`, 196613, [][]model.Value{{n("1.3")}}},
		{"filter", `SELECT sum(x::numeric(3,1)) FILTER(WHERE active) FROM (VALUES(false,'bad'::text),(false,'999'),(true,'1.25'),(true,'2.25'),(true,NULL)) v(active,x)`, -1, [][]model.Value{{n("3.6")}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			assertCollationSQL(t, scenario.sql, []model.TypeLayout{typmodNumericLayout(scenario.modifier)}, scenario.want)
		})
	}
}

func TestSQLTypmodLaterActiveErrorsReleaseResources(t *testing.T) {
	for _, scenario := range []struct{ name, sql, code string }{
		{"case overflow", `SELECT CASE WHEN active THEN x::numeric(3,1) ELSE 0::numeric(3,1) END FROM (VALUES(false,'bad'::text),(true,'999')) v(active,x)`, "22003"},
		{"coalesce overflow", `SELECT coalesce(x::numeric(3,1),y::numeric(3,1)) FROM (VALUES('0'::text,'bad'::text),(NULL,'999')) v(x,y)`, "22003"},
		{"character boolean input", `SELECT CASE WHEN active THEN x::varchar(4)::boolean ELSE false END FROM (VALUES(false,'bad'::text),(true,'bad')) v(active,x)`, "22P02"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env, dir := groupingSetsEnvironment(t, false)
			env.Limits.BatchRows = 1
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatalf("Open evaluated an inactive conversion: %v", err)
			}
			defer closePortal(t, portal)
			var batch columnar.Batch
			if err := portal.Next(t.Context(), &batch); err != nil {
				t.Fatal(err)
			}
			layout, value := typmodNumericLayout(196613), typmodNumericValue("0.0")
			if scenario.code == "22P02" {
				layout = model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}
				value = model.BoolVal(false)
			}
			assertArrayAggregateBatch(t, &batch, []model.TypeLayout{layout}, [][]model.Value{{value}})
			assertArrayAggregateFailure(t, portal, scenario.code)
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLTypmodParametersKeepIndependentModifiersAndOwnInput(t *testing.T) {
	numeric, text := typmodNumericLayout(-1), collationTextLayout(100)
	binary := model.TypeLayout{Type: model.Type{ID: 17, Modifier: -1, Storage: model.TypeBytes}}
	graph := sqlGraph(t, `SELECT $1::numeric(4,1),$1::numeric(5,2),$1,
		$2::varchar(1),$2::char(4),$2,$3::text::varchar(4) FROM (VALUES(1),(2)) v(id)`, []model.TypeLayout{numeric, text, binary})
	env, dir := groupingSetsEnvironment(t, false)
	env.Limits.BatchRows = 1
	payload := []byte{0xab, 0xcd}
	env.Parameters = []expr.ParameterValue{
		{Number: 1, Layout: numeric, Value: typmodNumericValue("12.345")},
		{Number: 2, Layout: text, Value: model.StringVal("é界x")},
		{Number: 3, Layout: binary, Value: model.BytesVal(payload)},
	}
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	env.Parameters[0].Value = typmodNumericValue("99999")
	env.Parameters[1].Value = model.StringVal("changed")
	payload[0] = 0xff
	layouts := []model.TypeLayout{typmodNumericLayout(262149), typmodNumericLayout(327686), numeric,
		typmodCharacterLayout(1043, 5, 100), typmodCharacterLayout(1042, 8, 100), text, typmodCharacterLayout(1043, 8, 100)}
	want := []model.Value{typmodNumericValue("12.3"), typmodNumericValue("12.35"), typmodNumericValue("12.345"), model.StringVal("é"), model.StringVal("é界x "), model.StringVal("é界x"), model.StringVal(`\xab`)}
	assertArrayAggregateRows(t, portal, layouts, [][]model.Value{want, want})
	groupingSetsReleased(t, env, dir)
}

func TestSQLCharacterTypmodPreservesCastCollation(t *testing.T) {
	assertCollationSQL(t, `SELECT (('é界x' COLLATE "POSIX")::varchar(2))::char(4),
		(('é界x' COLLATE "POSIX")::char(2))::text,
		(('a' COLLATE "POSIX")::varchar(1)::text)||b FROM (VALUES('b' COLLATE "C")) v(b)`,
		[]model.TypeLayout{typmodCharacterLayout(1042, 8, 951), collationTextLayout(951), collationTextLayout(951)},
		[][]model.Value{{model.StringVal("é界  "), model.StringVal("é界"), model.StringVal("ab")}})
}

func TestSQLTypmodInferredParametersRemainUnconstrained(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
		input     model.TypeLayout
		value     model.Value
		layouts   []model.TypeLayout
		want      []model.Value
	}{
		{"numeric", `SELECT $1::numeric(3,1),$1::numeric(5,2),$1`, typmodNumericLayout(-1), typmodNumericValue("12.345"),
			[]model.TypeLayout{typmodNumericLayout(196613), typmodNumericLayout(327686), typmodNumericLayout(-1)},
			[]model.Value{typmodNumericValue("12.3"), typmodNumericValue("12.35"), typmodNumericValue("12.345")}},
		{"varchar", `SELECT $1::varchar(1),$1::varchar(4),$1`, typmodCharacterLayout(1043, -1, 100), model.StringVal("é界x"),
			[]model.TypeLayout{typmodCharacterLayout(1043, 5, 100), typmodCharacterLayout(1043, 8, 100), typmodCharacterLayout(1043, -1, 100)},
			[]model.Value{model.StringVal("é"), model.StringVal("é界x"), model.StringVal("é界x")}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			parsed, err := zpgparse.Parse(scenario.sql)
			if err != nil {
				t.Fatal(err)
			}
			bound, err := bind.Bind(t.Context(), parsed.Stmts[0], nil, bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950, Limits: irLimits()}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if parameters := bound.Parameters(); len(parameters) != 1 || parameters[0].Type != scenario.input.Type {
				t.Fatalf("inferred parameters=%+v, want one unconstrained %+v", parameters, scenario.input.Type)
			}
			graph := sqlGraph(t, scenario.sql, nil)
			env, dir := groupingSetsEnvironment(t, false)
			env.Parameters = []expr.ParameterValue{{Number: 1, Layout: scenario.input, Value: scenario.value}}
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertArrayAggregateRows(t, portal, scenario.layouts, [][]model.Value{scenario.want})
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLCharacterTypmodIndeterminateComparisonMasks(t *testing.T) {
	graph := sqlGraph(t, `SELECT CASE WHEN active THEN a::char(3)=b::char(2) ELSE false END
		FROM (VALUES(false,'a' COLLATE "C",'a' COLLATE "POSIX"),(true,NULL,'a'),(true,'a','a')) v(active,a,b)`, nil)
	env, dir := groupingSetsEnvironment(t, false)
	env.Limits.BatchRows = 1
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	boolean := model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}
	for _, want := range []model.Value{model.BoolVal(false), model.NullVal(model.TypeBoolean)} {
		var batch columnar.Batch
		if err := portal.Next(t.Context(), &batch); err != nil {
			t.Fatal(err)
		}
		assertArrayAggregateBatch(t, &batch, []model.TypeLayout{boolean}, [][]model.Value{{want}})
	}
	assertArrayAggregateFailure(t, portal, "42P22")
	groupingSetsReleased(t, env, dir)
}

func TestSQLTypmodAggregateInputsAreCoercedBeforeTransition(t *testing.T) {
	assertCollationSQL(t, `SELECT sum(x::numeric(3,1)),string_agg(y::varchar(2),',' ORDER BY id),
		count(DISTINCT y::char(2)) FROM (VALUES(1,1.24::numeric,'é界x'::text),(2,1.24,'é界y'),(3,NULL,NULL)) v(id,x,y)`,
		[]model.TypeLayout{typmodNumericLayout(-1), collationTextLayout(100), {Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}},
		[][]model.Value{{typmodNumericValue("2.4"), model.StringVal("é界,é界"), model.Int64Val(1)}})
}

func TestSQLTypmodWindowInputsAreCoercedBeforeFrameEvaluation(t *testing.T) {
	assertCollationSQL(t, `SELECT x::numeric(3,1),sum(x::numeric(3,1)) OVER w,
		string_agg(y::varchar(2),',') OVER w FROM (VALUES(1,1.24::numeric,'é界x'::text),(2,1.24,'é界y'),(3,NULL,NULL)) v(id,x,y)
		WINDOW w AS (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) ORDER BY id`,
		[]model.TypeLayout{typmodNumericLayout(196613), typmodNumericLayout(-1), collationTextLayout(100)},
		[][]model.Value{
			{typmodNumericValue("1.2"), typmodNumericValue("1.2"), model.StringVal("é界")},
			{typmodNumericValue("1.2"), typmodNumericValue("2.4"), model.StringVal("é界,é界")},
			{model.NullVal(model.TypeNumeric), typmodNumericValue("1.2"), model.StringVal("é界")},
		})
}

func TestSQLTypmodWindowCloseReleasesActiveSpill(t *testing.T) {
	for _, ending := range []string{"close", "cancel"} {
		t.Run(ending, func(t *testing.T) {
			graph := sqlGraph(t, `SELECT sum(x::numeric(3,1)) OVER w,string_agg(y::varchar(2),',') OVER w
				FROM (VALUES(1,1.24::numeric,'é界x'::text),(2,1.24,'é界y'),(3,NULL,NULL)) v(id,x,y)
				WINDOW w AS (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)`, nil)
			env, dir := groupingSetsEnvironment(t, true)
			env.Aggregate.PartitionMemoryBytes = 4096
			env.Window.PartitionMemoryBytes = 0
			env.Limits.BatchRows = 1
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			portal, err := exec.Open(ctx, graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var batch columnar.Batch
			if err := portal.Next(ctx, &batch); err != nil {
				t.Fatal(err)
			}
			assertArrayAggregateBatch(t, &batch, []model.TypeLayout{typmodNumericLayout(-1), collationTextLayout(100)}, [][]model.Value{{typmodNumericValue("2.4"), model.StringVal("é界,é界")}})
			if usage := env.Query.Usage(); usage.Spill.Live == 0 || usage.Files == 0 {
				t.Fatalf("spill was not active before %s: %+v", ending, usage)
			}
			if ending == "cancel" {
				cancel()
				if err := portal.Next(ctx, &batch); !errors.Is(err, context.Canceled) {
					t.Fatalf("Next error=%v, want context cancellation", err)
				}
			} else {
				closePortal(t, portal)
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

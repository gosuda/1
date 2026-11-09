package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLMinMaxCommonTypeAndNulls(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}
	i, n := model.Int64Val, model.NullVal(model.TypeInt64)
	assertCollationSQL(t, `SELECT greatest(a,b),least(a,b),greatest(a),least(b)
		FROM (VALUES(1,2::integer,5::bigint),(2,NULL,(-9)::bigint),(3,7,NULL),(4,NULL,NULL)) v(k,a,b) ORDER BY k`,
		[]model.TypeLayout{integer, integer, {Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}, integer},
		[][]model.Value{{i(5), i(2), i(2), i(5)}, {i(-9), i(-9), n, i(-9)}, {i(7), i(7), i(7), n}, {n, n, n, n}})
}

func TestSQLMinMaxPreservesCharacterModifiersAndTies(t *testing.T) {
	s := model.StringVal
	assertCollationSQL(t, `SELECT greatest(a::varchar(3),b::varchar(3)),least(a::varchar(1),b::varchar(4)),
		greatest(a::char(2),a::char(3)),least(a::char(2),a::char(3))
		FROM (VALUES('a' COLLATE "POSIX",'abcd'::text)) v(a,b)`,
		[]model.TypeLayout{typmodCharacterLayout(1043, 7, 951), typmodCharacterLayout(1043, -1, 951), typmodCharacterLayout(1042, -1, 951), typmodCharacterLayout(1042, -1, 951)},
		[][]model.Value{{s("abc"), s("a"), s("a "), s("a ")}})
}

func TestSQLMinMaxOrdersNaNAboveInfinity(t *testing.T) {
	boolean := model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}
	truth := model.BoolVal(true)
	assertCollationSQL(t, `SELECT greatest(a,b)='NaN'::float8,least(a,b)='Infinity'::float8,
		greatest(c,d)='NaN'::numeric,least(c,d)='Infinity'::numeric
		FROM (VALUES('NaN'::float8,'Infinity'::float8,'Infinity'::numeric,'NaN'::numeric)) v(a,b,c,d)`,
		[]model.TypeLayout{boolean, boolean, boolean, boolean}, [][]model.Value{{truth, truth, truth, truth}})
}

func TestSQLMinMaxMasksOnlyOuterInactiveLanes(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	i := model.Int64Val
	assertCollationSQL(t, `SELECT CASE WHEN x=0 THEN 7 ELSE greatest(NULL,1,10/x) END,
		CASE WHEN x=0 THEN 7 ELSE least(NULL,1,10/x) END FROM (VALUES(0),(2)) v(x)`,
		[]model.TypeLayout{integer, integer}, [][]model.Value{{i(7), i(7)}, {i(5), i(1)}})
	for _, sql := range []string{
		`SELECT greatest(NULL::integer,1,10/x) FROM (VALUES(0)) v(x)`,
		`SELECT least(NULL::integer,1,10/x) FROM (VALUES(0)) v(x)`,
	} {
		graph := sqlGraph(t, sql, nil)
		env := statefulEnvironment(t)
		portal, err := exec.Open(t.Context(), graph, env)
		if err != nil {
			t.Fatal(err)
		}
		defer closePortal(t, portal)
		assertArrayAggregateFailure(t, portal, "22012")
		aggregateProjectionReleased(t, env.Query)
	}
}

func TestSQLMinMaxExceedsBuiltinFixedArity(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	assertCollationSQL(t, `SELECT greatest(x,1,2,3,4,5,6,7,8,9),least(x,1,2,3,4,5,6,7,8,9) FROM (VALUES(0)) v(x)`,
		[]model.TypeLayout{integer, integer}, [][]model.Value{{model.Int64Val(9), model.Int64Val(0)}})
}

package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLCharacterConversionsPreserveActiveLanes(t *testing.T) {
	graph := sqlGraph(t, `SELECT ascii(s), CASE WHEN n=0 THEN '' ELSE chr(n) END, to_hex(h), to_hex(h::bigint)
 FROM (VALUES(1,'🙂'::text,128578,-1),(2,''::text,0,256),(3,NULL::text,NULL::integer,NULL::integer)) t(k,s,n,h) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i, s := model.Int64Val, model.StringVal
	assertAnyValueRows(t, portal, []model.TypeID{23, 25, 25, 25}, [][]model.Value{
		{i(128578), s("🙂"), s("ffffffff"), s("ffffffffffffffff")},
		{i(0), s(""), s("100"), s("100")},
		{model.NullVal(model.TypeInt64), model.NullVal(model.TypeString), model.NullVal(model.TypeString), model.NullVal(model.TypeString)},
	})
	aggregateProjectionReleased(t, env.Query)
}

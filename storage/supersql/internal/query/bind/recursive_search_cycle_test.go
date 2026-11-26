package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestBindRecursiveSearchCycleMetadata(t *testing.T) {
	sql := `WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM t WHERE n<3) SEARCH DEPTH FIRST BY n SET ord CYCLE n SET cycled TO 'hit' DEFAULT 'go' USING trail SELECT * FROM t`
	statement, err := bindSQL(t, sql, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 4 || columns[0].Name != "n" || columns[1].Name != "ord" || columns[2].Name != "cycled" || columns[3].Name != "trail" {
		t.Fatalf("recursive columns = %#v", columns)
	}
	if columns[1].Layout.Type.Storage != model.TypeArray || columns[2].Layout.Type.Storage != model.TypeString || columns[3].Layout.Type.Storage != model.TypeArray {
		t.Fatalf("generated layouts = %#v", columns[1:])
	}
	graph := statement.Graph()
	var recursion ir.Rel
	var recursionID ir.RelID
	for id := ir.RelID(1); uint32(id) <= graph.RelCount(); id++ {
		relation, _ := graph.Rel(id)
		if relation.Kind == ir.RecursiveUnion {
			recursion, recursionID = relation, id
			break
		}
	}
	if recursionID == 0 || recursion.Recursion.Search != ir.DepthFirst || recursion.Recursion.SearchKeyCount() != 1 || recursion.Recursion.CycleKeyCount() != 1 || recursion.Recursion.CycleContinue == 0 {
		t.Fatalf("recursive metadata = %#v", recursion)
	}
	searchKey, _ := graph.SearchKey(recursionID, 0)
	cycleKey, _ := graph.CycleKey(recursionID, 0)
	if searchKey != cycleKey || searchKey == recursion.Recursion.SearchSlot || searchKey == recursion.Recursion.CycleMark || searchKey == recursion.Recursion.CyclePath {
		t.Fatalf("generated/key slots = %d/%d in %#v", searchKey, cycleKey, recursion.Recursion)
	}
}

func TestBindRecursiveSearchCycleRejectsMalformedClauses(t *testing.T) {
	for _, tc := range []struct {
		name, sql, code string
	}{
		{"nonrecursive", `WITH RECURSIVE t(n) AS (SELECT 1) SEARCH DEPTH FIRST BY n SET ord SELECT * FROM t`, "42601"},
		{"missing search key", `WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n FROM t) SEARCH DEPTH FIRST BY absent SET ord SELECT * FROM t`, "42601"},
		{"duplicate search key", `WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n FROM t) SEARCH DEPTH FIRST BY n,n SET ord SELECT * FROM t`, "42701"},
		{"search output collision", `WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n FROM t) SEARCH DEPTH FIRST BY n SET n SELECT * FROM t`, "42601"},
		{"cycle output collision", `WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n FROM t) CYCLE n SET n USING trail SELECT * FROM t`, "42601"},
		{"cycle output alias", `WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n FROM t) CYCLE n SET repeated USING repeated SELECT * FROM t`, "42601"},
		{"search cycle collision", `WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n FROM t) SEARCH DEPTH FIRST BY n SET trail CYCLE n SET repeated USING trail SELECT * FROM t`, "42601"},
		{"left values", `WITH RECURSIVE t(n) AS (VALUES(1) UNION ALL SELECT n FROM t) SEARCH DEPTH FIRST BY n SET ord SELECT * FROM t`, "0A000"},
		{"right values", `WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL VALUES(2)) SEARCH DEPTH FIRST BY n SET ord SELECT * FROM t`, "42601"},
		{"recursive generated column hidden", `WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT ord FROM t) SEARCH DEPTH FIRST BY n SET ord SELECT * FROM t`, "42703"},
		{"cycle key without equality", `WITH RECURSIVE t(j) AS (SELECT '{}'::json UNION ALL SELECT j FROM t) CYCLE j SET repeated USING trail SELECT * FROM t`, "42883"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindSQL(t, tc.sql, nil, nil)
			requireCode(t, err, tc.code)
		})
	}
}

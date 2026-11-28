package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

// PostgreSQL 18.4 references:
//   src/backend/executor/nodeProjectSet.c (lockstep advancement, NULL padding,
//   pinned outer tuple, and nested ProjectSet levels)
//   src/backend/optimizer/util/tlist.c (split_pathtarget_at_srfs_extended)
func TestSQLProjectSetLockstepNestedAndTailOrder(t *testing.T) {
	i := model.Int64Val
	null := model.NullVal(model.TypeInt64)
	for _, test := range []struct {
		name string
		sql  string
		oids []model.TypeID
		want [][]model.Value
	}{
		{"lockstep null padding", `SELECT generate_series(1,2), generate_series(10,12)`, []model.TypeID{23, 23}, [][]model.Value{{i(1), i(10)}, {i(2), i(11)}, {null, i(12)}}},
		{"empty sibling still pads", `SELECT generate_series(1,0), generate_series(7,8)`, []model.TypeID{23, 23}, [][]model.Value{{null, i(7)}, {null, i(8)}}},
		{"all empty suppresses outer row", `SELECT generate_series(1,0), generate_series(4,3)`, []model.TypeID{23, 23}, nil},
		{"nested levels", `SELECT generate_series(1,generate_series(2,3))`, []model.TypeID{23}, [][]model.Value{{i(1)}, {i(2)}, {i(1)}, {i(2)}, {i(3)}}},
		{"tail after expansion", `SELECT generate_series(3,1,-1) AS x ORDER BY x LIMIT 2`, []model.TypeID{23}, [][]model.Value{{i(1)}, {i(2)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			graph := sqlGraph(t, test.sql, nil)
			environment := applyEnvironment(t)
			environment.Limits.BatchRows = 1
			portal, err := exec.Open(t.Context(), graph, environment)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertAnyValueRows(t, portal, test.oids, test.want)
			aggregateProjectionReleased(t, environment.Query)
		})
	}
}

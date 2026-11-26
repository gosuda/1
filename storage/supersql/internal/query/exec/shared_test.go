package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

func TestMaterializedCTEHasIndependentConsumers(t *testing.T) {
	graph := sqlGraph(t, `WITH c AS MATERIALIZED (VALUES(7),(9)) SELECT * FROM c UNION ALL SELECT * FROM c`, nil)
	env, shared := environment(t)
	env.Spool = rowstore.Limits{MemoryBytes: 64 << 10, MaxBytes: 1 << 20, MaxRows: 1024, MaxRecordBytes: 4096, MetadataBytes: 4096, DecodeBytes: 4096, MaxReaders: 4}
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	counts := map[int64]int{}
	for _, row := range rows(t, portal) {
		if len(row) != 1 || row[0].IsNull {
			t.Fatalf("CTE row=%v", row)
		}
		counts[row[0].IntVal]++
	}
	if len(counts) != 2 || counts[7] != 2 || counts[9] != 2 {
		t.Fatalf("CTE consumers=%v", counts)
	}
	if usage := shared.Usage(); usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("CTE EOF retained resources: %+v", usage)
	}
}

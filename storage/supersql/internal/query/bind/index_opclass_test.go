package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func indexClassSnapshot(t *testing.T) metadataSnapshot {
	t.Helper()
	snapshot := tableSnapshot(t)
	spec, err := snapshot.descriptors[1].SpecWithin(t.Context(), model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20})
	if err != nil {
		t.Fatal(err)
	}
	spec.Table.PrimaryKey = []model.KeyElement{{Column: 1, Type: spec.Table.Columns[0].Layout.Type}}
	snapshot.descriptors[1] = testDescriptor(t, spec)
	return snapshot
}

func bindIndexClass(t *testing.T, sql string) (*bind.BoundCreateIndex, error) {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	return bind.BindCreateIndex(t.Context(), parsed.Stmts[0].Stmt.GetIndexStmt(), indexClassSnapshot(t), bind.CreateIndexOptions{
		Bind: bindingOptions(sql), Descriptors: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20},
		ObjectID: 12, OID: 10012, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
	})
}

func TestBindIndexResolvesPinnedBtreeOperatorClass(t *testing.T) {
	bound, err := bindIndexClass(t, `CREATE INDEX cls_idx ON public."Mixed" ("X" pg_catalog.int4_ops DESC NULLS LAST)`)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := bound.Index.SpecWithin(t.Context(), model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20})
	if err != nil {
		t.Fatal(err)
	}
	key := spec.Index.Keys[0]
	if key.OperatorClass != 1978 || key.Type.ID != 23 || !key.Descending || key.NullsFirst {
		t.Fatalf("bound ordered index key: %+v", key)
	}
}

func TestBindIndexRejectsIncompatibleClassInput(t *testing.T) {
	_, err := bindIndexClass(t, `CREATE INDEX cls_idx ON public."Mixed" ("X" int8_ops)`)
	requireCode(t, err, "42804")
}

func TestBindIndexRejectsClassFromWrongNamespace(t *testing.T) {
	_, err := bindIndexClass(t, `CREATE INDEX cls_idx ON public."Mixed" ("X" public.int4_ops)`)
	requireCode(t, err, "42704")
}

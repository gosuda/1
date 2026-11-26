package bind_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestStandaloneIndexPreservesRepeatedColumns(t *testing.T) {
	bound, err := bindIndexClass(t, `CREATE INDEX ON public."Mixed" ("X" ASC, "X" DESC) INCLUDE ("X", "X")`)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Index.Name().Name != "Mixed_X_X1_X2_X3_idx" {
		t.Fatalf("generated index name %q", bound.Index.Name().Name)
	}
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20}
	spec, err := bound.Index.SpecWithin(t.Context(), limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Index.Keys) != 2 || len(spec.Index.Include) != 2 {
		t.Fatalf("repeated attributes lost: %+v", spec.Index)
	}
	if spec.Index.Keys[0].Column != 1 || spec.Index.Keys[1].Column != 1 || spec.Index.Keys[0].Descending || !spec.Index.Keys[1].Descending {
		t.Fatalf("key roles changed: %+v", spec.Index.Keys)
	}
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := schema.NewIndexPlan(t.Context(), bound.Table, bound.Index, encoder, nil, schema.IndexLimits{MetadataBytes: 1 << 20, EntryBytes: 1 << 16, MaxColumns: 16})
	if err != nil {
		t.Fatal(err)
	}
	typ := model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}
	key, err := encoder.EncodePrefix(t.Context(), []storage.KeyColumn{{ColumnID: 1, Type: typ}}, []model.Value{model.Int64Val(42)}, 128)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := plan.Entry(t.Context(), &model.Row{Key: key, Values: []model.Value{model.Int64Val(42)}})
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil || len(entry.Values) != 6 {
		t.Fatalf("incomplete repeated-column entry: %+v", entry)
	}
	for _, position := range []int{0, 1, 4, 5} {
		if entry.Values[position].Type != model.TypeInt64 || entry.Values[position].IntVal != 42 {
			t.Fatalf("index attribute %d: %+v", position, entry.Values[position])
		}
	}
}

func TestPrimaryKeyStillRejectsRepeatedColumns(t *testing.T) {
	snapshot := indexClassSnapshot(t)
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20}
	spec, err := snapshot.descriptors[1].SpecWithin(t.Context(), limits)
	if err != nil {
		t.Fatal(err)
	}
	spec.Table.PrimaryKey = append(spec.Table.PrimaryKey, spec.Table.PrimaryKey[0])
	if _, err := model.NewDescriptor(t.Context(), spec, limits); !errors.Is(err, model.ErrInvalidDescriptor) {
		t.Fatalf("repeated primary-key column accepted: %v", err)
	}
}

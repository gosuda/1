package schema

import (
	"bytes"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestGINIndexPlanAndEntries(t *testing.T) {
	ctx := t.Context()
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	int64Type := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
	jsonbType := model.Type{ID: 3802, Modifier: -1, Storage: model.TypeJSONB}
	intArrLayout := model.TypeLayout{
		Type:     model.Type{ID: 1016, Modifier: -1, Storage: model.TypeArray},
		Children: []model.TypeLayout{{Type: int64Type}},
	}
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64}

	table, err := model.NewDescriptor(ctx, model.DescriptorSpec{
		ID: 10, OID: 10010, Version: 1, State: model.StatePublic,
		Name:     model.NamespaceName{Parent: 2, Kind: model.ObjectTable, Name: "gin_docs"},
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Table: &model.TableMetadata{
			ColumnIDHighWatermark: 3,
			Columns: []model.ColumnDescriptor{
				{ID: 1, Name: "id", State: model.StatePublic, Layout: model.TypeLayout{Type: int64Type}},
				{ID: 2, Name: "doc", State: model.StatePublic, Nullable: true, Layout: model.TypeLayout{Type: jsonbType}},
				{ID: 3, Name: "tags", State: model.StatePublic, Nullable: true, Layout: intArrLayout},
			},
			PrimaryKey: []model.KeyElement{{Column: 1, Type: int64Type}},
		},
	}, limits)
	if err != nil {
		t.Fatal(err)
	}

	idxJSONB, err := model.NewDescriptor(ctx, model.DescriptorSpec{
		ID: 11, OID: 10011, Version: 1, State: model.StatePublic,
		Name:     model.NamespaceName{Parent: 2, Kind: model.ObjectIndex, Name: "idx_gin_doc"},
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Index: &model.IndexMetadata{
			Table:   10,
			Method:  "gin",
			OpClass: "jsonb_ops",
			Keys:    []model.KeyElement{{Column: 2, Type: jsonbType}},
		},
	}, limits)
	if err != nil {
		t.Fatal(err)
	}

	idxArr, err := model.NewDescriptor(ctx, model.DescriptorSpec{
		ID: 12, OID: 10012, Version: 1, State: model.StatePublic,
		Name:     model.NamespaceName{Parent: 2, Kind: model.ObjectIndex, Name: "idx_gin_tags"},
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Index: &model.IndexMetadata{
			Table:   10,
			Method:  "gin",
			OpClass: "array_ops",
			Keys:    []model.KeyElement{{Column: 3, Type: intArrLayout.Type, Children: intArrLayout.Children}},
		},
	}, limits)
	if err != nil {
		t.Fatal(err)
	}

	idxLimits := IndexLimits{MetadataBytes: 1 << 20, EntryBytes: 1 << 20, MaxColumns: 64}
	planJSONB, err := NewIndexPlan(ctx, table, idxJSONB, encoder, nil, idxLimits)
	if err != nil {
		t.Fatalf("NewIndexPlan jsonb: %v", err)
	}
	if !planJSONB.IsGIN() || planJSONB.OpClass() != "jsonb_ops" || planJSONB.GINColumn() != 2 || planJSONB.OwnerColumn() != 3 {
		t.Fatalf("unexpected jsonb GIN plan: isGIN=%v opClass=%q col=%d owner=%d", planJSONB.IsGIN(), planJSONB.OpClass(), planJSONB.GINColumn(), planJSONB.OwnerColumn())
	}

	planArr, err := NewIndexPlan(ctx, table, idxArr, encoder, nil, idxLimits)
	if err != nil {
		t.Fatalf("NewIndexPlan array: %v", err)
	}
	if !planArr.IsGIN() || planArr.OpClass() != "array_ops" || planArr.GINColumn() != 3 {
		t.Fatalf("unexpected array GIN plan: isGIN=%v opClass=%q col=%d", planArr.IsGIN(), planArr.OpClass(), planArr.GINColumn())
	}

	pk1, err := encoder.EncodePrefix(ctx, []storage.KeyColumn{{ColumnID: 1, Type: int64Type}}, []model.Value{model.Int64Val(1)}, 1024)
	if err != nil {
		t.Fatal(err)
	}

	// Non-null JSONB & array row produces multiple distinct posting entries.
	row := &model.Row{
		Key: pk1,
		Values: []model.Value{
			model.Int64Val(1),
			{Type: model.TypeJSONB, StrVal: `{"a": 1, "b": "hello"}`},
			{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(10), model.Int64Val(20), model.Int64Val(10)}, Dimensions: []model.ArrayDimension{{Length: 3, LowerBound: 1}}},
		},
	}
	jsonEntries, err := planJSONB.Entries(ctx, row)
	if err != nil {
		t.Fatalf("jsonb Entries: %v", err)
	}
	if len(jsonEntries) < 4 {
		t.Fatalf("expected >=4 jsonb tokens, got %d", len(jsonEntries))
	}
	for _, e := range jsonEntries {
		if len(e.Values) != 3 || !bytes.Equal(e.Values[2].BytesVal, pk1) {
			t.Fatalf("invalid GIN posting entry: %+v", e)
		}
	}

	arrEntries, err := planArr.Entries(ctx, row)
	if err != nil {
		t.Fatalf("array Entries: %v", err)
	}
	// Deduplicated {10, 20} -> 2 tokens!
	if len(arrEntries) != 2 {
		t.Fatalf("expected 2 deduplicated array tokens, got %d", len(arrEntries))
	}

	// NULL values emit 0 postings cleanly without error.
	nullRow := &model.Row{
		Key: pk1,
		Values: []model.Value{
			model.Int64Val(1),
			model.NullVal(model.TypeJSONB),
			model.NullVal(model.TypeArray),
		},
	}
	if entries, err := planJSONB.Entries(ctx, nullRow); err != nil || len(entries) != 0 {
		t.Fatalf("NULL jsonb entries = (%d, %v), want (0, nil)", len(entries), err)
	}
	if entries, err := planArr.Entries(ctx, nullRow); err != nil || len(entries) != 0 {
		t.Fatalf("NULL array entries = (%d, %v), want (0, nil)", len(entries), err)
	}
}

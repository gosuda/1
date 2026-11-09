package session

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// TestAnalyzeTableLayoutPreservesNullabilityAndDefaults pins that the ANALYZE
// scan layout matches the stored layout: a nullable column stays nullable and a
// constant default is carried as the missing value. Without it the storage layer
// reports a NULL in a NOT NULL column (or a column absent from a PAX block) as
// corruption, so ANALYZE failed on any table with a null value.
func TestAnalyzeTableLayoutPreservesNullabilityAndDefaults(t *testing.T) {
	integer := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
	fallback := model.Int64Val(7)
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 64, MaxDepth: 64}
	descriptor, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: 12, OID: 20012, Version: 1, State: model.StatePublic,
		Name:     model.NamespaceName{Parent: 2, Kind: model.ObjectTable, Name: "wide"},
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Table: &model.TableMetadata{
			Owner:                 10,
			ColumnIDHighWatermark: 3,
			Columns: []model.ColumnDescriptor{
				{ID: 1, Name: "id", State: model.StatePublic, Layout: model.TypeLayout{Type: integer}},
				{ID: 2, Name: "nullable", State: model.StatePublic, Layout: model.TypeLayout{Type: integer}, Nullable: true},
				{ID: 3, Name: "defaulted", State: model.StatePublic, Layout: model.TypeLayout{Type: integer}, Nullable: true, ConstantDefault: &fallback},
			},
			PrimaryKey: []model.KeyElement{{Column: 1, Type: integer}},
		},
	}, limits)
	if err != nil {
		t.Fatal(err)
	}
	_, _, layout, err := analyzeTableLayout(t.Context(), descriptor, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout) != 3 {
		t.Fatalf("layout columns = %d, want 3", len(layout))
	}
	if layout[0].Nullable || !layout[1].Nullable || !layout[2].Nullable {
		t.Fatalf("nullability not preserved: %+v", layout)
	}
	if layout[1].HasMissingValue {
		t.Fatalf("nullable column without a default gained a missing value: %+v", layout[1])
	}
	if !layout[2].HasMissingValue || layout[2].MissingValue.IntVal != 7 {
		t.Fatalf("constant default not carried as the missing value: %+v", layout[2])
	}
}

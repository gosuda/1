package schema_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type uncertifiedKeys struct{ storage.KeyEncoder }

func patternIndexDescriptors(t *testing.T) (*model.Descriptor, *model.Descriptor) {
	t.Helper()
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 32, MaxDepth: 16}
	integer := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
	text := model.Type{ID: 1042, Modifier: -1, Storage: model.TypeString, Collation: 950}
	table, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{ID: 1, OID: 90001, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Parent: 10, Kind: model.ObjectTable, Name: "items"}, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Table: &model.TableMetadata{ColumnIDHighWatermark: 2, Columns: []model.ColumnDescriptor{{ID: 1, Name: "id", State: model.StatePublic, Layout: model.TypeLayout{Type: integer}}, {ID: 2, Name: "name", State: model.StatePublic, Layout: model.TypeLayout{Type: text}}}, PrimaryKey: []model.KeyElement{{Column: 1, Type: integer}}, Indexes: []model.ObjectID{2}}}, limits)
	if err != nil {
		t.Fatal(err)
	}
	index, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{ID: 2, OID: 90002, Version: 1, State: model.StateWriteOnly,
		Name: model.NamespaceName{Parent: 10, Kind: model.ObjectIndex, Name: "names"}, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Index: &model.IndexMetadata{Table: 1, Unique: true, Keys: []model.KeyElement{{Column: 2, Type: text, Collation: 950, OperatorClass: 4219}}}}, limits)
	if err != nil {
		t.Fatal(err)
	}
	return table, index
}

func TestPatternIndexPreservesBlankInsensitiveUniqueness(t *testing.T) {
	table, index := patternIndexDescriptors(t)
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := schema.NewIndexPlan(t.Context(), table, index, encoder, nil, schema.IndexLimits{MetadataBytes: 1 << 20, EntryBytes: 1 << 16, MaxColumns: 16})
	if err != nil {
		t.Fatal(err)
	}
	integer := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
	var keys [][]byte
	for i, value := range []string{"name", "name   "} {
		primary, err := encoder.EncodePrefix(t.Context(), []storage.KeyColumn{{ColumnID: 1, Type: integer}}, []model.Value{model.Int64Val(int64(i + 1))}, 128)
		if err != nil {
			t.Fatal(err)
		}
		entry, err := plan.Entry(t.Context(), &model.Row{Key: primary, Values: []model.Value{model.Int64Val(int64(i + 1)), model.StringVal(value)}})
		if err != nil {
			t.Fatal(err)
		}
		if entry == nil {
			t.Fatal("non-null row omitted from index")
		}
		keys = append(keys, entry.Key)
	}
	if !bytes.Equal(keys[0], keys[1]) {
		t.Fatal("unique pattern index distinguished only trailing spaces")
	}
}

func TestIndexRejectsEncoderWithoutClassCertification(t *testing.T) {
	table, index := patternIndexDescriptors(t)
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	_, err = schema.NewIndexPlan(t.Context(), table, index, uncertifiedKeys{encoder}, nil, schema.IndexLimits{MetadataBytes: 1 << 20, EntryBytes: 1 << 16, MaxColumns: 16})
	var missing *schema.IndexKeyImplementationError
	if !errors.As(err, &missing) || missing.OperatorClass != 4219 {
		t.Fatalf("uncertified codec accepted: %v", err)
	}
}

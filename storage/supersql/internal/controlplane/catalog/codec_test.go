package catalog_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestDescriptorPersistencePreservesNestedDefaultBitsAndOwnership(t *testing.T) {
	bits := uint64(0x7ff8000000000042)
	value := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.Float64Val(math.Float64frombits(bits)), {Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: -2}}, ArrayVal: []model.Value{model.BytesVal([]byte("owned"))}}}}
	layout := model.TypeLayout{Type: model.Type{ID: 500, Storage: model.TypeComposite}, Children: []model.TypeLayout{{Type: model.Type{ID: 701, Storage: model.TypeFloat64}}, {Type: model.Type{ID: 1001, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 17, Storage: model.TypeBytes}}}}}}
	spec := model.DescriptorSpec{ID: 20, OID: 120, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectTable, Name: "defaults"}, Version: 1, State: model.StatePublic, Physical: model.PhysicalIdentity{Generation: 7, EncodingVersion: 2}, Table: &model.TableMetadata{ColumnIDHighWatermark: 2, Columns: []model.ColumnDescriptor{{ID: 1, Name: "payload", State: model.StatePublic, Nullable: true, Layout: layout, ConstantDefault: &value}, {ID: 2, Name: "computed", State: model.StatePublic, DefaultOID: 121, Layout: model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}, Default: &model.ExpressionDefinition{SQL: "nextval('seq')", SemanticVersion: 3, Encoding: []byte{1, 2}, Dependencies: []model.Dependency{{Object: 30, Version: 4}}}}}}}
	d, err := model.NewDescriptor(t.Context(), spec, catalogLimits().Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	value.ArrayVal[1].ArrayVal[0].BytesVal[0] = 'X'
	layout.Children[0].Type.ID = 999
	spec.Table.Columns[1].Default.Encoding[0] = 9
	spec.Table.Columns[1].Default.Dependencies[0].Object = 999
	encoded, err := catalog.EncodeDescriptor(t.Context(), d, catalogLimits())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := catalog.DecodeDescriptor(t.Context(), encoded, catalogLimits())
	if err != nil {
		t.Fatal(err)
	}
	clear(encoded)
	owned, err := decoded.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := owned.Table.Columns[0].ConstantDefault
	if math.Float64bits(got.ArrayVal[0].FloatVal) != bits || string(got.ArrayVal[1].ArrayVal[0].BytesVal) != "owned" || got.ArrayVal[1].Dimensions[0].LowerBound != -2 {
		t.Fatalf("nested default changed: %+v", got)
	}
	if owned.Table.Columns[0].Layout.Children[0].Type.ID != 701 || owned.Table.Columns[1].Default.Encoding[0] != 1 || owned.Table.Columns[1].Default.Dependencies[0].Object != 30 {
		t.Fatal("caller metadata aliases persisted descriptor")
	}
	got.ArrayVal[1].ArrayVal[0].BytesVal[0] = 'Y'
	owned.Table.Columns[1].Default.Encoding[0] = 8
	again, err := decoded.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if string(again.Table.Columns[0].ConstantDefault.ArrayVal[1].ArrayVal[0].BytesVal) != "owned" || again.Table.Columns[1].Default.Encoding[0] != 1 {
		t.Fatal("owned accessor mutated immutable descriptor")
	}
	if decoded.Physical().Generation != 7 || decoded.Version() != 1 {
		t.Fatal("physical generation conflated with descriptor version")
	}
}
func TestCatalogDecoderRejectsCorruptAndBoundedPayloads(t *testing.T) {
	good, err := catalog.EncodeDescriptor(t.Context(), tableDescriptor(t, 20, "bounded", 1), catalogLimits())
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(good, &document); err != nil {
		t.Fatal(err)
	}
	descriptor := document["Descriptor"].(map[string]any)
	table := descriptor["Table"].(map[string]any)
	columns := table["Columns"].([]any)
	table["Columns"] = append(columns, columns[0])
	duplicate, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	table["Columns"] = columns
	document["Version"] = 999
	future, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{good[:len(good)-1], append(bytes.Clone(good), good...), duplicate, future, []byte(`{"Version":1,"Version":1}`), []byte(`{"Version":1,"Unknown":2}`)} {
		if _, err := catalog.DecodeDescriptor(t.Context(), data, catalogLimits()); !errors.Is(err, catalog.ErrCorruptCatalog) {
			t.Fatalf("corrupt record accepted: %v", err)
		}
	}
	small := catalogLimits()
	small.MaxEncodedBytes = uint64(len(good) - 1)
	if _, err := catalog.DecodeDescriptor(t.Context(), good, small); !errors.Is(err, model.ErrDescriptorLimit) {
		t.Fatalf("encoded size bound: %v", err)
	}
	nested := append(bytes.Repeat([]byte("["), 200), bytes.Repeat([]byte("]"), 200)...)
	if _, err := catalog.DecodeDescriptor(t.Context(), nested, catalogLimits()); !errors.Is(err, model.ErrDescriptorLimit) {
		t.Fatalf("depth bound: %v", err)
	}
	count := catalogLimits()
	count.Descriptor.MaxItems = 32
	many := append([]byte("[0"), bytes.Repeat([]byte(",0"), 33)...)
	many = append(many, ']')
	if _, err := catalog.DecodeDescriptor(t.Context(), many, count); !errors.Is(err, model.ErrDescriptorLimit) {
		t.Fatalf("count bound: %v", err)
	}
}
func TestCorruptPersistedDescriptorDoesNotRebindHistoricalMetadata(t *testing.T) {
	f := newCatalogFixture(t)
	empty := f.snapshot(t, 1)
	d := tableDescriptor(t, 20, "corrupt", 1)
	change, err := f.store.Create(t.Context(), empty, d)
	if err != nil {
		t.Fatal(err)
	}
	created := f.commit(t, change)
	mutation := change.Mutations[1]
	mutation.Op = storage.MutationUpdate
	mutation.Values[1] = model.BytesVal([]byte("{}"))
	changed := f.commit(t, catalog.Change{Layouts: f.store.Layouts(), Mutations: []storage.Mutation{mutation}})
	if _, err := f.snapshot(t, changed).Resolve(t.Context(), d.Name()); !errors.Is(err, catalog.ErrCorruptCatalog) {
		t.Fatalf("corrupt descriptor resolution: %v", err)
	}
	requireResolved(t, f.snapshot(t, created), d.Name(), d.ID(), 1, created)
}
func TestCatalogReaderOwnership(t *testing.T) {
	f := newCatalogFixture(t)
	reader, err := f.manager.HistoricalSnapshot(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	borrowed, err := f.store.Snapshot(reader, 1, catalog.BorrowReader)
	if err != nil {
		t.Fatal(err)
	}
	if err := borrowed.Close(); err != nil {
		t.Fatal(err)
	}
	owned, err := f.store.Snapshot(reader, 1, catalog.OwnReader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owned.Descriptor(t.Context(), 20); !errors.Is(err, catalog.ErrUnknownObject) {
		t.Fatalf("borrowed close released reader: %v", err)
	}
	if err := owned.Close(); err != nil {
		t.Fatal(err)
	}
	layout := f.store.Layouts()[1]
	_, err = reader.Get(t.Context(), storage.ReadRequest{Target: layout.Target, Key: []byte{1}, Layout: layout.Columns, ColumnIDs: []model.ColumnID{1, 2}})
	if !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("owned close retained reader pin: %v", err)
	}
}

func TestDescriptorEncodingUsesCatalogOwnershipLimit(t *testing.T) {
	spec := editableDescriptor(t, tableDescriptor(t, 20, "bounded", 1))
	spec.Table.Columns[0].Default = &model.ExpressionDefinition{SQL: string(bytes.Repeat([]byte("1+"), 4096)) + "1", SemanticVersion: 1}
	spec.Table.Columns[0].DefaultOID = 120
	descriptor := ownedDescriptor(t, spec)
	receiving := catalogLimits()
	receiving.Descriptor.MaxBytes = 4096
	if data, err := catalog.EncodeDescriptor(t.Context(), descriptor, receiving); !errors.Is(err, model.ErrDescriptorLimit) || data != nil {
		t.Fatalf("oversized descriptor escaped catalog ownership admission: bytes=%d err=%v", len(data), err)
	}
}

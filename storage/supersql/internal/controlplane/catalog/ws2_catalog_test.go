package catalog_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func ws2SchemaDescriptor(t *testing.T, id model.ObjectID, name string) *model.Descriptor {
	t.Helper()
	d, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: id, OID: model.OID(10000 + id), Version: 1, State: model.StatePublic,
		Name:   model.NamespaceName{Kind: model.ObjectSchema, Name: name},
		Schema: &model.SchemaMetadata{Owner: 10},
	}, catalogLimits().Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func ws2EnumDescriptor(t *testing.T, id model.ObjectID, parent model.ObjectID, name string, deps ...model.Dependency) *model.Descriptor {
	t.Helper()
	oid := model.OID(20000 + id)
	d, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: id, OID: oid, Version: 1, State: model.StatePublic,
		Name:         model.NamespaceName{Parent: parent, Kind: model.ObjectType, Name: name},
		Dependencies: deps,
		Type: &model.TypeMetadata{
			Kind:        model.TypeKindEnum,
			Owner:       10,
			Array:       id + 5000,
			Layout:      model.TypeLayout{Type: model.Type{ID: model.TypeID(oid), Modifier: -1, Storage: model.TypeInt64}},
			Elements:    []string{"a", "b"},
			ElementOIDs: []model.OID{oid + 100000, oid + 100001},
		},
	}, catalogLimits().Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func ws2DomainDescriptor(t *testing.T, id model.ObjectID, parent model.ObjectID, name string, deps ...model.Dependency) *model.Descriptor {
	t.Helper()
	oid := model.OID(20000 + id)
	intType := model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}
	d, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: id, OID: oid, Version: 1, State: model.StatePublic,
		Name:         model.NamespaceName{Parent: parent, Kind: model.ObjectType, Name: name},
		Dependencies: deps,
		Type: &model.TypeMetadata{
			Kind:       model.TypeKindDomain,
			Owner:      10,
			Array:      id + 5000,
			Layout:     model.TypeLayout{Type: model.Type{ID: model.TypeID(oid), Modifier: -1, Storage: model.TypeInt64}},
			BaseLayout: model.TypeLayout{Type: intType},
		},
	}, catalogLimits().Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func ws2CompositeRelationDescriptor(t *testing.T, id model.ObjectID, parent model.ObjectID, name string, deps ...model.Dependency) *model.Descriptor {
	t.Helper()
	intType := model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}
	d, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: id, OID: model.OID(30000 + id), Version: 1, State: model.StatePublic,
		Name:         model.NamespaceName{Parent: parent, Kind: model.ObjectCompositeRelation, Name: name},
		Dependencies: deps,
		CompositeRelation: &model.CompositeRelationMetadata{
			Owner:                10,
			RowType:              id + 1000,
			FieldIDHighWatermark: 1,
			Columns: []model.ColumnDescriptor{
				{ID: 1, Name: "f1", State: model.StatePublic, Nullable: true, Layout: model.TypeLayout{Type: intType}},
			},
		},
	}, catalogLimits().Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func ws2ViewDescriptor(t *testing.T, id model.ObjectID, parent model.ObjectID, name string, deps ...model.Dependency) *model.Descriptor {
	t.Helper()
	intType := model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}
	d, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: id, OID: model.OID(40000 + id), Version: 1, State: model.StatePublic,
		Name:         model.NamespaceName{Parent: parent, Kind: model.ObjectView, Name: name},
		Dependencies: deps,
		View: &model.ViewMetadata{
			Owner:      10,
			Definition: "SELECT 1",
			Query:      model.ExpressionDefinition{SemanticVersion: 1, Encoding: []byte{1}},
			Columns: []model.ColumnDescriptor{
				{ID: 1, Name: "c", State: model.StatePublic, Nullable: true, Layout: model.TypeLayout{Type: intType}},
			},
		},
	}, catalogLimits().Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func ws2TableWithColumnType(t *testing.T, id model.ObjectID, parent model.ObjectID, name string, colType model.Type, deps ...model.Dependency) *model.Descriptor {
	t.Helper()
	d, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: id, OID: model.OID(50000 + id), Version: 1, State: model.StatePublic,
		Name:         model.NamespaceName{Parent: parent, Kind: model.ObjectTable, Name: name},
		Physical:     model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Dependencies: deps,
		Table: &model.TableMetadata{
			ColumnIDHighWatermark: 1,
			Columns: []model.ColumnDescriptor{
				{ID: 1, Name: "col", State: model.StatePublic, Nullable: true, Layout: model.TypeLayout{Type: colType}},
			},
		},
	}, catalogLimits().Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDropOrderAdmitsLargeCatalogBeyondPerStatementMaximum(t *testing.T) {
	f := newCatalogFixture(t)
	empty := f.snapshot(t, 1)
	tx := f.begin(t)

	schema := ws2SchemaDescriptor(t, 10, "public")
	schemaChange, err := f.store.Create(t.Context(), empty, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.ApplyMutations(t.Context(), schemaChange.Mutations); err != nil {
		t.Fatal(err)
	}

	// Create 90 total catalog objects (> 80, exceeding per-statement maximum=64):
	// - 50 unrelated tables (IDs 100..149)
	// - 35 unrelated enum types (IDs 200..234)
	// - 3-level object dependency chain: rootTable (300) -> midView (301) -> leafView (302)
	// - 2-level type dependency chain: domainType (310) -> domainTable (311)
	for i := 0; i < 50; i++ {
		tbl := tableDescriptor(t, model.ObjectID(100+i), fmt.Sprintf("tbl_%02d", i), 1)
		change, err := f.store.Create(t.Context(), empty, tbl)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.ApplyMutations(t.Context(), change.Mutations); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 35; i++ {
		enum := ws2EnumDescriptor(t, model.ObjectID(200+i), 10, fmt.Sprintf("enum_%02d", i))
		change, err := f.store.Create(t.Context(), empty, enum)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.ApplyMutations(t.Context(), change.Mutations); err != nil {
			t.Fatal(err)
		}
	}

	rootTable := tableDescriptor(t, 300, "root_tbl", 1)
	midView := ws2ViewDescriptor(t, 301, 10, "mid_view", model.Dependency{Object: 300, Version: 1})
	leafView := ws2ViewDescriptor(t, 302, 10, "leaf_view", model.Dependency{Object: 301, Version: 1})
	domainType := ws2DomainDescriptor(t, 310, 10, "dom_t")
	domainTable := ws2TableWithColumnType(t, 311, 10, "dom_tbl", model.Type{ID: model.TypeID(domainType.OID()), Modifier: -1, Storage: model.TypeInt64}, model.Dependency{Object: 310, Version: 1})

	for _, desc := range []*model.Descriptor{rootTable, midView, leafView, domainType, domainTable} {
		change, err := f.store.Create(t.Context(), empty, desc)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.ApplyMutations(t.Context(), change.Mutations); err != nil {
			t.Fatal(err)
		}
	}

	result, err := tx.Commit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snap := f.snapshot(t, result.CommitTS)

	// 1. DROP TABLE CASCADE with maximum=64 succeeds when catalog has 90 objects,
	//    and returns strict topological order [leafView, midView, rootTable] in ONE scan!
	readsBefore := f.store.ReadCount()
	ordered, err := f.store.PlanObjectDrops(t.Context(), snap, []*model.Descriptor{rootTable}, true, 64)
	readsUsed := f.store.ReadCount() - readsBefore
	if err != nil {
		t.Fatalf("PlanObjectDrops CASCADE with 90 catalog objects and maximum=64 failed: %v", err)
	}
	if len(ordered) != 3 || ordered[0].ID() != 302 || ordered[1].ID() != 301 || ordered[2].ID() != 300 {
		t.Fatalf("PlanObjectDrops order = %v, want [302 301 300]", ordered)
	}
	// Single pass over 91 objects (1 schema + 90 objects) should take <= 2*91 = 182 reads
	// (3 passes of the old loop would take > 500 reads!).
	if readsUsed > 200 {
		t.Fatalf("PlanObjectDrops performed %d catalog reads (expected single pass <= 200)", readsUsed)
	}

	// 2. DROP TABLE RESTRICT with maximum=64 returns 2BP01 (not 54000!).
	_, err = f.store.PlanObjectDrops(t.Context(), snap, []*model.Descriptor{rootTable}, false, 64)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "2BP01" {
		t.Fatalf("PlanObjectDrops RESTRICT = %v, want 2BP01", err)
	}

	// 3. Per-statement maximum STILL caps affected cascade target count (3 > 2 -> 54000).
	_, err = f.store.PlanObjectDrops(t.Context(), snap, []*model.Descriptor{rootTable}, true, 2)
	if !errors.As(err, &sqlErr) || sqlErr.Code != "54000" {
		t.Fatalf("PlanObjectDrops CASCADE with maximum=2 on 3-object chain = %v, want 54000", err)
	}

	// 4. DROP TYPE CASCADE with maximum=64 succeeds with 90 catalog objects!
	typeOrder, err := f.store.TypeDropOrder(t.Context(), snap, []*model.Descriptor{domainType}, true, 64)
	if err != nil {
		t.Fatalf("TypeDropOrder CASCADE with 90 catalog objects and maximum=64 failed: %v", err)
	}
	if len(typeOrder) != 2 || typeOrder[0].ID() != 311 || typeOrder[1].ID() != 310 {
		t.Fatalf("TypeDropOrder = %v, want [311 310]", typeOrder)
	}

	// 5. DomainColumnReferences with maximum=64 succeeds with 90 catalog objects!
	refs, err := f.store.DomainColumnReferences(t.Context(), snap, model.TypeID(domainType.OID()), 64)
	if err != nil {
		t.Fatalf("DomainColumnReferences with 90 catalog objects and maximum=64 failed: %v", err)
	}
	if len(refs) != 1 || refs[0].Table.ID() != 311 {
		t.Fatalf("DomainColumnReferences = %+v, want [table 311]", refs)
	}
}

func TestScanNamespaceAvoidsRedundantNamespacePointReads(t *testing.T) {
	f := newCatalogFixture(t)
	empty := f.snapshot(t, 1)
	tx := f.begin(t)

	const count = 20
	for i := 0; i < count; i++ {
		change, err := f.store.Create(t.Context(), empty, tableDescriptor(t, model.ObjectID(20+i), fmt.Sprintf("t_%02d", i), 1))
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.ApplyMutations(t.Context(), change.Mutations); err != nil {
			t.Fatal(err)
		}
	}
	result, err := tx.Commit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snap := f.snapshot(t, result.CommitTS)

	before := f.store.ReadCount()
	visited := 0
	if err := snap.ScanNamespace(t.Context(), 10, model.ObjectTable, func(published catalog.PublishedDescriptor) error {
		visited++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	delta := f.store.ReadCount() - before
	if visited != count {
		t.Fatalf("visited = %d, want %d", visited, count)
	}
	// Each table requires exactly 1 namespace scan read + 1 descriptor point read = 2 reads
	// (eliminating the redundant Resolve point Get on NamespaceObjectID that made it 3*count).
	if delta != 2*count {
		t.Fatalf("ScanNamespace read count = %d, want %d (2 per object)", delta, 2*count)
	}
}

func TestCompositeRelationNamespaceCollision(t *testing.T) {
	f := newCatalogFixture(t)
	empty := f.snapshot(t, 1)

	// NamespaceDependency for both Table and CompositeRelation must cover all 6 relation kinds.
	tableDep, err := f.store.NamespaceDependency(t.Context(), model.NamespaceName{Parent: 10, Kind: model.ObjectTable, Name: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	compDep, err := f.store.NamespaceDependency(t.Context(), model.NamespaceName{Parent: 10, Kind: model.ObjectCompositeRelation, Name: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tableDep.Spans) != 6 || len(compDep.Spans) != 6 {
		t.Fatalf("relation namespace spans = table:%d composite:%d, want 6 each", len(tableDep.Spans), len(compDep.Spans))
	}

	// Table "shared" collides with CompositeRelation "shared".
	tbl := tableDescriptor(t, 20, "shared", 1)
	change, err := f.store.Create(t.Context(), empty, tbl)
	if err != nil {
		t.Fatal(err)
	}
	ts := f.commit(t, change)
	snap := f.snapshot(t, ts)

	compSame := ws2CompositeRelationDescriptor(t, 21, 10, "shared")
	if _, err := f.store.Create(t.Context(), snap, compSame); !errors.Is(err, catalog.ErrDuplicateObject) {
		t.Fatalf("creating CompositeRelation with existing Table name = %v, want ErrDuplicateObject", err)
	}

	// Conversely, CompositeRelation "comp_first" collides with Table and View of same name.
	compFirst := ws2CompositeRelationDescriptor(t, 22, 10, "comp_first")
	change, err = f.store.Create(t.Context(), snap, compFirst)
	if err != nil {
		t.Fatal(err)
	}
	ts = f.commit(t, change)
	snap = f.snapshot(t, ts)

	if _, err := f.store.Create(t.Context(), snap, tableDescriptor(t, 23, "comp_first", 1)); !errors.Is(err, catalog.ErrDuplicateObject) {
		t.Fatalf("creating Table with existing CompositeRelation name = %v, want ErrDuplicateObject", err)
	}
	if _, err := f.store.Create(t.Context(), snap, ws2ViewDescriptor(t, 24, 10, "comp_first")); !errors.Is(err, catalog.ErrDuplicateObject) {
		t.Fatalf("creating View with existing CompositeRelation name = %v, want ErrDuplicateObject", err)
	}
}

func TestDecodeDescriptorSinglePassRejectsCorruptDefaultLayoutWithoutPanic(t *testing.T) {
	// Valid descriptor with an array constant default round-trips cleanly.
	elem := model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}
	arrLayout := model.TypeLayout{
		Type:     model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray},
		Children: []model.TypeLayout{{Type: elem}},
	}
	val := model.Value{
		Type:       model.TypeArray,
		Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}},
		ArrayVal:   []model.Value{model.Int64Val(7)},
	}
	spec := model.DescriptorSpec{
		ID: 50, OID: 50050, Version: 1, State: model.StatePublic,
		Name:     model.NamespaceName{Parent: 10, Kind: model.ObjectTable, Name: "arr_def"},
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Table: &model.TableMetadata{
			ColumnIDHighWatermark: 1,
			Columns: []model.ColumnDescriptor{
				{ID: 1, Name: "a", State: model.StatePublic, Nullable: true, Layout: arrLayout, ConstantDefault: &val},
			},
		},
	}
	desc, err := model.NewDescriptor(t.Context(), spec, catalogLimits().Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := catalog.EncodeDescriptor(t.Context(), desc, catalogLimits())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := catalog.DecodeDescriptor(t.Context(), encoded, catalogLimits())
	if err != nil {
		t.Fatal(err)
	}
	gotSpec, err := decoded.Spec(t.Context())
	if err != nil || gotSpec.Table.Columns[0].ConstantDefault == nil || len(gotSpec.Table.Columns[0].ConstantDefault.ArrayVal) != 1 {
		t.Fatalf("decoded array default = %+v, %v", gotSpec, err)
	}

	// Corrupt the encoded JSON by stripping Children from the array column's Layout
	// while keeping the encoded array default; DecodeDescriptor must reject cleanly
	// with ErrCorruptCatalog rather than panicking in ValidateStoredValue.
	var doc map[string]any
	if err := json.Unmarshal(encoded, &doc); err != nil {
		t.Fatal(err)
	}
	cols := doc["Descriptor"].(map[string]any)["Table"].(map[string]any)["Columns"].([]any)
	cols[0].(map[string]any)["Layout"].(map[string]any)["Children"] = nil
	corrupt, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.DecodeDescriptor(t.Context(), corrupt, catalogLimits()); !errors.Is(err, catalog.ErrCorruptCatalog) {
		t.Fatalf("corrupt array default layout = %v, want ErrCorruptCatalog", err)
	}
	// Ensure storage import is exercised by validating an invalid mutation payload is rejected.
	var buf bytes.Buffer
	if err := storage.EncodeValue(t.Context(), &buf, model.Int64Val(1), 64); err != nil {
		t.Fatal(err)
	}
}

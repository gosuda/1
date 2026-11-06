package catalog_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func ownedDescriptor(t *testing.T, spec model.DescriptorSpec) *model.Descriptor {
	t.Helper()
	d, err := model.NewDescriptor(t.Context(), spec, catalogLimits().Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func editableDescriptor(t *testing.T, d *model.Descriptor) model.DescriptorSpec {
	t.Helper()
	spec, err := d.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return spec
}
func identityTable(t *testing.T) *model.Descriptor {
	t.Helper()
	spec := editableDescriptor(t, tableDescriptor(t, 20, "identities", 1))
	spec.Physical.Generation = 7
	spec.Table.ColumnIDHighWatermark = 3
	spec.Table.Columns = append(spec.Table.Columns, model.ColumnDescriptor{ID: 3, Name: "dropped", State: model.StatePublic, Layout: model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}})
	return ownedDescriptor(t, spec)
}
func TestDroppedColumnIDCannotBeReallocatedAfterReopen(t *testing.T) {
	f := newCatalogFixture(t)
	empty := f.snapshot(t, 1)
	original := identityTable(t)
	create, err := f.store.Create(t.Context(), empty, original)
	if err != nil {
		t.Fatal(err)
	}
	created := f.commit(t, create)
	before := f.snapshot(t, created)
	dropSpec := editableDescriptor(t, original)
	dropSpec.Version = 2
	dropSpec.Table.Columns = dropSpec.Table.Columns[:1]
	dropped := ownedDescriptor(t, dropSpec)
	change, err := f.store.Update(t.Context(), before, 1, dropped)
	if err != nil {
		t.Fatal(err)
	}
	dropTS := f.commit(t, change)
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.backend.Close(); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	snapshot := f.snapshot(t, dropTS)
	resolved, err := snapshot.Descriptor(t.Context(), 20)
	if err != nil {
		t.Fatal(err)
	}
	persisted := editableDescriptor(t, resolved.Descriptor)
	if persisted.Table.ColumnIDHighWatermark != 3 {
		t.Fatalf("removed column allocation history lost: watermark=%d", persisted.Table.ColumnIDHighWatermark)
	}
	for _, id := range []model.ColumnID{2, 3} {
		spec := editableDescriptor(t, resolved.Descriptor)
		spec.Version = 3
		spec.Table.Columns = append(spec.Table.Columns, model.ColumnDescriptor{ID: id, Name: "replacement", State: model.StatePublic, Layout: model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}})
		if _, err := f.store.Update(t.Context(), snapshot, 2, ownedDescriptor(t, spec)); !errors.Is(err, model.ErrInvalidDescriptor) {
			t.Fatalf("retired/reserved column ID %d reused: %v", id, err)
		}
	}
	decreased := editableDescriptor(t, resolved.Descriptor)
	decreased.Version = 3
	decreased.Table.ColumnIDHighWatermark = 1
	if _, err := f.store.Update(t.Context(), snapshot, 2, ownedDescriptor(t, decreased)); !errors.Is(err, model.ErrInvalidDescriptor) {
		t.Fatalf("allocation watermark decreased: %v", err)
	}
	renamedSpec := editableDescriptor(t, resolved.Descriptor)
	renamedSpec.Version = 3
	renamedSpec.Table.Columns[0].Name = "renamed_existing"
	renamed := ownedDescriptor(t, renamedSpec)
	rename, err := f.store.Update(t.Context(), snapshot, 2, renamed)
	if err != nil {
		t.Fatal(err)
	}
	renameTS := f.commit(t, rename)
	afterRename := f.snapshot(t, renameTS)
	freshSpec := editableDescriptor(t, renamed)
	freshSpec.Version = 4
	freshSpec.Table.ColumnIDHighWatermark = 4
	freshSpec.Table.Columns = append(freshSpec.Table.Columns, model.ColumnDescriptor{ID: 4, Name: "dropped", State: model.StatePublic, Layout: model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}})
	fresh := ownedDescriptor(t, freshSpec)
	add, err := f.store.Update(t.Context(), afterRename, 3, fresh)
	if err != nil {
		t.Fatal(err)
	}
	added := f.commit(t, add)
	final, err := f.snapshot(t, added).Descriptor(t.Context(), 20)
	if err != nil {
		t.Fatal(err)
	}
	finalSpec := editableDescriptor(t, final.Descriptor)
	if len(finalSpec.Table.Columns) != 2 || finalSpec.Table.Columns[0].ID != 1 || finalSpec.Table.Columns[0].Name != "renamed_existing" || finalSpec.Table.Columns[1].ID != 4 || finalSpec.Table.ColumnIDHighWatermark != 4 {
		t.Fatalf("fresh allocation or stable rename lost: %+v", finalSpec.Table)
	}
	historical, err := f.snapshot(t, created).Descriptor(t.Context(), 20)
	if err != nil {
		t.Fatal(err)
	}
	historicalSpec := editableDescriptor(t, historical.Descriptor)
	if historicalSpec.Table.Columns[1].ID != 3 {
		t.Fatal("historical column identity changed")
	}
}
func TestCatalogRejectsPhysicalGenerationRegression(t *testing.T) {
	f := newCatalogFixture(t)
	empty := f.snapshot(t, 1)
	original := identityTable(t)
	change, err := f.store.Create(t.Context(), empty, original)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := f.snapshot(t, f.commit(t, change))
	next := editableDescriptor(t, original)
	next.Version = 2
	next.Physical.Generation = 6
	if _, err := f.store.Update(t.Context(), snapshot, 1, ownedDescriptor(t, next)); !errors.Is(err, model.ErrInvalidDescriptor) {
		t.Fatalf("physical generation moved backward: %v", err)
	}
}
func TestCatalogLayoutChangesRequireFreshPhysicalGeneration(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*model.DescriptorSpec)
	}{
		{"column_type", func(spec *model.DescriptorSpec) {
			spec.Table.Columns[1].Layout.Type = model.Type{ID: 701, Storage: model.TypeFloat64}
		}},
		{"primary_key", func(spec *model.DescriptorSpec) {
			spec.Table.PrimaryKey = []model.KeyElement{{Column: 3, Type: spec.Table.Columns[1].Layout.Type, Descending: true}}
		}},
		{"encoding", func(spec *model.DescriptorSpec) { spec.Physical.EncodingVersion = 2 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCatalogFixture(t)
			empty := f.snapshot(t, 1)
			original := identityTable(t)
			create, err := f.store.Create(t.Context(), empty, original)
			if err != nil {
				t.Fatal(err)
			}
			created := f.commit(t, create)
			snapshot := f.snapshot(t, created)
			spec := editableDescriptor(t, original)
			spec.Version = 2
			test.change(&spec)
			if _, err := f.store.Update(t.Context(), snapshot, 1, ownedDescriptor(t, spec)); !errors.Is(err, model.ErrInvalidDescriptor) {
				t.Fatalf("layout replaced existing generation: %v", err)
			}
			spec.Physical.Generation = 8
			next := ownedDescriptor(t, spec)
			change, err := f.store.Update(t.Context(), snapshot, 1, next)
			if err != nil {
				t.Fatal(err)
			}
			updated := f.commit(t, change)
			before, err := snapshot.Descriptor(t.Context(), 20)
			if err != nil {
				t.Fatal(err)
			}
			after, err := f.snapshot(t, updated).Descriptor(t.Context(), 20)
			if err != nil {
				t.Fatal(err)
			}
			if before.Descriptor.Physical().Generation != 7 || after.Descriptor.Physical().Generation != 8 {
				t.Fatal("generation publication did not preserve historical identity")
			}
		})
	}
}

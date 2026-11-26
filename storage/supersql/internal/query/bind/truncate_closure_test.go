package bind

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type truncateClosureSnapshot struct {
	descriptors []*model.Descriptor
}

func (s truncateClosureSnapshot) Timestamp() model.Timestamp { return 1 }
func (s truncateClosureSnapshot) Resolve(ctx context.Context, name model.NamespaceName) (catalog.PublishedDescriptor, error) {
	for _, descriptor := range s.descriptors {
		if err := ctx.Err(); err != nil {
			return catalog.PublishedDescriptor{}, err
		}
		if descriptor.Name() == name {
			return catalog.PublishedDescriptor{Descriptor: descriptor}, nil
		}
	}
	return catalog.PublishedDescriptor{}, catalog.ErrUnknownObject
}
func (s truncateClosureSnapshot) Descriptor(ctx context.Context, id model.ObjectID) (catalog.PublishedDescriptor, error) {
	for _, descriptor := range s.descriptors {
		if err := ctx.Err(); err != nil {
			return catalog.PublishedDescriptor{}, err
		}
		if descriptor.ID() == id {
			return catalog.PublishedDescriptor{Descriptor: descriptor}, nil
		}
	}
	return catalog.PublishedDescriptor{}, catalog.ErrUnknownObject
}
func (s truncateClosureSnapshot) ScanNamespace(ctx context.Context, parent model.ObjectID, kind model.ObjectKind, visit func(catalog.PublishedDescriptor) error) error {
	for _, descriptor := range s.descriptors {
		if err := ctx.Err(); err != nil {
			return err
		}
		if descriptor.Name().Parent == parent && descriptor.Kind() == kind {
			if err := visit(catalog.PublishedDescriptor{Descriptor: descriptor}); err != nil {
				return err
			}
		}
	}
	return nil
}

func truncateClosureDescriptor(t *testing.T, spec model.DescriptorSpec) *model.Descriptor {
	t.Helper()
	descriptor, err := model.NewDescriptor(t.Context(), spec, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20})
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func truncateClosureTable(t *testing.T, id model.ObjectID, name string, foreign *model.ConstraintDescriptor) (*model.Descriptor, *model.Descriptor) {
	t.Helper()
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	key := model.KeyElement{Column: 1, Type: integer.Type}
	primary := model.ConstraintDescriptor{
		ID: id + 100, OID: model.OID(30000 + id), Name: name + "_pkey", Kind: model.ConstraintPrimaryKey,
		Columns: []model.ColumnID{1}, Index: id + 200, Validated: true, Enforced: true, Local: true,
	}
	metadata := &model.TableMetadata{
		ColumnIDHighWatermark: 1, Columns: []model.ColumnDescriptor{{ID: 1, Name: "id", State: model.StatePublic, Layout: integer, Nullable: false}},
		PrimaryKey: []model.KeyElement{key}, Indexes: []model.ObjectID{id + 200}, Constraints: []model.ConstraintDescriptor{primary},
	}
	if foreign != nil {
		metadata.Constraints = append(metadata.Constraints, *foreign)
	}
	table := truncateClosureDescriptor(t, model.DescriptorSpec{
		ID: id, OID: model.OID(10000 + id), Version: 1, State: model.StatePublic,
		Name:     model.NamespaceName{Parent: 10, Kind: model.ObjectTable, Name: name},
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}, Table: metadata,
	})
	index := truncateClosureDescriptor(t, model.DescriptorSpec{
		ID: id + 200, OID: model.OID(40000 + id), Version: 1, State: model.StatePublic,
		Name:         model.NamespaceName{Parent: 10, Kind: model.ObjectIndex, Name: name + "_pkey"},
		Physical:     model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Dependencies: []model.Dependency{{Object: id, Version: 1}, {Object: id, Version: 1, Column: 1}},
		Index:        &model.IndexMetadata{Owner: 1, Table: id, Keys: []model.KeyElement{key}, Unique: true},
	})
	return table, index
}

func truncateForeignKey(id, parent model.ObjectID, name string) model.ConstraintDescriptor {
	return model.ConstraintDescriptor{
		ID: id, OID: model.OID(20000 + id), Name: name, Kind: model.ConstraintForeignKey,
		Columns: []model.ColumnID{1}, ReferencedObject: parent, ReferencedColumns: []model.ColumnID{1},
		ReferencedConstraint: parent + 100, ReferencedIndex: parent + 200,
		ParentForeignEqualityOperators: []model.OperatorID{96}, ParentEqualityOperators: []model.OperatorID{96}, ForeignEqualityOperators: []model.OperatorID{96},
		Validated: true, Enforced: true, Local: true, OnDelete: "no action", OnUpdate: "no action", Match: "simple",
	}
}

func truncateClosureFixture(t *testing.T, cycle bool) (truncateClosureSnapshot, []*model.Descriptor) {
	t.Helper()
	schema := truncateClosureDescriptor(t, model.DescriptorSpec{ID: 10, OID: 10010, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "public"}, Schema: &model.SchemaMetadata{Owner: 1}})
	var firstForeign *model.ConstraintDescriptor
	if cycle {
		constraint := truncateForeignKey(101, 13, "a_to_c")
		firstForeign = &constraint
	}
	first, firstIndex := truncateClosureTable(t, 11, "a", firstForeign)
	secondConstraint := truncateForeignKey(102, 11, "b_to_a")
	second, secondIndex := truncateClosureTable(t, 12, "b", &secondConstraint)
	thirdConstraint := truncateForeignKey(103, 12, "c_to_b")
	third, thirdIndex := truncateClosureTable(t, 13, "c", &thirdConstraint)
	return truncateClosureSnapshot{descriptors: []*model.Descriptor{thirdIndex, third, schema, firstIndex, first, secondIndex, second}}, []*model.Descriptor{first, second, third}
}

func TestBindTruncateClosureRejectsExternalForeignKey(t *testing.T) {
	snapshot, tables := truncateClosureFixture(t, false)
	b := &binder{ctx: t.Context(), dependencySet: make(map[model.Dependency]struct{})}
	_, err := BindTruncateClosure(t.Context(), b, snapshot, tables[:1], false, 16)
	var sqlError *model.Error
	if !errors.As(err, &sqlError) || sqlError.Code != "2BP01" {
		t.Fatalf("error = %v, want 2BP01", err)
	}
}

func TestBindTruncateClosureExpandsTransitiveCycle(t *testing.T) {
	snapshot, tables := truncateClosureFixture(t, true)
	b := &binder{ctx: t.Context(), dependencySet: make(map[model.Dependency]struct{})}
	closure, err := BindTruncateClosure(t.Context(), b, snapshot, tables[:1], true, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(closure) != 3 || closure[0].ID() != 11 || closure[1].ID() != 12 || closure[2].ID() != 13 {
		t.Fatalf("closure = %v", closure)
	}
	for _, id := range []model.ObjectID{10, 11, 12, 13, 211, 212, 213} {
		if _, found := b.dependencySet[model.Dependency{Object: id, Version: 1}]; !found {
			t.Fatalf("dependency for descriptor %d is missing", id)
		}
	}
}

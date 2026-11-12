package catalog_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// TestTypeCascadeCeilingRejectsOverPopulation proves the walk's ceiling works
// through the store's configured limit: a store built with the production
// constructor whose descriptor item ceiling is below the namespace population
// fails the walk with 54000 before any dependency is retained. The ceiling is
// Limits.Descriptor.MaxItems, lowered only by building the store with a smaller
// value, never by editing a constant.
func TestTypeCascadeCeilingRejectsOverPopulation(t *testing.T) {
	f := newCatalogFixture(t)
	empty := f.snapshot(t, 1)
	transaction := f.begin(t)
	schema, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: 10, OID: 10010, Version: 1, State: model.StatePublic,
		Name:   model.NamespaceName{Kind: model.ObjectSchema, Name: "public"},
		Schema: &model.SchemaMetadata{Owner: 10},
	}, catalogLimits().Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	schemaChange, err := f.store.Create(t.Context(), empty, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.ApplyMutations(t.Context(), schemaChange.Mutations); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"a", "b", "c"} {
		change, err := f.store.Create(t.Context(), empty, tableDescriptor(t, model.ObjectID(30+i), name, 1))
		if err != nil {
			t.Fatal(err)
		}
		if err := transaction.ApplyMutations(t.Context(), change.Mutations); err != nil {
			t.Fatal(err)
		}
	}
	result, err := transaction.Commit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := f.snapshot(t, result.CommitTS)

	limits := catalogLimits()
	limits.Descriptor.MaxItems = 1
	small, err := catalog.New(byteEncoder{}, limits)
	if err != nil {
		t.Fatal(err)
	}
	_, err = small.TypeCascadeScanDependencies(t.Context(), snapshot)
	var sql *model.Error
	if !errors.As(err, &sql) || sql.Code != "54000" {
		t.Fatalf("walk over ceiling: %v, want 54000", err)
	}
}

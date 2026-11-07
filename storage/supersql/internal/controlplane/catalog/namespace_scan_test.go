package catalog_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestNamespaceScanBlocksCreationUntilTransactionEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newCatalogFixture(t)
		empty := f.snapshot(t, 1)
		inside := tableDescriptor(t, 20, "created", 1)
		create, err := f.store.Create(t.Context(), empty, inside)
		if err != nil {
			t.Fatal(err)
		}
		outsideSpec, err := tableDescriptor(t, 21, "neighbor", 1).Spec(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		outsideSpec.Name.Parent = 11
		outside, err := model.NewDescriptor(t.Context(), outsideSpec, catalogLimits().Descriptor)
		if err != nil {
			t.Fatal(err)
		}
		neighbor, err := f.store.Create(t.Context(), empty, outside)
		if err != nil {
			t.Fatal(err)
		}
		reader := f.begin(t)
		dependency, err := f.store.NamespaceScanDependency(t.Context(), 10, model.ObjectTable)
		if err != nil {
			t.Fatal(err)
		}
		view, err := reader.ReadView(t.Context(), []txn.ReadDependency{dependency})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := f.store.TransactionSnapshot(view, catalog.OwnReader)
		if err != nil {
			t.Fatal(err)
		}
		if err := snapshot.ScanNamespace(t.Context(), 10, model.ObjectTable, func(catalog.PublishedDescriptor) error {
			t.Fatal("empty namespace returned a descriptor")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.Close(); err != nil {
			t.Fatal(err)
		}
		writer := f.begin(t)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			defer close(done)
			done <- writer.ApplyMutations(ctx, create.Mutations)
		}()
		defer func() {
			cancel()
			for range done {
			}
		}()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("creation passed a retained empty namespace read: %v", err)
		default:
		}
		// A neighboring parent must remain writable while this range is protected.
		neighborCommit := f.commit(t, neighbor)
		if _, err := reader.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		result, err := writer.Commit(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		current := f.snapshot(t, result.CommitTS)
		var names []string
		if err := current.ScanNamespace(t.Context(), 10, model.ObjectTable, func(published catalog.PublishedDescriptor) error {
			names = append(names, published.Descriptor.Name().Name)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(names) != 1 || names[0] != "created" {
			t.Fatalf("namespace after creation = %v, want [created]", names)
		}
		requireResolved(t, current, outside.Name(), outside.ID(), outside.Version(), neighborCommit)
	})
}

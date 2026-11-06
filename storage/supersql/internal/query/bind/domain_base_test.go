package bind_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// countingSnapshot wraps metadataSnapshot and counts catalog namespace scans,
// so tests can assert that declared-type resolution is memoized rather than
// repeated per overload candidate.
type countingSnapshot struct {
	metadataSnapshot
	scans int
}

func (s *countingSnapshot) ScanNamespace(ctx context.Context, parent model.ObjectID, kind model.ObjectKind, visit func(catalog.PublishedDescriptor) error) error {
	s.scans++
	return s.metadataSnapshot.ScanNamespace(ctx, parent, kind, visit)
}

func domainSnapshot(t *testing.T) metadataSnapshot {
	t.Helper()
	snapshot := tableSnapshot(t)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	domainLayout := model.TypeLayout{Type: model.Type{ID: 90040, Modifier: -1, Storage: model.TypeInt64}}
	domain := testDescriptor(t, model.DescriptorSpec{
		ID: 40, OID: 90040, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "positive_int"},
		Type: &model.TypeMetadata{Kind: model.TypeKindDomain, Layout: domainLayout, Array: 41, BaseLayout: integer},
	})
	tableSpec, err := snapshot.descriptors[1].SpecWithin(t.Context(), model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20})
	if err != nil {
		t.Fatal(err)
	}
	tableSpec.Table.Columns[0].Layout = domainLayout
	snapshot.descriptors[1] = testDescriptor(t, tableSpec)
	snapshot.descriptors = append(snapshot.descriptors, domain)
	return snapshot
}

// Overload scoring consults the domain base of every argument for every
// candidate. With a memoized catalog lookup, a domain-typed operand resolves
// its descriptor once for the whole binding pass; builtin argument types must
// never reach the catalog at all.
func TestDomainBaseMemoizesCatalogResolution(t *testing.T) {
	sql := `SELECT "X" + "X" FROM "Mixed"`
	counting := &countingSnapshot{metadataSnapshot: domainSnapshot(t)}
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bind.Bind(t.Context(), parsed.Stmts[0], counting, bindingOptions(sql), nil); err != nil {
		t.Fatal(err)
	}
	// One miss resolves the domain: a schema scan plus a type scan. Every
	// other lookup (both domain arguments across all + candidates, and every
	// builtin target type) must hit the binder cache or the builtin fast path.
	if scans := counting.scans; scans > 8 {
		t.Fatalf("namespace scans during domain operator bind = %d, want <= 8", scans)
	}
}

var errScanFailed = errors.New("injected catalog scan failure")

// errorSnapshot fails catalog type-namespace scans: the domain cast itself
// resolves by qualified name, while overload scoring hits the injected error
// through findNamedType.
type errorSnapshot struct {
	metadataSnapshot
}

func (s *errorSnapshot) ScanNamespace(ctx context.Context, parent model.ObjectID, kind model.ObjectKind, visit func(catalog.PublishedDescriptor) error) error {
	if kind == model.ObjectType {
		return errScanFailed
	}
	return s.metadataSnapshot.ScanNamespace(ctx, parent, kind, visit)
}

// A catalog failure inside overload scoring must surface as itself rather
// than degrading to a misleading "operator does not exist" diagnostic.
func TestDomainBasePropagatesCatalogError(t *testing.T) {
	sql := `SELECT "X" + "X" FROM "Mixed"`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	_, err = bind.Bind(t.Context(), parsed.Stmts[0], &errorSnapshot{metadataSnapshot: domainSnapshot(t)}, bindingOptions(sql), nil)
	if !errors.Is(err, errScanFailed) {
		t.Fatalf("error = %v, want catalog scan failure", err)
	}
}

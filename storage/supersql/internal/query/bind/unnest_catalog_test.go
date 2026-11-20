package bind_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestBindUnnestNamedCompositeExpandsPublicFields(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	text := model.TypeLayout{Type: model.Type{ID: 1043, Modifier: 12, Storage: model.TypeString, Collation: 950}}
	// Attribute IDs stay sparse so the dependency assertions below still pin the
	// descriptors the expansion actually reads.
	fields := []model.ColumnDescriptor{
		{ID: 1, Name: "number", State: model.StatePublic, Layout: integer},
		{ID: 3, Name: "label", State: model.StatePublic, Layout: text},
	}
	snapshot, array := unnestNamedSnapshot(t, fields, []model.TypeLayout{integer, text})
	for _, tc := range []struct {
		sql   string
		names []string
	}{
		{`SELECT * FROM unnest($1) AS u`, []string{"number", "label"}},
		{`SELECT u.n,u.label,u.seq FROM unnest($1) WITH ORDINALITY AS u(n,label,seq)`, []string{"n", "label", "seq"}},
	} {
		statement, err := bindSQL(t, tc.sql, snapshot, []model.TypeLayout{array})
		if err != nil {
			t.Fatal(err)
		}
		columns := statement.Columns()
		if len(columns) != len(tc.names) {
			t.Fatalf("columns = %+v, want %v", columns, tc.names)
		}
		for i, name := range tc.names {
			if columns[i].Name != name || columns[i].Nullable != (i < 2) {
				t.Errorf("column %d = %+v, want %s", i, columns[i], name)
			}
		}
		requireArrayAggregateLayout(t, columns[0].Layout, integer)
		requireArrayAggregateLayout(t, columns[1].Layout, text)
		dependencies := statement.Dependencies()
		for _, want := range []model.Dependency{{Object: 10, Version: 1}, {Object: 30, Version: 3}, {Object: 30, Version: 3, Column: 1}, {Object: 30, Version: 3, Column: 3}} {
			found := false
			for _, dependency := range dependencies {
				found = found || dependency == want
			}
			if !found {
				t.Errorf("dependencies = %+v, missing %+v", dependencies, want)
			}
		}
	}
	_, err := bindSQL(t, `SELECT * FROM unnest($1) AS u(a int,b text)`, snapshot, []model.TypeLayout{array})
	requireCode(t, err, "42601")
}

func TestBindUnnestEmptyNamedComposite(t *testing.T) {
	snapshot, array := unnestNamedSnapshot(t, nil, nil)
	statement, err := bindSQL(t, `SELECT * FROM unnest($1) WITH ORDINALITY AS u(n)`, snapshot, []model.TypeLayout{array})
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 1 || columns[0].Name != "n" || columns[0].Nullable || columns[0].Layout.Type.ID != 20 {
		t.Fatalf("empty composite schema = %+v", columns)
	}
}

func TestBindUnnestRejectsInconsistentNamedCompositeLayouts(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 950}}
	// Descriptor and argument layout are independent inputs. A descriptor the
	// catalog could hold is still rejected when the element the caller passes
	// carries different attribute layouts.
	snapshot, array := unnestNamedSnapshot(t, []model.ColumnDescriptor{{ID: 1, Name: "wrong_type", State: model.StatePublic, Layout: integer}}, []model.TypeLayout{integer})
	array.Children[0].Children = []model.TypeLayout{text}
	_, err := bindSQL(t, `SELECT * FROM unnest($1)`, snapshot, []model.TypeLayout{array})
	requireCode(t, err, "XX000")
}

// TestNamedCompositeDescriptorRequiresAttributeParity pins the invariant that
// replaced the tombstoned-attribute fixtures: a composite declaration is only
// well-formed when every attribute has exactly one physical layout child.
func TestNamedCompositeDescriptorRequiresAttributeParity(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	for _, tc := range []struct {
		name     string
		fields   []model.ColumnDescriptor
		children []model.TypeLayout
	}{
		{"tombstoned attribute with no layout slot", []model.ColumnDescriptor{{ID: 1, Name: "dropped", State: model.StateDropDeleteOnly, Layout: integer}}, nil},
		{"attribute layout disagrees with physical child", []model.ColumnDescriptor{{ID: 1, Name: "wrong_type", State: model.StatePublic, Layout: integer}}, []model.TypeLayout{{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 950}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := model.TypeLayout{Type: model.Type{ID: 90000, Storage: model.TypeComposite, Modifier: -1}, Children: tc.children}
			_, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
				ID: 30, OID: 90000, Version: 3, State: model.StatePublic,
				Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "record_type"},
				Type: &model.TypeMetadata{Kind: model.TypeKindComposite, Layout: record, Array: 31, Fields: tc.fields},
			}, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20})
			if !errors.Is(err, model.ErrInvalidDescriptor) {
				t.Fatalf("composite declaration accepted: %v", err)
			}
		})
	}
}

func TestBindUnnestSkipsUnrelatedDescriptorPayloads(t *testing.T) {
	snapshot, array := unnestNamedSnapshot(t, nil, nil)
	unrelated := testDescriptor(t, model.DescriptorSpec{ID: 20, OID: 80000, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "unrelated"}, Type: &model.TypeMetadata{Kind: model.TypeKindEnum, Layout: model.TypeLayout{Type: model.Type{ID: 80000, Modifier: -1, Storage: model.TypeInt64}}, Array: 21, Elements: []string{strings.Repeat("x", 128<<10)}, ElementOIDs: []model.OID{80001}}})
	snapshot.descriptors = append(snapshot.descriptors[:1], unrelated, snapshot.descriptors[1])
	sql := `SELECT * FROM unnest($1) WITH ORDINALITY`
	options := bindingOptions(sql)
	options.Limits.MaxBytes = 64 << 10
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	statement, err := bind.Bind(t.Context(), parsed.Stmts[0], snapshot, options, []model.TypeLayout{array})
	if err != nil {
		t.Fatal(err)
	}
	if columns := statement.Columns(); len(columns) != 1 || columns[0].Layout.Type.ID != 20 {
		t.Fatalf("columns = %+v", columns)
	}
}

func TestBindUnnestBoundsCatalogTraversal(t *testing.T) {
	snapshot, array := unnestNamedSnapshot(t, nil, nil)
	for i := range 50 {
		snapshot.descriptors = append(snapshot.descriptors[:len(snapshot.descriptors)-1], testDescriptor(t, model.DescriptorSpec{ID: model.ObjectID(100 + i), OID: model.OID(10000 + i), Version: 1, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: fmt.Sprintf("unrelated%d", i)}, Type: &model.TypeMetadata{Kind: model.TypeKindEnum, Layout: model.TypeLayout{Type: model.Type{ID: model.TypeID(10000 + i), Modifier: -1, Storage: model.TypeInt64}}, Array: model.ObjectID(1000 + i)}}), snapshot.descriptors[len(snapshot.descriptors)-1])
	}
	sql := `SELECT * FROM unnest($1)`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	options := bindingOptions(sql)
	options.Limits.MaxOperands = 40
	_, err = bind.Bind(t.Context(), parsed.Stmts[0], snapshot, options, []model.TypeLayout{array})
	requireCode(t, err, "54000")
}

type unnestCancelSnapshot struct {
	metadataSnapshot
	cancel context.CancelFunc
}

func (s unnestCancelSnapshot) ScanNamespace(ctx context.Context, parent model.ObjectID, kind model.ObjectKind, visit func(catalog.PublishedDescriptor) error) error {
	if kind == model.ObjectType {
		s.cancel()
	}
	return s.metadataSnapshot.ScanNamespace(ctx, parent, kind, visit)
}

func TestBindUnnestCatalogTraversalCancellation(t *testing.T) {
	snapshot, array := unnestNamedSnapshot(t, nil, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sql := `SELECT * FROM unnest($1)`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	_, err = bind.Bind(ctx, parsed.Stmts[0], unnestCancelSnapshot{snapshot, cancel}, bindingOptions(sql), []model.TypeLayout{array})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("binding error = %v, want cancellation during catalog traversal", err)
	}
}

func unnestNamedSnapshot(t *testing.T, fields []model.ColumnDescriptor, children []model.TypeLayout) (metadataSnapshot, model.TypeLayout) {
	t.Helper()
	record := model.TypeLayout{Type: model.Type{ID: 90000, Storage: model.TypeComposite, Modifier: -1}, Children: children}
	schema := testDescriptor(t, model.DescriptorSpec{ID: 10, OID: 10010, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "outside_search_path"}, Schema: &model.SchemaMetadata{Owner: 10}})
	typ := testDescriptor(t, model.DescriptorSpec{ID: 30, OID: 90000, Version: 3, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "record_type"}, Type: &model.TypeMetadata{Kind: model.TypeKindComposite, Layout: record, Array: 31, Fields: fields}})
	array := model.TypeLayout{Type: model.Type{ID: 90001, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{record}}
	return metadataSnapshot{descriptors: []*model.Descriptor{schema, typ}}, array
}

type unnestFailingScanSnapshot struct {
	metadataSnapshot
	failure error
}

func (s unnestFailingScanSnapshot) ScanNamespace(ctx context.Context, parent model.ObjectID, kind model.ObjectKind, visit func(catalog.PublishedDescriptor) error) error {
	return errors.Join(s.metadataSnapshot.ScanNamespace(ctx, parent, kind, visit), s.failure)
}

func TestBindUnnestRetainsCatalogFailureAfterFindingType(t *testing.T) {
	snapshot, array := unnestNamedSnapshot(t, nil, nil)
	failure := errors.New("catalog scan cleanup failed")
	_, err := bindSQL(t, `SELECT * FROM unnest($1) WITH ORDINALITY`, unnestFailingScanSnapshot{snapshot, failure}, []model.TypeLayout{array})
	if !errors.Is(err, failure) {
		t.Fatalf("binding error = %v, want catalog cleanup failure", err)
	}
}

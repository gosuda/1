package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestDeclaredCompositeAndRangeConstructorsBindFromSQL(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	schema := testDescriptor(t, model.DescriptorSpec{
		ID: 10, OID: 10010, Version: 1, State: model.StatePublic,
		Name:   model.NamespaceName{Kind: model.ObjectSchema, Name: "outside_search_path"},
		Schema: &model.SchemaMetadata{Owner: 10},
	})
	compositeLayout := model.TypeLayout{
		Type:     model.Type{ID: 90020, Modifier: -1, Storage: model.TypeComposite},
		Children: []model.TypeLayout{integer, integer},
	}
	composite := testDescriptor(t, model.DescriptorSpec{
		ID: 20, OID: 90020, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "pair"},
		Type: &model.TypeMetadata{
			Kind: model.TypeKindComposite, Layout: compositeLayout, Array: 21, FieldIDHighWatermark: 2,
			Fields: []model.ColumnDescriptor{
				{ID: 1, Name: "left", State: model.StatePublic, Layout: integer, Nullable: true},
				{ID: 2, Name: "right", State: model.StatePublic, Layout: integer, Nullable: true},
			},
		},
	})
	rangeLayout := model.TypeLayout{Type: model.Type{ID: 90030, Modifier: -1, Storage: model.TypeBytes}}
	multiLayout := model.TypeLayout{Type: model.Type{ID: 90031, Modifier: -1, Storage: model.TypeBytes}}
	rangeArrayLayout := model.TypeLayout{Type: model.Type{ID: 90032, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{rangeLayout}}
	enumLayout := model.TypeLayout{Type: model.Type{ID: 90025, Modifier: -1, Storage: model.TypeInt64}}
	enum := testDescriptor(t, model.DescriptorSpec{
		ID: 25, OID: 90025, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "color"},
		Type: &model.TypeMetadata{Kind: model.TypeKindEnum, Layout: enumLayout, Array: 26, Elements: []string{"red", "blue"}, ElementOIDs: []model.OID{92001, 92002}},
	})
	family := &model.RangeTypeMetadata{Subtype: integer, OperatorClass: 1978, Range: 30, Multirange: 31}
	rangeType := testDescriptor(t, model.DescriptorSpec{
		ID: 30, OID: 90030, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "stepped"},
		Type: &model.TypeMetadata{Kind: model.TypeKindRange, Layout: rangeLayout, Array: 32, Range: family},
	})
	multirangeType := testDescriptor(t, model.DescriptorSpec{
		ID: 31, OID: 90031, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "stepped_multirange"},
		Type: &model.TypeMetadata{Kind: model.TypeKindMultirange, Layout: multiLayout, Array: 33, Range: family},
	})
	rangeArray := testDescriptor(t, model.DescriptorSpec{
		ID: 32, OID: 90032, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "_stepped"},
		Type: &model.TypeMetadata{Kind: model.TypeKindArray, Layout: rangeArrayLayout, Base: 30, BaseLayout: rangeLayout},
	})
	snapshot := metadataSnapshot{descriptors: []*model.Descriptor{schema, composite, enum, rangeType, multirangeType, rangeArray}}
	for _, test := range []struct {
		sql  string
		want model.TypeLayout
	}{
		{`SELECT outside_search_path.pair(1, 2)`, compositeLayout},
		{`SELECT outside_search_path.stepped(1, 5)`, rangeLayout},
		{`SELECT outside_search_path.stepped_multirange(outside_search_path.stepped(1, 5), outside_search_path.stepped(8, 13))`, multiLayout},
		{`SELECT '(1,2)'::outside_search_path.pair`, compositeLayout},
		{`SELECT '[1,5)'::outside_search_path.stepped`, rangeLayout},
		{`SELECT ('red'::outside_search_path.color)::text`, model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 100}}},
	} {
		statement, err := bindSQL(t, test.sql, snapshot, nil)
		if err != nil {
			t.Fatal(err)
		}
		if columns := statement.Columns(); len(columns) != 1 || columns[0].Layout.Type != test.want.Type {
			t.Fatalf("%s layout = %+v", test.sql, columns)
		}
	}
}

func TestInsertIntoDomainColumnBindsDomainInput(t *testing.T) {
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
	// A non-constant source must be coerced by the domain's input function at
	// execution time; a literal is folded during binding, as PostgreSQL folds
	// constants, so it leaves no domain_in scalar behind.
	statement, err := bindSQL(t, `INSERT INTO public."Mixed" ("X") SELECT 7 + 0`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for id := ir.ScalarID(1); uint32(id) <= statement.Graph().ScalarCount(); id++ {
		scalar, _ := statement.Graph().Scalar(id)
		found = found || scalar.Function == 2597
	}
	if !found {
		t.Fatal("domain column write bypassed domain input")
	}
}

func TestShellTypeCompletesAsRangeWithStableIdentity(t *testing.T) {
	schema := testDescriptor(t, model.DescriptorSpec{
		ID: 10, OID: 10010, Version: 1, State: model.StatePublic,
		Name:   model.NamespaceName{Kind: model.ObjectSchema, Name: "outside_search_path"},
		Schema: &model.SchemaMetadata{Owner: 10},
	})
	options := func(sql string, next model.ObjectID) bind.CreateDeclarationOptions {
		return bind.CreateDeclarationOptions{
			Bind:        bindingOptions(sql),
			Descriptors: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20},
			Identities:  &declarationIdentities{next: next},
			Owner:       (model.Principal{}).OID(),
		}
	}
	shellSQL := `CREATE TYPE outside_search_path.stepped`
	parsed, err := zpgparse.Parse(shellSQL)
	if err != nil {
		t.Fatal(err)
	}
	shellCommand, handled, err := bind.BindTypeCommand(t.Context(), parsed.Stmts[0].Stmt, metadataSnapshot{descriptors: []*model.Descriptor{schema}}, options(shellSQL, 100))
	if err != nil || !handled || len(shellCommand.Creates()) != 1 {
		t.Fatalf("shell command = %+v, %v, %v", shellCommand, handled, err)
	}
	shell := shellCommand.Creates()[0]
	rangeSQL := `CREATE TYPE outside_search_path.stepped AS RANGE (subtype = integer)`
	parsed, err = zpgparse.Parse(rangeSQL)
	if err != nil {
		t.Fatal(err)
	}
	rangeCommand, handled, err := bind.BindTypeCommand(t.Context(), parsed.Stmts[0].Stmt, metadataSnapshot{descriptors: []*model.Descriptor{schema, shell}}, options(rangeSQL, 200))
	if err != nil || !handled || len(rangeCommand.Updates()) != 1 || len(rangeCommand.Creates()) != 3 {
		t.Fatalf("range completion = %+v, %v, %v", rangeCommand, handled, err)
	}
	update := rangeCommand.Updates()[0]
	if update.Base.ID() != update.Next.ID() || update.Base.OID() != update.Next.OID() {
		t.Fatal("range completion changed shell identity")
	}
	if err := update.Base.ValidateSuccessor(t.Context(), update.Next); err != nil {
		t.Fatal(err)
	}
}

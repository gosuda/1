package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// dropTypeFixture returns a public schema and two public enums, the minimum
// population for DROP TYPE of one name, several names, and a dependent row.
func dropTypeFixture(t *testing.T) (*model.Descriptor, []*model.Descriptor) {
	t.Helper()
	schema := testDescriptor(t, model.DescriptorSpec{
		ID: 10, OID: 10010, Version: 1, State: model.StatePublic,
		Name:   model.NamespaceName{Kind: model.ObjectSchema, Name: "public"},
		Schema: &model.SchemaMetadata{Owner: 10},
	})
	layout := model.TypeLayout{Type: model.Type{ID: 90025, Modifier: -1, Storage: model.TypeInt64}}
	d := testDescriptor(t, model.DescriptorSpec{
		ID: 25, OID: 90025, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "d"},
		Type: &model.TypeMetadata{Kind: model.TypeKindEnum, Layout: layout, Array: 26, Elements: []string{"a"}, ElementOIDs: []model.OID{92001}},
	})
	layout2 := model.TypeLayout{Type: model.Type{ID: 90027, Modifier: -1, Storage: model.TypeInt64}}
	e := testDescriptor(t, model.DescriptorSpec{
		ID: 27, OID: 90027, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "e"},
		Type: &model.TypeMetadata{Kind: model.TypeKindEnum, Layout: layout2, Array: 28, Elements: []string{"x"}, ElementOIDs: []model.OID{92701}},
	})
	// Array companions are owned by their base type; the drop closure fetches
	// them by id, so the fixture publishes them as well.
	arrayD := testDescriptor(t, model.DescriptorSpec{
		ID: 26, OID: 90026, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "_d"},
		Type: &model.TypeMetadata{Kind: model.TypeKindArray, Layout: arrayLayout(layout), Base: 25, BaseLayout: layout},
	})
	arrayE := testDescriptor(t, model.DescriptorSpec{
		ID: 28, OID: 90028, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "_e"},
		Type: &model.TypeMetadata{Kind: model.TypeKindArray, Layout: arrayLayout(layout2), Base: 27, BaseLayout: layout2},
	})
	return schema, []*model.Descriptor{d, e, arrayD, arrayE}
}

func arrayLayout(element model.TypeLayout) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: element.Type.ID + 1, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{element}}
}

func bindDrop(t *testing.T, sql string, descriptors []*model.Descriptor) (*bind.BoundTypeCommand, bool, error) {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	options := bind.CreateDeclarationOptions{
		Bind:        bindingOptions(sql),
		Descriptors: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20},
		Identities:  &declarationIdentities{next: 100},
		Owner:       (model.Principal{}).OID(),
	}
	return bind.BindTypeCommand(t.Context(), parsed.Stmts[0].Stmt, metadataSnapshot{descriptors: descriptors}, options)
}

// TestDropTypeBindsEveryNameForm is the failing-before test for the rejected
// "invalid DROP TYPE name": the parser emits a TypeName node per object, which
// the binder used to reject because it only accepted a bare name list.
func TestDropTypeBindsEveryNameForm(t *testing.T) {
	schema, descriptors := dropTypeFixture(t)
	all := append([]*model.Descriptor{schema}, descriptors...)
	for _, sql := range []string{
		`DROP TYPE d`,
		`DROP TYPE public.d`,
		`DROP TYPE IF EXISTS d`,
		`DROP TYPE d CASCADE`,
		`DROP TYPE d RESTRICT`,
		`DROP TYPE d, e`,
		`DROP TYPE IF EXISTS d, missing CASCADE`,
	} {
		command, handled, err := bindDrop(t, sql, all)
		if err != nil || !handled {
			t.Fatalf("%q: handled=%v err=%v", sql, handled, err)
		}
		if command == nil {
			t.Fatalf("%q: nil command", sql)
		}
	}
}

// TestDropTypeDropsEveryNamedType checks that a multi-name DROP collects each
// named descriptor exactly once, and that IF EXISTS skips a missing name with a
// notice rather than failing.
func TestDropTypeDropsEveryNamedType(t *testing.T) {
	schema, descriptors := dropTypeFixture(t)
	all := append([]*model.Descriptor{schema}, descriptors...)
	command, _, err := bindDrop(t, `DROP TYPE IF EXISTS d, missing, e`, all)
	if err != nil {
		t.Fatal(err)
	}
	// Each enum drops with its array companion; the closure is exactly
	// {d, _d, e, _e}, each once.
	drops := command.Drops()
	seen := make(map[model.ObjectID]int)
	for _, drop := range drops {
		seen[drop.ID()]++
	}
	if len(drops) != 4 || seen[25] != 1 || seen[26] != 1 || seen[27] != 1 || seen[28] != 1 {
		t.Fatalf("drops = %v, want {25,26,27,28} once each", seen)
	}
	if len(command.Warnings()) != 1 || command.Warnings()[0].Code != "42704" {
		t.Fatalf("warnings = %+v, want one 42704 notice", command.Warnings())
	}
}

// TestDropTypeMissingWithoutIfExistsFails keeps the non-IF EXISTS error: a
// missing name is an undefined-object error, not a skipped notice.
func TestDropTypeMissingWithoutIfExistsFails(t *testing.T) {
	schema, descriptors := dropTypeFixture(t)
	all := append([]*model.Descriptor{schema}, descriptors...)
	_, _, err := bindDrop(t, `DROP TYPE missing`, all)
	if err == nil {
		t.Fatal("DROP TYPE missing bound without error")
	}
}

package catalog_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type rejectingTextSearchIdentities struct{ t *testing.T }

func (i rejectingTextSearchIdentities) AllocateDeclarationIdentity(context.Context) (model.ObjectID, model.OID, error) {
	i.t.Helper()
	i.t.Fatal("SET SCHEMA allocated a catalog identity")
	return 0, 0, model.ErrInvalidDescriptor
}

func bindTextSearchSchemaMove(t *testing.T, snapshot *catalog.CatalogSnapshot, sql string) *bind.BoundObjectCommand {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	options := bind.CreateDeclarationOptions{
		Bind: bind.Options{
			SearchPath:       []string{"public"},
			Collation:        950,
			DefaultCollation: 950,
			Limits: ir.Limits{
				MaxNodes:    10000,
				MaxOperands: 20000,
				MaxSlots:    10000,
				MaxDepth:    100,
				MaxBytes:    16 << 20,
				SourceBytes: uint32(len(sql)),
			},
		},
		Descriptors: catalogLimits().Descriptor,
		Identities:  rejectingTextSearchIdentities{t: t},
		Owner:       10,
	}
	command, handled, err := bind.BindObjectCommand(t.Context(), parsed.Stmts[0].Stmt, snapshot, options)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("text search SET SCHEMA was not routed")
	}
	return command
}

func TestTextSearchSetSchemaAppliesAndRollsBackForBothObjectFamilies(t *testing.T) {
	fixture := newCatalogFixture(t)
	limits := catalogLimits().Descriptor
	descriptor := func(spec model.DescriptorSpec) *model.Descriptor {
		d, err := model.NewDescriptor(t.Context(), spec, limits)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	public := descriptor(model.DescriptorSpec{
		ID: 10, OID: 10010, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "public"},
		Schema: &model.SchemaMetadata{Owner: 10},
	})
	alternate := descriptor(model.DescriptorSpec{
		ID: 12, OID: 10012, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "alternate"},
		Schema: &model.SchemaMetadata{Owner: 10},
	})
	dictionary := descriptor(model.DescriptorSpec{
		ID: 41, OID: 20041, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Parent: public.ID(), Kind: model.ObjectTextSearchDictionary, Name: "words"},
		Dependencies: []model.Dependency{{Object: public.ID(), Version: public.Version()}},
		TextSearchDictionary: &model.TextSearchDictionaryMetadata{Owner: 10, Template: 3727, Kind: model.TextSearchSimpleDictionary, Accept: true},
	})
	configuration := descriptor(model.DescriptorSpec{
		ID: 42, OID: 20042, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Parent: public.ID(), Kind: model.ObjectTextSearchConfiguration, Name: "search"},
		Dependencies: []model.Dependency{{Object: public.ID(), Version: public.Version()}},
		TextSearchConfiguration: &model.TextSearchConfigurationMetadata{Owner: 10, Parser: 3722},
	})

	empty := fixture.snapshot(t, 1)
	var creates []catalog.Change
	for _, object := range []*model.Descriptor{public, alternate, dictionary, configuration} {
		change, err := fixture.store.Create(t.Context(), empty, object)
		if err != nil {
			t.Fatal(err)
		}
		creates = append(creates, change)
	}
	committedAt := fixture.commit(t, creates...)
	base := fixture.snapshot(t, committedAt)
	transaction := fixture.begin(t)
	var mutations []catalog.Change
	for _, sql := range []string{
		"ALTER TEXT SEARCH DICTIONARY public.words SET SCHEMA alternate",
		"ALTER TEXT SEARCH CONFIGURATION public.search SET SCHEMA alternate",
	} {
		command := bindTextSearchSchemaMove(t, base, sql)
		for _, update := range command.Updates() {
			change, err := fixture.store.Update(t.Context(), base, update.Base.Version(), update.Next)
			if err != nil {
				t.Fatal(err)
			}
			mutations = append(mutations, change)
		}
	}
	for _, change := range mutations {
		if err := transaction.ApplyMutations(t.Context(), change.Mutations); err != nil {
			t.Fatal(err)
		}
	}
	staged := fixture.transactionSnapshot(t, transaction)
	for _, object := range []*model.Descriptor{dictionary, configuration} {
		moved := object.Name()
		moved.Parent = alternate.ID()
		requireResolved(t, staged, moved, object.ID(), 2, 0)
		if _, err := staged.Resolve(t.Context(), object.Name()); !errors.Is(err, catalog.ErrUnknownObject) {
			t.Fatalf("old namespace still resolves after staging %s: %v", object.Name().Name, err)
		}
	}
	if err := transaction.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	afterRollback := fixture.snapshot(t, committedAt)
	for _, object := range []*model.Descriptor{dictionary, configuration} {
		requireResolved(t, afterRollback, object.Name(), object.ID(), 1, committedAt)
		moved := object.Name()
		moved.Parent = alternate.ID()
		if _, err := afterRollback.Resolve(t.Context(), moved); !errors.Is(err, catalog.ErrUnknownObject) {
			t.Fatalf("rolled-back namespace resolves %s: %v", object.Name().Name, err)
		}
	}
}

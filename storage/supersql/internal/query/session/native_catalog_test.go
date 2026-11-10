package session

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func testCatalogStore(t *testing.T) *catalog.Store {
	t.Helper()
	store, err := catalog.New(testKeyEncoder{}, catalog.Limits{
		Descriptor:      model.DescriptorLimits{MaxBytes: 1 << 16, MaxItems: 8, MaxDepth: 8},
		MaxEncodedBytes: 1 << 16,
		MaxKeyBytes:     1 << 12,
		ScanBatchRows:   8,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

type testKeyEncoder struct{}

func (testKeyEncoder) EncodePrefix(_ context.Context, _ []storage.KeyColumn, values []model.Value, _ uint64) ([]byte, error) {
	if len(values) != 1 || values[0].Type != model.TypeBytes {
		return nil, storage.ErrInvalidLayout
	}
	return append([]byte(nil), values[0].BytesVal...), nil
}

func nativeCatalogTestManifest() catalogrel.NativeCatalogManifest {
	next := model.OID(600000)
	allocate := func() model.OID {
		value := next
		next++
		return value
	}
	manifest := catalogrel.NativeCatalogManifest{
		PgCatalogObjectID: 800000,
		InformationSchema: catalogrel.NamespaceIdentity{ObjectID: 800001, OID: 800002},
		Config: catalogrel.RegistryConfig{
			Relations:                  make(map[catalogrel.RelationID]catalogrel.RelationIdentity),
			InformationDomains:         make(map[builtin.InformationDomainKind]catalogrel.InformationDomainIdentity),
			InformationSchemaFunctions: make(map[builtin.InformationSchemaFunction]model.OID),
		},
	}
	for _, relation := range catalogrel.All() {
		identity := relation.Identity
		if identity.RelationOID == 0 {
			identity.RelationOID = allocate()
		}
		if identity.RowTypeOID == 0 {
			identity.RowTypeOID = allocate()
		}
		if identity.ArrayTypeOID == 0 {
			identity.ArrayTypeOID = allocate()
		}
		manifest.Config.Relations[relation.ID] = identity
	}
	for _, domain := range builtin.InformationDomains() {
		identity := catalogrel.InformationDomainIdentity{TypeOID: allocate(), ArrayTypeOID: allocate()}
		if domain.ConstraintName != "" {
			identity.ConstraintOID = allocate()
		}
		manifest.Config.InformationDomains[domain.Kind] = identity
	}
	for _, function := range builtin.InformationSchemaFunctions() {
		manifest.Config.InformationSchemaFunctions[function.Operation] = allocate()
	}
	return manifest
}

func nativeCatalogTestRegistry(t *testing.T) *catalogrel.Registry {
	t.Helper()
	bootstrap, err := catalogrel.NewBootstrapRegistry(nativeCatalogTestManifest().RegistryConfig())
	if err != nil {
		t.Fatal(err)
	}
	definitions := make(map[catalogrel.RelationID]catalogrel.BoundViewDefinition)
	sources, err := catalogrel.NativeViewSources()
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		parsed, parseErr := zpgparse.Parse(source.SQL)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		if parsed == nil || len(parsed.Stmts) != 1 || parsed.Stmts[0] == nil ||
			parsed.Stmts[0].Stmt == nil || parsed.Stmts[0].Stmt.GetSelectStmt() == nil {
			t.Fatalf("%s.%s is not a canonical SELECT", source.Namespace, source.Name)
		}
		encoding, encodingErr := (proto.MarshalOptions{Deterministic: true}).Marshal(&zpgparse.RawStmt{Stmt: parsed.Stmts[0].Stmt})
		if encodingErr != nil {
			t.Fatal(encodingErr)
		}
		definitions[source.Relation] = catalogrel.BoundViewDefinition{
			Relation:        source.Relation,
			RegistryVersion: bootstrap.Version(),
			Semantic: model.ExpressionDefinition{
				SemanticVersion: catalogrel.NativeViewSemanticVersion,
				Encoding:        encoding,
				Dependencies:    []model.Dependency{{Object: 1, Version: 1}},
			},
		}
	}
	sealed, err := bootstrap.Seal(definitions)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func TestNativeCatalogConfigRejectsIncompleteCollaborators(t *testing.T) {
	complete := NativeCatalogConfig{
		Catalog:       testCatalogStore(t),
		Identities:    &schema.JobStore{},
		Binding:       bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950},
		Descriptors:   model.DescriptorLimits{MaxBytes: 1 << 16, MaxItems: 8, MaxDepth: 8},
		ManifestBytes: 1 << 16,
	}
	if err := complete.validate(); err != nil {
		t.Fatalf("complete configuration was rejected: %v", err)
	}
	missingCatalog := complete
	missingCatalog.Catalog = nil
	missingIdentities := complete
	missingIdentities.Identities = nil
	zeroManifest := complete
	zeroManifest.ManifestBytes = 0
	invalidDescriptors := complete
	invalidDescriptors.Descriptors = model.DescriptorLimits{}
	emptyPath := complete
	emptyPath.Binding.SearchPath = nil
	missingCollation := complete
	missingCollation.Binding.DefaultCollation = 0
	for name, config := range map[string]NativeCatalogConfig{
		"missing catalog":     missingCatalog,
		"missing identities":  missingIdentities,
		"zero manifest bound": zeroManifest,
		"invalid descriptors": invalidDescriptors,
		"empty search path":   emptyPath,
		"missing collation":   missingCollation,
	} {
		if err := config.validate(); err == nil {
			t.Fatalf("%s: incomplete configuration was accepted", name)
		}
	}
}

func TestSealedNativeViewDefinitionsAgreeWithDurableRecord(t *testing.T) {
	registry := nativeCatalogTestRegistry(t)
	definitions, err := catalogrel.BoundViewDefinitions(registry)
	if err != nil {
		t.Fatal(err)
	}
	manifest := nativeCatalogTestManifest()
	manifest.Config.ViewDefinitions = definitions
	if err := validateSealedNativeViewDefinitions(manifest, registry); err != nil {
		t.Fatalf("matching definitions were rejected: %v", err)
	}
	missing := nativeCatalogTestManifest()
	missing.Config.ViewDefinitions = make(map[catalogrel.RelationID]catalogrel.BoundViewDefinition, len(definitions))
	for id, definition := range definitions {
		missing.Config.ViewDefinitions[id] = definition
	}
	delete(missing.Config.ViewDefinitions, catalogrel.IDViews)
	if err := validateSealedNativeViewDefinitions(missing, registry); err == nil {
		t.Fatal("a manifest missing one native view definition was accepted")
	}
	drifted := nativeCatalogTestManifest()
	drifted.Config.ViewDefinitions = make(map[catalogrel.RelationID]catalogrel.BoundViewDefinition, len(definitions))
	for id, definition := range definitions {
		drifted.Config.ViewDefinitions[id] = definition
	}
	tampered := drifted.Config.ViewDefinitions[catalogrel.IDViews]
	tampered.Semantic.Encoding = []byte{0xFF}
	drifted.Config.ViewDefinitions[catalogrel.IDViews] = tampered
	if err := validateSealedNativeViewDefinitions(drifted, registry); err == nil {
		t.Fatal("a manifest whose view definition drifted was accepted")
	}
}

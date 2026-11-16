package catalogrel

import (
	"context"
	"encoding/binary"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// nativeCatalogFixture builds a complete identity inventory from the pinned
// logical registry plus fresh identities for every unpinned object. Pinned
// PostgreSQL identities are reused verbatim; the rest come from one counter so
// the fixture stays tiny and deterministic.
func nativeCatalogFixture(t *testing.T) NativeCatalogManifest {
	t.Helper()
	next := model.OID(500000)
	allocate := func() model.OID {
		allocateValue := next
		next++
		return allocateValue
	}
	manifest := NativeCatalogManifest{
		PgCatalogObjectID: 700000,
		InformationSchema: NamespaceIdentity{ObjectID: 700001, OID: 700002},
		Config: RegistryConfig{
			Relations:                  make(map[RelationID]RelationIdentity),
			InformationDomains:         make(map[builtin.InformationDomainKind]InformationDomainIdentity),
			InformationSchemaFunctions: make(map[builtin.InformationSchemaFunction]model.OID),
		},
	}
	for _, relation := range All() {
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
		identity := InformationDomainIdentity{TypeOID: allocate(), ArrayTypeOID: allocate()}
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

// sealedFixture returns a Complete registry plus its bound definitions. Binding
// lives in the query/bind package, which imports catalogrel, so this package
// exercises Seal's contract with minimal well-formed definitions instead.
func sealedFixture(t *testing.T) (*Registry, map[RelationID]BoundViewDefinition) {
	t.Helper()
	bootstrap, err := NewBootstrapRegistry(nativeCatalogFixture(t).RegistryConfig())
	if err != nil {
		t.Fatal(err)
	}
	sources, err := NativeViewSources()
	if err != nil {
		t.Fatal(err)
	}
	definitions := make(map[RelationID]BoundViewDefinition, len(sources))
	for _, source := range sources {
		encoding, encodingErr := canonicalNativeViewSemanticEncoding(source.Relation)
		if encodingErr != nil {
			t.Fatal(encodingErr)
		}
		definitions[source.Relation] = BoundViewDefinition{
			Relation:        source.Relation,
			RegistryVersion: bootstrap.Version(),
			Semantic: model.ExpressionDefinition{
				SemanticVersion: NativeViewSemanticVersion,
				Encoding:        encoding,
				Dependencies:    []model.Dependency{{Object: 1, Version: 1}},
			},
		}
	}
	sealed, err := bootstrap.Seal(definitions)
	if err != nil {
		t.Fatal(err)
	}
	return sealed, definitions
}

// TestNativeViewSourcesResolveOnlyLogicalRelations pins the binding seam the
// bootstrap composition depends on: every canonical view reads only logical
// catalog relations, so binding needs no physical descriptor snapshot. A view
// that reached a physical relation would make bootstrap binding impossible.
func TestNativeViewSourcesResolveOnlyLogicalRelations(t *testing.T) {
	sources, err := NativeViewSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != len(nativeViewSources) {
		t.Fatalf("native view source inventory is partial: got %d entries, want %d", len(sources), len(nativeViewSources))
	}
	for _, source := range sources {
		parsed, err := zpgparse.Parse(source.SQL)
		if err != nil {
			t.Fatalf("%s.%s: %v", source.Namespace, source.Name, err)
		}
		selectStatement := parsed.Stmts[0].Stmt.GetSelectStmt()
		if selectStatement == nil {
			t.Fatalf("%s.%s: canonical definition is not a SELECT", source.Namespace, source.Name)
		}
		var references []*zpgparse.RangeVar
		collectRangeVars(selectStatement.FromClause, &references)
		for _, reference := range references {
			namespace := reference.Schemaname
			if namespace == "" {
				namespace = "pg_catalog"
			}
			if _, ok := registryByName[namespace+"."+reference.Relname]; !ok {
				t.Fatalf("%s.%s: FROM target %q is not a logical catalog relation", source.Namespace, source.Name, reference.Relname)
			}
		}
	}
}

func TestNativeCatalogCollationsPreservePostgreSQLIdentities(t *testing.T) {
	bootstrap, err := NewBootstrapRegistry(nativeCatalogFixture(t).RegistryConfig())
	if err != nil {
		t.Fatal(err)
	}
	indexes, ok := bootstrap.Lookup("pg_catalog", "pg_indexes")
	if !ok {
		t.Fatal("pg_catalog.pg_indexes is unavailable")
	}
	if got := indexes.Columns[0].Layout.Type.Collation; got != 950 {
		t.Fatalf("pg_indexes.schemaname collation = %d, want C identity 950", got)
	}
	if got := indexes.Columns[4].Layout.Type.Collation; got != 100 {
		t.Fatalf("pg_indexes.indexdef collation = %d, want default identity 100", got)
	}
	roles, ok := bootstrap.Lookup("pg_catalog", "pg_roles")
	if !ok {
		t.Fatal("pg_catalog.pg_roles is unavailable")
	}
	config := roles.Columns[11].Layout
	if config.Type.Collation != 100 || len(config.Children) != 1 || config.Children[0].Type.Collation != 100 {
		t.Fatalf("pg_roles.rolconfig layout = %+v, want default-collated text array", config)
	}
	character, ok := bootstrap.InformationDomain(builtin.InformationCharacterData)
	if !ok || character.Layout.Type.Collation != 100 || character.Definition.Base.Type.Collation != 100 {
		t.Fatalf("character_data domain = %+v, want default collation identity 100", character)
	}
	identifier, ok := bootstrap.InformationDomain(builtin.InformationSQLIdentifier)
	if !ok || identifier.Layout.Type.Collation != 950 || identifier.Definition.Base.Type.Collation != 950 {
		t.Fatalf("sql_identifier domain = %+v, want name/C collation identity 950", identifier)
	}
}

func TestTypeCatalogRelationshipsDistinguishTrueArrays(t *testing.T) {
	manifest := nativeCatalogFixture(t)
	bootstrap, err := NewBootstrapRegistry(manifest.RegistryConfig())
	if err != nil {
		t.Fatal(err)
	}
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 64, MaxDepth: 16}
	descriptor := func(spec model.DescriptorSpec) *model.Descriptor {
		result, descriptorErr := model.NewDescriptor(t.Context(), spec, limits)
		if descriptorErr != nil {
			t.Fatal(descriptorErr)
		}
		return result
	}
	const (
		publicSchemaID  model.ObjectID = 900000
		domainID        model.ObjectID = 900001
		arrayID         model.ObjectID = 900002
		publicSchemaOID model.OID      = 900000
		domainOID       model.OID      = 900001
		arrayOID        model.OID      = 900002
	)
	textArray := catalogLayout(1009, -1)
	domainLayout := cloneLayout(textArray)
	domainLayout.Type.ID = model.TypeID(domainOID)
	arrayLayout := model.TypeLayout{
		Type:     model.Type{ID: model.TypeID(arrayOID), Storage: model.TypeArray, Modifier: -1, Collation: 100},
		Children: []model.TypeLayout{domainLayout},
	}
	snapshot := nativeTypeCatalogSnapshot{descriptors: []*model.Descriptor{
		descriptor(model.DescriptorSpec{
			ID: manifest.InformationSchema.ObjectID, OID: manifest.InformationSchema.OID, Version: 1, State: model.StatePublic,
			Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "information_schema"},
			Schema: &model.SchemaMetadata{Owner: 10},
		}),
		descriptor(model.DescriptorSpec{
			ID: publicSchemaID, OID: publicSchemaOID, Version: 1, State: model.StatePublic,
			Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "public"},
			Schema: &model.SchemaMetadata{Owner: 10},
		}),
		descriptor(model.DescriptorSpec{
			ID: domainID, OID: domainOID, Version: 1, State: model.StatePublic,
			Name: model.NamespaceName{Parent: publicSchemaID, Kind: model.ObjectType, Name: "text_vector"},
			Type: &model.TypeMetadata{
				Kind: model.TypeKindDomain, Owner: 10, Layout: domainLayout, Array: arrayID, BaseLayout: textArray,
			},
		}),
		descriptor(model.DescriptorSpec{
			ID: arrayID, OID: arrayOID, Version: 1, State: model.StatePublic,
			Name: model.NamespaceName{Parent: publicSchemaID, Kind: model.ObjectType, Name: "_text_vector"},
			Type: &model.TypeMetadata{
				Kind: model.TypeKindArray, Owner: 10, Layout: arrayLayout, Base: domainID, BaseLayout: domainLayout,
			},
		}),
	}}
	pgType, ok := bootstrap.LookupID(IDPgType)
	if !ok {
		t.Fatal("pg_catalog.pg_type is unavailable")
	}
	columnIndex := func(name string) int {
		for i, column := range pgType.Columns {
			if column.Name == name {
				return i
			}
		}
		t.Fatalf("pg_type.%s is unavailable", name)
		return -1
	}
	oidIndex, elementIndex, arrayIndex := columnIndex("oid"), columnIndex("typelem"), columnIndex("typarray")
	type arrayRelationship struct {
		element int64
		array   int64
	}
	rows := make(map[model.OID]arrayRelationship)
	options := SourceOptions{
		Registry: bootstrap, MaxDescriptors: 64,
		MaxDescriptorBytes: limits.MaxBytes, MaxDescriptorItems: limits.MaxItems, MaxDescriptorDepth: limits.MaxDepth,
	}
	if err := emitTypes(t.Context(), snapshot, pgType, options, func(row []model.Value) error {
		rows[model.OID(row[oidIndex].IntVal)] = arrayRelationship{
			element: row[elementIndex].IntVal,
			array:   row[arrayIndex].IntVal,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertArray := func(oid model.OID, want bool) {
		t.Helper()
		relationship, ok := rows[oid]
		if !ok {
			t.Fatalf("pg_type row %d is unavailable", oid)
		}
		got := relationship.element != 0 && relationship.array == 0
		if got != want {
			t.Fatalf("pg_type row %d: typelem=%d typarray=%d, array=%t want %t", oid, relationship.element, relationship.array, got, want)
		}
	}
	assertArray(1009, true)
	assertArray(19, false)
	assertArray(22, false)
	assertArray(30, false)
	assertArray(domainOID, false)
	assertArray(arrayOID, true)
	informationDomain, ok := bootstrap.InformationDomain(builtin.InformationCharacterData)
	if !ok {
		t.Fatal("information_schema.character_data is unavailable")
	}
	assertArray(informationDomain.Identity.TypeOID, false)
	assertArray(informationDomain.Identity.ArrayTypeOID, true)
	classIdentity, ok := bootstrap.Identity(IDPgClass)
	if !ok {
		t.Fatal("pg_catalog.pg_class identity is unavailable")
	}
	assertArray(classIdentity.RowTypeOID, false)
	assertArray(classIdentity.ArrayTypeOID, true)
}

type nativeTypeCatalogSnapshot struct {
	descriptors []*model.Descriptor
}

func (s nativeTypeCatalogSnapshot) Close() error               { return nil }
func (s nativeTypeCatalogSnapshot) Timestamp() model.Timestamp { return 1 }
func (s nativeTypeCatalogSnapshot) Descriptor(_ context.Context, id model.ObjectID) (catalog.PublishedDescriptor, error) {
	for _, descriptor := range s.descriptors {
		if descriptor.ID() == id {
			return catalog.PublishedDescriptor{Descriptor: descriptor, PublishedAt: 1}, nil
		}
	}
	return catalog.PublishedDescriptor{}, catalog.ErrUnknownObject
}
func (s nativeTypeCatalogSnapshot) ScanNamespace(ctx context.Context, parent model.ObjectID, kind model.ObjectKind, visit func(catalog.PublishedDescriptor) error) error {
	for _, descriptor := range s.descriptors {
		if err := ctx.Err(); err != nil {
			return err
		}
		if descriptor.Name().Parent == parent && descriptor.Kind() == kind {
			if err := visit(catalog.PublishedDescriptor{Descriptor: descriptor, PublishedAt: 1}); err != nil {
				return err
			}
		}
	}
	return nil
}

func collectRangeVars(nodes []*zpgparse.Node, out *[]*zpgparse.RangeVar) {
	for _, node := range nodes {
		if node == nil {
			continue
		}
		if rangeVar := node.GetRangeVar(); rangeVar != nil {
			*out = append(*out, rangeVar)
			continue
		}
		if join := node.GetJoinExpr(); join != nil {
			collectRangeVars([]*zpgparse.Node{join.Larg, join.Rarg}, out)
			continue
		}
		if subselect := node.GetRangeSubselect(); subselect != nil {
			if statement := subselect.Subquery.GetSelectStmt(); statement != nil {
				collectRangeVars(statement.FromClause, out)
			}
		}
	}
}

// TestNativeCatalogManifestRoundTripPreservesIdentities pins reopen stability:
// the durable record must reproduce every relation, row-type, array, domain,
// constraint, and helper-function identity exactly.
func TestNativeCatalogManifestRoundTripPreservesIdentities(t *testing.T) {
	sealed, definitions := sealedFixture(t)
	manifest := nativeCatalogFixture(t)
	manifest.Config.ViewDefinitions = definitions

	payload, err := EncodeNativeCatalogManifest(manifest, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeNativeCatalogManifest(payload, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.PgCatalogObjectID != manifest.PgCatalogObjectID || decoded.InformationSchema != manifest.InformationSchema {
		t.Fatalf("namespace identity changed: %#v", decoded)
	}
	if len(decoded.Config.Relations) != len(manifest.Config.Relations) {
		t.Fatalf("relation inventory size = %d", len(decoded.Config.Relations))
	}
	for id, identity := range manifest.Config.Relations {
		if decoded.Config.Relations[id] != identity {
			t.Fatalf("relation %d identity = %#v want %#v", id, decoded.Config.Relations[id], identity)
		}
	}
	for kind, identity := range manifest.Config.InformationDomains {
		if decoded.Config.InformationDomains[kind] != identity {
			t.Fatalf("domain %d identity = %#v want %#v", kind, decoded.Config.InformationDomains[kind], identity)
		}
	}
	for operation, oid := range manifest.Config.InformationSchemaFunctions {
		if decoded.Config.InformationSchemaFunctions[operation] != oid {
			t.Fatalf("function %d oid = %d want %d", operation, decoded.Config.InformationSchemaFunctions[operation], oid)
		}
	}
	if len(decoded.Config.ViewDefinitions) != len(definitions) {
		t.Fatalf("view definition count = %d", len(decoded.Config.ViewDefinitions))
	}
	reopened, err := NewRegistry(decoded.RegistryConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Complete() || reopened.Version() != sealed.Version() {
		t.Fatalf("reopened registry is not identity stable: complete=%v", reopened.Complete())
	}
	for id := range definitions {
		identity, ok := reopened.Identity(id)
		if !ok || identity != manifest.Config.Relations[id] {
			t.Fatalf("reopened relation %d identity is unavailable", id)
		}
	}
}

// TestNativeCatalogManifestRejectsIncompatibleRecords pins that a truncated,
// version-bumped, or self-inconsistent record is rejected rather than repaired
// with fresh identities.
func TestNativeCatalogManifestRejectsIncompatibleRecords(t *testing.T) {
	_, definitions := sealedFixture(t)
	manifest := nativeCatalogFixture(t)
	manifest.Config.ViewDefinitions = definitions
	payload, err := EncodeNativeCatalogManifest(manifest, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	versionBump := append([]byte(nil), payload...)
	versionBump[11] = 2
	namespaceMissing := append([]byte(nil), payload...)
	for index := 12; index < 28; index++ {
		namespaceMissing[index] = 0
	}
	for name, record := range map[string][]byte{
		"truncated header":  payload[:4],
		"truncated body":    payload[:len(payload)-3],
		"version bump":      versionBump,
		"trailing bytes":    append(append([]byte(nil), payload...), 0),
		"empty":             {},
		"namespace missing": namespaceMissing,
	} {
		if _, err := DecodeNativeCatalogManifest(record, 1<<20); err == nil {
			t.Fatalf("%s: incompatible manifest was accepted", name)
		}
	}
	if _, err := DecodeNativeCatalogManifest(payload, uint64(len(payload)-1)); err == nil {
		t.Fatal("oversized manifest was accepted")
	}
}

type nativeManifestRecordOffsets struct {
	relationID      int
	viewID          int
	viewEncoding    int
	encodingLength  int
	dependencyCount int
}

func firstNativeManifestRecordOffsets(t *testing.T, payload []byte) nativeManifestRecordOffsets {
	t.Helper()
	reader := &manifestReader{in: payload, limit: uint64(len(payload))}
	reader.take(uint64(len(nativeCatalogManifestHeader)))
	reader.u32()
	reader.u64()
	reader.u32()
	reader.u64()
	relationCount := reader.u32()
	offsets := nativeManifestRecordOffsets{relationID: int(reader.at)}
	reader.take(uint64(relationCount) * 16)
	domainCount := reader.u32()
	reader.take(uint64(domainCount) * 13)
	functionCount := reader.u32()
	reader.take(uint64(functionCount) * 5)
	if viewCount := reader.u32(); viewCount == 0 {
		t.Fatal("manifest has no native views")
	}
	offsets.viewID = int(reader.at)
	reader.u32()
	reader.take(32)
	reader.u32()
	encodingLength := reader.u32()
	offsets.viewEncoding = int(reader.at)
	offsets.encodingLength = int(encodingLength)
	reader.take(uint64(encodingLength))
	offsets.dependencyCount = int(reader.at)
	if reader.fail != nil || offsets.encodingLength == 0 {
		t.Fatalf("valid manifest offsets are unavailable: %v", reader.fail)
	}
	return offsets
}

func TestNativeCatalogManifestRejectsMalformedViewRecords(t *testing.T) {
	_, definitions := sealedFixture(t)
	manifest := nativeCatalogFixture(t)
	manifest.Config.ViewDefinitions = definitions
	payload, err := EncodeNativeCatalogManifest(manifest, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	offsets := firstNativeManifestRecordOffsets(t, payload)
	highRelationID := append([]byte(nil), payload...)
	relationID := binary.BigEndian.Uint32(highRelationID[offsets.relationID:])
	binary.BigEndian.PutUint32(highRelationID[offsets.relationID:], relationID+(1<<16))
	highViewID := append([]byte(nil), payload...)
	viewID := binary.BigEndian.Uint32(highViewID[offsets.viewID:])
	binary.BigEndian.PutUint32(highViewID[offsets.viewID:], viewID+(1<<16))
	malformedEncoding := append([]byte(nil), payload...)
	malformedEncoding[offsets.viewEncoding] ^= 0xff
	oversizedDependencies := append([]byte(nil), payload...)
	binary.BigEndian.PutUint32(oversizedDependencies[offsets.dependencyCount:], math.MaxUint32)
	records := []struct {
		name   string
		record []byte
	}{
		{"high relation ID", highRelationID},
		{"high view ID", highViewID},
		{"malformed view encoding", malformedEncoding},
		{"oversized dependency count", oversizedDependencies},
	}
	for _, test := range records {
		if _, err := DecodeNativeCatalogManifest(test.record, 1<<20); err == nil {
			t.Fatalf("%s was accepted", test.name)
		}
	}
}

// TestRegistryIdentityIndexingFollowsRelationID pins that ID lookups index by
// identity, not by the declaration literal's position, and that All() is
// ascending and excludes the sentinel slot.
func TestRegistryIdentityIndexingFollowsRelationID(t *testing.T) {
	sealed, _ := sealedFixture(t)
	relations := sealed.All()
	if len(relations) != len(registry) {
		t.Fatalf("All() returned %d relations, want %d", len(relations), len(registry))
	}
	for index, relation := range relations {
		if relation.ID != RelationID(index+1) {
			t.Fatalf("All()[%d].ID = %d, want %d", index, relation.ID, index+1)
		}
		byID, ok := sealed.LookupID(relation.ID)
		if !ok || byID.Name != relation.Name || byID.Namespace != relation.Namespace {
			t.Fatalf("LookupID(%d) = %s.%s", relation.ID, byID.Namespace, byID.Name)
		}
		identity, ok := sealed.Identity(relation.ID)
		if !ok || identity != byID.Identity {
			t.Fatalf("Identity(%d) disagrees with LookupID", relation.ID)
		}
	}
	if _, ok := sealed.LookupID(0); ok {
		t.Fatal("sentinel relation ID was resolved")
	}
	if _, ok := sealed.LookupID(RelationID(len(registry) + 1)); ok {
		t.Fatal("out-of-range relation ID was resolved")
	}
}

// TestBootstrapRegistryRejectsIncompleteInventories pins that construction and
// sealing refuse partial manifests instead of relaxing Complete.
func TestBootstrapRegistryRejectsIncompleteInventories(t *testing.T) {
	config := nativeCatalogFixture(t).RegistryConfig()
	if _, err := NewBootstrapRegistry(config); err != nil {
		t.Fatalf("complete inventory was rejected: %v", err)
	}
	missing := nativeCatalogFixture(t)
	delete(missing.Config.Relations, IDPgClass)
	if _, err := NewBootstrapRegistry(missing.RegistryConfig()); err == nil {
		t.Fatal("incomplete relation inventory was accepted")
	}
	missingDomain := nativeCatalogFixture(t)
	delete(missingDomain.Config.InformationDomains, builtin.InformationYesOrNo)
	if _, err := NewBootstrapRegistry(missingDomain.RegistryConfig()); err == nil {
		t.Fatal("incomplete information domain inventory was accepted")
	}
	missingFunction := nativeCatalogFixture(t)
	delete(missingFunction.Config.InformationSchemaFunctions, builtin.InformationExpandArray)
	if _, err := NewBootstrapRegistry(missingFunction.RegistryConfig()); err == nil {
		t.Fatal("incomplete information schema function inventory was accepted")
	}
	bootstrap, err := NewBootstrapRegistry(nativeCatalogFixture(t).RegistryConfig())
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.Complete() || bootstrap.IdentityComplete() == false {
		t.Fatal("a bootstrap registry must be identity-complete and unsealed")
	}
	if _, err := bootstrap.Seal(nil); err == nil {
		t.Fatal("sealing without definitions was accepted")
	}
	if _, err := bootstrap.Seal(map[RelationID]BoundViewDefinition{}); err == nil {
		t.Fatal("sealing with an empty definition set was accepted")
	}
}

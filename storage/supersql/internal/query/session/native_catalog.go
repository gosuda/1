package session

import (
	"bytes"
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
)

// NativeCatalogConfig borrows database services and immutable binding settings.
// Binding must already carry the resolved search path, collations, and limits;
// this composition supplies the bootstrap registry and leaves every other field
// untouched. Descriptors bounds published catalog metadata and ManifestBytes
// bounds the durable identity record.
type NativeCatalogConfig struct {
	Catalog       *catalog.Store
	Identities    *schema.JobStore
	Binding       bind.Options
	Descriptors   model.DescriptorLimits
	ManifestBytes uint64
}

// NativeCatalogIdentity is the durable identity of a bootstrapped database. It
// carries exactly what a catalog source needs beyond the sealed registry: the
// pg_catalog schema descriptor identity, which is not part of the logical
// relation inventory, and the information_schema namespace identity.
type NativeCatalogIdentity struct {
	PgCatalog         catalogrel.NamespaceIdentity
	PgCatalogObjectID model.ObjectID
	InformationSchema catalogrel.NamespaceIdentity
}

// NativeCatalog is a published, sealed native catalog. Registry is Complete and
// identity-stable across reopen: loading the same database yields the same
// relation, row-type, array, domain, and helper-function identities.
type NativeCatalog struct {
	Registry *catalogrel.Registry
	Identity NativeCatalogIdentity
	Version  catalogrel.RegistryVersion
}

func (config NativeCatalogConfig) validate() error {
	if config.Catalog == nil || config.Identities == nil || config.ManifestBytes == 0 {
		return &model.Error{Code: "22023", Message: "native catalog composition requires catalog, identity store, and manifest bounds"}
	}
	if err := config.Descriptors.Validate(); err != nil {
		return err
	}
	if config.Binding.Collation == 0 || config.Binding.DefaultCollation == 0 || len(config.Binding.SearchPath) == 0 {
		return &model.Error{Code: "22023", Message: "native catalog binding requires a resolved search path and collations"}
	}
	return nil
}

// BootstrapNativeCatalog allocates and publishes the complete native catalog in
// one atomic statement. Every non-pinned identity comes from the durable
// catalog allocator; pinned PostgreSQL identities are never reallocated. The
// returned registry is sealed and Complete only after the identity allocations,
// the namespace descriptors, and the durable manifest commit together. Any
// binding or publication failure rolls the identity reservations back.
func BootstrapNativeCatalog(ctx context.Context, transactions *Transactions, config NativeCatalogConfig) (*NativeCatalog, error) {
	if transactions == nil {
		return nil, errSessionTransactionConfig
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	if transactions.active {
		return nil, settingError("25001", "native catalog bootstrap cannot run inside a transaction block")
	}
	if err := transactions.begin(operation, TransactionStart{}); err != nil {
		return nil, err
	}
	if err := transactions.ordinary(); err != nil {
		if transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(operation)))
		}
		return nil, err
	}
	var result *NativeCatalog
	err = transactions.routineAtomicStatement(operation, func(statement context.Context) error {
		if transactions.settings.TransactionPolicy().ReadOnly {
			return settingError("25006", "cannot bootstrap the native catalog in a read-only transaction")
		}
		published, err := bootstrapNativeCatalog(statement, transactions, config)
		if err != nil {
			return err
		}
		result = published
		return nil
	})
	err = errors.Join(err, transactions.markDataAccess())
	if err != nil {
		transactions.markFailed(err)
		if transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(operation)))
		}
		return nil, err
	}
	if _, err = transactions.commit(operation, false); err != nil {
		transactions.markFailed(err)
		if transactions.active && transactions.transaction.State() != txn.Indeterminate {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(operation)))
		}
		return nil, err
	}
	return result, nil
}

// LoadNativeCatalog reopens a bootstrapped database from its durable manifest.
// It never allocates: every identity is read back and validated, and a record
// that is missing, truncated, or inconsistent with this build is rejected
// rather than completed with fresh identities.
func LoadNativeCatalog(ctx context.Context, transactions *Transactions, config NativeCatalogConfig) (*NativeCatalog, error) {
	if transactions == nil {
		return nil, errSessionTransactionConfig
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	if err := transactions.ordinary(); err != nil {
		return nil, err
	}
	var result *NativeCatalog
	err = transactions.routineAtomicStatement(operation, func(statement context.Context) error {
		manifest, err := readNativeCatalogManifest(statement, transactions, config)
		if err != nil {
			return err
		}
		decoded, err := catalogrel.DecodeNativeCatalogManifest(manifest, config.ManifestBytes)
		if err != nil {
			return err
		}
		pgCatalog, informationSchema, err := verifyNativeCatalogNamespaces(statement, transactions, config, decoded.PgCatalogObjectID, decoded.InformationSchema.ObjectID, decoded.InformationSchema.OID)
		if err != nil {
			return err
		}
		registry, err := catalogrel.NewRegistry(decoded.RegistryConfig())
		if err != nil {
			return err
		}
		result = &NativeCatalog{
			Registry: registry,
			Identity: NativeCatalogIdentity{
				PgCatalog:         pgCatalog,
				PgCatalogObjectID: decoded.PgCatalogObjectID,
				InformationSchema: informationSchema,
			},
			Version: registry.Version(),
		}
		return nil
	})
	err = errors.Join(err, transactions.markDataAccess())
	if err != nil {
		return nil, err
	}
	return result, nil
}

// readNativeCatalogManifest reads the durable record through its own protected
// read view. The manifest must be decoded before the namespace descriptor
// dependencies can be declared, so the two reads cannot share one view.
func readNativeCatalogManifest(ctx context.Context, transactions *Transactions, config NativeCatalogConfig) (payload []byte, err error) {
	view, err := transactions.transaction.ReadView(ctx, []txn.ReadDependency{config.Identities.CatalogManifestDependency()})
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, view.Close()) }()
	return config.Identities.CatalogManifest(ctx, view, config.ManifestBytes)
}

// verifyNativeCatalogNamespaces reads both persisted namespace descriptors
// through a dedicated read view covering exactly those two descriptor rows, so
// the reopen path never scans the catalog namespace.
func verifyNativeCatalogNamespaces(ctx context.Context, transactions *Transactions, config NativeCatalogConfig, pgCatalogObjectID, informationSchemaObjectID model.ObjectID, informationSchemaOID model.OID) (pgCatalog, informationSchema catalogrel.NamespaceIdentity, err error) {
	if pgCatalogObjectID == 0 || informationSchemaObjectID == 0 || informationSchemaOID == 0 || informationSchemaOID == 11 {
		return catalogrel.NamespaceIdentity{}, catalogrel.NamespaceIdentity{}, &model.Error{Code: "XX001", Message: "native catalog manifest namespace identity is unavailable"}
	}
	dependencies := []txn.ReadDependency{
		config.Catalog.DescriptorDependency(pgCatalogObjectID),
		config.Catalog.DescriptorDependency(informationSchemaObjectID),
	}
	view, err := transactions.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return catalogrel.NamespaceIdentity{}, catalogrel.NamespaceIdentity{}, err
	}
	snapshot, err := config.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return catalogrel.NamespaceIdentity{}, catalogrel.NamespaceIdentity{}, errors.Join(err, view.Close())
	}
	defer func() { err = errors.Join(err, snapshot.Close(), view.Close()) }()
	owner := model.Principal{}.OID()
	pgCatalogDescriptor, err := verifyNamespaceDescriptor(ctx, snapshot, pgCatalogObjectID, "pg_catalog", 11, owner)
	if err != nil {
		return catalogrel.NamespaceIdentity{}, catalogrel.NamespaceIdentity{}, err
	}
	informationSchemaDescriptor, err := verifyNamespaceDescriptor(ctx, snapshot, informationSchemaObjectID, "information_schema", informationSchemaOID, owner)
	if err != nil {
		return catalogrel.NamespaceIdentity{}, catalogrel.NamespaceIdentity{}, err
	}
	pgCatalog = catalogrel.NamespaceIdentity{ObjectID: pgCatalogDescriptor.ID(), OID: pgCatalogDescriptor.OID()}
	informationSchema = catalogrel.NamespaceIdentity{ObjectID: informationSchemaDescriptor.ID(), OID: informationSchemaDescriptor.OID()}
	return pgCatalog, informationSchema, nil
}

func bootstrapNativeCatalog(ctx context.Context, transactions *Transactions, config NativeCatalogConfig) (*NativeCatalog, error) {
	allocator := sequenceIdentityAllocator{jobs: config.Identities, transaction: transactions.transaction}
	switch _, err := config.Identities.CatalogManifest(ctx, transactions.transaction, config.ManifestBytes); {
	case err == nil:
		return nil, &model.Error{Code: "42710", Message: "native catalog is already bootstrapped"}
	case !errors.Is(err, storage.ErrRowNotFound):
		return nil, err
	}
	manifest, err := allocateNativeCatalogIdentities(ctx, allocator)
	if err != nil {
		return nil, err
	}
	pgCatalog, informationSchema, err := publishNativeCatalogNamespaces(ctx, transactions, config, manifest)
	if err != nil {
		return nil, err
	}
	manifest.PgCatalogObjectID = pgCatalog.ID()
	manifest.InformationSchema = catalogrel.NamespaceIdentity{ObjectID: informationSchema.ID(), OID: informationSchema.OID()}

	binding := config.Binding
	bootstrap, err := catalogrel.NewBootstrapRegistry(manifest.RegistryConfig())
	if err != nil {
		return nil, err
	}
	binding.CatalogRelations = bootstrap
	binding.Builtin.InformationSchemaTypes = bootstrap
	registry, err := bindNativeCatalogViews(ctx, binding, bootstrap)
	if err != nil {
		return nil, err
	}
	definitions, err := catalogrel.BoundViewDefinitions(registry)
	if err != nil {
		return nil, err
	}
	manifest.Config.ViewDefinitions = definitions
	if err := validateSealedNativeViewDefinitions(manifest, registry); err != nil {
		return nil, err
	}
	payload, err := catalogrel.EncodeNativeCatalogManifest(manifest, config.ManifestBytes)
	if err != nil {
		return nil, err
	}
	if err := config.Identities.CreateCatalogManifest(ctx, transactions.transaction, payload, config.ManifestBytes); err != nil {
		return nil, err
	}
	return &NativeCatalog{
		Registry: registry,
		Identity: NativeCatalogIdentity{
			PgCatalog:         catalogrel.NamespaceIdentity{ObjectID: pgCatalog.ID(), OID: 11},
			PgCatalogObjectID: pgCatalog.ID(),
			InformationSchema: manifest.InformationSchema,
		},
		Version: registry.Version(),
	}, nil
}

func bindNativeCatalogViews(ctx context.Context, binding bind.Options, bootstrap *catalogrel.Registry) (*catalogrel.Registry, error) {
	sources, err := catalogrel.NativeViewSources()
	if err != nil {
		return nil, err
	}
	definitions := make(map[catalogrel.RelationID]catalogrel.BoundViewDefinition, len(sources))
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		definition, err := bind.BindCatalogViewDefinition(ctx, source, nil, binding)
		if err != nil {
			return nil, errors.Join(err, &model.Error{Code: "XX000", Message: "native view definition binding failed", Detail: source.Namespace + "." + source.Name})
		}
		definitions[source.Relation] = definition
	}
	return bootstrap.Seal(definitions)
}

// validateSealedNativeViewDefinitions re-derives each persisted definition from
// the sealed registry, so the durable record can never carry a definition that
// disagrees with the registry it was sealed against.
func validateSealedNativeViewDefinitions(manifest catalogrel.NativeCatalogManifest, registry *catalogrel.Registry) error {
	sources, err := catalogrel.NativeViewSources()
	if err != nil {
		return err
	}
	for _, source := range sources {
		definition, ok := manifest.Config.ViewDefinitions[source.Relation]
		if !ok {
			return &model.Error{Code: "XX000", Message: "sealed native view definition is absent from the durable record", Detail: source.Namespace + "." + source.Name}
		}
		sealed, ok := registry.ViewDefinition(source.Namespace, source.Name)
		if !ok || sealed.Bound.RegistryVersion != definition.RegistryVersion ||
			sealed.Bound.Semantic.SemanticVersion != definition.Semantic.SemanticVersion ||
			!bytes.Equal(sealed.Bound.Semantic.Encoding, definition.Semantic.Encoding) {
			return &model.Error{Code: "XX000", Message: "sealed native view definition disagrees with the durable record", Detail: source.Namespace + "." + source.Name}
		}
	}
	return nil
}

func allocateNativeCatalogIdentities(ctx context.Context, allocator bind.DeclarationIdentityAllocator) (catalogrel.NativeCatalogManifest, error) {
	allocate := func() (model.ObjectID, model.OID, error) {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		return allocator.AllocateDeclarationIdentity(ctx)
	}
	manifest := catalogrel.NativeCatalogManifest{
		Config: catalogrel.RegistryConfig{
			Relations:                  make(map[catalogrel.RelationID]catalogrel.RelationIdentity),
			InformationDomains:         make(map[builtin.InformationDomainKind]catalogrel.InformationDomainIdentity),
			InformationSchemaFunctions: make(map[builtin.InformationSchemaFunction]model.OID),
		},
	}
	informationObject, informationOID, err := allocate()
	if err != nil {
		return catalogrel.NativeCatalogManifest{}, err
	}
	if informationObject == 0 || informationOID == 0 || informationOID == 11 {
		return catalogrel.NativeCatalogManifest{}, &model.Error{Code: "XX000", Message: "allocated information_schema identity is invalid"}
	}
	manifest.InformationSchema = catalogrel.NamespaceIdentity{ObjectID: informationObject, OID: informationOID}
	if manifest.PgCatalogObjectID, _, err = allocate(); err != nil {
		return catalogrel.NativeCatalogManifest{}, err
	}
	for _, relation := range catalogrel.All() {
		identity := relation.Identity
		if identity.RelationOID == 0 {
			if _, identity.RelationOID, err = allocate(); err != nil {
				return catalogrel.NativeCatalogManifest{}, err
			}
		}
		if identity.RowTypeOID == 0 {
			if _, identity.RowTypeOID, err = allocate(); err != nil {
				return catalogrel.NativeCatalogManifest{}, err
			}
		}
		if identity.ArrayTypeOID == 0 {
			if _, identity.ArrayTypeOID, err = allocate(); err != nil {
				return catalogrel.NativeCatalogManifest{}, err
			}
		}
		manifest.Config.Relations[relation.ID] = identity
	}
	for _, domain := range builtin.InformationDomains() {
		identity := catalogrel.InformationDomainIdentity{}
		if _, identity.TypeOID, err = allocate(); err != nil {
			return catalogrel.NativeCatalogManifest{}, err
		}
		if _, identity.ArrayTypeOID, err = allocate(); err != nil {
			return catalogrel.NativeCatalogManifest{}, err
		}
		if domain.ConstraintName != "" {
			if _, identity.ConstraintOID, err = allocate(); err != nil {
				return catalogrel.NativeCatalogManifest{}, err
			}
		}
		manifest.Config.InformationDomains[domain.Kind] = identity
	}
	for _, function := range builtin.InformationSchemaFunctions() {
		_, oid, err := allocate()
		if err != nil {
			return catalogrel.NativeCatalogManifest{}, err
		}
		manifest.Config.InformationSchemaFunctions[function.Operation] = oid
	}
	return manifest, nil
}

// publishNativeCatalogNamespaces creates the two fixed namespace descriptors.
// pg_catalog is pinned to SQL OID 11 and owner 10 so catalog projections can
// discover it as the fixed persisted schema.
func publishNativeCatalogNamespaces(ctx context.Context, transactions *Transactions, config NativeCatalogConfig, manifest catalogrel.NativeCatalogManifest) (pgCatalog, informationSchema *model.Descriptor, err error) {
	owner := model.Principal{}.OID()
	pgCatalog, err = model.NewDescriptor(ctx, model.DescriptorSpec{
		ID: manifest.PgCatalogObjectID, OID: 11, Version: 1, State: model.StatePublic,
		Name:   model.NamespaceName{Kind: model.ObjectSchema, Name: "pg_catalog"},
		Schema: &model.SchemaMetadata{Owner: owner},
	}, config.Descriptors)
	if err != nil {
		return nil, nil, err
	}
	informationSchema, err = model.NewDescriptor(ctx, model.DescriptorSpec{
		ID: manifest.InformationSchema.ObjectID, OID: manifest.InformationSchema.OID, Version: 1, State: model.StatePublic,
		Name:   model.NamespaceName{Kind: model.ObjectSchema, Name: "information_schema"},
		Schema: &model.SchemaMetadata{Owner: owner},
	}, config.Descriptors)
	if err != nil {
		return nil, nil, err
	}
	descriptors := []*model.Descriptor{pgCatalog, informationSchema}
	dependencies := make([]txn.ReadDependency, 0, len(descriptors)*3)
	for _, descriptor := range descriptors {
		created, err := config.Catalog.CreateDependencies(ctx, descriptor)
		if err != nil {
			return nil, nil, err
		}
		dependencies = append(dependencies, created...)
	}
	view, err := transactions.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return nil, nil, err
	}
	snapshot, err := config.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, nil, errors.Join(err, view.Close())
	}
	defer func() { err = errors.Join(err, snapshot.Close(), view.Close()) }()
	for _, descriptor := range descriptors {
		change, err := config.Catalog.Create(ctx, snapshot, descriptor)
		if err != nil {
			return nil, nil, err
		}
		if err := transactions.transaction.ApplyMutations(ctx, change.Mutations); err != nil {
			return nil, nil, err
		}
	}
	return pgCatalog, informationSchema, nil
}

func verifyNamespaceDescriptor(ctx context.Context, snapshot *catalog.CatalogSnapshot, id model.ObjectID, name string, oid model.OID, owner model.OID) (*model.Descriptor, error) {
	published, err := snapshot.Descriptor(ctx, id)
	if err != nil {
		return nil, err
	}
	descriptor := published.Descriptor
	if descriptor == nil || descriptor.State() != model.StatePublic || descriptor.Kind() != model.ObjectSchema ||
		descriptor.Name().Name != name || descriptor.OID() != oid {
		return nil, &model.Error{Code: "XX001", Message: "native catalog namespace descriptor does not match the manifest", Detail: name}
	}
	spec, err := descriptor.SpecWithin(ctx, model.DescriptorLimits{MaxBytes: 1 << 16, MaxItems: 8, MaxDepth: 8})
	if err != nil {
		return nil, err
	}
	if spec.Schema == nil || spec.Schema.Owner != owner {
		return nil, &model.Error{Code: "XX001", Message: "native catalog namespace owner is inconsistent", Detail: name}
	}
	return descriptor, nil
}

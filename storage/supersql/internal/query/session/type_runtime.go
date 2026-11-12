package session

import (
	"context"
	"crypto/sha256"
	"slices"
	"sync"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
)

type DeclaredTypeRuntimeOptions struct {
	Descriptors model.DescriptorLimits
	Expressions ColumnExpressionOptions
	Routines    procedural.Limits
}

// sessionTypeResolver delegates declared-type lookups to the runtime pinned by
// the session's active statement. The transaction key encoder holds it so row
// keys, index keys, and dependency spans resolve enum, domain, and composite
// layouts against the same snapshot the statement bound.
type sessionTypeResolver struct {
	owner *Transactions
}

func (r *sessionTypeResolver) ResolveType(id model.TypeID) (*builtin.ResolvedType, bool) {
	if r == nil || r.owner == nil || r.owner.statementTypes == nil {
		return nil, false
	}
	return r.owner.statementTypes.ResolveType(id)
}

func (r *sessionTypeResolver) SemanticFingerprint() [sha256.Size]byte {
	if r == nil || r.owner == nil || r.owner.statementTypes == nil {
		return [sha256.Size]byte{}
	}
	return r.owner.statementTypes.SemanticFingerprint()
}

// KeyEncoder is the session's statement-pinned key encoder. It resolves
// declared types through the runtime the active statement installed, so it is
// safe to capture for snapshot-scoped planning collaborators.
func (transactions *Transactions) KeyEncoder() storage.KeyEncoder {
	if transactions == nil {
		return nil
	}
	return transactions.options.KeyEncoder
}

// activateDeclaredTypes pins declared-type resolution for the statement to its
// own snapshot. Statement bodies that bind or admit keys outside the prepared
// pipeline call it once their catalog snapshot exists; the session key encoder
// delegates to it for every subsequent encode in the statement.
func (transactions *Transactions) activateDeclaredTypes(ctx context.Context, snapshot *catalog.CatalogSnapshot, builtins builtin.ResolveOptions) error {
	if transactions == nil || transactions.typeRuntime == nil {
		return nil
	}
	options := *transactions.typeRuntime
	options.Expressions.Builtin = builtins
	if options.Expressions.Builtin.InformationSchemaTypes == nil {
		options.Expressions.Builtin.InformationSchemaTypes = transactions.typeRuntime.Expressions.Builtin.InformationSchemaTypes
	}
	if options.Expressions.Builtin.InformationSchemaFunctions == nil {
		options.Expressions.Builtin.InformationSchemaFunctions = transactions.typeRuntime.Expressions.Builtin.InformationSchemaFunctions
	}
	types, err := transactions.declaredTypeRuntime(ctx, snapshot, options)
	if err != nil {
		return err
	}
	transactions.statementTypes = types
	return nil
}

// NewDeclaredTypeKeyEncoderFactory pins key semantics to each policy snapshot.
// The cache shares the definition scan and the routine discovery of snapshots
// that expose no overlay; a nil cache builds every runtime from a fresh scan.
func NewDeclaredTypeKeyEncoderFactory(base *builtin.KeyEncoder, options DeclaredTypeRuntimeOptions, cache *DeclaredTypeCache) (schema.KeyEncoderFactory, error) {
	if base == nil {
		return nil, errPreparedInvocation
	}
	if err := options.Descriptors.Validate(); err != nil {
		return nil, err
	}
	return func(ctx context.Context, snapshot *catalog.CatalogSnapshot) (schema.KeyCodec, error) {
		types, err := cache.Runtime(ctx, snapshot, options)
		if err != nil {
			return nil, err
		}
		resolvers := options.Expressions.Builtin
		resolvers.Types = types
		return base.WithResolveOptions(resolvers), nil
	}, nil
}

// DeclaredTypeCache shares declared-type metadata between callers that read the
// same catalog content. It is safe to share across sessions because it serves
// only snapshots that expose no overlay: such a snapshot's content is exactly
// the committed catalog at its identity's version (or its fixed timestamp), which
// equal identities name exactly. A snapshot that carries an overlay is
// statement- or session-specific, so its runtime is built without this cache.
type DeclaredTypeCache struct {
	definitions declaredTypeDefinitionsCache
	routines    declaredRoutineCache
}

// NewDeclaredTypeCache returns an empty shared declared-type cache.
func NewDeclaredTypeCache() *DeclaredTypeCache { return &DeclaredTypeCache{} }

// overlayFree reports whether a snapshot exposes only committed catalog rows, so
// that its identity names its content without any session- or
// statement-specific overlay. A nil or unnamed snapshot is never overlay-free.
func overlayFree(snapshot *catalog.CatalogSnapshot) bool {
	identity := snapshot.CatalogIdentity()
	if identity == (txn.CatalogReadIdentity{}) {
		return false
	}
	return identity.Overlay == 0 && identity.SnapshotOverlay == 0
}

// Runtime returns the declared-type runtime for snapshot. It is the shared-cache
// entry point for a caller that owns no session: an overlay-free snapshot reuses
// the definition scan and routine discovery of any other caller reading the same
// catalog content, while the resolver itself is always compiled for these
// options, so the result is indistinguishable from a fresh scan.
func (cache *DeclaredTypeCache) Runtime(ctx context.Context, snapshot *catalog.CatalogSnapshot, options DeclaredTypeRuntimeOptions) (builtin.TypeResolver, error) {
	if err := options.Descriptors.Validate(); err != nil {
		return nil, err
	}
	if cache == nil || snapshot == nil || !overlayFree(snapshot) {
		return NewDeclaredTypeRuntime(ctx, snapshot, options)
	}
	identity := snapshot.CatalogIdentity()
	definitions, err := cache.definitions.load(declaredTypeDefinitionsKey{identity: identity, descriptors: options.Descriptors}, func() ([]builtin.TypeDefinition, error) {
		return scanDeclaredTypeDefinitions(ctx, snapshot, options.Descriptors, nil)
	})
	if err != nil {
		return nil, err
	}
	key := declaredRoutineKey{identity: identity, descriptors: options.Descriptors, routines: options.Routines, seeds: declaredRoutineSeeds(definitions)}
	return buildDeclaredTypeRuntime(ctx, snapshot, definitions, options, &cache.routines, key)
}

// declaredTypeDefinitionsKey names every input that determines the declared-type
// definition set a statement scans: the catalog content the statement reads, the
// session's temporary-object overlay, and the descriptor limit that bounds the
// scan. Nothing else affects which definitions exist, so the compiled runtimes
// stay per-statement while the scan result is shared.
type declaredTypeDefinitionsKey struct {
	identity    txn.CatalogReadIdentity
	temporary   uint64
	descriptors model.DescriptorLimits
}

// declaredTypeDefinitionsCache retains the most recently scanned definition sets.
// The bound is a constant, so retained metadata is bounded and no unbounded
// growth is possible. The mutex admits a nested statement that scans while its
// enclosing statement is still running.
type declaredTypeDefinitionsCache struct {
	mu      sync.Mutex
	entries []declaredTypeDefinitionsCacheEntry
	// scans counts definition sets scanned because no cached set matched. It is
	// the observable measure of declared-type rebuilds.
	scans uint64
}

type declaredTypeDefinitionsCacheEntry struct {
	key         declaredTypeDefinitionsKey
	definitions []builtin.TypeDefinition
}

// declaredTypeDefinitionsCacheEntries bounds retained definition sets. A
// statement scans with at most a couple of distinct keys (the session overlay
// and the statement's own additions to it); the allowance leaves room for a
// nested statement without evicting the common pair.
const declaredTypeDefinitionsCacheEntries = 4

// load returns the cached definitions for key, or scans them with build and
// caches the result. Definitions own their metadata and outlive the snapshot
// that produced them, which is what makes retention safe.
func (cache *declaredTypeDefinitionsCache) load(key declaredTypeDefinitionsKey, build func() ([]builtin.TypeDefinition, error)) ([]builtin.TypeDefinition, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for index := range cache.entries {
		if cache.entries[index].key != key {
			continue
		}
		entry := cache.entries[index]
		copy(cache.entries[1:index+1], cache.entries[:index])
		cache.entries[0] = entry
		return entry.definitions, nil
	}
	cache.scans++
	definitions, err := build()
	if err != nil {
		return nil, err
	}
	if len(cache.entries) < declaredTypeDefinitionsCacheEntries {
		cache.entries = append(cache.entries, declaredTypeDefinitionsCacheEntry{})
	}
	copy(cache.entries[1:], cache.entries[:len(cache.entries)-1])
	cache.entries[0] = declaredTypeDefinitionsCacheEntry{key: key, definitions: definitions}
	return definitions, nil
}

// declaredTypeDefinitions returns the declared-type definitions one statement
// scans. The scan is the only O(catalog) I/O in building a declared-type
// runtime, and it is a pure function of the catalog content the statement reads,
// the session overlay, and the descriptor limit, so a cached set is
// indistinguishable from a fresh scan. A snapshot whose reader cannot name its
// catalog content is never cached.
func (transactions *Transactions) declaredTypeDefinitions(ctx context.Context, snapshot *catalog.CatalogSnapshot, limits model.DescriptorLimits) ([]builtin.TypeDefinition, error) {
	identity := snapshot.CatalogIdentity()
	if identity == (txn.CatalogReadIdentity{}) {
		return scanDeclaredTypeDefinitions(ctx, snapshot, limits, nil)
	}
	key := declaredTypeDefinitionsKey{identity: identity, temporary: transactions.named.overlayVersion(), descriptors: limits}
	return transactions.declaredTypes.load(key, func() ([]builtin.TypeDefinition, error) {
		return scanDeclaredTypeDefinitions(ctx, snapshot, limits, nil)
	})
}

// declaredTypeRuntime returns the statement's declared-type runtime. The scanned
// definitions are shared with any statement reading the same catalog content,
// while the runtime itself is always compiled for this statement's options, so
// the result is indistinguishable from building it from a fresh scan.
func (transactions *Transactions) declaredTypeRuntime(ctx context.Context, snapshot *catalog.CatalogSnapshot, options DeclaredTypeRuntimeOptions) (builtin.TypeResolver, error) {
	if err := options.Descriptors.Validate(); err != nil {
		return nil, err
	}
	definitions, err := transactions.declaredTypeDefinitions(ctx, snapshot, options.Descriptors)
	if err != nil {
		return nil, err
	}
	identity := snapshot.CatalogIdentity()
	if identity == (txn.CatalogReadIdentity{}) {
		return buildDeclaredTypeRuntime(ctx, snapshot, definitions, options, nil, declaredRoutineKey{})
	}
	key := declaredRoutineKey{identity: identity, temporary: transactions.named.overlayVersion(), descriptors: options.Descriptors, routines: options.Routines, seeds: declaredRoutineSeeds(definitions)}
	return buildDeclaredTypeRuntime(ctx, snapshot, definitions, options, &transactions.declaredRoutines, key)
}

func NewDeclaredTypeRuntime(ctx context.Context, snapshot *catalog.CatalogSnapshot, options DeclaredTypeRuntimeOptions) (builtin.TypeResolver, error) {
	return newDeclaredTypeRuntime(ctx, snapshot, options, nil)
}

// DeclaredTypeRuntime returns the declared-type runtime for one snapshot. It is
// the entry point for a caller that holds the session: the definition scan and
// the routine discovery are shared with the session's other statements reading
// the same catalog content, while the resolver itself is always compiled for the
// caller's options, so the result is indistinguishable from a fresh scan.
func (transactions *Transactions) DeclaredTypeRuntime(ctx context.Context, snapshot *catalog.CatalogSnapshot, options DeclaredTypeRuntimeOptions) (builtin.TypeResolver, error) {
	return transactions.declaredTypeRuntime(ctx, snapshot, options)
}

func NewDeclaredTypeRuntimeWithUpdates(ctx context.Context, snapshot *catalog.CatalogSnapshot, options DeclaredTypeRuntimeOptions, updates []catalog.TypeUpdate) (builtin.TypeResolver, error) {
	replacements := make(map[model.ObjectID]*model.Descriptor, len(updates))
	for _, update := range updates {
		if update.Base == nil || update.Next == nil || update.Base.ID() != update.Next.ID() {
			return nil, model.ErrInvalidDescriptor
		}
		if update.Next.Kind() == model.ObjectType {
			replacements[update.Base.ID()] = update.Next
		}
	}
	return newDeclaredTypeRuntime(ctx, snapshot, options, replacements)
}

func newDeclaredTypeRuntime(ctx context.Context, snapshot *catalog.CatalogSnapshot, options DeclaredTypeRuntimeOptions, replacements map[model.ObjectID]*model.Descriptor) (builtin.TypeResolver, error) {
	if snapshot == nil {
		return nil, errPreparedInvocation
	}
	if err := options.Descriptors.Validate(); err != nil {
		return nil, err
	}
	definitions, err := scanDeclaredTypeDefinitions(ctx, snapshot, options.Descriptors, replacements)
	if err != nil {
		return nil, err
	}
	return buildDeclaredTypeRuntime(ctx, snapshot, definitions, options, nil, declaredRoutineKey{})
}

// scanDeclaredTypeDefinitions reads every published declared type through one
// namespace walk. It is the only O(catalog) I/O a declared-type runtime costs,
// so it is the part worth sharing between statements that read the same catalog
// content. A non-nil replacements overlay makes the result statement-specific,
// and such callers never share it.
func scanDeclaredTypeDefinitions(ctx context.Context, snapshot *catalog.CatalogSnapshot, limits model.DescriptorLimits, replacements map[model.ObjectID]*model.Descriptor) ([]builtin.TypeDefinition, error) {
	definitions := make([]builtin.TypeDefinition, 0, min(limits.MaxItems, 64))
	if err := snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(schema catalog.PublishedDescriptor) error {
		return snapshot.ScanNamespace(ctx, schema.Descriptor.ID(), model.ObjectType, func(published catalog.PublishedDescriptor) error {
			if len(definitions) == limits.MaxItems {
				return &model.Error{Code: "54000", Message: "declared type descriptor limit exceeded"}
			}
			descriptor := published.Descriptor
			committed := published.PublishedAt != 0
			if replacement := replacements[descriptor.ID()]; replacement != nil {
				descriptor = replacement
				committed = false
			}
			definitions = append(definitions, builtin.TypeDefinition{Descriptor: descriptor, Published: committed})
			return nil
		})
	}); err != nil {
		return nil, err
	}
	return definitions, nil
}

// buildDeclaredTypeRuntime compiles one runtime from already scanned definitions.
// Routine discovery reads the descriptors the definitions depend on; when cache is
// non-nil the discovered definitions are shared with statements reading the same
// catalog content, and the per-statement resolver is always compiled fresh.
func buildDeclaredTypeRuntime(ctx context.Context, snapshot *catalog.CatalogSnapshot, definitions []builtin.TypeDefinition, options DeclaredTypeRuntimeOptions, cache *declaredRoutineCache, key declaredRoutineKey) (builtin.TypeResolver, error) {
	discovered, cached := []procedural.Definition(nil), false
	if cache != nil {
		discovered, cached = cache.load(key)
	}
	if !cached {
		var err error
		discovered, err = discoverDeclaredRoutines(ctx, snapshot, definitions, options)
		if err != nil {
			return nil, err
		}
		// The discovery output is keyed by the identity the snapshot reported
		// before it ran. Storing it under that identity is valid only while the
		// snapshot still exposes it, so re-read the identity here: a snapshot
		// whose overlay grew mid-discovery is not cached under its old identity.
		if cache != nil && snapshot.CatalogIdentity() == key.identity {
			cache.store(key, discovered)
		}
	}
	routines, err := compileDeclaredRoutines(ctx, discovered, options)
	if err != nil {
		return nil, err
	}
	options.Expressions.Functions = routines
	compiler := &declaredTypeCompiler{snapshot: snapshot, options: options}
	return builtin.NewDeclaredTypeResolver(ctx, definitions, compiler, options.Descriptors)
}

// discoverDeclaredRoutines walks the routine descriptors reachable from the type
// definitions. It is the only step that reads catalog rows, and its output is a
// pure function of the catalog content, the seed descriptors, and the limits.
func discoverDeclaredRoutines(ctx context.Context, snapshot *catalog.CatalogSnapshot, types []builtin.TypeDefinition, options DeclaredTypeRuntimeOptions) ([]procedural.Definition, error) {
	objects := make(map[model.ObjectID]struct{})
	queue := make([]model.ObjectID, 0, min(options.Descriptors.MaxItems, 64))
	appendObject := func(object model.ObjectID) error {
		if object == 0 {
			return nil
		}
		if _, exists := objects[object]; exists {
			return nil
		}
		if len(objects) == options.Descriptors.MaxItems {
			return &model.Error{Code: "54000", Message: "declared type routine dependency limit exceeded"}
		}
		objects[object] = struct{}{}
		queue = append(queue, object)
		return nil
	}
	for _, definition := range types {
		spec, err := definition.Descriptor.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		for _, dependency := range spec.Dependencies {
			if err := appendObject(dependency.Object); err != nil {
				return nil, err
			}
		}
	}
	slices.Sort(queue)
	definitions := make([]procedural.Definition, 0, len(queue))
	for cursor := 0; cursor < len(queue); cursor++ {
		published, err := snapshot.Descriptor(ctx, queue[cursor])
		if err != nil {
			return nil, err
		}
		if published.Descriptor.Kind() != model.ObjectRoutine {
			continue
		}
		spec, err := published.Descriptor.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if spec.Routine == nil {
			return nil, model.ErrInvalidDescriptor
		}
		definitions = append(definitions, procedural.Definition{ID: model.FunctionID(published.Descriptor.OID()), Object: published.Descriptor.ID(), Metadata: *spec.Routine})
		for _, dependency := range spec.Dependencies {
			if err := appendObject(dependency.Object); err != nil {
				return nil, err
			}
		}
	}
	return definitions, nil
}

// compileDeclaredRoutines builds the per-statement routine resolver from the
// discovered definitions. It captures the statement's fallback and limits, never
// the snapshot.
func compileDeclaredRoutines(ctx context.Context, definitions []procedural.Definition, options DeclaredTypeRuntimeOptions) (expr.Resolver, error) {
	if len(definitions) == 0 {
		return options.Expressions.Functions, nil
	}
	return procedural.NewResolver(ctx, definitions, options.Expressions.Functions, options.Routines)
}

type declaredTypeCompiler struct {
	snapshot *catalog.CatalogSnapshot
	options  DeclaredTypeRuntimeOptions
}

func (c *declaredTypeCompiler) InformationSchemaTypeResolver() builtin.InformationSchemaTypeResolver {
	if c == nil {
		return nil
	}
	return c.options.Expressions.Builtin.InformationSchemaTypes
}

func (c *declaredTypeCompiler) CompileDomainCheck(ctx context.Context, types builtin.TypeResolver, domain *model.Descriptor, constraint model.ConstraintDescriptor, base model.TypeLayout) (builtin.TypeScalarCall, error) {
	if domain == nil || constraint.ID == 0 || constraint.Expression == nil {
		return nil, model.ErrInvalidDescriptor
	}
	options := c.options.Expressions
	options.Builtin.Types = types
	compiler, err := NewColumnExpressionCompiler(c.snapshot, options)
	if err != nil {
		return nil, err
	}
	binding, err := bind.BindDomainCheck(ctx, constraint.Expression, domain.ID(), constraint.ID, base, c.snapshot, options.Binding)
	if err != nil {
		return nil, err
	}
	columns := []storage.ColumnLayout{{ID: 1, Type: base.Type, Children: base.Children, Nullable: true}}
	program, err := compiler.compile(ctx, binding, columns, columnCheck)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, arguments []model.Value, budget builtin.CallBudget) (model.Value, error) {
		if len(arguments) != 1 || arguments[0].Type != base.Type.Storage {
			return model.Value{}, &model.Error{Code: "42804", Message: "domain CHECK value differs from its base type"}
		}
		result, err := program.evaluateWithin(ctx, arguments, budget)
		if err != nil {
			return model.Value{}, err
		}
		if result.IsNull || result.Type == model.TypeBoolean {
			return result, nil
		}
		return model.Value{}, &model.Error{Code: "42804", Message: "domain CHECK returned a non-boolean value"}
	}, nil
}

func (c *declaredTypeCompiler) ResolveTypeFunction(ctx context.Context, types builtin.TypeResolver, object model.ObjectID, id model.FunctionID, arguments []model.TypeLayout, result model.TypeLayout) (builtin.TypeScalarCall, error) {
	options := c.options.Expressions.Builtin
	options.Types = types
	if object == 0 {
		callable, err := builtin.ResolveTyped(ctx, id, arguments, result, options, c.options.Expressions.Evaluation.MaxBytes)
		if err != nil {
			return nil, err
		}
		return callable.Call, nil
	}
	published, err := c.snapshot.Descriptor(ctx, object)
	if err != nil {
		return nil, err
	}
	descriptor := published.Descriptor
	if descriptor.Kind() != model.ObjectRoutine || model.FunctionID(descriptor.OID()) != id {
		return nil, model.ErrInvalidDescriptor
	}
	spec, err := descriptor.SpecWithin(ctx, c.options.Descriptors)
	if err != nil {
		return nil, err
	}
	if spec.Routine == nil || c.options.Expressions.Functions == nil {
		return nil, &model.Error{Code: "42883", Message: "range support routine is unavailable in the pinned runtime"}
	}
	call, err := c.options.Expressions.Functions.Resolve(expr.Function{ID: id, Arguments: arguments, Result: result, Strict: spec.Routine.Strict})
	if err != nil {
		return nil, err
	}
	return builtin.TypeScalarCall(call), nil
}

// DeclaredTypeScans reports how many declared-type definition sets this session
// has scanned because no cached set matched. A no-DDL statement must not advance
// it once the set is warm.
func (transactions *Transactions) DeclaredTypeScans() uint64 {
	if transactions == nil {
		return 0
	}
	transactions.declaredTypes.mu.Lock()
	defer transactions.declaredTypes.mu.Unlock()
	return transactions.declaredTypes.scans
}

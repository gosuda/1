package session

import (
	"context"
	"errors"
	"io"
	"slices"
	"unsafe"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type routineReentryKey struct{}

func (transactions *Transactions) routineReentry(ctx context.Context) (*routineExecutor, bool) {
	executor, ok := ctx.Value(routineReentryKey{}).(*routineExecutor)
	if !ok || executor == nil || executor.transactions != transactions || executor.transaction != transactions.transaction || executor.frame == nil || executor.frame.view != executor.view || executor.scope == nil || !executor.scope.active.Load() {
		return nil, false
	}
	operation, ok := ctx.Value(sessionTransactionKey{}).(*sessionTransactionOperation)
	if !ok || operation.owner != transactions || !operation.active.Load() {
		return nil, false
	}
	return executor, true
}

func (transactions *Transactions) routineAtomicStatement(ctx context.Context, run func(context.Context) error) error {
	if run == nil || transactions.transaction == nil {
		return errSessionStatement
	}
	if _, ok := transactions.routineReentry(ctx); !ok {
		return transactions.transaction.Statement(ctx, run)
	}
	if err := transactions.transaction.RequireStatement(ctx); err != nil {
		return err
	}
	return transactions.transaction.RunSubtransaction(ctx, run)
}

func routineTypedStatement(statement *zpgparse.RawStmt) bool {
	if statement == nil || statement.Stmt == nil {
		return false
	}
	node := statement.Stmt
	if transaction := node.GetTransactionStmt(); transaction != nil {
		return true
	}
	if sessionCommandStatement(node) && node.GetDoStmt() == nil {
		return true
	}
	if isStatisticsCommand(node) || isTypeCommand(node) || isObjectCommand(node) || node.GetExplainStmt() != nil {
		return true
	}
	define := node.GetDefineStmt()
	if define != nil && (define.Kind == zpgparse.ObjectType_OBJECT_TSDICTIONARY || define.Kind == zpgparse.ObjectType_OBJECT_TSCONFIGURATION) {
		return true
	}
	if node.GetAlterTsdictionaryStmt() != nil || node.GetAlterTsconfigurationStmt() != nil || node.GetCreateFunctionStmt() != nil || node.GetCreateTrigStmt() != nil || bind.IsRoutineLifecycleCommand(node) {
		return true
	}
	alter := node.GetAlterTableStmt()
	drop := node.GetDropStmt()
	index := node.GetIndexStmt()
	return alter != nil && bind.SchemaJobAlter(alter) || drop != nil && drop.Concurrent || index != nil && index.Concurrent
}

func routineReentryNeedsQuery(node *zpgparse.Node) bool {
	if node == nil {
		return false
	}
	return node.GetExplainStmt() != nil || objectCommandRequiresPopulation(node) || node.GetExecuteStmt() != nil || node.GetDeclareCursorStmt() != nil || node.GetFetchStmt() != nil
}

// routineReentryReferencesParameters reports whether a reentered body statement
// names a routine datum. The body compiler rewrites every datum reference into a
// positional parameter, so a ParamRef anywhere in the tree is exactly the set of
// datum values the statement consumes.
//
// Needing a query and needing parameters are different questions. A statement
// that needs a query but names no datum -- CREATE TABLE ... PARTITION OF, SET
// CONSTRAINTS, LOCK -- must not receive the enclosing routine's datum values:
// utility and object commands reject any parameter with 42P02, which is what
// PostgreSQL does for parameters in utility statements. Supplying parameters
// only when the statement references one keeps that rejection meaningful for
// genuinely parameterised prepared invocations.
func routineReentryReferencesParameters(node *zpgparse.Node) bool {
	if node == nil {
		return false
	}
	found := false
	var walk func(message protoreflect.Message)
	walk = func(message protoreflect.Message) {
		if found || message == nil || !message.IsValid() {
			return
		}
		if current, ok := message.Interface().(*zpgparse.Node); ok && current.GetParamRef() != nil {
			found = true
			return
		}
		fields := message.Descriptor().Fields()
		for i := range fields.Len() {
			field := fields.Get(i)
			if field.IsMap() || field.Kind() != protoreflect.MessageKind && field.Kind() != protoreflect.GroupKind {
				continue
			}
			if field.IsList() {
				list := message.Get(field).List()
				for j := range list.Len() {
					walk(list.Get(j).Message())
				}
			} else if message.Has(field) {
				walk(message.Get(field).Message())
			}
		}
	}
	walk(node.ProtoReflect())
	return found
}

func (executor *routineExecutor) executeReenteredStatement(ctx context.Context, request procedural.SQLRequest) (result procedural.SQLResult, err error) {
	if request.Statement == nil || request.Statement.Stmt == nil {
		return result, errPreparedInvocation
	}
	if request.Statement.Stmt.GetTransactionStmt() != nil {
		return result, settingError("2D000", "invalid transaction termination")
	}
	var checkBatch func(context.Context, *columnar.Batch) error
	hadReturning := true
	if executor.options.RewriteStatement != nil {
		var rewritten *zpgparse.RawStmt
		rewritten, checkBatch, hadReturning, err = executor.options.RewriteStatement(ctx, request.Statement)
		if err != nil {
			return result, err
		}
		if rewritten != nil {
			request.Statement = rewritten
		}
	}
	execution := executor.execution
	execution.Dependencies = nil
	execution.Routines = nil
	execution.Copy = nil
	execution.Environment = executor.environment
	execution.Environment.Query = nil
	execution.Environment.Reader = nil
	execution.Environment.CopySource = nil
	execution.Environment.Modify.Transaction = nil
	execution.Environment.Modify.ConstraintTransaction = nil
	execution.Environment.Modify.Deferral = nil
	if execution.Schema != nil {
		schema := *execution.Schema
		schema.Snapshot = executor.frame.snapshot
		schema.Online = nil
		schema.StatementIndex = 0
		execution.Schema = &schema
	}
	parameterLayouts := []model.TypeLayout(nil)
	needsQuery := routineReentryNeedsQuery(request.Statement.Stmt)
	if needsQuery {
		execution.Environment.Query, err = executor.options.NewQuery(ctx)
		if err != nil {
			return result, err
		}
		defer func() { err = errors.Join(err, execution.Environment.Query.Close()) }()
	}
	// Needing a query does not imply needing parameters. A statement that names
	// no routine datum must arrive parameterless, or utility and object commands
	// reject it with 42P02 even though the enclosing routine had values to carry.
	if needsQuery && routineReentryReferencesParameters(request.Statement.Stmt) {
		parameterLayouts = make([]model.TypeLayout, len(request.Parameters))
		execution.Environment.Parameters = make([]expr.ParameterValue, len(request.Parameters))
		for i, parameter := range request.Parameters {
			parameterLayouts[i] = parameter.Layout
			execution.Environment.Parameters[i] = expr.ParameterValue{Number: uint32(i + 1), Layout: parameter.Layout, Value: parameter.Value}
		}
	} else {
		execution.Environment.Parameters = nil
	}
	statementOptions := StatementExecution{
		Execution:  execution,
		Parameters: parameterLayouts,
		Planning: func(ctx context.Context, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader) (PrepareOptions, error) {
			return executor.options.Planning(ctx, snapshot, reader, request.Statement, parameterLayouts)
		},
	}
	used := uint64(0)
	consume := func(ctx context.Context, prepared *Prepared, rows *Rows) error {
		columns := prepared.Columns()
		if checkBatch != nil {
			if !hadReturning {
				columns = nil
			} else if len(columns) > 0 {
				columns = columns[:len(columns)-1]
			}
		}
		result.Columns = make([]model.TypeLayout, len(columns))
		result.ColumnNames = make([]string, len(columns))
		for i := range columns {
			result.Columns[i] = columns[i].Layout
			result.ColumnNames[i] = columns[i].Name
		}
		used = uint64(len(result.Columns)) * uint64(unsafe.Sizeof(model.TypeLayout{}))
		if used > request.MaxBytes {
			return procedural.ErrLimit
		}
		for {
			remaining := request.MaxRows - len(result.Rows)
			batchLimit := min(remaining+1, columnar.DefaultBatchSize)
			if checkBatch != nil && !hadReturning {
				batchLimit = columnar.DefaultBatchSize
			}
			batch := columnar.Batch{MaxRows: batchLimit}
			nextErr := rows.Next(ctx, &batch)
			if nextErr == io.EOF {
				return nil
			}
			if nextErr != nil {
				return nextErr
			}
			if checkBatch != nil {
				if err := checkBatch(ctx, &batch); err != nil {
					return err
				}
				if !hadReturning {
					continue
				}
			}
			for rowIndex := range batch.Rows() {
				if len(result.Rows) >= request.MaxRows {
					return procedural.ErrLimit
				}
				header := uint64(len(batch.Columns)) * uint64(unsafe.Sizeof(model.Value{}))
				if header > request.MaxBytes-used {
					return procedural.ErrLimit
				}
				used += header
				row := make([]model.Value, len(batch.Columns))
				lane := batch.Lane(rowIndex)
				for columnIndex := range batch.Columns {
					value, size, copyErr := columnar.CopyValue(ctx, &batch.Columns[columnIndex], lane, request.MaxBytes-used)
					if copyErr != nil {
						return errors.Join(copyErr, procedural.ErrLimit)
					}
					used += size
					row[columnIndex] = value
				}
				result.Rows = append(result.Rows, row)
			}
		}
	}
	if needsQuery {
		frame := executor.retainFrame()
		defer func() { err = errors.Join(err, executor.releaseFrame(frame)) }()
	}
	reentry := context.WithValue(ctx, routineReentryKey{}, executor)
	executed, runErr := executor.transactions.RunStatement(reentry, request.Statement, statementOptions, consume)
	if runErr != nil {
		return result, runErr
	}
	result.RowsAffected = executed.RowsAffected
	for _, warning := range executed.Warnings {
		executor.Report(ctx, warning)
	}
	if refreshErr := executor.replaceFrame(ctx, executor.view, executor.frame.ownedView); refreshErr != nil {
		return result, refreshErr
	}
	return result, nil
}

// catalogDiscoveryDependencies returns base plus the catalog read set that binding
// a statement can touch. Discovery reads namespace name indexes and descriptor
// rows at one snapshot; the exact descriptor rows and per-kind namespace spans are
// then declared and the topology re-checked, so a concurrent catalog change cannot
// leave the declaration narrower than what binding reads. This mirrors the
// protected discovery used by table declarations and type cascades.
//
// The discovered suffix is a pure function of the catalog content the discovery
// snapshot exposes and of the budget. Its locks are still acquired on every call:
// a cached suffix is declared under a fresh view, and it is served only while that
// view reports the identity it was discovered under. See catalogDiscoveryCache.
func catalogDiscoveryDependencies(ctx context.Context, transaction *txn.Transaction, store *catalog.Store, discovery catalogDiscovery, base []txn.ReadDependency, maximum uint64) ([]txn.ReadDependency, error) {
	if transaction == nil || store == nil || maximum == 0 {
		return nil, errPreparedInvocation
	}
	root, err := store.NamespaceScanDependency(ctx, 0, model.ObjectSchema)
	if err != nil {
		return nil, err
	}
	remaining := maximum
	charge := func(bytes uint64) error {
		if bytes > remaining {
			return model.ErrValueBudget
		}
		remaining -= bytes
		return nil
	}
	header := uint64(unsafe.Sizeof(txn.ReadDependency{}))
	if err := charge(uint64(len(base)) * header); err != nil {
		return nil, err
	}
	if err := charge(header); err != nil {
		return nil, err
	}
	// A caller that carries no cache keeps the plain walk: one scan view, one
	// declaration view, no probe.
	if discovery.cache == nil {
		dependencies, _, _, _, err := walkCatalogDiscovery(ctx, transaction, store, root, base, remaining)
		return dependencies, err
	}
	// The probe declares only base and the schema root, as the walk's first phase
	// does, and reads no rows: it names the content the walk would discover.
	var identity txn.CatalogReadIdentity
	probe := append(slices.Clone(base), root)
	if err := inspectDeclarationCatalog(ctx, transaction, store, probe, func(snapshot *catalog.CatalogSnapshot) error {
		identity = snapshot.CatalogIdentity()
		return nil
	}); err != nil {
		return nil, err
	}
	// A snapshot whose reader cannot name its content is never cached: nothing
	// proves the cached suffix describes the rows this statement will read.
	if identity == (txn.CatalogReadIdentity{}) {
		dependencies, _, _, _, err := walkCatalogDiscovery(ctx, transaction, store, root, base, remaining)
		return dependencies, err
	}
	key := catalogDiscoveryKey{identity: identity, temporary: discovery.temporary}
	if entry, ok := discovery.cache.load(key); ok && entry.content <= remaining {
		dependencies, verified, err := declareCachedDiscovery(ctx, transaction, store, base, entry.suffix, identity)
		if err != nil || verified {
			return dependencies, err
		}
	}
	dependencies, content, walked, stable, err := walkCatalogDiscovery(ctx, transaction, store, root, base, remaining)
	if err != nil {
		return nil, err
	}
	// The suffix describes the content named by `identity`, so it is cacheable
	// only when the scan phase, the declaration phase, and the probe all observed
	// that same content.
	if stable && walked == identity {
		discovery.cache.store(catalogDiscoveryEntry{key: key, suffix: cloneCatalogDependencies(dependencies[len(base):]), content: content})
	}
	return dependencies, nil
}

// declareCachedDiscovery declares base plus a cached suffix under a fresh view and
// confirms the view exposes the identity the suffix was discovered under. The
// caller falls back to a walk when the identity has moved.
func declareCachedDiscovery(ctx context.Context, transaction *txn.Transaction, store *catalog.Store, base, suffix []txn.ReadDependency, identity txn.CatalogReadIdentity) ([]txn.ReadDependency, bool, error) {
	dependencies := append(slices.Clone(base), cloneCatalogDependencies(suffix)...)
	var observed txn.CatalogReadIdentity
	if err := inspectDeclarationCatalog(ctx, transaction, store, dependencies, func(snapshot *catalog.CatalogSnapshot) error {
		observed = snapshot.CatalogIdentity()
		return nil
	}); err != nil {
		return nil, false, err
	}
	if observed != identity {
		return nil, false, nil
	}
	return dependencies, true, nil
}

// walkCatalogDiscovery scans the catalog and returns base plus the discovered
// suffix, the budget the suffix charged, the identity the declaration phase ran
// under, and whether the scan and declaration phases observed the same identity.
func walkCatalogDiscovery(ctx context.Context, transaction *txn.Transaction, store *catalog.Store, root txn.ReadDependency, base []txn.ReadDependency, remaining uint64) ([]txn.ReadDependency, uint64, txn.CatalogReadIdentity, bool, error) {
	var identity txn.CatalogReadIdentity
	before := remaining
	charge := func(bytes uint64) error {
		if bytes > remaining {
			return model.ErrValueBudget
		}
		remaining -= bytes
		return nil
	}
	const dependencyBytes = uint64(512)
	var scanned txn.CatalogReadIdentity
	schemas := make([]catalog.NamespaceReference, 0)
	scanView := append(slices.Clone(base), root)
	err := inspectDeclarationCatalog(ctx, transaction, store, scanView, func(snapshot *catalog.CatalogSnapshot) error {
		scanned = snapshot.CatalogIdentity()
		return snapshot.ScanNamespaceReferences(ctx, 0, model.ObjectSchema, func(reference catalog.NamespaceReference) error {
			if reference.ID == 0 || reference.ID >= catalog.NamespaceObjectID {
				return catalog.ErrCorruptCatalog
			}
			names := uint64(len(reference.Name.Name)) + uint64(len(reference.Name.Signature))
			if err := charge(dependencyBytes + names); err != nil {
				return err
			}
			schemas = append(schemas, reference)
			return nil
		})
	})
	if err != nil {
		return nil, 0, identity, false, err
	}
	dependencies := append(slices.Clone(base), root)
	// Execution resolves catalog objects by OID (regclass casts, sequence and
	// introspection builtins, capability frames) through the OID index. Those
	// OIDs are runtime values, so the index is declared whole; the descriptor
	// rows it reports are declared per discovered object below.
	if err := charge(dependencyBytes); err != nil {
		return nil, 0, identity, false, err
	}
	dependencies = append(dependencies, store.OIDScanDependency())
	seen := make(map[model.ObjectID]struct{}, len(schemas))
	appendDescriptor := func(id model.ObjectID) error {
		if id == 0 || id >= catalog.NamespaceObjectID {
			return catalog.ErrCorruptCatalog
		}
		if _, exists := seen[id]; exists {
			return nil
		}
		if err := charge(dependencyBytes); err != nil {
			return err
		}
		seen[id] = struct{}{}
		dependencies = append(dependencies, store.DescriptorDependency(id))
		return nil
	}
	for _, schema := range schemas {
		if err := appendDescriptor(schema.ID); err != nil {
			return nil, 0, identity, false, err
		}
		for _, kind := range catalogDiscoveryKinds {
			dependency, err := store.NamespaceScanDependency(ctx, schema.ID, kind)
			if err != nil {
				return nil, 0, identity, false, err
			}
			if err := charge(dependencyBytes); err != nil {
				return nil, 0, identity, false, err
			}
			dependencies = append(dependencies, dependency)
		}
	}
	// The capability frame the routine later runs under builds declared-type,
	// relation, sequence, and text-search runtimes. Constructing that frame
	// resolves user objects by name, and each resolution reads the object's
	// descriptor row. The per-kind namespace spans above cover the name-index
	// reads only, so declare the descriptor rows those scans report as well:
	// any object the frame can resolve by name lives in one of these namespaces,
	// and descriptors reached through dependency edges are themselves such
	// objects.
	err = inspectDeclarationCatalog(ctx, transaction, store, dependencies, func(snapshot *catalog.CatalogSnapshot) error {
		identity = snapshot.CatalogIdentity()
		position := 0
		if err := snapshot.ScanNamespaceReferences(ctx, 0, model.ObjectSchema, func(reference catalog.NamespaceReference) error {
			if position >= len(schemas) || schemas[position] != reference {
				return catalog.ErrDescriptorChanged
			}
			position++
			return nil
		}); err != nil {
			return err
		}
		if position != len(schemas) {
			return catalog.ErrDescriptorChanged
		}
		for _, schema := range schemas {
			for _, kind := range catalogDiscoveryKinds {
				if err := snapshot.ScanNamespaceReferences(ctx, schema.ID, kind, func(reference catalog.NamespaceReference) error {
					return appendDescriptor(reference.ID)
				}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, 0, identity, false, err
	}
	return dependencies, before - remaining, identity, scanned == identity, nil
}

// catalogDiscoveryKinds lists the namespaces binding walks by name while
// binding a routine-body statement. It matches the type cascade's kind set so the
// body can resolve every kind of object a routine may name.
var catalogDiscoveryKinds = [...]model.ObjectKind{
	model.ObjectTable, model.ObjectIndex, model.ObjectSequence, model.ObjectView,
	model.ObjectMaterializedView, model.ObjectRoutine, model.ObjectType, model.ObjectCompositeRelation,
	model.ObjectTextSearchDictionary, model.ObjectTextSearchConfiguration, model.ObjectTextSearchTemplate,
	model.ObjectCast, model.ObjectOperator,
}

func (executor *routineExecutor) installDefinitions(ctx context.Context, definitions []procedural.Definition) error {
	updated := append([]procedural.Definition(nil), executor.definitions...)
	knownObjects := make(map[model.ObjectID]struct{}, len(updated))
	knownFunctions := make(map[model.FunctionID]struct{}, len(updated))
	for _, definition := range updated {
		knownObjects[definition.Object] = struct{}{}
		knownFunctions[definition.ID] = struct{}{}
	}
	for _, definition := range definitions {
		if _, exists := knownObjects[definition.Object]; exists {
			continue
		}
		if _, exists := knownFunctions[definition.ID]; exists {
			continue
		}
		updated = append(updated, definition)
		knownObjects[definition.Object] = struct{}{}
		knownFunctions[definition.ID] = struct{}{}
	}
	resolver, err := procedural.NewResolver(ctx, updated, executor.baseFunctions, executor.options.Limits)
	if err != nil {
		return err
	}
	triggers, err := procedural.NewTriggerRuntime(resolver, executor.options.Limits)
	if err != nil {
		return err
	}
	executor.definitions = updated
	executor.environment.Functions = resolver
	executor.environment.Triggers = triggers
	if executor.frame != nil {
		executor.frame.definitions = updated
		executor.frame.environment.Functions = resolver
		executor.frame.environment.Triggers = triggers
	}
	return nil
}

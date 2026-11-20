package session

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
)

// RoutineTransactionCapabilities is the session-owned composition root for each
// transaction segment entered by a non-atomic routine. Sequence is borrowed for
// the session lifetime; each frame owns its snapshot and replacement read view.
type RoutineTransactionCapabilities struct {
	Sequence             *SequenceSession
	CatalogRelations     *catalogrel.Registry
	TextSearchRewrite    TextSearchRewrite
	TextSearchStatistics TextSearchStatistics
	// BaseFunctions, BaseBuiltin, and BaseKeys are immutable database roots.
	// They must not capture a transaction snapshot or session operation token.
	BaseFunctions expr.Resolver
	BaseBuiltin   builtin.ResolveOptions
	BaseKeys      *builtin.KeyEncoder
	PathBytes     uint64
}

func (options RoutineTransactionCapabilities) validate(execution PreparedExecution) error {
	if options.Sequence == nil || options.CatalogRelations == nil || !options.CatalogRelations.Complete() || options.BaseFunctions == nil || options.BaseBuiltin.Collation == 0 || options.BaseBuiltin.DefaultCollation == 0 || options.BaseKeys == nil || options.PathBytes == 0 || execution.TypeRuntime == nil {
		return errPreparedInvocation
	}
	return execution.TypeRuntime.Descriptors.Validate()
}

type routineCapabilityFrame struct {
	view        txn.ReadView
	snapshot    *catalog.CatalogSnapshot
	ownedView   bool
	borrowers   uint32
	retired     bool
	definitions []procedural.Definition
	environment exec.Environment
}

func (frame *routineCapabilityFrame) close() error {
	if frame == nil {
		return nil
	}
	err := error(nil)
	if frame.snapshot != nil {
		err = frame.snapshot.Close()
		frame.snapshot = nil
	}
	if frame.ownedView && frame.view != nil {
		err = errors.Join(err, frame.view.Close())
	}
	frame.view = nil
	frame.environment.Reader = nil
	frame.environment.Catalog = nil
	frame.environment.Relations = nil
	frame.environment.Sequences = nil
	frame.environment.TextSearch = nil
	frame.environment.Settings = nil
	frame.environment.Functions = nil
	frame.environment.Triggers = nil
	frame.environment.Native = model.NativeScalarRuntime{}
	clear(frame.definitions)
	frame.definitions = nil
	frame.environment.Builtin.Types = nil
	// The aggregate resolver borrows the snapshot this frame just closed, so the
	// frame must drop it rather than leave a dangling reference behind.
	frame.environment.Aggregate.Aggregates = nil
	return err
}

func (executor *routineExecutor) buildCapabilityFrame(ctx context.Context, view txn.ReadView, ownedView bool) (_ *routineCapabilityFrame, err error) {
	if executor == nil || view == nil || executor.catalog == nil || executor.execution.TypeRuntime == nil {
		return nil, errPreparedInvocation
	}
	snapshot, err := executor.catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, snapshot.Close())
		}
	}()
	if err = addTemporaryOverlay(snapshot, &executor.transactions.named); err != nil {
		return nil, err
	}
	configured := executor.options.Capabilities
	path := executor.transactions.settings.SearchPath()
	builtins := executor.transactions.settings.BuiltinOptions(executor.baseBuiltin.Collation, executor.baseBuiltin.DefaultCollation)
	builtins.Types = executor.baseBuiltin.Types
	builtins.InformationSchemaFunctions = configured.CatalogRelations
	builtins.InformationSchemaTypes = configured.CatalogRelations
	typeOptions := *executor.execution.TypeRuntime
	typeOptions.Expressions.Builtin = builtins
	typeOptions.Expressions.Functions = executor.baseFunctions
	typeOptions.Routines = executor.options.Limits
	types, err := executor.transactions.declaredTypeRuntime(ctx, snapshot, typeOptions)
	if err != nil {
		return nil, err
	}
	builtins.Types = types
	relations, err := NewRelationRuntime(snapshot, configured.CatalogRelations, path, configured.PathBytes)
	if err != nil {
		return nil, err
	}
	sequences, err := NewSequenceRuntime(snapshot, configured.Sequence)
	if err != nil {
		return nil, err
	}
	if executor.transactions.settings.TransactionPolicy().ReadOnly {
		sequences = readOnlySequences{SequenceRuntime: sequences}
	}
	textSearch, err := NewTextSearchRuntime(snapshot, path, configured.PathBytes, configured.TextSearchRewrite, configured.TextSearchStatistics)
	if err != nil {
		return nil, err
	}
	definitions, err := reloadRoutineDefinitions(ctx, snapshot, executor.definitions, executor.options.Limits.MaxStatements)
	if err != nil {
		return nil, err
	}
	functions, err := procedural.NewResolver(ctx, definitions, executor.baseFunctions, executor.options.Limits)
	if err != nil {
		return nil, err
	}
	triggers, err := procedural.NewTriggerRuntime(functions, executor.options.Limits)
	if err != nil {
		return nil, err
	}
	aggregates := newAggregateRuntime(snapshot, functions, builtins)
	scanner, err := catalogrel.AttachSettings(executor.sourceCatalog, executor.catalogSettings)
	if err != nil {
		return nil, err
	}
	scanner, err = attachCatalogView(scanner, view)
	if err != nil {
		return nil, err
	}
	environment := executor.template
	environment.Reader = view
	environment.Catalog = scanner
	environment.Builtin = builtins
	environment.Keys = executor.baseKeys.WithResolveOptions(builtins)
	environment.Functions = functions
	environment.Triggers = triggers
	environment.Sequences = sequences
	environment.Relations = relations
	environment.TextSearch = textSearch
	environment.Settings = executor.settings
	environment.Temporal = executor.transactions.temporal
	environment.Advisory = executor.transactions.advisoryRuntime()
	environment.Native.Random = executor.transactions
	environment.Native.Database, _ = environment.Catalog.(model.DatabaseRuntime)
	environment.Native.TypeInput = newTypeInputRuntime(relations, builtins, environment.Temporal, environment.Native)
	environment.Native.Triggers = triggers
	environment.Aggregate.Aggregates = aggregates
	return &routineCapabilityFrame{view: view, snapshot: snapshot, ownedView: ownedView, definitions: definitions, environment: environment}, nil
}

func (executor *routineExecutor) installFrame(frame *routineCapabilityFrame) {
	executor.frame = frame
	executor.definitions = frame.definitions
	executor.view = frame.view
	executor.environment = frame.environment
	executor.transaction = executor.transactions.transaction
	executor.scope.transaction = executor.transaction
}

func (executor *routineExecutor) replaceFrame(ctx context.Context, view txn.ReadView, ownedView bool) error {
	current := executor.frame
	frame, err := executor.buildCapabilityFrame(ctx, view, ownedView)
	if err != nil {
		if ownedView && (current == nil || current.view != view) {
			err = errors.Join(err, view.Close())
		}
		return err
	}
	previous := current
	if previous == nil {
		return errors.Join(errPreparedInvocation, frame.close())
	}
	if previous.view == view {
		previous.ownedView = false
	}
	executor.installFrame(frame)
	previous.retired = true
	if previous.borrowers != 0 {
		executor.retiredFrames = append(executor.retiredFrames, previous)
		return nil
	}
	return previous.close()
}

func (executor *routineExecutor) retainFrame() *routineCapabilityFrame {
	frame := executor.frame
	frame.borrowers++
	return frame
}

func (executor *routineExecutor) releaseFrame(frame *routineCapabilityFrame) error {
	if frame == nil || frame.borrowers == 0 {
		return errPreparedInvocation
	}
	frame.borrowers--
	if frame.borrowers != 0 || !frame.retired {
		return nil
	}
	for index, retired := range executor.retiredFrames {
		if retired != frame {
			continue
		}
		copy(executor.retiredFrames[index:], executor.retiredFrames[index+1:])
		executor.retiredFrames[len(executor.retiredFrames)-1] = nil
		executor.retiredFrames = executor.retiredFrames[:len(executor.retiredFrames)-1]
		break
	}
	return frame.close()
}

func (executor *routineExecutor) closeCapabilities() error {
	if executor == nil {
		return nil
	}
	frame := executor.frame
	executor.frame = nil
	err := error(nil)
	for _, retired := range executor.retiredFrames {
		err = errors.Join(err, retired.close())
	}
	clear(executor.retiredFrames)
	executor.retiredFrames = nil
	return errors.Join(err, frame.close())
}

// applyRoutineCapabilities transfers the frame's execution collaborators to the
// caller's environment. The routine body reads its own environment from the
// executor, so only the collaborators the caller's plan needs are adopted.
// Builtin keeps the caller's declared-type and information-schema identity: those
// are part of the semantics the caller's statement was bound against, and
// Prepared.Open verifies them before execution. Overwriting them with the frame's
// freshly built runtime would reject an otherwise valid plan.
func applyRoutineCapabilities(target *exec.Environment, source exec.Environment) {
	query := target.Query
	borrowQuery := target.BorrowQuery
	instrumentation := target.Instrumentation
	parameters := target.Parameters
	copySource := target.CopySource
	types := target.Builtin.Types
	informationSchemaTypes := target.Builtin.InformationSchemaTypes
	informationSchemaFunctions := target.Builtin.InformationSchemaFunctions
	*target = source
	target.Query = query
	target.BorrowQuery = borrowQuery
	target.Instrumentation = instrumentation
	target.Parameters = parameters
	target.CopySource = copySource
	target.Builtin.Types = types
	target.Builtin.InformationSchemaTypes = informationSchemaTypes
	target.Builtin.InformationSchemaFunctions = informationSchemaFunctions
}

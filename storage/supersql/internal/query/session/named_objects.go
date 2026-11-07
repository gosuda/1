package session

import (
	"context"
	"errors"
	"math"
	"slices"
	"time"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	execution "github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
)

// SessionNamedObjectOptions bounds session-owned prepared metadata and cursors.
// NewQuery creates a query owned by a cursor or parameter evaluation; it must
// accept a session-lifetime context and return a fresh query on every call.
// PlanningCostUnitsPerSecond calibrates measured planning time against optimizer
// cost units for the per-statement generic/custom policy.
type SessionNamedObjectOptions struct {
	NewQuery                   func(context.Context) (*runtime.Query, error)
	DiscardTemporary           func(context.Context, *txn.Transaction) error
	PlanningCostUnitsPerSecond float64
	MaxPrepared                int
	MaxCursors                 int
	MetadataBytes              uint64
}

type namedPreparedStatement struct {
	dependencies      []txn.ReadDependency
	name              string
	statement         string
	searchPath        []string
	semantics         builtin.ResolveOptions
	parameterLayouts  []model.TypeLayout
	analysis          *Prepared
	generic           *Prepared
	policy            opt.ParameterPolicy
	parameterTypes    []model.TypeID
	resultColumns     []bind.ResultColumn
	resultTypes       []model.TypeID
	preparedAt        time.Time
	genericPlans      uint64
	customPlans       uint64
	metadataBytes     uint64
	genericBytes      uint64
	policyCustomPlans uint32
}

type temporaryCatalogChange struct {
	before *model.Descriptor
	after  *model.Descriptor
}

type sessionNamedObjects struct {
	options        SessionNamedObjectOptions
	prepared       map[string]*namedPreparedStatement
	cursors        map[string]*namedCursor
	metadataBytes  uint64
	cursorSerial   uint64
	temporary      map[model.ObjectID]*model.Descriptor
	temporaryNames map[model.NamespaceName]model.ObjectID
	// temporaryIDs retains every id this session ever declared temporary, so a
	// drop still recognizes the object as session-local after the descriptor
	// leaves the overlay.
	temporaryIDs map[model.ObjectID]struct{}
	// temporaryTargets carries the write policy of each live session-temporary
	// relation: its descriptor, index expansion and row normalizer. Nothing
	// outside the session can rebuild them, because temporary descriptors are
	// never published.
	temporaryTargets map[model.ObjectID]execution.MutationTarget
	// temporaryRows carries the row normalizer of each live session-temporary
	// relation and of each index over it: the transaction's write path resolves
	// its policy by physical object id, and no catalog row can supply one.
	temporaryRows    map[model.ObjectID]*txn.RowNormalizer
	temporaryChanges []temporaryCatalogChange
	temporaryMarks   []int
	temporaryOptions *ObjectCommandOptions
	// temporaryVersion counts every change to the session's temporary object
	// set. Metadata derived from a snapshot's session overlay is keyed by it,
	// because the overlay is the only part of that snapshot the session owns.
	temporaryVersion uint64
}

var errNamedObjectsDisabled = &model.Error{Code: "55000", Message: "session named objects are not configured"}

func newSessionNamedObjects(options SessionNamedObjectOptions) (sessionNamedObjects, error) {
	allZero := options.NewQuery == nil && options.DiscardTemporary == nil && options.MaxPrepared == 0 && options.MaxCursors == 0 && options.MetadataBytes == 0 && options.PlanningCostUnitsPerSecond == 0
	if allZero {
		return sessionNamedObjects{}, nil
	}
	if options.NewQuery == nil || options.MaxPrepared <= 0 || options.MaxCursors <= 0 || options.MetadataBytes == 0 ||
		options.PlanningCostUnitsPerSecond <= 0 || math.IsNaN(options.PlanningCostUnitsPerSecond) || math.IsInf(options.PlanningCostUnitsPerSecond, 0) {
		return sessionNamedObjects{}, &model.Error{Code: "22023", Message: "named session objects require a query factory and positive finite limits"}
	}
	return sessionNamedObjects{
		options:  options,
		prepared: make(map[string]*namedPreparedStatement, options.MaxPrepared),
		cursors:  make(map[string]*namedCursor, options.MaxCursors),
	}, nil
}

func (named *sessionNamedObjects) enabled() bool {
	return named != nil && named.options.NewQuery != nil
}

func (named *sessionNamedObjects) reserve(bytes uint64) error {
	if !named.enabled() {
		return errNamedObjectsDisabled
	}
	if bytes > named.options.MetadataBytes-named.metadataBytes {
		return &model.Error{Code: "54000", Message: "session named object metadata limit exceeded"}
	}
	named.metadataBytes += bytes
	return nil
}

func (named *sessionNamedObjects) release(bytes uint64) {
	if bytes > named.metadataBytes {
		named.metadataBytes = 0
		return
	}
	named.metadataBytes -= bytes
}

func namedPreparedBytes(statement *namedPreparedStatement) uint64 {
	bytes := uint64(unsafe.Sizeof(namedPreparedStatement{})) + uint64(len(statement.name)+len(statement.statement))
	bytes += uint64(cap(statement.searchPath)) * uint64(unsafe.Sizeof(""))
	for _, name := range statement.searchPath {
		bytes += uint64(len(name))
	}
	bytes += uint64(cap(statement.parameterLayouts)) * uint64(unsafe.Sizeof(model.TypeLayout{}))
	bytes += uint64(cap(statement.parameterTypes)+cap(statement.resultTypes)) * uint64(unsafe.Sizeof(model.TypeID(0)))
	bytes += uint64(cap(statement.resultColumns)) * uint64(unsafe.Sizeof(bind.ResultColumn{}))
	for _, column := range statement.resultColumns {
		bytes += uint64(len(column.Name))
	}
	if statement.analysis != nil {
		bytes = saturatingNamedBytes(bytes, uint64(unsafe.Sizeof(Prepared{})))
		if statement.analysis.bound != nil {
			bytes = saturatingNamedBytes(bytes, uint64(cap(statement.analysis.statistics))*uint64(unsafe.Sizeof(opt.StatisticVersion{})))
			bytes = saturatingNamedBytes(bytes, uint64(unsafe.Sizeof(*statement.analysis.bound)))
			if graph := statement.analysis.bound.Graph(); graph != nil {
				bytes = saturatingNamedBytes(bytes, uint64(unsafe.Sizeof(*graph)))
				bytes = saturatingNamedBytes(bytes, graph.OwnedBytes())
			}
		}
	}
	return saturatingNamedBytes(bytes, statement.genericBytes)
}

func preparedCachedPlanBytes(prepared *Prepared) uint64 {
	if prepared == nil || prepared.graph == nil {
		return 0
	}
	bytes := uint64(unsafe.Sizeof(Prepared{}))
	bytes = saturatingNamedBytes(bytes, uint64(cap(prepared.statistics))*uint64(unsafe.Sizeof(opt.StatisticVersion{})))
	bytes = saturatingNamedBytes(bytes, prepared.graph.OwnedBytes())
	if plan := prepared.graph.Plan(); plan != nil {
		bytes = saturatingNamedBytes(bytes, plan.OwnedBytes())
	}
	return bytes
}

func saturatingNamedBytes(left, right uint64) uint64 {
	if right > math.MaxUint64-left {
		return math.MaxUint64
	}
	return left + right
}

func copyNamedDependencies(ctx context.Context, input []txn.ReadDependency, maximum uint64) ([]txn.ReadDependency, uint64, error) {
	header := uint64(len(input)) * uint64(unsafe.Sizeof(txn.ReadDependency{}))
	if header > maximum {
		return nil, 0, model.ErrValueBudget
	}
	used := header
	result := make([]txn.ReadDependency, len(input))
	for i, dependency := range input {
		result[i].Target = dependency.Target
		keyBytes := uint64(len(dependency.KeyLayout)) * uint64(unsafe.Sizeof(storage.KeyColumn{}))
		spanBytes := uint64(len(dependency.Spans)) * uint64(unsafe.Sizeof(storage.KeySpan{}))
		if keyBytes > maximum-used || spanBytes > maximum-used-keyBytes {
			return nil, 0, model.ErrValueBudget
		}
		used += keyBytes + spanBytes
		result[i].KeyLayout = make([]storage.KeyColumn, len(dependency.KeyLayout))
		for j, key := range dependency.KeyLayout {
			childHeaders := uint64(len(key.Children)) * uint64(unsafe.Sizeof(model.TypeLayout{}))
			if childHeaders > maximum-used {
				return nil, 0, model.ErrValueBudget
			}
			used += childHeaders
			result[i].KeyLayout[j] = key
			result[i].KeyLayout[j].Children = make([]model.TypeLayout, len(key.Children))
			for k, child := range key.Children {
				layout, bytes, err := model.CopyTypeLayout(ctx, child, maximum-used)
				if err != nil {
					return nil, 0, err
				}
				result[i].KeyLayout[j].Children[k] = layout
				used += bytes
			}
		}
		result[i].Spans = make([]storage.KeySpan, len(dependency.Spans))
		for j, span := range dependency.Spans {
			result[i].Spans[j].Lower.Inclusive = span.Lower.Inclusive
			result[i].Spans[j].Lower.Unbounded = span.Lower.Unbounded
			result[i].Spans[j].Upper.Inclusive = span.Upper.Inclusive
			result[i].Spans[j].Upper.Unbounded = span.Upper.Unbounded
			for _, bound := range []struct {
				source []model.Value
				target *[]model.Value
			}{{span.Lower.Values, &result[i].Spans[j].Lower.Values}, {span.Upper.Values, &result[i].Spans[j].Upper.Values}} {
				valueHeaders := uint64(len(bound.source)) * uint64(unsafe.Sizeof(model.Value{}))
				if valueHeaders > maximum-used {
					return nil, 0, model.ErrValueBudget
				}
				used += valueHeaders
				*bound.target = make([]model.Value, len(bound.source))
				for k, value := range bound.source {
					owned, bytes, err := model.CopyValue(ctx, value, maximum-used)
					if err != nil {
						return nil, 0, err
					}
					(*bound.target)[k] = owned
					used += bytes
				}
			}
		}
	}
	return result, used, nil
}

func (transactions *Transactions) closeNamedObjects() error {
	var err error
	for name, cursor := range transactions.named.cursors {
		err = errors.Join(err, cursor.close())
		delete(transactions.named.cursors, name)
	}
	clear(transactions.named.prepared)
	transactions.named.metadataBytes = 0
	return err
}

func (transactions *Transactions) withNamedObjects(ctx context.Context, visit func(context.Context) error) error {
	if visit == nil {
		return errPreparedInvocation
	}
	if operation, ok := ctx.Value(sessionTransactionKey{}).(*sessionTransactionOperation); ok && operation.owner == transactions && operation.active.Load() {
		return visit(ctx)
	}
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return err
	}
	defer finish()
	return visit(operation)
}

func (transactions *Transactions) VisitPreparedStatements(ctx context.Context, visit func(model.PreparedStatementSummary) error) error {
	if visit == nil {
		return errPreparedInvocation
	}
	if !transactions.introspecting.CompareAndSwap(false, true) {
		return errSessionTransactionReentry
	}
	defer transactions.introspecting.Store(false)
	return transactions.withNamedObjects(ctx, func(operation context.Context) error {
		names := make([]string, 0, len(transactions.named.prepared))
		for name := range transactions.named.prepared {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if err := operation.Err(); err != nil {
				return sessionTransactionError(err)
			}
			statement := transactions.named.prepared[name]
			summary := model.PreparedStatementSummary{
				Name: statement.name, Statement: statement.statement,
				PreparedAt: statement.preparedAt, ParameterTypes: statement.parameterTypes,
				ResultTypes: statement.resultTypes, FromSQL: true,
				GenericPlans: statement.genericPlans, CustomPlans: statement.customPlans,
			}
			if err := visit(summary); err != nil {
				return err
			}
		}
		return nil
	})
}

func (transactions *Transactions) VisitCursors(ctx context.Context, visit func(model.CursorSummary) error) error {
	if visit == nil {
		return errPreparedInvocation
	}
	if !transactions.introspecting.CompareAndSwap(false, true) {
		return errSessionTransactionReentry
	}
	defer transactions.introspecting.Store(false)
	return transactions.withNamedObjects(ctx, func(operation context.Context) error {
		names := make([]string, 0, len(transactions.named.cursors))
		for name := range transactions.named.cursors {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if err := operation.Err(); err != nil {
				return sessionTransactionError(err)
			}
			cursor := transactions.named.cursors[name]
			summary := model.CursorSummary{
				Name: cursor.name, Statement: cursor.statement, CreatedAt: cursor.createdAt,
				Holdable: cursor.holdable, Binary: cursor.binary, Scrollable: cursor.scrollable,
			}
			if err := visit(summary); err != nil {
				return err
			}
		}
		return nil
	})
}

func namedObjectTime(moment sqlMoment) (time.Time, error) {
	if moment.err != nil {
		return time.Time{}, moment.err
	}
	const postgresEpochUnixSeconds int64 = 946684800
	seconds := int64(moment.value) / 1_000_000
	microseconds := int64(moment.value) % 1_000_000
	return time.Unix(postgresEpochUnixSeconds+seconds, microseconds*1_000).UTC(), nil
}

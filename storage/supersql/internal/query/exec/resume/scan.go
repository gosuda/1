// Package resume coordinates acknowledged, key-ordered local leaf scans.
package resume

import (
	"context"
	"errors"
	"io"
	"math"
	"slices"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/routing"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
)

var (
	ErrPendingBatch = errors.New("local scan has unacknowledged rows")
	ErrInvalid      = errors.New("invalid local continuation scan")
	ErrCapability   = errors.New("storage cannot provide a resumable ordered leaf scan")
)

type Options struct {
	Query                              *runtime.Query
	Reader                             storage.SnapshotReader
	Routing                            *routing.Table
	Encoder                            storage.KeyEncoder
	Identity                           fragment.ScanIdentity
	Request                            storage.PushdownRequest
	Start                              *fragment.Continuation
	BatchRows                          int
	MetadataBytes, KeyBytes, WorkBytes uint64
}

type boundedEncoder struct {
	storage.KeyEncoder
	bytes uint64
}

func (e boundedEncoder) EncodePrefix(ctx context.Context, layout []storage.KeyColumn, values []model.Value, limit uint64) ([]byte, error) {
	return e.KeyEncoder.EncodePrefix(ctx, layout, values, min(limit, e.bytes))
}

// Scan borrows the query and reader, including its externally held snapshot pin.
// Next and Accept are serialized. Close interrupts an active operation and waits
// for native cleanup. Checkpoints own no reader or snapshot resources.
type Scan struct {
	mu                        sync.Mutex
	ctx                       context.Context
	cancel                    context.CancelCauseFunc
	stopQuery, stopClose      func() bool
	reader                    storage.SnapshotReader
	encoder                   storage.KeyEncoder
	identity                  fragment.ScanIdentity
	request                   storage.PushdownRequest
	scanRequest               storage.PushdownRequest
	limits                    routing.Limits
	metadataBytes, keyBytes   uint64
	batchRows                 int
	claim                     columnar.Allocation
	token                     fragment.Continuation
	morsels                   []routing.Morsel
	morsel                    int
	plan                      storage.PushdownPlan
	hasPlan                   bool
	cursor                    storage.BatchCursor
	input                     columnar.Batch
	published                 []columnar.Vector
	keyColumns, layoutColumns []int
	key                       []model.Value
	accepted                  int
	closed                    bool
	closeErr                  error
	failure                   error
}

func New(ctx context.Context, o Options) (*Scan, error) {
	missingRuntime := o.Query == nil || o.Reader == nil || o.Routing == nil || o.Encoder == nil
	invalidBatch := o.BatchRows <= 0 || uint64(o.BatchRows) > math.MaxUint32
	invalidMemory := o.MetadataBytes == 0 || o.KeyBytes == 0 || o.WorkBytes == 0
	if missingRuntime || invalidBatch || invalidMemory {
		return nil, ErrInvalid
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(o.Query.Context())); err != nil {
		return nil, err
	}
	if o.Request.SafeLimit.Enabled {
		return nil, ErrCapability
	}
	bytes, limits, err := admission(o)
	if err != nil {
		return nil, err
	}
	claim, err := o.Query.Allocator(runtime.WorkMemory).Reserve(bytes)
	if err != nil {
		return nil, err
	}
	s := &Scan{reader: o.Reader, encoder: boundedEncoder{KeyEncoder: o.Encoder, bytes: o.KeyBytes}, identity: o.Identity, limits: limits, metadataBytes: o.MetadataBytes, keyBytes: o.KeyBytes, batchRows: o.BatchRows, claim: claim}
	s.ctx, s.cancel = context.WithCancelCause(ctx)
	queryContext := o.Query.Context()
	s.stopQuery = context.AfterFunc(queryContext, func() { s.cancel(context.Cause(queryContext)) })
	fail := func(err error) (*Scan, error) { return nil, errors.Join(err, s.Close()) }
	s.request, _, err = storage.CopyPushdownRequest(ctx, o.Request, o.MetadataBytes)
	if err != nil {
		return fail(err)
	}
	if o.Start == nil {
		s.token, err = fragment.NewContinuation(ctx, o.Identity, s.request, s.encoder, limits)
		if err != nil {
			return fail(err)
		}
	} else {
		s.token = *o.Start
	}
	s.morsels, err = s.token.Resume(ctx, o.Identity, s.request, o.Routing, s.encoder, limits)
	if err != nil {
		return fail(err)
	}
	s.scanRequest = s.request
	s.scanRequest.ColumnIDs = make([]model.ColumnID, len(s.request.ColumnIDs), len(s.request.ColumnIDs)+len(s.request.KeyLayout))
	copy(s.scanRequest.ColumnIDs, s.request.ColumnIDs)
	s.keyColumns = make([]int, len(s.request.KeyLayout))
	for i, key := range s.request.KeyLayout {
		col := slices.Index(s.scanRequest.ColumnIDs, key.ColumnID)
		if col < 0 {
			col = len(s.scanRequest.ColumnIDs)
			s.scanRequest.ColumnIDs = append(s.scanRequest.ColumnIDs, key.ColumnID)
		}
		s.keyColumns[i] = col
	}
	s.layoutColumns = make([]int, len(s.scanRequest.ColumnIDs))
	for i, id := range s.scanRequest.ColumnIDs {
		col := slices.IndexFunc(s.request.Layout, func(c storage.ColumnLayout) bool { return c.ID == id })
		if col < 0 {
			return fail(ErrInvalid)
		}
		s.layoutColumns[i] = col
	}
	s.input.Columns = make([]columnar.Vector, 0, len(s.scanRequest.ColumnIDs))
	s.published = make([]columnar.Vector, len(s.request.ColumnIDs))
	s.key = make([]model.Value, len(s.request.KeyLayout))
	s.scanRequest.Spans = make([]storage.KeySpan, 1)
	s.mu.Lock()
	s.stopClose = context.AfterFunc(s.ctx, func() { _ = s.Close() })
	s.mu.Unlock()
	if err := context.Cause(s.ctx); err != nil {
		return fail(err)
	}
	return s, nil
}

func admission(o Options) (uint64, routing.Limits, error) {
	// Eight metadata slots cover the request, immutable token, routed bounds,
	// plan verification and overlapping continuation/routing scratch. Four key
	// slots cover extracted values, old/new checkpoints and encoder scratch.
	used := uint64(unsafe.Sizeof(Scan{})) + 4096
	add := func(n, size uint64) bool {
		if used > o.WorkBytes || size != 0 && n > (o.WorkBytes-used)/size {
			return false
		}
		used += n * size
		return true
	}
	columns, keys := uint64(len(o.Request.ColumnIDs)), uint64(len(o.Request.KeyLayout))
	if !add(8, o.MetadataBytes) || !add(4, o.KeyBytes) ||
		!add(columns+keys, uint64(unsafe.Sizeof(model.ColumnID(0)))+uint64(unsafe.Sizeof(int(0)))+2*uint64(unsafe.Sizeof(columnar.Vector{}))) ||
		!add(keys, uint64(unsafe.Sizeof(int(0)))+uint64(unsafe.Sizeof(model.Value{}))) || used > math.MaxInt64 {
		return 0, routing.Limits{}, budgetError()
	}
	count := max(uint64(1), o.MetadataBytes/uint64(unsafe.Sizeof(routing.Morsel{})))
	limits := routing.Limits{MaxBytes: o.MetadataBytes, MaxNodes: o.MetadataBytes, MaxSpans: count, MaxPartitions: count, MaxMorsels: count, MaxWork: min(o.MetadataBytes, uint64(math.MaxUint64)/128) * 128}
	return used, limits, nil
}

func budgetError() error {
	return &model.Error{Code: "53200", Message: "local continuation scan memory limit", Cause: runtime.ErrBudget}
}

func (s *Scan) open() error {
	s.scanRequest.Spans[0] = s.morsels[s.morsel].Span
	plan, err := s.reader.PlanScan(s.ctx, s.scanRequest)
	// A backend may return an issued binding together with an error.
	s.plan, s.hasPlan = plan, plan.Binding != ([32]byte{})
	if err != nil {
		return err
	}
	if !s.hasPlan {
		return ErrCapability
	}
	if err := s.verifyPlan(plan); err != nil {
		return err
	}
	s.cursor, err = s.reader.VectorizedScan(s.ctx, plan)
	if err != nil {
		return err
	}
	if s.cursor == nil {
		return ErrCapability
	}
	return nil
}

func (s *Scan) verifyPlan(plan storage.PushdownPlan) error {
	if plan.SnapshotTS != s.identity.Snapshot || !plan.Residual.Empty() || plan.AppliedLimit.Enabled || !slices.Equal(plan.Projection, s.scanRequest.ColumnIDs) || len(plan.Ordering) != len(s.request.KeyLayout) {
		return ErrCapability
	}
	for i, key := range s.request.KeyLayout {
		desc, nulls := key.Descending, key.NullsFirst
		if s.request.Direction == storage.ScanReverse {
			desc, nulls = !desc, !nulls
		}
		want := storage.ScanOrdering{KeyOrdinal: uint32(i), Type: key.Type, Descending: desc, NullsFirst: nulls}
		if plan.Ordering[i] != want {
			return ErrCapability
		}
	}
	want, err := storage.HashPushdownPlan(s.ctx, storage.PushdownPlan{Request: s.scanRequest, Accepted: s.scanRequest.Predicate, SnapshotTS: s.identity.Snapshot}, s.metadataBytes)
	if err != nil {
		return err
	}
	request, _, err := storage.CopyPushdownRequest(s.ctx, plan.Request, s.metadataBytes)
	if err != nil {
		return err
	}
	got, err := storage.HashPushdownPlan(s.ctx, storage.PushdownPlan{Request: request, Accepted: plan.Accepted, SnapshotTS: plan.SnapshotTS}, s.metadataBytes)
	if err != nil {
		return err
	}
	if got != want {
		return ErrCapability
	}
	return nil
}

func (s *Scan) Next(ctx context.Context, out *columnar.Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.Join(runtime.ErrClosed, s.failure, s.closeErr)
	}
	if out == nil || out.MaxRows < 0 {
		return ErrInvalid
	}
	if s.accepted < s.input.Rows() {
		return ErrPendingBatch
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(s.ctx)); err != nil {
		return s.fail(err)
	}
	// Storage can retain its opening context. Only an actual caller cancellation
	// during this pull cancels the persistent scan, not expiration after success.
	stop := context.AfterFunc(ctx, func() { s.cancel(context.Cause(ctx)) })
	defer stop()
	out.Reset()
	demand := s.batchRows
	if out.MaxRows > 0 {
		demand = min(demand, out.MaxRows)
	}
	for {
		if err := errors.Join(context.Cause(ctx), context.Cause(s.ctx)); err != nil {
			return s.fail(err)
		}
		if s.cursor == nil {
			if s.morsel == len(s.morsels) {
				if err := s.closeLocked(); err != nil {
					return err
				}
				return io.EOF
			}
			if err := s.open(); err != nil {
				return s.fail(err)
			}
		}
		s.input.Reset()
		s.input.MaxRows = demand
		s.accepted = 0
		err := s.cursor.Next(s.ctx, &s.input)
		if err == io.EOF {
			if err = s.closeMorsel(); err != nil {
				return s.fail(err)
			}
			s.morsel++
			continue
		}
		if err != nil {
			return s.fail(err)
		}
		if err := errors.Join(context.Cause(ctx), context.Cause(s.ctx)); err != nil {
			return s.fail(err)
		}
		if err := s.input.Validate(max(demand, s.input.Length)); err != nil {
			return s.fail(err)
		}
		if len(s.input.Columns) != len(s.layoutColumns) || s.input.Rows() > demand {
			return s.fail(ErrCapability)
		}
		for i, index := range s.layoutColumns {
			layout := s.request.Layout[index]
			if !s.input.Columns[i].MatchesLayout(model.TypeLayout{Type: layout.Type, Children: layout.Children}) {
				return s.fail(ErrCapability)
			}
		}
		copy(s.published, s.input.Columns[:len(s.published)])
		out.Columns = s.published
		out.Length, out.Selection = s.input.Length, s.input.Selection
		return nil
	}
}

// Accept acknowledges an additional prefix of the currently published batch.
// A failed acknowledgement leaves the complete previous checkpoint intact.
func (s *Scan) Accept(ctx context.Context, rows int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.Join(runtime.ErrClosed, s.failure, s.closeErr)
	}
	if rows < 0 || rows > s.input.Rows()-s.accepted {
		return ErrInvalid
	}
	if rows == 0 {
		return nil
	}
	opCtx, cancel := context.WithCancelCause(s.ctx)
	stop := context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
	defer stop()
	defer cancel(nil)
	if err := errors.Join(context.Cause(ctx), context.Cause(opCtx)); err != nil {
		return err
	}
	candidate := s.token
	for row := s.accepted; row < s.accepted+rows; row++ {
		clear(s.key)
		remaining := s.keyBytes
		for i, col := range s.keyColumns {
			value, bytes, err := columnar.CopyValue(opCtx, &s.input.Columns[col], s.input.Lane(row), remaining)
			if err != nil {
				clear(s.key)
				return err
			}
			s.key[i] = value
			remaining -= bytes
		}
		next, err := candidate.Advance(opCtx, s.key, s.encoder, s.limits)
		clear(s.key)
		if err != nil {
			return err
		}
		candidate = next
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(opCtx)); err != nil {
		return err
	}
	s.token, s.accepted = candidate, s.accepted+rows
	return nil
}

func (s *Scan) Checkpoint() fragment.Continuation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

func (s *Scan) closeMorsel() error {
	var err error
	if s.cursor != nil {
		err = s.cursor.Close()
		s.cursor = nil
	}
	if s.hasPlan {
		err = errors.Join(err, s.reader.ReleasePlan(s.plan))
		s.hasPlan = false
	}
	s.plan = storage.PushdownPlan{}
	s.input.Reset()
	return err
}

func (s *Scan) fail(err error) error {
	s.failure = err
	s.cancel(err)
	return errors.Join(err, s.closeLocked())
}

func (s *Scan) closeLocked() error {
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	if s.stopClose != nil {
		s.stopClose()
		s.stopClose = nil
	}
	if s.stopQuery != nil {
		s.stopQuery()
		s.stopQuery = nil
	}
	s.cancel(runtime.ErrClosed)
	s.closeErr = s.closeMorsel()
	s.reader, s.encoder = nil, nil
	s.input = columnar.Batch{}
	s.published, s.key, s.keyColumns, s.layoutColumns, s.morsels = nil, nil, nil, nil, nil
	s.request, s.scanRequest = storage.PushdownRequest{}, storage.PushdownRequest{}
	if s.claim != nil {
		s.claim.Release()
		s.claim = nil
	}
	return s.closeErr
}

func (s *Scan) Close() error {
	s.cancel(runtime.ErrClosed)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

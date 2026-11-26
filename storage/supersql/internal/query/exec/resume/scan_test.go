package resume_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/routing"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/resume"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
)

var intType = model.Type{ID: 20, Storage: model.TypeInt64}
var errInjected = errors.New("injected storage failure")

type reader struct {
	storage.SnapshotReader
	values                                          []int64
	livePlans, liveCursors, releases, closes, pulls int
	serial                                          byte
	empty                                           bool
	physical                                        bool
	mutate                                          func(*storage.PushdownPlan)
	planErr, openErr                                error
	pull                                            func(context.Context, *columnar.Batch) error
}

func (r *reader) Close() error { r.closes++; return nil }
func (r *reader) PlanScan(ctx context.Context, request storage.PushdownRequest) (storage.PushdownPlan, error) {
	if err := context.Cause(ctx); err != nil {
		return storage.PushdownPlan{}, err
	}
	if r.livePlans != 0 {
		return storage.PushdownPlan{}, runtime.ErrBudget
	}
	r.livePlans++
	r.serial++
	p := storage.PushdownPlan{Request: request, Projection: request.ColumnIDs, SnapshotTS: 7, Binding: [32]byte{r.serial}, Accepted: request.Predicate}
	for i, key := range request.KeyLayout {
		desc, nulls := key.Descending, key.NullsFirst
		if request.Direction == storage.ScanReverse {
			desc, nulls = !desc, !nulls
		}
		p.Ordering = append(p.Ordering, storage.ScanOrdering{KeyOrdinal: uint32(i), Type: key.Type, Descending: desc, NullsFirst: nulls})
	}
	if r.mutate != nil {
		r.mutate(&p)
	}
	return p, r.planErr
}
func (r *reader) ReleasePlan(storage.PushdownPlan) error {
	if r.liveCursors != 0 || r.livePlans != 1 {
		return errInjected
	}
	r.livePlans--
	r.releases++
	return nil
}
func (r *reader) VectorizedScan(ctx context.Context, plan storage.PushdownPlan) (storage.BatchCursor, error) {
	r.liveCursors++
	c := &cursor{reader: r, ctx: ctx, columns: plan.Projection, empty: r.empty}
	for _, value := range r.values {
		span := plan.Request.Spans[0]
		below := !span.Lower.Unbounded
		if below {
			below = value < span.Lower.Values[0].IntVal || value == span.Lower.Values[0].IntVal && !span.Lower.Inclusive
		}
		if below {
			continue
		}
		above := !span.Upper.Unbounded
		if above {
			above = value > span.Upper.Values[0].IntVal || value == span.Upper.Values[0].IntVal && !span.Upper.Inclusive
		}
		if above {
			continue
		}
		c.values = append(c.values, value)
	}
	if plan.Request.Direction == storage.ScanReverse {
		slices.Reverse(c.values)
	}
	return c, r.openErr
}

type cursor struct {
	reader        *reader
	ctx           context.Context
	columns       []model.ColumnID
	values        []int64
	index         int
	empty, closed bool
}

func (c *cursor) Next(ctx context.Context, out *columnar.Batch) error {
	c.reader.pulls++
	if err := errors.Join(context.Cause(ctx), context.Cause(c.ctx)); err != nil {
		return err
	}
	if c.reader.pull != nil {
		return c.reader.pull(ctx, out)
	}
	out.Reset()
	if c.index == len(c.values) {
		return io.EOF
	}
	n := min(out.MaxRows, len(c.values)-c.index)
	physical := n
	if c.reader.physical {
		physical += 3
	}
	for _, col := range c.columns {
		data := make([]int64, physical)
		for i := range n {
			data[i] = c.values[c.index+i]
			if col == 2 {
				data[i] *= 10
			}
		}
		out.Columns = append(out.Columns, columnar.Vector{Type: intType, Length: physical, Int64: data})
	}
	out.Length = physical
	if c.empty {
		c.empty = false
		out.Selection = []uint32{}
		return nil
	}
	if c.reader.physical {
		out.Selection = make([]uint32, n)
		for i := range n {
			out.Selection[i] = uint32(i)
		}
	}
	c.index += n
	return nil
}
func (c *cursor) Close() error {
	if c.closed {
		return errInjected
	}
	c.closed = true
	c.reader.liveCursors--
	return nil
}

func fixture(t *testing.T) (resume.Options, *reader) {
	t.Helper()
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	root, err := runtime.NewBudget(1<<27, 100)
	if err != nil {
		t.Fatal(err)
	}
	q, err := runtime.NewQuery(t.Context(), pool, root, nil, runtime.QueryConfig{SpillMode: runtime.SpillDisabled})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := q.Close(); err != nil {
			t.Error(err)
		}
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	full := storage.KeySpan{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}
	request := storage.PushdownRequest{
		Target:    storage.ObjectRef{ObjectID: 1, Generation: 2, DescriptorVersion: 3, EncodingVersion: 1},
		Layout:    []storage.ColumnLayout{{ID: 1, Type: intType}, {ID: 2, Type: intType}},
		KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: intType}},
		ColumnIDs: []model.ColumnID{2}, Spans: []storage.KeySpan{full},
	}
	table, err := routing.NewTable(t.Context(), routing.Spec{Target: request.Target, Epoch: 1, KeyLayout: request.KeyLayout, Partitions: []routing.Partition{{ID: 1, Span: full}}}, encoder, routing.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	r := &reader{values: []int64{1, 2, 3, 4}}
	return resume.Options{Query: q, Reader: r, Routing: table, Encoder: encoder, Identity: fragment.ScanIdentity{Snapshot: 7, Plan: [32]byte{1}}, Request: request, BatchRows: 3, MetadataBytes: 1 << 16, KeyBytes: 4096, WorkBytes: 1 << 20}, r
}

func newScan(t *testing.T, o resume.Options) *resume.Scan {
	t.Helper()
	s, err := resume.New(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func next(t *testing.T, s *resume.Scan, out *columnar.Batch) {
	t.Helper()
	if err := s.Next(t.Context(), out); err != nil {
		t.Fatal(err)
	}
}
func accept(t *testing.T, s *resume.Scan, rows int) {
	t.Helper()
	if err := s.Accept(t.Context(), rows); err != nil {
		t.Fatal(err)
	}
}
func value(t *testing.T, b *columnar.Batch, row int) int64 {
	t.Helper()
	if len(b.Columns) != 1 {
		t.Fatalf("exposed %d columns, want original projection only", len(b.Columns))
	}
	v, _, err := columnar.CopyValue(t.Context(), &b.Columns[0], b.Lane(row), 4096)
	if err != nil {
		t.Fatal(err)
	}
	return v.IntVal
}
func checkpointLower(t *testing.T, s *resume.Scan, o resume.Options, want int64) {
	t.Helper()
	m, err := s.Checkpoint().Resume(t.Context(), o.Identity, o.Request, o.Routing, o.Encoder, routing.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 {
		t.Fatalf("morsels = %d, want 1", len(m))
	}
	bound := m[0].Span.Lower
	if want == 0 {
		if !bound.Unbounded {
			t.Fatalf("fetched rows advanced checkpoint: %+v", bound)
		}
		return
	}
	if bound.Unbounded || bound.Inclusive || len(bound.Values) != 1 || bound.Values[0].IntVal != want {
		t.Fatalf("checkpoint lower = %+v, want exclusive %d", bound, want)
	}
}

func TestAcknowledgedPrefixPreservesPendingBatch(t *testing.T) {
	o, r := fixture(t)
	s := newScan(t, o)
	if r.pulls != 0 || r.livePlans != 0 {
		t.Fatal("construction consumed storage")
	}
	var out columnar.Batch
	next(t, s, &out)
	checkpointLower(t, s, o, 0)
	for _, n := range []int{-1, 4} {
		if err := s.Accept(t.Context(), n); !errors.Is(err, resume.ErrInvalid) {
			t.Fatalf("Accept(%d) = %v", n, err)
		}
	}
	accept(t, s, 0)
	checkpointLower(t, s, o, 0)
	accept(t, s, 1)
	checkpointLower(t, s, o, 1)
	if got := value(t, &out, 2); got != 30 {
		t.Fatalf("Accept changed row buffers: %d", got)
	}
	if err := s.Next(t.Context(), &out); !errors.Is(err, resume.ErrPendingBatch) {
		t.Fatalf("pending Next = %v", err)
	}
	if got := value(t, &out, 0); got != 10 {
		t.Fatalf("pending Next discarded batch: %d", got)
	}
	out.Reset()
	accept(t, s, 1)
	checkpointLower(t, s, o, 2)
	if err := s.Accept(t.Context(), 2); !errors.Is(err, resume.ErrInvalid) {
		t.Fatalf("cumulative overflow = %v", err)
	}
	accept(t, s, 1)
	checkpointLower(t, s, o, 3)
	next(t, s, &out)
	if got := value(t, &out, 0); got != 40 {
		t.Fatalf("next row = %d, want 40", got)
	}
}

func TestPartialCheckpointReplaysUnacceptedRows(t *testing.T) {
	o, _ := fixture(t)
	s := newScan(t, o)
	var out columnar.Batch
	next(t, s, &out)
	accept(t, s, 1)
	token := s.Checkpoint()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	o.Start = &token
	r := newScan(t, o)
	next(t, r, &out)
	if out.Rows() != 3 || value(t, &out, 0) != 20 || value(t, &out, 2) != 40 {
		t.Fatalf("resumed rows = %+v", out)
	}
}

func TestSuccessfulNextContextMayExpire(t *testing.T) {
	o, _ := fixture(t)
	s := newScan(t, o)
	ctx, cancel := context.WithCancel(t.Context())
	out := columnar.Batch{MaxRows: 1}
	if err := s.Next(ctx, &out); err != nil {
		t.Fatal(err)
	}
	cancel()
	accept(t, s, 1)
	next(t, s, &out)
	if got := value(t, &out, 0); got != 20 {
		t.Fatalf("next row = %d", got)
	}
}

func TestEmptySelectedBatchesAndHiddenKeys(t *testing.T) {
	o, r := fixture(t)
	r.empty, r.physical = true, true
	s := newScan(t, o)
	out := columnar.Batch{MaxRows: 1}
	next(t, s, &out)
	if out.Rows() != 0 {
		t.Fatalf("empty batch rows = %d", out.Rows())
	}
	checkpointLower(t, s, o, 0)
	next(t, s, &out)
	if out.Rows() != 1 || out.Length <= out.MaxRows || value(t, &out, 0) != 10 {
		t.Fatalf("selected batch = %+v", out)
	}
	accept(t, s, 1)
	checkpointLower(t, s, o, 1)
}

func TestMorselsReleasePlansBeforeOpeningNext(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "forward", true: "reverse"}[reverse], func(t *testing.T) {
			o, r := fixture(t)
			if reverse {
				o.Request.Direction = storage.ScanReverse
			}
			full := o.Request.Spans[0]
			cut := storage.KeyBound{Values: []model.Value{model.Int64Val(3)}}
			upper := cut
			cut.Inclusive = true
			var err error
			o.Routing, err = routing.NewTable(t.Context(), routing.Spec{Target: o.Request.Target, Epoch: 2, KeyLayout: o.Request.KeyLayout, Partitions: []routing.Partition{{ID: 1, Span: storage.KeySpan{Lower: full.Lower, Upper: upper}}, {ID: 2, Span: storage.KeySpan{Lower: cut, Upper: full.Upper}}}}, o.Encoder, routing.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			s := newScan(t, o)
			var out columnar.Batch
			var got []int64
			for {
				err := s.Next(t.Context(), &out)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				for i := range out.Rows() {
					got = append(got, value(t, &out, i))
				}
				accept(t, s, out.Rows())
			}
			want := []int64{10, 20, 30, 40}
			if reverse {
				slices.Reverse(want)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("rows = %v, want %v", got, want)
			}
			if r.releases != 2 || r.livePlans != 0 || r.liveCursors != 0 {
				t.Fatalf("native resources after EOF: %+v", r)
			}
		})
	}
}

func TestRejectsChangedPlanCapabilities(t *testing.T) {
	predicate, err := storage.NewPredicate([]storage.PredicateNode{{Kind: storage.PredicateConstant, Type: model.Type{ID: 16, Storage: model.TypeBoolean}}}, nil, []model.Value{model.BoolVal(false)})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*storage.PushdownPlan){
		"snapshot":   func(p *storage.PushdownPlan) { p.SnapshotTS++ },
		"target":     func(p *storage.PushdownPlan) { p.Request.Target.Generation++ },
		"projection": func(p *storage.PushdownPlan) { p.Projection = []model.ColumnID{1} },
		"ordering":   func(p *storage.PushdownPlan) { p.Ordering = nil },
		"direction":  func(p *storage.PushdownPlan) { p.Ordering[0].Descending = !p.Ordering[0].Descending },
		"limit":      func(p *storage.PushdownPlan) { p.AppliedLimit = storage.ScanLimit{Enabled: true, Count: 1} },
		"residual":   func(p *storage.PushdownPlan) { p.Residual = predicate },
		"accepted":   func(p *storage.PushdownPlan) { p.Accepted = predicate },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			o, r := fixture(t)
			r.mutate = mutate
			s := newScan(t, o)
			var out columnar.Batch
			if err := s.Next(t.Context(), &out); !errors.Is(err, resume.ErrCapability) {
				t.Fatalf("Next = %v", err)
			}
			if r.livePlans != 0 || r.liveCursors != 0 || r.releases != 1 || r.pulls != 0 {
				t.Fatalf("rejected plan leaked/consumed: %+v", r)
			}
			checkpointLower(t, s, o, 0)
		})
	}
}

func TestErrorWithIssuedResourceStillReleasesIt(t *testing.T) {
	for _, atPlan := range []bool{false, true} {
		t.Run(map[bool]string{true: "plan", false: "cursor"}[atPlan], func(t *testing.T) {
			o, r := fixture(t)
			if atPlan {
				r.planErr = errInjected
			} else {
				r.openErr = errInjected
			}
			s := newScan(t, o)
			var out columnar.Batch
			if err := s.Next(t.Context(), &out); !errors.Is(err, errInjected) {
				t.Fatalf("Next = %v", err)
			}
			if r.livePlans != 0 || r.liveCursors != 0 || r.releases != 1 {
				t.Fatalf("error leaked native ownership: %+v", r)
			}
			if usage := o.Query.Usage(); usage.Work.Live != 0 {
				t.Fatalf("error leaked work: %+v", usage)
			}
		})
	}
}

func TestCloseInterruptsPullAndPreservesBorrowedOwners(t *testing.T) {
	o, r := fixture(t)
	started := make(chan struct{})
	r.pull = func(ctx context.Context, _ *columnar.Batch) error {
		close(started)
		<-ctx.Done()
		return context.Cause(ctx)
	}
	s := newScan(t, o)
	done := make(chan error, 1)
	go func() { var out columnar.Batch; done <- s.Next(t.Context(), &out) }()
	<-started
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, runtime.ErrClosed) {
		t.Fatalf("interrupted pull = %v", err)
	}
	if r.livePlans != 0 || r.liveCursors != 0 || r.closes != 0 {
		t.Fatalf("Close ownership = %+v", r)
	}
	if usage := o.Query.Usage(); usage.Work.Live != 0 {
		t.Fatalf("work after Close = %+v", usage)
	}
	claim, err := o.Query.Allocator(runtime.WorkMemory).Reserve(1)
	if err != nil {
		t.Fatal(err)
	}
	claim.Release()
	r.pull = nil
	reopened := newScan(t, o)
	var out columnar.Batch
	next(t, reopened, &out)
	if value(t, &out, 0) != 10 {
		t.Fatal("borrowed reader no longer usable")
	}
}

type cancelEncoder struct {
	storage.KeyEncoder
	cancel context.CancelFunc
	calls  atomic.Int32
	after  int32
}

func (e *cancelEncoder) EncodePrefix(ctx context.Context, keys []storage.KeyColumn, values []model.Value, limit uint64) ([]byte, error) {
	out, err := e.KeyEncoder.EncodePrefix(ctx, keys, values, limit)
	if e.cancel != nil && e.calls.Add(1) == e.after {
		e.cancel()
	}
	return out, err
}

func TestCancelledAcceptDoesNotCommitAnyPrefix(t *testing.T) {
	o, _ := fixture(t)
	encoder := &cancelEncoder{KeyEncoder: o.Encoder}
	o.Encoder = encoder
	s := newScan(t, o)
	var out columnar.Batch
	next(t, s, &out)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	encoder.cancel, encoder.after = cancel, 2
	if err := s.Accept(ctx, 3); !errors.Is(err, context.Canceled) {
		t.Fatalf("Accept = %v", err)
	}
	encoder.cancel = nil
	checkpointLower(t, s, o, 0)
	if value(t, &out, 2) != 30 {
		t.Fatal("cancelled acknowledgement invalidated rows")
	}
	accept(t, s, 3)
	checkpointLower(t, s, o, 3)
}

func TestAdmissionAndKeyQuotasLeaveCheckpointIntact(t *testing.T) {
	t.Run("work", func(t *testing.T) {
		o, r := fixture(t)
		o.WorkBytes = 1
		if _, err := resume.New(t.Context(), o); !errors.Is(err, runtime.ErrBudget) {
			t.Fatalf("New = %v", err)
		}
		if r.livePlans != 0 || o.Query.Usage().Work.Live != 0 {
			t.Fatal("failed admission retained resources")
		}
	})
	t.Run("key", func(t *testing.T) {
		o, _ := fixture(t)
		o.KeyBytes = 1
		s := newScan(t, o)
		var out columnar.Batch
		next(t, s, &out)
		if err := s.Accept(t.Context(), 1); err == nil {
			t.Fatal("oversized encoded key accepted")
		}
		checkpointLower(t, s, o, 0)
	})
}

func TestRejectsUnsafeLimitAndStaleStartWithoutStorage(t *testing.T) {
	o, r := fixture(t)
	o.Request.SafeLimit = storage.ScanLimit{Enabled: true, Count: 1}
	if _, err := resume.New(t.Context(), o); !errors.Is(err, resume.ErrCapability) {
		t.Fatalf("limit New = %v", err)
	}
	o.Request.SafeLimit = storage.ScanLimit{}
	s := newScan(t, o)
	token := s.Checkpoint()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	o.Start = &token
	o.Identity.Plan[0]++
	if _, err := resume.New(t.Context(), o); !errors.Is(err, fragment.ErrContinuationMismatch) {
		t.Fatalf("stale New = %v", err)
	}
	if r.livePlans != 0 || r.pulls != 0 || o.Query.Usage().Work.Live != 0 {
		t.Fatal("stale continuation consumed or retained resources")
	}
}

func TestOutOfOrderAcknowledgementDoesNotCommitEarlierRows(t *testing.T) {
	o, r := fixture(t)
	r.values = []int64{1, 3, 2}
	s := newScan(t, o)
	var out columnar.Batch
	next(t, s, &out)
	if err := s.Accept(t.Context(), 3); !errors.Is(err, fragment.ErrContinuationOrder) {
		t.Fatalf("Accept = %v", err)
	}
	checkpointLower(t, s, o, 0)
	accept(t, s, 1)
	checkpointLower(t, s, o, 1)
}

func TestOwnsCallerMetadataBeforeOpeningStorage(t *testing.T) {
	o, _ := fixture(t)
	s := newScan(t, o)
	o.Request.ColumnIDs[0] = 1
	o.Request.KeyLayout[0].Descending = true
	o.Request.Layout[1].Type = model.Type{ID: 25, Storage: model.TypeString}
	o.Request.Spans[0].Upper = storage.KeyBound{Values: []model.Value{model.Int64Val(1)}}
	var out columnar.Batch
	next(t, s, &out)
	if out.Rows() != 3 || value(t, &out, 2) != 30 {
		t.Fatal("caller mutation changed the owned scan request")
	}
	accept(t, s, 3)
}

func TestCallerCancellationReleasesNativeResources(t *testing.T) {
	o, r := fixture(t)
	started := make(chan struct{})
	r.pull = func(ctx context.Context, _ *columnar.Batch) error {
		close(started)
		<-ctx.Done()
		return context.Cause(ctx)
	}
	s := newScan(t, o)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { var out columnar.Batch; done <- s.Next(ctx, &out) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Next = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if r.liveCursors != 0 || r.livePlans != 0 || r.closes != 0 || o.Query.Usage().Work.Live != 0 {
		t.Fatal("cancelled pull leaked or closed borrowed owner")
	}
	checkpointLower(t, s, o, 0)
}

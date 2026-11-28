package modify_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/exec/modify"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

var integer = model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}
var record = model.TypeLayout{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, Children: []model.TypeLayout{integer, integer}}

// The test clock advances only on certified waits, without timers or wall time.
type testClock struct {
	mu          sync.Mutex
	now         clock.Instant
	scale       clock.ScaleDescriptor
	epoch       clock.Epoch
	unavailable bool
}

func newClock() *testClock {
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	id := sha256.Sum256(history)
	return &testClock{now: 100, scale: clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: id, CanonicalLeapHistory: history}, epoch: clock.Epoch{Assurance: [16]byte{1}, Config: [32]byte{1}, LeapHistory: id}}
}
func (c *testClock) Read(ctx context.Context) (clock.Reading, error) {
	if err := ctx.Err(); err != nil {
		return clock.Reading{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.unavailable {
		return clock.Reading{}, clock.ErrClockUnavailable
	}
	return clock.Reading{Earliest: c.now, Latest: c.now, Status: clock.Synced, Epoch: c.epoch}, nil
}
func (c *testClock) WaitUntilAfter(ctx context.Context, value clock.Instant, epoch clock.Epoch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.unavailable {
		return clock.ErrClockUnavailable
	}
	if epoch != c.epoch {
		return clock.ErrClockEpochChanged
	}
	if value < 0 || value == math.MaxInt64 {
		return clock.ErrClockRange
	}
	c.now = max(c.now, value+1)
	return nil
}
func (c *testClock) Scale() clock.ScaleDescriptor {
	copy := c.scale
	copy.CanonicalLeapHistory = append([]byte(nil), c.scale.CanonicalLeapHistory...)
	return copy
}
func (*testClock) FromUTC(value time.Time) (clock.Instant, error) {
	return clock.Instant(value.UnixNano()), nil
}
func (*testClock) ToUTC(value clock.Instant) (clock.UTCProjection, error) {
	return clock.UTCProjection{Time: time.Unix(0, int64(value)).UTC()}, nil
}

type resolver struct {
	target     storage.ObjectRef
	normalizer *txn.RowNormalizer
}

func (r resolver) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if target != r.target {
		return nil, storage.ErrInvalidTarget
	}
	return r.normalizer, nil
}

type store struct {
	backend   *neosepia.Backend
	config    neosepia.Config
	manager   *txn.Manager
	clock     *testClock
	authority *txn.FenceAuthority
	tx        *txn.Transaction
	query     *runtime.Query
	directory string
	options   modify.Options
}

type constantDefault model.Value

func (value constantDefault) Evaluate(ctx context.Context, _ []model.Value, _ model.ExpressionRuntime) (model.Value, error) {
	return model.Value(value), ctx.Err()
}

func newStore(t *testing.T) *store {
	t.Helper()
	return newStoreWithDefault(t, nil)
}

func newStoreWithDefault(t *testing.T, defaultValue *model.Value) *store {
	t.Helper()
	return newStoreWithValueLayout(t, integer, defaultValue)
}

// newNullableStore builds the shared fixture table with a NULL-able value column.
// The default fixture keeps value NOT NULL because the constraint-error tests
// depend on that rejection; aggregate semantics need a real NULL row (STRICT
// transitions skip it, non-strict ones see it), so those fixtures opt in here.
func newNullableStore(t *testing.T) *store {
	t.Helper()
	return newStoreWithValueLayoutAndNullability(t, integer, nil, true)
}

func newStoreWithValueLayout(t *testing.T, valueLayout model.TypeLayout, defaultValue *model.Value) *store {
	t.Helper()
	return newStoreWithValueLayoutAndNullability(t, valueLayout, defaultValue, false)
}

func newStoreWithValueLayoutAndNullability(t *testing.T, valueLayout model.TypeLayout, defaultValue *model.Value, nullable bool) *store {
	t.Helper()
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	s := &store{clock: newClock(), authority: authority, directory: t.TempDir()}
	s.config = neosepia.Config{Directory: t.TempDir(), Scale: s.clock.Scale(), KeyEncoder: encoder, FenceVerifier: authority}
	s.backend, err = neosepia.Open(t.Context(), s.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.backend.Close(); err != nil {
			t.Error(err)
		}
	})
	s.openManager(t)
	t.Cleanup(func() {
		if err := s.manager.Close(); err != nil {
			t.Error(err)
		}
	})
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	budget, err := runtime.NewBudget(256<<20, 4096)
	if err != nil {
		t.Fatal(err)
	}
	s.query, err = runtime.NewQuery(t.Context(), pool, budget, runtime.OSFileSystem{}, runtime.QueryConfig{WorkBytes: 128 << 20, OutputBytes: 16 << 20, SpillBytes: 64 << 20, Directory: s.directory, MaxFiles: 8, MaxReaders: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.query.Close(); err != nil {
			t.Error(err)
		}
	})
	spec := descriptorSpec()
	spec.Table.Columns[1].Layout = valueLayout
	spec.Table.Columns[1].ConstantDefault = defaultValue
	spec.Table.Columns[1].Nullable = nullable
	descriptor, err := model.NewDescriptor(t.Context(), spec, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64})
	if err != nil {
		t.Fatal(err)
	}
	target := storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
	layout := []storage.ColumnLayout{{ID: 1, Type: integer.Type}, {ID: 2, Type: valueLayout.Type, Children: valueLayout.Children, Nullable: nullable}}
	rules := []txn.ColumnRule{{Layout: layout[0], KeyPosition: 1}, {Layout: layout[1]}}
	if defaultValue != nil {
		rules[1].Default = constantDefault(*defaultValue)
	}
	policy, err := txn.NewRowNormalizer(t.Context(), target, rules, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	s.tx, err = s.manager.Begin(t.Context(), txn.TransactionOptions{Mode: txn.ReadWrite, Resolver: resolver{target, policy}, KeyEncoder: encoder, MemoryBytes: 8 << 20, SpillBytes: 32 << 20, SpillDirectory: t.TempDir(), MaxViews: 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.tx.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	inputs := []model.TypeLayout{integer, valueLayout}
	rowLayout := model.TypeLayout{Type: record.Type, Children: inputs}
	oid := model.TypeLayout{Type: model.Type{ID: 26, Modifier: -1, Storage: model.TypeInt64}}
	returning := []model.TypeLayout{rowLayout, rowLayout, oid, oid}
	s.options = modify.Options{Operation: storage.MutationInsert, TargetKind: ir.PhysicalModifyTarget, Transaction: s.tx, Descriptor: descriptor, Layout: layout, KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: integer.Type}}, InputLayouts: inputs, Assignment: program(t, inputs, []int{0, 1}, nil), AssignmentColumns: []model.ColumnID{1, 2}, Returning: program(t, returning, []int{0, 1}, nil), ReturningInputLayouts: returning, ReturningTableOIDs: 2, Query: s.query, Encoder: encoder, BatchRows: 2, BatchBytes: 1 << 20, MaxRecordBytes: 1024, SpoolMemoryBytes: 0, MaxSpoolBytes: 8 << 20, MaxRows: 1000, WorkBytes: 64 << 20, SortRunBytes: 1 << 20, SortMaxRuns: 8, SortMergeFanIn: 2}
	return s
}

func descriptorSpec() model.DescriptorSpec {
	return model.DescriptorSpec{ID: 1, OID: 10001, Name: model.NamespaceName{Parent: 2, Kind: model.ObjectTable, Name: "items"}, Version: 1, State: model.StatePublic, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}, Table: &model.TableMetadata{Owner: 1, ColumnIDHighWatermark: 2, Columns: []model.ColumnDescriptor{{ID: 1, Name: "id", State: model.StatePublic, Layout: integer}, {ID: 2, Name: "value", State: model.StatePublic, Layout: integer}}, PrimaryKey: []model.KeyElement{{Column: 1, Type: integer.Type}}}}
}

func (s *store) openManager(t *testing.T) {
	t.Helper()
	state, err := s.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s.manager, err = txn.NewManager(t.Context(), txn.ManagerConfig{Backend: s.backend, Clock: s.clock, Authority: s.authority, MaxOperations: 16, Recovered: txn.RecoveredState{LogPosition: state.LogPosition, CommitTS: state.CommitTS, ClosedTS: state.ClosedTS, HistoryFloor: state.HistoryFloor, Epoch: state.Epoch, LeapHistory: state.Scale.LeapHistoryID}})
	if err != nil {
		t.Fatal(err)
	}
}

func (s *store) request(t *testing.T, id int64) storage.ReadRequest {
	t.Helper()
	key, err := s.options.Encoder.EncodePrefix(t.Context(), s.options.KeyLayout, []model.Value{model.Int64Val(id)}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return storage.ReadRequest{Target: storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, Key: key, ColumnIDs: []model.ColumnID{1, 2}, Layout: s.options.Layout}
}

func (s *store) seed(t *testing.T, rows ...[]model.Value) {
	t.Helper()
	for _, row := range rows {
		r := s.request(t, row[0].IntVal)
		if err := s.tx.ApplyMutations(t.Context(), []storage.Mutation{{Op: storage.MutationInsert, Target: r.Target, Key: r.Key, ColumnIDs: r.ColumnIDs, Values: row}}); err != nil {
			t.Fatal(err)
		}
	}
}

func (s *store) require(t *testing.T, id, value int64) {
	t.Helper()
	row, err := s.tx.Get(t.Context(), s.request(t, id))
	if err != nil {
		t.Fatal(err)
	}
	if len(row.Values) != 2 || row.Values[0].IntVal != id || row.Values[1].IntVal != value {
		t.Fatalf("row %d = %+v, want (%d,%d)", id, row.Values, id, value)
	}
}
func (s *store) absent(t *testing.T, id int64) {
	t.Helper()
	row, err := s.tx.Get(t.Context(), s.request(t, id))
	if !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("row %d = %+v, %v; want absent", id, row, err)
	}
}

func (s *store) cursor(t *testing.T, rows ...[]model.Value) modify.Cursor {
	t.Helper()
	options := s.options
	if options.Operation != storage.MutationInsert {
		options.Source = s.sourceView(t)
	}
	input := newSource(t, s.options.InputLayouts, rows)
	cursor, err := modify.New(t.Context(), input, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	return cursor
}

func (s *store) sourceView(t *testing.T) txn.ReadView {
	t.Helper()
	request := s.request(t, 1)
	spans := []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}
	view, err := s.tx.ReadView(t.Context(), []txn.ReadDependency{{Target: request.Target, KeyLayout: s.options.KeyLayout, Spans: spans}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := view.Close(); err != nil {
			t.Error(err)
		}
	})
	return view
}

type source struct {
	builder *columnar.Builder
	rows    [][]model.Value
	index   int
	closed  bool
	pulls   int
}

func newSource(t *testing.T, layouts []model.TypeLayout, rows [][]model.Value) *source {
	t.Helper()
	builder, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: 2, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s := &source{builder: builder, rows: rows}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func (s *source) Next(ctx context.Context, out *columnar.Batch) error {
	s.pulls++
	if s.index == len(s.rows) {
		return io.EOF
	}
	s.builder.Reset()
	limit := 2
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	for range min(limit, len(s.rows)-s.index) {
		if err := s.builder.Append(ctx, s.rows[s.index]); err != nil {
			return err
		}
		s.index++
	}
	return s.builder.Publish(out)
}
func (s *source) Close() error { s.closed = true; return s.builder.Close() }
func row(id, value int64) []model.Value {
	return []model.Value{model.Int64Val(id), model.Int64Val(value)}
}

type functionResolver struct{ call expr.ScalarCall }

func (r functionResolver) Resolve(expr.Function) (expr.ScalarCall, error) { return r.call, nil }

// The optional volatile call transforms the final output, allowing tests to
// observe assignment effects without replacing transaction/storage semantics.
func program(t *testing.T, layouts []model.TypeLayout, columns []int, call expr.ScalarCall) *expr.Program {
	t.Helper()
	b, err := ir.NewBuilder(ir.Limits{MaxNodes: 128, MaxOperands: 512, MaxSlots: 64, MaxDepth: 64, MaxBytes: 1 << 20, SourceBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	slots := make([]model.SlotID, len(layouts))
	layoutIDs := make([]ir.LayoutID, len(layouts))
	expressions := make([]ir.ScalarID, len(layouts))
	inputs := make([]expr.Input, len(layouts))
	for i, layout := range layouts {
		layoutIDs[i], err = b.AddLayout(t.Context(), layout)
		if err != nil {
			t.Fatal(err)
		}
		slots[i], err = b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: layoutIDs[i], Nullable: true, Object: 1, Column: model.ColumnID(i + 1)})
		if err != nil {
			t.Fatal(err)
		}
		expressions[i], err = b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: scope, Layout: layoutIDs[i], Slot: slots[i], Nullable: true}})
		if err != nil {
			t.Fatal(err)
		}
		inputs[i] = expr.Input{Slot: slots[i], Column: i}
	}
	roots := make([]ir.ScalarID, len(columns))
	for i, column := range columns {
		roots[i] = expressions[column]
	}
	if call != nil {
		last := len(roots) - 1
		layout := layoutIDs[columns[last]]
		effects := ir.Effects{Volatility: ir.Volatile, MayError: true}
		sig, err := b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 900001, Result: layout, Nullable: true, Effects: effects}, Parameters: []ir.LayoutID{layout}})
		if err != nil {
			t.Fatal(err)
		}
		roots[last], err = b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: scope, Layout: layout, Signature: sig, Function: 900001, Nullable: true, Effects: effects}, Operands: []ir.ScalarID{roots[last]}})
		if err != nil {
			t.Fatal(err)
		}
	}
	rel, err := b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Scan, Scope: scope, Object: 1, DescriptorVersion: 1, Generation: 1}, Outputs: slots})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := b.Build(t.Context(), rel)
	if err != nil {
		t.Fatal(err)
	}
	p, err := expr.Compile(t.Context(), graph, roots, expr.Options{Limits: expr.DefaultLimits(), Inputs: inputs, Functions: functionResolver{call}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func booleanProgram(t *testing.T, layouts []model.TypeLayout, value bool) *expr.Program {
	t.Helper()
	b, err := ir.NewBuilder(ir.Limits{MaxNodes: 128, MaxOperands: 512, MaxSlots: 64, MaxDepth: 64, MaxBytes: 1 << 20, SourceBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	slots := make([]model.SlotID, len(layouts))
	inputs := make([]expr.Input, len(layouts))
	for i, layout := range layouts {
		layoutID, layoutErr := b.AddLayout(t.Context(), layout)
		if layoutErr != nil {
			t.Fatal(layoutErr)
		}
		slots[i], err = b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: layoutID, Nullable: true, Object: 1, Column: model.ColumnID(i + 1)})
		if err != nil {
			t.Fatal(err)
		}
		inputs[i] = expr.Input{Slot: slots[i], Column: i}
	}
	boolean, err := b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: scope, Layout: boolean}, Value: model.BoolVal(value)})
	if err != nil {
		t.Fatal(err)
	}
	rel, err := b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Scan, Scope: scope, Object: 1, DescriptorVersion: 1, Generation: 1}, Outputs: slots})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := b.Build(t.Context(), rel)
	if err != nil {
		t.Fatal(err)
	}
	result, err := expr.Compile(t.Context(), graph, []ir.ScalarID{root}, expr.Options{Limits: expr.DefaultLimits(), Inputs: inputs})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func returned(t *testing.T, cursor storage.BatchCursor) [][]model.Value {
	t.Helper()
	var rows [][]model.Value
	for {
		batch := columnar.Batch{MaxRows: 1}
		err := cursor.Next(t.Context(), &batch)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for lane := range batch.Rows() {
			values := make([]model.Value, len(batch.Columns))
			for i := range values {
				values[i], _, err = columnar.CopyValue(t.Context(), &batch.Columns[i], batch.Lane(lane), 1<<20)
				if err != nil {
					t.Fatal(err)
				}
			}
			rows = append(rows, values)
		}
	}
	return rows
}

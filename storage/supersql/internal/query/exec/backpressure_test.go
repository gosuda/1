package exec_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	exaggregate "github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

const portalGateBatchRows = 64

// The optional real snapshot contributes its native cursor and run pin; only
// its row payload is replaced by a bounded, deterministic generated stream.
type portalGateReader struct {
	storage.SnapshotReader
	query        *runtime.Query
	count        int
	value        func(int) int64
	generated    int
	maxGenerated int
	emptyFirst   bool
	closed       bool
	cursor       *portalGateCursor
}

func (r *portalGateReader) PlanScan(ctx context.Context, request storage.PushdownRequest) (storage.PushdownPlan, error) {
	if r.SnapshotReader != nil {
		return r.SnapshotReader.PlanScan(ctx, request)
	}
	if err := ctx.Err(); err != nil {
		return storage.PushdownPlan{}, err
	}
	return storage.PushdownPlan{Request: request, Projection: request.ColumnIDs}, nil
}

func (r *portalGateReader) VectorizedScan(ctx context.Context, plan storage.PushdownPlan) (storage.BatchCursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	claim, err := r.query.Allocator(runtime.WorkMemory).Reserve(portalGateBatchRows*8 + 1024)
	if err != nil {
		return nil, err
	}
	c := &portalGateCursor{reader: r, typ: plan.Request.Layout[0].Type, claim: claim}
	if r.SnapshotReader != nil {
		c.native, err = r.SnapshotReader.VectorizedScan(ctx, plan)
		if err != nil {
			claim.Release()
			return nil, err
		}
	}
	r.cursor = c
	return c, nil
}

func (r *portalGateReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.SnapshotReader != nil {
		return r.SnapshotReader.Close()
	}
	return nil
}

type portalGateCursor struct {
	reader *portalGateReader
	typ    model.Type
	claim  columnar.Allocation
	native storage.BatchCursor
	values [portalGateBatchRows]int64
	empty  [0]uint32
	closed bool
}

func (c *portalGateCursor) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.closed || c.reader.generated == c.reader.count {
		return io.EOF
	}
	n := min(portalGateBatchRows, c.reader.count-c.reader.generated)
	if out.MaxRows > 0 {
		n = min(n, out.MaxRows)
	}
	if c.reader.maxGenerated > 0 && c.reader.generated+n > c.reader.maxGenerated {
		return fmt.Errorf("scan generated beyond demand: next=%d allowed=%d", c.reader.generated+n, c.reader.maxGenerated)
	}
	for i := range n {
		c.values[i] = c.reader.value(c.reader.generated + i)
	}
	c.reader.generated += n
	out.Columns = append(out.Columns, columnar.Vector{Type: c.typ, Length: n, Int64: c.values[:n]})
	out.Length = n
	if c.reader.emptyFirst {
		out.Selection = c.empty[:]
		c.reader.emptyFirst = false
	}
	return nil
}

func (c *portalGateCursor) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	clear(c.values[:])
	c.claim.Release()
	if c.native != nil {
		return c.native.Close()
	}
	return nil
}

func portalGateEnvironment(t *testing.T, work int64) (exec.Environment, *runtime.Budget, string) {
	t.Helper()
	dir := t.TempDir()
	env, shared := environmentWithQuery(t, runtime.QueryConfig{WorkBytes: work, OutputBytes: 128 << 10, SpillBytes: 32 << 20, SpillMode: runtime.SpillEnabled, Directory: dir, MaxFiles: 4, MaxReaders: 2, MaxReservations: 256})
	env.Limits = exec.Limits{
		Expressions: expr.Limits{MaxInstructions: 128, MaxRegisters: 16, MaxDepth: 8, MaxRows: portalGateBatchRows, MaxBytes: 16 << 10, MaxConstantBytes: 4 << 10, MaxStateBytes: 64 << 10, MaxOutputBytes: 32 << 10},
		BatchRows:   portalGateBatchRows, BatchBytes: 32 << 10, ParameterBytes: 4 << 10, MetadataBytes: 64 << 10,
	}
	var err error
	env.Keys, err = builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	env.Sort = exsort.Options{MaxRecordBytes: 128, RunBytes: 16 << 10, WorkBytes: 512 << 10, MaxRuns: 4, MergeFanIn: 2}
	env.Aggregate = exaggregate.Options{MaxRecordBytes: 128, GroupBytes: 32 << 10, WorkBytes: 512 << 10, PartitionMemoryBytes: 4 << 10, MaxPartitions: 32}
	if work > 1<<20 {
		env.Sort.RunBytes, env.Sort.WorkBytes = 1<<20, 8<<20
		env.Aggregate.GroupBytes, env.Aggregate.WorkBytes = 1<<20, 4<<20
	}
	return env, shared, dir
}

func portalGateReleased(t *testing.T, env exec.Environment, shared *runtime.Budget, dir string, reader *portalGateReader) {
	t.Helper()
	if !reader.closed || reader.cursor == nil || !reader.cursor.closed {
		t.Errorf("source resources survived: snapshot closed=%v cursor=%+v", reader.closed, reader.cursor)
	}
	usage := env.Query.Usage()
	for name, account := range map[string]runtime.Usage{"work": usage.Work, "output": usage.Output, "spill": usage.Spill, "shared": shared.Usage()} {
		if account.Live != 0 || account.Reservations != 0 {
			t.Errorf("%s resources survived Close: %+v", name, account)
		}
		if account.Peak > account.Limit {
			t.Errorf("%s exceeded quota: %+v", name, account)
		}
	}
	if usage.Files != 0 || usage.Readers != 0 {
		t.Errorf("spill handles survived Close: %+v", usage)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("spill files survived Close: %v", files)
	}
}

func portalGateGraph(t *testing.T, reader *portalGateReader, aggregate bool) *fragment.FragmentGraph {
	t.Helper()
	b, err := ir.NewBuilder(irLimits())
	if err != nil {
		t.Fatal(err)
	}
	scope, err := b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	typ := model.Type{ID: 23, Storage: model.TypeInt64}
	layout, err := b.AddLayout(t.Context(), model.TypeLayout{Type: typ})
	if err != nil {
		t.Fatal(err)
	}
	x, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: layout, Object: 1, Column: 1})
	if err != nil {
		t.Fatal(err)
	}
	scan, err := b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Scan, Scope: scope, Object: 1, Generation: 1, DescriptorVersion: 1}, Outputs: []model.SlotID{x}})
	if err != nil {
		t.Fatal(err)
	}
	column, err := b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: scope, Layout: layout, Slot: x}})
	if err != nil {
		t.Fatal(err)
	}
	request := storage.PushdownRequest{Target: storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, Layout: []storage.ColumnLayout{{ID: 1, Type: typ}}, ColumnIDs: []model.ColumnID{1}, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}}
	negotiated, err := reader.PlanScan(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	properties := physical.Properties{Distribution: physical.Distribution{Kind: physical.SingleNode, Partitions: 1}, Rewindability: physical.OnePass}
	slot := physical.Slot{Graph: 1, ID: x, Layout: layout}
	key := physical.Key{Slot: slot, Type: typ}
	nodes := []physical.NodeSpec{{Logical: physical.RelRef{Graph: 1, Rel: scan}, Algorithm: physical.TableScan, Output: []physical.Slot{slot}, Delivered: properties, Break: physical.Streaming, Scan: physical.ScanSpec{Source: request.Target, ColumnMap: []physical.ScanColumn{{Source: 1, Target: 1}}, CapabilityEpoch: 1, Request: request, Projection: negotiated.Projection}}}
	var root ir.RelID
	if aggregate {
		resultLayout, err := b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}})
		if err != nil {
			t.Fatal(err)
		}
		group, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: layout})
		if err != nil {
			t.Fatal(err)
		}
		total, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: resultLayout, Nullable: true})
		if err != nil {
			t.Fatal(err)
		}
		effects := ir.Effects{MayError: true}
		signature, err := b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 2108, Result: resultLayout, Nullable: true, Effects: effects}, Parameters: []ir.LayoutID{layout}})
		if err != nil {
			t.Fatal(err)
		}
		root, err = b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Aggregate, Scope: scope, Effects: effects}, Inputs: []ir.RelID{scan}, Outputs: []model.SlotID{group, total}, Expressions: []ir.ScalarID{column}, Bindings: []ir.Binding{{Slot: group, Expr: column}}, GroupingSets: [][]uint32{{0}}, Aggregates: []ir.AggregateSpec{{AggregateCall: ir.AggregateCall{Function: 2108, Signature: signature, Result: total, Effects: effects}, Args: []ir.ScalarID{column}}}})
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, physical.NodeSpec{Logical: physical.RelRef{Graph: 1, Rel: root}, Algorithm: physical.HashAggregate, Children: []physical.NodeID{1}, Output: []physical.Slot{{Graph: 1, ID: group, Layout: layout}, {Graph: 1, ID: total, Layout: resultLayout, Nullable: true}}, Required: []physical.Properties{properties}, Delivered: properties, Break: physical.MaterializeInput, Memory: physical.Memory{PeakBytes: 512 << 10, Spillable: true}, Aggregate: physical.AggregateSpec{Phase: physical.CompleteAggregate, GroupKeys: []physical.Key{key}}})
	} else {
		root, err = b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Sort, Scope: scope}, Inputs: []ir.RelID{scan}, Outputs: []model.SlotID{x}, Order: []ir.OrderKey{{Expr: column, Direction: ir.Ascending, Nulls: ir.NullsLast, Operator: 97}}})
		if err != nil {
			t.Fatal(err)
		}
		ordered := properties
		ordered.Ordering = []physical.OrderKey{{Key: key, Direction: ir.Ascending, Nulls: ir.NullsLast, Operator: 97}}
		nodes = append(nodes, physical.NodeSpec{Logical: physical.RelRef{Graph: 1, Rel: root}, Algorithm: physical.Sort, Children: []physical.NodeID{1}, Output: []physical.Slot{slot}, Required: []physical.Properties{properties}, Delivered: ordered, Break: physical.MaterializeInput, Memory: physical.Memory{PeakBytes: 512 << 10, Spillable: true}})
	}
	logical, err := b.Build(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := physical.NewPlan(t.Context(), physical.Spec{Graphs: []*ir.Graph{logical}, Nodes: nodes, Root: 2}, physical.Limits{MaxNodes: 100, MaxOperands: 1000, MaxProperties: 1000, MaxDepth: 100, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := fragment.Build(t.Context(), plan, fragment.Limits{MaxNodes: 100, MaxEdges: 100, MaxDepth: 100, MaxWork: 10000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func TestPortalBackpressureAndSpill(t *testing.T) {
	t.Run("million_row_scan_closes_after_first_batch", func(t *testing.T) {
		native, request := realScan(t)
		request.Predicate = storage.Predicate{}
		env, shared, dir := portalGateEnvironment(t, 1<<20)
		reader := &portalGateReader{SnapshotReader: native, query: env.Query, count: 1_000_000, maxGenerated: portalGateBatchRows, value: func(i int) int64 { return int64(i) }}
		env.Reader = reader
		portal, err := exec.Open(t.Context(), scanGraph(t, reader, request, false, false), env)
		if err != nil {
			t.Fatal(err)
		}
		defer closePortal(t, portal)
		var batch columnar.Batch
		if err := portal.Next(t.Context(), &batch); err != nil {
			t.Fatal(err)
		}
		if batch.Rows() != portalGateBatchRows || reader.generated != portalGateBatchRows {
			t.Fatalf("first batch rows=%d generated=%d, want %d", batch.Rows(), reader.generated, portalGateBatchRows)
		}
		for i := range batch.Rows() {
			value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], batch.Lane(i), 128)
			if err != nil || value.IsNull || value.IntVal != int64(i) {
				t.Fatalf("first batch row %d = %v, %v", i, value, err)
			}
		}
		closePortal(t, portal)
		if reader.generated != portalGateBatchRows || !native.cursorClosed || !native.readerClosed {
			t.Fatalf("early close generated=%d native cursor=%v snapshot=%v", reader.generated, native.cursorClosed, native.readerClosed)
		}
		portalGateReleased(t, env, shared, dir, reader)
	})

	t.Run("empty_filtered_morsel_does_not_end_scan", func(t *testing.T) {
		native, request := realScan(t)
		request.Predicate = storage.Predicate{}
		env, shared, dir := portalGateEnvironment(t, 1<<20)
		reader := &portalGateReader{SnapshotReader: native, query: env.Query, count: portalGateBatchRows + 2, emptyFirst: true, value: func(i int) int64 { return int64(i) }}
		env.Reader = reader
		portal, err := exec.Open(t.Context(), scanGraph(t, reader, request, false, false), env)
		if err != nil {
			t.Fatal(err)
		}
		defer closePortal(t, portal)
		got := rows(t, portal)
		want := [][]model.Value{{model.Int64Val(64)}, {model.Int64Val(65)}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("rows after empty filtered morsel=%v want=%v", got, want)
		}
		closePortal(t, portal)
		portalGateReleased(t, env, shared, dir, reader)
	})

	t.Run("external_sort", func(t *testing.T) {
		baseline := portalGateSort(t, 16<<20)
		if constrained := portalGateSort(t, 1<<20); constrained != baseline {
			t.Fatal("constrained ordered rows differ from sufficient-memory execution")
		}
	})
	t.Run("partitioned_hash_sum", func(t *testing.T) {
		baseline := portalGateAggregate(t, 16<<20)
		if constrained := portalGateAggregate(t, 1<<20); !reflect.DeepEqual(constrained, baseline) {
			t.Fatal("constrained grouped sums differ from sufficient-memory execution")
		}
	})
}

func portalGateSort(t *testing.T, work int64) [sha256.Size]byte {
	t.Helper()
	env, shared, dir := portalGateEnvironment(t, work)
	reader := &portalGateReader{query: env.Query, count: 4096, value: func(i int) int64 { return int64((i*2053)%4096 - 2048) }}
	env.Reader = reader
	portal, err := exec.Open(t.Context(), portalGateGraph(t, reader, false), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	hash := sha256.New()
	count := 0
	var batch columnar.Batch
	for portalGateNext(t, portal, &batch) {
		for i := range batch.Rows() {
			value := portalGateInteger(t, &batch, 0, i)
			if value != int64(count-2048) {
				t.Fatalf("sorted row %d=%d want=%d", count, value, count-2048)
			}
			var encoded [8]byte
			binary.BigEndian.PutUint64(encoded[:], uint64(value))
			_, _ = hash.Write(encoded[:])
			count++
		}
	}
	if count != 4096 {
		t.Fatalf("sorted row count=%d want=4096", count)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	closePortal(t, portal)
	portalGateSpillReleased(t, env, shared, dir, reader)
	return digest
}

func portalGateAggregate(t *testing.T, work int64) map[int64]int64 {
	t.Helper()
	env, shared, dir := portalGateEnvironment(t, work)
	reader := &portalGateReader{query: env.Query, count: 128 * 16, value: func(i int) int64 { return int64((i*53)%128 - 64) }}
	env.Reader = reader
	portal, err := exec.Open(t.Context(), portalGateGraph(t, reader, true), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	groups := make(map[int64]int64, 128)
	var batch columnar.Batch
	for portalGateNext(t, portal, &batch) {
		for i := range batch.Rows() {
			key := portalGateInteger(t, &batch, 0, i)
			total := portalGateInteger(t, &batch, 1, i)
			_, duplicate := groups[key]
			validKey := key >= -64 && key < 64
			if !validKey || duplicate || total != key*16 {
				t.Fatalf("group=%d sum=%d duplicate=%v want 16*group in [-64,64)", key, total, duplicate)
			}
			groups[key] = total
		}
	}
	if len(groups) != 128 {
		t.Fatalf("group count=%d want=128", len(groups))
	}
	closePortal(t, portal)
	portalGateSpillReleased(t, env, shared, dir, reader)
	return groups
}

func portalGateNext(t *testing.T, portal *exec.Portal, batch *columnar.Batch) bool {
	t.Helper()
	err := portal.Next(t.Context(), batch)
	if err == io.EOF {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	if batch.Rows() > portalGateBatchRows {
		t.Fatalf("unbounded output batch: %d", batch.Rows())
	}
	return true
}

func portalGateInteger(t *testing.T, batch *columnar.Batch, column, row int) int64 {
	t.Helper()
	value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[column], batch.Lane(row), 128)
	if err != nil || value.IsNull {
		t.Fatalf("output row=%d column=%d value=%v err=%v", row, column, value, err)
	}
	return value.IntVal
}

func portalGateSpillReleased(t *testing.T, env exec.Environment, shared *runtime.Budget, dir string, reader *portalGateReader) {
	t.Helper()
	if reader.generated != reader.count {
		t.Fatalf("source rows=%d want=%d", reader.generated, reader.count)
	}
	usage := env.Query.Usage()
	if usage.Work.Limit == 1<<20 && (usage.Spill.Peak == 0 || usage.PeakFiles == 0) {
		t.Fatalf("constrained execution did not spill: %+v", usage)
	}
	if usage.Work.Limit > 1<<20 && (usage.Spill.Peak != 0 || usage.PeakFiles != 0) {
		t.Fatalf("sufficient-memory execution spilled: %+v", usage)
	}
	portalGateReleased(t, env, shared, dir, reader)
}

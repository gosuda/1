package exec

import (
	"bytes"
	"context"
	"io"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

var (
	topKInt  = model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
	topKText = model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString, Collation: 950}}
)

type topKTestSource struct {
	builder  *columnar.Builder
	rows     [][]model.Value
	index    int
	emitted  int
	batchMax int
	closed   bool
}

func newTopKTestSource(t *testing.T, layouts []model.TypeLayout, rows [][]model.Value, batchMax int) *topKTestSource {
	t.Helper()
	b, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: max(1, batchMax), MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s := &topKTestSource{builder: b, rows: rows, batchMax: max(1, batchMax)}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func (s *topKTestSource) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.index >= len(s.rows) {
		return io.EOF
	}
	s.builder.Reset()
	n := s.batchMax
	if out.MaxRows > 0 {
		n = min(n, out.MaxRows)
	}
	for range min(n, len(s.rows)-s.index) {
		if err := s.builder.Append(ctx, s.rows[s.index]); err != nil {
			return err
		}
		s.index++
		s.emitted++
	}
	return s.builder.Publish(out)
}

func (s *topKTestSource) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.builder.Close()
}

func topKTestInvocation(t *testing.T, runBytes, workBytes uint64, batchRows int) (*invocation, *runtime.Query, string) {
	t.Helper()
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	budget, err := runtime.NewBudget(64<<20, 10000)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	q, err := runtime.NewQuery(t.Context(), pool, budget, runtime.OSFileSystem{}, runtime.QueryConfig{
		WorkBytes:       32 << 20,
		OutputBytes:     8 << 20,
		SpillBytes:      32 << 20,
		Directory:       dir,
		MaxFiles:        16,
		MaxReaders:      4,
		MaxReservations: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := q.Close(); err != nil {
			t.Error(err)
		}
		if usage := budget.Usage(); usage.Live != 0 || usage.Reservations != 0 {
			t.Errorf("leaked query memory: %+v", usage)
		}
		if files, err := os.ReadDir(dir); err != nil || len(files) != 0 {
			t.Errorf("leaked spill files: files=%v err=%v", files, err)
		}
	})
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	env := Environment{
		Query: q,
		Keys:  encoder,
		Limits: Limits{
			Expressions:   expr.DefaultLimits(),
			BatchRows:     batchRows,
			BatchBytes:    1 << 20,
			MetadataBytes: 1 << 20,
		},
		row: make([]model.Value, 8),
		Spool: rowstore.Limits{
			MemoryBytes:    1 << 20,
			MaxBytes:       4 << 20,
			MaxRows:        10000,
			MaxRecordBytes: 64 << 10,
			MetadataBytes:  64 << 10,
			DecodeBytes:    64 << 10,
			MaxReaders:     4,
		},
		Sort: exsort.Options{
			MaxRecordBytes: 16 << 10,
			RunBytes:       runBytes,
			MaxRuns:        4,
			MergeFanIn:     2,
			WorkBytes:      workBytes,
		},
	}
	return &invocation{portal: &Portal{ctx: t.Context(), env: env}}, q, dir
}

func topKDrain(t *testing.T, cur storage.BatchCursor, maxRows int) [][]model.Value {
	t.Helper()
	var outRows [][]model.Value
	out := columnar.Batch{MaxRows: maxRows}
	for {
		err := cur.Next(t.Context(), &out)
		if err == io.EOF {
			return outRows
		}
		if err != nil {
			t.Fatal(err)
		}
		for i := range out.Rows() {
			row := make([]model.Value, len(out.Columns))
			for j := range row {
				v, _, err := columnar.CopyValue(t.Context(), &out.Columns[j], out.Lane(i), 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				row[j] = v
			}
			outRows = append(outRows, row)
		}
	}
}

func topKReference(t *testing.T, encoder storage.KeyEncoder, rows [][]model.Value, keys []storage.KeyColumn, keyCols []int, offset, count uint64, withTies bool) [][]model.Value {
	t.Helper()
	type item struct {
		key     []byte
		ordinal int
		row     []model.Value
	}
	items := make([]item, len(rows))
	kv := make([]model.Value, len(keys))
	for i, r := range rows {
		for k, col := range keyCols {
			kv[k] = r[col]
		}
		enc, err := encoder.EncodePrefix(t.Context(), keys, kv, 16<<10)
		if err != nil {
			t.Fatal(err)
		}
		items[i] = item{key: enc, ordinal: i, row: r}
	}
	slices.SortFunc(items, func(a, b item) int {
		if n := bytes.Compare(a.key, b.key); n != 0 {
			return n
		}
		return a.ordinal - b.ordinal
	})
	if count == 0 || offset >= uint64(len(items)) {
		return nil
	}
	end := uint64(len(items))
	if count != math.MaxUint64 && offset <= math.MaxUint64-count && offset+count < end {
		end = offset + count
		if withTies {
			boundary := items[end-1].key
			for end < uint64(len(items)) && bytes.Equal(items[end].key, boundary) {
				end++
			}
		}
	}
	out := make([][]model.Value, 0, end-offset)
	for i := offset; i < end; i++ {
		out = append(out, items[i].row)
	}
	return out
}

func topKOrderSpec(slots []physical.Slot, descending, nullsFirst bool) []physical.OrderKey {
	dir := ir.Ascending
	if descending {
		dir = ir.Descending
	}
	nulls := ir.NullsLast
	if nullsFirst {
		nulls = ir.NullsFirst
	}
	return []physical.OrderKey{{
		Key:       physical.Key{Slot: slots[0], Type: topKInt.Type},
		Direction: dir,
		Nulls:     nulls,
	}}
}

func TestTopKBoundedHeapOrderOffsetLimit(t *testing.T) {
	inv, q, _ := topKTestInvocation(t, 1<<20, 16<<20, 7)
	layouts := []model.TypeLayout{topKInt, topKInt, topKText}
	slots := []physical.Slot{
		{Graph: 1, ID: 1},
		{Graph: 1, ID: 2},
		{Graph: 1, ID: 3},
	}

	// Build 200 unsorted rows with repeating keys to also check input-order stability.
	rows := make([][]model.Value, 200)
	for i := range rows {
		key := int64((i*37 + 13) % 50)
		rows[i] = []model.Value{model.Int64Val(key), model.Int64Val(int64(i)), model.StringVal("row-" + strings.Repeat("x", i%17))}
	}

	node := physical.NodeSpec{
		Algorithm: physical.TopK,
		Output:    slots,
		Delivered: physical.Properties{
			Ordering:     topKOrderSpec(slots, false, false),
			Distribution: physical.Distribution{Kind: physical.SingleNode, Partitions: 1},
		},
		Limit: physical.LimitSpec{Scope: physical.GlobalLimit, Offset: 3, Count: 5},
	}
	src := newTopKTestSource(t, layouts, rows, 7)
	cur, err := inv.topK(src, node, slots, layouts, 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	tk, ok := cur.(*topKCursor)
	if !ok {
		t.Fatalf("expected *topKCursor, got %T", cur)
	}

	got := topKDrain(t, tk, 3)
	if tk.fellBack {
		t.Fatalf("expected bounded heap execution without external sort fallback")
	}
	keys := []storage.KeyColumn{{ColumnID: 1, Type: topKInt.Type, Descending: false, NullsFirst: false}}
	want := topKReference(t, inv.portal.env.Keys, rows, keys, []int{0}, 3, 5, false)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("topK rows mismatch:\n got=%v\nwant=%v", got, want)
	}
	if err := tk.Close(); err != nil {
		t.Fatal(err)
	}
	if u := q.Usage(); u.Work.Live != 0 || u.Output.Live != 0 || u.Files != 0 {
		t.Fatalf("topK retained resources: %+v", u)
	}
}

func TestTopKDescendingAndNullsFirstLast(t *testing.T) {
	layouts := []model.TypeLayout{topKInt, topKInt}
	slots := []physical.Slot{{Graph: 1, ID: 1}, {Graph: 1, ID: 2}}
	rows := [][]model.Value{
		{model.Int64Val(5), model.Int64Val(0)},
		{model.NullVal(model.TypeInt64), model.Int64Val(1)},
		{model.Int64Val(1), model.Int64Val(2)},
		{model.Int64Val(3), model.Int64Val(3)},
		{model.NullVal(model.TypeInt64), model.Int64Val(4)},
		{model.Int64Val(5), model.Int64Val(5)},
		{model.Int64Val(2), model.Int64Val(6)},
		{model.Int64Val(4), model.Int64Val(7)},
		{model.Int64Val(1), model.Int64Val(8)},
	}

	for _, desc := range []bool{false, true} {
		for _, nullsFirst := range []bool{false, true} {
			name := "ASC"
			if desc {
				name = "DESC"
			}
			if nullsFirst {
				name += "_NULLS_FIRST"
			} else {
				name += "_NULLS_LAST"
			}
			t.Run(name, func(t *testing.T) {
				inv, _, _ := topKTestInvocation(t, 1<<20, 16<<20, 4)
				node := physical.NodeSpec{
					Algorithm: physical.TopK,
					Output:    slots,
					Delivered: physical.Properties{
						Ordering:     topKOrderSpec(slots, desc, nullsFirst),
						Distribution: physical.Distribution{Kind: physical.SingleNode, Partitions: 1},
					},
					Limit: physical.LimitSpec{Scope: physical.GlobalLimit, Offset: 1, Count: 4},
				}
				cur, err := inv.topK(newTopKTestSource(t, layouts, rows, 4), node, slots, layouts, 1, 4)
				if err != nil {
					t.Fatal(err)
				}
				defer cur.Close()
				got := topKDrain(t, cur, 2)
				keys := []storage.KeyColumn{{ColumnID: 1, Type: topKInt.Type, Descending: desc, NullsFirst: nullsFirst}}
				want := topKReference(t, inv.portal.env.Keys, rows, keys, []int{0}, 1, 4, false)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("got=%v, want=%v", got, want)
				}
			})
		}
	}
}

func TestTopKWithTiesComplexEvictions(t *testing.T) {
	layouts := []model.TypeLayout{topKInt, topKInt}
	slots := []physical.Slot{{Graph: 1, ID: 1}, {Graph: 1, ID: 2}}

	cases := []struct {
		name   string
		keys   []int64
		offset uint64
		count  uint64
	}{
		{
			// Boundary key 2 appears multiple times; 1 arrives and evicts one copy of 2 while
			// another copy of 2 stays as heap[0] (evicted copy must move to ties!).
			name:   "partial_boundary_eviction_keeps_all_peers",
			keys:   []int64{2, 2, 2, 1, 2, 3, 2},
			offset: 0,
			count:  2,
		},
		{
			// Boundary key 2 accumulates ties, then smaller keys 0 and 1 push heap[0] down to 1,
			// discarding all old key=2 ties cleanly.
			name:   "boundary_drop_flushes_old_ties",
			keys:   []int64{2, 2, 2, 1, 2, 0, 1},
			offset: 0,
			count:  2,
		},
		{
			// OFFSET combined with WITH TIES retains complete peer group of (offset+count)-th row.
			name:   "offset_with_ties",
			keys:   []int64{5, 2, 1, 2, 4, 2, 3, 1},
			offset: 2,
			count:  2,
		},
		{
			// Fewer rows than offset+count.
			name:   "fewer_rows_than_k",
			keys:   []int64{3, 1, 2},
			offset: 1,
			count:  10,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv, _, _ := topKTestInvocation(t, 1<<20, 16<<20, 3)
			rows := make([][]model.Value, len(tc.keys))
			for i, k := range tc.keys {
				rows[i] = []model.Value{model.Int64Val(k), model.Int64Val(int64(i))}
			}
			node := physical.NodeSpec{
				Algorithm: physical.TopK,
				Output:    slots,
				Delivered: physical.Properties{
					Ordering:     topKOrderSpec(slots, false, false),
					Distribution: physical.Distribution{Kind: physical.SingleNode, Partitions: 1},
				},
				Limit: physical.LimitSpec{Scope: physical.GlobalLimit, Offset: tc.offset, Count: tc.count, WithTies: true},
			}
			cur, err := inv.topK(newTopKTestSource(t, layouts, rows, 3), node, slots, layouts, tc.offset, tc.count)
			if err != nil {
				t.Fatal(err)
			}
			tk, ok := cur.(*topKCursor)
			if !ok {
				t.Fatalf("expected *topKCursor, got %T", cur)
			}
			defer tk.Close()
			got := topKDrain(t, tk, 2)
			if tk.fellBack {
				t.Fatalf("unexpected fallback on small WITH TIES test")
			}
			keyCols := []storage.KeyColumn{{ColumnID: 1, Type: topKInt.Type, Descending: false, NullsFirst: false}}
			want := topKReference(t, inv.portal.env.Keys, rows, keyCols, []int{0}, tc.offset, tc.count, true)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("WITH TIES mismatch:\n got=%v\nwant=%v", got, want)
			}
		})
	}
}

func TestTopKAlreadySortedFastPathStreamsWithoutSorting(t *testing.T) {
	inv, _, _ := topKTestInvocation(t, 1<<20, 16<<20, 4)

	// Build a validated 2-node physical plan: child (Values/Sort) already delivers Ordering -> TopK parent.
	b, err := ir.NewBuilder(ir.Limits{MaxNodes: 64, MaxOperands: 128, MaxSlots: 16, MaxDepth: 16, MaxBytes: 1 << 20, SourceBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	intLayoutID, err := b.AddLayout(t.Context(), topKInt)
	if err != nil {
		t.Fatal(err)
	}
	slotID, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: intLayoutID, Nullable: false, Object: 1, Column: 1})
	if err != nil {
		t.Fatal(err)
	}
	valScalar, err := b.AddScalar(t.Context(), ir.ScalarSpec{
		Scalar: ir.Scalar{Kind: ir.Const, Scope: scope, Layout: intLayoutID},
		Value:  model.Int64Val(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	valuesRel, err := b.AddRel(t.Context(), ir.RelSpec{
		Rel:         ir.Rel{Kind: ir.Values, Scope: scope, Rows: 1},
		Outputs:     []model.SlotID{slotID},
		Expressions: []ir.ScalarID{valScalar},
	})
	if err != nil {
		t.Fatal(err)
	}
	slotScalar, err := b.AddScalar(t.Context(), ir.ScalarSpec{
		Scalar: ir.Scalar{Kind: ir.Slot, Scope: scope, Layout: intLayoutID, Slot: slotID},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ltOp model.OperatorID
	for op := range builtin.OperatorsNamed("<") {
		if op.Left == 20 && op.Right == 20 && op.Result == 16 {
			ltOp = op.ID
			break
		}
	}
	if ltOp == 0 {
		t.Fatal("missing int8 < operator")
	}
	sortRel, err := b.AddRel(t.Context(), ir.RelSpec{
		Rel:     ir.Rel{Kind: ir.Sort, Scope: scope},
		Inputs:  []ir.RelID{valuesRel},
		Outputs: []model.SlotID{slotID},
		Order:   []ir.OrderKey{{Expr: slotScalar, Direction: ir.Ascending, Nulls: ir.NullsLast, Operator: ltOp}},
	})
	if err != nil {
		t.Fatal(err)
	}
	countScalar, err := b.AddScalar(t.Context(), ir.ScalarSpec{
		Scalar: ir.Scalar{Kind: ir.Const, Scope: scope, Layout: intLayoutID},
		Value:  model.Int64Val(3),
	})
	if err != nil {
		t.Fatal(err)
	}
	limitRel, err := b.AddRel(t.Context(), ir.RelSpec{
		Rel:     ir.Rel{Kind: ir.Limit, Scope: scope, Count: countScalar, Effects: ir.Effects{MayError: true}},
		Inputs:  []ir.RelID{sortRel},
		Outputs: []model.SlotID{slotID},
	})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := b.Build(t.Context(), limitRel)
	if err != nil {
		t.Fatal(err)
	}

	slot := physical.Slot{Graph: 1, ID: slotID, Layout: intLayoutID, Nullable: false}
	order := []physical.OrderKey{{
		Key:       physical.Key{Slot: slot, Type: topKInt.Type},
		Direction: ir.Ascending,
		Nulls:     ir.NullsLast,
		Operator:  ltOp,
	}}
	single := physical.Distribution{Kind: physical.SingleNode, Partitions: 1}
	plan, err := physical.NewPlan(t.Context(), physical.Spec{
		Graphs: []*ir.Graph{graph},
		Nodes: []physical.NodeSpec{
			{
				Logical:   physical.RelRef{Graph: 1, Rel: valuesRel},
				Algorithm: physical.Values,
				Output:    []physical.Slot{slot},
				Delivered: physical.Properties{Distribution: single, Rewindability: physical.OnePass},
				Break:     physical.Streaming,
			},
			{
				Logical:   physical.RelRef{Graph: 1, Rel: sortRel},
				Algorithm: physical.Sort,
				Children:  []physical.NodeID{1},
				Output:    []physical.Slot{slot},
				Required:  []physical.Properties{{Distribution: single, Rewindability: physical.OnePass}},
				Delivered: physical.Properties{Ordering: order, Distribution: single, Rewindability: physical.OnePass},
				Break:     physical.MaterializeInput,
				Memory:    physical.Memory{PeakBytes: 1 << 20, Spillable: true},
			},
			{
				Logical:   physical.RelRef{Graph: 1, Rel: limitRel},
				Algorithm: physical.TopK,
				Children:  []physical.NodeID{2},
				Output:    []physical.Slot{slot},
				Required:  []physical.Properties{{Distribution: single, Rewindability: physical.OnePass}},
				Delivered: physical.Properties{Ordering: order, Distribution: single, Rewindability: physical.OnePass},
				Break:     physical.MaterializeInput,
				Memory:    physical.Memory{PeakBytes: 1 << 20, Spillable: true},
				Limit:     physical.LimitSpec{Scope: physical.GlobalLimit, Offset: 0, Count: 3},
			},
		},
		Root: 3,
	}, physical.Limits{MaxNodes: 32, MaxOperands: 128, MaxProperties: 128, MaxDepth: 32, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	inv.plan = plan
	topKNode, err := plan.CloneNode(t.Context(), 3, 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	// Already-sorted input with 100 rows.
	rows := make([][]model.Value, 100)
	for i := range rows {
		rows[i] = []model.Value{model.Int64Val(int64(i + 1))}
	}
	src := newTopKTestSource(t, []model.TypeLayout{topKInt}, rows, 4)
	cur, err := inv.topK(src, topKNode, []physical.Slot{slot}, []model.TypeLayout{topKInt}, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	lim, ok := cur.(*limitCursor)
	if !ok {
		t.Fatalf("already-sorted TopK should stream via *limitCursor, got %T", cur)
	}
	if lim.input != src {
		t.Fatalf("already-sorted TopK wrapped input in an unexpected cursor: %T", lim.input)
	}
	defer lim.Close()
	got := topKDrain(t, lim, 4)
	want := [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}, {model.Int64Val(3)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("already-sorted TopK got=%v, want=%v", got, want)
	}
	if src.emitted > 3 {
		t.Fatalf("already-sorted TopK drained %d rows from source, expected streaming stop at <= 3", src.emitted)
	}
}

func TestTopKLargeKAndBudgetFallbackToExternalSort(t *testing.T) {
	layouts := []model.TypeLayout{topKInt, topKText}
	slots := []physical.Slot{{Graph: 1, ID: 1}, {Graph: 1, ID: 2}}

	t.Run("upfront_unbounded_falls_back_to_exsort", func(t *testing.T) {
		inv, _, _ := topKTestInvocation(t, 64<<10, 16<<20, 4)
		rows := [][]model.Value{
			{model.Int64Val(3), model.StringVal("c")},
			{model.Int64Val(1), model.StringVal("a")},
			{model.Int64Val(2), model.StringVal("b")},
			{model.Int64Val(4), model.StringVal("d")},
		}
		node := physical.NodeSpec{
			Algorithm: physical.TopK,
			Output:    slots,
			Delivered: physical.Properties{
				Ordering:     topKOrderSpec(slots, false, false),
				Distribution: physical.Distribution{Kind: physical.SingleNode, Partitions: 1},
			},
			Limit: physical.LimitSpec{Scope: physical.GlobalLimit, Offset: 1, Count: 0},
		}
		cur, err := inv.topK(newTopKTestSource(t, layouts, rows, 2), node, slots, layouts, 1, math.MaxUint64)
		if err != nil {
			t.Fatal(err)
		}
		if _, isLimit := cur.(*limitCursor); !isLimit {
			t.Fatalf("unbounded TopK should fall back upfront to *limitCursor+exsort, got %T", cur)
		}
		defer cur.Close()
		got := topKDrain(t, cur, 2)
		want := [][]model.Value{
			{model.Int64Val(2), model.StringVal("b")},
			{model.Int64Val(3), model.StringVal("c")},
			{model.Int64Val(4), model.StringVal("d")},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("unbounded fallback got=%v, want=%v", got, want)
		}
	})

	t.Run("upfront_large_k_falls_back_to_exsort", func(t *testing.T) {
		// RunBytes=4096 cannot admit K=10000 upfront.
		inv, _, _ := topKTestInvocation(t, 4096, 16<<20, 4)
		rows := [][]model.Value{
			{model.Int64Val(9), model.StringVal("nine")},
			{model.Int64Val(1), model.StringVal("one")},
			{model.Int64Val(5), model.StringVal("five")},
		}
		node := physical.NodeSpec{
			Algorithm: physical.TopK,
			Output:    slots,
			Delivered: physical.Properties{
				Ordering:     topKOrderSpec(slots, false, false),
				Distribution: physical.Distribution{Kind: physical.SingleNode, Partitions: 1},
			},
			Limit: physical.LimitSpec{Scope: physical.GlobalLimit, Offset: 0, Count: 10000},
		}
		cur, err := inv.topK(newTopKTestSource(t, layouts, rows, 2), node, slots, layouts, 0, 10000)
		if err != nil {
			t.Fatal(err)
		}
		if _, isLimit := cur.(*limitCursor); !isLimit {
			t.Fatalf("large-K TopK should fall back upfront to *limitCursor+exsort, got %T", cur)
		}
		defer cur.Close()
		got := topKDrain(t, cur, 2)
		want := [][]model.Value{
			{model.Int64Val(1), model.StringVal("one")},
			{model.Int64Val(5), model.StringVal("five")},
			{model.Int64Val(9), model.StringVal("nine")},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("large-K fallback got=%v, want=%v", got, want)
		}
	})

	t.Run("midstream_payload_budget_triggers_dynamic_fallback", func(t *testing.T) {
		// RunBytes=4096 admits K=6 upfront (6*minEntryBytes < 4096),
		// but 1200-byte strings in 12 rows exceed heapBudget mid-stream, triggering fallbackToSort.
		inv, q, _ := topKTestInvocation(t, 4096, 16<<20, 4)
		rows := make([][]model.Value, 12)
		for i := range rows {
			key := int64((12 - i) % 5)
			rows[i] = []model.Value{model.Int64Val(key), model.StringVal(strings.Repeat(string(rune('a'+i)), 1200))}
		}
		node := physical.NodeSpec{
			Algorithm: physical.TopK,
			Output:    slots,
			Delivered: physical.Properties{
				Ordering:     topKOrderSpec(slots, false, false),
				Distribution: physical.Distribution{Kind: physical.SingleNode, Partitions: 1},
			},
			Limit: physical.LimitSpec{Scope: physical.GlobalLimit, Offset: 1, Count: 4, WithTies: true},
		}
		cur, err := inv.topK(newTopKTestSource(t, layouts, rows, 4), node, slots, layouts, 1, 4)
		if err != nil {
			t.Fatal(err)
		}
		tk, ok := cur.(*topKCursor)
		if !ok {
			t.Fatalf("expected *topKCursor before mid-stream fallback, got %T", cur)
		}
		got := topKDrain(t, tk, 3)
		if !tk.fellBack {
			t.Fatalf("expected mid-stream payload budget overflow to trigger external sort fallback")
		}
		keyCols := []storage.KeyColumn{{ColumnID: 1, Type: topKInt.Type, Descending: false, NullsFirst: false}}
		want := topKReference(t, inv.portal.env.Keys, rows, keyCols, []int{0}, 1, 4, true)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("mid-stream fallback mismatch: got %d rows, want %d rows", len(got), len(want))
		}
		if err := tk.Close(); err != nil {
			t.Fatal(err)
		}
		if u := q.Usage(); u.Work.Live != 0 || u.Output.Live != 0 || u.Files != 0 {
			t.Fatalf("mid-stream fallback retained resources: %+v", u)
		}
	})
}

func TestCursorBuilderReuseAcrossBatches(t *testing.T) {
	layouts := []model.TypeLayout{topKInt, topKText}
	rows := make([][]model.Value, 10)
	for i := range rows {
		rows[i] = []model.Value{model.Int64Val(int64(i + 1)), model.StringVal("val-" + strings.Repeat("z", i+1))}
	}

	t.Run("limitCursor", func(t *testing.T) {
		inv, q, _ := topKTestInvocation(t, 1<<20, 16<<20, 3)
		lim := &limitCursor{
			input:     newTopKTestSource(t, layouts, rows, 3),
			env:       &inv.portal.env,
			layouts:   layouts,
			offset:    1,
			remaining: 8,
			source:    columnar.Batch{Columns: make([]columnar.Vector, 0, len(layouts))},
		}
		defer lim.Close()
		var firstBuilder *columnar.Builder
		var got [][]model.Value
		out := columnar.Batch{MaxRows: 3}
		for {
			err := lim.Next(t.Context(), &out)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if firstBuilder == nil {
				firstBuilder = lim.output
			} else if lim.output != firstBuilder {
				t.Fatalf("limitCursor allocated a new columnar.Builder across batches")
			}
			for i := range out.Rows() {
				v0, _, _ := columnar.CopyValue(t.Context(), &out.Columns[0], out.Lane(i), 1<<20)
				v1, _, _ := columnar.CopyValue(t.Context(), &out.Columns[1], out.Lane(i), 1<<20)
				got = append(got, []model.Value{v0, v1})
			}
		}
		if !reflect.DeepEqual(got, rows[1:9]) {
			t.Fatalf("limitCursor got=%v, want=%v", got, rows[1:9])
		}
		if lim.output != nil {
			t.Fatalf("limitCursor did not close output builder on EOF")
		}
		if err := lim.Close(); err != nil {
			t.Fatal(err)
		}
		if u := q.Usage(); u.Output.Live != 0 || u.Work.Live != 0 {
			t.Fatalf("limitCursor retained resources: %+v", u)
		}
	})

	t.Run("materializeCursor", func(t *testing.T) {
		inv, q, _ := topKTestInvocation(t, 1<<20, 16<<20, 3)
		mat := &materializeCursor{
			input:   newTopKTestSource(t, layouts, rows, 3),
			env:     &inv.portal.env,
			layouts: layouts,
			source:  columnar.Batch{Columns: make([]columnar.Vector, 0, len(layouts))},
		}
		defer mat.Close()
		var firstBuilder *columnar.Builder
		var got [][]model.Value
		out := columnar.Batch{MaxRows: 3}
		for {
			err := mat.Next(t.Context(), &out)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if firstBuilder == nil {
				firstBuilder = mat.output
			} else if mat.output != firstBuilder {
				t.Fatalf("materializeCursor allocated a new columnar.Builder across batches")
			}
			for i := range out.Rows() {
				v0, _, _ := columnar.CopyValue(t.Context(), &out.Columns[0], out.Lane(i), 1<<20)
				v1, _, _ := columnar.CopyValue(t.Context(), &out.Columns[1], out.Lane(i), 1<<20)
				got = append(got, []model.Value{v0, v1})
			}
		}
		if !reflect.DeepEqual(got, rows) {
			t.Fatalf("materializeCursor got=%v, want=%v", got, rows)
		}
		if mat.output != nil {
			t.Fatalf("materializeCursor did not close output builder on EOF")
		}
		if err := mat.Close(); err != nil {
			t.Fatal(err)
		}
		if u := q.Usage(); u.Output.Live != 0 || u.Work.Live != 0 {
			t.Fatalf("materializeCursor retained resources: %+v", u)
		}
	})

	t.Run("sharedCursor", func(t *testing.T) {
		inv, q, _ := topKTestInvocation(t, 1<<20, 16<<20, 3)
		prod := &sharedProducer{
			input:   newTopKTestSource(t, layouts, rows, 3),
			env:     &inv.portal.env,
			layouts: layouts,
			source:  columnar.Batch{Columns: make([]columnar.Vector, 0, len(layouts))},
		}
		defer prod.Close()
		sc := &sharedCursor{
			producer: prod,
			env:      &inv.portal.env,
			layouts:  layouts,
		}
		defer sc.Close()
		var firstBuilder *columnar.Builder
		var got [][]model.Value
		out := columnar.Batch{MaxRows: 3}
		for {
			err := sc.Next(t.Context(), &out)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if firstBuilder == nil {
				firstBuilder = sc.output
			} else if sc.output != firstBuilder {
				t.Fatalf("sharedCursor allocated a new columnar.Builder across batches")
			}
			for i := range out.Rows() {
				v0, _, _ := columnar.CopyValue(t.Context(), &out.Columns[0], out.Lane(i), 1<<20)
				v1, _, _ := columnar.CopyValue(t.Context(), &out.Columns[1], out.Lane(i), 1<<20)
				got = append(got, []model.Value{v0, v1})
			}
		}
		if !reflect.DeepEqual(got, rows) {
			t.Fatalf("sharedCursor got=%v, want=%v", got, rows)
		}
		if sc.output != nil {
			t.Fatalf("sharedCursor did not close output builder on EOF")
		}
		if err := sc.Close(); err != nil {
			t.Fatal(err)
		}
		if err := prod.Close(); err != nil {
			t.Fatal(err)
		}
		if u := q.Usage(); u.Output.Live != 0 || u.Work.Live != 0 {
			t.Fatalf("sharedCursor retained resources: %+v", u)
		}
	})

	t.Run("valuesCursor", func(t *testing.T) {
		inv, q, _ := topKTestInvocation(t, 1<<20, 16<<20, 3)
		vc := &valuesCursor{
			env:  &inv.portal.env,
			rows: 10,
		}
		defer vc.Close()
		var firstBuilder *columnar.Builder
		totalRows := 0
		out := columnar.Batch{MaxRows: 3}
		for {
			err := vc.Next(t.Context(), &out)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if firstBuilder == nil {
				firstBuilder = vc.output
			} else if vc.output != firstBuilder {
				t.Fatalf("valuesCursor allocated a new columnar.Builder across batches")
			}
			totalRows += out.Rows()
		}
		if totalRows != 10 {
			t.Fatalf("valuesCursor rows = %d, want 10", totalRows)
		}
		if vc.output != nil {
			t.Fatalf("valuesCursor did not close output builder on EOF")
		}
		if err := vc.Close(); err != nil {
			t.Fatal(err)
		}
		if u := q.Usage(); u.Output.Live != 0 || u.Work.Live != 0 {
			t.Fatalf("valuesCursor retained resources: %+v", u)
		}
	})
}

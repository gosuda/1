package join

import (
	"context"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"github.com/zeebo/xxh3"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type recursiveTestSource struct {
	builder *columnar.Builder
	rows    [][]model.Value
	index   int
	closed  bool
}

func newRecursiveTestSource(t *testing.T, layouts []model.TypeLayout, rows [][]model.Value) *recursiveTestSource {
	t.Helper()
	b, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: 8, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s := &recursiveTestSource{builder: b, rows: rows}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func (s *recursiveTestSource) Next(ctx context.Context, out *columnar.Batch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.builder.Reset()
	if s.index >= len(s.rows) {
		return io.EOF
	}
	n := 8
	if out.MaxRows > 0 {
		n = min(n, out.MaxRows)
	}
	for range min(n, len(s.rows)-s.index) {
		if err := s.builder.Append(ctx, s.rows[s.index]); err != nil {
			return err
		}
		s.index++
	}
	return s.builder.Publish(out)
}

func (s *recursiveTestSource) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.builder.Close()
}

type recursivePartitionedSource struct {
	builder   *columnar.Builder
	rows      [][]model.Value
	parts     []uint32
	index     int
	published uint32
	closed    bool
}

func newRecursivePartitionedSource(t *testing.T, layouts []model.TypeLayout, encoder storage.KeyEncoder, count uint32, rows [][]model.Value) *recursivePartitionedSource {
	t.Helper()
	b, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: 1, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	keyLayout := []storage.KeyColumn{{ColumnID: 1, Type: layouts[0].Type, NullsFirst: true}}
	parts := make([]uint32, len(rows))
	for i, r := range rows {
		enc, err := encoder.EncodePrefix(t.Context(), keyLayout, r[:1], 4096)
		if err != nil {
			t.Fatal(err)
		}
		parts[i] = uint32(xxh3.Hash(enc) % uint64(count))
	}
	s := &recursivePartitionedSource{builder: b, rows: rows, parts: parts}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func (s *recursivePartitionedSource) Partition() uint32 { return s.published }

func (s *recursivePartitionedSource) Next(ctx context.Context, out *columnar.Batch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.builder.Reset()
	if s.index >= len(s.rows) {
		return io.EOF
	}
	if err := s.builder.Append(ctx, s.rows[s.index]); err != nil {
		return err
	}
	s.published = s.parts[s.index]
	s.index++
	return s.builder.Publish(out)
}

func (s *recursivePartitionedSource) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.builder.Close()
}

func recursiveTestQuery(t *testing.T, dir string, maxFiles int) *runtime.Query {
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
	budget, err := runtime.NewBudget(64<<20, 4096)
	if err != nil {
		t.Fatal(err)
	}
	q, err := runtime.NewQuery(t.Context(), pool, budget, runtime.OSFileSystem{}, runtime.QueryConfig{
		WorkBytes:   32 << 20,
		OutputBytes: 4 << 20,
		SpillBytes:  32 << 20,
		Directory:   dir,
		MaxFiles:    maxFiles,
		MaxReaders:  2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := q.Close(); err != nil {
			t.Error(err)
		}
	})
	return q
}

func recursiveCollect(t *testing.T, c storage.BatchCursor) [][]model.Value {
	t.Helper()
	var rows [][]model.Value
	out := columnar.Batch{MaxRows: 8}
	for {
		err := c.Next(t.Context(), &out)
		if err == io.EOF {
			return rows
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
			rows = append(rows, row)
		}
	}
}

func recursiveBag(rows [][]model.Value) []string {
	result := make([]string, 0, len(rows))
	for _, row := range rows {
		fields := make([]string, len(row))
		for i, v := range row {
			if v.IsNull {
				fields[i] = "NULL"
			} else {
				fields[i] = fmt.Sprint(v.IntVal)
			}
		}
		result = append(result, strings.Join(fields, ","))
	}
	slices.Sort(result)
	return result
}

func recursiveReferenceBag(kind ir.JoinKind, left, right [][]model.Value) []string {
	var out [][]model.Value
	rightMatched := make([]bool, len(right))
	for _, l := range left {
		matched := false
		for j, r := range right {
			if !l[0].IsNull && !r[0].IsNull && l[0].IntVal == r[0].IntVal {
				matched = true
				rightMatched[j] = true
				if kind != ir.SemiJoin && kind != ir.AntiJoin {
					out = append(out, []model.Value{l[1], r[1]})
				}
			}
		}
		switch kind {
		case ir.SemiJoin:
			if matched {
				out = append(out, []model.Value{l[1]})
			}
		case ir.AntiJoin:
			if !matched {
				out = append(out, []model.Value{l[1]})
			}
		case ir.LeftJoin, ir.FullJoin:
			if !matched {
				out = append(out, []model.Value{l[1], model.NullVal(model.TypeInt64)})
			}
		}
	}
	if kind == ir.RightJoin || kind == ir.FullJoin {
		for j, r := range right {
			if !rightMatched[j] {
				out = append(out, []model.Value{model.NullVal(model.TypeInt64), r[1]})
			}
		}
	}
	return recursiveBag(out)
}

func recursiveAssertReleased(t *testing.T, q *runtime.Query, dir string) {
	t.Helper()
	u := q.Usage()
	if u.Work.Live != 0 || u.Work.Reservations != 0 || u.Output.Live != 0 || u.Output.Reservations != 0 || u.Spill.Live != 0 || u.Files != 0 || u.Readers != 0 {
		t.Fatalf("join retained resources: %+v", u)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("spill cleanup: files=%v err=%v", files, err)
	}
}

func recursiveBaseOptions(t *testing.T, q *runtime.Query, kind ir.JoinKind, hashRows uint64, maxDepth int) Options {
	t.Helper()
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	intLayout := model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
	perEntry := uint64(unsafe.Sizeof(entry{})) + uint64(unsafe.Sizeof(int(0)))*2
	o := Options{
		Kind:           kind,
		LeftLayouts:    []model.TypeLayout{intLayout, intLayout},
		RightLayouts:   []model.TypeLayout{intLayout, intLayout},
		OutputLayouts:  []model.TypeLayout{intLayout, intLayout},
		Output:         []Column{{Side: Left, Index: 1}, {Side: Right, Index: 1}},
		Keys:           []Key{{Left: 0, Right: 0}},
		Encoder:        encoder,
		Query:          q,
		BatchRows:      8,
		BatchBytes:     1 << 16,
		MaxRecordBytes: 4096,
		MemoryBytes:    0,
		HashBytes:      perEntry * hashRows,
		WorkBytes:      16 << 20,
		PartitionBytes: 4 << 20,
		MaxPartitions:  2,
		MaxDepth:       maxDepth,
	}
	if kind == ir.SemiJoin || kind == ir.AntiJoin {
		o.Output = o.Output[:1]
		o.OutputLayouts = o.OutputLayouts[:1]
	}
	return o
}

func TestHashJoinRecursivePartitioningSplitsOversizedPartitions(t *testing.T) {
	var left, right [][]model.Value
	for k := int64(1); k <= 24; k++ {
		if k <= 18 {
			left = append(left, []model.Value{model.Int64Val(k), model.Int64Val(100 + k)})
		}
		if k >= 5 {
			right = append(right, []model.Value{model.Int64Val(k), model.Int64Val(200 + k)})
		}
	}
	left = append(left, []model.Value{model.NullVal(model.TypeInt64), model.Int64Val(901)})
	right = append(right, []model.Value{model.NullVal(model.TypeInt64), model.Int64Val(902)})

	for _, kind := range []ir.JoinKind{ir.InnerJoin, ir.LeftJoin, ir.RightJoin, ir.FullJoin, ir.SemiJoin, ir.AntiJoin} {
		t.Run(fmt.Sprintf("kind=%d", kind), func(t *testing.T) {
			dir := t.TempDir()
			q := recursiveTestQuery(t, dir, 64)
			o := recursiveBaseOptions(t, q, kind, 5, 5)
			raw, err := New(t.Context(), newRecursiveTestSource(t, o.LeftLayouts, left), newRecursiveTestSource(t, o.RightLayouts, right), o)
			if err != nil {
				t.Fatal(err)
			}
			cur := raw.(*cursor)
			got := recursiveBag(recursiveCollect(t, cur))
			want := recursiveReferenceBag(kind, left, right)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("bag mismatch:\n got=%v\nwant=%v", got, want)
			}
			if cur.repartitions == 0 {
				t.Fatalf("expected recursive partitioning to split oversized partitions, got repartitions=0")
			}
			if cur.replayFallbacks != 0 {
				t.Fatalf("expected zero nested-loop replay fallbacks for distinct keys, got %d", cur.replayFallbacks)
			}
			if err := cur.Close(); err != nil {
				t.Fatal(err)
			}
			recursiveAssertReleased(t, q, dir)
		})
	}
}

func TestHashJoinDuplicateKeySkewFallsBackCleanly(t *testing.T) {
	var left, right [][]model.Value
	for i := int64(0); i < 6; i++ {
		left = append(left, []model.Value{model.Int64Val(42), model.Int64Val(10 + i)})
	}
	for i := int64(0); i < 12; i++ {
		right = append(right, []model.Value{model.Int64Val(42), model.Int64Val(100 + i)})
	}

	for _, kind := range []ir.JoinKind{ir.InnerJoin, ir.LeftJoin, ir.RightJoin, ir.FullJoin, ir.SemiJoin, ir.AntiJoin} {
		t.Run(fmt.Sprintf("kind=%d", kind), func(t *testing.T) {
			dir := t.TempDir()
			q := recursiveTestQuery(t, dir, 64)
			o := recursiveBaseOptions(t, q, kind, 4, 4)
			raw, err := New(t.Context(), newRecursiveTestSource(t, o.LeftLayouts, left), newRecursiveTestSource(t, o.RightLayouts, right), o)
			if err != nil {
				t.Fatal(err)
			}
			cur := raw.(*cursor)
			got := recursiveBag(recursiveCollect(t, cur))
			want := recursiveReferenceBag(kind, left, right)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("skew bag mismatch:\n got=%v\nwant=%v", got, want)
			}
			if cur.repartitions != 0 {
				t.Fatalf("single-key skew should skip useless repartitioning, got repartitions=%d", cur.repartitions)
			}
			if cur.replayFallbacks != 1 {
				t.Fatalf("expected exactly 1 replay fallback for single-key skew, got %d", cur.replayFallbacks)
			}
			if err := cur.Close(); err != nil {
				t.Fatal(err)
			}
			recursiveAssertReleased(t, q, dir)
		})
	}
}

func TestHashJoinMixedSkewSplitsDistinctKeysAndIsolatesHotKey(t *testing.T) {
	var left, right [][]model.Value
	for k := int64(1); k <= 20; k++ {
		left = append(left, []model.Value{model.Int64Val(k), model.Int64Val(100 + k)})
		right = append(right, []model.Value{model.Int64Val(k), model.Int64Val(200 + k)})
	}
	// Add a hot duplicate key (999) exceeding the per-partition hash table budget of 5 rows.
	for i := int64(0); i < 3; i++ {
		left = append(left, []model.Value{model.Int64Val(999), model.Int64Val(500 + i)})
	}
	for i := int64(0); i < 9; i++ {
		right = append(right, []model.Value{model.Int64Val(999), model.Int64Val(600 + i)})
	}

	dir := t.TempDir()
	q := recursiveTestQuery(t, dir, 64)
	o := recursiveBaseOptions(t, q, ir.FullJoin, 5, 5)
	raw, err := New(t.Context(), newRecursiveTestSource(t, o.LeftLayouts, left), newRecursiveTestSource(t, o.RightLayouts, right), o)
	if err != nil {
		t.Fatal(err)
	}
	cur := raw.(*cursor)
	got := recursiveBag(recursiveCollect(t, cur))
	want := recursiveReferenceBag(ir.FullJoin, left, right)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mixed-skew bag mismatch:\n got=%v\nwant=%v", got, want)
	}
	if cur.repartitions == 0 {
		t.Fatalf("expected recursive partitioning to isolate hot key from distinct keys")
	}
	if cur.replayFallbacks != 1 {
		t.Fatalf("expected only the isolated hot-key leaf to fall back to replay, got %d", cur.replayFallbacks)
	}
	if err := cur.Close(); err != nil {
		t.Fatal(err)
	}
	recursiveAssertReleased(t, q, dir)
}

func TestHashJoinPartitionedRecursivePreservesLogicalPartitionOrder(t *testing.T) {
	var left, right [][]model.Value
	for k := int64(1); k <= 24; k++ {
		left = append(left, []model.Value{model.Int64Val(k), model.Int64Val(100 + k)})
		right = append(right, []model.Value{model.Int64Val(k), model.Int64Val(200 + k)})
	}

	dir := t.TempDir()
	q := recursiveTestQuery(t, dir, 64)
	o := recursiveBaseOptions(t, q, ir.FullJoin, 5, 5)
	l := newRecursivePartitionedSource(t, o.LeftLayouts, o.Encoder, 2, left)
	r := newRecursivePartitionedSource(t, o.RightLayouts, o.Encoder, 2, right)
	raw, err := NewPartitioned(t.Context(), l, r, Partitioning{Count: 2, KeyIndices: []int{0}}, o)
	if err != nil {
		t.Fatal(err)
	}
	pcur := raw.(*partitionedCursor)

	var collected [][]model.Value
	var lastPart uint32
	seenAny := false
	out := columnar.Batch{MaxRows: 4}
	for {
		err := pcur.Next(t.Context(), &out)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		part := pcur.Partition()
		if seenAny && part < lastPart {
			t.Fatalf("logical partition moved backwards: %d -> %d", lastPart, part)
		}
		seenAny = true
		lastPart = part
		for i := range out.Rows() {
			row := make([]model.Value, len(out.Columns))
			for j := range row {
				v, _, err := columnar.CopyValue(t.Context(), &out.Columns[j], out.Lane(i), 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				row[j] = v
			}
			collected = append(collected, row)
		}
	}
	want := recursiveReferenceBag(ir.FullJoin, left, right)
	if got := recursiveBag(collected); !reflect.DeepEqual(got, want) {
		t.Fatalf("partitioned recursive bag mismatch:\n got=%v\nwant=%v", got, want)
	}
	if pcur.repartitions == 0 {
		t.Fatalf("expected recursive partitioning inside NewPartitioned")
	}
	if pcur.replayFallbacks != 0 {
		t.Fatalf("expected zero replay fallbacks, got %d", pcur.replayFallbacks)
	}
	if err := pcur.Close(); err != nil {
		t.Fatal(err)
	}
	recursiveAssertReleased(t, q, dir)
}

func TestHashJoinTightMaxFilesFallsBackGracefully(t *testing.T) {
	var left, right [][]model.Value
	for k := int64(1); k <= 16; k++ {
		left = append(left, []model.Value{model.Int64Val(k), model.Int64Val(100 + k)})
		right = append(right, []model.Value{model.Int64Val(k), model.Int64Val(200 + k)})
	}

	dir := t.TempDir()
	// Minimum provisioned files = 2*MaxPartitions + 1 = 5; prevents allocating extra recursive child files.
	q := recursiveTestQuery(t, dir, 5)
	o := recursiveBaseOptions(t, q, ir.FullJoin, 3, 4)
	raw, err := New(t.Context(), newRecursiveTestSource(t, o.LeftLayouts, left), newRecursiveTestSource(t, o.RightLayouts, right), o)
	if err != nil {
		t.Fatal(err)
	}
	cur := raw.(*cursor)
	got := recursiveBag(recursiveCollect(t, cur))
	want := recursiveReferenceBag(ir.FullJoin, left, right)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tight-files fallback bag mismatch:\n got=%v\nwant=%v", got, want)
	}
	if err := cur.Close(); err != nil {
		t.Fatal(err)
	}
	recursiveAssertReleased(t, q, dir)
}

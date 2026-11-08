package exec

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/zeebo/xxh3"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/exchange"
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

var partitionSortInt = model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
var partitionSortText = model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString, Collation: 950}}

var partitionSortLayouts = []model.TypeLayout{partitionSortInt, partitionSortInt, partitionSortText}

type partitionSortSource struct {
	builder  *columnar.Builder
	rows     [][]model.Value
	index    int
	closed   bool
	closeErr error
	mask     [1]uint32
}

func (s *partitionSortSource) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if s.index == len(s.rows) {
		return io.EOF
	}
	s.builder.Reset()
	// An inactive lane must not acquire a transport tag or reach the sort.
	if err := s.builder.Append(ctx, []model.Value{model.Int64Val(-1), model.Int64Val(-999), model.StringVal("inactive")}); err != nil {
		return err
	}
	if err := s.builder.Append(ctx, s.rows[s.index]); err != nil {
		return err
	}
	s.index++
	if err := s.builder.Publish(out); err != nil {
		return err
	}
	s.mask[0] = 1
	out.Selection = s.mask[:]
	return nil
}

func (s *partitionSortSource) Close() error {
	if !s.closed {
		s.closed = true
		s.closeErr = errors.Join(s.closeErr, s.builder.Close())
	}
	return s.closeErr
}

func partitionSortEnvironment(t *testing.T, batchBytes uint64) (*invocation, string) {
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
	query, err := runtime.NewQuery(t.Context(), pool, budget, runtime.OSFileSystem{}, runtime.QueryConfig{WorkBytes: 32 << 20, OutputBytes: 8 << 20, SpillBytes: 32 << 20, Directory: dir, MaxFiles: 3, MaxReaders: 2, MaxReservations: 10000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := query.Close(); err != nil {
			t.Error(err)
		}
		if usage := budget.Usage(); usage.Live != 0 || usage.Reservations != 0 {
			t.Errorf("leaked query memory: %+v", usage)
		}
	})
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := query.Allocator(runtime.WorkMemory).Reserve(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(claim.Release)
	env := Environment{Query: query, Keys: encoder, Limits: Limits{BatchRows: 8, BatchBytes: batchBytes, MetadataBytes: 1 << 20}, Sort: exsort.Options{MaxRecordBytes: 4096, RunBytes: 4096, MaxRuns: 4, MergeFanIn: 2, WorkBytes: 16 << 20}}
	return &invocation{portal: &Portal{ctx: t.Context(), env: env}}, dir
}

func partitionSortKeys(t *testing.T, c *invocation) [2]int64 {
	t.Helper()
	var keys [2]int64
	var found [2]bool
	layout := []storage.KeyColumn{{ColumnID: 1, Type: partitionSortInt.Type, NullsFirst: true}}
	for value := int64(0); value < 512; value++ {
		encoded, err := c.portal.env.Keys.EncodePrefix(t.Context(), layout, []model.Value{model.Int64Val(value)}, 4096)
		if err != nil {
			t.Fatal(err)
		}
		partition := xxh3.Hash(encoded) % 2
		keys[partition], found[partition] = value, true
		if found[0] && found[1] {
			return keys
		}
	}
	t.Fatal("could not populate both canonical hash partitions")
	return keys
}

func partitionSortInput(t *testing.T, c *invocation, rows [][]model.Value) (*hashExchangeCursor, *partitionSortSource) {
	t.Helper()
	builder, err := columnar.NewBuilder(partitionSortLayouts, columnar.BuilderOptions{MaxRows: 8, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	source := &partitionSortSource{builder: builder, rows: rows}
	env := &c.portal.env
	hash, err := exchange.NewHashRepartition(t.Context(), []storage.BatchCursor{source}, exchange.HashOptions{Sources: exchange.Options{Query: env.Query, Layouts: partitionSortLayouts, BatchRows: env.Limits.BatchRows, BatchBytes: 1 << 20, QueueBytes: 1 << 20, MetadataBytes: 2 << 20}, Keys: []exchange.HashKey{{Column: 0}}, Encoder: env.Keys, Function: exchange.XXH3V1, Partitions: 2, MaxKeyBytes: 4096, WorkBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	input := &hashExchangeCursor{input: hash}
	t.Cleanup(func() { _ = input.Close() })
	return input, source
}

func partitionSortNode() (physical.NodeSpec, []physical.Slot) {
	slots := []physical.Slot{{Graph: 1, ID: 1}, {Graph: 1, ID: 2}, {Graph: 1, ID: 3}}
	node := physical.NodeSpec{Algorithm: physical.Sort, Delivered: physical.Properties{Distribution: physical.Distribution{Kind: physical.HashPartitioned, Partitions: 2}, Ordering: []physical.OrderKey{{Key: physical.Key{Slot: slots[1], Type: partitionSortInt.Type}, Direction: ir.Ascending, Nulls: ir.NullsLast}}}}
	return node, slots
}

func partitionSortOpen(t *testing.T, c *invocation, input storage.BatchCursor) *partitionSortCursor {
	t.Helper()
	node, slots := partitionSortNode()
	cursor, err := c.partitionSort(node, slots, partitionSortLayouts, input)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	return cursor.(*partitionSortCursor)
}

func partitionSortReleased(t *testing.T, c *invocation, dir string) {
	t.Helper()
	usage := c.portal.env.Query.Usage()
	// The invocation's compiler-metadata reservation lives through Portal.Close.
	if usage.Work.Live != 1<<20 || usage.Output.Live != 0 || usage.Spill.Live != 0 || usage.Files != 0 || usage.Readers != 0 {
		t.Errorf("sort resources remain: %+v", usage)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("sort spill files remain: %v", files)
	}
}

func TestPartitionSortStableRunsSurviveCallerReset(t *testing.T) {
	c, dir := partitionSortEnvironment(t, 64<<10)
	c.portal.env.Sort.RunBytes = 512
	keys := partitionSortKeys(t, c)
	row := func(p int, key model.Value, label string) []model.Value {
		return []model.Value{model.Int64Val(keys[p]), key, model.StringVal(label)}
	}
	input, source := partitionSortInput(t, c, [][]model.Value{
		row(1, model.Int64Val(2), "a"), row(0, model.Int64Val(1), "b"),
		row(1, model.Int64Val(1), "c"), row(0, model.Int64Val(2), "d"),
		row(0, model.Int64Val(1), "e"), row(1, model.NullVal(model.TypeInt64), "f"),
		row(0, model.NullVal(model.TypeInt64), "g"), row(1, model.Int64Val(3), "h"),
	})
	cursor := partitionSortOpen(t, c, input)
	if !cursor.globallyOrdered() {
		t.Fatal("missing global ordering proof")
	}
	var labels []string
	var tags []uint32
	out := columnar.Batch{}
	requests := []int{8, 1, 2, 8}
	for call := 0; ; call++ {
		out.Reset()
		out.MaxRows = requests[call%len(requests)]
		err := cursor.Next(t.Context(), &out)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := out.Validate(8); err != nil {
			t.Fatal(err)
		}
		if len(out.Columns) != 3 {
			t.Fatalf("hidden tag leaked: %d columns", len(out.Columns))
		}
		for row := range out.Rows() {
			lane := out.Lane(row)
			p := out.Columns[0].Int64[out.Columns[0].ValueIndex(lane)]
			if p != keys[cursor.partition()] {
				t.Fatalf("partition %d includes hash key %d", cursor.partition(), p)
			}
			value, _, err := columnar.CopyValue(t.Context(), &out.Columns[2], lane, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			labels = append(labels, value.StrVal)
			tags = append(tags, cursor.partition())
		}
	}
	if want := []string{"b", "c", "e", "a", "d", "h", "f", "g"}; !reflect.DeepEqual(labels, want) {
		t.Fatalf("stable order = %v, want %v", labels, want)
	}
	if want := []uint32{0, 1, 0, 1, 0, 1, 1, 0}; !reflect.DeepEqual(tags, want) {
		t.Fatalf("partition tags = %v, want %v", tags, want)
	}
	if !source.closed {
		t.Fatal("EOF retained source")
	}
	partitionSortReleased(t, c, dir)
}

func TestPartitionSortByteFullBatchesRetainEveryRow(t *testing.T) {
	c, dir := partitionSortEnvironment(t, 6000)
	keys := partitionSortKeys(t, c)
	rows := make([][]model.Value, 17)
	for i := range rows {
		rows[i] = []model.Value{model.Int64Val(keys[0]), model.Int64Val(int64(16 - i)), model.StringVal(strings.Repeat("x", 1000))}
	}
	input, _ := partitionSortInput(t, c, rows)
	cursor := partitionSortOpen(t, c, input)
	out := columnar.Batch{MaxRows: 8}
	var got []int64
	byteFull := false
	for {
		out.Reset()
		err := cursor.Next(t.Context(), &out)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(got)+out.Rows() < len(rows) && out.Rows() < 8 {
			byteFull = true
		}
		for row := range out.Rows() {
			lane := out.Lane(row)
			got = append(got, out.Columns[1].Int64[out.Columns[1].ValueIndex(lane)])
		}
	}
	want := []int64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("byte-bounded order = %v, want %v", got, want)
	}
	if !byteFull {
		t.Fatal("fixture did not fill the byte bound before row demand")
	}
	partitionSortReleased(t, c, dir)
}

func TestPartitionSortCapsCountEachPartition(t *testing.T) {
	c, dir := partitionSortEnvironment(t, 64<<10)
	keys := partitionSortKeys(t, c)
	var rows [][]model.Value
	for _, key := range []int64{3, 1, 2, 0} {
		for _, p := range []int{1, 0} {
			rows = append(rows, []model.Value{model.Int64Val(keys[p]), model.Int64Val(key), model.StringVal("cap")})
		}
	}
	input, _ := partitionSortInput(t, c, rows)
	cursor := partitionSortOpen(t, c, input)
	if err := c.charge(16); err != nil {
		t.Fatal(err)
	}
	cursor.capped, cursor.count, cursor.counts = true, 2, make([]uint64, 2)
	out := columnar.Batch{MaxRows: 8}
	var got [2][]int64
	for {
		out.Reset()
		err := cursor.Next(t.Context(), &out)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for row := range out.Rows() {
			lane := out.Lane(row)
			got[cursor.partition()] = append(got[cursor.partition()], out.Columns[1].Int64[out.Columns[1].ValueIndex(lane)])
		}
	}
	if want := [2][]int64{{0, 1}, {0, 1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("per-partition caps = %v, want %v", got, want)
	}
	partitionSortReleased(t, c, dir)
}

func TestPartitionSortFailuresCloseOwnedInput(t *testing.T) {
	for _, scenario := range []string{"metadata", "sort-quota", "unproved-cap", "cancel", "early-close"} {
		t.Run(scenario, func(t *testing.T) {
			c, dir := partitionSortEnvironment(t, 64<<10)
			keys := partitionSortKeys(t, c)
			input, source := partitionSortInput(t, c, [][]model.Value{{model.Int64Val(keys[0]), model.Int64Val(1), model.StringVal("x")}})
			node, slots := partitionSortNode()
			switch scenario {
			case "metadata":
				c.portal.env.Limits.MetadataBytes = 1
			case "sort-quota":
				c.portal.env.Sort.WorkBytes = 1
			case "unproved-cap":
				node.Algorithm = physical.TopK
				node.Limit = physical.LimitSpec{Scope: physical.PartitionLimit, Count: 1, Global: 3}
			}
			cursor, err := c.partitionSort(node, slots, partitionSortLayouts, input)
			if scenario == "cancel" || scenario == "early-close" {
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "cancel" {
					ctx, cancel := context.WithCancel(t.Context())
					cancel()
					err = cursor.Next(ctx, &columnar.Batch{})
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation = %v", err)
					}
				} else if err := cursor.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("accepted rejected construction")
			}
			if !source.closed || source.index != 0 {
				t.Fatalf("failure consumed or retained input: rows=%d closed=%v", source.index, source.closed)
			}
			partitionSortReleased(t, c, dir)
		})
	}
}

func TestPartitionSortJoinsAdmissionAndCleanupErrors(t *testing.T) {
	c, dir := partitionSortEnvironment(t, 64<<10)
	input, source := partitionSortInput(t, c, nil)
	closeErr := errors.New("source cleanup failed")
	source.closeErr = closeErr
	c.portal.env.Limits.MetadataBytes = 1
	node, slots := partitionSortNode()
	_, err := c.partitionSort(node, slots, partitionSortLayouts, input)
	if !errors.Is(err, columnar.ErrBatchBudget) || !errors.Is(err, closeErr) {
		t.Fatalf("admission lost an error cause: %v", err)
	}
	partitionSortReleased(t, c, dir)
}

func TestPartitionSortReleasesPendingSpillOnStop(t *testing.T) {
	for _, cancelPending := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "cancel"}[cancelPending], func(t *testing.T) {
			c, dir := partitionSortEnvironment(t, 64<<10)
			keys := partitionSortKeys(t, c)
			rows := make([][]model.Value, 17)
			for i := range rows {
				rows[i] = []model.Value{model.Int64Val(keys[i%2]), model.Int64Val(int64(16 - i)), model.StringVal(strings.Repeat("x", 1000))}
			}
			input, _ := partitionSortInput(t, c, rows)
			cursor := partitionSortOpen(t, c, input)
			out := columnar.Batch{MaxRows: 8}
			if err := cursor.Next(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			if c.portal.env.Query.Usage().PeakFiles == 0 {
				t.Fatal("fixture did not spill")
			}
			out.Reset()
			if cancelPending {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := cursor.Next(ctx, &out); !errors.Is(err, context.Canceled) {
					t.Fatalf("pending cancellation = %v", err)
				}
			} else if err := cursor.Close(); err != nil {
				t.Fatal(err)
			}
			partitionSortReleased(t, c, dir)
		})
	}
}

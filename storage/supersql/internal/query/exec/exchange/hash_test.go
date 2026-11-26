package exchange

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/zeebo/xxh3"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func hashOptions(t *testing.T, q *runtime.Query) HashOptions {
	t.Helper()
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	return HashOptions{Sources: transportOptions(q), Keys: []HashKey{{Column: 0}}, Encoder: encoder, Function: XXH3V1, Partitions: 4, MaxKeyBytes: 4096, WorkBytes: 4 << 20}
}
func openHash(t *testing.T, inputs []storage.BatchCursor, options HashOptions) *Repartition {
	t.Helper()
	cursor, err := NewHashRepartition(t.Context(), inputs, options)
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
func hashReleased(t *testing.T, q *runtime.Query) {
	t.Helper()
	usage := q.Usage()
	if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Reservations != 0 {
		t.Fatalf("retained exchange buffers: %+v", usage)
	}
}
func hashRows(t *testing.T, layouts []model.TypeLayout, rows [][]model.Value) *transportCursor {
	t.Helper()
	builder, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: 8, MaxBytes: 16384})
	if err != nil {
		t.Fatal(err)
	}
	index := 0
	return &transportCursor{cleanup: builder.Close, next: func(ctx context.Context, out *columnar.Batch) error {
		out.Reset()
		builder.Reset()
		if index == len(rows) {
			return io.EOF
		}
		count := min(8, len(rows)-index)
		if out.MaxRows > 0 {
			count = min(count, out.MaxRows)
		}
		for range count {
			if err := builder.Append(ctx, rows[index]); err != nil {
				return err
			}
			index++
		}
		return builder.Publish(out)
	}}
}

func TestHashRepartitionPreservesBagAndCoLocation(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	first := make([]int64, 200)
	second := make([]int64, len(first))
	for i := range first {
		first[i], second[i] = int64(i), int64(len(first)-1-i)
	}
	options := hashOptions(t, q)
	expected := make(map[int64]uint32, len(first))
	layout := []storage.KeyColumn{{ColumnID: 1, Type: transportInt.Type, NullsFirst: true}}
	for _, value := range first {
		key, err := options.Encoder.EncodePrefix(t.Context(), layout, []model.Value{model.Int64Val(value)}, 128)
		if err != nil {
			t.Fatal(err)
		}
		expected[value] = uint32(xxh3.Hash(key) % uint64(options.Partitions))
	}
	options.Sources.ParallelEligible, options.Sources.EstimatedRows = true, 65536
	left, right := transportInts(first...), transportInts(second...)
	cursor := openHash(t, []storage.BatchCursor{left, right}, options)
	options.Keys[0].Column = 99
	counts, partitions := make(map[int64]int), make(map[int64]uint32)
	out := PartitionBatch{Batch: columnar.Batch{MaxRows: 8}}
	for {
		err := cursor.Next(t.Context(), &out)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if out.Partition >= 4 || out.Batch.Rows() == 0 || out.Batch.Rows() > out.Batch.MaxRows {
			t.Fatalf("invalid partition batch: %+v", out)
		}
		for row := range out.Batch.Rows() {
			value := transportValue(t, &out.Batch, row)
			if out.Partition != expected[value] {
				t.Fatalf("XXH3V1 key %d partition=%d, want %d", value, out.Partition, expected[value])
			}
			if partition, exists := partitions[value]; exists && partition != out.Partition {
				t.Fatalf("key %d routed to %d and %d", value, partition, out.Partition)
			}
			partitions[value] = out.Partition
			counts[value]++
		}
		out.Batch.MaxRows = 1
	}
	for _, value := range first {
		if counts[value] != 2 {
			t.Fatalf("key %d multiplicity %d", value, counts[value])
		}
	}
	if len(counts) != len(first) || left.closes.Load() != 1 || right.closes.Load() != 1 {
		t.Fatal("lost rows or unfinished inputs")
	}
	hashReleased(t, q)
}

func TestHashRepartitionCanonicalNumericClasses(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	numeric := model.TypeLayout{Type: model.Type{ID: 1700, Storage: model.TypeNumeric}}
	options := hashOptions(t, q)
	options.Sources.Layouts = []model.TypeLayout{numeric, transportInt}
	values := []model.Value{
		{Type: model.TypeNumeric, StrVal: "1.00"}, {Type: model.TypeNumeric, StrVal: "1e0"},
		{Type: model.TypeNumeric, StrVal: "-0"}, {Type: model.TypeNumeric, StrVal: "0.000"},
		{Type: model.TypeNumeric, StrVal: "NaN"}, {Type: model.TypeNumeric, StrVal: "nan"},
		model.NullVal(model.TypeNumeric), model.NullVal(model.TypeNumeric),
	}
	rows := make([][]model.Value, len(values))
	for i, value := range values {
		rows[i] = []model.Value{value, model.Int64Val(int64(i))}
	}
	input := hashRows(t, options.Sources.Layouts, rows)
	cursor := openHash(t, []storage.BatchCursor{input}, options)
	var partitions [8]uint32
	var seen [8]bool
	out := PartitionBatch{Batch: columnar.Batch{MaxRows: 3}}
	for {
		err := cursor.Next(t.Context(), &out)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for row := range out.Batch.Rows() {
			lane := out.Batch.Lane(row)
			id := out.Batch.Columns[1].Int64[out.Batch.Columns[1].ValueIndex(lane)]
			if id < 0 || id >= 8 || seen[id] {
				t.Fatalf("repeated/unknown row %d", id)
			}
			seen[id], partitions[id] = true, out.Partition
		}
	}
	for i := range seen {
		if !seen[i] {
			t.Fatalf("missing numeric row %d", i)
		}
	}
	for i := 0; i < 8; i += 2 {
		if partitions[i] != partitions[i+1] {
			t.Fatalf("equivalent numeric rows %d/%d split: %v", i, i+1, partitions)
		}
	}
	hashReleased(t, q)
}

func TestHashRepartitionRetainsNestedPayloads(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	nested := model.TypeLayout{Type: model.Type{ID: 9000, Storage: model.TypeComposite}, Children: []model.TypeLayout{transportInt}}
	layouts := []model.TypeLayout{transportInt, nested}
	rows := make([][]model.Value, 8)
	for i := range rows {
		rows[i] = []model.Value{model.Int64Val(int64(i)), {Type: model.TypeComposite, ArrayVal: []model.Value{model.Int64Val(int64(i + 100))}}}
	}
	options := hashOptions(t, q)
	options.Sources.Layouts = layouts
	cursor := openHash(t, []storage.BatchCursor{hashRows(t, layouts, rows)}, options)
	layouts[1].Children[0] = model.TypeLayout{}
	seen := make(map[int64]model.Value)
	out := PartitionBatch{Batch: columnar.Batch{MaxRows: 8}}
	for {
		err := cursor.Next(t.Context(), &out)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for row := range out.Batch.Rows() {
			id := transportValue(t, &out.Batch, row)
			value, _, err := columnar.CopyValue(t.Context(), &out.Batch.Columns[1], out.Batch.Lane(row), 4096)
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := seen[id]; exists {
				t.Fatalf("duplicate nested row %d", id)
			}
			seen[id] = value
		}
		out.Batch.MaxRows = 1
	}
	for i, row := range rows {
		if !reflect.DeepEqual(seen[int64(i)], row[1]) {
			t.Fatalf("nested payload %d changed: %#v", i, seen[int64(i)])
		}
	}
	hashReleased(t, q)
}

func TestHashRepartitionEarlyCloseAndCallContext(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	left, right := transportInts(1, 2, 3, 4), transportInts(5, 6)
	cursor := openHash(t, []storage.BatchCursor{left, right}, hashOptions(t, q))
	out := PartitionBatch{Batch: columnar.Batch{MaxRows: 1}}
	call, cancel := context.WithCancel(t.Context())
	err := cursor.Next(call, &out)
	cancel()
	if err != nil || out.Batch.Rows() != 1 {
		t.Fatalf("first demand: rows=%d error=%v", out.Batch.Rows(), err)
	}
	if err := cursor.Next(t.Context(), &out); err != nil || transportValue(t, &out.Batch, 0) != 2 {
		t.Fatalf("expired prior call invalidated stream: %v", err)
	}
	if left.calls.Load() != 2 || right.calls.Load() != 0 {
		t.Fatal("read beyond serial demand")
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if left.closes.Load() != 1 || right.closes.Load() != 1 {
		t.Fatal("early close leaked inputs")
	}
	hashReleased(t, q)
}

func TestHashRepartitionAdmissionClosesInputs(t *testing.T) {
	for _, sample := range []struct {
		name      string
		configure func(*HashOptions)
		want      error
	}{
		{"unknown_function", func(o *HashOptions) { o.Function = 2 }, runtime.ErrInvalidConfig},
		{"zero_partitions", func(o *HashOptions) { o.Partitions = 0 }, runtime.ErrInvalidConfig},
		{"column", func(o *HashOptions) { o.Keys[0].Column = 1 }, storage.ErrInvalidScanPlan},
		{"workspace", func(o *HashOptions) { o.WorkBytes = 1 }, runtime.ErrBudget},
	} {
		t.Run(sample.name, func(t *testing.T) {
			q, _ := transportRuntime(t, runtime.QueryConfig{})
			input := transportInts(1)
			options := hashOptions(t, q)
			sample.configure(&options)
			cursor, err := NewHashRepartition(t.Context(), []storage.BatchCursor{input}, options)
			if cursor != nil {
				err = errors.Join(err, cursor.Close())
			}
			if !errors.Is(err, sample.want) {
				t.Fatalf("admission: %v, want %v", err, sample.want)
			}
			if input.calls.Load() != 0 || input.closes.Load() != 1 {
				t.Fatal("failed construction consumed or leaked input")
			}
			hashReleased(t, q)
		})
	}
}

var errHashRead = errors.New("hash input failure")
var errHashCleanup = errors.New("hash input cleanup failure")

func TestHashRepartitionFailureRetainsCleanupCauses(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	input := &transportCursor{next: func(context.Context, *columnar.Batch) error { return errHashRead }, cleanup: func() error { return errHashCleanup }}
	cursor, err := NewHashRepartition(t.Context(), []storage.BatchCursor{input}, hashOptions(t, q))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); !errors.Is(err, errHashCleanup) {
			t.Errorf("cached cleanup cause: %v", err)
		}
	})
	err = cursor.Next(t.Context(), &PartitionBatch{})
	if !errors.Is(err, errHashRead) || !errors.Is(err, errHashCleanup) {
		t.Fatalf("lost error causes: %v", err)
	}
	if input.closes.Load() != 1 {
		t.Fatal("input cleanup count changed")
	}
	hashReleased(t, q)
}

func TestHashRepartitionKeyQuotaReleasesInputs(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	input := transportInts(42)
	options := hashOptions(t, q)
	options.MaxKeyBytes = 1
	cursor, err := NewHashRepartition(t.Context(), []storage.BatchCursor{input}, options)
	if err != nil {
		t.Fatal(err)
	}
	err = cursor.Next(t.Context(), &PartitionBatch{})
	if !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("encoded key quota: %v", err)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if input.closes.Load() != 1 {
		t.Fatal("key failure leaked input")
	}
	hashReleased(t, q)
}

func TestHashRepartitionCancellationReleasesPendingPartitions(t *testing.T) {
	q, _ := transportRuntime(t, runtime.QueryConfig{})
	input := transportInts(1, 2, 3, 4, 5, 6, 7, 8)
	cursor := openHash(t, []storage.BatchCursor{input}, hashOptions(t, q))
	out := PartitionBatch{Batch: columnar.Batch{MaxRows: 8}}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := cursor.Next(ctx, &out); !errors.Is(err, context.Canceled) {
		t.Fatalf("pending partition cancellation: %v", err)
	}
	if input.calls.Load() != 1 || input.closes.Load() != 1 {
		t.Fatal("cancellation pulled more input or missed cleanup")
	}
	hashReleased(t, q)
}

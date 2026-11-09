package groupingset_test

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	"github.com/gosuda/supersql-alpha/internal/query/exec/groupingset"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

var int4 = model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64}}

type source struct {
	builder *columnar.Builder
	pulls   int
	closed  bool
}

func (s *source) Next(ctx context.Context, out *columnar.Batch) error {
	s.pulls++
	if s.pulls > 1 {
		return io.EOF
	}
	if err := s.builder.Append(ctx, []model.Value{model.NullVal(model.TypeInt64), model.Int64Val(10)}); err != nil {
		return err
	}
	return s.builder.Publish(out)
}

func (s *source) Close() error {
	s.closed = true
	return s.builder.Close()
}

func setup(t *testing.T) (groupingset.Options, *source, string) {
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
	budget, err := runtime.NewBudget(192<<20, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	q, err := runtime.NewQuery(t.Context(), pool, budget, runtime.OSFileSystem{}, runtime.QueryConfig{WorkBytes: 96 << 20, OutputBytes: 16 << 20, SpillMode: runtime.SpillEnabled, SpillBytes: 64 << 20, Directory: dir, MaxFiles: 24, MaxReaders: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := q.Close(); err != nil {
			t.Error(err)
		}
	})
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	options := groupingset.Options{
		Aggregate: aggregate.Options{InputLayouts: []model.TypeLayout{int4, int4}, OutputLayouts: []model.TypeLayout{int4, int4, int4}, GroupColumns: []int{0, 1}, Encoder: encoder, Query: q, BatchRows: 1, BatchBytes: 1 << 20, MaxRecordBytes: 1024, GroupBytes: 1 << 20, PartitionMemoryBytes: 4096, WorkBytes: 16 << 20, MaxPartitions: 16},
		Sets:      [][]uint32{{1, 0}, {1}, {}, {1}, {0}},
		Groupings: [][]uint32{{0, 1, 0}},
		Spool:     rowstore.Limits{MaxBytes: 1 << 20, MaxRows: 100, MaxRecordBytes: 4096, MetadataBytes: 4096, DecodeBytes: 4096, MaxReaders: 1},
	}
	builder, err := columnar.NewBuilder(options.Aggregate.InputLayouts, columnar.BuilderOptions{MaxRows: 1, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	input := &source{builder: builder}
	t.Cleanup(func() {
		if err := input.Close(); err != nil {
			t.Error(err)
		}
	})
	return options, input, dir
}

func released(t *testing.T, q *runtime.Query, dir string) {
	t.Helper()
	u := q.Usage()
	workRetained := u.Work.Live != 0 || u.Work.Reservations != 0
	outputRetained := u.Output.Live != 0 || u.Output.Reservations != 0
	spillRetained := u.Spill.Live != 0 || u.Spill.Reservations != 0 || u.Files != 0 || u.Readers != 0
	if workRetained || outputRetained || spillRetained {
		t.Errorf("retained grouping resources: %+v", u)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("retained grouping spill files: %v", files)
	}
}

func TestGroupingOwnsRepeatedArguments(t *testing.T) {
	options, input, dir := setup(t)
	options.Groupings = append(options.Groupings, make([]uint32, 31))
	options.Aggregate.OutputLayouts = append(options.Aggregate.OutputLayouts, int4)
	cursor, err := groupingset.New(t.Context(), input, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	options.Groupings[0][0] = 1
	options.Groupings[1][0] = 1
	options.Groupings[0] = []uint32{1}
	options.Sets[0][0] = 0
	options.Sets[1] = nil

	want := [][3]int64{{10, 0, 0}, {10, 5, 0x7fffffff}, {0, 7, 0x7fffffff}, {10, 5, 0x7fffffff}, {0, 2, 0}}
	var batch columnar.Batch
	for row, expected := range want {
		if err := cursor.Next(t.Context(), &batch); err != nil {
			t.Fatalf("row %d: %v", row, err)
		}
		if batch.Rows() != 1 || len(batch.Columns) != 4 {
			t.Fatalf("row %d: shape=%dx%d, want 1x4", row, batch.Rows(), len(batch.Columns))
		}
		for column := range batch.Columns {
			value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[column], batch.Lane(0), 4096)
			if err != nil {
				t.Fatal(err)
			}
			if column == 0 {
				if !value.IsNull {
					t.Errorf("row %d: first key=%v, want NULL", row, value)
				}
				continue
			}
			wantNull := column == 1 && (row == 2 || row == 4)
			wrongValue := !wantNull && value.IntVal != expected[column-1]
			if value.Type != model.TypeInt64 || value.IsNull != wantNull || wrongValue {
				t.Errorf("row %d column %d: value=%v, want %d (NULL=%t)", row, column, value, expected[column-1], wantNull)
			}
		}
	}
	if err := cursor.Next(t.Context(), &batch); err != io.EOF {
		t.Fatalf("after all sets: %v, want EOF", err)
	}
	released(t, options.Aggregate.Query, dir)
}

func TestGroupingRejectsInvalidMetadataBeforeInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*groupingset.Options)
		want   error
	}{
		{"empty arguments", func(o *groupingset.Options) { o.Groupings[0] = nil }, runtime.ErrInvalidConfig},
		{"too many arguments", func(o *groupingset.Options) { o.Groupings[0] = make([]uint32, 32) }, runtime.ErrInvalidConfig},
		{"unknown key", func(o *groupingset.Options) { o.Groupings[0] = []uint32{2} }, runtime.ErrInvalidConfig},
		{"missing result", func(o *groupingset.Options) { o.Aggregate.OutputLayouts = o.Aggregate.OutputLayouts[:2] }, runtime.ErrInvalidConfig},
		{"wrong SQL type", func(o *groupingset.Options) { o.Aggregate.OutputLayouts[2].Type.ID = 20 }, columnar.ErrValueLayout},
		{"wrong storage type", func(o *groupingset.Options) { o.Aggregate.OutputLayouts[2].Type.Storage = model.TypeFloat64 }, columnar.ErrValueLayout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options, input, dir := setup(t)
			tc.change(&options)
			cursor, err := groupingset.New(t.Context(), input, options)
			if cursor != nil {
				if err := cursor.Close(); err != nil {
					t.Error(err)
				}
				t.Fatal("invalid GROUPING metadata returned a cursor")
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("New error=%v, want %v", err, tc.want)
			}
			if input.pulls != 0 || !input.closed {
				t.Errorf("rejected input pulls=%d, closed=%t", input.pulls, input.closed)
			}
			released(t, options.Aggregate.Query, dir)
		})
	}
}

func TestGroupingReleasesActiveMasks(t *testing.T) {
	for _, cancelNext := range []bool{false, true} {
		name := "close"
		if cancelNext {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			options, input, dir := setup(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cursor, err := groupingset.New(ctx, input, options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cursor.Close(); err != nil {
					t.Error(err)
				}
			})
			var batch columnar.Batch
			if err := cursor.Next(ctx, &batch); err != nil {
				t.Fatal(err)
			}
			if u := options.Aggregate.Query.Usage(); u.Spill.Live == 0 || u.Files == 0 {
				t.Fatalf("replay is not active: %+v", u)
			}
			if cancelNext {
				cancel()
				if err := cursor.Next(ctx, &batch); !errors.Is(err, context.Canceled) {
					t.Errorf("Next error=%v, want cancellation", err)
				}
			} else if err := cursor.Close(); err != nil {
				t.Fatal(err)
			}
			released(t, options.Aggregate.Query, dir)
		})
	}
}

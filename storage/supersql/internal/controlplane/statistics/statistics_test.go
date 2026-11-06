package statistics_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/statistics"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func limits() statistics.Limits {
	return statistics.Limits{MaxBytes: 1 << 20, MaxColumns: 32, MaxValues: 4096, MaxDepth: 32}
}
func target() storage.ObjectRef {
	return storage.ObjectRef{ObjectID: 77, Generation: 2, DescriptorVersion: 3, EncodingVersion: statistics.EncodingVersion}
}
func integerLayout() model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
}
func options() statistics.AnalyzeOptions {
	return statistics.AnalyzeOptions{Target: target(), Snapshot: 10, Epoch: 1, Seed: 42, Layout: []storage.ColumnLayout{{ID: 1, Type: integerLayout().Type, Nullable: true}}, KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: integerLayout().Type}}, SampleRows: 32, BatchRows: 7, MostCommon: 4, HistogramBins: 8, MaxScanRows: 10000, MaxBatches: 10000, MaxLiveBytes: 4 << 20, MaxReservoirBytes: 1 << 20, MaxKeyBytes: 4096, Limits: limits()}
}
func encoder(t *testing.T) *builtin.KeyEncoder {
	t.Helper()
	e, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

type scanReader struct {
	rows                             [][]model.Value
	snapshot                         model.Timestamp
	cursor                           *scanCursor
	planError, nextError, closeError error
	cancel                           context.CancelFunc
	closed                           bool
	row                              *model.Row
}

type columnExpression int

func (column columnExpression) Evaluate(ctx context.Context, values []model.Value, _ model.ExpressionRuntime) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	return values[column], nil
}

type positivePredicate struct{}

func (positivePredicate) Evaluate(ctx context.Context, values []model.Value, _ model.ExpressionRuntime) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	return model.BoolVal(!values[0].IsNull && values[0].IntVal > 0), nil
}

func (r *scanReader) Get(context.Context, storage.ReadRequest) (model.Row, error) {
	if r.row == nil {
		return model.Row{}, storage.ErrRowNotFound
	}
	return *r.row, nil
}
func (r *scanReader) PlanScan(_ context.Context, request storage.PushdownRequest) (storage.PushdownPlan, error) {
	return storage.PushdownPlan{Request: request, Projection: request.ColumnIDs, SnapshotTS: r.snapshot}, r.planError
}
func (r *scanReader) ReleasePlan(storage.PushdownPlan) error { return nil }
func (r *scanReader) VectorizedScan(_ context.Context, plan storage.PushdownPlan) (storage.BatchCursor, error) {
	layouts := make([]model.TypeLayout, len(plan.Request.Layout))
	for i, c := range plan.Request.Layout {
		layouts[i] = model.TypeLayout{Type: c.Type, Children: c.Children}
	}
	r.cursor = &scanCursor{reader: r, layouts: layouts}
	return r.cursor, nil
}
func (r *scanReader) Close() error { r.closed = true; return nil }

type scanCursor struct {
	reader   *scanReader
	layouts  []model.TypeLayout
	position int
	builder  *columnar.Builder
	closed   bool
}

func (c *scanCursor) Next(ctx context.Context, b *columnar.Batch) error {
	if c.builder != nil {
		if err := c.builder.Close(); err != nil {
			return err
		}
		c.builder = nil
	}
	if c.reader.cancel != nil {
		c.reader.cancel()
		return ctx.Err()
	}
	if c.reader.nextError != nil {
		return c.reader.nextError
	}
	if c.position == len(c.reader.rows) {
		return io.EOF
	}
	builder, err := columnar.NewBuilder(c.layouts, columnar.BuilderOptions{MaxRows: b.MaxRows, MaxBytes: 8 << 20})
	if err != nil {
		return err
	}
	c.builder = builder
	end := min(len(c.reader.rows), c.position+b.MaxRows)
	for _, row := range c.reader.rows[c.position:end] {
		if err := builder.Append(ctx, row); err != nil {
			return err
		}
	}
	c.position = end
	return builder.Publish(b)
}
func (c *scanCursor) Close() error {
	c.closed = true
	if c.builder != nil {
		err := c.builder.Close()
		c.builder = nil
		return errors.Join(err, c.reader.closeError)
	}
	return c.reader.closeError
}

func TestAnalyzeDeterministicReservoirAcrossBatchBoundaries(t *testing.T) {
	rows := make([][]model.Value, 1000)
	for i := range rows {
		rows[i] = []model.Value{model.Int64Val(int64(i))}
	}
	o := options()
	firstReader := &scanReader{rows: rows, snapshot: o.Snapshot}
	first, err := statistics.Analyze(t.Context(), firstReader, encoder(t), o)
	if err != nil {
		t.Fatal(err)
	}
	o.BatchRows = 31
	secondReader := &scanReader{rows: rows, snapshot: o.Snapshot}
	second, err := statistics.Analyze(t.Context(), secondReader, encoder(t), o)
	if err != nil {
		t.Fatal(err)
	}
	a, err := statistics.Encode(t.Context(), first, limits())
	if err != nil {
		t.Fatal(err)
	}
	b, err := statistics.Encode(t.Context(), second, limits())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("fixed seed and snapshot changed sample across batch boundaries")
	}
	if first.LiveRows != (statistics.Count{Value: 1000, Known: true}) || first.SampleRows != 32 || first.Seed != 42 || first.Snapshot != 10 {
		t.Fatalf("sample identity/counts: %+v", first)
	}
	if first.Columns[0].Correlation < .99 || first.Columns[0].Distinct != 1000 {
		t.Fatalf("ordered unique sample: %+v", first.Columns[0])
	}
	if !firstReader.cursor.closed || firstReader.closed || !secondReader.cursor.closed {
		t.Fatal("cursor/borrowed reader lifetime violated")
	}
}

func TestAnalyzeEmptyDistinguishesUnknownStorageCounters(t *testing.T) {
	o := options()
	result, err := statistics.Analyze(t.Context(), &scanReader{snapshot: o.Snapshot}, encoder(t), o)
	if err != nil {
		t.Fatal(err)
	}
	if result.LiveRows != (statistics.Count{Known: true}) || result.SampleRows != 0 || result.Columns[0].Distinct != 0 || result.Confidence != 1 {
		t.Fatalf("empty statistics: %+v", result)
	}
	for _, count := range []statistics.Count{result.DeadRows, result.HistoricalRows, result.Files, result.Blocks, result.DeltaBytes} {
		if count.Known || count.Value != 0 {
			t.Fatalf("unobserved storage count: %+v", count)
		}
	}
	if result.VersionChainWorkKnown {
		t.Fatal("unobserved version work reported as measured")
	}
	o.Observed = &statistics.ObservedCounters{Target: o.Target, Snapshot: o.Snapshot, DeadRows: statistics.Count{Known: true}, Blocks: statistics.Count{Known: true, Value: 9}, VersionChainWork: 2.5, VersionChainWorkKnown: true}
	result, err = statistics.Analyze(t.Context(), &scanReader{snapshot: o.Snapshot}, encoder(t), o)
	if err != nil {
		t.Fatal(err)
	}
	if !result.DeadRows.Known || result.Blocks.Value != 9 || !result.VersionChainWorkKnown || result.VersionChainWork != 2.5 {
		t.Fatalf("observed counters: %+v", result)
	}
	o.Observed.Snapshot++
	if _, err := statistics.Analyze(t.Context(), &scanReader{snapshot: o.Snapshot}, encoder(t), o); !errors.Is(err, statistics.ErrStale) {
		t.Fatalf("mismatched observation: %v", err)
	}
}

func TestAnalyzeSkewNullsAndNumericEquality(t *testing.T) {
	o := options()
	o.SampleRows = 10
	o.HistogramBins = 3
	o.Layout = []storage.ColumnLayout{{ID: 1, Type: model.Type{ID: 1700, Storage: model.TypeNumeric}, Nullable: true}}
	o.KeyLayout = []storage.KeyColumn{{ColumnID: 1, Type: o.Layout[0].Type}}
	rows := [][]model.Value{{{Type: model.TypeNumeric, StrVal: "1.00"}}, {{Type: model.TypeNumeric, StrVal: "1"}}, {{Type: model.TypeNumeric, StrVal: "1.0"}}, {{Type: model.TypeNumeric, StrVal: "9"}}, {model.NullVal(model.TypeNumeric)}}
	result, err := statistics.Analyze(t.Context(), &scanReader{snapshot: o.Snapshot, rows: rows}, encoder(t), o)
	if err != nil {
		t.Fatal(err)
	}
	c := result.Columns[0]
	if c.Distinct != 2 || c.NullFraction != .2 || c.MostCommon[0].Fraction != .6 || c.MostCommon[0].Value.StrVal != "1.00" || c.Histogram[len(c.Histogram)-1].StrVal != "9" {
		t.Fatalf("numeric skew: %+v", c)
	}
}

func TestAnalyzeColumnTargetControlsDetailStatistics(t *testing.T) {
	o := options()
	o.ColumnTargets = []statistics.ColumnTarget{{ID: 1, Target: 0}}
	rows := [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(1)}, {model.Int64Val(2)}}
	result, err := statistics.Analyze(t.Context(), &scanReader{snapshot: o.Snapshot, rows: rows}, encoder(t), o)
	if err != nil {
		t.Fatal(err)
	}
	if column := result.Columns[0]; column.Distinct != 2 || len(column.MostCommon) != 0 || len(column.Histogram) != 0 {
		t.Fatalf("zero target statistics: %+v", column)
	}
	o.ColumnTargets[0].Target = 1
	result, err = statistics.Analyze(t.Context(), &scanReader{snapshot: o.Snapshot, rows: rows}, encoder(t), o)
	if err != nil {
		t.Fatal(err)
	}
	if column := result.Columns[0]; len(column.MostCommon) != 1 || len(column.Histogram) != 1 {
		t.Fatalf("target one statistics: %+v", column)
	}
}

func TestAnalyzeExtendedStatisticsProduceCanonicalPayloadsAndHonorTarget(t *testing.T) {
	o := options()
	o.Layout = append(o.Layout, storage.ColumnLayout{ID: 2, Type: integerLayout().Type})
	o.Extended = []statistics.ExtendedDefinition{{
		OID:    9001,
		Keys:   []model.ColumnID{1},
		Kinds:  []byte{'d', 'e', 'f', 'm'},
		Target: 0, TargetSet: true,
		Expressions: []statistics.ExtendedExpression{{
			Expression: columnExpression(1), Layout: integerLayout(),
			EqualityOperator: 1, OrderingOperator: 2,
		}},
	}}
	rows := [][]model.Value{
		{model.Int64Val(1), model.Int64Val(10)},
		{model.Int64Val(1), model.Int64Val(10)},
		{model.Int64Val(2), model.Int64Val(20)},
	}
	result, err := statistics.Analyze(t.Context(), &scanReader{snapshot: o.Snapshot, rows: rows}, encoder(t), o)
	if err != nil {
		t.Fatal(err)
	}
	extended := result.Extended[0]
	if len(extended.NDistinct) == 0 || len(extended.Dependencies) == 0 || len(extended.MCV) == 0 {
		t.Fatalf("missing extended payload: %+v", extended)
	}
	list, err := statistics.DecodeMCVList(t.Context(), extended.MCV, limits())
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("zero target MCV items: %+v", list.Items)
	}
	record := extended.Expressions.ArrayVal[0].ArrayVal
	for _, field := range record[6:11] {
		if field.IntVal == 1 || field.IntVal == 2 {
			t.Fatalf("zero target expression detail kind: %d", field.IntVal)
		}
	}
	o.Extended[0].Target = 1
	result, err = statistics.Analyze(t.Context(), &scanReader{snapshot: o.Snapshot, rows: rows}, encoder(t), o)
	if err != nil {
		t.Fatal(err)
	}
	extended = result.Extended[0]
	list, err = statistics.DecodeMCVList(t.Context(), extended.MCV, limits())
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("target one MCV items: %+v", list.Items)
	}
	record = extended.Expressions.ArrayVal[0].ArrayVal
	if record[6].IntVal != 1 || record[7].IntVal != 2 {
		t.Fatalf("target one expression detail kinds: %d, %d", record[6].IntVal, record[7].IntVal)
	}
}

func TestAnalyzeBundleUsesSharedSampleAndPartialIndexPopulation(t *testing.T) {
	o := options()
	o.SampleRows, o.MostCommon, o.HistogramBins = 2, 2, 2
	o.Indexes = []statistics.IndexDefinition{{
		Target: storage.ObjectRef{ObjectID: 88, Generation: 3, DescriptorVersion: 4, EncodingVersion: statistics.EncodingVersion},
		Epoch:  7,
		Columns: []statistics.IndexColumn{{
			ID: 1, Expression: columnExpression(0), Layout: integerLayout(), Target: 0, TargetSet: true,
		}},
		Predicate: positivePredicate{},
	}}
	rows := [][]model.Value{{model.Int64Val(4)}, {model.Int64Val(-1)}, {model.Int64Val(4)}, {model.Int64Val(4)}}
	result, err := statistics.AnalyzeBundle(t.Context(), &scanReader{snapshot: o.Snapshot, rows: rows}, encoder(t), o)
	if err != nil {
		t.Fatal(err)
	}
	if result.Table.LiveRows.Value != 4 || len(result.Indexes) != 1 {
		t.Fatalf("bundle population: %+v", result)
	}
	index := result.Indexes[0]
	if index.Target != o.Indexes[0].Target || index.Epoch != 7 || index.LiveRows != (statistics.Count{Value: 3, Known: true}) || index.SampleRows == 0 || index.SampleRows > 2 {
		t.Fatalf("partial index identity or population: %+v", index)
	}
	column := index.Columns[0]
	if column.ID != 1 || column.Distinct != 1 || len(column.MostCommon) != 0 || len(column.Histogram) != 0 {
		t.Fatalf("zero-target index expression statistics: %+v", column)
	}
}

func TestAnalyzeRejectsReservoirAndLiveAllocationLimits(t *testing.T) {
	for _, scenario := range []string{"fixed reservoir", "variable reservoir", "live allocation", "scan work"} {
		t.Run(scenario, func(t *testing.T) {
			o := options()
			r := &scanReader{snapshot: o.Snapshot, rows: [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}}}
			switch scenario {
			case "fixed reservoir":
				o.MaxReservoirBytes = 1
			case "live allocation":
				o.MaxLiveBytes = 1
			case "scan work":
				o.MaxScanRows = 1
			case "variable reservoir":
				o.MaxReservoirBytes = 10000
				o.Layout[0].Type = model.Type{ID: 25, Storage: model.TypeString}
				o.KeyLayout[0].Type = o.Layout[0].Type
				r.rows = [][]model.Value{{model.StringVal(strings.Repeat("x", 20000))}}
			}
			result, err := statistics.Analyze(t.Context(), r, encoder(t), o)
			if err == nil || result != nil {
				t.Fatalf("%s returned success", scenario)
			}
			if r.cursor != nil && !r.cursor.closed {
				t.Fatal("failed scan leaked cursor")
			}
		})
	}
}

var errScan = errors.New("scan failure")
var errClose = errors.New("close failure")

// TestAnalyzeClampsReservoirForWideLayout pins that a table too wide for the
// requested SampleRows still samples: the effective sample shrinks to fit
// MaxReservoirBytes instead of failing with ErrLimit, while the other half of
// the reservoir stays available for variable-length payload.
func TestAnalyzeClampsReservoirForWideLayout(t *testing.T) {
	const columns = 12
	o := options()
	o.Layout = make([]storage.ColumnLayout, columns)
	for i := range o.Layout {
		o.Layout[i] = storage.ColumnLayout{ID: model.ColumnID(i + 1), Type: integerLayout().Type, Nullable: true}
	}
	o.KeyLayout = []storage.KeyColumn{{ColumnID: 1, Type: integerLayout().Type}}
	o.SampleRows, o.MostCommon, o.HistogramBins = 30000, 100, 100
	o.MaxLiveBytes, o.MaxReservoirBytes = 64<<20, 16<<20
	rows := make([][]model.Value, 3)
	for i := range rows {
		rows[i] = make([]model.Value, columns)
		for c := range rows[i] {
			rows[i][c] = model.Int64Val(int64(i*100 + c + 1))
		}
	}
	r := &scanReader{rows: rows, snapshot: o.Snapshot}
	table, err := statistics.Analyze(t.Context(), r, encoder(t), o)
	if err != nil {
		t.Fatalf("wide ANALYZE: %v", err)
	}
	if table.LiveRows != (statistics.Count{Value: 3, Known: true}) || table.SampleRows != 3 || len(table.Columns) != columns {
		t.Fatalf("wide sample: %+v", table)
	}
}

func TestAnalyzeCancellationAndErrorsCloseCursor(t *testing.T) {
	for _, scenario := range []string{"cancel", "next", "close", "plan", "snapshot"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := &scanReader{snapshot: 10}
			want := errScan
			switch scenario {
			case "cancel":
				r.cancel = cancel
				want = context.Canceled
			case "next":
				r.nextError = errScan
				r.closeError = errClose
			case "close":
				r.closeError = errClose
				want = errClose
			case "plan":
				r.planError = errScan
			case "snapshot":
				r.snapshot++
				want = statistics.ErrStale
			}
			result, err := statistics.Analyze(ctx, r, encoder(t), options())
			if result != nil || !errors.Is(err, want) {
				t.Fatalf("result=%v error=%v, want %v", result, err, want)
			}
			if r.cursor != nil && !r.cursor.closed {
				t.Fatal("cursor not closed")
			}
			if scenario == "next" && !errors.Is(err, errClose) {
				t.Fatal("close error discarded")
			}
		})
	}
}

func TestCodecPreservesTypedValuesAndOwnsDecodedPayloads(t *testing.T) {
	arrayLayout := model.TypeLayout{Type: model.Type{ID: 1009, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 25, Storage: model.TypeString}}}}
	cases := []struct {
		layout model.TypeLayout
		value  model.Value
	}{
		{integerLayout(), model.Int64Val(math.MaxInt64)},
		{model.TypeLayout{Type: model.Type{ID: 701, Storage: model.TypeFloat64}}, model.Float64Val(math.Float64frombits(0x7ff8000000001234))},
		{model.TypeLayout{Type: model.Type{ID: 1700, Storage: model.TypeNumeric}}, model.Value{Type: model.TypeNumeric, StrVal: "99999999999999999999999999.0000000001"}},
		{arrayLayout, model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -4}}, ArrayVal: []model.Value{model.StringVal("owned"), model.NullVal(model.TypeString)}}},
		{model.TypeLayout{Type: model.Type{ID: 1186, Storage: model.TypeInterval}}, model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Months: 3, Days: -2, Microseconds: 9}}},
	}
	table := &statistics.Table{Target: target(), Epoch: 4, Snapshot: 10, SampleRows: 1, LiveRows: statistics.Count{Value: 1, Known: true}, Columns: make([]statistics.Column, len(cases))}
	for i, c := range cases {
		table.Columns[i] = statistics.Column{ID: model.ColumnID(i + 1), Layout: c.layout, Distinct: 1, MostCommon: []statistics.Frequency{{Value: c.value, Fraction: 1}}, Histogram: []model.Value{c.value}}
	}
	encoded, err := statistics.Encode(t.Context(), table, limits())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := statistics.Decode(t.Context(), encoded, limits())
	if err != nil {
		t.Fatal(err)
	}
	again, err := statistics.Encode(t.Context(), decoded, limits())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, again) {
		t.Fatal("canonical typed roundtrip changed frame")
	}
	clear(encoded)
	if math.Float64bits(decoded.Columns[1].Histogram[0].FloatVal) != 0x7ff8000000001234 || decoded.Columns[2].Histogram[0].StrVal != cases[2].value.StrVal || decoded.Columns[3].Histogram[0].ArrayVal[0].StrVal != "owned" || decoded.Columns[3].Histogram[0].Dimensions[0].LowerBound != -4 {
		t.Fatal("typed or owned payload lost")
	}
	decoded.Columns[3].MostCommon[0].Value.ArrayVal[0].StrVal = "changed"
	if decoded.Columns[3].Histogram[0].ArrayVal[0].StrVal != "owned" {
		t.Fatal("MCV aliases histogram")
	}
}

func TestCodecRejectsCorruptionLimitsAndCancellation(t *testing.T) {
	table := &statistics.Table{Target: target(), Epoch: 1, Snapshot: 10}
	encoded, err := statistics.Encode(t.Context(), table, limits())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{encoded[:len(encoded)-1], append(bytes.Clone(encoded), 0), bytes.Repeat([]byte{255}, 4096)} {
		if _, err := statistics.Decode(t.Context(), bad, limits()); err == nil {
			t.Fatal("corrupt frame accepted")
		}
	}
	small := limits()
	small.MaxBytes = 1
	if _, err := statistics.Decode(t.Context(), encoded, small); !errors.Is(err, statistics.ErrLimit) {
		t.Fatalf("decode bound: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := statistics.Decode(ctx, encoded, limits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("decode cancellation: %v", err)
	}
	if _, err := statistics.Encode(ctx, table, limits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("encode cancellation: %v", err)
	}
	table.DeadRows.Value = 2
	if err := statistics.Validate(t.Context(), table, limits()); !errors.Is(err, statistics.ErrInvalid) {
		t.Fatalf("unknown nonzero counter: %v", err)
	}
}

func TestStoreRejectsStaleEpochGenerationAndFutureSnapshot(t *testing.T) {
	s, err := statistics.New(t.Context(), storage.ObjectRef{ObjectID: 999, Generation: 1, DescriptorVersion: 1, EncodingVersion: statistics.EncodingVersion}, encoder(t), limits(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	r := &scanReader{}
	view, err := s.Snapshot(r, 20)
	if err != nil {
		t.Fatal(err)
	}
	next := &statistics.Table{Target: target(), Snapshot: 10, Epoch: 2}
	change, err := s.Publish(t.Context(), view, next)
	if err != nil {
		t.Fatal(err)
	}
	r.row = &model.Row{CommitTS: 20, Values: change.Mutations[0].Values}
	if _, err := s.Publish(t.Context(), view, next); !errors.Is(err, statistics.ErrStale) {
		t.Fatalf("repeated epoch: %v", err)
	}
	next.Epoch = 3
	next.Snapshot = 9
	if _, err := s.Publish(t.Context(), view, next); !errors.Is(err, statistics.ErrStale) {
		t.Fatalf("older sample: %v", err)
	}
	wrong := target()
	wrong.Generation++
	if _, _, err := view.Lookup(t.Context(), wrong); err == nil {
		t.Fatal("mismatched generation accepted")
	}
	future, err := s.Snapshot(r, 19)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := future.Lookup(t.Context(), target()); !errors.Is(err, statistics.ErrStale) {
		t.Fatalf("future publication: %v", err)
	}
}

func TestOwnedBytesRejectsRetainedCapacityAndCyclicValues(t *testing.T) {
	table := &statistics.Table{Target: target(), Epoch: 1, Snapshot: 10}
	size, err := statistics.OwnedBytes(t.Context(), table, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	table.Columns = make([]statistics.Column, 0, 32)
	if _, err := statistics.OwnedBytes(t.Context(), table, size); !errors.Is(err, statistics.ErrLimit) {
		t.Fatalf("retained empty column capacity: %v", err)
	}
	value := model.Value{Type: model.TypeArray, ArrayVal: make([]model.Value, 1)}
	value.ArrayVal[0] = value
	table.Columns = []statistics.Column{{ID: 1, Layout: integerLayout(), Histogram: []model.Value{value}}}
	if _, err := statistics.OwnedBytes(t.Context(), table, 1<<20); !errors.Is(err, statistics.ErrLimit) {
		t.Fatalf("cyclic value: %v", err)
	}
}

func TestVersionWorkKnownZeroSurvivesCodec(t *testing.T) {
	table := &statistics.Table{Target: target(), Epoch: 1, Snapshot: 10, VersionChainWorkKnown: true}
	data, err := statistics.Encode(t.Context(), table, limits())
	if err != nil {
		t.Fatal(err)
	}
	got, err := statistics.Decode(t.Context(), data, limits())
	if err != nil {
		t.Fatal(err)
	}
	if !got.VersionChainWorkKnown || got.VersionChainWork != 0 {
		t.Fatal("measured zero became unknown")
	}
	table.VersionChainWorkKnown = false
	table.VersionChainWork = 1
	if err := statistics.Validate(t.Context(), table, limits()); !errors.Is(err, statistics.ErrInvalid) {
		t.Fatalf("unknown work with nonzero payload: %v", err)
	}
}

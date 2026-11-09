package neosepia

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// BenchmarkStoragePushdown measures the storage scan contract against the
// direct PAX cursor kernel over identical run files and snapshots. The fixture
// is built once per benchmark outside the timed region: two committed
// generations — a base run holding every row at timestamp 100 and a delta run
// rewriting every tenth row at timestamp 200. Column 2 carries a 20% null
// fraction; column 65 carries a 128-byte binary payload.
//
// Reported artifacts per sub-benchmark: ns/op, B/op, allocs/op, a row+value
// checksum, decoded rows, pruned blocks (ScanObserver), and rows/s.
// SUPERSQL_BENCH_ROWS overrides the default 1,048,576-row fixture for smoke
// runs; committed measurements must use the default.

const (
	benchColumns   = 64
	benchBlockRows = 4096
	benchPayload   = 128
	benchBaseTS    = model.Timestamp(100)
	benchDeltaTS   = model.Timestamp(200)
)

var (
	benchInt8Type  = model.Type{ID: 20, Storage: model.TypeInt64}
	benchBytesType = model.Type{ID: 17, Storage: model.TypeBytes}
	benchTarget    = storage.ObjectRef{ObjectID: 7, Generation: 3, DescriptorVersion: 5, EncodingVersion: 2}
)

type benchFixture struct {
	runs []*sstFile
	set  *runSet
	rows int
}

func benchRowCount(b *testing.B) int {
	if raw := os.Getenv("SUPERSQL_BENCH_ROWS"); raw != "" {
		if rows, err := strconv.Atoi(raw); err == nil && rows > 0 {
			return rows
		}
	}
	return 1 << 20
}

func benchLayout() []storage.ColumnLayout {
	layout := make([]storage.ColumnLayout, benchColumns+1)
	for i := 0; i < benchColumns; i++ {
		layout[i] = storage.ColumnLayout{ID: model.ColumnID(i + 1), Type: benchInt8Type, Nullable: true}
	}
	layout[benchColumns] = storage.ColumnLayout{ID: model.ColumnID(benchColumns + 1), Type: benchBytesType, Nullable: true}
	return layout
}

func benchProjection(ids ...model.ColumnID) []model.ColumnID { return ids }

func benchFullProjection() []model.ColumnID {
	out := make([]model.ColumnID, benchColumns+1)
	for i := range out {
		out[i] = model.ColumnID(i + 1)
	}
	return out
}

func benchKey(row int) []byte {
	key := []byte(fmt.Sprintf("row%09d", row))
	return key
}

// benchBlock encodes the given sorted row indices at commit timestamp ts.
// Column 1 holds the row index (negated for delta rows), column 2 is NULL
// every fifth row, remaining integer columns hold row*column, and the payload
// column holds a deterministic 128-byte image.
func benchBlock(tb testing.TB, rows []int, ts model.Timestamp, delta bool) []byte {
	tb.Helper()
	count := len(rows)
	var keyData []byte
	ranges := make([]columnar.Range, count)
	versions := blockVersions{
		CommitTS: make([]model.Timestamp, count),
		Ordinals: make([]uint64, count),
		Kinds:    make([]storage.MutationOp, count),
	}
	for i, row := range rows {
		key := benchKey(row)
		ranges[i] = columnar.Range{Offset: uint32(len(keyData)), Length: uint32(len(key))}
		keyData = append(keyData, key...)
	}
	versions.Keys = columnar.Vector{Type: model.Type{ID: 17, Storage: model.TypeBytes}, Encoding: columnar.Flat, Length: count, Data: keyData, Ranges: ranges}
	columns := make([]columnar.Vector, benchColumns+1)
	for c := 0; c < benchColumns; c++ {
		columns[c] = columnar.Vector{Type: benchInt8Type, Encoding: columnar.Flat, Length: count, Int64: make([]int64, count)}
	}
	var nulls []byte
	for i, row := range rows {
		versions.CommitTS[i] = ts
		versions.Ordinals[i] = uint64(row) + 1
		versions.Kinds[i] = storage.MutationUpsert
		for c := 0; c < benchColumns; c++ {
			columns[c].Int64[i] = int64(row)*int64(c+1) + int64(c)
		}
		if delta {
			columns[0].Int64[i] = -int64(row)
		}
		if row%5 == 0 {
			if nulls == nil {
				nulls = make([]byte, (count+7)/8)
			}
			nulls[i/8] |= 1 << uint(i%8)
		}
	}
	columns[1].Nulls = nulls
	payload := make([]byte, count*benchPayload)
	payloadRanges := make([]columnar.Range, count)
	for i, row := range rows {
		payloadRanges[i] = columnar.Range{Offset: uint32(i * benchPayload), Length: benchPayload}
		for j := 0; j < 8; j++ {
			payload[i*benchPayload+j] = byte(row >> (j * 8))
		}
	}
	columns[benchColumns] = columnar.Vector{Type: benchBytesType, Encoding: columnar.Flat, Length: count, Data: payload, Ranges: payloadRanges}
	batch := &columnar.Batch{Length: count, Columns: columns}
	columnIDs := make([]model.ColumnID, benchColumns+1)
	for i := range columnIDs {
		columnIDs[i] = model.ColumnID(i + 1)
	}
	limits := blockLimits{MaxRows: benchBlockRows, MaxBytes: 1 << 28, MaxDecodedBytes: 1 << 28}
	data, err := encodeBlock(context.Background(), benchTarget, versions, batch, columnIDs, limits)
	if err != nil {
		tb.Fatal(err)
	}
	return data
}

func benchWriteRun(tb testing.TB, blocks [][]byte) string {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "run.sst")
	file, err := os.Create(path)
	if err != nil {
		tb.Fatal(err)
	}
	limits := sstLimits{Block: blockLimits{MaxRows: benchBlockRows, MaxBytes: 1 << 28, MaxDecodedBytes: 1 << 28}, MaxBytes: 1 << 33, MaxIndexBytes: 1 << 28, MaxBlocks: 1 << 20}
	writer, err := newSSTWriter(file, benchTarget, limits)
	if err != nil {
		tb.Fatal(err)
	}
	for _, block := range blocks {
		if err := writer.Append(context.Background(), block); err != nil {
			tb.Fatal(err)
		}
	}
	if err := writer.Finish(context.Background()); err != nil {
		tb.Fatal(err)
	}
	if err := file.Close(); err != nil {
		tb.Fatal(err)
	}
	return path
}

func benchOpenRun(tb testing.TB, path string) *sstFile {
	tb.Helper()
	file, err := os.Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	run, err := openSST(context.Background(), file, sstLimits{Block: blockLimits{MaxRows: benchBlockRows, MaxBytes: 1 << 28, MaxDecodedBytes: 1 << 28}, MaxBytes: 1 << 33, MaxIndexBytes: 1 << 28, MaxBlocks: 1 << 20})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := run.Close(); err != nil {
			tb.Error(err)
		}
	})
	return run
}

func newBenchFixture(b *testing.B) *benchFixture {
	b.Helper()
	rows := benchRowCount(b)
	var baseBlocks, deltaBlocks [][]byte
	for first := 0; first < rows; first += benchBlockRows {
		end := first + benchBlockRows
		if end > rows {
			end = rows
		}
		base := make([]int, 0, end-first)
		delta := make([]int, 0, (end-first)/10+1)
		for row := first; row < end; row++ {
			base = append(base, row)
			if row%10 == 0 {
				delta = append(delta, row)
			}
		}
		baseBlocks = append(baseBlocks, benchBlock(b, base, benchBaseTS, false))
		if len(delta) > 0 {
			deltaBlocks = append(deltaBlocks, benchBlock(b, delta, benchDeltaTS, true))
		}
	}
	base := benchOpenRun(b, benchWriteRun(b, baseBlocks))
	delta := benchOpenRun(b, benchWriteRun(b, deltaBlocks))
	set, err := newRunSet(runSetLimits{MaxBytes: 1 << 32, MaxFiles: 16, MaxPins: 8})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := set.Close(); err != nil {
			b.Error(err)
		}
	})
	ctx := context.Background()
	if err := set.publish(ctx, 1, benchBaseTS, []*sstFile{base}); err != nil {
		b.Fatal(err)
	}
	if err := set.publish(ctx, 2, benchDeltaTS, []*sstFile{delta}); err != nil {
		b.Fatal(err)
	}
	return &benchFixture{runs: []*sstFile{base, delta}, set: set, rows: rows}
}

func benchSpec(projection []model.ColumnID, program *predicateProgram) paxScanSpec {
	return paxScanSpec{
		Target:     benchTarget,
		Layout:     benchLayout(),
		Projection: projection,
		Spans:      []encodedSpan{{lower: keyCut{infinity: -1}, upper: keyCut{infinity: 1}}},
		Predicate:  program,
		MaxRows:    2048, MaxBytes: 1 << 22, PredicateBytes: 1 << 20,
	}
}

// benchPredicate compiles `column < const` over int8 column 1. The threshold
// selects the row-density fraction named by the sub-benchmark.
func benchPredicate(tb testing.TB, threshold int64) (*predicateProgram, storage.Predicate) {
	tb.Helper()
	nodes := []storage.PredicateNode{
		{Kind: storage.PredicateColumn, ColumnID: 1, Type: benchInt8Type},
		{Kind: storage.PredicateConstant, Type: benchInt8Type, ConstantIndex: 0},
		{Kind: storage.PredicateComparison, Type: model.Type{ID: 16, Storage: model.TypeBoolean}, Operator: 412, Left: 0, Right: 1},
	}
	pred, err := storage.NewPredicate(nodes, nil, []model.Value{model.Int64Val(threshold)})
	if err != nil {
		tb.Fatal(err)
	}
	program, accepted, err := compilePredicate(context.Background(), pred, benchLayout(), 4096, 1<<24)
	if err != nil {
		tb.Fatal(err)
	}
	if !accepted {
		tb.Fatal("int8 less-than predicate was not executable")
	}
	return program, pred
}

// drainScan consumes a batch cursor and returns a row/value checksum over the
// first projected column plus the total row count.
func drainScan(b *testing.B, ctx context.Context, cursor storage.BatchCursor) (uint64, int) {
	b.Helper()
	out := columnar.Batch{MaxRows: 2048}
	var sum uint64
	var rows int
	for {
		err := cursor.Next(ctx, &out)
		if err == io.EOF {
			return sum, rows
		}
		if err != nil {
			b.Fatal(err)
		}
		rows += out.Rows()
		if len(out.Columns) == 0 {
			continue
		}
		column := &out.Columns[0]
		for row := 0; row < out.Rows(); row++ {
			lane := out.Lane(row)
			if column.IsNull(lane) {
				continue
			}
			sum += uint64(column.Int64[lane])
		}
	}
}

type benchObserver struct{ blocks uint64 }

func (o *benchObserver) ObserveScan(obs storage.ScanObservation) {
	if obs.BlocksPruned.Known {
		o.blocks += obs.BlocksPruned.Value
	}
}

// benchDirect scans the fixture through the raw run/MVCC/PAX cursor stack —
// the same kernel Backend.VectorizedScan drives, minus the contract layer.
func (f *benchFixture) benchDirect(b *testing.B, ts model.Timestamp, spec paxScanSpec) (uint64, int) {
	b.Helper()
	ctx := context.Background()
	sources := make([]*runVersionCursor, len(f.runs))
	for i, run := range f.runs {
		source, err := newRunVersionCursor(run, 0, run.count, 1<<30, nil)
		if err != nil {
			b.Fatal(err)
		}
		sources[i] = source
	}
	merge, err := newMVCCCursor(ctx, sources, ts, mergeLimits{MaxBytes: 1 << 20, MaxKeyBytes: 64}, storage.ScanForward)
	if err != nil {
		b.Fatal(err)
	}
	cursor, err := newPAXScanCursor(ctx, merge, spec)
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := cursor.Close(); err != nil {
			b.Error(err)
		}
	}()
	return drainScan(b, ctx, cursor)
}

// benchContract scans the same fixture through the exported SnapshotReader
// contract: PlanScan then VectorizedScan on a pinned run set.
func (f *benchFixture) benchContract(b *testing.B, ts model.Timestamp, request storage.PushdownRequest) (uint64, int, uint64) {
	b.Helper()
	ctx := context.Background()
	pin, err := f.set.pin(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := pin.Close(); err != nil {
			b.Error(err)
		}
	}()
	reader, err := newSnapshotReader(ctx, pin, storage.SnapshotSpec{Timestamp: ts, Fence: storage.ReadFence{ID: [16]byte{1}, ClosedTS: ts, LogPosition: 2}}, nil, benchReadLimits())
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			b.Error(err)
		}
	}()
	observer := &benchObserver{}
	plan, err := reader.PlanScan(storage.WithScanObserver(ctx, observer), request)
	if err != nil {
		b.Fatal(err)
	}
	cursor, err := reader.VectorizedScan(storage.WithScanObserver(ctx, observer), plan)
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := cursor.Close(); err != nil {
			b.Error(err)
		}
	}()
	sum, rows := drainScan(b, ctx, cursor)
	return sum, rows, observer.blocks
}

func benchReadLimits() readLimits {
	return readLimits{
		SourceBytes: 1 << 30, BlockBytes: 1 << 24, MergeBytes: 1 << 22, KeyBytes: 4096,
		ScanBytes: 1 << 24, RowBytes: 1 << 24, PredicateBytes: 1 << 20,
		OverlayBytes: 64 << 10, OverlayBlockBytes: 1 << 20, PlanBytes: 8 << 20,
		MaxRows: 2048, MaxNodes: 128, MaxPlans: 16,
	}
}

func benchRequest(projection []model.ColumnID, pred storage.Predicate) storage.PushdownRequest {
	return storage.PushdownRequest{
		Target:    benchTarget,
		Layout:    benchLayout(),
		ColumnIDs: projection,
		Spans:     []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
		Predicate: pred,
	}
}

func BenchmarkStoragePushdown(b *testing.B) {
	fixture := newBenchFixture(b)
	ctx := context.Background()
	b.ReportAllocs()

	narrow := benchProjection(1, 2)
	full := benchFullProjection()
	selectiveProg, selectivePred := benchPredicate(b, int64(fixture.rows/100))
	nonselectiveProg, nonselectivePred := benchPredicate(b, int64(fixture.rows*9/10))
	currentTS := model.Timestamp(300)
	historicalTS := model.Timestamp(150)

	report := func(b *testing.B, rows int, pruned uint64, checksum uint64) {
		b.ReportMetric(float64(rows)/b.Elapsed().Seconds()*float64(b.N), "rows/s")
		b.ReportMetric(float64(pruned), "blocks_pruned")
		b.ReportMetric(float64(checksum), "checksum")
	}

	b.Run("direct/projection=narrow", func(b *testing.B) {
		b.ReportAllocs()
		var checksum uint64
		var rows int
		for i := 0; i < b.N; i++ {
			checksum, rows = fixture.benchDirect(b, currentTS, benchSpec(narrow, nil))
		}
		report(b, rows, 0, checksum)
	})
	b.Run("direct/projection=full", func(b *testing.B) {
		b.ReportAllocs()
		var checksum uint64
		var rows int
		for i := 0; i < b.N; i++ {
			checksum, rows = fixture.benchDirect(b, currentTS, benchSpec(full, nil))
		}
		report(b, rows, 0, checksum)
	})
	b.Run("contract/projection=narrow", func(b *testing.B) {
		b.ReportAllocs()
		var checksum uint64
		var rows int
		var pruned uint64
		for i := 0; i < b.N; i++ {
			checksum, rows, pruned = fixture.benchContract(b, currentTS, benchRequest(narrow, storage.Predicate{}))
		}
		report(b, rows, pruned, checksum)
	})
	b.Run("contract/projection=full", func(b *testing.B) {
		b.ReportAllocs()
		var checksum uint64
		var rows int
		var pruned uint64
		for i := 0; i < b.N; i++ {
			checksum, rows, pruned = fixture.benchContract(b, currentTS, benchRequest(full, storage.Predicate{}))
		}
		report(b, rows, pruned, checksum)
	})
	b.Run("contract/predicate=selective", func(b *testing.B) {
		b.ReportAllocs()
		var checksum uint64
		var rows int
		var pruned uint64
		for i := 0; i < b.N; i++ {
			checksum, rows, pruned = fixture.benchContract(b, currentTS, benchRequest(narrow, selectivePred))
		}
		report(b, rows, pruned, checksum)
	})
	b.Run("contract/predicate=nonselective", func(b *testing.B) {
		b.ReportAllocs()
		var checksum uint64
		var rows int
		var pruned uint64
		for i := 0; i < b.N; i++ {
			checksum, rows, pruned = fixture.benchContract(b, currentTS, benchRequest(narrow, nonselectivePred))
		}
		report(b, rows, pruned, checksum)
	})
	b.Run("direct/snapshot=historical", func(b *testing.B) {
		b.ReportAllocs()
		var checksum uint64
		var rows int
		for i := 0; i < b.N; i++ {
			checksum, rows = fixture.benchDirect(b, historicalTS, benchSpec(narrow, nil))
		}
		report(b, rows, 0, checksum)
	})
	b.Run("contract/snapshot=historical", func(b *testing.B) {
		b.ReportAllocs()
		var checksum uint64
		var rows int
		var pruned uint64
		for i := 0; i < b.N; i++ {
			checksum, rows, pruned = fixture.benchContract(b, historicalTS, benchRequest(narrow, storage.Predicate{}))
		}
		report(b, rows, pruned, checksum)
	})
	b.Run("direct/predicate=selective", func(b *testing.B) {
		b.ReportAllocs()
		var checksum uint64
		var rows int
		for i := 0; i < b.N; i++ {
			checksum, rows = fixture.benchDirect(b, currentTS, benchSpec(narrow, selectiveProg))
		}
		report(b, rows, 0, checksum)
	})

	// Mixed workload: an analytical scan interleaved with point reads through
	// the same pinned snapshot — the sharing surface a live backend sees.
	b.Run("contract/mixed=scan+point", func(b *testing.B) {
		b.ReportAllocs()
		pin, err := fixture.set.pin(ctx)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() {
			if err := pin.Close(); err != nil {
				b.Error(err)
			}
		})
		reader, err := newSnapshotReader(ctx, pin, storage.SnapshotSpec{Timestamp: currentTS, Fence: storage.ReadFence{ID: [16]byte{1}, ClosedTS: currentTS, LogPosition: 2}}, nil, benchReadLimits())
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() {
			if err := reader.Close(); err != nil {
				b.Error(err)
			}
		})
		plan, err := reader.PlanScan(ctx, benchRequest(narrow, selectivePred))
		if err != nil {
			b.Fatal(err)
		}
		pointLimits := pinnedPointLimits{SourceBytes: 3 << 20, BlockBytes: 1 << 22, Merge: mergeLimits{MaxBytes: 1 << 20, MaxKeyBytes: 64}, Point: pointReadLimits{ScanBytes: 1 << 20, RowBytes: 1 << 20}}
		layout := benchLayout()
		var checksum uint64
		var points int
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			cursor, err := reader.VectorizedScan(ctx, plan)
			if err != nil {
				b.Fatal(err)
			}
			sum, _ := drainScan(b, ctx, cursor)
			if err := cursor.Close(); err != nil {
				b.Fatal(err)
			}
			checksum += sum
			// One point read per 4,096 scanned rows.
			for row := 0; row < fixture.rows; row += benchBlockRows {
				request := storage.ReadRequest{Target: benchTarget, Key: benchKey(row), Layout: layout, ColumnIDs: narrow}
				got, err := pin.get(ctx, currentTS, nil, request, pointLimits)
				if err != nil {
					b.Fatal(err)
				}
				checksum += uint64(got.Values[0].IntVal)
				points++
			}
		}
		report(b, points, 0, checksum)
	})

	// Net heap retention across a full scan+close must be flat — no
	// scan-size-proportional output may be retained by the cursor contract.
	b.Run("retained=scan+close", func(b *testing.B) {
		b.ReportAllocs()
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		var checksum uint64
		for i := 0; i < b.N; i++ {
			checksum, _, _ = fixture.benchContract(b, currentTS, benchRequest(full, storage.Predicate{}))
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/float64(b.N), "retained_B/op")
		b.ReportMetric(float64(checksum), "checksum")
	})

	// Cross-path correctness: direct and contract scans over the same fixture
	// and snapshot must produce identical checksums before timings mean
	// anything.
	b.Run("verify=direct_matches_contract", func(b *testing.B) {
		directSum, directRows := fixture.benchDirect(b, currentTS, benchSpec(full, nil))
		contractSum, contractRows, _ := fixture.benchContract(b, currentTS, benchRequest(full, storage.Predicate{}))
		if directSum != contractSum || directRows != contractRows {
			b.Fatalf("direct/contract mismatch: sum %d vs %d, rows %d vs %d", directSum, contractSum, directRows, contractRows)
		}
		directSum, directRows = fixture.benchDirect(b, historicalTS, benchSpec(narrow, nonselectiveProg))
		contractSum, contractRows, _ = fixture.benchContract(b, historicalTS, benchRequest(narrow, nonselectivePred))
		if directSum != contractSum || directRows != contractRows {
			b.Fatalf("historical direct/contract mismatch: sum %d vs %d, rows %d vs %d", directSum, contractSum, directRows, contractRows)
		}
	})
}

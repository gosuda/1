package neosepia

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func makeTestVectorF32(seed int, dims int) []float32 {
	out := make([]float32, dims)
	for i := range out {
		out[i] = float32(seed) + float32(i)*0.25
	}
	return out
}

func makeTestJSONB(seed int, approxLen int) string {
	prefix := fmt.Sprintf(`{"id":%d,"pad":"`, seed)
	suffix := `"}`
	padLen := max(0, approxLen-len(prefix)-len(suffix))
	ch := byte('a' + seed%26)
	return prefix + strings.Repeat(string(ch), padLen) + suffix
}

func makeTestNumeric(seed int, digits int) string {
	if digits < 4 {
		digits = 4
	}
	ch := byte('1' + seed%9)
	return strings.Repeat(string(ch), digits-2) + ".5"
}

func makeTestUUID(seed int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", seed)
}

func decodeMemPAXBlockRow(t *testing.T, mb *memPAXBlock, colIDs []model.ColumnID) (keys [][]byte, rows [][]model.Value) {
	t.Helper()
	view, err := mb.openView(64<<20, nil)
	if err != nil {
		t.Fatalf("openView: %v", err)
	}
	defer func() {
		if err := view.Close(); err != nil {
			t.Errorf("close view: %v", err)
		}
	}()
	vecs := make([]columnar.Vector, len(colIDs))
	for i, id := range colIDs {
		v, err := view.Column(t.Context(), id)
		if err != nil {
			t.Fatalf("view.Column(%d): %v", id, err)
		}
		vecs[i] = v
	}
	for r := range view.Rows() {
		key, _, _, _, _, err := view.Version(r)
		if err != nil {
			t.Fatalf("view.Version(%d): %v", r, err)
		}
		keys = append(keys, bytes.Clone(key))
		vals := make([]model.Value, len(colIDs))
		for c := range colIDs {
			val, _, err := columnar.CopyValue(t.Context(), &vecs[c], r, 16<<20)
			if err != nil {
				t.Fatalf("CopyValue(row=%d,col=%d): %v", r, c, err)
			}
			vals[c] = val
		}
		rows = append(rows, vals)
	}
	return keys, rows
}

func TestBuildMemTargetBlocksLargeComplexTypes(t *testing.T) {
	// Verify saturating arithmetic and per-type PAX byte estimates.
	if got := saturatingAddU64(math.MaxUint64-5, 10); got != math.MaxUint64 {
		t.Fatalf("saturatingAddU64 overflow = %d, want MaxUint64", got)
	}
	if got := saturatingAddU64(100, 23); got != 123 {
		t.Fatalf("saturatingAddU64(100, 23) = %d, want 123", got)
	}
	if got := estimateMemValuePAXBytes(model.NullVal(model.TypeJSONB)); got != 8 {
		t.Fatalf("NULL estimate = %d, want 8", got)
	}
	if got := estimateMemValuePAXBytes(model.Value{Type: model.TypeInterval}); got != 16 {
		t.Fatalf("TypeInterval estimate = %d, want 16", got)
	}
	if got := estimateMemValuePAXBytes(model.Int64Val(42)); got != 8 {
		t.Fatalf("TypeInt64 estimate = %d, want 8", got)
	}
	if got := estimateMemValuePAXBytes(model.BytesVal(make([]byte, 100))); got != 105 {
		t.Fatalf("inline TypeBytes estimate = %d, want 105", got)
	}
	if got := estimateMemValuePAXBytes(model.BytesVal(make([]byte, toastThreshold+1))); got != 13 {
		t.Fatalf("TOAST TypeBytes estimate = %d, want 13", got)
	}
	if got := estimateMemValuePAXBytes(model.Value{Type: model.TypeJSONB, StrVal: strings.Repeat("x", 500)}); got != 505 {
		t.Fatalf("inline TypeJSONB estimate = %d, want 505", got)
	}
	if got := estimateMemValuePAXBytes(model.Value{Type: model.TypeJSONB, StrVal: strings.Repeat("x", toastThreshold+10)}); got != 13 {
		t.Fatalf("TOAST TypeJSONB estimate = %d, want 13", got)
	}
	if got := estimateMemValuePAXBytes(model.Value{Type: model.TypeVectorF32, VectorVal: make([]float32, 1024)}); got != 8+1024*4 {
		t.Fatalf("TypeVectorF32 estimate = %d, want %d", got, 8+1024*4)
	}
	if got := estimateMemValuePAXBytes(model.Value{
		Type:       model.TypeArray,
		Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}},
		ArrayVal:   []model.Value{model.StringVal("abcd"), model.StringVal("efgh")},
	}); got != 16+8+9+9 {
		t.Fatalf("TypeArray estimate = %d, want %d", got, 16+8+9+9)
	}
	if got := estimateMemValuePAXBytes(model.Value{
		Type:     model.TypeComposite,
		ArrayVal: []model.Value{model.Int64Val(1), model.StringVal("hello")},
	}); got != 8+8+10 {
		t.Fatalf("TypeComposite estimate = %d, want %d", got, 8+8+10)
	}

	target := encTestTarget()
	intType := model.Type{ID: 20, Storage: model.TypeInt64}
	vecType := model.Type{ID: 143, Storage: model.TypeVectorF32}
	jsonbType := model.Type{ID: 3802, Storage: model.TypeJSONB}
	numType := model.Type{ID: 1700, Storage: model.TypeNumeric}
	uuidType := model.Type{ID: 2950, Storage: model.TypeUUID}
	textType := model.Type{ID: 25, Storage: model.TypeString}
	strArrType := model.Type{ID: 1009, Storage: model.TypeArray}
	nestedArrType := model.Type{ID: 1010, Storage: model.TypeArray}
	compType := model.Type{ID: 2249, Storage: model.TypeComposite}

	layout := storage.ObjectLayout{
		Target: target,
		Columns: []storage.ColumnLayout{
			{ID: 1, Type: intType},
			{ID: 2, Type: vecType, Nullable: true},
			{ID: 3, Type: jsonbType, Nullable: true},
			{ID: 4, Type: numType, Nullable: true},
			{ID: 5, Type: uuidType, Nullable: true},
			{
				ID:       6,
				Type:     nestedArrType,
				Nullable: true,
				Children: []model.TypeLayout{{
					Type:     strArrType,
					Children: []model.TypeLayout{{Type: textType}},
				}},
			},
			{
				ID:       7,
				Type:     compType,
				Nullable: true,
				Children: []model.TypeLayout{
					{Type: intType},
					{Type: vecType},
					{Type: strArrType, Children: []model.TypeLayout{{Type: textType}}},
				},
			},
		},
	}
	colIDs := []model.ColumnID{1, 2, 3, 4, 5, 6, 7}

	state := &memTargetState{
		layouts: map[uint64]storage.ObjectLayout{target.DescriptorVersion: layout},
	}

	const rowCount = 16
	wantKeys := make([][]byte, 0, rowCount)
	wantRows := make([][]model.Value, 0, rowCount)

	for i := range rowCount {
		key := backendKey(int64(i + 1))
		jsonLen := 3000
		if i == 5 {
			// Exercise TOASTed JSONB alongside inline JSONB.
			jsonLen = toastThreshold + 4096
		}
		innerA := model.Value{
			Type:       model.TypeArray,
			Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}},
			ArrayVal: []model.Value{
				model.StringVal(fmt.Sprintf("row-%02d-a-%s", i, strings.Repeat("x", 512))),
				model.StringVal(fmt.Sprintf("row-%02d-b-%s", i, strings.Repeat("y", 512))),
			},
		}
		innerB := model.Value{
			Type:       model.TypeArray,
			Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}},
			ArrayVal: []model.Value{
				model.StringVal(fmt.Sprintf("row-%02d-c-%s", i, strings.Repeat("z", 512))),
			},
		}
		nestedArr := model.Value{
			Type:       model.TypeArray,
			Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}},
			ArrayVal:   []model.Value{innerA, innerB},
		}
		comp := model.Value{
			Type: model.TypeComposite,
			ArrayVal: []model.Value{
				model.Int64Val(int64(i * 10)),
				{Type: model.TypeVectorF32, VectorVal: makeTestVectorF32(i+100, 256)},
				{
					Type:       model.TypeArray,
					Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}},
					ArrayVal: []model.Value{
						model.StringVal(fmt.Sprintf("cf1-%02d-%s", i, strings.Repeat("q", 256))),
						model.StringVal(fmt.Sprintf("cf2-%02d-%s", i, strings.Repeat("w", 256))),
					},
				},
			},
		}
		vals := []model.Value{
			model.Int64Val(int64(i + 1)),
			{Type: model.TypeVectorF32, VectorVal: makeTestVectorF32(i+1, 768)},
			{Type: model.TypeJSONB, StrVal: makeTestJSONB(i+1, jsonLen)},
			{Type: model.TypeNumeric, StrVal: makeTestNumeric(i+1, 1024)},
			{Type: model.TypeUUID, StrVal: makeTestUUID(i + 1)},
			nestedArr,
			comp,
		}
		if i == 11 {
			// Exercise NULL complex values as well.
			vals[1] = model.NullVal(model.TypeVectorF32)
			vals[2] = model.NullVal(model.TypeJSONB)
			vals[5] = model.NullVal(model.TypeArray)
			vals[6] = model.NullVal(model.TypeComposite)
		}
		entry := &memEntry{
			target:    target,
			key:       key,
			commitTS:  model.Timestamp(10 + i),
			ordinal:   uint64(i + 1),
			op:        storage.MutationUpsert,
			columnIDs: colIDs,
			values:    vals,
		}
		if est := estimateMemEntryPAXBytes(entry); est < 1000 && i != 11 {
			t.Fatalf("row %d estimateMemEntryPAXBytes = %d, expected accurate large estimate", i, est)
		}
		var err error
		state.entries, _, err = state.entries.Set(memVersionKey{key: key, commitTS: entry.commitTS, ordinal: entry.ordinal}, entry, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		state.rows++
		wantKeys = append(wantKeys, key)
		wantRows = append(wantRows, vals)
	}

	limits := blockLimits{
		MaxRows:         1024,
		MaxBytes:        32 << 10,
		MaxDecodedBytes: 64 << 10,
	}
	blocks, err := buildMemTargetBlocks(t.Context(), state, 0, limits)
	if err != nil {
		t.Fatalf("buildMemTargetBlocks under tight limits failed: %v", err)
	}
	if len(blocks) <= 1 {
		t.Fatalf("expected multiple PAX blocks under %d-byte limit, got %d", limits.MaxBytes, len(blocks))
	}

	var gotKeys [][]byte
	var gotRows [][]model.Value
	for i, mb := range blocks {
		if uint64(len(mb.data)) > limits.MaxBytes {
			t.Fatalf("block %d size %d exceeds MaxBytes %d", i, len(mb.data), limits.MaxBytes)
		}
		ks, rs := decodeMemPAXBlockRow(t, mb, colIDs)
		gotKeys = append(gotKeys, ks...)
		gotRows = append(gotRows, rs...)
	}
	if len(gotRows) != rowCount {
		t.Fatalf("decoded %d rows across %d blocks, want %d", len(gotRows), len(blocks), rowCount)
	}
	for i := range rowCount {
		if !bytes.Equal(gotKeys[i], wantKeys[i]) {
			t.Fatalf("row %d key mismatch: got %x want %x", i, gotKeys[i], wantKeys[i])
		}
		if !reflect.DeepEqual(gotRows[i], wantRows[i]) {
			t.Fatalf("row %d value mismatch:\ngot  %+v\nwant %+v", i, gotRows[i], wantRows[i])
		}
	}
}

func TestEncodeMemEntriesBlocksSplitsOnBudget(t *testing.T) {
	target := encTestTarget()
	intType := model.Type{ID: 20, Storage: model.TypeInt64}
	vecType := model.Type{ID: 143, Storage: model.TypeVectorF32}
	layout := storage.ObjectLayout{
		Target: target,
		Columns: []storage.ColumnLayout{
			{ID: 1, Type: intType},
			{ID: 2, Type: vecType},
		},
	}
	colIDs := []model.ColumnID{1, 2}

	const count = 12
	entries := make([]*memEntry, count)
	for i := range count {
		entries[i] = &memEntry{
			target:    target,
			key:       backendKey(int64(i + 1)),
			commitTS:  model.Timestamp(100),
			ordinal:   uint64(i + 1),
			op:        storage.MutationUpsert,
			columnIDs: colIDs,
			values: []model.Value{
				model.Int64Val(int64(i + 1)),
				{Type: model.TypeVectorF32, VectorVal: makeTestVectorF32(i+1, 1024)},
			},
		}
	}

	// Single block cannot hold all 12 rows (~49 KiB); encodeMemEntriesBlock must fail
	// with ErrBatchBudget and encodeMemEntriesBlocks must split recursively.
	tightMaxBytes := blockLimits{MaxRows: count, MaxBytes: 16 << 10, MaxDecodedBytes: 64 << 10}
	if _, err := encodeMemEntriesBlock(t.Context(), layout, entries, tightMaxBytes); !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("expected single-block encodeMemEntriesBlock to hit ErrBatchBudget, got %v", err)
	}

	blocks, err := encodeMemEntriesBlocks(t.Context(), layout, entries, tightMaxBytes)
	if err != nil {
		t.Fatalf("encodeMemEntriesBlocks failed: %v", err)
	}
	if len(blocks) <= 1 {
		t.Fatalf("expected split into multiple blocks, got %d", len(blocks))
	}
	var decodedCount int
	for i, mb := range blocks {
		if uint64(len(mb.data)) > tightMaxBytes.MaxBytes {
			t.Fatalf("split block %d size %d exceeds MaxBytes %d", i, len(mb.data), tightMaxBytes.MaxBytes)
		}
		ks, rs := decodeMemPAXBlockRow(t, mb, colIDs)
		for j := range rs {
			want := entries[decodedCount]
			if !bytes.Equal(ks[j], want.key) {
				t.Fatalf("row %d key mismatch: got %x want %x", decodedCount, ks[j], want.key)
			}
			if !reflect.DeepEqual(rs[j], want.values) {
				t.Fatalf("row %d values mismatch", decodedCount)
			}
			decodedCount++
		}
	}
	if decodedCount != count {
		t.Fatalf("decoded %d rows across %d split blocks, want %d", decodedCount, len(blocks), count)
	}

	// Also verify split when MaxDecodedBytes (encoder temporary budget) is the limiting factor.
	tightDecoded := blockLimits{MaxRows: count, MaxBytes: 128 << 10, MaxDecodedBytes: 16 << 10}
	decodedBlocks, err := encodeMemEntriesBlocks(t.Context(), layout, entries, tightDecoded)
	if err != nil {
		t.Fatalf("encodeMemEntriesBlocks under tight MaxDecodedBytes failed: %v", err)
	}
	if len(decodedBlocks) <= 1 {
		t.Fatalf("expected split under tight MaxDecodedBytes, got %d blocks", len(decodedBlocks))
	}

	// Single entry that cannot fit even alone must still return ErrBatchBudget.
	impossible := blockLimits{MaxRows: 1, MaxBytes: 1024, MaxDecodedBytes: 1024}
	if _, err := encodeMemEntriesBlocks(t.Context(), layout, entries[:1], impossible); !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("expected ErrBatchBudget for single oversized entry, got %v", err)
	}
}

func TestBackendMemtableFlushAndScanWithLargeVectorAndJSONB(t *testing.T) {
	limits := DefaultLimits()
	limits.BlockBytes = 32 << 10

	fixture := openBackendFixtureWithLimits(t, limits)
	target := encTestTarget()
	intType := model.Type{ID: 20, Storage: model.TypeInt64}
	vecType := model.Type{ID: 143, Storage: model.TypeVectorF32}
	jsonbType := model.Type{ID: 3802, Storage: model.TypeJSONB}
	layout := []storage.ColumnLayout{
		{ID: 1, Type: intType},
		{ID: 2, Type: vecType},
		{ID: 3, Type: jsonbType},
	}
	colIDs := []model.ColumnID{1, 2, 3}

	type testRow struct {
		id    int64
		vec   []float32
		jsonb string
	}
	const numRows = 16
	rows := make([]testRow, numRows)
	for i := range numRows {
		jsonLen := 8 << 10 // inline JSONB (< toastThreshold)
		if i == 3 || i == 12 {
			jsonLen = toastThreshold + 8192 // TOASTed JSONB (> toastThreshold)
		}
		rows[i] = testRow{
			id:    int64(i + 1),
			vec:   makeTestVectorF32(i+1, 1536), // 6 KiB inline float32 vector
			jsonb: makeTestJSONB(i+1, jsonLen),
		}
	}

	// Insert across 4 commits of 4 rows each so they accumulate in the memtable.
	var maxTS model.Timestamp
	for batchIdx := range 4 {
		commitTS := model.Timestamp((batchIdx + 1) * 10)
		maxTS = commitTS
		batch := storage.MutationBatch{
			CommitTS:        commitTS,
			Epoch:           fixture.epoch,
			EncodingVersion: storage.MutationEncodingVersion,
			Layouts:         []storage.ObjectLayout{{Target: target, Columns: layout}},
		}
		binary.LittleEndian.PutUint64(batch.TxID[:], uint64(batchIdx+1))
		for j := range 4 {
			r := rows[batchIdx*4+j]
			batch.Mutations = append(batch.Mutations, storage.Mutation{
				Op:        storage.MutationUpsert,
				Target:    target,
				Key:       backendKey(r.id),
				Ordinal:   uint64(j + 1),
				ColumnIDs: colIDs,
				Values: []model.Value{
					model.Int64Val(r.id),
					{Type: model.TypeVectorF32, VectorVal: r.vec},
					{Type: model.TypeJSONB, StrVal: r.jsonb},
				},
			})
		}
		var err error
		batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
			t.Fatalf("ApplyMutations batch %d: %v", batchIdx, err)
		}
	}

	verifyAll := func(stage string) {
		t.Helper()
		reader := fixture.snapshot(t, maxTS)
		// 1) Point Get for every row.
		for _, want := range rows {
			got, err := reader.Get(t.Context(), storage.ReadRequest{
				Target:    target,
				Key:       backendKey(want.id),
				ColumnIDs: colIDs,
				Layout:    layout,
			})
			if err != nil {
				t.Fatalf("%s Get(id=%d): %v", stage, want.id, err)
			}
			if got.Values[0].IntVal != want.id || !slices.Equal(got.Values[1].VectorVal, want.vec) || got.Values[2].StrVal != want.jsonb {
				t.Fatalf("%s Get(id=%d) mismatch: id=%d vecLen=%d jsonLen=%d", stage, want.id, got.Values[0].IntVal, len(got.Values[1].VectorVal), len(got.Values[2].StrVal))
			}
		}

		// 2) VectorizedScan both forward and reverse.
		for _, dir := range []storage.ScanDirection{storage.ScanForward, storage.ScanReverse} {
			plan, err := reader.PlanScan(t.Context(), storage.PushdownRequest{
				Target:    target,
				KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: intType}},
				Layout:    layout,
				ColumnIDs: colIDs,
				Spans:     []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
				Direction: dir,
			})
			if err != nil {
				t.Fatalf("%s PlanScan(dir=%v): %v", stage, dir, err)
			}
			cursor, err := reader.VectorizedScan(t.Context(), plan)
			if err != nil {
				_ = reader.ReleasePlan(plan)
				t.Fatalf("%s VectorizedScan(dir=%v): %v", stage, dir, err)
			}
			var out columnar.Batch
			var got []testRow
			for {
				err := cursor.Next(t.Context(), &out)
				if err == io.EOF {
					break
				}
				if err != nil {
					_ = cursor.Close()
					_ = reader.ReleasePlan(plan)
					t.Fatalf("%s cursor.Next(dir=%v): %v", stage, dir, err)
				}
				for r := range out.Rows() {
					lane := out.Lane(r)
					idV, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], lane, 64)
					if err != nil {
						t.Fatal(err)
					}
					vecV, _, err := columnar.CopyValue(t.Context(), &out.Columns[1], lane, 1<<20)
					if err != nil {
						t.Fatal(err)
					}
					jsonV, _, err := columnar.CopyValue(t.Context(), &out.Columns[2], lane, 1<<20)
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, testRow{id: idV.IntVal, vec: vecV.VectorVal, jsonb: jsonV.StrVal})
				}
			}
			_ = cursor.Close()
			_ = reader.ReleasePlan(plan)
			if len(got) != numRows {
				t.Fatalf("%s scan(dir=%v) got %d rows, want %d", stage, dir, len(got), numRows)
			}
			for i := range numRows {
				wantIdx := i
				if dir == storage.ScanReverse {
					wantIdx = numRows - 1 - i
				}
				want := rows[wantIdx]
				if got[i].id != want.id || !slices.Equal(got[i].vec, want.vec) || got[i].jsonb != want.jsonb {
					t.Fatalf("%s scan(dir=%v) row %d mismatch: got id=%d want id=%d", stage, dir, i, got[i].id, want.id)
				}
			}
		}
	}

	// Stage 1: Verify data is buffered in memtable (no SSTs yet) and scan directly from memtable.
	if ssts := runSSTNames(t, fixture.config.Directory); len(ssts) != 0 {
		t.Fatalf("expected 0 SST files before flush, got %d: %v", len(ssts), ssts)
	}
	verifyAll("memtable")

	// Stage 2: Flush memtable to SST runs under tight 32 KiB BlockBytes.
	fixture.backend.commitMu.Lock()
	fixture.backend.mu.Lock()
	flushErr := fixture.backend.flushMemtableLocked(t.Context(), 0)
	fixture.backend.mu.Unlock()
	fixture.backend.commitMu.Unlock()
	if flushErr != nil {
		t.Fatalf("flushMemtableLocked with large VectorF32 and JSONB: %v", flushErr)
	}
	if ssts := runSSTNames(t, fixture.config.Directory); len(ssts) == 0 {
		t.Fatalf("expected SST files after memtable flush, got 0")
	}
	verifyAll("post-flush")

	// Stage 3: Close and reopen backend, then verify all rows survive restart.
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatalf("reopen backend after flush: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.backend = reopened
	verifyAll("reopened")
}

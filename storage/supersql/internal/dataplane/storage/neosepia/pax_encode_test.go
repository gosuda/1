package neosepia

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// These tests parse the fixed wire format directly (header, descriptors,
// visibility records, payloads) so they hold with or without the decoder.

func encTestTarget() storage.ObjectRef {
	return storage.ObjectRef{ObjectID: 7, Generation: 3, DescriptorVersion: 5, EncodingVersion: 2}
}

func encTestLimits() blockLimits {
	return blockLimits{MaxRows: 1024, MaxBytes: 1 << 24, MaxDecodedBytes: 1 << 24}
}

func encTestKeyVector(keys ...string) columnar.Vector {
	var data []byte
	ranges := make([]columnar.Range, len(keys))
	for i, k := range keys {
		ranges[i] = columnar.Range{Offset: uint32(len(data)), Length: uint32(len(k))}
		data = append(data, k...)
	}
	return columnar.Vector{
		Type:     model.Type{ID: 17, Storage: model.TypeBytes},
		Encoding: columnar.Flat,
		Length:   len(keys),
		Data:     data,
		Ranges:   ranges,
	}
}

func encTestVersions(keys columnar.Vector, ts []model.Timestamp, kinds []storage.MutationOp) blockVersions {
	ords := make([]uint64, len(ts))
	for i := range ords {
		ords[i] = uint64(i + 1)
	}
	return blockVersions{Keys: keys, CommitTS: ts, Ordinals: ords, Kinds: kinds}
}

func encTestBuildBatch(t *testing.T, layouts []model.TypeLayout, rows [][]model.Value) (columnar.Batch, func()) {
	t.Helper()
	b, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: 16, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	closeBuilder := func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(closeBuilder)
	ctx := context.Background()
	for i, row := range rows {
		if err := b.Append(ctx, row); err != nil {
			t.Fatalf("append row %d: %v", i, err)
		}
	}
	var out columnar.Batch
	if err := b.Publish(&out); err != nil {
		t.Fatal(err)
	}
	if err := out.Validate(16); err != nil {
		t.Fatal(err)
	}
	return out, closeBuilder
}

func encTestHeader(t *testing.T, out []byte, rows, cols int) {
	t.Helper()
	if len(out) < paxHeaderSize {
		t.Fatalf("block shorter than header: %d", len(out))
	}
	if got := binary.LittleEndian.Uint32(out[0:]); got != paxMagic {
		t.Fatalf("magic = %#x, want %#x", got, paxMagic)
	}
	if got := binary.LittleEndian.Uint32(out[4:]); got != paxVersion {
		t.Fatalf("version = %d, want %d", got, paxVersion)
	}
	if got := binary.LittleEndian.Uint32(out[8:]); got != uint32(rows) {
		t.Fatalf("rows = %d, want %d", got, rows)
	}
	if got := binary.LittleEndian.Uint32(out[12:]); got != uint32(cols) {
		t.Fatalf("cols = %d, want %d", got, cols)
	}
	if got := binary.LittleEndian.Uint64(out[48:]); got != uint64(len(out)) {
		t.Fatalf("total = %d, len = %d", got, len(out))
	}
	if got := binary.LittleEndian.Uint32(out[60:]); got != 0 {
		t.Fatalf("reserved = %d, want 0", got)
	}
	if got := binary.LittleEndian.Uint32(out[56:]); got != paxChecksum(out) {
		t.Fatalf("checksum = %#x, recomputed %#x", got, paxChecksum(out))
	}
}

func encTestDescriptor(out []byte, index int) []byte {
	base := paxHeaderSize + index*paxColumnSize
	return out[base : base+paxColumnSize : base+paxColumnSize]
}

func TestPAXEncodeHeaderVisibility(t *testing.T) {
	layouts := []model.TypeLayout{{Type: model.Type{ID: 20, Storage: model.TypeInt64}}}
	batch, done := encTestBuildBatch(t, layouts, [][]model.Value{
		{model.Int64Val(5)},
		{model.NullVal(model.TypeInt64)},
		{model.Int64Val(-2)},
	})
	defer done()
	target := encTestTarget()
	keys := encTestKeyVector("k1", "k2", "k3")
	versions := encTestVersions(keys,
		[]model.Timestamp{10, 20, 30},
		[]storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert, storage.MutationUpsert})
	out, err := encodeBlock(context.Background(), target, versions, &batch, []model.ColumnID{11}, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	encTestHeader(t, out, 3, 1)
	if got := binary.LittleEndian.Uint64(out[16:]); got != 7 {
		t.Fatalf("objectID = %d", got)
	}
	if got := binary.LittleEndian.Uint64(out[24:]); got != 3 {
		t.Fatalf("generation = %d", got)
	}
	if got := binary.LittleEndian.Uint64(out[32:]); got != 5 {
		t.Fatalf("descriptorVersion = %d", got)
	}
	if got := binary.LittleEndian.Uint32(out[40:]); got != 2 {
		t.Fatalf("encodingVersion = %d", got)
	}
	visLen := binary.LittleEndian.Uint32(out[44:])
	wantVis := uint32(3*paxVersionSize + len("k1k2k3"))
	if visLen != wantVis {
		t.Fatalf("visLen = %d, want %d", visLen, wantVis)
	}
	desc := encTestDescriptor(out, 0)
	if got := binary.LittleEndian.Uint32(desc[0:]); got != 11 {
		t.Fatalf("columnID = %d", got)
	}
	if desc[4] != byte(model.TypeInt64) {
		t.Fatalf("storage = %d", desc[4])
	}
	switch columnEncoding(desc[5]) {
	case encodingPlain, encodingRLE, encodingDelta:
	default:
		t.Fatalf("unexpected integer encoding %d", desc[5])
	}
	if got := binary.LittleEndian.Uint16(desc[6:]); got != paxZoneInteger {
		t.Fatalf("flags = %d, want zone", got)
	}
	if got := binary.LittleEndian.Uint32(desc[8:]); got != 3 {
		t.Fatalf("node rows = %d", got)
	}
	if got := binary.LittleEndian.Uint32(desc[12:]); got != 1 {
		t.Fatalf("nulls = %d", got)
	}
	if got := binary.LittleEndian.Uint32(desc[16:]); got != 20 {
		t.Fatalf("typeID = %d", got)
	}
	if got := binary.LittleEndian.Uint32(desc[28:]); got != 0 {
		t.Fatalf("childCount = %d", got)
	}
	off := binary.LittleEndian.Uint64(desc[32:])
	if off%8 != 0 {
		t.Fatalf("payload offset %d not 8-aligned", off)
	}
	payLen := binary.LittleEndian.Uint64(desc[40:])
	payload := out[off : off+payLen]
	if got := int64(binary.LittleEndian.Uint64(desc[48:])); got != -2 {
		t.Fatalf("zone min = %d", got)
	}
	if got := int64(binary.LittleEndian.Uint64(desc[56:])); got != 5 {
		t.Fatalf("zone max = %d", got)
	}
	// NULL bitmap: row 1 set, trailing unused bits zero.
	if payload[0] != 0b00000010 {
		t.Fatalf("null bitmap = %08b", payload[0])
	}
	body := payload[1:]
	dst := make([]int64, 3)
	if err := decodeIntegers(body, columnEncoding(desc[5]), dst); err != nil {
		t.Fatalf("decodeIntegers: %v", err)
	}
	if dst[0] != 5 || dst[1] != 0 || dst[2] != -2 {
		t.Fatalf("integer lanes = %v", dst)
	}
	// Visibility records.
	vbase := uint64(paxHeaderSize) + paxColumnSize
	for r, want := range []struct {
		key string
		ts  model.Timestamp
		ord uint64
	}{
		{"k1", 10, 1}, {"k2", 20, 2}, {"k3", 30, 3},
	} {
		rec := out[vbase+uint64(r)*paxVersionSize:]
		if got := model.Timestamp(binary.LittleEndian.Uint64(rec[0:])); got != want.ts {
			t.Fatalf("row %d ts = %d", r, got)
		}
		if got := binary.LittleEndian.Uint64(rec[8:]); got != want.ord {
			t.Fatalf("row %d ordinal = %d", r, got)
		}
		if rec[16] != byte(storage.MutationUpsert) {
			t.Fatalf("row %d op = %d", r, rec[16])
		}
		if !bytes.Equal(rec[17:24], make([]byte, 7)) {
			t.Fatalf("row %d reserved nonzero", r)
		}
		ko := binary.LittleEndian.Uint32(rec[24:])
		kl := binary.LittleEndian.Uint32(rec[28:])
		el := binary.LittleEndian.Uint32(rec[36:])
		if el != 0 {
			t.Fatalf("row %d end length = %d", r, el)
		}
		region := out[vbase : vbase+uint64(visLen)]
		if got := string(region[ko : ko+kl]); got != want.key {
			t.Fatalf("row %d key = %q", r, got)
		}
	}
}

func TestPAXEncodeScalarBodies(t *testing.T) {
	nanBits := uint64(0x7FF8000000000001)
	layouts := []model.TypeLayout{
		{Type: model.Type{ID: 701, Storage: model.TypeFloat64}},
		{Type: model.Type{ID: 1186, Storage: model.TypeInterval}},
		{Type: model.Type{ID: 16, Storage: model.TypeBoolean}},
		{Type: model.Type{ID: 1114, Storage: model.TypeTimestamp}},
	}
	batch, done := encTestBuildBatch(t, layouts, [][]model.Value{
		{
			{Type: model.TypeFloat64, FloatVal: 1.5},
			{Type: model.TypeInterval, IntervalVal: model.Interval{Months: 1, Days: 2, Microseconds: 3}},
			model.BoolVal(true),
			{Type: model.TypeTimestamp, IntVal: 100},
		},
		{
			{Type: model.TypeFloat64, FloatVal: math.Float64frombits(nanBits)},
			model.NullVal(model.TypeInterval),
			model.BoolVal(false),
			model.NullVal(model.TypeTimestamp),
		},
		{
			{Type: model.TypeFloat64, FloatVal: math.Float64frombits(0x8000000000000000)}, // -0.0
			{Type: model.TypeInterval, IntervalVal: model.Interval{Months: -5, Days: 0, Microseconds: -9}},
			model.NullVal(model.TypeBoolean),
			{Type: model.TypeTimestamp, IntVal: 200},
		},
	})
	defer done()
	keys := encTestKeyVector("a", "b", "c")
	versions := encTestVersions(keys,
		[]model.Timestamp{1, 2, 3},
		[]storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert, storage.MutationUpsert})
	ids := []model.ColumnID{1, 2, 3, 4}
	out, err := encodeBlock(context.Background(), encTestTarget(), versions, &batch, ids, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	encTestHeader(t, out, 3, 4)
	// Float64: exact IEEE bits, all three lanes.
	fdesc := encTestDescriptor(out, 0)
	if columnEncoding(fdesc[5]) != encodingPlain {
		t.Fatalf("float encoding = %d", fdesc[5])
	}
	foff := binary.LittleEndian.Uint64(fdesc[32:])
	flen := binary.LittleEndian.Uint64(fdesc[40:])
	if flen != 24 {
		t.Fatalf("float payload len = %d", flen)
	}
	fpay := out[foff : foff+flen]
	for i, want := range []uint64{0x3FF8000000000000, nanBits, 0x8000000000000000} {
		if got := binary.LittleEndian.Uint64(fpay[i*8:]); got != want {
			t.Fatalf("float lane %d bits = %#x, want %#x", i, got, want)
		}
	}
	// Interval: 16 bytes per lane, NULL lane zeroed by the builder.
	idesc := encTestDescriptor(out, 1)
	ioff := binary.LittleEndian.Uint64(idesc[32:])
	ilen := binary.LittleEndian.Uint64(idesc[40:])
	if ilen != 1+48 { // bitmap + 3 lanes
		t.Fatalf("interval payload len = %d", ilen)
	}
	ipay := out[ioff : ioff+ilen]
	if ipay[0] != 0b00000010 {
		t.Fatalf("interval bitmap = %08b", ipay[0])
	}
	if got := int32(binary.LittleEndian.Uint32(ipay[1:])); got != 1 {
		t.Fatalf("interval months = %d", got)
	}
	// Boolean zone covers only non-NULL lanes; NULL lane body slot decodes.
	bdesc := encTestDescriptor(out, 2)
	if got := binary.LittleEndian.Uint16(bdesc[6:]); got != paxZoneInteger {
		t.Fatalf("bool flags = %d", got)
	}
	if got := int64(binary.LittleEndian.Uint64(bdesc[48:])); got != 0 {
		t.Fatalf("bool min = %d", got)
	}
	if got := int64(binary.LittleEndian.Uint64(bdesc[56:])); got != 1 {
		t.Fatalf("bool max = %d", got)
	}
	boff := binary.LittleEndian.Uint64(bdesc[32:])
	blen := binary.LittleEndian.Uint64(bdesc[40:])
	bpay := out[boff : boff+blen]
	if bpay[0] != 0b00000100 {
		t.Fatalf("bool bitmap = %08b", bpay[0])
	}
	bdst := make([]int64, 3)
	if err := decodeIntegers(bpay[1:], columnEncoding(bdesc[5]), bdst); err != nil {
		t.Fatalf("bool decode: %v", err)
	}
	if bdst[0] != 1 || bdst[1] != 0 || bdst[2] != 0 {
		t.Fatalf("bool lanes = %v", bdst)
	}
	// Timestamp zone ignores the NULL lane.
	tdesc := encTestDescriptor(out, 3)
	if got := int64(binary.LittleEndian.Uint64(tdesc[48:])); got != 100 {
		t.Fatalf("ts min = %d", got)
	}
	if got := int64(binary.LittleEndian.Uint64(tdesc[56:])); got != 200 {
		t.Fatalf("ts max = %d", got)
	}
}

func TestPAXEncodeVarBytesPlainAndDict(t *testing.T) {
	layouts := []model.TypeLayout{
		{Type: model.Type{ID: 25, Storage: model.TypeString}},
		{Type: model.Type{ID: 17, Storage: model.TypeBytes}},
		{Type: model.Type{ID: 1700, Storage: model.TypeNumeric}},
	}
	// Distinct long strings: dictionary must lose, plain wins.
	batch, done := encTestBuildBatch(t, layouts, [][]model.Value{
		{model.StringVal("alpha-distinct-00"), model.BytesVal([]byte{0x01}), {Type: model.TypeNumeric, StrVal: "1.10"}},
		{model.StringVal("bravo-distinct-01"), model.NullVal(model.TypeBytes), {Type: model.TypeNumeric, StrVal: "2"}},
		{model.StringVal(""), model.BytesVal(nil), model.NullVal(model.TypeNumeric)},
	})
	defer done()
	keys := encTestKeyVector("a", "b", "c")
	versions := encTestVersions(keys,
		[]model.Timestamp{1, 2, 3},
		[]storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert, storage.MutationUpsert})
	out, err := encodeBlock(context.Background(), encTestTarget(), versions, &batch, []model.ColumnID{1, 2, 3}, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	sdesc := encTestDescriptor(out, 0)
	if columnEncoding(sdesc[5]) != encodingPlain {
		t.Fatalf("distinct strings encoding = %d, want plain", sdesc[5])
	}
	soff := binary.LittleEndian.Uint64(sdesc[32:])
	slen := binary.LittleEndian.Uint64(sdesc[40:])
	spay := out[soff : soff+slen]
	// Bitmap marks row 1 NULL... there is no NULL in the string column;
	// rows are ("alpha..", "bravo..", "") so no bitmap and three entries.
	if got := binary.LittleEndian.Uint32(sdesc[12:]); got != 0 {
		t.Fatalf("string nulls = %d", got)
	}
	pos := 0
	for i, want := range []string{"alpha-distinct-00", "bravo-distinct-01", ""} {
		l := binary.LittleEndian.Uint32(spay[pos:])
		pos += 4
		if got := string(spay[pos : pos+int(l)]); got != want {
			t.Fatalf("string lane %d = %q", i, got)
		}
		pos += int(l)
	}
	if pos != len(spay) {
		t.Fatalf("string body has %d trailing bytes", len(spay)-pos)
	}
	// Bytes column: NULL lane encodes as zero length with no payload bytes.
	bydesc := encTestDescriptor(out, 1)
	byoff := binary.LittleEndian.Uint64(bydesc[32:])
	bylen := binary.LittleEndian.Uint64(bydesc[40:])
	bypay := out[byoff : byoff+bylen]
	if bypay[0] != 0b00000010 {
		t.Fatalf("bytes bitmap = %08b", bypay[0])
	}
	rest := bypay[1:]
	if got := binary.LittleEndian.Uint32(rest[0:]); got != 1 || rest[4] != 0x01 {
		t.Fatalf("bytes lane 0 = %x", rest[:5])
	}
	if got := binary.LittleEndian.Uint32(rest[5:]); got != 0 {
		t.Fatalf("bytes NULL lane length = %d", got)
	}
	if got := binary.LittleEndian.Uint32(rest[9:]); got != 0 {
		t.Fatalf("bytes empty lane length = %d", got)
	}
	if len(rest) != 4+1+4+4 {
		t.Fatalf("bytes body len = %d", len(rest))
	}
	// Numeric preserves its text form exactly.
	ndesc := encTestDescriptor(out, 2)
	noff := binary.LittleEndian.Uint64(ndesc[32:])
	nlen := binary.LittleEndian.Uint64(ndesc[40:])
	npay := out[noff : noff+nlen]
	if npay[0] != 0b00000100 {
		t.Fatalf("numeric bitmap = %08b", npay[0])
	}
	nrest := npay[1:]
	if l := binary.LittleEndian.Uint32(nrest[0:]); string(nrest[4:4+l]) != "1.10" {
		t.Fatalf("numeric lane 0 = %q", nrest[4:4+l])
	}
	// Repeated short strings: dictionary must win.
	b2, done2 := encTestBuildBatch(t,
		[]model.TypeLayout{{Type: model.Type{ID: 25, Storage: model.TypeString}}},
		[][]model.Value{
			{model.StringVal("x")}, {model.StringVal("y")},
			{model.StringVal("x")}, {model.StringVal("y")},
			{model.StringVal("x")}, {model.StringVal("y")},
			{model.StringVal("x")}, {model.StringVal("y")},
		})
	defer done2()
	k2 := encTestKeyVector("a", "b", "c", "d", "e", "f", "g", "h")
	v2 := encTestVersions(k2,
		[]model.Timestamp{1, 2, 3, 4, 5, 6, 7, 8},
		[]storage.MutationOp{
			storage.MutationUpsert, storage.MutationUpsert,
			storage.MutationUpsert, storage.MutationUpsert,
			storage.MutationUpsert, storage.MutationUpsert,
			storage.MutationUpsert, storage.MutationUpsert,
		})
	out2, err := encodeBlock(context.Background(), encTestTarget(), v2, &b2, []model.ColumnID{1}, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	ddesc := encTestDescriptor(out2, 0)
	if columnEncoding(ddesc[5]) != encodingDictionary {
		t.Fatalf("repeated strings encoding = %d, want dictionary", ddesc[5])
	}
	doff := binary.LittleEndian.Uint64(ddesc[32:])
	dlen := binary.LittleEndian.Uint64(ddesc[40:])
	dpay := out2[doff : doff+dlen]
	// Dictionary body: [dictCount, totalRows, bitWidth], entries, packed codes.
	if got := binary.LittleEndian.Uint32(dpay[0:]); got != 2 {
		t.Fatalf("dictCount = %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint32(dpay[4:]); got != 8 {
		t.Fatalf("totalRows = %d, want 8", got)
	}
	if got := dpay[8]; got != 1 {
		t.Fatalf("bitWidth = %d, want 1", got)
	}
	pos = 9
	for _, want := range []string{"x", "y"} {
		l := binary.LittleEndian.Uint32(dpay[pos:])
		pos += 4
		if got := string(dpay[pos : pos+int(l)]); got != want {
			t.Fatalf("dict entry = %q, want %q", got, want)
		}
		pos += int(l)
	}
	// 8 one-bit codes pack into exactly 1 byte: lanes x,y,x,y... = 10101010b.
	if len(dpay)-pos != 1 {
		t.Fatalf("packed codes len = %d, want 1", len(dpay)-pos)
	}
	if dpay[pos] != 0b10101010 {
		t.Fatalf("packed codes = %08b, want 10101010", dpay[pos])
	}
}

func TestPAXEncodeRangeDelete(t *testing.T) {
	batch, done := encTestBuildBatch(t,
		[]model.TypeLayout{{Type: model.Type{ID: 20, Storage: model.TypeInt64}}},
		[][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}})
	defer done()
	versions := blockVersions{
		Keys:     encTestKeyVector("a", "m"),
		EndKeys:  encTestKeyVector("c", ""),
		CommitTS: []model.Timestamp{5, 6},
		Ordinals: []uint64{1, 2},
		Kinds:    []storage.MutationOp{storage.MutationDeleteRange, storage.MutationUpsert},
	}
	out, err := encodeBlock(context.Background(), encTestTarget(), versions, &batch, []model.ColumnID{9}, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	encTestHeader(t, out, 2, 1)
	visLen := binary.LittleEndian.Uint32(out[44:])
	vbase := uint64(paxHeaderSize) + paxColumnSize
	rec0 := out[vbase:]
	if rec0[16] != byte(storage.MutationDeleteRange) {
		t.Fatalf("row 0 op = %d", rec0[16])
	}
	region := out[vbase : vbase+uint64(visLen)]
	ko := binary.LittleEndian.Uint32(rec0[24:])
	kl := binary.LittleEndian.Uint32(rec0[28:])
	eo := binary.LittleEndian.Uint32(rec0[32:])
	el := binary.LittleEndian.Uint32(rec0[36:])
	if got := string(region[ko : ko+kl]); got != "a" {
		t.Fatalf("range key = %q", got)
	}
	if got := string(region[eo : eo+el]); got != "c" {
		t.Fatalf("range end = %q", got)
	}
	rec1 := out[vbase+paxVersionSize:]
	if rec1[16] != byte(storage.MutationUpsert) {
		t.Fatalf("row 1 op = %d", rec1[16])
	}
	if el1 := binary.LittleEndian.Uint32(rec1[36:]); el1 != 0 {
		t.Fatalf("point row end length = %d", el1)
	}
}

func TestPAXEncodeNestedAlignment(t *testing.T) {
	intChild := model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
	strChild := model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString}}
	layouts := []model.TypeLayout{
		{
			Type:     model.Type{ID: 1007, Storage: model.TypeArray},
			Children: []model.TypeLayout{intChild},
		},
		{
			Type:     model.Type{ID: 2249, Storage: model.TypeComposite},
			Children: []model.TypeLayout{intChild, strChild},
		},
		{Type: model.Type{ID: 143, Storage: model.TypeVectorF32}},
	}
	arrVal := model.Value{
		Type:       model.TypeArray,
		Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}},
		ArrayVal:   []model.Value{model.Int64Val(1), model.Int64Val(2)},
	}
	emptyArr := model.Value{Type: model.TypeArray, ArrayVal: nil}
	compVal := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.Int64Val(7), model.StringVal("s")}}
	compNullField := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.NullVal(model.TypeInt64), model.StringVal("z")}}
	batch, done := encTestBuildBatch(t, layouts, [][]model.Value{
		{arrVal, compVal, {Type: model.TypeVectorF32, VectorVal: []float32{1, 2}}},
		{model.NullVal(model.TypeArray), model.NullVal(model.TypeComposite), model.NullVal(model.TypeVectorF32)},
		{emptyArr, compNullField, {Type: model.TypeVectorF32, VectorVal: nil}},
	})
	defer done()
	keys := encTestKeyVector("a", "b", "c")
	versions := encTestVersions(keys,
		[]model.Timestamp{1, 2, 3},
		[]storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert, storage.MutationUpsert})
	out, err := encodeBlock(context.Background(), encTestTarget(), versions, &batch, []model.ColumnID{1, 2, 3}, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	encTestHeader(t, out, 3, 3)
	// Every root payload is 8-aligned, non-overlapping, and inside the block.
	type span struct{ off, ln uint64 }
	var spans []span
	for i := range 3 {
		d := encTestDescriptor(out, i)
		off := binary.LittleEndian.Uint64(d[32:])
		ln := binary.LittleEndian.Uint64(d[40:])
		if off%8 != 0 {
			t.Fatalf("root %d offset %d unaligned", i, off)
		}
		if off+ln > uint64(len(out)) {
			t.Fatalf("root %d exceeds block", i)
		}
		spans = append(spans, span{off, ln})
	}
	for i := range spans {
		for j := i + 1; j < len(spans); j++ {
			a, b := spans[i], spans[j]
			if a.off < b.off+b.ln && b.off < a.off+a.ln {
				t.Fatalf("roots %d and %d overlap", i, j)
			}
		}
	}
	// Array root: child count 1, nested descriptor + payload inside parent.
	adesc := encTestDescriptor(out, 0)
	if got := binary.LittleEndian.Uint32(adesc[28:]); got != 1 {
		t.Fatalf("array childCount = %d", got)
	}
	aoff := binary.LittleEndian.Uint64(adesc[32:])
	alen := binary.LittleEndian.Uint64(adesc[40:])
	apay := out[aoff : aoff+alen]
	// value ranges (3*8) + shape ranges (3*8) + dim count (4) + 1 dim (8) = 60.
	if got := binary.LittleEndian.Uint32(apay[1+48:]); got != 1 {
		t.Fatalf("array dim count = %d", got)
	}
	// Composite root: two children, each covering all 3 parent rows.
	cdesc := encTestDescriptor(out, 1)
	if got := binary.LittleEndian.Uint32(cdesc[28:]); got != 2 {
		t.Fatalf("composite childCount = %d", got)
	}
	coff := binary.LittleEndian.Uint64(cdesc[32:])
	clen := binary.LittleEndian.Uint64(cdesc[40:])
	if coff+clen > uint64(len(out)) {
		t.Fatalf("composite payload exceeds block")
	}
	// VectorF32 root: spine + count + pool.
	vdesc := encTestDescriptor(out, 2)
	voff := binary.LittleEndian.Uint64(vdesc[32:])
	vlen := binary.LittleEndian.Uint64(vdesc[40:])
	vpay := out[voff : voff+vlen]
	// Skip bitmap (row 1 NULL).
	vbody := vpay[1:]
	if got := binary.LittleEndian.Uint32(vbody[24:]); got != 2 {
		t.Fatalf("f32 element count = %d", got)
	}
	if got := math.Float32frombits(binary.LittleEndian.Uint32(vbody[28:])); got != 1 {
		t.Fatalf("f32 element 0 = %v", got)
	}
}

func TestPAXEncodeRejects(t *testing.T) {
	boolLayout := []model.TypeLayout{
		{Type: model.Type{ID: 20, Storage: model.TypeInt64}},
		{Type: model.Type{ID: 16, Storage: model.TypeBoolean}},
	}
	build := func(t *testing.T, layouts []model.TypeLayout, rows [][]model.Value) columnar.Batch {
		t.Helper()
		b, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: 8, MaxBytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := b.Close(); err != nil {
				t.Error(err)
			}
		})
		for _, row := range rows {
			if err := b.Append(context.Background(), row); err != nil {
				t.Fatal(err)
			}
		}
		var out columnar.Batch
		if err := b.Publish(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	validBatch := func(t *testing.T) columnar.Batch {
		t.Helper()
		return build(t, boolLayout, [][]model.Value{
			{model.Int64Val(1), model.BoolVal(true)},
			{model.Int64Val(2), model.BoolVal(false)},
		})
	}
	validVersions := func() blockVersions {
		keys := encTestKeyVector("a", "b")
		return encTestVersions(keys,
			[]model.Timestamp{1, 2},
			[]storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert})
	}
	cases := map[string]func(t *testing.T) ([]byte, error){
		"nil batch": func(t *testing.T) ([]byte, error) {
			return encodeBlock(context.Background(), encTestTarget(), validVersions(), nil, []model.ColumnID{1, 2}, encTestLimits())
		},
		"empty batch": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			b.Length = 0
			for i := range b.Columns {
				b.Columns[i].Length = 0
			}
			return encodeBlock(context.Background(), encTestTarget(), blockVersions{}, &b, []model.ColumnID{1, 2}, encTestLimits())
		},
		"selection": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			b.Selection = []uint32{0}
			return encodeBlock(context.Background(), encTestTarget(), validVersions(), &b, []model.ColumnID{1, 2}, encTestLimits())
		},
		"column mismatch": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			return encodeBlock(context.Background(), encTestTarget(), validVersions(), &b, []model.ColumnID{1}, encTestLimits())
		},
		"zero column id": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			return encodeBlock(context.Background(), encTestTarget(), validVersions(), &b, []model.ColumnID{0, 2}, encTestLimits())
		},
		"unordered ids": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			return encodeBlock(context.Background(), encTestTarget(), validVersions(), &b, []model.ColumnID{2, 1}, encTestLimits())
		},
		"bad target": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			return encodeBlock(context.Background(), storage.ObjectRef{}, validVersions(), &b, []model.ColumnID{1, 2}, encTestLimits())
		},
		"version length": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			v := validVersions()
			v.CommitTS = v.CommitTS[:1]
			return encodeBlock(context.Background(), encTestTarget(), v, &b, []model.ColumnID{1, 2}, encTestLimits())
		},
		"zero ordinal": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			v := validVersions()
			v.Ordinals[0] = 0
			return encodeBlock(context.Background(), encTestTarget(), v, &b, []model.ColumnID{1, 2}, encTestLimits())
		},
		"zero timestamp": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			v := validVersions()
			v.CommitTS[0] = 0
			return encodeBlock(context.Background(), encTestTarget(), v, &b, []model.ColumnID{1, 2}, encTestLimits())
		},
		"unnormalized op": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			v := validVersions()
			v.Kinds[0] = storage.MutationInsert
			return encodeBlock(context.Background(), encTestTarget(), v, &b, []model.ColumnID{1, 2}, encTestLimits())
		},
		"point with end": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			v := validVersions()
			v.EndKeys = encTestKeyVector("e1", "e2")
			return encodeBlock(context.Background(), encTestTarget(), v, &b, []model.ColumnID{1, 2}, encTestLimits())
		},
		"range empty end": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			v := validVersions()
			v.Kinds[0] = storage.MutationDeleteRange
			v.EndKeys = encTestKeyVector("", "")
			return encodeBlock(context.Background(), encTestTarget(), v, &b, []model.ColumnID{1, 2}, encTestLimits())
		},
		"unordered keys": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			keys := encTestKeyVector("b", "a")
			v := encTestVersions(keys,
				[]model.Timestamp{1, 2},
				[]storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert})
			return encodeBlock(context.Background(), encTestTarget(), v, &b, []model.ColumnID{1, 2}, encTestLimits())
		},
		"duplicate version": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			keys := encTestKeyVector("a", "a")
			v := encTestVersions(keys,
				[]model.Timestamp{1, 1},
				[]storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert})
			v.Ordinals[1] = v.Ordinals[0]
			return encodeBlock(context.Background(), encTestTarget(), v, &b, []model.ColumnID{1, 2}, encTestLimits())
		},
		"non-flat vector": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			b.Columns[0].Encoding = columnar.Constant
			return encodeBlock(context.Background(), encTestTarget(), validVersions(), &b, []model.ColumnID{1, 2}, encTestLimits())
		},
		"bad bool lane": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			b.Columns[1].Int64[0] = 2
			return encodeBlock(context.Background(), encTestTarget(), validVersions(), &b, []model.ColumnID{1, 2}, encTestLimits())
		},
		"max rows": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			lim := encTestLimits()
			lim.MaxRows = 1
			return encodeBlock(context.Background(), encTestTarget(), validVersions(), &b, []model.ColumnID{1, 2}, lim)
		},
		"max bytes": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			lim := encTestLimits()
			lim.MaxBytes = paxHeaderSize
			return encodeBlock(context.Background(), encTestTarget(), validVersions(), &b, []model.ColumnID{1, 2}, lim)
		},
		"tiny temp budget": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			lim := encTestLimits()
			lim.MaxDecodedBytes = 8
			return encodeBlock(context.Background(), encTestTarget(), validVersions(), &b, []model.ColumnID{1, 2}, lim)
		},
		"cancelled": func(t *testing.T) ([]byte, error) {
			b := validBatch(t)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return encodeBlock(ctx, encTestTarget(), validVersions(), &b, []model.ColumnID{1, 2}, encTestLimits())
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := fn(t)
			if err == nil {
				t.Fatalf("expected error, got %d bytes", len(out))
			}
			if out != nil {
				t.Fatalf("expected nil output on error, got %d bytes", len(out))
			}
			if name == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled err = %v", err)
				}
				return
			}
			if name == "max rows" || name == "max bytes" || name == "tiny temp budget" {
				if !errors.Is(err, columnar.ErrBatchBudget) {
					t.Fatalf("budget err = %v", err)
				}
				return
			}
			if !errors.Is(err, errPAXEncode) {
				t.Fatalf("err = %v, want errPAXEncode", err)
			}
		})
	}
}

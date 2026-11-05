package copy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

var (
	copyTestText = model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString}}
	copyTestInt4 = model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
)

func copyTestSource(t *testing.T, data io.Reader, columns []Column, options Options, report func(context.Context, Diagnostic)) builtin.RowSourceIterator {
	t.Helper()
	plan, err := NewPlan(From, columns, options)
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewSource(t.Context(), plan, &Streams{Reader: data, MaxRecordBytes: 4096, Report: report}, builtin.ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	iterator, err := source.Open(t.Context(), nil, builtin.RowSourceOptions{MaxRowBytes: 4096, MaxMemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := iterator.Close(); err != nil {
			t.Error(err)
		}
	})
	return iterator
}

func copyTestNext(t *testing.T, iterator builtin.RowSourceIterator) []model.Value {
	t.Helper()
	row, ok, err := iterator.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("COPY source ended before the expected row")
	}
	result := make([]model.Value, len(row))
	for i := range row {
		result[i] = row[i].Clone()
	}
	return result
}

func TestTextAndCSVPreserveNullEmptyAndEscapedRecords(t *testing.T) {
	columns := []Column{{Name: "a", Layout: copyTestText, Nullable: true}, {Name: "b", Layout: copyTestText, Nullable: true}}
	text := copyTestSource(t, bytes.NewBufferString("\\N\t\n\\\\N\tleft\\\tright\n"), columns, Options{Format: Text, Delimiter: '\t', Null: `\N`}, nil)
	first := copyTestNext(t, text)
	if !first[0].IsNull || first[1].IsNull || first[1].StrVal != "" {
		t.Fatalf("text NULL/empty row = %#v", first)
	}
	second := copyTestNext(t, text)
	if second[0].IsNull || second[0].StrVal != `\N` || second[1].StrVal != "left\tright" {
		t.Fatalf("text escaped row = %#v", second)
	}

	csvOptions := Options{Format: CSV, Delimiter: ',', Null: "", Quote: '"', Escape: '"'}
	csv := copyTestSource(t, bytes.NewBufferString(",x\n\"\",\"left,right\nnext\"\n"), columns, csvOptions, nil)
	first = copyTestNext(t, csv)
	if !first[0].IsNull || first[1].StrVal != "x" {
		t.Fatalf("CSV unquoted NULL row = %#v", first)
	}
	second = copyTestNext(t, csv)
	if second[0].IsNull || second[0].StrVal != "" || second[1].StrVal != "left,right\nnext" {
		t.Fatalf("CSV quoted row = %#v", second)
	}
}

// Text input that ends without the \. terminator ends cleanly: the final
// record is yielded and the next call reports end of stream, not io.EOF.
func TestTextEndOfInputWithoutTerminator(t *testing.T) {
	columns := []Column{{Name: "v", Layout: copyTestText, Nullable: true}}
	for _, input := range []string{"one\n", "one"} {
		iterator := copyTestSource(t, bytes.NewBufferString(input), columns, Options{Format: Text, Delimiter: '\t', Null: `\N`}, nil)
		if row := copyTestNext(t, iterator); row[0].StrVal != "one" {
			t.Fatalf("%q: row = %#v", input, row)
		}
		if _, ok, err := iterator.Next(t.Context()); ok || err != nil {
			t.Fatalf("%q: after last record ok=%v err=%v, want end of stream", input, ok, err)
		}
	}
}

func TestDefaultMarkerAndIgnoredConversionClass(t *testing.T) {
	columns := []Column{{Name: "v", Layout: copyTestInt4, Nullable: true}}
	var diagnostics []Diagnostic
	options := Options{Format: Text, Delimiter: '\t', Null: `\N`, Default: "default", HasDefault: true, ErrorMode: IgnoreConversionErrors, RejectLimit: 2}
	iterator := copyTestSource(t, bytes.NewBufferString("bad\ndefault\n7\n"), columns, options, func(_ context.Context, diagnostic Diagnostic) {
		diagnostics = append(diagnostics, diagnostic)
	})
	row := copyTestNext(t, iterator)
	if !row[0].IsNull || row[1].IsNull || row[1].IntVal == 0 {
		t.Fatalf("default marker row = %#v", row)
	}
	row = copyTestNext(t, iterator)
	if row[0].IsNull || row[0].IntVal != 7 || row[1].IntVal != 0 {
		t.Fatalf("converted row = %#v", row)
	}
	if len(diagnostics) != 1 || diagnostics[0].Row != 1 || diagnostics[0].Column != "v" {
		t.Fatalf("conversion diagnostics = %#v", diagnostics)
	}
	if ignorableConversion(&model.Error{Code: "54000", Message: "budget"}) || ignorableConversion(context.Canceled) || !ignorableConversion(&model.Error{Code: "22P02", Message: "syntax"}) {
		t.Fatal("ON_ERROR conversion-class filtering changed")
	}
}

func TestBinaryRejectsMalformedAndTruncatedFrames(t *testing.T) {
	columns := []Column{{Name: "v", Layout: copyTestInt4, Nullable: true}}
	header := append([]byte(nil), binarySignature[:]...)
	header = binary.BigEndian.AppendUint32(header, 0)
	header = binary.BigEndian.AppendUint32(header, 0)
	truncatedField := append([]byte(nil), header...)
	truncatedField = binary.BigEndian.AppendUint16(truncatedField, 1)
	truncatedField = binary.BigEndian.AppendUint32(truncatedField, 4)
	truncatedField = append(truncatedField, 0, 1)
	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "header", data: []byte("PGCOPY")},
		{name: "field", data: truncatedField},
	} {
		t.Run(test.name, func(t *testing.T) {
			iterator := copyTestSource(t, bytes.NewReader(test.data), columns, Options{Format: Binary}, nil)
			if _, _, err := iterator.Next(t.Context()); err == nil {
				t.Fatal("malformed binary COPY frame succeeded")
			}
		})
	}

	missingTrailer := append([]byte(nil), header...)
	missingTrailer = binary.BigEndian.AppendUint16(missingTrailer, 1)
	missingTrailer = binary.BigEndian.AppendUint32(missingTrailer, 4)
	missingTrailer = binary.BigEndian.AppendUint32(missingTrailer, 7)
	iterator := copyTestSource(t, bytes.NewReader(missingTrailer), columns, Options{Format: Binary}, nil)
	if row := copyTestNext(t, iterator); row[0].IntVal != 7 {
		t.Fatalf("binary row = %#v", row)
	}
	if _, _, err := iterator.Next(t.Context()); err == nil {
		t.Fatal("binary COPY without trailer succeeded")
	}
}

var errCopyTestRead = errors.New("COPY read failed")

type copyTestErrorReader struct{ sent bool }

func (reader *copyTestErrorReader) Read(buffer []byte) (int, error) {
	if !reader.sent {
		reader.sent = true
		return copy(buffer, "ok\n"), nil
	}
	return 0, errCopyTestRead
}

type copyTestShortWriter struct{}

func (copyTestShortWriter) Write([]byte) (int, error) { return 0, nil }

func TestStreamErrorsPropagateWithoutFalseCompletion(t *testing.T) {
	iterator := copyTestSource(t, &copyTestErrorReader{}, []Column{{Name: "v", Layout: copyTestText}}, Options{Format: Text, Delimiter: '\t', Null: `\N`}, nil)
	if row := copyTestNext(t, iterator); row[0].StrVal != "ok" {
		t.Fatalf("row before read error = %#v", row)
	}
	if _, _, err := iterator.Next(t.Context()); !errors.Is(err, errCopyTestRead) {
		t.Fatalf("read error = %v", err)
	}
	if err := writeAll(t.Context(), copyTestShortWriter{}, []byte("record")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error = %v", err)
	}
}

func TestTextAndCSVWritersProtectDataThatLooksLikeMarkers(t *testing.T) {
	textPlan, err := NewPlan(To, []Column{{Name: "v", Layout: copyTestText}}, Options{Format: Text, Delimiter: '\t', Null: "NULL"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := (&Writer{plan: textPlan, maximum: 4096}).encodeStrings(t.Context(), []string{"NULL"}, []bool{false})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `\116ULL`+"\n" {
		t.Fatalf("protected text marker = %q", encoded)
	}
	csvPlan, err := NewPlan(To, []Column{{Name: "v", Layout: copyTestText}}, Options{Format: CSV, Delimiter: ',', Null: "", Quote: '"', Escape: '"'})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = (&Writer{plan: csvPlan, maximum: 4096}).encodeStrings(t.Context(), []string{"left,right\nnext"}, []bool{false})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "\"left,right\nnext\"\n" {
		t.Fatalf("quoted CSV field = %q", encoded)
	}
}

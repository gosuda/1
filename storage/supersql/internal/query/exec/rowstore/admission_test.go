package rowstore

import (
	"errors"
	"io"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestClosingReaderReturnsAdmissionWithoutClosingSpool(t *testing.T) {
	q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{MaxReaders: 1})
	limits := testLimits()
	limits.MemoryBytes = 0
	s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, limits)
	if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(19)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Seal(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := openReader(t, s)
	before := q.Usage()
	if _, err := s.Reader(t.Context()); !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("query reader cap = %v", err)
	}
	if after := q.Usage(); after.Work.Live != before.Work.Live || after.Work.Reservations != before.Work.Reservations || after.Readers != before.Readers {
		t.Fatalf("failed reader admission leaked: %+v -> %+v", before, after)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if q.Usage().Readers != 0 || q.Usage().Spill.Live != 57 {
		t.Fatalf("early close ownership = %+v", q.Usage())
	}
	second := openReader(t, s)
	p, row, err := second.NextRow(t.Context())
	if err != nil || p != 0 || row[0].IntVal != 19 {
		t.Fatalf("replacement reader = %d, %v, %v", p, row, err)
	}
}

func TestEmptySealHasReadableEmptyExtent(t *testing.T) {
	q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{})
	s := newSpool(t, q, nil, testLimits())
	if err := s.Seal(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Seal(t.Context()); err != nil {
		t.Fatal(err)
	}
	r := openReader(t, s)
	if _, _, err := r.NextRow(t.Context()); !errors.Is(err, io.EOF) {
		t.Fatalf("empty prefix = %v", err)
	}
	if _, err := r.Read(t.Context(), 0); !errors.Is(err, ErrPosition) {
		t.Fatalf("empty random read = %v", err)
	}
	if q.Usage().Files != 0 || s.Bytes() != 0 || s.Rows() != 0 {
		t.Fatalf("empty spool owns extent: %+v", q.Usage())
	}
}

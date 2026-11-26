package window_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/window"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestUnsupportedFunctionRejectsBeforeReading(t *testing.T) {
	q, dir := query(t, false)
	o := options(t, q, []model.TypeLayout{int8}, []model.TypeLayout{int8}, []window.Call{{Function: 999999}})
	o.OrderBy = []window.OrderKey{{Column: 0}}
	s := newSource(t, o.InputLayouts, [][]model.Value{{iv(1)}})
	closeErr := errors.New("source close failed")
	s.closeErr = closeErr
	_, err := window.New(t.Context(), s, o)
	var unsupported *window.UnsupportedError
	if !errors.As(err, &unsupported) || !errors.Is(err, closeErr) {
		t.Fatalf("error=%v, want unsupported and source close failure", err)
	}
	if s.pulls != 0 || !s.closed {
		t.Fatalf("input pulls=%d closed=%v", s.pulls, s.closed)
	}
	released(t, q, dir)
}

func TestInvalidContractsRejectBeforeReading(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*window.Options)
	}{
		{name: "wrong result type", change: func(o *window.Options) { o.OutputLayouts[0] = float8 }},
		{name: "argument count", change: func(o *window.Options) { o.Calls[0].Arguments = []int{0} }},
		{name: "unbounded following start", change: func(o *window.Options) { o.Frame.Start.Kind = ir.UnboundedFollowing }},
		{name: "groups without order", change: func(o *window.Options) { o.Frame.Mode = ir.GroupsFrame }},
		{name: "missing frame offset", change: func(o *window.Options) { o.Frame.Mode = ir.RowsFrame; o.Frame.Start.Kind = ir.OffsetPreceding }},
		{name: "negative frame offset", change: func(o *window.Options) {
			o.Frame.Mode = ir.RowsFrame
			o.Frame.Start.Kind = ir.OffsetPreceding
			value := iv(-1)
			o.StartOffset = &value
		}},
		{name: "null frame offset", change: func(o *window.Options) {
			o.Frame.Mode = ir.RowsFrame
			o.Frame.Start.Kind = ir.OffsetPreceding
			value := null()
			o.StartOffset = &value
		}},
		{name: "exclusion unknown", change: func(o *window.Options) { o.Frame.Exclusion = 99 }},
		{name: "input projection column", change: func(o *window.Options) { o.OutputColumns = []int{3}; o.OutputLayouts = append(o.OutputLayouts, int8) }},
		{name: "partition column", change: func(o *window.Options) { o.PartitionColumns = []int{-1} }},
		{name: "work admission", change: func(o *window.Options) { o.WorkBytes = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, dir := query(t, false)
			o := options(t, q, []model.TypeLayout{int8}, []model.TypeLayout{int8}, []window.Call{{Function: 3100}})
			tc.change(&o)
			s := newSource(t, o.InputLayouts, [][]model.Value{{iv(1)}})
			if _, err := window.New(t.Context(), s, o); err == nil {
				t.Fatal("invalid contract accepted")
			}
			if s.pulls != 0 || !s.closed {
				t.Fatalf("input pulls=%d closed=%v", s.pulls, s.closed)
			}
			released(t, q, dir)
		})
	}
}

func TestRuntimeArgumentErrorsReleasePartition(t *testing.T) {
	for _, tc := range []struct {
		name     string
		function model.FunctionID
		args     []int
		row      []model.Value
		code     string
	}{
		{name: "ntile zero", function: 3105, args: []int{1}, row: []model.Value{iv(1), iv(0)}, code: "22014"},
		{name: "nth zero", function: 3114, args: []int{0, 1}, row: []model.Value{iv(1), iv(0)}, code: "22016"},
		{name: "offset outside int4", function: 3107, args: []int{0, 1}, row: []model.Value{iv(1), iv(1 << 32)}, code: "22003"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, dir := query(t, false)
			result := int8
			if tc.function == 3105 {
				result = int4
			}
			o := options(t, q, []model.TypeLayout{int8, int4}, []model.TypeLayout{result}, []window.Call{{Function: tc.function, Arguments: tc.args}})
			o.PartitionMemoryBytes = 0
			c := open(t, newSource(t, o.InputLayouts, [][]model.Value{tc.row}), o)
			_, err := drain(t, c)
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != tc.code {
				t.Fatalf("error=%v, want SQLSTATE %s", err, tc.code)
			}
			released(t, q, dir)
		})
	}
}

func TestPartitionLimitsAndSpillPolicyReleaseResources(t *testing.T) {
	for _, tc := range []struct {
		name     string
		disabled bool
		change   func(*window.Options)
		cause    error
	}{
		{name: "row limit", change: func(o *window.Options) { o.MaxPartitionRows = 2 }, cause: runtime.ErrBudget},
		{name: "combined partition bytes", change: func(o *window.Options) { o.MaxPartitionBytes = 300 }, cause: runtime.ErrBudget},
		{name: "spill disabled", disabled: true, change: func(o *window.Options) { o.PartitionMemoryBytes = 0 }, cause: runtime.ErrSpillDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, dir := query(t, tc.disabled)
			o := options(t, q, []model.TypeLayout{int8}, []model.TypeLayout{int8}, []window.Call{{Function: 3100}})
			tc.change(&o)
			s := newSource(t, o.InputLayouts, [][]model.Value{{iv(1)}, {iv(2)}, {iv(3)}})
			c := open(t, s, o)
			_, err := drain(t, c)
			if !errors.Is(err, tc.cause) {
				t.Fatalf("error=%v, want %v", err, tc.cause)
			}
			if !s.closed {
				t.Fatal("source remains open after limit")
			}
			released(t, q, dir)
		})
	}
}

func TestSkewedPartitionSpillsDirectoriesAndClosesEarly(t *testing.T) {
	q, dir := query(t, false)
	rows := make([][]model.Value, 1024)
	for i := range rows {
		rows[i] = []model.Value{iv(int64(i)), model.StringVal(strings.Repeat("x", i%80) + strconv.Itoa(i))}
	}
	o := options(t, q, []model.TypeLayout{int8, text}, []model.TypeLayout{text, text}, []window.Call{{Function: 3113, Arguments: []int{1}}})
	o.OutputColumns = []int{1}
	o.OrderBy = []window.OrderKey{{Column: 0}}
	o.Frame.End.Kind = ir.UnboundedFollowing
	o.PartitionMemoryBytes = 0
	s := newSource(t, o.InputLayouts, rows)
	c := open(t, s, o)
	out := columnar.Batch{MaxRows: 1}
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	want := []string{"0", strings.Repeat("x", 1023%80) + "1023"}
	for i := range out.Columns {
		v, _, err := columnar.CopyValue(t.Context(), &out.Columns[i], 0, 4096)
		if err != nil || v.StrVal != want[i] {
			t.Fatalf("column %d=%v error=%v, want %q", i, v, err, want[i])
		}
	}
	u := q.Usage()
	if u.Files != 3 || u.Readers != 3 || u.Spill.Live == 0 {
		t.Fatalf("partition/directories did not spill: %+v", u)
	}
	if uint64(u.Work.Live) > o.WorkBytes {
		t.Fatalf("work bytes=%d exceed operator cap=%d", u.Work.Live, o.WorkBytes)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !s.closed {
		t.Fatal("source remains open")
	}
	released(t, q, dir)
}

func TestCancellationClosesUnreadInput(t *testing.T) {
	q, dir := query(t, false)
	o := options(t, q, []model.TypeLayout{int8}, []model.TypeLayout{int8}, []window.Call{{Function: 3100}})
	s := newSource(t, o.InputLayouts, [][]model.Value{{iv(1)}})
	c := open(t, s, o)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Next(ctx, &columnar.Batch{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if s.pulls != 0 || !s.closed {
		t.Fatalf("input pulls=%d closed=%v", s.pulls, s.closed)
	}
	released(t, q, dir)
}

func TestConstructorCancellationReleasesSpilledReaders(t *testing.T) {
	q, dir := query(t, false)
	o := options(t, q, []model.TypeLayout{int8}, []model.TypeLayout{int8}, []window.Call{{Function: 3100}})
	o.PartitionMemoryBytes = 0
	ctx, cancel := context.WithCancel(t.Context())
	s := newSource(t, o.InputLayouts, [][]model.Value{{iv(1)}, {iv(2)}})
	c, err := window.New(ctx, s, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := c.Next(t.Context(), &columnar.Batch{MaxRows: 1}); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := c.Next(t.Context(), &columnar.Batch{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	released(t, q, dir)
}

func TestCorruptSpilledDirectoryFailsClosed(t *testing.T) {
	q, dir := query(t, false)
	o := options(t, q, []model.TypeLayout{int8}, []model.TypeLayout{int8}, []window.Call{{Function: 3100}})
	o.PartitionMemoryBytes = 0
	c := open(t, newSource(t, o.InputLayouts, [][]model.Value{{iv(1)}, {iv(2)}, {iv(3)}}), o)
	if err := c.Next(t.Context(), &columnar.Batch{MaxRows: 1}); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range files {
		file, err := os.OpenFile(filepath.Join(dir, entry.Name()), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		info, statErr := file.Stat()
		if statErr != nil {
			t.Fatal(errors.Join(statErr, file.Close()))
		}
		var b [1]byte
		_, readErr := file.ReadAt(b[:], info.Size()-1)
		b[0] ^= 0xff
		_, writeErr := file.WriteAt(b[:], info.Size()-1)
		if err := errors.Join(readErr, writeErr, file.Close()); err != nil {
			t.Fatal(err)
		}
	}
	_, err = drain(t, c)
	if err == nil || err == io.EOF {
		t.Fatalf("corruption error=%v", err)
	}
	released(t, q, dir)
}

func TestOversizedRecordReleasesInputAndSpools(t *testing.T) {
	q, dir := query(t, false)
	o := options(t, q, []model.TypeLayout{text}, []model.TypeLayout{text}, []window.Call{{Function: 3112, Arguments: []int{0}}})
	o.MaxRecordBytes = 128
	s := newSource(t, o.InputLayouts, [][]model.Value{{model.StringVal(strings.Repeat("x", 512))}})
	c := open(t, s, o)
	_, err := drain(t, c)
	if !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("error=%v, want resource limit", err)
	}
	if !s.closed {
		t.Fatal("oversized input remains open")
	}
	released(t, q, dir)
}

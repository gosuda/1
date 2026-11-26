package statistics

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const wireMagic uint64 = 0x5354415400000002

type wireWriter struct {
	ctx         context.Context
	data        []byte
	destination io.Writer
	used, limit uint64
	err         error
}

func (w *wireWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if err := w.ctx.Err(); err != nil {
		w.err = err
		return 0, err
	}
	if uint64(len(p)) > w.limit-w.used {
		w.err = ErrLimit
		return 0, w.err
	}
	if w.destination != nil {
		if _, err := w.destination.Write(p); err != nil {
			w.err = err
			return 0, err
		}
	}
	if w.data != nil {
		copy(w.data[w.used:], p)
	}
	w.used += uint64(len(p))
	return len(p), nil
}
func (w *wireWriter) number(n uint64) {
	var p [8]byte
	binary.BigEndian.PutUint64(p[:], n)
	_, w.err = w.Write(p[:])
}
func (w *wireWriter) layout(l model.TypeLayout) {
	w.number(uint64(l.Type.ID))
	w.number(uint64(uint32(l.Type.Modifier)))
	w.number(uint64(l.Type.Collation))
	w.number(uint64(l.Type.Storage))
	w.number(uint64(len(l.Children)))
	for _, child := range l.Children {
		w.layout(child)
	}
}
func (w *wireWriter) value(v model.Value) {
	if w.err != nil {
		return
	}
	size := wireWriter{ctx: w.ctx, limit: w.limit - w.used}
	if err := storage.EncodeValue(w.ctx, &size, v, size.limit); err != nil {
		w.err = err
		return
	}
	w.number(size.used)
	if w.err != nil {
		return
	}
	w.err = storage.EncodeValue(w.ctx, w, v, w.limit-w.used)
}
func (w *wireWriter) blob(data []byte) {
	w.number(uint64(len(data)))
	if w.err != nil {
		return
	}
	_, w.err = w.Write(data)
}
func (w *wireWriter) table(t *Table) {
	w.number(wireMagic)
	for _, n := range [...]uint64{uint64(t.Target.ObjectID), t.Target.Generation, t.Target.DescriptorVersion, uint64(t.Target.EncodingVersion), t.Epoch, uint64(t.Snapshot), t.Seed, t.SampleRows} {
		w.number(n)
	}
	for _, c := range [...]Count{t.LiveRows, t.DeadRows, t.HistoricalRows, t.Blocks, t.Files, t.DeltaBytes} {
		var known uint64
		if c.Known {
			known = 1
		}
		w.number(known)
		w.number(c.Value)
	}
	for _, f := range [...]float64{t.AverageRowBytes, t.VersionChainWork, t.Confidence} {
		w.number(math.Float64bits(f))
	}
	var workKnown uint64
	if t.VersionChainWorkKnown {
		workKnown = 1
	}
	w.number(workKnown)
	w.number(uint64(len(t.Columns)))
	for _, c := range t.Columns {
		w.number(uint64(c.ID))
		w.layout(c.Layout)
		for _, f := range [...]float64{c.NullFraction, c.Distinct, c.AverageBytes, c.Correlation} {
			w.number(math.Float64bits(f))
		}
		var correlationKnown uint64
		if c.CorrelationKnown {
			correlationKnown = 1
		}
		w.number(correlationKnown)
		w.number(uint64(len(c.MostCommon)))
		for _, f := range c.MostCommon {
			w.number(math.Float64bits(f.Fraction))
			w.value(f.Value)
		}
		w.number(uint64(len(c.Histogram)))
		for _, v := range c.Histogram {
			w.value(v)
		}
	}
	w.number(uint64(len(t.Extended)))
	for _, statistic := range t.Extended {
		w.number(uint64(statistic.OID))
		if statistic.Inherit {
			w.number(1)
		} else {
			w.number(0)
		}
		w.blob(statistic.NDistinct)
		w.blob(statistic.Dependencies)
		w.blob(statistic.MCV)
		if statistic.Expressions.Type == model.TypeAny {
			w.number(0)
		} else {
			w.number(1)
			w.value(statistic.Expressions)
		}
	}
}

// Encode preserves typed scalar bit patterns, including NaN payloads and signed
// zero. MaxBytes separately bounds the owned Table and the returned wire frame.
func Encode(ctx context.Context, t *Table, limits Limits) ([]byte, error) {
	if err := Validate(ctx, t, limits); err != nil {
		return nil, err
	}
	w := wireWriter{ctx: ctx, limit: limits.MaxBytes}
	w.table(t)
	if w.err != nil {
		return nil, w.err
	}
	w.data = make([]byte, int(w.used))
	w.used = 0
	w.table(t)
	if w.err != nil {
		return nil, w.err
	}
	return w.data, ctx.Err()
}

type wireReader struct {
	data   []byte
	offset int
	budget meter
	err    error
}

func (r *wireReader) number() uint64 {
	if r.err != nil {
		return 0
	}
	if err := r.budget.ctx.Err(); err != nil {
		r.err = err
		return 0
	}
	if len(r.data)-r.offset < 8 {
		r.err = io.ErrUnexpectedEOF
		return 0
	}
	n := binary.BigEndian.Uint64(r.data[r.offset:])
	r.offset += 8
	return n
}
func (r *wireReader) bounded(maximum uint64) uint64 {
	n := r.number()
	if n > maximum {
		r.err = ErrInvalid
		return 0
	}
	return n
}
func (r *wireReader) count(size uint64) int {
	n := r.bounded(uint64(r.budget.maxValues))
	if r.err == nil {
		r.err = r.budget.add(n, size)
	}
	if r.err != nil {
		return 0
	}
	return int(n)
}
func (r *wireReader) layout(depth int) model.TypeLayout {
	if r.err != nil {
		return model.TypeLayout{}
	}
	if r.err = r.budget.node(depth); r.err != nil {
		return model.TypeLayout{}
	}
	l := model.TypeLayout{Type: model.Type{ID: model.TypeID(r.bounded(math.MaxUint32)), Modifier: int32(uint32(r.bounded(math.MaxUint32))), Collation: model.CollationID(r.bounded(math.MaxUint32)), Storage: model.DataType(r.bounded(math.MaxUint8))}}
	n := r.count(uint64(unsafe.Sizeof(model.TypeLayout{})))
	if n != 0 {
		l.Children = make([]model.TypeLayout, n)
	}
	for i := range l.Children {
		l.Children[i] = r.layout(depth + 1)
	}
	return l
}
func (r *wireReader) value() model.Value {
	n := r.number()
	if r.err != nil {
		return model.Value{}
	}
	if n > uint64(len(r.data)-r.offset) {
		r.err = io.ErrUnexpectedEOF
		return model.Value{}
	}
	v, err := storage.DecodeValue(r.budget.ctx, r.data[r.offset:r.offset+int(n)], r.budget.limit-r.budget.used)
	if err != nil {
		r.err = err
		return model.Value{}
	}
	r.offset += int(n)
	r.err = r.budget.value(v, 0)
	return v
}
func (r *wireReader) blob() []byte {
	n := r.number()
	if r.err != nil {
		return nil
	}
	if n > uint64(len(r.data)-r.offset) {
		r.err = io.ErrUnexpectedEOF
		return nil
	}
	if r.err = r.budget.add(n, 1); r.err != nil {
		return nil
	}
	data := bytes.Clone(r.data[r.offset : r.offset+int(n)])
	r.offset += int(n)
	return data
}

func Decode(ctx context.Context, data []byte, limits Limits) (*Table, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if uint64(len(data)) > limits.MaxBytes {
		return nil, ErrLimit
	}
	r := wireReader{data: data, budget: meter{ctx: ctx, limit: limits.MaxBytes, maxValues: limits.MaxValues, maxDepth: limits.MaxDepth}}
	r.err = r.budget.add(1, uint64(unsafe.Sizeof(Table{})))
	magic := r.number()
	if r.err != nil {
		return nil, r.err
	}
	if magic != wireMagic {
		return nil, ErrInvalid
	}
	t := &Table{}
	t.Target = storage.ObjectRef{ObjectID: model.ObjectID(r.number()), Generation: r.number(), DescriptorVersion: r.number(), EncodingVersion: uint32(r.bounded(math.MaxUint32))}
	t.Epoch = r.number()
	t.Snapshot = model.Timestamp(r.bounded(math.MaxInt64))
	t.Seed = r.number()
	t.SampleRows = r.number()
	for _, c := range [...]*Count{&t.LiveRows, &t.DeadRows, &t.HistoricalRows, &t.Blocks, &t.Files, &t.DeltaBytes} {
		c.Known = r.bounded(1) == 1
		c.Value = r.number()
	}
	t.AverageRowBytes = math.Float64frombits(r.number())
	t.VersionChainWork = math.Float64frombits(r.number())
	t.Confidence = math.Float64frombits(r.number())
	t.VersionChainWorkKnown = r.bounded(1) == 1
	n := r.bounded(uint64(limits.MaxColumns))
	if r.err == nil {
		r.err = r.budget.add(n, uint64(unsafe.Sizeof(Column{})))
	}
	if r.err != nil {
		return nil, r.err
	}
	t.Columns = make([]Column, int(n))
	for i := range t.Columns {
		c := &t.Columns[i]
		c.ID = model.ColumnID(r.bounded(math.MaxUint32))
		c.Layout = r.layout(0)
		c.NullFraction = math.Float64frombits(r.number())
		c.Distinct = math.Float64frombits(r.number())
		c.AverageBytes = math.Float64frombits(r.number())
		c.Correlation = math.Float64frombits(r.number())
		c.CorrelationKnown = r.bounded(1) == 1
		c.MostCommon = make([]Frequency, r.count(uint64(unsafe.Sizeof(Frequency{}))))
		for j := range c.MostCommon {
			c.MostCommon[j].Fraction = math.Float64frombits(r.number())
			c.MostCommon[j].Value = r.value()
		}
		c.Histogram = make([]model.Value, r.count(uint64(unsafe.Sizeof(model.Value{}))))
		for j := range c.Histogram {
			c.Histogram[j] = r.value()
		}
		if r.err != nil {
			return nil, r.err
		}
	}
	t.Extended = make([]ExtendedStatistic, r.count(uint64(unsafe.Sizeof(ExtendedStatistic{}))))
	for i := range t.Extended {
		statistic := &t.Extended[i]
		statistic.OID = model.OID(r.bounded(math.MaxUint32))
		statistic.Inherit = r.bounded(1) == 1
		statistic.NDistinct = r.blob()
		statistic.Dependencies = r.blob()
		statistic.MCV = r.blob()
		if r.bounded(1) == 1 {
			statistic.Expressions = r.value()
		}
		if r.err != nil {
			return nil, r.err
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.offset != len(data) {
		return nil, ErrInvalid
	}
	if err := Validate(ctx, t, limits); err != nil {
		return nil, err
	}
	// Comparing a streaming re-encoding rejects noncanonical value spellings
	// without retaining a second frame beside the decoded result.
	check := canonicalWriter{remaining: data}
	w := wireWriter{ctx: ctx, limit: limits.MaxBytes, destination: &check}
	w.table(t)
	if w.err != nil {
		return nil, w.err
	}
	if len(check.remaining) != 0 {
		return nil, ErrInvalid
	}
	return t, ctx.Err()
}

type canonicalWriter struct{ remaining []byte }

func (w *canonicalWriter) Write(p []byte) (int, error) {
	if len(p) > len(w.remaining) || !bytes.Equal(p, w.remaining[:len(p)]) {
		return 0, ErrInvalid
	}
	w.remaining = w.remaining[len(p):]
	return len(p), nil
}

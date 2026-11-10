package statistics

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	ErrInvalid = errors.New("invalid statistics")
	ErrLimit   = errors.New("statistics resource limit exceeded")
	ErrStale   = errors.New("stale statistics identity or epoch")
)

const EncodingVersion uint32 = 2

type Count struct {
	Value uint64
	Known bool
}

type Frequency struct {
	Value    model.Value
	Fraction float64
}

type Column struct {
	ID           model.ColumnID
	Layout       model.TypeLayout
	NullFraction float64
	Distinct     float64
	AverageBytes float64
	MostCommon   []Frequency
	Histogram    []model.Value
	// Correlation is the sample's rank correlation with forward scan order.
	Correlation      float64
	CorrelationKnown bool
}
type ExtendedStatistic struct {
	OID          model.OID
	Inherit      bool
	NDistinct    []byte
	Dependencies []byte
	MCV          []byte
	Expressions  model.Value
}

// Table and every nested slice/value belong to the recipient. Unknown counters
// have Known=false and Value=0; a measured empty table has LiveRows.Known=true.
type Table struct {
	Target                                                        storage.ObjectRef
	Epoch                                                         uint64
	Snapshot                                                      model.Timestamp
	Seed                                                          uint64
	SampleRows                                                    uint64
	LiveRows, DeadRows, HistoricalRows, Blocks, Files, DeltaBytes Count
	AverageRowBytes, VersionChainWork                             float64
	VersionChainWorkKnown                                         bool
	// Confidence is sampled/live coverage, not a statistical confidence interval.
	Confidence float64
	Columns    []Column
	Extended   []ExtendedStatistic
}

type Limits struct {
	MaxBytes                        uint64
	MaxColumns, MaxValues, MaxDepth int
}

func (l Limits) Validate() error {
	invalidBytes := l.MaxBytes == 0 || l.MaxBytes > uint64(math.MaxInt)
	invalidCounts := l.MaxColumns <= 0 || l.MaxValues <= 0
	if invalidBytes || invalidCounts || l.MaxDepth <= 0 || l.MaxDepth > 64 {
		return ErrLimit
	}
	return nil
}

type meter struct {
	ctx                         context.Context
	used, limit                 uint64
	values, maxValues, maxDepth int
}

func (m *meter) add(count, size uint64) error {
	if err := m.ctx.Err(); err != nil {
		return err
	}
	if size != 0 && count > (m.limit-m.used)/size {
		return ErrLimit
	}
	m.used += count * size
	return nil
}

func (m *meter) node(depth int) error {
	if err := m.ctx.Err(); err != nil {
		return err
	}
	if depth > m.maxDepth || m.values >= m.maxValues {
		return ErrLimit
	}
	m.values++
	return nil
}

func (m *meter) layout(l model.TypeLayout, depth int) error {
	if err := m.node(depth); err != nil {
		return err
	}
	if err := m.add(uint64(cap(l.Children)), uint64(unsafe.Sizeof(model.TypeLayout{}))); err != nil {
		return err
	}
	if err := m.add(uint64(cap(l.FieldNames)), uint64(unsafe.Sizeof(""))); err != nil {
		return err
	}
	for _, name := range l.FieldNames[:cap(l.FieldNames)] {
		if err := m.add(uint64(len(name)), 1); err != nil {
			return err
		}
	}
	for _, child := range l.Children {
		if err := m.layout(child, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (m *meter) value(v model.Value, depth int) error {
	if err := m.node(depth); err != nil {
		return err
	}
	invalidBoolean := v.Type == model.TypeBoolean && v.IntVal != 0 && v.IntVal != 1
	if !v.Type.Valid() || (!v.IsNull && invalidBoolean) {
		return ErrInvalid
	}
	for _, a := range [...]struct{ n, size uint64 }{
		{uint64(len(v.StrVal)), 1}, {uint64(cap(v.BytesVal)), 1}, {uint64(cap(v.VectorVal)), 4},
		{uint64(cap(v.Dimensions)), uint64(unsafe.Sizeof(model.ArrayDimension{}))},
		{uint64(cap(v.ArrayVal)), uint64(unsafe.Sizeof(model.Value{}))},
	} {
		if err := m.add(a.n, a.size); err != nil {
			return err
		}
	}
	for _, child := range v.ArrayVal {
		if err := m.value(child, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func measure(ctx context.Context, t *Table, l Limits) (uint64, error) {
	if t == nil {
		return 0, ErrInvalid
	}
	m := meter{ctx: ctx, limit: l.MaxBytes, maxValues: l.MaxValues, maxDepth: l.MaxDepth}
	if err := m.add(1, uint64(unsafe.Sizeof(Table{}))); err != nil {
		return 0, err
	}
	if len(t.Columns) > l.MaxColumns {
		return 0, ErrLimit
	}
	if err := m.add(uint64(cap(t.Columns)), uint64(unsafe.Sizeof(Column{}))); err != nil {
		return 0, err
	}
	for _, c := range t.Columns {
		if err := m.layout(c.Layout, 0); err != nil {
			return 0, err
		}
		if err := m.add(uint64(cap(c.MostCommon)), uint64(unsafe.Sizeof(Frequency{}))); err != nil {
			return 0, err
		}
		if err := m.add(uint64(cap(c.Histogram)), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
			return 0, err
		}
		for _, f := range c.MostCommon {
			if err := m.value(f.Value, 0); err != nil {
				return 0, err
			}
		}
		for _, v := range c.Histogram {
			if err := m.value(v, 0); err != nil {
				return 0, err
			}
		}
	}
	if err := m.add(uint64(cap(t.Extended)), uint64(unsafe.Sizeof(ExtendedStatistic{}))); err != nil {
		return 0, err
	}
	for _, statistic := range t.Extended {
		for _, data := range [...][]byte{statistic.NDistinct, statistic.Dependencies, statistic.MCV} {
			if err := m.add(uint64(cap(data)), 1); err != nil {
				return 0, err
			}
		}
		if statistic.Expressions.Type != model.TypeAny {
			if err := m.value(statistic.Expressions, 0); err != nil {
				return 0, err
			}
		}
	}
	return m.used, nil
}

// OwnedBytes counts retained capacities as well as visible nested payloads.
func OwnedBytes(ctx context.Context, t *Table, maxBytes uint64) (uint64, error) {
	return measure(ctx, t, Limits{MaxBytes: maxBytes, MaxColumns: math.MaxInt, MaxValues: math.MaxInt, MaxDepth: 64})
}

func finiteNonnegative(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && !math.Signbit(v)
}
func fraction(v float64) bool { return finiteNonnegative(v) && v <= 1 }

func Validate(ctx context.Context, t *Table, l Limits) error {
	if err := l.Validate(); err != nil {
		return err
	}
	if _, err := measure(ctx, t, l); err != nil {
		return err
	}
	if err := t.Target.Validate(); err != nil {
		return err
	}
	if t.Epoch == 0 || t.Snapshot <= 0 || !fraction(t.Confidence) || !finiteNonnegative(t.AverageRowBytes) || !finiteNonnegative(t.VersionChainWork) {
		return ErrInvalid
	}
	for _, count := range [...]Count{t.LiveRows, t.DeadRows, t.HistoricalRows, t.Blocks, t.Files, t.DeltaBytes} {
		if !count.Known && count.Value != 0 {
			return ErrInvalid
		}
	}
	if t.LiveRows.Known && t.SampleRows > t.LiveRows.Value {
		return ErrInvalid
	}
	if !t.VersionChainWorkKnown && t.VersionChainWork != 0 {
		return ErrInvalid
	}
	if t.SampleRows == 0 && t.AverageRowBytes != 0 {
		return ErrInvalid
	}
	for i, c := range t.Columns {
		if err := ctx.Err(); err != nil {
			return err
		}
		unordered := i > 0 && t.Columns[i-1].ID >= c.ID
		invalidMoments := !fraction(c.NullFraction) || !finiteNonnegative(c.Distinct) || !finiteNonnegative(c.AverageBytes)
		invalidCorrelation := math.IsNaN(c.Correlation) || math.IsInf(c.Correlation, 0) || c.Correlation < -1 || c.Correlation > 1 || !c.CorrelationKnown && c.Correlation != 0 || c.CorrelationKnown && t.SampleRows < 2
		if c.ID == 0 || unordered || invalidMoments || invalidCorrelation {
			return ErrInvalid
		}
		if err := c.Layout.Validate(); err != nil {
			return err
		}
		if t.LiveRows.Known && c.Distinct > float64(t.LiveRows.Value) {
			return ErrInvalid
		}
		var sum float64
		for _, f := range c.MostCommon {
			if !fraction(f.Fraction) || f.Fraction == 0 || f.Value.IsNull {
				return ErrInvalid
			}
			sum += f.Fraction
			if err := storage.ValidateStoredValue(ctx, f.Value, c.Layout); err != nil {
				return err
			}
		}
		if sum > 1-c.NullFraction+1e-12 {
			return ErrInvalid
		}
		for _, v := range c.Histogram {
			if v.IsNull {
				return ErrInvalid
			}
			if err := storage.ValidateStoredValue(ctx, v, c.Layout); err != nil {
				return err
			}
		}
		hasMoments := c.NullFraction != 0 || c.Distinct != 0 || c.AverageBytes != 0 || c.CorrelationKnown
		hasValues := len(c.MostCommon) != 0 || len(c.Histogram) != 0
		if t.SampleRows == 0 && (hasMoments || hasValues) {
			return ErrInvalid
		}
	}
	for i, statistic := range t.Extended {
		if statistic.OID == 0 || i != 0 && t.Extended[i-1].OID >= statistic.OID {
			return ErrInvalid
		}
		dimensions := 0
		if len(statistic.NDistinct) != 0 {
			summary, err := DecodeNDistinct(ctx, statistic.NDistinct, l)
			if err != nil {
				return err
			}
			dimensions = len(summary.Dimensions)
		}
		if len(statistic.Dependencies) != 0 {
			count, _, err := DecodeDependencies(ctx, statistic.Dependencies, l)
			if err != nil {
				return err
			}
			if dimensions != 0 && count != dimensions {
				return ErrInvalid
			}
			dimensions = count
		}
		if len(statistic.MCV) != 0 {
			list, err := DecodeMCVList(ctx, statistic.MCV, l)
			if err != nil {
				return err
			}
			if dimensions != 0 && len(list.Types) != dimensions {
				return ErrInvalid
			}
			dimensions = len(list.Types)
		}
		if statistic.Expressions.Type != model.TypeAny {
			if err := validateExpressionStatisticRecords(statistic.Expressions, l); err != nil {
				return err
			}
			if dimensions != 0 && len(statistic.Expressions.ArrayVal) > dimensions {
				return ErrInvalid
			}
			if dimensions == 0 {
				dimensions = len(statistic.Expressions.ArrayVal)
			}
		}
		if dimensions == 0 {
			return ErrInvalid
		}
	}
	return ctx.Err()
}

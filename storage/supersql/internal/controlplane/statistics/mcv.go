package statistics

import (
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const mcvWireMagic uint64 = 0x4d43560000000001

type MCVItem struct {
	Values        []model.Value
	Nulls         []bool
	Frequency     float64
	BaseFrequency float64
}

type MCVList struct {
	Types []model.TypeLayout
	Items []MCVItem
}

func mcvListBytes(ctx context.Context, list *MCVList, limits Limits) (uint64, error) {
	if list == nil || len(list.Types) == 0 || len(list.Types) > limits.MaxColumns || len(list.Items) > limits.MaxValues {
		return 0, ErrInvalid
	}
	meter := meter{ctx: ctx, limit: limits.MaxBytes, maxValues: limits.MaxValues, maxDepth: limits.MaxDepth}
	if err := meter.add(1, uint64(unsafe.Sizeof(MCVList{}))); err != nil {
		return 0, err
	}
	if err := meter.add(uint64(cap(list.Types)), uint64(unsafe.Sizeof(model.TypeLayout{}))); err != nil {
		return 0, err
	}
	if err := meter.add(uint64(cap(list.Items)), uint64(unsafe.Sizeof(MCVItem{}))); err != nil {
		return 0, err
	}
	for _, layout := range list.Types {
		if err := meter.layout(layout, 0); err != nil {
			return 0, err
		}
	}
	for _, item := range list.Items {
		if len(item.Values) != len(list.Types) || len(item.Nulls) != len(list.Types) || math.IsNaN(item.Frequency) || math.IsNaN(item.BaseFrequency) || item.Frequency < 0 || item.Frequency > 1 || item.BaseFrequency < 0 || item.BaseFrequency > 1 {
			return 0, ErrInvalid
		}
		if err := meter.add(uint64(cap(item.Values)), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
			return 0, err
		}
		if err := meter.add(uint64(cap(item.Nulls)), 1); err != nil {
			return 0, err
		}
		for i, value := range item.Values {
			if item.Nulls[i] {
				if !value.IsNull {
					return 0, ErrInvalid
				}
				continue
			}
			if value.IsNull || value.Type != list.Types[i].Type.Storage {
				return 0, ErrInvalid
			}
			if err := meter.value(value, 0); err != nil {
				return 0, err
			}
		}
	}
	return meter.used, ctx.Err()
}

func validateMCVList(ctx context.Context, list *MCVList, limits Limits) error {
	_, err := mcvListBytes(ctx, list, limits)
	return err
}

// MCVListBytes measures the independently owned decoded representation.
func MCVListBytes(ctx context.Context, list *MCVList, limits Limits) (uint64, error) {
	if err := limits.Validate(); err != nil {
		return 0, err
	}
	return mcvListBytes(ctx, list, limits)
}

func writeMCVList(w *wireWriter, list *MCVList) {
	w.number(mcvWireMagic)
	w.number(uint64(len(list.Types)))
	for _, layout := range list.Types {
		w.layout(layout)
	}
	w.number(uint64(len(list.Items)))
	for _, item := range list.Items {
		w.number(math.Float64bits(item.Frequency))
		w.number(math.Float64bits(item.BaseFrequency))
		for i, value := range item.Values {
			if item.Nulls[i] {
				w.number(1)
				continue
			}
			w.number(0)
			w.value(value)
		}
	}
}

// EncodeMCVList encodes a typed extended-statistics MCV list. The frame is
// independent of Go object layout and preserves scalar bit patterns.
func EncodeMCVList(ctx context.Context, list *MCVList, limits Limits) ([]byte, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if err := validateMCVList(ctx, list, limits); err != nil {
		return nil, err
	}
	writer := wireWriter{ctx: ctx, limit: limits.MaxBytes}
	writeMCVList(&writer, list)
	if writer.err != nil {
		return nil, writer.err
	}
	writer.data = make([]byte, int(writer.used))
	writer.used = 0
	writeMCVList(&writer, list)
	return writer.data, writer.err
}

// DecodeMCVList returns independently owned typed values under the supplied
// statistics limits and rejects noncanonical or trailing wire data.
func DecodeMCVList(ctx context.Context, data []byte, limits Limits) (*MCVList, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if uint64(len(data)) > limits.MaxBytes {
		return nil, ErrLimit
	}
	reader := wireReader{data: data, budget: meter{ctx: ctx, limit: limits.MaxBytes, maxValues: limits.MaxValues, maxDepth: limits.MaxDepth}}
	if reader.number() != mcvWireMagic {
		return nil, ErrInvalid
	}
	if err := reader.budget.add(1, uint64(unsafe.Sizeof(MCVList{}))); err != nil {
		return nil, err
	}
	dimensions := reader.bounded(uint64(limits.MaxColumns))
	if dimensions == 0 || reader.err != nil {
		return nil, ErrInvalid
	}
	if err := reader.budget.add(dimensions, uint64(unsafe.Sizeof(model.TypeLayout{}))); err != nil {
		return nil, err
	}
	list := &MCVList{Types: make([]model.TypeLayout, int(dimensions))}
	for i := range list.Types {
		list.Types[i] = reader.layout(0)
	}
	count := reader.count(uint64(unsafe.Sizeof(MCVItem{})))
	list.Items = make([]MCVItem, count)
	for i := range list.Items {
		item := &list.Items[i]
		item.Frequency = math.Float64frombits(reader.number())
		item.BaseFrequency = math.Float64frombits(reader.number())
		if err := reader.budget.add(dimensions, uint64(unsafe.Sizeof(model.Value{}))+1); err != nil {
			return nil, err
		}
		item.Values = make([]model.Value, len(list.Types))
		item.Nulls = make([]bool, len(list.Types))
		for dimension, layout := range list.Types {
			item.Nulls[dimension] = reader.bounded(1) == 1
			if item.Nulls[dimension] {
				item.Values[dimension] = model.NullVal(layout.Type.Storage)
			} else {
				item.Values[dimension] = reader.value()
			}
		}
		if reader.err != nil {
			return nil, reader.err
		}
	}
	if reader.offset != len(data) {
		return nil, ErrInvalid
	}
	if err := validateMCVList(ctx, list, limits); err != nil {
		return nil, err
	}
	check := canonicalWriter{remaining: data}
	writer := wireWriter{ctx: ctx, limit: limits.MaxBytes, destination: &check}
	writeMCVList(&writer, list)
	if writer.err != nil {
		return nil, writer.err
	}
	if len(check.remaining) != 0 {
		return nil, ErrInvalid
	}
	return list, ctx.Err()
}

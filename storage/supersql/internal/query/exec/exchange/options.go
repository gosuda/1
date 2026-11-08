package exchange

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type Options struct {
	Query            *runtime.Query
	Layouts          []model.TypeLayout
	BatchRows        int
	BatchBytes       uint64
	QueueBytes       uint64
	MetadataBytes    uint64
	ParallelEligible bool
	EstimatedRows    float64
}

type sourceBudget struct {
	slot, queue, metadata uint64
}

// CheckSupport validates bounds without allocating buffers or taking ownership.
func CheckSupport(ctx context.Context, inputCount int, options Options) error {
	_, err := checkSources(ctx, inputCount, options)
	return err
}

func checkSources(ctx context.Context, count int, o Options) (sourceBudget, error) {
	var b sourceBudget
	invalidRuntime := o.Query == nil || count < 0
	invalidBatch := o.BatchRows <= 0 || o.BatchBytes == 0
	invalidMemory := o.QueueBytes == 0 || o.MetadataBytes == 0
	invalidEstimate := math.IsNaN(o.EstimatedRows) || math.IsInf(o.EstimatedRows, 0) || o.EstimatedRows < 0
	if invalidRuntime || invalidBatch || invalidMemory || invalidEstimate {
		return b, runtime.ErrInvalidConfig
	}
	if uint64(o.BatchRows) > math.MaxUint32 {
		return b, runtime.ErrInvalidConfig
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(o.Query.Context())); err != nil {
		return b, err
	}
	for _, value := range []uint64{o.BatchBytes, o.QueueBytes, o.MetadataBytes, uint64(o.BatchRows), uint64(count), uint64(len(o.Layouts))} {
		if value > uint64(math.MaxInt)/1024 {
			return b, runtime.ErrInvalidConfig
		}
	}
	b.slot = min(o.BatchBytes, o.QueueBytes/uint64(max(1, count)))
	if b.slot == 0 {
		return b, exchangeBudget("admit exchange queue", nil)
	}
	b.queue = b.slot * uint64(count)
	b.metadata = uint64(unsafe.Sizeof(Sources{})) + 1024
	if !addSourceBytes(&b.metadata, uint64(count), uint64(unsafe.Sizeof(source{}))+uint64(unsafe.Sizeof(columnar.Builder{}))+1024+b.slot, o.MetadataBytes) ||
		!addSourceBytes(&b.metadata, uint64(count), uint64(o.BatchRows)*4, o.MetadataBytes) ||
		!addSourceBytes(&b.metadata, uint64(count), uint64(len(o.Layouts))*(2*uint64(unsafe.Sizeof(columnar.Vector{}))+uint64(unsafe.Sizeof(model.Value{}))), o.MetadataBytes) {
		return b, exchangeBudget("admit exchange metadata", nil)
	}
	var nodes uint64
	for _, layout := range o.Layouts {
		if err := countSourceLayout(ctx, layout, 0, &nodes, &b.metadata, o.MetadataBytes); err != nil {
			return b, err
		}
	}
	// Builder marks contain eleven int lengths; both headers and marks are
	// retained even before the first payload lane is appended.
	if nodes > b.slot/(uint64(unsafe.Sizeof(columnar.Vector{}))+11*uint64(unsafe.Sizeof(int(0)))) {
		return b, exchangeBudget("admit exchange batch headers", nil)
	}
	return b, nil
}

func addSourceBytes(used *uint64, count, size, limit uint64) bool {
	if *used > limit || size != 0 && count > (limit-*used)/size {
		return false
	}
	*used += count * size
	return true
}

func countSourceLayout(ctx context.Context, layout model.TypeLayout, depth int, nodes, used *uint64, limit uint64) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if depth > 64 || layout.Type.ID == 0 || !layout.Type.Storage.Valid() {
		return model.ErrInvalidTypeLayout
	}
	switch layout.Type.Storage {
	case model.TypeArray:
		if len(layout.Children) != 1 {
			return model.ErrInvalidTypeLayout
		}
	case model.TypeComposite:
	default:
		if len(layout.Children) != 0 {
			return model.ErrInvalidTypeLayout
		}
	}
	if !addSourceBytes(used, 1, uint64(unsafe.Sizeof(model.TypeLayout{})), limit) {
		return exchangeBudget("copy exchange layouts", nil)
	}
	*nodes++
	for _, child := range layout.Children {
		if err := countSourceLayout(ctx, child, depth+1, nodes, used, limit); err != nil {
			return err
		}
	}
	return nil
}

func exchangeBudget(operation string, cause error) error {
	return &model.Error{Code: "53200", Message: operation, Cause: errors.Join(runtime.ErrBudget, cause)}
}

func copySourceRow(ctx context.Context, batch *columnar.Batch, row int, values []model.Value, limit uint64) error {
	clear(values)
	lane := batch.Lane(row)
	for i := range values {
		value, used, err := columnar.CopyValue(ctx, &batch.Columns[i], lane, limit)
		if err != nil {
			clear(values)
			return err
		}
		values[i] = value
		limit -= used
	}
	return nil
}

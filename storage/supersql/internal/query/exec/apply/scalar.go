package apply

import (
	"context"
	"errors"
	"io"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type ScalarOptions struct {
	Layouts                []model.TypeLayout
	Query                  *runtime.Query
	BatchRows              int
	MaxRowBytes, WorkBytes uint64
}

// ConsumeScalar opens one body and proves cardinality before lending its row.
// Captures, options, and the row are borrowed until return; every cursor is closed.
func ConsumeScalar(ctx context.Context, factory RightFactory, captures []model.Value, options ScalarOptions, consume func([]model.Value, bool) error) (err error) {
	if factory == nil || consume == nil || options.Query == nil || options.BatchRows <= 0 || options.MaxRowBytes == 0 || len(options.Layouts) == 0 {
		return runtime.ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	width := uint64(len(options.Layouts))
	base := uint64(unsafe.Sizeof(columnar.Batch{})) + width*(uint64(unsafe.Sizeof(columnar.Vector{}))+uint64(unsafe.Sizeof(model.Value{}))) + uint64(unsafe.Sizeof(ScalarOptions{}))
	if base > options.WorkBytes {
		return runtime.ErrBudget
	}
	claim, err := options.Query.Allocator(runtime.WorkMemory).Reserve(base)
	if err != nil {
		return err
	}
	defer claim.Release()
	source, err := factory.Open(ctx, captures)
	if source != nil {
		defer func() { err = errors.Join(err, source.Close()) }()
	}
	if err != nil {
		return err
	}
	if source == nil {
		return errFactoryCursor
	}
	batch := columnar.Batch{Columns: make([]columnar.Vector, 0, len(options.Layouts))}
	row := make([]model.Value, len(options.Layouts))
	defer func() {
		clear(row)
		batch = columnar.Batch{}
	}()
	index := 0
	limit := min(options.BatchRows, 2)
	lane, readErr := readRow(ctx, source, &batch, &index, options.Layouts, limit)
	present := readErr == nil
	if present {
		bytes, measureErr := borrowedRowBytes(ctx, &batch, lane, options.MaxRowBytes)
		if measureErr != nil {
			return measureErr
		}
		if bytes > options.WorkBytes-base {
			return runtime.ErrBudget
		}
		if err = claim.Resize(base + bytes); err != nil {
			return err
		}
		if err = copyBorrowedRow(ctx, &batch, lane, row, options.MaxRowBytes); err != nil {
			return err
		}
		_, readErr = readRow(ctx, source, &batch, &index, options.Layouts, limit)
		if readErr == nil {
			return errCardinality
		}
	} else {
		for i, layout := range options.Layouts {
			row[i] = model.NullVal(layout.Type.Storage)
		}
	}
	if readErr != io.EOF {
		return readErr
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = consume(row, present); err != nil {
		return err
	}
	return ctx.Err()
}

type RowsOptions struct {
	Layouts                []model.TypeLayout
	Query                  *runtime.Query
	Exists                 bool
	MaxRowBytes, WorkBytes uint64
}

// ConsumeRows lends one result row at a time, closing the child on stop or failure.
// EXISTS lends an empty presence row without copying discarded columns.
func ConsumeRows(ctx context.Context, factory RightFactory, captures []model.Value, options RowsOptions, visit func([]model.Value) (bool, error)) (err error) {
	if factory == nil || visit == nil || options.Query == nil || options.MaxRowBytes == 0 || (!options.Exists && len(options.Layouts) == 0) {
		return runtime.ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	valueCount := len(options.Layouts)
	if options.Exists {
		valueCount = 0
	}
	base := uint64(unsafe.Sizeof(columnar.Batch{})) + uint64(len(options.Layouts))*uint64(unsafe.Sizeof(columnar.Vector{})) + uint64(valueCount)*uint64(unsafe.Sizeof(model.Value{}))
	if base > options.WorkBytes {
		return runtime.ErrBudget
	}
	claim, err := options.Query.Allocator(runtime.WorkMemory).Reserve(base)
	if err != nil {
		return err
	}
	defer claim.Release()
	source, err := factory.Open(ctx, captures)
	if source != nil {
		defer func() { err = errors.Join(err, source.Close()) }()
	}
	if err != nil {
		return err
	}
	if source == nil {
		return errFactoryCursor
	}
	batch := columnar.Batch{Columns: make([]columnar.Vector, 0, len(options.Layouts))}
	row := make([]model.Value, valueCount)
	defer clear(row)
	index := 0
	for {
		// One-row demand preserves early-stop error and volatile-call timing.
		lane, err := readRow(ctx, source, &batch, &index, options.Layouts, 1)
		if err == io.EOF {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if !options.Exists {
			bytes, err := borrowedRowBytes(ctx, &batch, lane, options.MaxRowBytes)
			if err != nil {
				return err
			}
			if bytes > options.WorkBytes-base {
				return runtime.ErrBudget
			}
			if err := claim.Resize(base + bytes); err != nil {
				return err
			}
			if err := copyBorrowedRow(ctx, &batch, lane, row, options.MaxRowBytes); err != nil {
				return err
			}
		}
		more, err := visit(row)
		clear(row)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !more {
			return nil
		}
		if err := claim.Resize(base); err != nil {
			return err
		}
	}
}

func borrowedRowBytes(ctx context.Context, batch *columnar.Batch, lane int, maxBytes uint64) (uint64, error) {
	var total uint64
	for i := range batch.Columns {
		bytes, err := columnar.ValueBytes(ctx, &batch.Columns[i], lane, maxBytes)
		if err != nil {
			return 0, err
		}
		if bytes > maxBytes-total {
			return 0, runtime.ErrBudget
		}
		total += bytes
	}
	return total, nil
}

func copyBorrowedRow(ctx context.Context, batch *columnar.Batch, lane int, row []model.Value, maxBytes uint64) error {
	clear(row)
	remaining := maxBytes
	for i := range row {
		value, used, err := columnar.CopyValue(ctx, &batch.Columns[i], lane, remaining)
		if err != nil {
			return err
		}
		row[i], remaining = value, remaining-used
	}
	return nil
}

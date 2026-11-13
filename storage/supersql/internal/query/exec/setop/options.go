// Package setop executes typed SQL bag operations with bounded resident groups.
package setop

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type Mode uint8

const (
	Distinct Mode = iota + 1
	DistinctOn
	Binary
)

type OrderKey struct {
	Column     int
	Descending bool
	NullsFirst bool
}

// OrderedInput is a planner proof of the complete input ordering, including
// representative tie-breakers. DISTINCT ON requires DistinctColumns as its
// leading keys and checks monotonicity while reading; it never chooses by hash.
// DISTINCT and binary operators compare complete rows. DistinctColumns may
// explicitly enumerate all columns for DISTINCT; binary operators require nil.
// Limits are positive except PartitionMemoryBytes (zero forces disk). WorkBytes
// excludes input/output batches. MaxSpillBytes caps cumulative deferred record
// bytes, including in-memory records. MaxPasses bounds repeated partition I/O:
// each pass resolves resident keys and rewrites the remainder, O(rows*passes)
// in the worst case. At most two spools and one reader are live.
// Layouts and index slices are copied; Query and Encoder are borrowed.
type Options struct {
	Mode                 Mode
	Kind                 ir.SetKind
	All                  bool
	Layouts              []model.TypeLayout
	DistinctColumns      []int
	OrderedInput         []OrderKey
	Query                *runtime.Query
	Encoder              storage.KeyEncoder
	BatchRows            int
	BatchBytes           uint64
	MaxRecordBytes       uint64
	GroupBytes           uint64
	PartitionMemoryBytes uint64
	MaxSpillBytes        uint64
	WorkBytes            uint64
	MaxPasses            int
}

var ErrUnprovenOrder = errors.New("DISTINCT ON requires a proven input order with the distinct columns as leading keys")
var ErrInputOrder = errors.New("DISTINCT ON input violates its proven ordering")
var ErrMultiplicityOverflow = &model.Error{Code: "22003", Message: "set operation multiplicity overflow"}

func resource(message string, cause error) error {
	return &model.Error{Code: "53200", Message: message, Cause: errors.Join(runtime.ErrBudget, cause)}
}

// New takes ownership of its unary input on every outcome, without pulling it.
// Next and Close must be serialized; output is borrowed until either call.
func New(ctx context.Context, input storage.BatchCursor, options Options) (storage.BatchCursor, error) {
	c := &cursor{options: options, inputs: [2]storage.BatchCursor{input}}
	if options.Mode != Distinct && options.Mode != DistinctOn {
		return nil, errors.Join(runtime.ErrInvalidConfig, c.Close())
	}
	if err := c.configure(ctx); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

// NewBinary takes ownership of both inputs on every outcome. Both must already
// be coerced to Layouts; binary comparison always includes every column.
func NewBinary(ctx context.Context, left, right storage.BatchCursor, options Options) (storage.BatchCursor, error) {
	c := &cursor{options: options, inputs: [2]storage.BatchCursor{left, right}}
	if options.Mode != Binary || right == nil {
		return nil, errors.Join(runtime.ErrInvalidConfig, c.Close())
	}
	if err := c.configure(ctx); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

func (c *cursor) configure(ctx context.Context) error {
	o := &c.options
	missingInput := c.inputs[0] == nil || o.Query == nil || o.Encoder == nil
	invalidBatch := o.BatchRows <= 0 || o.BatchBytes == 0 || o.MaxRecordBytes < 128
	invalidState := o.GroupBytes == 0 || o.MaxSpillBytes == 0 || o.WorkBytes == 0
	if missingInput || invalidBatch || invalidState || o.MaxPasses <= 0 {
		return runtime.ErrInvalidConfig
	}
	for _, n := range []uint64{o.BatchBytes, o.MaxRecordBytes, o.GroupBytes, o.PartitionMemoryBytes, o.MaxSpillBytes, o.WorkBytes} {
		if n > uint64(math.MaxInt)/128 {
			return runtime.ErrInvalidConfig
		}
	}
	if o.Mode == Binary {
		validKind := o.Kind == ir.Union || o.Kind == ir.Intersect || o.Kind == ir.Except
		if !validKind || o.DistinctColumns != nil || len(o.OrderedInput) != 0 {
			return runtime.ErrInvalidConfig
		}
	} else if o.Kind != 0 || o.All {
		return runtime.ErrInvalidConfig
	}
	if o.Mode != DistinctOn && len(o.OrderedInput) != 0 {
		return runtime.ErrInvalidConfig
	}
	if o.Mode == DistinctOn {
		if len(o.DistinctColumns) == 0 || len(o.OrderedInput) < len(o.DistinctColumns) {
			return ErrUnprovenOrder
		}
		for _, col := range o.DistinctColumns {
			matches := 0
			for _, key := range o.OrderedInput[:len(o.DistinctColumns)] {
				if key.Column == col {
					matches++
				}
			}
			if matches != 1 {
				return ErrUnprovenOrder
			}
		}
	}
	if o.Mode == Distinct && o.DistinctColumns != nil && len(o.DistinctColumns) != len(o.Layouts) {
		return columnar.ErrValueLayout
	}
	if err := c.check(ctx); err != nil {
		return err
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.stopQuery = context.AfterFunc(o.Query.Context(), c.cancel)
	base := uint64(unsafe.Sizeof(cursor{})) + 4096 + uint64(len(o.Layouts))*1024 + uint64(len(o.OrderedInput)+len(o.DistinctColumns))*256
	if base > o.WorkBytes {
		return resource("admit set operation schema", nil)
	}
	claim, err := o.Query.Allocator(runtime.WorkMemory).Reserve(base)
	if err != nil {
		return err
	}
	c.claim = claim
	c.layouts = make([]model.TypeLayout, len(o.Layouts)+1)
	remaining := base / 2
	for i, layout := range o.Layouts {
		owned, used, err := model.CopyTypeLayout(ctx, layout, remaining)
		if err != nil {
			return err
		}
		c.layouts[i], remaining = owned, remaining-used
	}
	c.layouts[len(o.Layouts)] = model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
	o.Layouts = c.layouts[:len(o.Layouts)]
	o.DistinctColumns = append([]int(nil), o.DistinctColumns...)
	o.OrderedInput = append([]OrderKey(nil), o.OrderedInput...)
	if o.Mode != DistinctOn && len(o.DistinctColumns) == 0 {
		o.DistinctColumns = make([]int, len(o.Layouts))
		for i := range o.DistinctColumns {
			o.DistinctColumns[i] = i
		}
	}
	c.keyLayout = make([]storage.KeyColumn, len(o.DistinctColumns))
	for i, col := range o.DistinctColumns {
		if col < 0 || col >= len(o.Layouts) {
			return columnar.ErrValueLayout
		}
		for _, previous := range o.DistinctColumns[:i] {
			if previous == col {
				return columnar.ErrValueLayout
			}
		}
		c.keyLayout[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: o.Layouts[col].Type, Children: o.Layouts[col].Children}
	}
	c.orderLayout = make([]storage.KeyColumn, len(o.OrderedInput))
	for i, key := range o.OrderedInput {
		if key.Column < 0 || key.Column >= len(o.Layouts) {
			return columnar.ErrValueLayout
		}
		c.orderLayout[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: o.Layouts[key.Column].Type, Children: o.Layouts[key.Column].Children, Descending: key.Descending, NullsFirst: key.NullsFirst}
	}
	if !c.unionAll() {
		if _, err := o.Encoder.EncodePrefix(ctx, c.keyLayout, nil, o.MaxRecordBytes); err != nil {
			return err
		}
	}
	if _, err := o.Encoder.EncodePrefix(ctx, c.orderLayout, nil, o.MaxRecordBytes); err != nil {
		return err
	}
	perGroup := uint64(256) + o.MaxRecordBytes*9 + uint64(len(o.Layouts))*uint64(unsafe.Sizeof(model.Value{}))
	c.maxGroups = int(o.GroupBytes / perGroup)
	if c.maxGroups == 0 {
		return resource("set operation group exceeds workspace", nil)
	}
	c.metadata = base + 4096
	fixed := base + o.GroupBytes + o.MaxRecordBytes*32
	spoolWork := c.metadata + o.MaxRecordBytes*9 + 8192
	spoolPeak := spoolWork*2 + o.MaxRecordBytes*9 + 8192 + o.PartitionMemoryBytes*2
	if fixed > o.WorkBytes || spoolPeak > o.WorkBytes-fixed {
		return resource("set operation work limit cannot admit partitions", nil)
	}
	if err := claim.Resize(fixed); err != nil {
		return err
	}
	if o.Mode != DistinctOn && !c.unionAll() {
		c.groups = make([]*group, 0, c.maxGroups)
		c.lookup = make(map[string]*group, c.maxGroups)
	}
	c.row = make([]model.Value, len(c.layouts))
	c.keyValues = make([]model.Value, max(len(c.keyLayout), len(c.orderLayout)))
	c.builder, err = columnar.NewBuilder(o.Layouts, columnar.BuilderOptions{MaxRows: o.BatchRows, MaxBytes: o.BatchBytes, Allocator: o.Query.Allocator(runtime.OutputMemory)})
	return err
}

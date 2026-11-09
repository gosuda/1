package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// EncodingVersion 1 frames bounded JSON metadata; defaults are versioned scalar
// codec bytes (JSON base64), never JSON numeric representations.
const EncodingVersion uint32 = 1

var ErrCorruptCatalog = errors.New("corrupt catalog record")

type Limits struct {
	Descriptor      model.DescriptorLimits
	MaxEncodedBytes uint64
	MaxKeyBytes     uint64
	ScanBatchRows   int
}

func (l Limits) Validate() error {
	if err := l.Descriptor.Validate(); err != nil {
		return err
	}
	if l.MaxEncodedBytes == 0 || l.MaxEncodedBytes > uint64(^uint(0)>>1) || l.MaxKeyBytes == 0 || l.MaxKeyBytes > uint64(^uint32(0)) || l.ScanBatchRows <= 0 {
		return model.ErrDescriptorLimit
	}
	return nil
}

type defaultWire struct {
	Section uint8
	Column  model.ColumnID
	Value   []byte
}
type descriptorWire struct {
	Version    uint32
	Descriptor model.DescriptorSpec
	Defaults   []defaultWire
}

func columnGroups(spec *model.DescriptorSpec) [3][]model.ColumnDescriptor {
	var groups [3][]model.ColumnDescriptor
	if spec.Table != nil {
		groups[0] = spec.Table.Columns
	}
	if spec.View != nil {
		groups[1] = spec.View.Columns
	}
	if spec.Type != nil {
		groups[2] = spec.Type.Fields
	}
	return groups
}
func EncodeDescriptor(ctx context.Context, d *model.Descriptor, limits Limits) ([]byte, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if d == nil {
		return nil, model.ErrInvalidDescriptor
	}
	spec, err := d.SpecWithin(ctx, limits.Descriptor)
	if err != nil {
		return nil, err
	}
	wire := descriptorWire{Version: EncodingVersion, Descriptor: spec}
	var total uint64
	for section, columns := range columnGroups(&wire.Descriptor) {
		for i := range columns {
			c := &columns[i]
			if c.ConstantDefault == nil {
				continue
			}
			if err := storage.ValidateStoredValue(ctx, *c.ConstantDefault, c.Layout); err != nil {
				return nil, err
			}
			var b bytes.Buffer
			if err := storage.EncodeValue(ctx, &b, *c.ConstantDefault, limits.MaxEncodedBytes-total); err != nil {
				return nil, err
			}
			total += uint64(b.Len())
			wire.Defaults = append(wire.Defaults, defaultWire{Section: uint8(section), Column: c.ID, Value: b.Bytes()})
			c.ConstantDefault = nil
		}
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	if err := preflight(ctx, data, limits); err != nil {
		return nil, err
	}
	return data, nil
}

type preflightLevel struct {
	object bool
	key    bool
	count  int
	names  map[string]struct{}
}

func (level *preflightLevel) accept(token json.Token, maxItems int) error {
	if delim, ok := token.(json.Delim); ok && (delim == '}' || delim == ']') {
		return nil
	}
	if level.object && level.key {
		key, ok := token.(string)
		if !ok {
			return ErrCorruptCatalog
		}
		if _, exists := level.names[key]; exists {
			return ErrCorruptCatalog
		}
		level.names[key] = struct{}{}
		level.key = false
		return nil
	}
	level.count++
	if level.count > maxItems {
		return model.ErrDescriptorLimit
	}
	if level.object {
		level.key = true
	}
	return nil
}

func advancePreflightStack(stack []preflightLevel, token json.Token, maxDepth int) ([]preflightLevel, error) {
	delim, ok := token.(json.Delim)
	if !ok {
		return stack, nil
	}
	switch delim {
	case '{', '[':
		if len(stack) >= maxDepth {
			return nil, model.ErrDescriptorLimit
		}
		entry := preflightLevel{object: delim == '{', key: delim == '{'}
		if entry.object {
			entry.names = make(map[string]struct{})
		}
		return append(stack, entry), nil
	case '}', ']':
		if len(stack) == 0 {
			return nil, ErrCorruptCatalog
		}
		return stack[:len(stack)-1], nil
	default:
		return stack, nil
	}
}

// Preflight walks the entire token stream before constructing wire structs.
// Token count also bounds zero-payload structs, whose heap cost exceeds their
// JSON spelling. Duplicate fields are rejected here; typed decoding rejects unknowns.
func preflight(ctx context.Context, data []byte, limits Limits) error {
	if uint64(len(data)) > limits.MaxEncodedBytes {
		return model.ErrDescriptorLimit
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	maxDepth := 2*limits.Descriptor.MaxDepth + 8
	stack := make([]preflightLevel, 0, maxDepth)
	var tokens uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		token, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		tokens++
		if tokens > limits.Descriptor.MaxBytes/512 {
			return model.ErrDescriptorLimit
		}
		if len(stack) > 0 {
			if err := stack[len(stack)-1].accept(token, limits.Descriptor.MaxItems); err != nil {
				return err
			}
		}
		stack, err = advancePreflightStack(stack, token, maxDepth)
		if err != nil {
			return err
		}
	}
	if len(stack) != 0 {
		return ErrCorruptCatalog
	}
	return nil
}
func DecodeDescriptor(ctx context.Context, data []byte, limits Limits) (*model.Descriptor, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if err := preflight(ctx, data, limits); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptCatalog, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var wire descriptorWire
	if err := dec.Decode(&wire); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptCatalog, err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, ErrCorruptCatalog
	}
	if wire.Version != EncodingVersion {
		return nil, ErrCorruptCatalog
	}
	groups := columnGroups(&wire.Descriptor)
	for _, columns := range groups {
		for _, c := range columns {
			if c.ConstantDefault != nil {
				return nil, ErrCorruptCatalog
			}
		}
	}
	if _, err := model.NewDescriptor(ctx, wire.Descriptor, limits.Descriptor); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptCatalog, err)
	}
	var used uint64
	for _, entry := range wire.Defaults {
		if int(entry.Section) >= len(groups) {
			return nil, ErrCorruptCatalog
		}
		found := false
		for i := range groups[entry.Section] {
			c := &groups[entry.Section][i]
			if c.ID != entry.Column {
				continue
			}
			if c.ConstantDefault != nil {
				return nil, ErrCorruptCatalog
			}
			value, err := storage.DecodeValue(ctx, entry.Value, limits.Descriptor.MaxBytes-used)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrCorruptCatalog, err)
			}
			// Account all defaults together, not one independent maximum per column.
			n, err := defaultSize(value, limits.Descriptor.MaxBytes-used, limits.Descriptor.MaxDepth)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrCorruptCatalog, err)
			}
			used += n
			if err := storage.ValidateStoredValue(ctx, value, c.Layout); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrCorruptCatalog, err)
			}
			c.ConstantDefault = &value
			found = true
			break
		}
		if !found {
			return nil, ErrCorruptCatalog
		}
	}
	result, err := model.NewDescriptor(ctx, wire.Descriptor, limits.Descriptor)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptCatalog, err)
	}
	return result, nil
}

func defaultSize(value model.Value, limit uint64, depth int) (uint64, error) {
	if depth <= 0 {
		return 0, model.ErrDescriptorLimit
	}
	size := uint64(unsafe.Sizeof(value)) + uint64(len(value.StrVal)) + uint64(len(value.BytesVal)) + uint64(len(value.VectorVal))*4 + uint64(len(value.Dimensions))*uint64(unsafe.Sizeof(model.ArrayDimension{}))
	if size > limit {
		return 0, model.ErrDescriptorLimit
	}
	for _, child := range value.ArrayVal {
		n, err := defaultSize(child, limit-size, depth-1)
		if err != nil {
			return 0, err
		}
		size += n
	}
	return size, nil
}

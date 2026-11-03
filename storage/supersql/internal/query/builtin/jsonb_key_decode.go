package builtin

import (
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type jsonbKeyFrame struct {
	remaining uint32
	object    bool
	started   bool
}

func (d *keyDecoder) jsonbValue(ctx context.Context, mask byte) (model.Value, error) {
	start := d.offset
	probe := *d
	if err := probe.skipJSONBKey(ctx, mask); err != nil {
		return model.Value{}, err
	}
	if d.data[start]^mask == 0 {
		d.offset = probe.offset
		return model.Value{Type: model.TypeJSONB, StrVal: "[]"}, nil
	}
	frameCount := 0
	if kind := d.data[start+1] ^ mask; kind == 4 || kind == 5 {
		frameCount = min((probe.offset-start)/5+1, jsonTextMaxDepth)
	}
	frameBytes := uint64(frameCount) * uint64(unsafe.Sizeof(jsonbKeyFrame{}))
	if err := d.charge(frameBytes); err != nil {
		return model.Value{}, err
	}
	frames := make([]jsonbKeyFrame, frameCount)
	budget := CallBudget{OutputBytes: int64(min(d.remaining, uint64(math.MaxInt64))), MemoryBytes: d.remaining}
	result, err := jsonbBuildDocument(ctx, budget, func(writer *jsonbBuildWriter) error {
		reader := keyDecoder{data: d.data, offset: start, remaining: writer.budget.MemoryBytes}
		if err := reader.emitJSONBKey(ctx, mask, frames, writer); err != nil {
			return err
		}
		if reader.offset != probe.offset {
			return storage.ErrInvalidEncoding
		}
		return nil
	})
	frames = nil
	d.remaining += frameBytes
	if err != nil {
		return model.Value{}, err
	}
	if err := d.charge(uint64(len(result.StrVal))); err != nil {
		return model.Value{}, err
	}
	d.offset = probe.offset
	return result, nil
}

// Counts let the validation pass skip containers without a stack or allocations.
func (d *keyDecoder) skipJSONBKey(ctx context.Context, mask byte) error {
	root, err := d.byte(mask)
	if err != nil {
		return err
	}
	if root == 0 {
		return nil
	}
	if root != 1 {
		return storage.ErrInvalidEncoding
	}
	pending := uint64(1)
	for pending != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if pending > uint64(len(d.data)-d.offset) {
			return storage.ErrInvalidEncoding
		}
		kind, err := d.byte(mask)
		if err != nil {
			return err
		}
		pending--
		switch kind {
		case 0:
		case 1:
			if err := d.skipEscaped(ctx, mask); err != nil {
				return err
			}
		case 2:
			if err := d.skipNumeric(ctx, mask); err != nil {
				return err
			}
		case 3:
			value, err := d.byte(mask)
			if err != nil {
				return err
			}
			if value > 1 {
				return storage.ErrInvalidEncoding
			}
		case 4, 5:
			count, err := d.number(4, mask)
			if err != nil {
				return err
			}
			if kind == 5 {
				count *= 2
			}
			pending += count
		default:
			return storage.ErrInvalidEncoding
		}
	}
	return nil
}

func (d *keyDecoder) emitJSONBKey(ctx context.Context, mask byte, frames []jsonbKeyFrame, writer *jsonbBuildWriter) error {
	root, err := d.byte(mask)
	if err != nil {
		return err
	}
	if root == 0 {
		return writer.raw("[]")
	}
	if root != 1 {
		return storage.ErrInvalidEncoding
	}
	depth := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth != 0 {
			frame := &frames[depth-1]
			if frame.started {
				if err := writer.raw(","); err != nil {
					return err
				}
			}
			if frame.object {
				kind, err := d.byte(mask)
				if err != nil {
					return err
				}
				if kind != 1 {
					return storage.ErrInvalidEncoding
				}
				if err := d.emitJSONBKeyScalar(ctx, kind, mask, writer); err != nil {
					return err
				}
				if err := writer.raw(":"); err != nil {
					return err
				}
			}
		}
		kind, err := d.byte(mask)
		if err != nil {
			return err
		}
		if kind == 4 || kind == 5 {
			count, err := d.number(4, mask)
			if err != nil {
				return err
			}
			opening := "["
			if kind == 5 {
				opening = "{"
			}
			if err := writer.raw(opening); err != nil {
				return err
			}
			if count != 0 {
				if depth == len(frames) {
					return storage.ErrInvalidEncoding
				}
				frames[depth] = jsonbKeyFrame{remaining: uint32(count), object: kind == 5}
				depth++
				continue
			}
			if err := closeJSONBKeyContainer(writer, kind == 5); err != nil {
				return err
			}
		} else if err := d.emitJSONBKeyScalar(ctx, kind, mask, writer); err != nil {
			return err
		}
		for depth != 0 {
			frame := &frames[depth-1]
			frame.remaining--
			frame.started = true
			if frame.remaining != 0 {
				break
			}
			if err := closeJSONBKeyContainer(writer, frame.object); err != nil {
				return err
			}
			depth--
		}
		if depth == 0 {
			return nil
		}
	}
}

func closeJSONBKeyContainer(writer *jsonbBuildWriter, object bool) error {
	if object {
		return writer.raw("}")
	}
	return writer.raw("]")
}

func (d *keyDecoder) emitJSONBKeyScalar(ctx context.Context, kind, mask byte, writer *jsonbBuildWriter) error {
	before := d.remaining
	defer func() { d.remaining = before }()
	switch kind {
	case 0:
		return writer.raw("null")
	case 1:
		raw, err := d.escaped(ctx, mask, true, 0)
		if err != nil {
			return err
		}
		return writer.quoted(string(raw))
	case 2:
		if d.offset == len(d.data) {
			return storage.ErrInvalidEncoding
		}
		category := d.data[d.offset] ^ mask
		if category == numericKeyNegativeInfinity || category == numericKeyPositiveInfinity || category == numericKeyNaN {
			return storage.ErrInvalidEncoding
		}
		text, err := d.numeric(ctx, mask)
		if err != nil {
			return err
		}
		return writer.raw(text)
	case 3:
		value, err := d.byte(mask)
		if err != nil {
			return err
		}
		if value == 0 {
			return writer.raw("false")
		}
		if value == 1 {
			return writer.raw("true")
		}
	}
	return storage.ErrInvalidEncoding
}

package builtin

import (
	"context"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileGeoIO(signature Signature, options ResolveOptions) scalarKernel {
	switch signature.ID {
	case 117, 119, 121, 123, 347, 1450, 1490:
		return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
			return geoInput(ctx, args[0].StrVal, signature.Result, budget)
		}
	case 118, 120, 122, 124, 348, 1451, 1491:
		return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
			return geoOutput(ctx, args[0], signature.Arguments[0], options.ExtraFloatDigits, budget)
		}
	case 2429, 2481, 2483, 2485, 2487, 2489, 2491:
		return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
			value := args[0]
			if value.Type != model.TypeBytes {
				return model.Value{}, invalidGeometry()
			}
			if err := ValidateGeometryValue(ctx, signature.Arguments[0], value.BytesVal); err != nil {
				return model.Value{}, err
			}
			buffer, err := newByteResult(ctx, uint64(len(value.BytesVal)), budget)
			if err != nil {
				return model.Value{}, err
			}
			defer buffer.close()
			for start := 0; start < len(value.BytesVal); start += 4096 {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
				copy(buffer.data[start:min(start+4096, len(buffer.data))], value.BytesVal[start:min(start+4096, len(buffer.data))])
			}
			return buffer.value(), nil
		}
	}
	return nil
}

// Measure before admission, then render into one owned string. Float spelling
// uses a fixed scratch buffer rather than per-coordinate strings.
func geoOutput(ctx context.Context, value model.Value, id model.TypeID, extraDigits int, budget CallBudget) (model.Value, error) {
	if value.Type != model.TypeBytes {
		return model.Value{}, invalidGeometry()
	}
	if err := ValidateGeometryValue(ctx, id, value.BytesVal); err != nil {
		return model.Value{}, err
	}
	if extraDigits < -15 || extraDigits > 3 {
		return model.Value{}, sqlError("22023", "extra_float_digits must be between -15 and 3")
	}
	writer := geoTextWriter{extraDigits: extraDigits}
	if err := renderGeoOutput(ctx, value.BytesVal, id, &writer); err != nil {
		return model.Value{}, err
	}
	claim, err := reserveOutputBytes(ctx, writer.size, budget)
	if err != nil {
		return model.Value{}, err
	}
	if claim != nil {
		defer claim.Release()
	}
	var builder strings.Builder
	builder.Grow(int(writer.size))
	writer.builder = &builder
	if err := renderGeoOutput(ctx, value.BytesVal, id, &writer); err != nil {
		return model.Value{}, err
	}
	return model.StringVal(builder.String()), nil
}

type geoTextWriter struct {
	builder     *strings.Builder
	size        uint64
	extraDigits int
	scratch     [32]byte
}

func (writer *geoTextWriter) text(text string) {
	if writer.builder == nil {
		writer.size += uint64(len(text))
		return
	}
	writer.builder.WriteString(text)
}

func (writer *geoTextWriter) scalar(value float64) {
	text := appendFloatOutput(writer.scratch[:0], value, 64, writer.extraDigits)
	if writer.builder == nil {
		writer.size += uint64(len(text))
		return
	}
	writer.builder.Write(text)
}

func (writer *geoTextWriter) point(point geoPoint) {
	writer.text("(")
	writer.scalar(point.x)
	writer.text(",")
	writer.scalar(point.y)
	writer.text(")")
}

func renderGeoOutput(ctx context.Context, data []byte, id model.TypeID, writer *geoTextWriter) error {
	switch id {
	case 628:
		writer.text("{")
		writer.scalar(geoReadFloat(data))
		writer.text(",")
		writer.scalar(geoReadFloat(data[8:]))
		writer.text(",")
		writer.scalar(geoReadFloat(data[16:]))
		writer.text("}")
	case 718:
		writer.text("<")
		writer.point(geoReadPoint(data))
		writer.text(",")
		writer.scalar(geoReadFloat(data[16:]))
		writer.text(">")
	default:
		opening, closing := "", ""
		switch id {
		case 601:
			opening, closing = "[", "]"
		case 602:
			opening, closing = "[", "]"
			if data[0] != 0 {
				opening, closing = "(", ")"
			}
			data = data[5:]
		case 604:
			opening, closing = "(", ")"
			data = data[4:]
		}
		writer.text(opening)
		for i := 0; i < len(data); i += 16 {
			if i%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if i > 0 {
				writer.text(",")
			}
			writer.point(geoReadPoint(data[i:]))
		}
		writer.text(closing)
	}
	return ctx.Err()
}

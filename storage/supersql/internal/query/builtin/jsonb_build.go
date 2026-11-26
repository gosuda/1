package builtin

import (
	"context"
	"fmt"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileJSONBBuild(signature Signature, options ResolveOptions) scalarKernel {
	switch signature.ID {
	case 3263, 3264:
		return jsonbObjectArrays
	case 3272, 3274:
		return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
			return jsonbBuildDocument(ctx, budget, func(writer *jsonbBuildWriter) error {
				if signature.ID == 3272 {
					return writer.raw("[]")
				}
				return writer.raw("{}")
			})
		}
	default:
		return nil
	}
}

// Construction uses two passes so the only retained text buffer has exact capacity.
// The existing canonical parser applies numeric limits and last-key-wins ordering.
func jsonbBuildDocument(ctx context.Context, budget CallBudget, emit func(*jsonbBuildWriter) error) (model.Value, error) {
	return jsonBuildDocument(ctx, budget, true, emit)
}

func jsonBuildDocument(ctx context.Context, budget CallBudget, canonicalize bool, emit func(*jsonbBuildWriter) error) (model.Value, error) {
	writer := jsonbBuildWriter{ctx: ctx, budget: budget, canonicalize: canonicalize}
	if err := emit(&writer); err != nil {
		return model.Value{}, err
	}
	size := writer.size
	nested, err := budget.After(uint64(size))
	if err != nil {
		return model.Value{}, err
	}
	if budget.Allocator != nil {
		claim, err := budget.Allocator.Reserve(uint64(size))
		if err != nil {
			return model.Value{}, err
		}
		defer claim.Release()
	}
	var output strings.Builder
	output.Grow(int(size))
	writer.output, writer.size, writer.budget = &output, 0, nested
	if err := emit(&writer); err != nil {
		return model.Value{}, err
	}
	if !canonicalize {
		return model.Value{Type: model.TypeJSONB, StrVal: output.String()}, nil
	}
	canonical, err := CanonicalJSONB(ctx, output.String(), nested)
	if err != nil {
		return model.Value{}, err
	}
	return model.Value{Type: model.TypeJSONB, StrVal: canonical}, nil
}

func CanonicalJSONB(ctx context.Context, text string, budget CallBudget) (string, error) {
	shape, err := walkJSONB(ctx, text, nil)
	if err != nil {
		return "", err
	}
	limit := int64(min(budget.MemoryBytes, uint64((1<<30)-5)))
	const nodeBytes = int64(unsafe.Sizeof(jsonbNode{}))
	const frameBytes = int64(unsafe.Sizeof(jsonbFrame{}))
	if shape.nodes > limit/nodeBytes {
		return "", sqlError("54000", "builtin invocation memory budget exceeded")
	}
	stateBytes := shape.nodes*nodeBytes + int64(shape.depth)*frameBytes
	if stateBytes > limit || shape.decodedBytes > limit-stateBytes {
		return "", sqlError("54000", "builtin invocation memory budget exceeded")
	}
	stateBytes += shape.decodedBytes
	var claim columnar.Allocation
	if budget.Allocator != nil {
		claim, err = budget.Allocator.Reserve(uint64(stateBytes))
		if err != nil {
			return "", err
		}
		defer claim.Release()
	}
	document, err := parseJSONBDocument(ctx, text, limit)
	if err != nil {
		return "", err
	}
	outputBytes := document.nodes[0].bytes
	if err := outputBudget(outputBytes, budget.OutputBytes); err != nil {
		return "", err
	}
	if err := outputBudget(stateBytes+outputBytes, limit); err != nil {
		return "", err
	}
	if claim != nil {
		if err := claim.Resize(uint64(stateBytes + outputBytes)); err != nil {
			return "", err
		}
	}
	return document.encode(ctx, stateBytes+outputBytes, false)
}

type jsonbBuildWriter struct {
	ctx          context.Context
	budget       CallBudget
	output       *strings.Builder
	size         int64
	lineFeeds    *jsonbDatumPlan
	canonicalize bool
}

func (writer *jsonbBuildWriter) raw(text string) error {
	if err := writer.ctx.Err(); err != nil {
		return err
	}
	if err := outputBudget(int64(len(text)), writer.budget.OutputBytes-writer.size); err != nil {
		return err
	}
	writer.size += int64(len(text))
	if err := outputBudget(writer.size, writer.budget.OutputBytes); err != nil {
		return err
	}
	if writer.output != nil {
		for start := 0; start < len(text); {
			if err := writer.ctx.Err(); err != nil {
				return err
			}
			end := start + min(cancellationChunk, len(text)-start)
			writer.output.WriteString(text[start:end])
			start = end
		}
	}
	return nil
}

func (writer *jsonbBuildWriter) quoted(text string) error {
	if _, err := textCharacters(writer.ctx, text); err != nil {
		return err
	}
	size, err := jsonQuotedBytes(writer.ctx, text)
	if err != nil {
		return err
	}
	if err := outputBudget(size, writer.budget.OutputBytes-writer.size); err != nil {
		return err
	}
	writer.size += size
	if err := outputBudget(writer.size, writer.budget.OutputBytes); err != nil {
		return err
	}
	if writer.output != nil {
		return appendJSONQuoted(writer.ctx, writer.output, text)
	}
	return nil
}

func jsonbObjectArrays(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
	return jsonObjectArrayDocument(ctx, args, budget, true)
}

func jsonObjectArrayDocument(ctx context.Context, args []model.Value, budget CallBudget, canonicalize bool) (model.Value, error) {
	for _, arg := range args {
		if arg.IsNull {
			return model.NullVal(model.TypeJSONB), nil
		}
		if err := model.ValidateArrayShape(arg.Dimensions, uint64(len(arg.ArrayVal))); err != nil {
			return model.Value{}, sqlError("2202E", "invalid array dimensions")
		}
	}
	keys := args[0]
	if len(args) == 1 {
		switch len(keys.Dimensions) {
		case 0:
		case 1:
			if len(keys.ArrayVal)%2 != 0 {
				return model.Value{}, sqlError("2202E", "array must have even number of elements")
			}
		case 2:
			if keys.Dimensions[1].Length != 2 {
				return model.Value{}, sqlError("2202E", "array must have two columns")
			}
		default:
			return model.Value{}, sqlError("2202E", "wrong number of array subscripts")
		}
	} else {
		if len(keys.Dimensions) > 1 || len(keys.Dimensions) != len(args[1].Dimensions) {
			return model.Value{}, sqlError("2202E", "wrong number of array subscripts")
		}
		if len(keys.ArrayVal) != len(args[1].ArrayVal) {
			return model.Value{}, sqlError("2202E", "mismatched array dimensions")
		}
	}
	return jsonBuildDocument(ctx, budget, canonicalize, func(writer *jsonbBuildWriter) error {
		if err := writer.raw("{"); err != nil {
			return err
		}
		count := len(keys.ArrayVal)
		if len(args) == 1 {
			count /= 2
		}
		for i := range count {
			key := keys.ArrayVal[i]
			var value model.Value
			if len(args) == 1 {
				key, value = keys.ArrayVal[2*i], keys.ArrayVal[2*i+1]
			} else {
				value = args[1].ArrayVal[i]
			}
			if key.IsNull {
				return sqlError("22004", "null value not allowed for object key")
			}
			if key.Type != model.TypeString || value.Type != model.TypeString {
				return sqlError("42804", "JSONB object array element must be text")
			}
			if i != 0 {
				separator := ","
				if !canonicalize {
					separator = ", "
				}
				if err := writer.raw(separator); err != nil {
					return err
				}
			}
			if err := writer.quoted(key.StrVal); err != nil {
				return err
			}
			separator := ":"
			if !canonicalize {
				separator = " : "
			}
			if err := writer.raw(separator); err != nil {
				return err
			}
			if value.IsNull {
				if err := writer.raw("null"); err != nil {
					return err
				}
			} else if err := writer.quoted(value.StrVal); err != nil {
				return err
			}
		}
		return writer.raw("}")
	})
}

func (codec *jsonbBuildCodec) call(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	values, err := decodeVariadicValues(arguments, 0, codec.array)
	if err != nil {
		return model.Value{}, err
	}
	if values.nullArray {
		return model.NullVal(model.TypeJSONB), nil
	}
	object := codec.signature.ID == 3273 || codec.signature.ID == 3200
	if object && len(values.values)%2 != 0 {
		return model.Value{}, sqlError("22023", "argument list must have even number of elements")
	}
	canonicalize := codec.signature.Result == 3802
	return jsonBuildDocument(ctx, budget, canonicalize, func(writer *jsonbBuildWriter) error {
		open, close := "[", "]"
		if object {
			open, close = "{", "}"
		}
		if err := writer.raw(open); err != nil {
			return err
		}
		for i := range values.values {
			key := object && i%2 == 0
			if key && values.values[i].IsNull {
				if !canonicalize {
					return sqlError("22004", "null value not allowed for object key")
				}
				return sqlError("22023", fmt.Sprintf("argument %d: key must not be null", i+1))
			}
			if i != 0 {
				separator := ","
				if !canonicalize {
					separator = ", "
				}
				if object && !key {
					separator = ":"
					if !canonicalize {
						separator = " : "
					}
				}
				if err := writer.raw(separator); err != nil {
					return err
				}
			}
			plan := i
			if codec.array {
				plan = 0
			}
			if err := codec.plans[plan].emit(writer, values.values[i:i+1], key); err != nil {
				return err
			}
		}
		return writer.raw(close)
	})
}

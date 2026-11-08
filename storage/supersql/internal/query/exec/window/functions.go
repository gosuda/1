package window

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

type tileState struct {
	bucket, rows, boundary, remainder int64
}

func integerArgument(value model.Value) (int64, error) {
	if value.IntVal < math.MinInt32 || value.IntVal > math.MaxInt32 {
		return 0, &model.Error{Code: "22003", Message: "window integer argument out of range"}
	}
	return value.IntVal, nil
}

func (c *cursor) valueAt(ctx context.Context, row int64, call Call) (model.Value, error) {
	if row < 0 || uint64(row) >= c.data.Rows() {
		return model.NullVal(call.argumentLayout(c.options.InputLayouts, 0).Type.Storage), nil
	}
	pos, _, err := c.rows.read(ctx, row)
	if err != nil {
		return model.Value{}, err
	}
	values, err := c.reader.Read(ctx, rowstore.Position(pos))
	if err != nil {
		return model.Value{}, err
	}
	value, err := c.argument(ctx, call, 0, values)
	if err != nil {
		return model.Value{}, err
	}
	if len(call.Expressions) != 0 && call.Expressions[0] != nil {
		return value, nil
	}
	owned, _, err := model.CopyValue(ctx, value, c.options.MaxRecordBytes*8)
	return owned, err
}

func (c *cursor) argument(ctx context.Context, call Call, index int, row []model.Value) (model.Value, error) {
	if len(call.Expressions) != 0 && call.Expressions[index] != nil {
		return call.Expressions[index].Eval(ctx, row)
	}
	return row[call.Arguments[index]], nil
}

func (c *cursor) evaluate(ctx context.Context, call Call, tile *tileState, group, peerStart, peerEnd int64) (model.Value, error) {
	n := int64(c.data.Rows())
	switch call.Function {
	case 3100:
		return model.Int64Val(c.position + 1), nil
	case 3101:
		return model.Int64Val(peerStart + 1), nil
	case 3102:
		return model.Int64Val(group + 1), nil
	case 3103:
		if n == 1 {
			return model.Float64Val(0), nil
		}
		return model.Float64Val(float64(peerStart) / float64(n-1)), nil
	case 3104:
		return model.Float64Val(float64(peerEnd) / float64(n)), nil
	case 3105:
		if tile.bucket == 0 {
			argument, err := c.argument(ctx, call, 0, c.current)
			if err != nil {
				return model.Value{}, err
			}
			if argument.IsNull {
				return model.NullVal(model.TypeInt64), nil
			}
			buckets, err := integerArgument(argument)
			if err != nil {
				return model.Value{}, err
			}
			if buckets <= 0 {
				return model.Value{}, &model.Error{Code: "22014", Message: "ntile argument must be greater than zero"}
			}
			tile.bucket = 1
			tile.boundary = n / buckets
			if tile.boundary == 0 {
				tile.boundary = 1
			} else {
				tile.remainder = n % buckets
				if tile.remainder != 0 {
					tile.boundary++
				}
			}
		}
		tile.rows++
		if tile.rows > tile.boundary {
			if tile.remainder != 0 && tile.bucket == tile.remainder {
				tile.remainder = 0
				tile.boundary--
			}
			tile.bucket++
			tile.rows = 1
		}
		return model.Int64Val(tile.bucket), nil
	}
	null := model.NullVal(call.argumentLayout(c.options.InputLayouts, 0).Type.Storage)
	if call.Function >= 3106 && call.Function <= 3111 {
		offset := int64(1)
		if len(call.Arguments) >= 2 {
			argument, err := c.argument(ctx, call, 1, c.current)
			if err != nil {
				return model.Value{}, err
			}
			if argument.IsNull {
				return null, nil
			}
			offset, err = integerArgument(argument)
			if err != nil {
				return model.Value{}, err
			}
		}
		if call.Function <= 3108 {
			offset = -offset
		}
		target := c.position + offset
		if target < 0 || target >= n {
			if len(call.Arguments) == 3 {
				return c.argument(ctx, call, 2, c.current)
			}
			return null, nil
		}
		return c.valueAt(ctx, target, call)
	}
	nth := int64(1)
	if call.Function == 3114 {
		argument, err := c.argument(ctx, call, 1, c.current)
		if err != nil {
			return model.Value{}, err
		}
		if argument.IsNull {
			return null, nil
		}
		nth, err = integerArgument(argument)
		if err != nil {
			return model.Value{}, err
		}
		if nth <= 0 {
			return model.Value{}, &model.Error{Code: "22016", Message: "nth_value argument must be greater than zero"}
		}
	}
	spans, count, err := c.frame(ctx, group, peerStart, peerEnd)
	if err != nil {
		return model.Value{}, err
	}
	if call.Function == 3113 {
		if count == 0 {
			return null, nil
		}
		return c.valueAt(ctx, spans[count-1].end-1, call)
	}
	for _, span := range spans[:count] {
		length := span.end - span.start
		if nth <= length {
			return c.valueAt(ctx, span.start+nth-1, call)
		}
		nth -= length
	}
	return null, nil
}

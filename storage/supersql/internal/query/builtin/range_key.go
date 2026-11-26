package builtin

import (
	"bytes"
	"cmp"
	"context"
	"math"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// A range orders empty first, then its lower and upper bounds. Inclusivity is
// compared only after the endpoint value, and in opposite orders at each side.
// A multirange is a prefix-free sequence of those ranges, ordered lexically.
type rangeKeyDatum struct {
	value             rangeDatum
	lower, upper      numericKey
	lowerEnumOrder    int
	upperEnumOrder    int
}

func compareRangeNumericKeys(a, b numericKey) int {
	if order := cmp.Compare(a.category, b.category); order != 0 {
		return order
	}
	direction := 1
	if a.category == numericKeyNegative {
		direction = -1
	}
	if order := cmp.Compare(a.exponent, b.exponent); order != 0 {
		return direction * order
	}
	return direction * strings.Compare(a.digits, b.digits)
}

func rangeKeyEndpointOrder(family RangeFamilyInfo, a, b rangeKeyDatum, sideA, sideB rangeBoundSide) int {
	upperA, upperB := sideA == rangeUpper, sideB == rangeUpper
	left, right := a.value.lower, b.value.lower
	ln, rn := a.lower, b.lower
	if upperA {
		left, ln = a.value.upper, a.upper
	}
	if upperB {
		right, rn = b.value.upper, b.upper
	}
	if left.infinite {
		if right.infinite && upperA == upperB {
			return 0
		}
		if upperA {
			return 1
		}
		return -1
	}
	if right.infinite {
		if upperB {
			return -1
		}
		return 1
	}
	if family.enum != nil {
		leftOrder, rightOrder := a.lowerEnumOrder, b.lowerEnumOrder
		if upperA {
			leftOrder = a.upperEnumOrder
		}
		if upperB {
			rightOrder = b.upperEnumOrder
		}
		return cmp.Compare(leftOrder, rightOrder)
	}
	switch family.subtypeStorage() {
	case model.TypeNumeric:
		return compareRangeNumericKeys(ln, rn)
	case model.TypeString, model.TypeUUID:
		return strings.Compare(left.numeric, right.numeric)
	case model.TypeBytes, model.TypeJSONB:
		return bytes.Compare(left.bytes, right.bytes)
	case model.TypeFloat64:
		leftNaN, rightNaN := math.IsNaN(left.floating), math.IsNaN(right.floating)
		if leftNaN || rightNaN {
			if leftNaN == rightNaN {
				return 0
			}
			if leftNaN {
				return 1
			}
			return -1
		}
		return cmp.Compare(cmp.Or(left.floating, 0), cmp.Or(right.floating, 0))
	case model.TypeInterval:
		return compareInterval(left.interval, right.interval)
	default:
		return cmp.Compare(left.integer, right.integer)
	}
}

func prepareRangeKey(ctx context.Context, family RangeFamilyInfo, value rangeDatum, maximum uint64) (rangeKeyDatum, uint64, error) {
	prepared := rangeKeyDatum{value: value}
	size := uint64(1)
	if size > maximum {
		return prepared, 0, columnar.ErrBatchBudget
	}
	if value.empty {
		return prepared, size, nil
	}
	for i, bound := range [2]rangeBound{value.lower, value.upper} {
		if size == maximum {
			return prepared, 0, columnar.ErrBatchBudget
		}
		size++
		if bound.infinite {
			continue
		}
		if family.subtypeStorage() == model.TypeNumeric {
			numeric, bytes, err := prepareNumericKey(ctx, bound.numeric, maximum-size)
			if err != nil {
				return prepared, 0, err
			}
			if i == 0 {
				prepared.lower = numeric
			} else {
				prepared.upper = numeric
			}
			size += bytes
			continue
		}
		if family.enum != nil {
			order, err := family.enum.enumOrder(borrowRangeBoundValue(family, bound))
			if err != nil {
				return prepared, 0, err
			}
			if i == 0 {
				prepared.lowerEnumOrder = order
			} else {
				prepared.upperEnumOrder = order
			}
			if maximum-size < 8 {
				return prepared, 0, columnar.ErrBatchBudget
			}
			size += 8
			continue
		}
		v := borrowRangeBoundValue(family, bound)
		typ := model.Type{ID: family.representation(), Storage: v.Type}
		bytes, err := keyFieldSize(ctx, typ, &v, maximum-size)
		if err != nil {
			return prepared, 0, err
		}
		size += bytes
	}
	order := rangeKeyEndpointOrder(family, prepared, prepared, rangeLower, rangeUpper)
	exclusive := !value.lower.inclusive || !value.upper.inclusive
	if order > 0 || (order == 0 && exclusive) {
		return prepared, 0, invalidRangeValue()
	}
	return prepared, size, ctx.Err()
}

func appendRangeKeyDatum(ctx context.Context, output []byte, family RangeFamilyInfo, prepared rangeKeyDatum, mask byte) ([]byte, error) {
	value := prepared.value
	if value.empty {
		return append(output, mask), nil
	}
	output = append(output, 1^mask)
	for i, bound := range [2]rangeBound{value.lower, value.upper} {
		marker := byte(0)
		if (i == 1) == bound.infinite {
			marker = 1
		}
		output = append(output, marker^mask)
		if bound.infinite {
			continue
		}
		if family.subtypeStorage() == model.TypeNumeric {
			numeric := prepared.lower
			if i == 1 {
				numeric = prepared.upper
			}
			var err error
			output, err = appendNumericKey(ctx, output, numeric, mask != 0)
			if err != nil {
				return nil, err
			}
		} else if family.enum != nil {
			order := prepared.lowerEnumOrder
			if i == 1 {
				order = prepared.upperEnumOrder
			}
			value := model.Int64Val(int64(order))
			column := storage.KeyColumn{Type: model.Type{ID: 20, Storage: model.TypeInt64, Modifier: -1}, Descending: mask != 0}
			var err error
			output, err = appendKeyValue(ctx, output, &column, &value)
			if err != nil {
				return nil, err
			}
		} else {
			value := borrowRangeBoundValue(family, bound)
			column := storage.KeyColumn{Type: model.Type{ID: family.representation(), Storage: value.Type}, Descending: mask != 0}
			var err error
			output, err = appendKeyValue(ctx, output, &column, &value)
			if err != nil {
				return nil, err
			}
		}
		inclusive := byte(0)
		if (i == 1) == bound.inclusive {
			inclusive = 1
		}
		output = append(output, inclusive^mask)
	}
	return output, ctx.Err()
}

// walkRangeKey retains only the current and preceding component, never state
// proportional to a multirange or an enclosing array. A nil output sizes only.
func walkRangeKey(ctx context.Context, family RangeFamilyInfo, multi bool, value model.Value, output []byte, mask byte, maximum uint64) ([]byte, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if !multi {
		decoded, err := decodeRange(ctx, family, value)
		if err != nil {
			return nil, 0, err
		}
		prepared, size, err := prepareRangeKey(ctx, family, decoded, maximum)
		if err != nil {
			return nil, 0, err
		}
		if output != nil {
			output, err = appendRangeKeyDatum(ctx, output, family, prepared, mask)
		}
		return output, size, err
	}
	cursor, err := newMultirangeCursor(family, value)
	if err != nil {
		return nil, 0, err
	}
	size := uint64(1)
	if size > maximum {
		return nil, 0, columnar.ErrBatchBudget
	}
	var previous rangeKeyDatum
	first := true
	for {
		decoded, ok, err := cursor.next(ctx)
		if err != nil {
			return nil, 0, err
		}
		if !ok {
			break
		}
		if size == maximum {
			return nil, 0, columnar.ErrBatchBudget
		}
		size++
		prepared, bytes, err := prepareRangeKey(ctx, family, decoded, maximum-size)
		if err != nil {
			return nil, 0, err
		}
		size += bytes
		if !first {
			order := rangeKeyEndpointOrder(family, previous, prepared, rangeUpper, rangeLower)
			coveredBoundary := previous.value.upper.inclusive || prepared.value.lower.inclusive
			if order > 0 || (order == 0 && coveredBoundary) {
				return nil, 0, invalidRangeValue()
			}
		}
		previous, first = prepared, false
		if output != nil {
			output = append(output, 1^mask)
			output, err = appendRangeKeyDatum(ctx, output, family, prepared, mask)
			if err != nil {
				return nil, 0, err
			}
		}
	}
	if output != nil {
		output = append(output, mask)
	}
	return output, size, ctx.Err()
}

func rangeKeyFieldSize(ctx context.Context, family RangeFamilyInfo, id model.TypeID, value model.Value, maximum uint64) (uint64, error) {
	if maximum == 0 {
		return 0, columnar.ErrBatchBudget
	}
	_, size, err := walkRangeKey(ctx, family, id == family.Multirange, value, nil, 0, maximum-1)
	return size + 1, err
}

func appendRangeKey(ctx context.Context, output []byte, family RangeFamilyInfo, id model.TypeID, value model.Value, mask byte) ([]byte, error) {
	result, _, err := walkRangeKey(ctx, family, id == family.Multirange, value, output, mask, uint64(cap(output)-len(output)))
	return result, err
}

func rangeKeyGenericInput(typ model.Type, input model.TypeID) bool {
	family, ok := LookupRangeFamily(typ.ID)
	return ok && typ.Storage == model.TypeBytes && ((input == 3831 && typ.ID == family.Range) || (input == 4537 && typ.ID == family.Multirange))
}

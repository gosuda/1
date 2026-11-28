package builtin

import (
	"context"
	"fmt"
	"math"
	"math/bits"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileBitString(signature Signature, _ ResolveOptions) scalarKernel {
	switch signature.ID {
	case 1564, 1565, 1579, 1580, 1581, 1582, 1592, 1593, 1594, 1595, 1596,
		1666, 1667, 1668, 1669, 1670, 1671, 1672, 1673, 1674, 1675, 1676,
		1677, 1678, 1679, 1680, 1681, 1682, 1683, 1684, 1685, 1687, 1698,
		1699, 1812, 2075, 2076, 2457, 2459, 3030, 3031, 3032, 3033, 6162:
	case 2902, 2919, 2920, 2921:
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		id := signature.ID
		switch id {
		case 2902, 2919:
			return bitStringTypmodIn(ctx, args[0], id == 2902)
		case 2920, 2921:
			return bitStringTypmodOut(ctx, int32(args[0].IntVal), budget)
		case 1564, 1579:
			return parseBitString(ctx, args[0].StrVal, int32(args[2].IntVal), id == 1579, budget)
		case 1683, 2075:
			value, length := args[0].IntVal, int64(int32(args[1].IntVal))
			if id == 1683 {
				value = int64(int32(value))
			}
			if length <= 0 || length > bitStringMaxLen {
				length = 1
			}
			return newBitString(ctx, length, budget, func(output []byte) error {
				for i := range output {
					if i%4096 == 0 {
						if err := ctx.Err(); err != nil {
							return err
						}
					}
					remaining := length - int64(i)*8
					if remaining >= 8 {
						output[i] = byte(value >> uint(remaining-8))
					} else {
						output[i] = byte(value << uint(8-remaining))
					}
				}
				return nil
			})
		}
		left, err := decodeBitString(args[0].BytesVal)
		if err != nil {
			return model.Value{}, err
		}
		var right bitString
		switch id {
		case 1581, 1582, 1592, 1593, 1594, 1595, 1596, 1666, 1667, 1668, 1669, 1670, 1671, 1672,
			1673, 1674, 1675, 1679, 1698, 3030, 3031:
			right, err = decodeBitString(args[1].BytesVal)
			if err != nil {
				return model.Value{}, err
			}
		}
		switch id {
		case 1581, 1582, 1592, 1593, 1594, 1595, 1596, 1666, 1667, 1668, 1669, 1670, 1671, 1672:
			order, err := compareBitStrings(ctx, left, right)
			if err != nil {
				return model.Value{}, err
			}
			switch id {
			case 1581, 1666:
				return model.BoolVal(order == 0), nil
			case 1582, 1667:
				return model.BoolVal(order != 0), nil
			case 1592, 1668:
				return model.BoolVal(order >= 0), nil
			case 1593, 1669:
				return model.BoolVal(order > 0), nil
			case 1594, 1670:
				return model.BoolVal(order <= 0), nil
			case 1595, 1671:
				return model.BoolVal(order < 0), nil
			default:
				return model.Int64Val(int64(order)), nil
			}
		case 1565, 1580:
			return formatBitString(ctx, left, budget)
		case 1681, 1812:
			return model.Int64Val(int64(left.bitLen)), nil
		case 1682:
			return model.Int64Val(int64(len(left.data))), nil
		case 6162:
			var count int64
			for i, value := range left.data {
				if i%4096 == 0 {
					if err := ctx.Err(); err != nil {
						return model.Value{}, err
					}
				}
				count += int64(bits.OnesCount8(value))
			}
			return model.Int64Val(count), nil
		case 1684, 2076:
			width, name := 32, "integer"
			if id == 2076 {
				width, name = 64, "bigint"
			}
			if left.bitLen > width {
				return model.Value{}, sqlError("22003", name+" out of range")
			}
			var value uint64
			for _, b := range left.data {
				value = value<<8 | uint64(b)
			}
			value >>= uint((8 - left.bitLen%8) % 8)
			if id == 1684 {
				return model.Int64Val(int64(int32(value))), nil
			}
			return model.Int64Val(int64(value)), nil
		case 1698:
			position, err := positionBitString(ctx, left, right)
			return model.Int64Val(int64(position)), err
		case 1680, 1699:
			length := int32(0)
			if id == 1680 {
				length = int32(args[2].IntVal)
			}
			start, count, err := bitSubstringRange(left.bitLen, int32(args[1].IntVal), length, id == 1699)
			if err != nil {
				return model.Value{}, err
			}
			return newBitString(ctx, int64(count), budget, func(output []byte) error { return copyBitString(ctx, output, 0, left, start, count) })
		case 3030, 3031:
			start, length := int64(int32(args[2].IntVal)), int64(right.bitLen)
			if id == 3030 {
				length = int64(int32(args[3].IntVal))
			}
			if start <= 0 {
				return model.Value{}, sqlError("22011", "negative substring length not allowed")
			}
			end := start + length
			if end < math.MinInt32 || end > math.MaxInt32 {
				return model.Value{}, sqlError("22003", "integer out of range")
			}
			prefix := min(int(start-1), left.bitLen)
			suffixStart, suffix, err := bitSubstringRange(left.bitLen, int32(end), 0, true)
			if err != nil {
				return model.Value{}, err
			}
			return newBitString(ctx, int64(prefix)+int64(right.bitLen)+int64(suffix), budget, func(output []byte) error {
				if err := copyBitString(ctx, output, 0, left, 0, prefix); err != nil {
					return err
				}
				if err := copyBitString(ctx, output, prefix, right, 0, right.bitLen); err != nil {
					return err
				}
				return copyBitString(ctx, output, prefix+right.bitLen, left, suffixStart, suffix)
			})
		case 1679:
			return newBitString(ctx, int64(left.bitLen)+int64(right.bitLen), budget, func(output []byte) error {
				if err := copyBitString(ctx, output, 0, left, 0, left.bitLen); err != nil {
					return err
				}
				return copyBitString(ctx, output, left.bitLen, right, 0, right.bitLen)
			})
		case 1685, 1687:
			length := int(int32(args[1].IntVal))
			unchangedLength := length <= 0 || length > bitStringMaxLen || length == left.bitLen
			varyingFits := id == 1687 && length >= left.bitLen
			if unchangedLength || varyingFits {
				length = left.bitLen
			}
			if length != left.bitLen && args[2].IntVal == 0 {
				if id == 1687 {
					return model.Value{}, sqlError("22001", fmt.Sprintf("bit string too long for type bit varying(%d)", length))
				}
				return model.Value{}, sqlError("22026", fmt.Sprintf("bit string length %d does not match type bit(%d)", left.bitLen, length))
			}
			return newBitString(ctx, int64(length), budget, func(output []byte) error { return copyBitString(ctx, output, 0, left, 0, min(length, left.bitLen)) })
		case 3032, 3033:
			index := int(int32(args[1].IntVal))
			if index < 0 || index >= left.bitLen {
				return model.Value{}, sqlError("2202E", fmt.Sprintf("bit index %d out of valid range (0..%d)", index, left.bitLen-1))
			}
			if id == 3032 {
				return model.Int64Val(int64(left.bit(index))), nil
			}
			newBit := int32(args[2].IntVal)
			if newBit != 0 && newBit != 1 {
				return model.Value{}, sqlError("22023", "new bit must be 0 or 1")
			}
			return newBitString(ctx, int64(left.bitLen), budget, func(output []byte) error {
				if err := copyBitString(ctx, output, 0, left, 0, left.bitLen); err != nil {
					return err
				}
				mask := byte(1 << (7 - uint(index%8)))
				output[index/8] = output[index/8]&^mask | byte(newBit)<<(7-uint(index%8))
				return nil
			})
		case 1677, 1678:
			shift := int64(int32(args[1].IntVal))
			if id == 1678 {
				shift = -shift
			}
			return newBitString(ctx, int64(left.bitLen), budget, func(output []byte) error {
				if shift >= int64(left.bitLen) || shift <= -int64(left.bitLen) {
					return nil
				}
				if shift >= 0 {
					return copyBitString(ctx, output, 0, left, int(shift), left.bitLen-int(shift))
				}
				return copyBitString(ctx, output, int(-shift), left, 0, left.bitLen+int(shift))
			})
		case 1673, 1674, 1675:
			if left.bitLen != right.bitLen {
				name := "AND"
				if id == 1674 {
					name = "OR"
				} else if id == 1675 {
					name = "XOR"
				}
				return model.Value{}, sqlError("22026", "cannot "+name+" bit strings of different sizes")
			}
		}
		return newBitString(ctx, int64(left.bitLen), budget, func(output []byte) error {
			for i := range output {
				if i%4096 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				switch id {
				case 1673:
					output[i] = left.data[i] & right.data[i]
				case 1674:
					output[i] = left.data[i] | right.data[i]
				case 1675:
					output[i] = left.data[i] ^ right.data[i]
				case 1676:
					output[i] = ^left.data[i]
				default:
					output[i] = left.data[i] // bit_send and varbit_send use the canonical wire layout.
				}
			}
			return nil
		})
	}
}

func bitSubstringRange(size int, start, length int32, unbounded bool) (int, int, error) {
	first := max(int64(start), 1)
	end := int64(size) + 1
	if !unbounded {
		if length < 0 {
			return 0, 0, sqlError("22011", "negative substring length not allowed")
		}
		requested := int64(start) + int64(length)
		if requested <= math.MaxInt32 {
			end = min(end, requested)
		}
	}
	if first > int64(size) || end <= first {
		return 0, 0, nil
	}
	return int(first - 1), int(end - first), nil
}

// Rabin-Karp retains constant scratch space for packed bit strings.
func positionBitString(ctx context.Context, input, needle bitString) (int, error) {
	if input.bitLen == 0 || needle.bitLen > input.bitLen {
		return 0, nil
	}
	if needle.bitLen == 0 {
		return 1, nil
	}
	const prime uint64 = 16777619
	var hash, wanted uint64
	power := uint64(1)
	for i := range needle.bitLen {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		hash = hash*prime + uint64(input.bit(i))
		wanted = wanted*prime + uint64(needle.bit(i))
		power *= prime
	}
	for position := 0; ; position++ {
		if position%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if hash == wanted {
			matched, err := bitStringMatches(ctx, input, needle, position)
			if err != nil {
				return 0, err
			}
			if matched {
				return position + 1, nil
			}
		}
		if position == input.bitLen-needle.bitLen {
			return 0, nil
		}
		hash = hash*prime + uint64(input.bit(position+needle.bitLen)) - power*uint64(input.bit(position))
	}
}

func bitStringMatches(ctx context.Context, input, needle bitString, position int) (bool, error) {
	for i := range needle.bitLen {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		if input.bit(position+i) != needle.bit(i) {
			return false, nil
		}
	}
	return true, nil
}

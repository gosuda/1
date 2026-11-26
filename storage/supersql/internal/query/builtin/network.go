package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileNetwork(signature Signature, _ ResolveOptions) scalarKernel {
	switch signature.ID {
	case 422, 598, 599, 605, 635, 683, 696, 697, 698, 699, 711, 730, 779, 910, 911,
		920, 921, 922, 923, 924, 925, 926, 927, 928, 929, 930, 1267, 1362, 1427, 1715,
		2497, 2499, 2627, 2628, 2629, 2630, 2631, 2632, 2633, 3551, 3562, 3563, 4063, 4071:
	default:
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		id := signature.ID
		if id == 910 || id == 1267 {
			a, err := parseNetwork(ctx, args[0].StrVal, id == 1267)
			if err != nil {
				return model.Value{}, err
			}
			return networkResult(ctx, a, budget)
		}
		input := 0
		if id == 2631 {
			input = 1
		}
		a, err := decodeNetwork(args[input].BytesVal, signature.Arguments[input] == 650)
		if err != nil {
			return model.Value{}, err
		}
		switch id {
		case 422:
			hash, err := pgHashBytes(ctx, args[0].BytesVal)
			return model.Int64Val(int64(int32(hash))), err
		case 779:
			hash, err := pgHashBytesExtended(ctx, args[0].BytesVal, uint64(args[1].IntVal))
			return model.Int64Val(int64(hash)), err
		case 598, 599, 699, 730, 911, 1427:
			var buffer [64]byte
			out := buffer[:0]
			if id == 599 {
				out = appendNetworkAbbrev(out, a)
			} else {
				out = appendNetworkHost(out, a)
				forcePrefix := id == 730 || id == 1427
				nonHostPrefix := id != 699 && int(a.prefix) != a.size()*8
				if forcePrefix || nonHostPrefix {
					out = appendNetworkPrefix(out, a.prefix)
				}
			}
			return networkOwnedResult(ctx, out, true, budget)
		case 697:
			return model.Int64Val(int64(a.prefix)), nil
		case 711:
			family := int64(4)
			if a.family == 3 {
				family = 6
			}
			return model.Int64Val(family), nil
		case 605, 635:
			prefix := args[1].IntVal
			if prefix == -1 {
				prefix = int64(a.size() * 8)
			}
			if prefix < 0 || prefix > int64(a.size()*8) {
				return model.Value{}, sqlError("22023", "invalid mask length")
			}
			a.prefix = byte(prefix)
			if id == 635 {
				a = a.network()
			}
			return networkResult(ctx, a, budget)
		case 683, 1715:
			return networkResult(ctx, a.network(), budget)
		case 696, 1362, 698:
			for i := range a.size() {
				mask := networkMask(int(a.prefix), i)
				switch id {
				case 696:
					a.address[i] = mask
				case 1362:
					a.address[i] = ^mask
				case 698:
					a.address[i] |= ^mask
				}
			}
			if id != 698 {
				a.prefix = byte(a.size() * 8)
			}
			return networkResult(ctx, a, budget)
		case 2497, 2499:
			var buffer [20]byte
			buffer[0], buffer[1], buffer[3] = a.family, a.prefix, byte(a.size())
			if id == 2499 {
				buffer[2] = 1
			}
			copy(buffer[4:], a.address[:a.size()])
			return networkOwnedResult(ctx, buffer[:a.size()+4], false, budget)
		case 2627:
			for i := range a.size() {
				a.address[i] = ^a.address[i]
			}
			return networkResult(ctx, a, budget)
		case 2630, 2631, 2632:
			addend := args[1-input].IntVal
			a, err = networkAdd(a, addend, id == 2632)
			if err != nil {
				return model.Value{}, err
			}
			return networkResult(ctx, a, budget)
		}
		b, err := decodeNetwork(args[1].BytesVal, signature.Arguments[1] == 650)
		if err != nil {
			return model.Value{}, err
		}
		switch id {
		case 920:
			return model.BoolVal(compareNetwork(a, b) == 0), nil
		case 921:
			return model.BoolVal(compareNetwork(a, b) < 0), nil
		case 922:
			return model.BoolVal(compareNetwork(a, b) <= 0), nil
		case 923:
			return model.BoolVal(compareNetwork(a, b) > 0), nil
		case 924:
			return model.BoolVal(compareNetwork(a, b) >= 0), nil
		case 925:
			return model.BoolVal(compareNetwork(a, b) != 0), nil
		case 926:
			return model.Int64Val(int64(compareNetwork(a, b))), nil
		case 3562, 3563:
			order := compareNetwork(a, b)
			chooseLarger := id == 3562 && order <= 0
			chooseSmaller := id == 3563 && order >= 0
			if chooseLarger || chooseSmaller {
				a = b
			}
			return networkResult(ctx, a, budget)
		case 4071:
			return model.BoolVal(a.family == b.family), nil
		case 927, 928, 929, 930, 3551:
			match := a.family == b.family && networkBitCompare(a, b, int(min(a.prefix, b.prefix))) == 0
			switch id {
			case 927:
				match = match && a.prefix > b.prefix
			case 928:
				match = match && a.prefix >= b.prefix
			case 929:
				match = match && a.prefix < b.prefix
			case 930:
				match = match && a.prefix <= b.prefix
			}
			return model.BoolVal(match), nil
		case 4063:
			if a.family != b.family {
				return model.Value{}, sqlError("22023", "cannot merge addresses from different families")
			}
			prefix := 0
			for prefix < int(min(a.prefix, b.prefix)) {
				mask := byte(0x80 >> uint(prefix%8))
				if a.address[prefix/8]&mask != b.address[prefix/8]&mask {
					break
				}
				prefix++
			}
			a.prefix = byte(prefix)
			return networkResult(ctx, a.network(), budget)
		case 2628, 2629:
			if a.family != b.family {
				if id == 2628 {
					return model.Value{}, sqlError("22023", "cannot AND inet values of different sizes")
				}
				return model.Value{}, sqlError("22023", "cannot OR inet values of different sizes")
			}
			for i := range a.size() {
				if id == 2628 {
					a.address[i] &= b.address[i]
				} else {
					a.address[i] |= b.address[i]
				}
			}
			a.prefix = max(a.prefix, b.prefix)
			return networkResult(ctx, a, budget)
		case 2633:
			value, err := networkDifference(a, b)
			return model.Int64Val(value), err
		}
		return model.Value{}, sqlError("XX000", "unhandled network operation")
	}
}

func networkAdd(a networkAddress, value int64, subtract bool) (networkAddress, error) {
	if subtract {
		value = -value
	}
	carry := 0
	for i := a.size() - 1; i >= 0; i-- {
		carry = int(a.address[i]) + int(value&255) + carry
		a.address[i] = byte(carry)
		carry >>= 8
		value >>= 8
	}
	nonnegativeFits := value == 0 && carry == 0
	negativeFits := value == -1 && carry == 1
	if !nonnegativeFits && !negativeFits {
		return networkAddress{}, sqlError("22003", "result is out of range")
	}
	return a, nil
}

func networkDifference(a, b networkAddress) (int64, error) {
	if a.family != b.family {
		return 0, sqlError("22023", "cannot subtract inet values of different sizes")
	}
	var result uint64
	carry := 1
	count := 0
	for i := a.size() - 1; i >= 0; i-- {
		carry = int(a.address[i]) + int(^b.address[i]) + carry
		octet := byte(carry)
		if count < 8 {
			result |= uint64(octet) << uint(count*8)
		} else {
			expected := byte(0)
			if int64(result) < 0 {
				expected = 255
			}
			if octet != expected {
				return 0, sqlError("22003", "result is out of range")
			}
		}
		carry >>= 8
		count++
	}
	if carry == 0 && count < 8 {
		result |= ^uint64(0) << uint(count*8)
	}
	return int64(result), nil
}

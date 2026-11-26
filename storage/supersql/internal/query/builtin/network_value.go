package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// networkAddress is the validated, fixed-size inet/cidr payload. IPv4 occupies
// the first four address bytes; the remaining bytes are zero.
type networkAddress struct {
	family  byte
	prefix  byte
	address [16]byte
}

func (a networkAddress) size() int {
	if a.family == 2 {
		return 4
	}
	return 16
}

func decodeNetwork(input []byte, cidr bool) (networkAddress, error) {
	var a networkAddress
	if len(input) < 2 || (input[0] != 2 && input[0] != 3) {
		return a, sqlError("22P03", "invalid network address family")
	}
	a.family, a.prefix = input[0], input[1]
	if len(input) != a.size()+2 || int(a.prefix) > a.size()*8 {
		return networkAddress{}, sqlError("22P03", "invalid network address length")
	}
	copy(a.address[:], input[2:])
	if cidr && !a.hostIsZero() {
		return networkAddress{}, sqlError("22P03", "invalid cidr value: bits set to right of mask")
	}
	return a, nil
}

func networkMask(prefix, index int) byte {
	return byte(0xff << (8 - min(8, max(0, prefix-index*8))))
}

func (a networkAddress) hostIsZero() bool {
	for i := range a.size() {
		if a.address[i] & ^networkMask(int(a.prefix), i) != 0 {
			return false
		}
	}
	return true
}

func (a networkAddress) network() networkAddress {
	for i := range a.size() {
		a.address[i] &= networkMask(int(a.prefix), i)
	}
	return a
}

func networkBitCompare(a, b networkAddress, count int) int {
	for i := range (count + 7) / 8 {
		mask := networkMask(count, i)
		left, right := a.address[i]&mask, b.address[i]&mask
		if left != right {
			if count-i*8 < 8 {
				if left < right {
					return -1
				}
				return 1
			}
			return int(left) - int(right)
		}
	}
	return 0
}

func compareNetwork(a, b networkAddress) int {
	if a.family != b.family {
		return int(a.family) - int(b.family)
	}
	if order := networkBitCompare(a, b, int(min(a.prefix, b.prefix))); order != 0 {
		return order
	}
	if a.prefix != b.prefix {
		return int(a.prefix) - int(b.prefix)
	}
	return networkBitCompare(a, b, a.size()*8)
}

func networkResult(ctx context.Context, a networkAddress, budget CallBudget) (model.Value, error) {
	var payload [18]byte
	payload[0], payload[1] = a.family, a.prefix
	copy(payload[2:], a.address[:a.size()])
	return networkOwnedResult(ctx, payload[:a.size()+2], false, budget)
}

func networkOwnedResult(ctx context.Context, data []byte, text bool, budget CallBudget) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if err := outputBudget(int64(len(data)), budget.OutputBytes); err != nil {
		return model.Value{}, err
	}
	if uint64(len(data)) > budget.MemoryBytes {
		return model.Value{}, sqlError("54000", "builtin invocation memory budget exceeded")
	}
	if len(data) != 0 && budget.Allocator != nil {
		claim, err := budget.Allocator.Reserve(uint64(len(data)))
		if err != nil {
			return model.Value{}, err
		}
		defer claim.Release()
	}
	if text {
		return model.StringVal(string(data)), nil
	}
	result := make([]byte, len(data))
	copy(result, data)
	return model.BytesVal(result), nil
}

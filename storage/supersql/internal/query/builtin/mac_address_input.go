package builtin

import (
	"context"
	"math"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func parseMACAddress(ctx context.Context, text string, typeID model.TypeID) ([8]byte, error) {
	for start := 0; start < len(text); {
		if err := ctx.Err(); err != nil {
			return [8]byte{}, err
		}
		end := start + min(cancellationChunk, len(text)-start)
		if offset := strings.IndexByte(text[start:end], 0); offset >= 0 {
			text = text[:start+offset]
			break
		}
		start = end
	}
	if typeID == 774 {
		return parseMACAddress8(ctx, text)
	}
	return parseMACAddress6(ctx, text)
}

func parseMACAddress8(ctx context.Context, text string) ([8]byte, error) {
	var address [8]byte
	position, err := recordInputSpace(ctx, text, 0)
	if err != nil {
		return address, err
	}
	var spacer byte
	count := 0
	// mac8.c deliberately loops only while two characters remain. A final
	// unpaired character, or a separator after the final byte, is accepted.
	for position+1 < len(text) {
		if count == len(address) {
			return address, invalidInput()
		}
		high, low := digit(text[position]), digit(text[position+1])
		if high < 0 || low < 0 {
			return address, invalidInput()
		}
		address[count] = byte(high<<4 | low)
		count++
		position += 2
		if position < len(text) {
			separator := text[position]
			if separator == ':' || separator == '-' || separator == '.' {
				if spacer != 0 && spacer != separator {
					return address, invalidInput()
				}
				spacer = separator
				position++
			}
		}
		completeWidth := count == 6 || count == 8
		if completeWidth && position < len(text) && isTSSpace(text[position]) {
			position, err = recordInputSpace(ctx, text, position)
			if err != nil {
				return address, err
			}
			if position != len(text) {
				return address, invalidInput()
			}
		}
	}
	if count == 6 {
		address = expandMACAddress(address)
	} else if count != 8 {
		return address, invalidInput()
	}
	return address, ctx.Err()
}

func parseMACAddress6(ctx context.Context, text string) ([8]byte, error) {
	// These are mac.c's seven sscanf formats, in their original precedence.
	formats := [...]struct {
		width     int
		group     int
		separator byte
	}{
		{0, 1, ':'}, {0, 1, '-'}, {2, 3, ':'}, {2, 3, '-'},
		{2, 2, '.'}, {2, 2, '-'}, {2, 6, 0},
	}
	for _, format := range formats {
		var address [8]byte
		position := 0
		matched, outOfRange := true, false
		for i := range 6 {
			value, next, ok, err := scanMACAddressHex(ctx, text, position, format.width)
			if err != nil {
				return address, err
			}
			if !ok {
				matched = false
				break
			}
			position = next
			address[i] = byte(value)
			outOfRange = outOfRange || value > 255
			if i != 5 && (i+1)%format.group == 0 {
				if position == len(text) || text[position] != format.separator {
					matched = false
					break
				}
				position++
			}
		}
		if !matched {
			continue
		}
		position, err := recordInputSpace(ctx, text, position)
		if err != nil {
			return address, err
		}
		if position != len(text) {
			continue
		}
		if outOfRange {
			return address, sqlError("22003", "invalid octet value in macaddr value")
		}
		return address, ctx.Err()
	}
	return [8]byte{}, invalidInput()
}

// PostgreSQL's %x accepts whitespace, a sign, and an optional 0x prefix.
// Width counts sign/prefix characters but not leading whitespace; conversion
// uses unsigned long before storing the low 32 bits in each int octet.
func scanMACAddressHex(ctx context.Context, text string, position, width int) (uint32, int, bool, error) {
	position, err := recordInputSpace(ctx, text, position)
	if err != nil {
		return 0, position, false, err
	}
	end := len(text)
	if width != 0 {
		end = position + min(width, end-position)
	}
	negative := false
	if position < end && (text[position] == '+' || text[position] == '-') {
		negative = text[position] == '-'
		position++
	}
	if position == end || digit(text[position]) < 0 {
		return 0, position, false, nil
	}
	var value uint64
	hexPrefix := position+1 < end && text[position] == '0'
	if hexPrefix && (text[position+1] == 'x' || text[position+1] == 'X') {
		position += 2
	}
	overflow := false
	for position < end {
		if position%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return 0, position, false, err
			}
		}
		hex := digit(text[position])
		if hex < 0 {
			break
		}
		if value > (math.MaxUint64-uint64(hex))/16 {
			overflow = true
		}
		value = value*16 + uint64(hex)
		position++
	}
	if overflow {
		value = math.MaxUint64
	} else if negative {
		value = -value
	}
	return uint32(value), position, true, nil
}

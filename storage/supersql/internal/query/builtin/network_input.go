package builtin

import (
	"context"
	"strings"
)

func parseNetwork(ctx context.Context, text string, cidr bool) (networkAddress, error) {
	var a networkAddress
	for start := 0; start < len(text); {
		if err := ctx.Err(); err != nil {
			return a, err
		}
		end := min(start+4096, len(text))
		if nul := strings.IndexByte(text[start:end], 0); nul >= 0 {
			text = text[:start+nul]
			break
		}
		start = end
	}
	a.family = 2
	var err error
	if strings.Contains(text, ":") {
		a.family = 3
		err = parseNetworkIPv6(text, &a)
	} else {
		err = parseNetworkIPv4(ctx, text, cidr, &a)
	}
	if err != nil {
		return networkAddress{}, err
	}
	if cidr && !a.hostIsZero() {
		return networkAddress{}, sqlError("22P02", "invalid cidr value: bits set to right of mask")
	}
	return a, ctx.Err()
}

func networkPrefix(ctx context.Context, text string, ipv6 bool) (int, error) {
	leadingZero := len(text) > 1 && text[0] == '0'
	if text == "" || (ipv6 && leadingZero) {
		return 0, invalidInput()
	}
	value := 0
	limit := 32
	if ipv6 {
		limit = 128
	}
	for i := range len(text) {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if text[i] < '0' || text[i] > '9' {
			return 0, invalidInput()
		}
		value = value*10 + int(text[i]-'0')
		if value > limit {
			return 0, invalidInput()
		}
	}
	return value, nil
}

func parseNetworkIPv4(ctx context.Context, text string, cidr bool, a *networkAddress) error {
	address, suffix, hasPrefix := strings.Cut(text, "/")
	count := 0
	hexPrefix := len(address) > 2 && address[0] == '0'
	hexadecimal := hexPrefix && (address[1] == 'x' || address[1] == 'X')
	if cidr && hexadecimal {
		if len(address) > 10 {
			return invalidInput()
		}
		for i := 2; i < len(address); i++ {
			d := digit(address[i])
			if d < 0 {
				return invalidInput()
			}
			if i%2 == 0 {
				a.address[count] = byte(d << 4)
				count++
			} else {
				a.address[count-1] |= byte(d)
			}
		}
	} else {
		var err error
		count, err = parseNetworkIPv4Decimal(ctx, address, cidr, a)
		if err != nil {
			return err
		}
	}
	if count == 0 {
		return invalidInput()
	}
	prefix := 32
	if hasPrefix {
		var err error
		prefix, err = networkPrefix(ctx, suffix, false)
		if err != nil {
			return err
		}
	} else if cidr {
		switch {
		case a.address[0] >= 240:
			prefix = 32
		case a.address[0] >= 224:
			prefix = 8
		case a.address[0] >= 192:
			prefix = 24
		case a.address[0] >= 128:
			prefix = 16
		default:
			prefix = 8
		}
		prefix = max(prefix, count*8)
		if prefix == 8 && a.address[0] == 224 {
			prefix = 4
		}
	} else if count != 4 {
		return invalidInput()
	}
	if !cidr && prefix/8 > count {
		return invalidInput()
	}
	a.prefix = byte(prefix)
	return nil
}

// This follows inet_cidr_pton_ipv6, including its abbreviated embedded IPv4
// mantissa and prefix rules, rather than netip's stricter address grammar.
func parseNetworkIPv6(text string, a *networkAddress) error {
	if len(text) > 64 {
		return invalidInput()
	}
	pos, end, colon, token := 0, 0, -1, 0
	if len(text) > 0 && text[0] == ':' {
		pos++
		if pos == len(text) || text[pos] != ':' {
			return invalidInput()
		}
	}
	token = pos
	value, digits := 0, 0
	prefix := 128
	for pos < len(text) {
		ch := text[pos]
		pos++
		if d := digit(ch); d >= 0 {
			value = value<<4 | d
			digits++
			if digits > 4 {
				return invalidInput()
			}
			continue
		}
		if ch == ':' {
			token = pos
			if digits == 0 {
				if colon >= 0 {
					return invalidInput()
				}
				colon = end
				continue
			}
			if pos == len(text) || end+2 > 16 {
				return invalidInput()
			}
			a.address[end], a.address[end+1] = byte(value>>8), byte(value)
			end += 2
			value, digits = 0, 0
			continue
		}
		if ch == '.' && end+4 <= 16 {
			v4, bits, ok := parseNetworkEmbeddedIPv4(text[token:])
			if !ok {
				return invalidInput()
			}
			copy(a.address[end:end+4], v4[:])
			end += 4
			digits = 0
			prefix = bits
			break
		}
		if ch == '/' {
			var ok bool
			prefix, ok = parseNetworkIPv6Prefix(text[pos:])
			if !ok {
				return invalidInput()
			}
			break
		}
		return invalidInput()
	}
	if digits != 0 {
		if end+2 > 16 {
			return invalidInput()
		}
		a.address[end], a.address[end+1] = byte(value>>8), byte(value)
		end += 2
	}
	if colon >= 0 {
		if end == 16 {
			return invalidInput()
		}
		for i := end - colon; i > 0; i-- {
			a.address[16-(end-colon)+i-1] = a.address[colon+i-1]
			a.address[colon+i-1] = 0
		}
		end = 16
	}
	if end != 16 {
		return invalidInput()
	}
	a.prefix = byte(prefix)
	return nil
}

func parseNetworkIPv6Prefix(text string) (int, bool) {
	leadingZero := len(text) > 1 && text[0] == '0'
	if len(text) == 0 || len(text) > 3 || leadingZero {
		return 0, false
	}
	value := 0
	for i := range len(text) {
		if text[i] < '0' || text[i] > '9' {
			return 0, false
		}
		value = value*10 + int(text[i]-'0')
	}
	return value, value <= 128
}

func parseNetworkEmbeddedIPv4(text string) ([4]byte, int, bool) {
	var out [4]byte
	value, digits, count := 0, 0, 0
	for i := range len(text) {
		ch := text[i]
		if ch >= '0' && ch <= '9' {
			if digits > 0 && value == 0 {
				return out, 0, false
			}
			digits++
			value = value*10 + int(ch-'0')
			if value > 255 {
				return out, 0, false
			}
			continue
		}
		if ch != '.' && ch != '/' || count > 3 {
			return out, 0, false
		}
		out[count] = byte(value)
		count++
		if ch == '/' {
			prefix, ok := parseNetworkIPv6Prefix(text[i+1:])
			return out, prefix, ok
		}
		value, digits = 0, 0
	}
	if digits == 0 || count > 3 {
		return out, 0, false
	}
	out[count] = byte(value)
	return out, 128, true
}

func parseNetworkIPv4Decimal(ctx context.Context, text string, cidr bool, address *networkAddress) (int, error) {
	count, position := 0, 0
	for position < len(text) {
		if count == 4 || text[position] < '0' || text[position] > '9' {
			return 0, invalidInput()
		}
		value, next, err := parseNetworkDecimalOctet(ctx, text, position)
		if err != nil {
			return 0, err
		}
		address.address[count] = value
		count++
		position = next
		if position == len(text) {
			break
		}
		if text[position] != '.' {
			return 0, invalidInput()
		}
		position++
		// inet accepts a trailing dot; cidr does not.
		if cidr && position == len(text) {
			return 0, invalidInput()
		}
	}
	return count, nil
}

func parseNetworkDecimalOctet(ctx context.Context, text string, position int) (byte, int, error) {
	value := 0
	for position < len(text) && text[position] >= '0' && text[position] <= '9' {
		if position%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, position, err
			}
		}
		value = value*10 + int(text[position]-'0')
		if value > 255 {
			return 0, position, invalidInput()
		}
		position++
	}
	return byte(value), position, nil
}

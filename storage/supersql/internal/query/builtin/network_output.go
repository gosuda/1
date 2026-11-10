package builtin

import "strconv"

func appendNetworkIPv4(out []byte, address []byte) []byte {
	for i, octet := range address {
		if i != 0 {
			out = append(out, '.')
		}
		out = strconv.AppendUint(out, uint64(octet), 10)
	}
	return out
}

func appendNetworkHost(out []byte, a networkAddress) []byte {
	if a.family == 2 {
		return appendNetworkIPv4(out, a.address[:4])
	}
	var words [8]uint16
	bestStart, bestLength, start, length := -1, 0, -1, 0
	for i := range words {
		words[i] = uint16(a.address[i*2])<<8 | uint16(a.address[i*2+1])
		if words[i] == 0 {
			if start < 0 {
				start, length = i, 1
			} else {
				length++
			}
		} else if start >= 0 {
			if length > bestLength {
				bestStart, bestLength = start, length
			}
			start = -1
		}
	}
	if start >= 0 && length > bestLength {
		bestStart, bestLength = start, length
	}
	if bestLength < 2 {
		bestStart = -1
	}
	compatibleIPv4 := bestLength == 6 || (bestLength == 7 && words[7] != 1)
	mappedIPv4 := bestLength == 5 && words[5] == 0xffff
	embeddedIPv4 := compatibleIPv4 || mappedIPv4
	for i := range words {
		if bestStart >= 0 && i >= bestStart && i < bestStart+bestLength {
			if i == bestStart {
				out = append(out, ':')
			}
			continue
		}
		if i != 0 {
			out = append(out, ':')
		}
		if i == 6 && bestStart == 0 && embeddedIPv4 {
			out = appendNetworkIPv4(out, a.address[12:])
			break
		}
		out = strconv.AppendUint(out, uint64(words[i]), 16)
	}
	if bestStart >= 0 && bestStart+bestLength == 8 {
		out = append(out, ':')
	}
	return out
}

func appendNetworkPrefix(out []byte, prefix byte) []byte {
	out = append(out, '/')
	return strconv.AppendUint(out, uint64(prefix), 10)
}

func appendNetworkAbbrev(out []byte, a networkAddress) []byte {
	if a.family == 2 {
		count := (int(a.prefix) + 7) / 8
		if count == 0 {
			out = append(out, '0')
		} else {
			out = appendNetworkIPv4(out, a.address[:count])
		}
		return appendNetworkPrefix(out, a.prefix)
	}
	if a.prefix == 0 {
		return append(out, ':', ':', '/', '0')
	}
	words := max(2, (int(a.prefix)+15)/16)
	zeroStart, zeroLength, tempStart, tempLength := 0, 0, 0, 0
	for i := range words {
		if a.address[i*2]|a.address[i*2+1] == 0 {
			if tempLength == 0 {
				tempStart = i
			}
			tempLength++
		} else if tempLength != 0 && zeroLength < tempLength {
			zeroStart, zeroLength = tempStart, tempLength
			tempLength = 0
		}
	}
	if tempLength != 0 && zeroLength < tempLength {
		zeroStart, zeroLength = tempStart, tempLength
	}
	ipv4 := zeroLength != words && zeroStart == 0 && (zeroLength == 6 || (zeroLength == 5 && a.address[10] == 255 && a.address[11] == 255) || (zeroLength == 7 && a.address[14] != 0 && a.address[15] != 1))
	initial := len(out)
	for p := range words {
		if zeroLength != 0 && p >= zeroStart && p < zeroStart+zeroLength {
			if p == zeroStart {
				out = append(out, ':')
			}
			if p == words-1 {
				out = append(out, ':')
			}
			continue
		}
		if ipv4 && p > 5 {
			separator := byte('.')
			if p == 6 {
				separator = ':'
			}
			out = append(out, separator)
			out = strconv.AppendUint(out, uint64(a.address[p*2]), 10)
			if p != 7 || a.prefix > 120 {
				out = append(out, '.')
				out = strconv.AppendUint(out, uint64(a.address[p*2+1]), 10)
			}
		} else {
			if len(out) != initial {
				out = append(out, ':')
			}
			out = strconv.AppendUint(out, uint64(a.address[p*2])<<8|uint64(a.address[p*2+1]), 16)
		}
	}
	return appendNetworkPrefix(out, a.prefix)
}

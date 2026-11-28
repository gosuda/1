package main

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/gosuda/supersql-alpha/internal/query/builtin/internal/tzrule"
)

type zone struct {
	name                                   string
	times, indices, types, chars, charsEnd uint32
	count                                  uint32
	typeCount                              uint16
	defaultType, timeSize                  uint8
	future                                 tzrule.Rule
	hasFuture                              bool
}

func decodeBlock(raw []byte, start, width int) (zone, int, error) {
	bad := func() (zone, int, error) { return zone{}, 0, fmt.Errorf("%w: malformed TZif block", errData) }
	if start < 0 || len(raw)-start < 44 || string(raw[start:start+4]) != "TZif" {
		return bad()
	}
	h := raw[start : start+44]
	var c [6]int
	for i := range c {
		c[i] = int(binary.BigEndian.Uint32(h[20+i*4:]))
	}
	ut, std, leaps, times, types, chars := c[0], c[1], c[2], c[3], c[4], c[5]
	if leaps != 0 || times >= 2000 {
		return bad()
	}
	validTypes := types >= 1 && types < 256
	validChars := chars >= 1 && chars < 50
	if !validTypes || !validChars {
		return bad()
	}
	validUTC := ut == 0 || ut == types
	validStandard := std == 0 || std == types
	if !validUTC || !validStandard {
		return bad()
	}
	end := start + 44 + times*(width+1) + types*6 + chars + std + ut
	if end > len(raw) {
		return bad()
	}
	z := zone{times: uint32(start + 44), count: uint32(times), typeCount: uint16(types), timeSize: uint8(width)}
	z.indices = z.times + uint32(times*width)
	z.types = z.indices + uint32(times)
	z.chars = z.types + uint32(types*6)
	z.charsEnd = z.chars + uint32(chars)
	var previous int64
	for i := range times {
		p := int(z.times) + i*width
		var at int64
		if width == 4 {
			at = int64(int32(binary.BigEndian.Uint32(raw[p:])))
		} else {
			at = int64(binary.BigEndian.Uint64(raw[p:]))
		}
		if i > 0 && at <= previous || int(raw[int(z.indices)+i]) >= types {
			return bad()
		}
		previous = at
	}
	for i := range types {
		p := int(z.types) + i*6
		a := int(raw[p+5])
		if raw[p+4] > 1 || a >= chars || bytes.IndexByte(raw[int(z.chars)+a:int(z.charsEnd)], 0) < 0 {
			return bad()
		}
	}
	for _, b := range raw[int(z.charsEnd):end] {
		if b > 1 {
			return bad()
		}
	}
	index := 0
	for _, b := range raw[z.indices:z.types] {
		if b == 0 {
			index = -1
			break
		}
	}
	if index < 0 && times > 0 && raw[int(z.types)+int(raw[z.indices])*6+4] != 0 {
		index = int(raw[z.indices]) - 1
		for index >= 0 && raw[int(z.types)+index*6+4] != 0 {
			index--
		}
	}
	if index < 0 {
		index = 0
		for index < types && raw[int(z.types)+index*6+4] != 0 {
			index++
		}
		if index == types {
			index = 0
		}
	}
	z.defaultType = uint8(index)
	return z, end, nil
}

func parseTZif(raw []byte) (zone, error) {
	if len(raw) < 44 {
		return zone{}, fmt.Errorf("%w: truncated TZif", errData)
	}
	version := raw[4]
	if version != 0 && version != '2' && version != '3' && version != '4' {
		return zone{}, fmt.Errorf("%w: TZif version", errData)
	}
	z, end, err := decodeBlock(raw, 0, 4)
	if err != nil {
		return zone{}, err
	}
	if version == 0 {
		if end != len(raw) {
			return zone{}, fmt.Errorf("%w: trailing TZif v1 bytes", errData)
		}
		return z, nil
	}
	if len(raw)-end < 44 || raw[end+4] != version {
		return zone{}, fmt.Errorf("%w: mismatched TZif headers", errData)
	}
	z, end, err = decodeBlock(raw, end, 8)
	if err != nil {
		return zone{}, err
	}
	tail := raw[end:]
	if len(tail) < 2 || tail[0] != '\n' || tail[len(tail)-1] != '\n' {
		return zone{}, fmt.Errorf("%w: TZif footer framing", errData)
	}
	if len(tail) > 2 {
		z.future, err = tzrule.Parse(string(tail[1 : len(tail)-1]))
		if err != nil {
			return zone{}, fmt.Errorf("TZif footer: %w", err)
		}
		z.hasFuture = true
	}
	return z, nil
}

package main

import (
	"cmp"
	"fmt"
	"math"
	"slices"
)

func (data *unicodeData) validate() error {
	indices := make(map[uint32]int, len(data.norm))
	poolEnd := 0
	for i, entry := range data.norm {
		if i > 0 && entry.code <= data.norm[i-1].code {
			return fmt.Errorf("%w: normalization codepoints not strictly ordered at U+%X", errInvalidData, entry.code)
		}
		if entry.code >= 0xac00 && entry.code < 0xac00+11172 {
			return fmt.Errorf("%w: Hangul syllable unexpectedly tabled at U+%X", errInvalidData, entry.code)
		}
		if categoryAt(data.categories, entry.code) == "PG_U_UNASSIGNED" {
			return fmt.Errorf("%w: unassigned normalization codepoint U+%X", errInvalidData, entry.code)
		}
		size := int(entry.flags & decompSize)
		unexpectedMetadata := entry.flags != 0 || entry.index != 0 || entry.class == 0
		if size == 0 && unexpectedMetadata {
			return fmt.Errorf("%w: empty decomposition metadata at U+%X", errInvalidData, entry.code)
		}
		if entry.flags&decompInline != 0 && size != 1 {
			return fmt.Errorf("%w: inline decomposition length at U+%X", errInvalidData, entry.code)
		}
		if entry.flags&decompNoCompose != 0 && size != 2 {
			return fmt.Errorf("%w: composition exclusion length at U+%X", errInvalidData, entry.code)
		}
		if size != 0 && entry.flags&decompInline == 0 {
			if int(entry.index) != poolEnd || size > len(data.decompositions)-poolEnd {
				return fmt.Errorf("%w: noncontiguous or out-of-bounds decomposition at U+%X", errInvalidData, entry.code)
			}
			poolEnd += size
		}
		indices[entry.code] = i
	}
	if poolEnd != len(data.decompositions) {
		return fmt.Errorf("%w: unreferenced decomposition pool suffix", errInvalidData)
	}
	for _, entry := range data.norm {
		for i := range int(entry.flags & decompSize) {
			code := data.decompositionAt(entry, i)
			if categoryAt(data.categories, code) == "PG_U_UNASSIGNED" {
				return fmt.Errorf("%w: U+%X decomposes to unassigned U+%X", errInvalidData, entry.code, code)
			}
		}
	}
	for i, entries := range data.quickChecks {
		for _, entry := range entries {
			if categoryAt(data.categories, entry.code) == "PG_U_UNASSIGNED" {
				return fmt.Errorf("%w: unassigned quick-check codepoint U+%X", errInvalidData, entry.code)
			}
			if entry.value != 1 {
				continue
			}
			index, ok := indices[entry.code]
			invalid := !ok
			if ok {
				compatibilityMismatch := i == 0 && data.norm[index].flags&decompCompat != 0
				invalid = data.norm[index].flags&decompSize == 0 || compatibilityMismatch
			}
			if invalid {
				return fmt.Errorf("%w: quick-check No has no matching decomposition at U+%X", errInvalidData, entry.code)
			}
		}
	}
	maximum, err := data.maximumDecomposition(indices)
	if err != nil {
		return err
	}
	data.maxDecomposition = maximum
	data.compositions = nil
	for _, entry := range data.norm {
		if entry.flags&decompSize != 2 || entry.flags&(decompCompat|decompNoCompose) != 0 {
			continue
		}
		left, right := data.decompositionAt(entry, 0), data.decompositionAt(entry, 1)
		if index, ok := indices[left]; ok && data.norm[index].class != 0 {
			return fmt.Errorf("%w: nonstarter composition without exclusion at U+%X", errInvalidData, entry.code)
		}
		data.compositions = append(data.compositions, composition{key: uint64(left)<<21 | uint64(right), code: entry.code})
	}
	slices.SortFunc(data.compositions, func(a, b composition) int { return cmp.Compare(a.key, b.key) })
	for i := 1; i < len(data.compositions); i++ {
		if data.compositions[i-1].key == data.compositions[i].key {
			return fmt.Errorf("%w: duplicate canonical composition pair 0x%X", errInvalidData, data.compositions[i].key)
		}
	}
	return nil
}

func (data *unicodeData) decompositionAt(entry normEntry, index int) uint32 {
	if entry.flags&decompInline != 0 {
		return uint32(entry.index)
	}
	return data.decompositions[int(entry.index)+index]
}

func (data *unicodeData) maximumDecomposition(indices map[uint32]int) (int, error) {
	states := make([]uint8, len(data.norm))
	lengths := make([]int, len(data.norm))
	var visit func(uint32) (int, error)
	visit = func(code uint32) (int, error) {
		if code >= 0xac00 && code < 0xac00+11172 {
			if (code-0xac00)%28 == 0 {
				return 2, nil
			}
			return 3, nil
		}
		index, ok := indices[code]
		if !ok || data.norm[index].flags&decompSize == 0 {
			return 1, nil
		}
		if states[index] == 1 {
			return 0, fmt.Errorf("%w: decomposition cycle at U+%X", errInvalidData, code)
		}
		if states[index] == 2 {
			return lengths[index], nil
		}
		states[index] = 1
		entry := data.norm[index]
		total := 0
		for i := range int(entry.flags & decompSize) {
			length, err := visit(data.decompositionAt(entry, i))
			if err != nil {
				return 0, err
			}
			if total > math.MaxInt-length {
				return 0, fmt.Errorf("%w: recursive decomposition length overflow at U+%X", errInvalidData, code)
			}
			total += length
		}
		states[index] = 2
		lengths[index] = total
		return total, nil
	}
	maximum := 3 // Algorithmic LVT decomposition is absent from the source table.
	for _, entry := range data.norm {
		length, err := visit(entry.code)
		if err != nil {
			return 0, err
		}
		maximum = max(maximum, length)
	}
	return maximum, nil
}

func combineQuickChecks(checks [2][]qcEntry) []qcRange {
	var ranges []qcRange
	left, right := 0, 0
	for left < len(checks[0]) || right < len(checks[1]) {
		code := uint32(maxCodepoint + 1)
		if left < len(checks[0]) {
			code = checks[0][left].code
		}
		if right < len(checks[1]) {
			code = min(code, checks[1][right].code)
		}
		var flags uint8
		if left < len(checks[0]) && checks[0][left].code == code {
			flags = checks[0][left].value
			left++
		}
		if right < len(checks[1]) && checks[1][right].code == code {
			flags |= checks[1][right].value << 2
			right++
		}
		if len(ranges) > 0 && ranges[len(ranges)-1].last+1 == code && ranges[len(ranges)-1].flags == flags {
			ranges[len(ranges)-1].last = code
		} else {
			ranges = append(ranges, qcRange{codeRange: codeRange{first: code, last: code}, flags: flags})
		}
	}
	return ranges
}

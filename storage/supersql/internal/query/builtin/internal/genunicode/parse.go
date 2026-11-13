package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	decompSize      = 0x1f
	decompCompat    = 0x20
	decompInline    = 0x40
	decompNoCompose = 0x80
	maxCodepoint    = 0x10ffff
)

type normEntry struct {
	code  uint32
	class uint8
	flags uint8
	index uint16
}

type qcEntry struct {
	code  uint32
	value uint8
}

type codeRange struct {
	first, last uint32
}

type categoryRange struct {
	codeRange
	category string
}

type composition struct {
	key  uint64
	code uint32
}

type qcRange struct {
	codeRange
	flags uint8
}

type unicodeData struct {
	version          string
	norm             []normEntry
	decompositions   []uint32
	quickChecks      [2][]qcEntry
	categories       []categoryRange
	compositions     []composition
	maxDecomposition int
}

var cComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

func parseData(sources map[string]string) (*unicodeData, error) {
	clean := make(map[string]string, len(sources))
	for name, contents := range sources {
		clean[name] = cComment.ReplaceAllString(contents, "")
	}
	version := regexp.MustCompile(`(?m)^#define\s+PG_UNICODE_VERSION\s+"([^"]+)"\s*$`).FindStringSubmatch(clean["unicode_version.h"])
	if len(version) != 2 || version[1] != "16.0" {
		return nil, fmt.Errorf("%w: expected Unicode 16.0 version definition", errInvalidData)
	}
	data := &unicodeData{version: version[1]}
	var err error
	data.norm, data.decompositions, err = parseNormalization(clean["unicode_norm_table.h"])
	if err != nil {
		return nil, err
	}
	for i, form := range []string{"NFC", "NFKC"} {
		data.quickChecks[i], err = parseQuickCheck(clean["unicode_normprops_table.h"], form)
		if err != nil {
			return nil, err
		}
	}
	data.categories, err = parseCategories(clean["unicode_category_table.h"])
	if err != nil {
		return nil, err
	}
	if err := data.validate(); err != nil {
		return nil, err
	}
	return data, nil
}

// The source grammar is deliberately limited to the generated C initializers.
// Every initializer token must be consumed, rather than silently skipping rows.
func arrayBody(source, name string) (string, int, error) {
	pattern := regexp.MustCompile(`(?s)\b` + regexp.QuoteMeta(name) + `\s*\[\s*([0-9]*)\s*\]\s*=\s*\{(.*?)\};`)
	matches := pattern.FindAllStringSubmatch(source, -1)
	if len(matches) != 1 {
		return "", 0, fmt.Errorf("%w: expected one %s array", errInvalidData, name)
	}
	count := -1
	if matches[0][1] != "" {
		var err error
		count, err = strconv.Atoi(matches[0][1])
		if err != nil {
			return "", 0, fmt.Errorf("%w: %s count: %v", errInvalidData, name, err)
		}
	}
	return matches[0][2], count, nil
}

func arrayRows(source, name string, width int) ([][]string, error) {
	body, count, err := arrayBody(source, name)
	if err != nil {
		return nil, err
	}
	var rows [][]string
	for body = strings.TrimSpace(body); body != ""; body = strings.TrimSpace(body) {
		if body[0] != '{' {
			return nil, fmt.Errorf("%w: %s row %d opening brace", errInvalidData, name, len(rows))
		}
		end := strings.IndexByte(body, '}')
		if end < 0 {
			return nil, fmt.Errorf("%w: %s row %d closing brace", errInvalidData, name, len(rows))
		}
		fields := strings.Split(body[1:end], ",")
		if len(fields) != width {
			return nil, fmt.Errorf("%w: %s row %d has %d fields, want %d", errInvalidData, name, len(rows), len(fields), width)
		}
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		rows = append(rows, fields)
		body = strings.TrimSpace(body[end+1:])
		if body != "" {
			if body[0] != ',' {
				return nil, fmt.Errorf("%w: %s missing row separator", errInvalidData, name)
			}
			body = body[1:]
		}
	}
	if count >= 0 && count != len(rows) {
		return nil, fmt.Errorf("%w: %s has %d rows, declared %d", errInvalidData, name, len(rows), count)
	}
	return rows, nil
}

func unsigned(text string, bits int) (uint32, error) {
	value, err := strconv.ParseUint(strings.TrimSpace(text), 0, bits)
	if err != nil {
		return 0, fmt.Errorf("%w: unsigned %d-bit value %q", errInvalidData, bits, text)
	}
	return uint32(value), nil
}

func codepoint(text string) (uint32, error) {
	code, err := unsigned(text, 32)
	if err != nil {
		return 0, err
	}
	if code > maxCodepoint {
		return 0, fmt.Errorf("%w: codepoint U+%X exceeds Unicode range", errInvalidData, code)
	}
	return code, nil
}

func parseNormalization(source string) ([]normEntry, []uint32, error) {
	flags := map[string]uint8{
		"DECOMP_COMPAT":     decompCompat,
		"DECOMP_INLINE":     decompInline,
		"DECOMP_NO_COMPOSE": decompNoCompose,
	}
	for _, name := range []string{"DECOMP_COMPAT", "DECOMP_INLINE", "DECOMP_NO_COMPOSE"} {
		match := regexp.MustCompile(`(?m)^#define\s+` + name + `\s+(0x[0-9a-fA-F]+)\s*$`).FindStringSubmatch(source)
		if len(match) != 2 {
			return nil, nil, fmt.Errorf("%w: missing %s definition", errInvalidData, name)
		}
		value, err := unsigned(match[1], 8)
		if err != nil || uint8(value) != flags[name] {
			return nil, nil, fmt.Errorf("%w: unexpected %s value", errInvalidData, name)
		}
	}
	rows, err := arrayRows(source, "UnicodeDecompMain", 4)
	if err != nil {
		return nil, nil, err
	}
	entries := make([]normEntry, 0, len(rows))
	for i, row := range rows {
		code, err := codepoint(row[0])
		if err != nil {
			return nil, nil, err
		}
		class, err := unsigned(row[1], 8)
		if err != nil {
			return nil, nil, err
		}
		parts := strings.Split(row[2], "|")
		size, err := unsigned(parts[0], 5)
		if err != nil {
			return nil, nil, err
		}
		value := uint8(size)
		for _, part := range parts[1:] {
			flag, ok := flags[strings.TrimSpace(part)]
			if !ok || value&flag != 0 {
				return nil, nil, fmt.Errorf("%w: decomposition row %d flag %q", errInvalidData, i, part)
			}
			value |= flag
		}
		index, err := unsigned(row[3], 16)
		if err != nil {
			return nil, nil, err
		}
		entries = append(entries, normEntry{code: code, class: uint8(class), flags: value, index: uint16(index)})
	}
	body, count, err := arrayBody(source, "UnicodeDecomp_codepoints")
	if err != nil {
		return nil, nil, err
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimSpace(body), ","), ",")
	if len(parts) != count {
		return nil, nil, fmt.Errorf("%w: decomposition pool has %d codepoints, declared %d", errInvalidData, len(parts), count)
	}
	codes := make([]uint32, 0, count)
	for _, part := range parts {
		code, err := codepoint(part)
		if err != nil {
			return nil, nil, err
		}
		codes = append(codes, code)
	}
	return entries, codes, nil
}

func parseQuickCheck(source, form string) ([]qcEntry, error) {
	name := "UnicodeNormProps_" + form + "_QC"
	rows, err := arrayRows(source, name, 2)
	if err != nil {
		return nil, err
	}
	info := regexp.MustCompile(`(?s)\bUnicodeNormInfo_` + form + `_QC\s*=\s*\{\s*` + name + `\s*,\s*` + form + `_QC_hash_func\s*,\s*([0-9]+)\s*\};`).FindStringSubmatch(source)
	if len(info) != 2 {
		return nil, fmt.Errorf("%w: missing %s count metadata", errInvalidData, name)
	}
	count, err := unsigned(info[1], 32)
	if err != nil || uint32(len(rows)) != count {
		return nil, fmt.Errorf("%w: %s count mismatch", errInvalidData, name)
	}
	entries := make([]qcEntry, 0, len(rows))
	for _, row := range rows {
		code, err := codepoint(row[0])
		if err != nil {
			return nil, err
		}
		if len(entries) != 0 && code <= entries[len(entries)-1].code {
			return nil, fmt.Errorf("%w: %s codepoints not strictly ordered at U+%X", errInvalidData, name, code)
		}
		var value uint8
		switch row[1] {
		case "UNICODE_NORM_QC_NO":
			value = 1
		case "UNICODE_NORM_QC_MAYBE":
			value = 2
		default:
			return nil, fmt.Errorf("%w: %s quick-check value %q", errInvalidData, name, row[1])
		}
		entries = append(entries, qcEntry{code: code, value: value})
	}
	return entries, nil
}

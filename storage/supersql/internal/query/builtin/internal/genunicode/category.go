package main

import (
	"fmt"
	"regexp"
	"strings"
)

// Fixed General_Category enumeration from PostgreSQL's unicode_category.h.
var categoryNames = strings.Fields(`
PG_U_UNASSIGNED PG_U_UPPERCASE_LETTER PG_U_LOWERCASE_LETTER
PG_U_TITLECASE_LETTER PG_U_MODIFIER_LETTER PG_U_OTHER_LETTER
PG_U_NONSPACING_MARK PG_U_ENCLOSING_MARK PG_U_SPACING_MARK
PG_U_DECIMAL_NUMBER PG_U_LETTER_NUMBER PG_U_OTHER_NUMBER
PG_U_SPACE_SEPARATOR PG_U_LINE_SEPARATOR PG_U_PARAGRAPH_SEPARATOR
PG_U_CONTROL PG_U_FORMAT PG_U_PRIVATE_USE PG_U_SURROGATE
PG_U_DASH_PUNCTUATION PG_U_OPEN_PUNCTUATION PG_U_CLOSE_PUNCTUATION
PG_U_CONNECTOR_PUNCTUATION PG_U_OTHER_PUNCTUATION PG_U_MATH_SYMBOL
PG_U_CURRENCY_SYMBOL PG_U_MODIFIER_SYMBOL PG_U_OTHER_SYMBOL
PG_U_INITIAL_PUNCTUATION PG_U_FINAL_PUNCTUATION`)

func parseCategories(source string) ([]categoryRange, error) {
	known := make(map[string]bool, len(categoryNames))
	for _, name := range categoryNames {
		known[name] = true
	}
	rows, err := arrayRows(source, "unicode_categories", 3)
	if err != nil {
		return nil, err
	}
	categories := make([]categoryRange, 0, len(rows))
	for _, row := range rows {
		entry, err := parseRange(row)
		if err != nil {
			return nil, err
		}
		if !known[row[2]] || row[2] == "PG_U_UNASSIGNED" {
			return nil, fmt.Errorf("%w: unexpected explicit category %s", errInvalidData, row[2])
		}
		if len(categories) > 0 && entry.first <= categories[len(categories)-1].last {
			return nil, fmt.Errorf("%w: category ranges not strictly ordered at U+%X", errInvalidData, entry.first)
		}
		categories = append(categories, categoryRange{codeRange: entry, category: row[2]})
	}
	if err := validateASCIIProperties(source, categories); err != nil {
		return nil, err
	}
	return categories, nil
}

func parseRange(row []string) (codeRange, error) {
	first, err := codepoint(row[0])
	if err != nil {
		return codeRange{}, err
	}
	last, err := codepoint(row[1])
	if err != nil {
		return codeRange{}, err
	}
	if first > last {
		return codeRange{}, fmt.Errorf("%w: reversed range U+%X..U+%X", errInvalidData, first, last)
	}
	return codeRange{first: first, last: last}, nil
}

func validateASCIIProperties(source string, categories []categoryRange) error {
	properties := [...]struct {
		name  string
		array string
	}{
		{"ALPHABETIC", "unicode_alphabetic"},
		{"LOWERCASE", "unicode_lowercase"},
		{"UPPERCASE", "unicode_uppercase"},
		{"CASED", ""},
		{"CASE_IGNORABLE", "unicode_case_ignorable"},
		{"WHITE_SPACE", "unicode_white_space"},
		{"JOIN_CONTROL", "unicode_join_control"},
		{"HEX_DIGIT", "unicode_hex_digit"},
	}
	masks := make(map[string]uint8, len(properties))
	var expected [128]uint8
	for bit, property := range properties {
		name := "PG_U_PROP_" + property.name
		definition := regexp.MustCompile(`(?m)^#define\s+` + name + `\s+\(1\s*<<\s*([0-9]+)\)\s*$`).FindStringSubmatch(source)
		if len(definition) != 2 || definition[1] != fmt.Sprint(bit) {
			return fmt.Errorf("%w: %s bit definition", errInvalidData, name)
		}
		masks[name] = 1 << bit
		if property.array == "" {
			continue
		}
		rows, err := arrayRows(source, property.array, 2)
		if err != nil {
			return err
		}
		var previous codeRange
		for i, row := range rows {
			entry, err := parseRange(row)
			if err != nil {
				return err
			}
			if i > 0 && entry.first <= previous.last {
				return fmt.Errorf("%w: %s ranges overlap at U+%X", errInvalidData, property.array, entry.first)
			}
			previous = entry
			for code := entry.first; code <= entry.last && code < 128; code++ {
				expected[code] |= 1 << bit
			}
		}
	}
	rows, err := arrayRows(source, "unicode_opt_ascii", 2)
	if err != nil {
		return err
	}
	if len(rows) != len(expected) {
		return fmt.Errorf("%w: ASCII property table requires 128 entries", errInvalidData)
	}
	for code, row := range rows {
		category, ok := strings.CutPrefix(row[0], ".category = ")
		if !ok || category != categoryAt(categories, uint32(code)) {
			return fmt.Errorf("%w: ASCII category disagreement at U+%X", errInvalidData, code)
		}
		expression, ok := strings.CutPrefix(row[1], ".properties = ")
		if !ok {
			return fmt.Errorf("%w: ASCII properties at U+%X", errInvalidData, code)
		}
		var actual uint8
		if expression != "0" {
			for _, token := range strings.Split(expression, "|") {
				mask, ok := masks[strings.TrimSpace(token)]
				if !ok || actual&mask != 0 {
					return fmt.Errorf("%w: ASCII property %q at U+%X", errInvalidData, token, code)
				}
				actual |= mask
			}
		}
		if expected[code]&(masks["PG_U_PROP_LOWERCASE"]|masks["PG_U_PROP_UPPERCASE"]) != 0 || category == "PG_U_TITLECASE_LETTER" {
			expected[code] |= masks["PG_U_PROP_CASED"]
		}
		if actual != expected[code] {
			return fmt.Errorf("%w: ASCII properties disagree with ranges at U+%X", errInvalidData, code)
		}
	}
	return nil
}

func categoryAt(categories []categoryRange, code uint32) string {
	low, high := 0, len(categories)
	for low < high {
		mid := low + (high-low)/2
		if categories[mid].last < code {
			low = mid + 1
		} else {
			high = mid
		}
	}
	if low < len(categories) && code >= categories[low].first {
		return categories[low].category
	}
	return "PG_U_UNASSIGNED"
}

func assignedRanges(categories []categoryRange) []codeRange {
	ranges := make([]codeRange, 0, len(categories))
	for _, entry := range categories {
		if len(ranges) > 0 && ranges[len(ranges)-1].last+1 == entry.first {
			ranges[len(ranges)-1].last = entry.last
		} else {
			ranges = append(ranges, entry.codeRange)
		}
	}
	return ranges
}

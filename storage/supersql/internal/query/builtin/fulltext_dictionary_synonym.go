package builtin

import (
	"context"
	"sort"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// nativeSynonymDictionary is PostgreSQL's synonym dictionary: an exact-match
// replacement table read from a pinned tsearch_data ".syn" file and sorted by
// input for binary search, exactly as dsynonym_init compiles it.
type nativeSynonymDictionary struct {
	inputs  []string
	outputs []string
	exact   bool
}

// synonymRow pairs an input with its replacement while the table is compiled.
type synonymRow struct {
	input  string
	output string
}

// newNativeSynonymDictionary compiles the pinned synonym file. The grammar is
// PostgreSQL's findwrd line reader: a line with fewer than two words is
// skipped silently, and only the first two words on a line are used. A
// case-insensitive dictionary folds both sides to lower case, matching
// dsynonym_init's default.
func newNativeSynonymDictionary(ctx context.Context, name string, caseSensitive bool) (*nativeSynonymDictionary, error) {
	data, ok := nativeTextSearchData[name+".syn"]
	if !ok {
		return nil, sqlError("F0000", "could not open synonym file \""+name+".syn\"")
	}
	rows := make([]synonymRow, 0, 16)
	for _, line := range splitNativeTextSearchLines(data) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		input, end, ok := takeSynonymWord(line)
		if !ok || end >= len(line) {
			continue
		}
		output, _, ok := takeSynonymWord(line[end+1:])
		if !ok {
			continue
		}
		// A single trailing '*' marks the synonym as a prefix (TSL_PREFIX) and
		// is not part of the stored word.
		output = strings.TrimSuffix(output, "*")
		row := synonymRow{input: input, output: output}
		if !caseSensitive {
			row.input = strings.ToLower(row.input)
			row.output = strings.ToLower(row.output)
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].input < rows[j].input })
	dictionary := &nativeSynonymDictionary{
		inputs:  make([]string, len(rows)),
		outputs: make([]string, len(rows)),
		exact:   caseSensitive,
	}
	for i, row := range rows {
		dictionary.inputs[i] = row.input
		dictionary.outputs[i] = row.output
	}
	return dictionary, nil
}

// splitNativeTextSearchLines splits a pinned tsearch_data file into lines
// without their terminators, matching PostgreSQL's line-at-a-time reader.
func splitNativeTextSearchLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	lines := make([]string, 0, 8)
	for len(data) > 0 {
		index := -1
		for i := range data {
			if data[i] == '\n' {
				index = i
				break
			}
		}
		if index < 0 {
			lines = append(lines, strings.TrimSuffix(string(data), "\r"))
			break
		}
		lines = append(lines, strings.TrimSuffix(string(data[:index]), "\r"))
		data = data[index+1:]
	}
	return lines
}

// takeSynonymWord consumes one whitespace-delimited word from line and reports
// the byte offset just past it, so the caller can reproduce PostgreSQL's
// "skip exactly one separator" step. PostgreSQL's findwrd returns no word for
// a blank line, and a leading '*' is an ordinary character on the input side.
func takeSynonymWord(line string) (word string, end int, ok bool) {
	start := 0
	for start < len(line) && isTSSpace(line[start]) {
		start++
	}
	if start == len(line) {
		return "", start, false
	}
	end = start
	for end < len(line) && !isTSSpace(line[end]) {
		end++
	}
	return line[start:end], end, true
}

// Lexize mirrors dsynonym_lexize: one lexeme on an exact match, and a nil
// result when the word is not in the table.
func (d *nativeSynonymDictionary) Lexize(ctx context.Context, token string) ([]string, error) {
	variants, err := d.LexizeVariants(ctx, token)
	if err != nil || variants == nil {
		return nil, err
	}
	return flattenTextSearchVariants(variants), nil
}

// LexizeVariants reports the single variant a synonym hit produces.
func (d *nativeSynonymDictionary) LexizeVariants(ctx context.Context, token string) ([][]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if token == "" || len(d.inputs) == 0 {
		return nil, nil
	}
	key := token
	if !d.exact {
		key = strings.ToLower(token)
	}
	if index := sort.SearchStrings(d.inputs, key); index < len(d.inputs) && d.inputs[index] == key {
		return [][]string{{d.outputs[index]}}, nil
	}
	return nil, nil
}

var _ model.TextSearchDictionaryKernel = (*nativeSynonymDictionary)(nil)
var _ model.TextSearchVariantKernel = (*nativeSynonymDictionary)(nil)

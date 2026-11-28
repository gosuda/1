package builtin

import (
	"bytes"
	"context"
	"encoding/binary"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	tsMaxLexemeBytes = (1 << 11) - 1
	tsMaxDataBytes   = (1 << 20) - 1
	tsMaxPositions   = 256
	tsMaxPosition    = (1 << 14) - 1
)

type tsPosition uint16

type tsLexeme struct {
	text      string
	positions []tsPosition
}

func tsPositionNumber(position tsPosition) int { return int(position & 0x3fff) }
func tsPositionWeight(position tsPosition) int { return int(position >> 14) }
func makeTSPosition(position, weight int) tsPosition {
	return tsPosition(uint16(position) | uint16(weight)<<14)
}

func decodeTSVector(ctx context.Context, data []byte) ([]tsLexeme, error) {
	if len(data) < 4 {
		return nil, sqlError("22P03", "invalid binary representation for tsvector")
	}
	count := int64(int32(binary.BigEndian.Uint32(data)))
	if count < 0 || count > tsMaxDataBytes || count > int64((len(data)-4)/3) {
		return nil, sqlError("22P03", "invalid size of tsvector")
	}
	lexemes := make([]tsLexeme, 0, int(count))
	offset := 4
	dataBytes := 0
	for range int(count) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := bytes.IndexByte(data[offset:], 0)
		if end <= 0 || end > tsMaxLexemeBytes {
			return nil, sqlError("22P03", "invalid tsvector lexeme")
		}
		if !utf8.Valid(data[offset : offset+end]) {
			return nil, sqlError("22021", "invalid UTF-8 in tsvector lexeme")
		}
		text := string(data[offset : offset+end])
		offset += end + 1
		dataBytes += end
		if dataBytes > tsMaxDataBytes || len(data)-offset < 2 {
			return nil, sqlError("22P03", "invalid binary representation for tsvector")
		}
		npos := int(binary.BigEndian.Uint16(data[offset:]))
		offset += 2
		if npos > tsMaxPositions || npos > (len(data)-offset)/2 {
			return nil, sqlError("22P03", "unexpected number of tsvector positions")
		}
		positions := make([]tsPosition, npos)
		last := -1
		for i := range positions {
			positions[i] = tsPosition(binary.BigEndian.Uint16(data[offset:]))
			offset += 2
			position := tsPositionNumber(positions[i])
			if position <= last {
				return nil, sqlError("22P03", "position information is misordered")
			}
			last = position
		}
		lexemes = append(lexemes, tsLexeme{text: text, positions: positions})
	}
	if offset != len(data) {
		return nil, sqlError("22P03", "invalid binary representation for tsvector")
	}
	return lexemes, nil
}

func encodeTSVector(ctx context.Context, lexemes []tsLexeme, budget int64) ([]byte, error) {
	size := 4
	for _, lexeme := range lexemes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(lexeme.text) == 0 || len(lexeme.text) > tsMaxLexemeBytes || !utf8.ValidString(lexeme.text) || len(lexeme.positions) > tsMaxPositions {
			return nil, sqlError("54000", "tsvector value exceeds PostgreSQL limits")
		}
		if size > tsMaxDataBytes-len(lexeme.text) {
			return nil, sqlError("54000", "tsvector value exceeds PostgreSQL limits")
		}
		size += len(lexeme.text) + 3 + len(lexeme.positions)*2
	}
	if err := outputBudget(int64(size), budget); err != nil {
		return nil, err
	}
	out := make([]byte, size)
	binary.BigEndian.PutUint32(out, uint32(len(lexemes)))
	offset := 4
	for _, lexeme := range lexemes {
		copy(out[offset:], lexeme.text)
		offset += len(lexeme.text) + 1
		binary.BigEndian.PutUint16(out[offset:], uint16(len(lexeme.positions)))
		offset += 2
		for _, position := range lexeme.positions {
			binary.BigEndian.PutUint16(out[offset:], uint16(position))
			offset += 2
		}
	}
	return out, nil
}

func normalizeTSVector(lexemes []tsLexeme) []tsLexeme {
	sort.Slice(lexemes, func(i, j int) bool { return bytes.Compare([]byte(lexemes[i].text), []byte(lexemes[j].text)) < 0 })
	out := lexemes[:0]
	for _, lexeme := range lexemes {
		lexeme.positions = normalizeTSPositions(lexeme.positions)
		if len(out) == 0 || out[len(out)-1].text != lexeme.text {
			out = append(out, lexeme)
			continue
		}
		out[len(out)-1].positions = normalizeTSPositions(append(out[len(out)-1].positions, lexeme.positions...))
	}
	return out
}

func normalizeTSPositions(positions []tsPosition) []tsPosition {
	sort.Slice(positions, func(i, j int) bool {
		pi, pj := tsPositionNumber(positions[i]), tsPositionNumber(positions[j])
		if pi != pj {
			return pi < pj
		}
		return tsPositionWeight(positions[i]) > tsPositionWeight(positions[j])
	})
	out := positions[:0]
	for _, position := range positions {
		if len(out) != 0 && tsPositionNumber(out[len(out)-1]) == tsPositionNumber(position) {
			continue
		}
		if len(out) == tsMaxPositions || tsPositionNumber(position) >= tsMaxPosition {
			if len(out) == 0 || tsPositionNumber(out[len(out)-1]) != tsMaxPosition {
				position = makeTSPosition(tsMaxPosition, tsPositionWeight(position))
				out = append(out, position)
			}
			break
		}
		out = append(out, position)
	}
	return out
}

func parseTSVector(ctx context.Context, input string, budget int64) ([]byte, error) {
	lexemes := make([]tsLexeme, 0, 8)
	for offset := 0; ; {
		for offset < len(input) && isTSSpace(input[offset]) {
			offset++
		}
		if offset == len(input) {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		text, next, err := parseTSLexeme(input, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		var positions []tsPosition
		if offset < len(input) && input[offset] == ':' {
			positions, offset, err = parseTSPositions(input, offset+1)
			if err != nil {
				return nil, err
			}
		}
		if offset < len(input) && !isTSSpace(input[offset]) {
			return nil, sqlError("42601", "syntax error in tsvector")
		}
		lexemes = append(lexemes, tsLexeme{text: text, positions: positions})
	}
	return encodeTSVector(ctx, normalizeTSVector(lexemes), budget)
}

func parseTSLexeme(input string, offset int) (string, int, error) {
	var out strings.Builder
	quoted := input[offset] == '\''
	if quoted {
		offset++
	}
	for offset < len(input) {
		c := input[offset]
		if c == '\\' {
			offset++
			if offset == len(input) {
				return "", 0, sqlError("42601", "syntax error in tsvector")
			}
			out.WriteByte(input[offset])
			offset++
			continue
		}
		if quoted && c == '\'' {
			if offset+1 < len(input) && input[offset+1] == '\'' {
				out.WriteByte('\'')
				offset += 2
				continue
			}
			offset++
			if out.Len() == 0 {
				return "", 0, sqlError("42601", "syntax error in tsvector")
			}
			return out.String(), offset, nil
		}
		if !quoted && (isTSSpace(c) || c == ':') {
			break
		}
		out.WriteByte(c)
		offset++
	}
	if quoted || out.Len() == 0 || out.Len() > tsMaxLexemeBytes || !utf8.ValidString(out.String()) {
		return "", 0, sqlError("42601", "syntax error in tsvector")
	}
	return out.String(), offset, nil
}

func parseTSPositions(input string, offset int) ([]tsPosition, int, error) {
	positions := make([]tsPosition, 0, 4)
	for {
		start := offset
		for offset < len(input) && input[offset] >= '0' && input[offset] <= '9' {
			offset++
		}
		if start == offset {
			return nil, 0, sqlError("42601", "syntax error in tsvector positions")
		}
		position, err := strconv.Atoi(input[start:offset])
		if err != nil || position <= 0 || position > tsMaxPosition {
			return nil, 0, sqlError("22023", "position is out of range")
		}
		weight := 0
		if offset < len(input) {
			switch input[offset] {
			case 'A', 'a':
				weight, offset = 3, offset+1
			case 'B', 'b':
				weight, offset = 2, offset+1
			case 'C', 'c':
				weight, offset = 1, offset+1
			case 'D', 'd':
				offset++
			}
		}
		positions = append(positions, makeTSPosition(position, weight))
		if len(positions) > tsMaxPositions {
			return nil, 0, sqlError("54000", "too many positions in tsvector")
		}
		if offset == len(input) || input[offset] != ',' {
			return normalizeTSPositions(positions), offset, nil
		}
		offset++
	}
}

func formatTSVector(ctx context.Context, data []byte, budget int64) (string, error) {
	lexemes, err := decodeTSVector(ctx, data)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for i, lexeme := range lexemes {
		if i != 0 {
			out.WriteByte(' ')
		}
		out.WriteByte('\'')
		for _, c := range lexeme.text {
			if c == '\'' || c == '\\' {
				out.WriteRune(c)
			}
			out.WriteRune(c)
		}
		out.WriteByte('\'')
		if len(lexeme.positions) != 0 {
			out.WriteByte(':')
			for j, position := range lexeme.positions {
				if j != 0 {
					out.WriteByte(',')
				}
				out.WriteString(strconv.Itoa(tsPositionNumber(position)))
				switch tsPositionWeight(position) {
				case 3:
					out.WriteByte('A')
				case 2:
					out.WriteByte('B')
				case 1:
					out.WriteByte('C')
				}
			}
		}
		if err := outputBudget(int64(out.Len()), budget); err != nil {
			return "", err
		}
	}
	return out.String(), nil
}

func compareTSVector(left, right []tsLexeme, leftBytes, rightBytes int) int {
	if leftBytes != rightBytes {
		if leftBytes < rightBytes { return -1 }
		return 1
	}
	if len(left) != len(right) {
		if len(left) < len(right) { return -1 }
		return 1
	}
	for i := range left {
		lh, rh := len(left[i].positions) != 0, len(right[i].positions) != 0
		if lh != rh {
			if lh { return -1 }
			return 1
		}
		if order := bytes.Compare([]byte(left[i].text), []byte(right[i].text)); order != 0 { return order }
		if len(left[i].positions) != len(right[i].positions) {
			if len(left[i].positions) > len(right[i].positions) { return -1 }
			return 1
		}
		for j := range left[i].positions {
			lp, rp := tsPositionNumber(left[i].positions[j]), tsPositionNumber(right[i].positions[j])
			if lp != rp { if lp > rp { return -1 }; return 1 }
			lw, rw := tsPositionWeight(left[i].positions[j]), tsPositionWeight(right[i].positions[j])
			if lw != rw { if lw > rw { return -1 }; return 1 }
		}
	}
	return 0
}

func tsVectorWeight(value model.Value) (int, error) {
	if len(value.StrVal) != 1 {
		return 0, sqlError("22023", "unrecognized weight")
	}
	switch value.StrVal[0] {
	case 'A', 'a': return 3, nil
	case 'B', 'b': return 2, nil
	case 'C', 'c': return 1, nil
	case 'D', 'd': return 0, nil
	default: return 0, sqlError("22023", "unrecognized weight")
	}
}

func isTSSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v' }

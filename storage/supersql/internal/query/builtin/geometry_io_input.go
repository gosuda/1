package builtin

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type geoTextParser struct {
	ctx       context.Context
	text      string
	name      string
	pos       int
	lastParen int
	err       error
	budget    CallBudget
}

func (p *geoTextParser) peek() byte {
	if p.pos >= len(p.text) || p.err != nil {
		return 0
	}
	return p.text[p.pos]
}
func (p *geoTextParser) advance() {
	p.pos++
	if p.pos%4096 == 0 && p.err == nil {
		p.err = p.ctx.Err()
	}
}
func (p *geoTextParser) space() {
	for isTSSpace(p.peek()) {
		p.advance()
	}
}
func (p *geoTextParser) take(c byte) bool {
	if p.peek() != c {
		return false
	}
	p.advance()
	return true
}
func (p *geoTextParser) syntax() error {
	if p.err != nil {
		return p.err
	}
	budget := p.budget
	budget.OutputBytes = math.MaxInt64
	claim, err := reserveOutputBytes(p.ctx, uint64(len(p.text))*2+128, budget)
	if err != nil {
		return err
	}
	if claim != nil {
		defer claim.Release()
	}
	return sqlError("22P02", fmt.Sprintf("invalid input syntax for type %s: \"%s\"", p.name, p.text))
}
func (p *geoTextParser) rangeError(token string) error {
	budget := p.budget
	budget.OutputBytes = math.MaxInt64
	claim, err := reserveOutputBytes(p.ctx, uint64(len(token))*2+128, budget)
	if err != nil {
		return err
	}
	if claim != nil {
		defer claim.Release()
	}
	return sqlError("22003", fmt.Sprintf("\"%s\" is out of range for type double precision", token))
}
func geoTextDigit(c byte, hex bool) bool {
	return c >= '0' && c <= '9' || hex && (c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F')
}

func geoNaNPayload(ctx context.Context, text string) (uint64, error) {
	base := uint64(10)
	if len(text) > 1 && text[0] == '0' {
		base = 8
		if text[1] == 'x' || text[1] == 'X' {
			base = 16
			text = text[2:]
		}
	}
	var value uint64
	overflow := false
	for i := range len(text) {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		c := text[i]
		var digit uint64
		switch {
		case c >= '0' && c <= '9':
			digit = uint64(c - '0')
		case c >= 'a' && c <= 'f':
			digit = uint64(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			digit = uint64(c - 'A' + 10)
		default:
			return 0, nil
		}
		if digit >= base {
			return 0, nil
		}
		if value > (math.MaxUint64-digit)/base {
			overflow = true
		}
		value = value*base + digit
	}
	if overflow {
		return 0, nil
	}
	return value & 0x000fffffffffffff, nil
}

func geoNaNPayloadCharacter(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	default:
		return c == '_'
	}
}

func (p *geoTextParser) nanNumber() (float64, error) {
	value := math.Float64frombits(0x7ff8000000000000)
	if p.peek() != '(' {
		return value, nil
	}
	saved := p.pos
	p.advance()
	for geoNaNPayloadCharacter(p.peek()) {
		p.advance()
	}
	if !p.take(')') {
		p.pos = saved
		return value, p.err
	}
	payload, err := geoNaNPayload(p.ctx, p.text[saved+1:p.pos-1])
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(0x7ff8000000000000 | payload), nil
}

func (p *geoTextParser) specialNumber(negative bool) (float64, bool, error) {
	for _, word := range []string{"infinity", "inf", "nan"} {
		if len(p.text)-p.pos < len(word) || !strings.EqualFold(p.text[p.pos:p.pos+len(word)], word) {
			continue
		}
		p.pos += len(word)
		value := math.Inf(1)
		if word == "nan" {
			var err error
			value, err = p.nanNumber()
			if err != nil {
				return 0, true, err
			}
		}
		if negative {
			value = -value
		}
		p.space()
		return value, true, p.err
	}
	return 0, false, nil
}

func (p *geoTextParser) takeHexPrefix() bool {
	if p.peek() != '0' || p.pos+1 >= len(p.text) {
		return false
	}
	if p.text[p.pos+1] != 'x' && p.text[p.pos+1] != 'X' {
		return false
	}
	next := p.pos + 2
	if next < len(p.text) && p.text[next] == '.' {
		next++
	}
	if next >= len(p.text) || !geoTextDigit(p.text[next], true) {
		return false
	}
	p.pos += 2
	return true
}

func geoExponentCharacter(c byte, hex bool) bool {
	if hex {
		return c == 'p' || c == 'P'
	}
	return c == 'e' || c == 'E'
}

// Scan strtod's numeric prefix, leaving delimiters to the enclosing grammar.
func (p *geoTextParser) number() (float64, error) {
	p.space()
	start := p.pos
	negative := p.take('-')
	if !negative {
		p.take('+')
	}
	if value, found, err := p.specialNumber(negative); found || err != nil {
		return value, err
	}
	hex := p.takeHexPrefix()
	digits, nonzero := 0, false
	for geoTextDigit(p.peek(), hex) {
		nonzero = nonzero || p.peek() != '0'
		digits++
		p.advance()
	}
	if p.take('.') {
		for geoTextDigit(p.peek(), hex) {
			nonzero = nonzero || p.peek() != '0'
			digits++
			p.advance()
		}
	}
	if p.err != nil {
		return 0, p.err
	}
	if digits == 0 {
		return 0, p.syntax()
	}
	hasExponent := false
	if geoExponentCharacter(p.peek(), hex) {
		saved := p.pos
		p.advance()
		if !p.take('-') {
			p.take('+')
		}
		if geoTextDigit(p.peek(), false) {
			hasExponent = true
			for geoTextDigit(p.peek(), false) {
				p.advance()
			}
		} else {
			p.pos = saved
		}
	}
	if p.err != nil {
		return 0, p.err
	}
	token := p.text[start:p.pos]
	// strconv's error object retains a copy of the token, and exponentless hex
	// needs a suffix. Admit that temporary storage independently of output size.
	budget := p.budget
	budget.OutputBytes = math.MaxInt64
	if len(token) > 64 {
		claim, err := reserveOutputBytes(p.ctx, uint64(len(token))*2+128, budget)
		if err != nil {
			return 0, err
		}
		if claim != nil {
			defer claim.Release()
		}
	}
	parsed := token
	if hex && !hasExponent {
		parsed += "p0"
	}
	value, err := strconv.ParseFloat(parsed, 64)
	if err != nil || value == 0 && nonzero {
		return 0, p.rangeError(token)
	}
	p.space()
	if p.err != nil {
		return 0, p.err
	}
	return value, nil
}

func (p *geoTextParser) pair() (geoPoint, error) {
	p.space()
	paren := p.take('(')
	x, err := p.number()
	if err != nil {
		return geoPoint{}, err
	}
	if !p.take(',') {
		return geoPoint{}, p.syntax()
	}
	y, err := p.number()
	if err != nil {
		return geoPoint{}, err
	}
	if paren {
		if !p.take(')') {
			return geoPoint{}, p.syntax()
		}
		p.space()
	}
	return geoPoint{x, y}, nil
}

func (p *geoTextParser) path(openType bool, count int, points []byte) (bool, error) {
	p.space()
	depth := false
	open := p.take('[')
	if open {
		if !openType {
			return false, p.syntax()
		}
		depth = true
	} else if p.peek() == '(' {
		saved := p.pos
		p.advance()
		p.space()
		if p.peek() == '(' || p.lastParen == saved {
			depth = true
		} else {
			p.pos = saved
		}
	}
	for i := range count {
		if i%256 == 0 {
			if err := p.ctx.Err(); err != nil {
				return false, err
			}
		}
		point, err := p.pair()
		if err != nil {
			return false, err
		}
		if points != nil {
			geoWritePoint(points[i*16:], point)
		}
		p.take(',')
	}
	if depth {
		if !p.take(')') && !(open && p.take(']')) {
			return false, p.syntax()
		}
		p.space()
	}
	return open, nil
}

func geoInput(ctx context.Context, text string, id model.TypeID, budget CallBudget) (model.Value, error) {
	p := geoTextParser{ctx: ctx, text: text, lastParen: -1, budget: budget}
	switch id {
	case 600:
		p.name = "point"
	case 601:
		p.name = "lseg"
	case 602:
		p.name = "path"
	case 603:
		p.name = "box"
	case 604:
		p.name = "polygon"
	case 628:
		p.name = "line"
	case 718:
		p.name = "circle"
	}
	commas := 0
	for i := range len(text) {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		if text[i] == 0 {
			p.text = text[:i]
			break
		}
		if text[i] == ',' {
			commas++
		}
		if text[i] == '(' {
			p.lastParen = i
		}
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	switch id {
	case 600:
		point, err := p.pair()
		if err != nil {
			return model.Value{}, err
		}
		if p.peek() != 0 || p.err != nil {
			return model.Value{}, p.syntax()
		}
		return geoPointValue(ctx, point, budget)
	case 601, 603, 628:
		p.space()
		if id == 628 && p.take('{') {
			a, err := p.number()
			if err != nil {
				return model.Value{}, err
			}
			if !p.take(',') {
				return model.Value{}, p.syntax()
			}
			b, err := p.number()
			if err != nil {
				return model.Value{}, err
			}
			if !p.take(',') {
				return model.Value{}, p.syntax()
			}
			c, err := p.number()
			if err != nil {
				return model.Value{}, err
			}
			if !p.take('}') {
				return model.Value{}, p.syntax()
			}
			p.space()
			if p.peek() != 0 || p.err != nil {
				return model.Value{}, p.syntax()
			}
			if geoFPZero(a) && geoFPZero(b) {
				return model.Value{}, sqlError("22P02", "invalid line specification: A and B cannot both be zero")
			}
			return geoLineValue(ctx, geoLine{a, b, c}, budget)
		}
		var points [32]byte
		if _, err := p.path(id != 603, 2, points[:]); err != nil {
			return model.Value{}, err
		}
		if p.peek() != 0 || p.err != nil {
			return model.Value{}, p.syntax()
		}
		a, b := geoReadPoint(points[:]), geoReadPoint(points[16:])
		if id == 603 {
			box := geoBox{high: a, low: b}
			if compareFloat(a.x, b.x) < 0 {
				box.high.x, box.low.x = b.x, a.x
			}
			if compareFloat(a.y, b.y) < 0 {
				box.high.y, box.low.y = b.y, a.y
			}
			return geoBoxValue(ctx, box, budget)
		}
		if id == 601 {
			return geoSegmentValue(ctx, geoSegment{a, b}, budget)
		}
		if geoPointSame(a, b) {
			return model.Value{}, sqlError("22P02", "invalid line specification: must be two distinct points")
		}
		line, err := geoLineFromPoints(a, b)
		if err != nil {
			return model.Value{}, err
		}
		return geoLineValue(ctx, line, budget)
	case 602, 604:
		if commas%2 != 1 {
			return model.Value{}, p.syntax()
		}
		count := (commas + 1) / 2
		header := 16
		if id == 604 {
			header = 40
		}
		if count > (math.MaxInt32-header)/16 {
			return model.Value{}, sqlError("54000", "too many points requested")
		}
		p.space()
		outer := false
		if id == 602 && p.peek() == '(' && p.lastParen == p.pos {
			p.advance()
			outer = true
		}
		start := p.pos
		open, err := p.path(id == 602, count, nil)
		if err != nil {
			return model.Value{}, err
		}
		if outer {
			if !p.take(')') {
				return model.Value{}, p.syntax()
			}
			p.space()
		}
		if p.peek() != 0 || p.err != nil {
			return model.Value{}, p.syntax()
		}
		var buffer byteResultBuffer
		if id == 602 {
			buffer, err = newGeoPath(ctx, count, !open, budget)
		} else {
			buffer, err = newGeoPolygon(ctx, count, budget)
		}
		if err != nil {
			return model.Value{}, err
		}
		defer buffer.close()
		offset := 4
		if id == 602 {
			offset = 5
		}
		p.pos = start
		p.budget.MemoryBytes -= uint64(len(buffer.data))
		if _, err = p.path(id == 602, count, buffer.data[offset:]); err != nil {
			return model.Value{}, err
		}
		return buffer.value(), nil
	case 718:
		p.space()
		depth := p.take('<')
		if !depth && p.peek() == '(' {
			saved := p.pos
			p.advance()
			p.space()
			if p.peek() == '(' {
				depth = true
			} else {
				p.pos = saved
			}
		}
		center, err := p.pair()
		if err != nil {
			return model.Value{}, err
		}
		p.take(',')
		radius, err := p.number()
		if err != nil {
			return model.Value{}, err
		}
		if radius < 0 {
			return model.Value{}, p.syntax()
		}
		if depth {
			if !p.take(')') && !p.take('>') {
				return model.Value{}, p.syntax()
			}
			p.space()
		}
		if p.peek() != 0 || p.err != nil {
			return model.Value{}, p.syntax()
		}
		return geoCircleValue(ctx, geoCircle{center, radius}, budget)
	}
	return model.Value{}, invalidGeometry()
}

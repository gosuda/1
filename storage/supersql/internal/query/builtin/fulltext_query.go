package builtin

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

)

const (
	tsQueryValue  = 1
	tsQueryOp     = 2
	tsQueryNot    = 1
	tsQueryAnd    = 2
	tsQueryOr     = 3
	tsQueryPhrase = 4
)

type tsQueryNode struct {
	kind     uint8
	op       uint8
	weight   uint8
	prefix   bool
	distance uint16
	word     string
	left     *tsQueryNode
	right    *tsQueryNode
}

func parseTSQuery(ctx context.Context, input string, budget int64) ([]byte, error) {
	parser := tsQueryParser{ctx: ctx, input: input}
	parser.space()
	if parser.offset == len(input) {
		return encodeTSQuery(ctx, nil, budget)
	}
	root, err := parser.or()
	if err != nil {
		return nil, err
	}
	parser.space()
	if parser.offset != len(input) {
		return nil, sqlError("42601", "syntax error in tsquery")
	}
	return encodeTSQuery(ctx, root, budget)
}

type tsQueryParser struct {
	ctx    context.Context
	input  string
	offset int
	depth  int
}

func (p *tsQueryParser) space() {
	for p.offset < len(p.input) && isTSSpace(p.input[p.offset]) { p.offset++ }
}

func (p *tsQueryParser) or() (*tsQueryNode, error) {
	left, err := p.and()
	if err != nil { return nil, err }
	for {
		p.space()
		if p.offset == len(p.input) || p.input[p.offset] != '|' { return left, nil }
		p.offset++
		right, err := p.and()
		if err != nil { return nil, err }
		left = &tsQueryNode{kind: tsQueryOp, op: tsQueryOr, left: left, right: right}
	}
}

func (p *tsQueryParser) and() (*tsQueryNode, error) {
	left, err := p.phrase()
	if err != nil { return nil, err }
	for {
		p.space()
		if p.offset == len(p.input) || p.input[p.offset] != '&' { return left, nil }
		p.offset++
		right, err := p.phrase()
		if err != nil { return nil, err }
		left = &tsQueryNode{kind: tsQueryOp, op: tsQueryAnd, left: left, right: right}
	}
}

func (p *tsQueryParser) phrase() (*tsQueryNode, error) {
	left, err := p.unary()
	if err != nil { return nil, err }
	for {
		p.space()
		distance, ok, err := p.phraseOperator()
		if err != nil { return nil, err }
		if !ok { return left, nil }
		right, err := p.unary()
		if err != nil { return nil, err }
		left = &tsQueryNode{kind: tsQueryOp, op: tsQueryPhrase, distance: distance, left: left, right: right}
	}
}

func (p *tsQueryParser) phraseOperator() (uint16, bool, error) {
	if p.offset >= len(p.input) || p.input[p.offset] != '<' { return 0, false, nil }
	start := p.offset
	p.offset++
	if p.offset < len(p.input) && p.input[p.offset] == '-' {
		p.offset++
		if p.offset < len(p.input) && p.input[p.offset] == '>' {
			p.offset++
			return 1, true, nil
		}
		p.offset = start
		return 0, false, nil
	}
	digits := p.offset
	for p.offset < len(p.input) && p.input[p.offset] >= '0' && p.input[p.offset] <= '9' { p.offset++ }
	if digits == p.offset || p.offset >= len(p.input) || p.input[p.offset] != '>' {
		p.offset = start
		return 0, false, nil
	}
	number, err := strconv.Atoi(p.input[digits:p.offset])
	if err != nil || number > tsMaxPosition+1 {
		return 0, false, sqlError("22023", "distance in phrase operator must be between zero and 16384")
	}
	p.offset++
	return uint16(number), true, nil
}

func (p *tsQueryParser) unary() (*tsQueryNode, error) {
	if err := p.ctx.Err(); err != nil { return nil, err }
	p.space()
	if p.offset == len(p.input) { return nil, sqlError("42601", "no operand in tsquery") }
	if p.input[p.offset] == '!' {
		p.offset++
		child, err := p.unary()
		if err != nil { return nil, err }
		return &tsQueryNode{kind: tsQueryOp, op: tsQueryNot, right: child}, nil
	}
	if p.input[p.offset] == '(' {
		p.depth++
		if p.depth > 512 { return nil, sqlError("54001", "tsquery nesting depth exceeded") }
		p.offset++
		p.space()
		if p.offset < len(p.input) && p.input[p.offset] == ')' { return nil, sqlError("42601", "no operand in tsquery") }
		node, err := p.or()
		if err != nil { return nil, err }
		p.space()
		if p.offset == len(p.input) || p.input[p.offset] != ')' { return nil, sqlError("42601", "unbalanced parentheses in tsquery") }
		p.offset++
		p.depth--
		return node, nil
	}
	return p.operand()
}

func (p *tsQueryParser) operand() (*tsQueryNode, error) {
	start := p.offset
	var out strings.Builder
	quoted := p.input[p.offset] == '\''
	if quoted { p.offset++ }
	for p.offset < len(p.input) {
		c := p.input[p.offset]
		if c == '\\' {
			p.offset++
			if p.offset == len(p.input) { return nil, sqlError("42601", "syntax error in tsquery") }
			out.WriteByte(p.input[p.offset]); p.offset++; continue
		}
		if quoted && c == '\'' {
			if p.offset+1 < len(p.input) && p.input[p.offset+1] == '\'' { out.WriteByte('\''); p.offset += 2; continue }
			p.offset++
			break
		}
		if !quoted && (isTSSpace(c) || strings.ContainsRune("!&|()<>:", rune(c))) { break }
		out.WriteByte(c); p.offset++
	}
	if out.Len() == 0 || out.Len() > tsMaxLexemeBytes || !utf8.ValidString(out.String()) || (quoted && (p.offset == len(p.input) && p.input[len(p.input)-1] != '\'')) {
		p.offset = start
		return nil, sqlError("42601", "syntax error in tsquery")
	}
	node := &tsQueryNode{kind: tsQueryValue, word: out.String()}
	if p.offset < len(p.input) && p.input[p.offset] == ':' {
		p.offset++
		for p.offset < len(p.input) {
			switch p.input[p.offset] {
			case '*': node.prefix = true
			case 'A', 'a': node.weight |= 1 << 3
			case 'B', 'b': node.weight |= 1 << 2
			case 'C', 'c': node.weight |= 1 << 1
			case 'D', 'd': node.weight |= 1
			default: return node, nil
			}
			p.offset++
		}
	}
	return node, nil
}

func decodeTSQuery(ctx context.Context, data []byte) (*tsQueryNode, int, error) {
	if len(data) < 4 { return nil, 0, sqlError("22P03", "invalid binary representation for tsquery") }
	count := int64(int32(binary.BigEndian.Uint32(data)))
	if count < 0 || count > int64(len(data)-4) { return nil, 0, sqlError("22P03", "invalid size of tsquery") }
	if count == 0 {
		if len(data) != 4 { return nil, 0, sqlError("22P03", "invalid binary representation for tsquery") }
		return nil, 0, nil
	}
	offset, seen := 4, 0
	var readNode func(int) (*tsQueryNode, error)
	readNode = func(depth int) (*tsQueryNode, error) {
		if err := ctx.Err(); err != nil { return nil, err }
		if depth > 512 || seen >= int(count) || offset >= len(data) { return nil, sqlError("22P03", "invalid tsquery tree") }
		kind := data[offset]; offset++; seen++
		node := &tsQueryNode{kind: kind}
		switch kind {
		case tsQueryValue:
			if len(data)-offset < 3 { return nil, sqlError("22P03", "invalid tsquery operand") }
			node.weight = data[offset]; node.prefix = data[offset+1] != 0
			if node.weight > 15 || data[offset+1] > 1 { return nil, sqlError("22P03", "invalid tsquery operand modifiers") }
			offset += 2
			end := bytes.IndexByte(data[offset:], 0)
			if end <= 0 || end > tsMaxLexemeBytes || !utf8.Valid(data[offset:offset+end]) { return nil, sqlError("22P03", "invalid tsquery operand") }
			node.word = string(data[offset:offset+end]); offset += end+1
		case tsQueryOp:
			if offset >= len(data) { return nil, sqlError("22P03", "invalid tsquery operator") }
			node.op = data[offset]; offset++
			if node.op < tsQueryNot || node.op > tsQueryPhrase { return nil, sqlError("22P03", "invalid tsquery operator") }
			if node.op == tsQueryPhrase {
				if len(data)-offset < 2 { return nil, sqlError("22P03", "invalid phrase operator") }
				node.distance = binary.BigEndian.Uint16(data[offset:]); offset += 2
				if node.distance > tsMaxPosition+1 { return nil, sqlError("22P03", "invalid phrase distance") }
			}
			var err error
			node.right, err = readNode(depth+1)
			if err != nil { return nil, err }
			if node.op != tsQueryNot {
				node.left, err = readNode(depth+1)
				if err != nil { return nil, err }
			}
		default:
			return nil, sqlError("22P03", "invalid tsquery item type")
		}
		return node, nil
	}
	root, err := readNode(0)
	if err != nil { return nil, 0, err }
	if seen != int(count) || offset != len(data) { return nil, 0, sqlError("22P03", "invalid binary representation for tsquery") }
	return root, seen, nil
}

func encodeTSQuery(ctx context.Context, root *tsQueryNode, budget int64) ([]byte, error) {
	count, payload, err := measureTSQuery(ctx, root, 0)
	if err != nil { return nil, err }
	size := 4 + payload
	if err := outputBudget(int64(size), budget); err != nil { return nil, err }
	out := make([]byte, size)
	binary.BigEndian.PutUint32(out, uint32(count))
	offset := 4
	var put func(*tsQueryNode)
	put = func(node *tsQueryNode) {
		if node == nil { return }
		out[offset] = node.kind; offset++
		if node.kind == tsQueryValue {
			out[offset], out[offset+1] = node.weight, 0
			if node.prefix { out[offset+1] = 1 }
			offset += 2
			copy(out[offset:], node.word); offset += len(node.word)+1
			return
		}
		out[offset] = node.op; offset++
		if node.op == tsQueryPhrase { binary.BigEndian.PutUint16(out[offset:], node.distance); offset += 2 }
		put(node.right)
		if node.op != tsQueryNot { put(node.left) }
	}
	put(root)
	return out, nil
}

func measureTSQuery(ctx context.Context, node *tsQueryNode, depth int) (int, int, error) {
	if node == nil { return 0, 0, nil }
	if err := ctx.Err(); err != nil { return 0, 0, err }
	if depth > 512 { return 0, 0, sqlError("54001", "tsquery nesting depth exceeded") }
	if node.kind == tsQueryValue {
		if len(node.word) == 0 || len(node.word) > tsMaxLexemeBytes || !utf8.ValidString(node.word) { return 0, 0, sqlError("54000", "tsquery operand exceeds PostgreSQL limits") }
		return 1, len(node.word)+4, nil
	}
	if node.op < tsQueryNot || node.op > tsQueryPhrase || node.right == nil || (node.op != tsQueryNot && node.left == nil) { return 0, 0, sqlError("XX000", "invalid tsquery tree") }
	rc, rb, err := measureTSQuery(ctx, node.right, depth+1); if err != nil { return 0, 0, err }
	lc, lb := 0, 0
	if node.op != tsQueryNot { lc, lb, err = measureTSQuery(ctx, node.left, depth+1); if err != nil { return 0, 0, err } }
	payload := 2 + rb + lb
	if node.op == tsQueryPhrase { payload += 2 }
	return 1+rc+lc, payload, nil
}

func formatTSQuery(ctx context.Context, data []byte, budget int64) (string, error) {
	root, _, err := decodeTSQuery(ctx, data)
	if err != nil { return "", err }
	var out strings.Builder
	formatTSQueryNode(&out, root, -1, false)
	if err := outputBudget(int64(out.Len()), budget); err != nil { return "", err }
	return out.String(), nil
}

func tsQueryPriority(node *tsQueryNode) int {
	if node == nil || node.kind == tsQueryValue { return 5 }
	switch node.op { case tsQueryOr: return 1; case tsQueryAnd: return 2; case tsQueryPhrase: return 3; default: return 4 }
}

func formatTSQueryNode(out *strings.Builder, node *tsQueryNode, parent int, rightPhrase bool) {
	if node == nil { return }
	if node.kind == tsQueryValue {
		out.WriteByte('\'')
		for _, c := range node.word { if c == '\'' || c == '\\' { out.WriteRune(c) }; out.WriteRune(c) }
		out.WriteByte('\'')
		if node.prefix || node.weight != 0 {
			out.WriteByte(':'); if node.prefix { out.WriteByte('*') }
			if node.weight&(1<<3) != 0 { out.WriteByte('A') }; if node.weight&(1<<2) != 0 { out.WriteByte('B') }; if node.weight&(1<<1) != 0 { out.WriteByte('C') }; if node.weight&1 != 0 { out.WriteByte('D') }
		}
		return
	}
	priority := tsQueryPriority(node)
	parentheses := priority < parent || (node.op == tsQueryPhrase && rightPhrase)
	if parentheses { out.WriteString("( ") }
	if node.op == tsQueryNot {
		out.WriteByte('!'); formatTSQueryNode(out, node.right, priority, false)
	} else {
		formatTSQueryNode(out, node.left, priority, false)
		switch node.op { case tsQueryOr: out.WriteString(" | "); case tsQueryAnd: out.WriteString(" & "); case tsQueryPhrase: if node.distance == 1 { out.WriteString(" <-> ") } else { out.WriteByte(' '); out.WriteByte('<'); out.WriteString(strconv.Itoa(int(node.distance))); out.WriteString("> ") } }
		formatTSQueryNode(out, node.right, priority, node.op == tsQueryPhrase)
	}
	if parentheses { out.WriteString(" )") }
}

func tsQueryInternalSize(node *tsQueryNode) (nodes, bytes int) {
	if node == nil { return 0, 8 }
	leftNodes, leftBytes := tsQueryInternalSize(node.left)
	rightNodes, rightBytes := tsQueryInternalSize(node.right)
	word := 0; if node.kind == tsQueryValue { word = len(node.word)+1 }
	return 1+leftNodes+rightNodes, 8+12*(1+leftNodes+rightNodes)+word+(leftBytes-8)+(rightBytes-8)
}

func compareTSQuery(left, right *tsQueryNode) int {
	ln, lb := tsQueryInternalSize(left); rn, rb := tsQueryInternalSize(right)
	if ln != rn { if ln < rn { return -1 }; return 1 }
	if lb != rb { if lb < rb { return -1 }; return 1 }
	return compareTSQueryNode(left, right)
}

func compareTSQueryNode(left, right *tsQueryNode) int {
	if left == nil || right == nil { if left == right { return 0 }; if left == nil { return -1 }; return 1 }
	if left.kind != right.kind { if left.kind > right.kind { return -1 }; return 1 }
	if left.kind == tsQueryOp {
		if left.op != right.op { if left.op > right.op { return -1 }; return 1 }
		if order := compareTSQueryNode(left.right, right.right); order != 0 { return order }
		if left.op != tsQueryNot { if order := compareTSQueryNode(left.left, right.left); order != 0 { return order } }
		if left.op == tsQueryPhrase && left.distance != right.distance { if left.distance > right.distance { return -1 }; return 1 }
		return 0
	}
	lc, rc := int32(crc32.ChecksumIEEE([]byte(left.word))), int32(crc32.ChecksumIEEE([]byte(right.word)))
	if lc != rc { if lc > rc { return -1 }; return 1 }
	return bytes.Compare([]byte(left.word), []byte(right.word))
}

func cloneTSQuery(node *tsQueryNode) *tsQueryNode {
	if node == nil { return nil }
	copy := *node; copy.left = cloneTSQuery(node.left); copy.right = cloneTSQuery(node.right); return &copy
}

func joinTSQuery(op uint8, distance uint16, left, right *tsQueryNode) *tsQueryNode {
	if left == nil { return cloneTSQuery(right) }; if right == nil { return cloneTSQuery(left) }
	return &tsQueryNode{kind: tsQueryOp, op: op, distance: distance, left: cloneTSQuery(left), right: cloneTSQuery(right)}
}

func tsQueryPositiveOperands(root *tsQueryNode) []*tsQueryNode {
	values := make([]*tsQueryNode, 0)
	var visit func(*tsQueryNode, bool)
	visit = func(node *tsQueryNode, negated bool) {
		if node == nil { return }
		if node.kind == tsQueryValue {
			if !negated { values = append(values, node) }
			return
		}
		if node.op == tsQueryNot {
			visit(node.right, !negated)
			return
		}
		visit(node.left, negated)
		visit(node.right, negated)
	}
	visit(root, false)
	return values
}

func rewriteTSQuery(node, target, substitute *tsQueryNode) *tsQueryNode {
	if node == nil { return nil }
	if compareTSQueryNode(node, target) == 0 { return cloneTSQuery(substitute) }
	if node.kind == tsQueryValue { return cloneTSQuery(node) }
	if node.op == tsQueryNot {
		child := rewriteTSQuery(node.right, target, substitute)
		if child == nil { return nil }
		return &tsQueryNode{kind: tsQueryOp, op: tsQueryNot, right: child}
	}
	left, right := rewriteTSQuery(node.left, target, substitute), rewriteTSQuery(node.right, target, substitute)
	if left == nil { return right }; if right == nil { return left }
	return &tsQueryNode{kind: tsQueryOp, op: node.op, distance: node.distance, left: left, right: right}
}

func tsQueryValues(root *tsQueryNode) []string {
	values := make([]string, 0)
	var visit func(*tsQueryNode)
	visit = func(node *tsQueryNode) { if node == nil { return }; if node.kind == tsQueryValue { values = append(values, node.word); return }; visit(node.left); visit(node.right) }
	visit(root); sort.Strings(values)
	return slicesCompact(values)
}

func slicesCompact(values []string) []string {
	if len(values) < 2 { return values }
	out := values[:1]
	for _, value := range values[1:] { if value != out[len(out)-1] { out = append(out, value) } }
	return out
}

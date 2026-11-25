package builtin

import (
	"context"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/columnar"
)

type regexpSyntax uint8

const (
	regexpARE regexpSyntax = iota
	regexpERE
	regexpBRE
	regexpQuote
)

type regexpFlags struct {
	syntax             regexpSyntax
	ignoreCase         bool
	newlineSensitive   bool
	newlineAnchors     bool
	expanded           bool
	global             bool
}

type regexpPreference uint8

const (
	regexpNoPreference regexpPreference = iota
	regexpLongest
	regexpShortest
)

type regexpNodeKind uint8

const (
	regexpEmpty regexpNodeKind = iota
	regexpLiteral
	regexpDot
	regexpClassNode
	regexpConcat
	regexpAlternate
	regexpRepeat
	regexpCapture
	regexpBackref
	regexpBegin
	regexpEnd
	regexpStringBegin
	regexpStringEnd
	regexpWordBegin
	regexpWordEnd
	regexpWordBoundary
	regexpNotWordBoundary
	regexpLookAhead
	regexpNegativeLookAhead
	regexpLookBehind
	regexpNegativeLookBehind
)

type regexpRuneRange struct {
	first rune
	last  rune
}

type regexpClassKind uint8

const (
	regexpClassLiteral regexpClassKind = iota
	regexpClassRange
	regexpClassAlnum
	regexpClassAlpha
	regexpClassASCII
	regexpClassBlank
	regexpClassControl
	regexpClassDigit
	regexpClassGraph
	regexpClassLower
	regexpClassPrint
	regexpClassPunct
	regexpClassSpace
	regexpClassUpper
	regexpClassWord
	regexpClassXDigit
)

type regexpClassTerm struct {
	kind     regexpClassKind
	value    rune
	rangeVal regexpRuneRange
	negated  bool
}

type regexpClass struct {
	negated bool
	terms   []regexpClassTerm
}

type regexpNode struct {
	kind       regexpNodeKind
	children   []int
	character  rune
	class      regexpClass
	minimum    int
	maximum    int
	group      int
	preference regexpPreference
}

type regexpProgram struct {
	nodes       []regexpNode
	root        int
	groups      int
	preferences []regexpPreference
	flags       regexpFlags
	ownedBytes  uint64
}

type regexpSpan struct {
	start int
	end   int
	valid bool
}

type regexpMatch struct {
	spans []regexpSpan
}

type regexpState struct {
	position int
	spans    []regexpSpan
}

type regexpRuntime struct {
	ctx        context.Context
	limit      uint64
	baseBytes  uint64
	claimBytes uint64
	stateWidth uint64
	maxStates  int
	maxWork    uint64
	work       uint64
	claim      columnar.Allocation
}

func newRegexpRuntime(ctx context.Context, limit uint64, allocator columnar.Allocator) (*regexpRuntime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	const base = uint64(512)
	if limit < base {
		return nil, sqlError("54000", "regular expression exceeded memory budget")
	}
	var claim columnar.Allocation
	var err error
	if allocator != nil {
		claim, err = allocator.Reserve(base)
		if err != nil {
			return nil, err
		}
	}
	work := limit
	if work > math.MaxUint64/32 {
		work = math.MaxUint64
	} else {
		work *= 32
	}
	return &regexpRuntime{ctx: ctx, limit: limit, baseBytes: base, claimBytes: base, stateWidth: 120, maxStates: 1, maxWork: work, claim: claim}, nil
}

func (runtime *regexpRuntime) close() {
	if runtime.claim != nil {
		runtime.claim.Release()
		runtime.claim = nil
	}
	runtime.claimBytes = 0
}

func (runtime *regexpRuntime) grow(bytes uint64) error {
	if bytes > runtime.limit {
		return sqlError("54000", "regular expression exceeded memory budget")
	}
	if bytes <= runtime.claimBytes {
		return nil
	}
	target := min(runtime.limit, max(bytes, runtime.claimBytes*2))
	if runtime.claim != nil {
		if err := runtime.claim.Resize(target); err != nil {
			if target == bytes {
				return err
			}
			if err := runtime.claim.Resize(bytes); err != nil {
				return err
			}
			target = bytes
		}
	}
	runtime.claimBytes = target
	return nil
}

func (runtime *regexpRuntime) bindInput(text, pattern string) error {
	needed := uint64(512)
	if uint64(len(text)) > (runtime.limit-needed)/16 {
		return sqlError("54000", "regular expression exceeded memory budget")
	}
	needed += uint64(len(text)) * 16
	if uint64(len(pattern)) > (runtime.limit-needed)/256 {
		return sqlError("54000", "regular expression exceeded memory budget")
	}
	needed += uint64(len(pattern)) * 256
	if err := runtime.grow(needed); err != nil {
		return err
	}
	runtime.baseBytes = needed
	return nil
}

func (runtime *regexpRuntime) bindGroups(groups int) {
	runtime.stateWidth = uint64(96) + uint64(groups+1)*uint64(24)
	available := runtime.limit - runtime.baseBytes
	states := available / (runtime.stateWidth * 4)
	if states > uint64(math.MaxInt) {
		states = uint64(math.MaxInt)
	}
	runtime.maxStates = int(max(states, 1))
}

func (runtime *regexpRuntime) tick() error {
	runtime.work++
	if runtime.work > runtime.maxWork {
		return sqlError("54000", "regular expression exceeded work budget")
	}
	states := min(runtime.work, uint64(runtime.maxStates))
	if states > (runtime.limit-runtime.baseBytes)/runtime.stateWidth {
		return sqlError("54000", "regular expression exceeded memory budget")
	}
	if err := runtime.grow(runtime.baseBytes + states*runtime.stateWidth); err != nil {
		return err
	}
	if runtime.work&1023 == 0 {
		return runtime.ctx.Err()
	}
	return nil
}

func (runtime *regexpRuntime) appendState(states []regexpState, state regexpState) ([]regexpState, error) {
	if len(states) >= runtime.maxStates {
		return nil, sqlError("54000", "regular expression exceeded memory budget")
	}
	return append(states, state), nil
}

func regexpMemoryFits(limit uint64, values ...string) error {
	used := uint64(512)
	if used > limit {
		return sqlError("54000", "regular expression exceeded memory budget")
	}
	for _, value := range values {
		if uint64(len(value)) > (limit-used)/16 {
			return sqlError("54000", "regular expression exceeded memory budget")
		}
		used += uint64(len(value)) * 16
	}
	return nil
}

func parseRegexpFlags(flags string) (regexpFlags, error) {
	result := regexpFlags{syntax: regexpARE}
	for _, flag := range flags {
		switch flag {
		case 'b':
			result.syntax = regexpBRE
		case 'c':
			result.ignoreCase = false
		case 'e':
			result.syntax = regexpERE
		case 'g':
			result.global = true
		case 'i':
			result.ignoreCase = true
		case 'm', 'n':
			result.newlineSensitive = true
			result.newlineAnchors = true
		case 'p':
			result.newlineSensitive = true
			result.newlineAnchors = false
		case 'q':
			result.syntax = regexpQuote
		case 's':
			result.newlineSensitive = false
			result.newlineAnchors = false
		case 't':
			result.expanded = false
		case 'w':
			result.newlineSensitive = false
			result.newlineAnchors = true
		case 'x':
			result.expanded = true
		default:
			return regexpFlags{}, sqlError("22023", "invalid regular expression option: "+strconv.QuoteRune(flag))
		}
	}
	return result, nil
}

func rejectRegexpGlobal(function string, flags regexpFlags) error {
	if flags.global {
		return sqlError("22023", function+"() does not support the \"global\" option")
	}
	return nil
}

func decodeRegexpRunes(ctx context.Context, value string) ([]rune, []int, error) {
	runes := make([]rune, 0, utf8.RuneCountInString(value))
	offsets := make([]int, 0, cap(runes)+1)
	for at := 0; at < len(value); {
		if at&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
		}
		character, width := utf8.DecodeRuneInString(value[at:])
		if (character == utf8.RuneError && width == 1) || character == 0 {
			return nil, nil, sqlError("22021", "invalid UTF8 text datum")
		}
		offsets = append(offsets, at)
		runes = append(runes, character)
		at += width
	}
	offsets = append(offsets, len(value))
	return runes, offsets, nil
}

type regexpParser struct {
	ctx          context.Context
	input        []rune
	position     int
	flags        regexpFlags
	nodes        []regexpNode
	groups       int
	closedGroups []bool
	inLookaround int
	depth        int
	limit        uint64
}
func compileRegexp(ctx context.Context, pattern string, flags regexpFlags, limit uint64) (regexpProgram, error) {
	if err := regexpMemoryFits(limit, pattern); err != nil {
		return regexpProgram{}, err
	}
	input, _, err := decodeRegexpRunes(ctx, pattern)
	if err != nil {
		return regexpProgram{}, err
	}
	if uint64(len(input)) > (limit-512)/256 {
		return regexpProgram{}, sqlError("54000", "regular expression exceeded memory budget")
	}
	parser := regexpParser{ctx: ctx, input: input, flags: flags, limit: limit, closedGroups: []bool{false}}
	if err := parser.initialOptions(); err != nil {
		return regexpProgram{}, err
	}
	if parser.flags.syntax == regexpQuote {
		children := make([]int, 0, len(parser.input)-parser.position)
		for parser.position < len(parser.input) {
			children = append(children, parser.addNode(regexpNode{kind: regexpLiteral, character: parser.input[parser.position]}))
			parser.position++
		}
		root := parser.combine(regexpConcat, children)
		return parser.program(root)
	}
	root, err := parser.parseAlternation(false)
	if err != nil {
		return regexpProgram{}, err
	}
	parser.skipExpanded()
	if parser.position != len(parser.input) {
		return regexpProgram{}, regexpCompileError("parentheses () not balanced")
	}
	return parser.program(root)
}

func (parser *regexpParser) program(root int) (regexpProgram, error) {
	owned := uint64(256 + len(parser.nodes)*80 + len(parser.closedGroups))
	preferences := make([]regexpPreference, parser.groups+1)
	parser.capturePreferences(root, regexpNoPreference, preferences)
	for _, node := range parser.nodes {
		owned += uint64(len(node.children))*8 + uint64(len(node.class.terms))*32
	}
	if owned > parser.limit {
		return regexpProgram{}, sqlError("54000", "regular expression exceeded memory budget")
	}
	return regexpProgram{nodes: parser.nodes, root: root, groups: parser.groups, preferences: preferences, flags: parser.flags, ownedBytes: owned}, nil
}
func (parser *regexpParser) capturePreferences(index int, inherited regexpPreference, preferences []regexpPreference) {
	node := parser.nodes[index]
	effective := inherited
	if node.kind == regexpRepeat && effective == regexpNoPreference {
		effective = node.preference
	}
	if node.kind == regexpCapture {
		preference := effective
		if preference == regexpNoPreference {
			preference = parser.nodes[node.children[0]].preference
		}
		preferences[node.group] = preference
	}
	for _, child := range node.children {
		parser.capturePreferences(child, effective, preferences)
	}
}


func (parser *regexpParser) initialOptions() error {
	if strings.HasPrefix(string(parser.input), "***=") {
		parser.position = 4
		parser.flags.syntax = regexpQuote
		parser.flags.expanded = false
		return nil
	}
	if strings.HasPrefix(string(parser.input), "***:") {
		parser.position = 4
		parser.flags.syntax = regexpARE
	}
	if parser.flags.syntax != regexpARE || parser.position+2 >= len(parser.input) || parser.input[parser.position] != '(' || parser.input[parser.position+1] != '?' {
		return nil
	}
	switch parser.input[parser.position+2] {
	case ':', '=', '!', '<':
		return nil
	}
	end := parser.position + 2
	for end < len(parser.input) && parser.input[end] != ')' {
		end++
	}
	if end == len(parser.input) {
		return nil
	}
	for _, option := range parser.input[parser.position+2 : end] {
		switch option {
		case 'b':
			parser.flags.syntax = regexpBRE
		case 'c':
			parser.flags.ignoreCase = false
		case 'e':
			parser.flags.syntax = regexpERE
		case 'i':
			parser.flags.ignoreCase = true
		case 'm', 'n':
			parser.flags.newlineSensitive = true
			parser.flags.newlineAnchors = true
		case 'p':
			parser.flags.newlineSensitive = true
			parser.flags.newlineAnchors = false
		case 'q':
			parser.flags.syntax = regexpQuote
			parser.flags.expanded = false
		case 's':
			parser.flags.newlineSensitive = false
			parser.flags.newlineAnchors = false
		case 't':
			parser.flags.expanded = false
		case 'w':
			parser.flags.newlineSensitive = false
			parser.flags.newlineAnchors = true
		case 'x':
			parser.flags.expanded = true
		default:
			return regexpCompileError("invalid embedded option")
		}
	}
	parser.position = end + 1
	return nil
}

func (parser *regexpParser) addNode(node regexpNode) int {
	parser.nodes = append(parser.nodes, node)
	return len(parser.nodes) - 1
}

func (parser *regexpParser) combine(kind regexpNodeKind, children []int) int {
	if len(children) == 0 {
		return parser.addNode(regexpNode{kind: regexpEmpty})
	}
	if len(children) == 1 {
		return children[0]
	}
	preference := regexpNoPreference
	if kind == regexpAlternate {
		preference = regexpLongest
	} else {
		for _, child := range children {
			if parser.nodes[child].preference != regexpNoPreference {
				preference = parser.nodes[child].preference
				break
			}
		}
	}
	return parser.addNode(regexpNode{kind: kind, children: children, preference: preference})
}

func (parser *regexpParser) skipExpanded() {
	if !parser.flags.expanded {
		return
	}
	for parser.position < len(parser.input) {
		switch parser.input[parser.position] {
		case ' ', '\t', '\n', '\r', '\f':
			parser.position++
		case '#':
			for parser.position < len(parser.input) && parser.input[parser.position] != '\n' {
				parser.position++
			}
		default:
			return
		}
	}
}

func (parser *regexpParser) parseAlternation(group bool) (int, error) {
	parser.depth++
	defer func() { parser.depth-- }()
	if parser.depth > 256 {
		return 0, regexpCompileError("regular expression is too complex")
	}
	branches := make([]int, 0, 2)
	for {
		branch, err := parser.parseConcatenation(group)
		if err != nil {
			return 0, err
		}
		branches = append(branches, branch)
		parser.skipExpanded()
		if parser.flags.syntax == regexpBRE || parser.position >= len(parser.input) || parser.input[parser.position] != '|' {
			break
		}
		parser.position++
	}
	return parser.combine(regexpAlternate, branches), nil
}

func (parser *regexpParser) parseConcatenation(group bool) (int, error) {
	children := make([]int, 0, 4)
	branchStart := true
	for {
		parser.skipExpanded()
		if parser.position >= len(parser.input) {
			break
		}
		if parser.flags.syntax != regexpBRE && (parser.input[parser.position] == ')' || parser.input[parser.position] == '|') {
			break
		}
		if parser.flags.syntax == regexpBRE && group && parser.position+1 < len(parser.input) && parser.input[parser.position] == '\\' && parser.input[parser.position+1] == ')' {
			break
		}
		atom, err := parser.parseAtom(branchStart, group)
		if err != nil {
			return 0, err
		}
		atom, err = parser.parseQuantifier(atom)
		if err != nil {
			return 0, err
		}
		children = append(children, atom)
		branchStart = false
		if len(parser.nodes)*80 > int(parser.limit) {
			return 0, sqlError("54000", "regular expression exceeded memory budget")
		}
	}
	return parser.combine(regexpConcat, children), nil
}

func (parser *regexpParser) parseAtom(branchStart, group bool) (int, error) {
	if err := parser.ctx.Err(); err != nil {
		return 0, err
	}
	character := parser.input[parser.position]
	parser.position++
	if parser.flags.syntax == regexpBRE {
		switch character {
		case '^':
			if branchStart {
				return parser.addNode(regexpNode{kind: regexpBegin}), nil
			}
		case '$':
			if parser.position == len(parser.input) || (group && parser.position+1 < len(parser.input) && parser.input[parser.position] == '\\' && parser.input[parser.position+1] == ')') {
				return parser.addNode(regexpNode{kind: regexpEnd}), nil
			}
		case '.':
			return parser.addNode(regexpNode{kind: regexpDot}), nil
		case '[':
			return parser.parseClass()
		case '\\':
			return parser.parseBREBackslash()
		case '*':
			if !branchStart {
				return 0, regexpCompileError("quantifier operand invalid")
			}
		}
		return parser.addNode(regexpNode{kind: regexpLiteral, character: character}), nil
	}
	switch character {
	case '^':
		return parser.addNode(regexpNode{kind: regexpBegin}), nil
	case '$':
		return parser.addNode(regexpNode{kind: regexpEnd}), nil
	case '.':
		return parser.addNode(regexpNode{kind: regexpDot}), nil
	case '[':
		if parser.flags.syntax == regexpARE && parser.position+5 <= len(parser.input) {
			rest := string(parser.input[parser.position-1:])
			if strings.HasPrefix(rest, "[[:<:]]") {
				parser.position += 6
				return parser.addNode(regexpNode{kind: regexpWordBegin}), nil
			}
			if strings.HasPrefix(rest, "[[:>:]]") {
				parser.position += 6
				return parser.addNode(regexpNode{kind: regexpWordEnd}), nil
			}
		}
		return parser.parseClass()
	case '(':
		return parser.parseGroup()
	case '\\':
		return parser.parseAREEscape()
	case '*', '+', '?', '{':
		return 0, regexpCompileError("quantifier operand invalid")
	case ')':
		return 0, regexpCompileError("parentheses () not balanced")
	}
	return parser.addNode(regexpNode{kind: regexpLiteral, character: character}), nil
}

func (parser *regexpParser) parseGroup() (int, error) {
	kind := regexpCapture
	capturing := parser.inLookaround == 0
	if parser.flags.syntax == regexpARE && parser.position < len(parser.input) && parser.input[parser.position] == '?' {
		parser.position++
		capturing = false
		switch {
		case parser.take(':'):
			kind = regexpConcat
		case parser.take('='):
			kind = regexpLookAhead
			parser.inLookaround++
		case parser.take('!'):
			kind = regexpNegativeLookAhead
			parser.inLookaround++
		case parser.take('<') && parser.take('='):
			kind = regexpLookBehind
			parser.inLookaround++
		case parser.position > 0 && parser.input[parser.position-1] == '<' && parser.take('!'):
			kind = regexpNegativeLookBehind
			parser.inLookaround++
		default:
			return 0, regexpCompileError("invalid embedded option")
		}
	}
	group := 0
	if capturing {
		parser.groups++
		group = parser.groups
		parser.closedGroups = append(parser.closedGroups, false)
	}
	child, err := parser.parseAlternation(true)
	if kind == regexpLookAhead || kind == regexpNegativeLookAhead || kind == regexpLookBehind || kind == regexpNegativeLookBehind {
		parser.inLookaround--
	}
	if err != nil {
		return 0, err
	}
	parser.skipExpanded()
	if parser.position >= len(parser.input) || parser.input[parser.position] != ')' {
		return 0, regexpCompileError("parentheses () not balanced")
	}
	parser.position++
	if capturing {
		parser.closedGroups[group] = true
		return parser.addNode(regexpNode{kind: regexpCapture, children: []int{child}, group: group, preference: parser.nodes[child].preference}), nil
	}
	if kind == regexpConcat {
		return child, nil
	}
	return parser.addNode(regexpNode{kind: kind, children: []int{child}, preference: parser.nodes[child].preference}), nil
}

func (parser *regexpParser) take(character rune) bool {
	if parser.position < len(parser.input) && parser.input[parser.position] == character {
		parser.position++
		return true
	}
	return false
}

func (parser *regexpParser) parseBREBackslash() (int, error) {
	if parser.position >= len(parser.input) {
		return 0, regexpCompileError("invalid escape \\ sequence")
	}
	character := parser.input[parser.position]
	parser.position++
	switch character {
	case '(':
		parser.groups++
		group := parser.groups
		parser.closedGroups = append(parser.closedGroups, false)
		child, err := parser.parseAlternation(true)
		if err != nil {
			return 0, err
		}
		if parser.position+1 >= len(parser.input) || parser.input[parser.position] != '\\' || parser.input[parser.position+1] != ')' {
			return 0, regexpCompileError("parentheses () not balanced")
		}
		parser.position += 2
		parser.closedGroups[group] = true
		return parser.addNode(regexpNode{kind: regexpCapture, children: []int{child}, group: group, preference: parser.nodes[child].preference}), nil
	case '<':
		return parser.addNode(regexpNode{kind: regexpWordBegin}), nil
	case '>':
		return parser.addNode(regexpNode{kind: regexpWordEnd}), nil
	}
	if character >= '1' && character <= '9' {
		group := int(character - '0')
		if group >= len(parser.closedGroups) || !parser.closedGroups[group] {
			return 0, regexpCompileError("invalid backreference number")
		}
		return parser.addNode(regexpNode{kind: regexpBackref, group: group}), nil
	}
	return parser.addNode(regexpNode{kind: regexpLiteral, character: character}), nil
}

func (parser *regexpParser) parseAREEscape() (int, error) {
	if parser.position >= len(parser.input) {
		return 0, regexpCompileError("invalid escape \\ sequence")
	}
	character := parser.input[parser.position]
	parser.position++
	if parser.flags.syntax == regexpERE {
		return parser.addNode(regexpNode{kind: regexpLiteral, character: character}), nil
	}
	switch character {
	case 'A':
		return parser.addNode(regexpNode{kind: regexpStringBegin}), nil
	case 'm':
		return parser.addNode(regexpNode{kind: regexpWordBegin}), nil
	case 'M':
		return parser.addNode(regexpNode{kind: regexpWordEnd}), nil
	case 'y':
		return parser.addNode(regexpNode{kind: regexpWordBoundary}), nil
	case 'Y':
		return parser.addNode(regexpNode{kind: regexpNotWordBoundary}), nil
	case 'Z':
		return parser.addNode(regexpNode{kind: regexpStringEnd}), nil
	case 'd', 'D', 's', 'S', 'w', 'W':
		term, _ := regexpClassEscape(character)
		return parser.addNode(regexpNode{kind: regexpClassNode, class: regexpClass{terms: []regexpClassTerm{term}}}), nil
	case 'a':
		character = '\a'
	case 'b':
		character = '\b'
	case 'B':
		character = '\\'
	case 'e':
		character = 0x1b
	case 'f':
		character = '\f'
	case 'n':
		character = '\n'
	case 'r':
		character = '\r'
	case 't':
		character = '\t'
	case 'v':
		character = '\v'
	case 'c':
		if parser.position >= len(parser.input) || parser.input[parser.position] > 0x7f {
			return 0, regexpCompileError("invalid escape \\ sequence")
		}
		character = parser.input[parser.position] & 0x1f
		parser.position++
	case 'u':
		value, err := parser.parseHex(4, 4)
		if err != nil {
			return 0, err
		}
		character = value
	case 'U':
		value, err := parser.parseHex(8, 8)
		if err != nil {
			return 0, err
		}
		character = value
	case 'x':
		value, err := parser.parseHex(1, -1)
		if err != nil {
			return 0, err
		}
		character = value
	case '0':
		character = parser.parseOctal(0, 2)
	default:
		if character >= '1' && character <= '9' {
			start := parser.position - 1
			for parser.position < len(parser.input) && parser.input[parser.position] >= '0' && parser.input[parser.position] <= '9' {
				parser.position++
			}
			digits := parser.input[start:parser.position]
			group64, _ := strconv.ParseInt(string(digits), 10, 32)
			group := int(group64)
			if group < len(parser.closedGroups) && parser.closedGroups[group] {
				if parser.inLookaround != 0 {
					return 0, regexpCompileError("invalid backreference in look-around constraint")
				}
				return parser.addNode(regexpNode{kind: regexpBackref, group: group}), nil
			}
			if len(digits) == 1 {
				return 0, regexpCompileError("invalid backreference number")
			}
			parser.position = start
			character = parser.parseOctal(0, 3)
		} else if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') {
			return 0, regexpCompileError("invalid escape \\ sequence")
		}
	}
	if character == 0 || !utf8.ValidRune(character) {
		return 0, regexpCompileError("invalid escape \\ sequence")
	}
	return parser.addNode(regexpNode{kind: regexpLiteral, character: character}), nil
}

func (parser *regexpParser) parseHex(minimum, maximum int) (rune, error) {
	start := parser.position
	value := uint64(0)
	overflow := false
	for parser.position < len(parser.input) && (maximum < 0 || parser.position-start < maximum) {
		digit, ok := hexadecimalDigit(parser.input[parser.position])
		if !ok {
			break
		}
		if value > utf8.MaxRune/16 {
			overflow = true
		} else {
			value = value*16 + uint64(digit)
			if value > utf8.MaxRune {
				overflow = true
			}
		}
		parser.position++
	}
	if parser.position-start < minimum || overflow || value == 0 || value >= 0xd800 && value <= 0xdfff {
		return 0, regexpCompileError("invalid escape \\\\ sequence")
	}
	return rune(value), nil
}

func hexadecimalDigit(character rune) (int, bool) {
	switch {
	case character >= '0' && character <= '9':
		return int(character - '0'), true
	case character >= 'a' && character <= 'f':
		return int(character-'a') + 10, true
	case character >= 'A' && character <= 'F':
		return int(character-'A') + 10, true
	default:
		return 0, false
	}
}

func (parser *regexpParser) parseOctal(initial, maximum int) rune {
	value := initial
	count := 0
	for parser.position < len(parser.input) && count < maximum && parser.input[parser.position] >= '0' && parser.input[parser.position] <= '7' {
		value = value*8 + int(parser.input[parser.position]-'0')
		parser.position++
		count++
	}
	return rune(value)
}

func (parser *regexpParser) parseQuantifier(atom int) (int, error) {
	parser.skipExpanded()
	if parser.position >= len(parser.input) {
		return atom, nil
	}
	minimum, maximum := -1, -1
	if parser.flags.syntax == regexpBRE {
		if parser.input[parser.position] == '*' {
			minimum, maximum = 0, -1
			parser.position++
		} else if parser.position+1 < len(parser.input) && parser.input[parser.position] == '\\' && parser.input[parser.position+1] == '{' {
			parser.position += 2
			var err error
			minimum, maximum, err = parser.parseBounds('}', true)
			if err != nil {
				return 0, err
			}
		} else {
			return atom, nil
		}
	} else {
		switch parser.input[parser.position] {
		case '*':
			minimum, maximum = 0, -1
			parser.position++
		case '+':
			minimum, maximum = 1, -1
			parser.position++
		case '?':
			minimum, maximum = 0, 1
			parser.position++
		case '{':
			parser.position++
			var err error
			minimum, maximum, err = parser.parseBounds('}', false)
			if err != nil {
				return 0, err
			}
		default:
			return atom, nil
		}
	}
	switch parser.nodes[atom].kind {
	case regexpBegin, regexpEnd, regexpStringBegin, regexpStringEnd, regexpWordBegin, regexpWordEnd, regexpWordBoundary, regexpNotWordBoundary, regexpLookAhead, regexpNegativeLookAhead, regexpLookBehind, regexpNegativeLookBehind:
		return 0, regexpCompileError("quantifier operand invalid")
	}
	preference := regexpLongest
	if parser.flags.syntax == regexpARE && parser.position < len(parser.input) && parser.input[parser.position] == '?' {
		preference = regexpShortest
		parser.position++
	}
	parser.skipExpanded()
	if parser.position < len(parser.input) {
		next := parser.input[parser.position]
		if next == '*' || parser.flags.syntax != regexpBRE && (next == '+' || next == '?' || next == '{') {
			return 0, regexpCompileError("quantifier operand invalid")
		}
	}
	return parser.addNode(regexpNode{kind: regexpRepeat, children: []int{atom}, minimum: minimum, maximum: maximum, preference: preference}), nil
}

func (parser *regexpParser) parseBounds(close rune, escapedClose bool) (int, int, error) {
	start := parser.position
	for parser.position < len(parser.input) && parser.input[parser.position] >= '0' && parser.input[parser.position] <= '9' {
		parser.position++
	}
	if parser.position == start {
		return 0, 0, regexpCompileError("invalid repetition count(s)")
	}
	minimum64, _ := strconv.ParseInt(string(parser.input[start:parser.position]), 10, 32)
	minimum := int(minimum64)
	maximum := minimum
	if parser.position < len(parser.input) && parser.input[parser.position] == ',' {
		parser.position++
		start = parser.position
		for parser.position < len(parser.input) && parser.input[parser.position] >= '0' && parser.input[parser.position] <= '9' {
			parser.position++
		}
		if parser.position == start {
			maximum = -1
		} else {
			maximum64, _ := strconv.ParseInt(string(parser.input[start:parser.position]), 10, 32)
			maximum = int(maximum64)
		}
	}
	closed := false
	if escapedClose {
		closed = parser.position+1 < len(parser.input) && parser.input[parser.position] == '\\' && parser.input[parser.position+1] == close
		if closed {
			parser.position += 2
		}
	} else if parser.position < len(parser.input) && parser.input[parser.position] == close {
		closed = true
		parser.position++
	}
	if !closed {
		return 0, 0, regexpCompileError("braces {} not balanced")
	}
	if minimum > 255 || maximum > 255 || maximum >= 0 && maximum < minimum {
		return 0, 0, regexpCompileError("invalid repetition count(s)")
	}
	return minimum, maximum, nil
}

func (parser *regexpParser) parseClass() (int, error) {
	class := regexpClass{}
	if parser.position < len(parser.input) && parser.input[parser.position] == '^' {
		class.negated = true
		parser.position++
	}
	first := true
	for parser.position < len(parser.input) {
		if parser.input[parser.position] == ']' && !first {
			parser.position++
			return parser.addNode(regexpNode{kind: regexpClassNode, class: class}), nil
		}
		left, literal, err := parser.parseClassTerm(first)
		if err != nil {
			return 0, err
		}
		first = false
		if literal && parser.position < len(parser.input) && parser.input[parser.position] == '-' && parser.position+1 < len(parser.input) && parser.input[parser.position+1] != ']' {
			parser.position++
			right, rightLiteral, err := parser.parseClassTerm(false)
			if err != nil {
				return 0, err
			}
			if !rightLiteral || left.value > right.value {
				return 0, regexpCompileError("invalid character range")
			}
			left = regexpClassTerm{kind: regexpClassRange, rangeVal: regexpRuneRange{first: left.value, last: right.value}}
		}
		class.terms = append(class.terms, left)
	}
	return 0, regexpCompileError("brackets [] not balanced")
}

func (parser *regexpParser) parseClassTerm(first bool) (regexpClassTerm, bool, error) {
	if parser.position >= len(parser.input) {
		return regexpClassTerm{}, false, regexpCompileError("brackets [] not balanced")
	}
	character := parser.input[parser.position]
	parser.position++
	if character == ']' && first {
		return regexpClassTerm{kind: regexpClassLiteral, value: character}, true, nil
	}
	if character == '[' && parser.position < len(parser.input) {
		marker := parser.input[parser.position]
		if marker == ':' || marker == '.' || marker == '=' {
			parser.position++
			start := parser.position
			for parser.position+1 < len(parser.input) && !(parser.input[parser.position] == marker && parser.input[parser.position+1] == ']') {
				parser.position++
			}
			if parser.position+1 >= len(parser.input) {
				return regexpClassTerm{}, false, regexpCompileError("brackets [] not balanced")
			}
			value := parser.input[start:parser.position]
			parser.position += 2
			if marker == ':' {
				term, ok := regexpNamedClass(string(value))
				if !ok {
					return regexpClassTerm{}, false, regexpCompileError("invalid character class")
				}
				return term, false, nil
			}
			if len(value) != 1 {
				return regexpClassTerm{}, false, regexpCompileError("invalid collating element")
			}
			return regexpClassTerm{kind: regexpClassLiteral, value: value[0]}, true, nil
		}
	}
	if character == '\\' && parser.flags.syntax == regexpARE {
		if parser.position >= len(parser.input) {
			return regexpClassTerm{}, false, regexpCompileError("invalid escape \\ sequence")
		}
		escape := parser.input[parser.position]
		parser.position++
		if term, ok := regexpClassEscape(escape); ok {
			return term, false, nil
		}
		parser.position--
		node, err := parser.parseAREEscape()
		if err != nil {
			return regexpClassTerm{}, false, err
		}
		parsed := parser.nodes[node]
		if parsed.kind != regexpLiteral {
			return regexpClassTerm{}, false, regexpCompileError("invalid escape \\ sequence")
		}
		return regexpClassTerm{kind: regexpClassLiteral, value: parsed.character}, true, nil
	}
	return regexpClassTerm{kind: regexpClassLiteral, value: character}, true, nil
}

func regexpClassEscape(character rune) (regexpClassTerm, bool) {
	term := regexpClassTerm{}
	switch character {
	case 'd', 'D':
		term.kind = regexpClassDigit
	case 's', 'S':
		term.kind = regexpClassSpace
	case 'w', 'W':
		term.kind = regexpClassWord
	default:
		return regexpClassTerm{}, false
	}
	term.negated = character == 'D' || character == 'S' || character == 'W'
	return term, true
}

func regexpNamedClass(name string) (regexpClassTerm, bool) {
	kind := regexpClassLiteral
	switch name {
	case "alnum":
		kind = regexpClassAlnum
	case "alpha":
		kind = regexpClassAlpha
	case "ascii":
		kind = regexpClassASCII
	case "blank":
		kind = regexpClassBlank
	case "cntrl":
		kind = regexpClassControl
	case "digit":
		kind = regexpClassDigit
	case "graph":
		kind = regexpClassGraph
	case "lower":
		kind = regexpClassLower
	case "print":
		kind = regexpClassPrint
	case "punct":
		kind = regexpClassPunct
	case "space":
		kind = regexpClassSpace
	case "upper":
		kind = regexpClassUpper
	case "word":
		kind = regexpClassWord
	case "xdigit":
		kind = regexpClassXDigit
	default:
		return regexpClassTerm{}, false
	}
	return regexpClassTerm{kind: kind}, true
}

func regexpCompileError(message string) error {
	return sqlError("2201B", "invalid regular expression: "+message)
}

func (program regexpProgram) find(runtime *regexpRuntime, text []rune, start int) (regexpMatch, bool, error) {
	if start < 0 {
		start = 0
	}
	for position := start; position <= len(text); position++ {
		if err := runtime.tick(); err != nil {
			return regexpMatch{}, false, err
		}
		spans := make([]regexpSpan, program.groups+1)
		states, err := program.matchNode(runtime, program.root, text, regexpState{position: position, spans: spans})
		if err != nil {
			return regexpMatch{}, false, err
		}
		if len(states) == 0 {
			continue
		}
		best := states[0]
		for _, candidate := range states[1:] {
			if program.better(candidate, best, position) {
				best = candidate
			}
		}
		best.spans[0] = regexpSpan{start: position, end: best.position, valid: true}
		return regexpMatch{spans: best.spans}, true, nil
	}
	return regexpMatch{}, false, nil
}

func (program regexpProgram) better(candidate, current regexpState, start int) bool {
	preference := program.nodes[program.root].preference
	candidateLength, currentLength := candidate.position-start, current.position-start
	if candidateLength != currentLength {
		if preference == regexpShortest {
			return candidateLength < currentLength
		}
		return candidateLength > currentLength
	}
	for group := 1; group <= program.groups; group++ {
		a, b := candidate.spans[group], current.spans[group]
		if a.valid != b.valid {
			return a.valid
		}
		if !a.valid || a.start == b.start && a.end == b.end {
			continue
		}
		if a.start != b.start {
			return a.start < b.start
		}
		if program.preferences[group] == regexpShortest {
			return a.end < b.end
		}
		return a.end > b.end
	}
	return false
}

func (program regexpProgram) matchNode(runtime *regexpRuntime, index int, text []rune, state regexpState) ([]regexpState, error) {
	if err := runtime.tick(); err != nil {
		return nil, err
	}
	node := program.nodes[index]
	switch node.kind {
	case regexpEmpty:
		return []regexpState{state}, nil
	case regexpLiteral:
		if state.position < len(text) && regexpRuneEqual(text[state.position], node.character, program.flags.ignoreCase) {
			state.position++
			return []regexpState{state}, nil
		}
	case regexpDot:
		if state.position < len(text) && (!program.flags.newlineSensitive || text[state.position] != '\n') {
			state.position++
			return []regexpState{state}, nil
		}
	case regexpClassNode:
		if state.position < len(text) && node.class.matches(text[state.position], program.flags) {
			state.position++
			return []regexpState{state}, nil
		}
	case regexpConcat:
		states := []regexpState{state}
		for _, child := range node.children {
			next := make([]regexpState, 0, len(states))
			for _, current := range states {
				matched, err := program.matchNode(runtime, child, text, current)
				if err != nil {
					return nil, err
				}
				for _, result := range matched {
					next, err = runtime.appendState(next, result)
					if err != nil {
						return nil, err
					}
				}
			}
			states = next
			if len(states) == 0 {
				break
			}
		}
		return states, nil
	case regexpAlternate:
		var states []regexpState
		for _, child := range node.children {
			matched, err := program.matchNode(runtime, child, text, state)
			if err != nil {
				return nil, err
			}
			for _, result := range matched {
				states, err = runtime.appendState(states, result)
				if err != nil {
					return nil, err
				}
			}
		}
		return states, nil
	case regexpRepeat:
		return program.matchRepeat(runtime, node, text, state)
	case regexpCapture:
		matched, err := program.matchNode(runtime, node.children[0], text, state)
		if err != nil {
			return nil, err
		}
		for at := range matched {
			spans := append([]regexpSpan(nil), matched[at].spans...)
			spans[node.group] = regexpSpan{start: state.position, end: matched[at].position, valid: true}
			matched[at].spans = spans
		}
		return matched, nil
	case regexpBackref:
		span := state.spans[node.group]
		if !span.valid || span.end-span.start > len(text)-state.position {
			return nil, nil
		}
		for offset := 0; offset < span.end-span.start; offset++ {
			if !regexpRuneEqual(text[span.start+offset], text[state.position+offset], program.flags.ignoreCase) {
				return nil, nil
			}
		}
		state.position += span.end - span.start
		return []regexpState{state}, nil
	case regexpBegin:
		if state.position == 0 || program.flags.newlineAnchors && state.position > 0 && text[state.position-1] == '\n' {
			return []regexpState{state}, nil
		}
	case regexpEnd:
		if state.position == len(text) || program.flags.newlineAnchors && state.position < len(text) && text[state.position] == '\n' {
			return []regexpState{state}, nil
		}
	case regexpStringBegin:
		if state.position == 0 {
			return []regexpState{state}, nil
		}
	case regexpStringEnd:
		if state.position == len(text) {
			return []regexpState{state}, nil
		}
	case regexpWordBegin:
		if (state.position == 0 || !regexpWord(text[state.position-1])) && state.position < len(text) && regexpWord(text[state.position]) {
			return []regexpState{state}, nil
		}
	case regexpWordEnd:
		if state.position > 0 && regexpWord(text[state.position-1]) && (state.position == len(text) || !regexpWord(text[state.position])) {
			return []regexpState{state}, nil
		}
	case regexpWordBoundary, regexpNotWordBoundary:
		left := state.position > 0 && regexpWord(text[state.position-1])
		right := state.position < len(text) && regexpWord(text[state.position])
		boundary := left != right
		if boundary == (node.kind == regexpWordBoundary) {
			return []regexpState{state}, nil
		}
	case regexpLookAhead, regexpNegativeLookAhead:
		matched, err := program.matchNode(runtime, node.children[0], text, state)
		if err != nil {
			return nil, err
		}
		success := len(matched) != 0
		if success == (node.kind == regexpLookAhead) {
			return []regexpState{state}, nil
		}
	case regexpLookBehind, regexpNegativeLookBehind:
		success := false
		for begin := 0; begin <= state.position && !success; begin++ {
			candidate := regexpState{position: begin, spans: state.spans}
			matched, err := program.matchNode(runtime, node.children[0], text, candidate)
			if err != nil {
				return nil, err
			}
			for _, result := range matched {
				if result.position == state.position {
					success = true
					break
				}
			}
		}
		if success == (node.kind == regexpLookBehind) {
			return []regexpState{state}, nil
		}
	}
	return nil, nil
}

func (program regexpProgram) matchRepeat(runtime *regexpRuntime, node regexpNode, text []rune, state regexpState) ([]regexpState, error) {
	frontier := []regexpState{state}
	var results []regexpState
	maximum := node.maximum
	if maximum < 0 {
		maximum = len(text) - state.position + 1
	}
	for count := 0; count <= maximum && len(frontier) != 0; count++ {
		if count >= node.minimum {
			for _, candidate := range frontier {
				var err error
				results, err = runtime.appendState(results, candidate)
				if err != nil {
					return nil, err
				}
			}
		}
		if count == maximum {
			break
		}
		next := make([]regexpState, 0, len(frontier))
		for _, candidate := range frontier {
			matched, err := program.matchNode(runtime, node.children[0], text, candidate)
			if err != nil {
				return nil, err
			}
			for _, result := range matched {
				if result.position == candidate.position {
					if count+1 >= node.minimum {
						results, err = runtime.appendState(results, result)
						if err != nil {
							return nil, err
						}
					}
					continue
				}
				next, err = runtime.appendState(next, result)
				if err != nil {
					return nil, err
				}
			}
		}
		frontier = next
	}
	return results, nil
}

func (class regexpClass) matches(character rune, flags regexpFlags) bool {
	matched := false
	for _, term := range class.terms {
		termMatch := term.matches(character, flags.ignoreCase)
		if term.negated {
			termMatch = !termMatch
		}
		matched = matched || termMatch
	}
	if class.negated {
		matched = !matched
		if flags.newlineSensitive && character == '\n' {
			return false
		}
	}
	return matched
}

func (term regexpClassTerm) matches(character rune, ignoreCase bool) bool {
	test := character
	if ignoreCase {
		test = regexpFold(test)
	}
	switch term.kind {
	case regexpClassLiteral:
		return regexpRuneEqual(test, term.value, ignoreCase)
	case regexpClassRange:
		first, last := term.rangeVal.first, term.rangeVal.last
		if ignoreCase {
			first, last = regexpFold(first), regexpFold(last)
		}
		return test >= first && test <= last
	case regexpClassAlnum:
		return regexpAlpha(character) || regexpDigit(character)
	case regexpClassAlpha:
		return regexpAlpha(character)
	case regexpClassASCII:
		return character >= 0 && character <= 0x7f
	case regexpClassBlank:
		return character == ' ' || character == '\t'
	case regexpClassControl:
		return character >= 0 && character < 0x20 || character == 0x7f
	case regexpClassDigit:
		return regexpDigit(character)
	case regexpClassGraph:
		return character >= 0x21 && character <= 0x7e
	case regexpClassLower:
		return character >= 'a' && character <= 'z' || ignoreCase && character >= 'A' && character <= 'Z'
	case regexpClassPrint:
		return character >= 0x20 && character <= 0x7e
	case regexpClassPunct:
		return character >= 0x21 && character <= 0x7e && !regexpAlpha(character) && !regexpDigit(character)
	case regexpClassSpace:
		return character == ' ' || character >= '\t' && character <= '\r'
	case regexpClassUpper:
		return character >= 'A' && character <= 'Z' || ignoreCase && character >= 'a' && character <= 'z'
	case regexpClassWord:
		return regexpWord(character)
	case regexpClassXDigit:
		_, ok := hexadecimalDigit(character)
		return ok
	default:
		return false
	}
}

func regexpRuneEqual(left, right rune, ignoreCase bool) bool {
	if ignoreCase {
		left, right = regexpFold(left), regexpFold(right)
	}
	return left == right
}

func regexpFold(character rune) rune {
	if character >= 'A' && character <= 'Z' {
		return character + ('a' - 'A')
	}
	return character
}

func regexpAlpha(character rune) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
}

func regexpDigit(character rune) bool { return character >= '0' && character <= '9' }

func regexpWord(character rune) bool { return regexpAlpha(character) || regexpDigit(character) || character == '_' }

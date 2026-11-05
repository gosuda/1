package builtin

import (
	"context"
	"sort"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// nativeThesaurusDictionary is PostgreSQL's thesaurus dictionary: phrase to
// phrase substitution driven by a pinned ".ths" file, where each sample word
// is normalized through a sub-dictionary. It is immutable after construction;
// per-match state lives in nativeThesaurusPhrase.
type nativeThesaurusDictionary struct {
	// subdict normalizes both the sample words at compile time and every word
	// offered during matching.
	subdict model.TextSearchDictionaryKernel

	// lexemes is the sorted array of compiled sample lexemes. NULL lexemes
	// represent the '?' stop-word marker.
	lexemes []thesaurusLexeme
	// substitutes holds, per rule, the pre-normalized replacement phrase.
	substitutes []thesaurusSubstitute
}

// thesaurusLexeme is one compiled sample word with its rule entries.
type thesaurusLexeme struct {
	// text is the normalized sample word; nil marks a '?' stop word.
	null bool
	text string
	// entries is the sorted linked list of rules naming this lexeme.
	entries *thesaurusEntry
}

// thesaurusSubstitute is one rule's replacement phrase.
type thesaurusSubstitute struct {
	// lastlexeme is the index of the rule's final sample word.
	lastlexeme uint16
	lexemes    []string
}

// thesaurusEntry locates one lexeme inside one rule.
type thesaurusEntry struct {
	idsubst    uint32
	posinsubst uint16
	tnvariant  uint16
	nextentry  *thesaurusEntry
	nextvariant *thesaurusEntry
}

// newNativeThesaurusDictionary compiles the pinned thesaurus file against the
// given sub-dictionary, reproducing thesaurus_init's two compile passes.
func newNativeThesaurusDictionary(ctx context.Context, name string, subdict model.TextSearchDictionaryKernel) (*nativeThesaurusDictionary, error) {
	data, ok := nativeTextSearchData[name+".ths"]
	if !ok {
		return nil, sqlError("F0000", "could not open thesaurus file \""+name+".ths\"")
	}
	if subdict == nil {
		return nil, sqlError("F0000", "thesaurus sub-dictionary is unavailable")
	}
	dictionary := &nativeThesaurusDictionary{subdict: subdict}
	rules, err := parseNativeThesaurus(ctx, data)
	if err != nil {
		return nil, err
	}
	if err := dictionary.compileLexemes(ctx, rules); err != nil {
		return nil, err
	}
	if err := dictionary.compileSubstitutes(ctx, rules); err != nil {
		return nil, err
	}
	return dictionary, nil
}

// thesaurusRule is one parsed "sample : substitute" line.
type thesaurusRule struct {
	samples    []thesaurusSample
	substitute []thesaurusSample
}

type thesaurusSample struct {
	text    string
	useAsIs bool
}

// parseNativeThesaurus ports thesaurusRead's state machine. Comments (leading
// '#'), blank lines, and a single-word line are skipped exactly as PostgreSQL
// does, and the same malformed-line diagnostics are preserved.
func parseNativeThesaurus(ctx context.Context, data []byte) ([]thesaurusRule, error) {
	rules := make([]thesaurusRule, 0, 8)
	for _, line := range splitNativeTextSearchLines(data) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		trimmed := strings.TrimLeft(line, " \t\r\v\f")
		if trimmed == "" || trimmed[0] == '#' {
			continue
		}
		rule, err := parseNativeThesaurusLine(trimmed)
		if err != nil {
			return nil, err
		}
		if len(rule.samples) == 0 || len(rule.substitute) == 0 {
			continue
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func parseNativeThesaurusLine(line string) (thesaurusRule, error) {
	var rule thesaurusRule
	separator := strings.IndexByte(line, ':')
	if separator < 0 {
		return thesaurusRule{}, sqlError("F0000", "unexpected end of line")
	}
	for _, word := range strings.Fields(line[:separator]) {
		rule.samples = append(rule.samples, thesaurusSample{text: word})
	}
	if len(rule.samples) == 0 {
		return thesaurusRule{}, sqlError("F0000", "unexpected delimiter")
	}
	for _, word := range strings.Fields(line[separator+1:]) {
		sample := thesaurusSample{text: word}
		if strings.HasPrefix(word, "*") {
			sample.useAsIs = true
			sample.text = strings.TrimPrefix(word, "*")
		} else if strings.HasPrefix(word, `\`) {
			sample.text = strings.TrimPrefix(word, `\`)
		}
		if sample.text == "" {
			return thesaurusRule{}, sqlError("F0000", "unexpected end of line or lexeme")
		}
		rule.substitute = append(rule.substitute, sample)
	}
	return rule, nil
}

// compileLexemes ports compileTheLexeme: every sample word is normalized by the
// sub-dictionary and cross-referenced, then the array is sorted and uniqued.
func (d *nativeThesaurusDictionary) compileLexemes(ctx context.Context, rules []thesaurusRule) error {
	compiled := make([]thesaurusLexeme, 0, 16)
	for ruleIndex := range rules {
		for sampleIndex, sample := range rules[ruleIndex].samples {
			if err := ctx.Err(); err != nil {
				return err
			}
			entry := &thesaurusEntry{idsubst: uint32(ruleIndex), posinsubst: uint16(sampleIndex)}
			if sample.text == "?" {
				compiled = append(compiled, thesaurusLexeme{null: true, entries: entry})
				continue
			}
			variants, err := lexizeTextSearchVariants(ctx, d.subdict, sample.text)
			if err != nil {
				return err
			}
			if variants == nil {
				return sqlError("F0000", "thesaurus sample word \""+sample.text+"\" isn't recognized by subdictionary (rule "+itoa(ruleIndex+1)+")")
			}
			if len(variants) == 0 {
				return sqlError("F0000", "thesaurus sample word \""+sample.text+"\" is a stop word (rule "+itoa(ruleIndex+1)+")")
			}
			for _, variant := range variants {
				for _, lexeme := range variant {
					compiled = append(compiled, thesaurusLexeme{
						text:    lexeme,
						entries: &thesaurusEntry{idsubst: entry.idsubst, posinsubst: entry.posinsubst, tnvariant: uint16(len(variant))},
					})
				}
			}
		}
	}
	if len(compiled) > 1 {
		sort.SliceStable(compiled, func(i, j int) bool {
			if left, right := compiled[i].null, compiled[j].null; left != right {
				return right
			}
			if compiled[i].text != compiled[j].text {
				return compiled[i].text < compiled[j].text
			}
			return compareThesaurusEntry(compiled[j].entries, compiled[i].entries) < 0
		})
		uniqued := compiled[:1]
		for index := 1; index < len(compiled); index++ {
			previous, current := &uniqued[len(uniqued)-1], &compiled[index]
			if previous.null != current.null || previous.text != current.text {
				uniqued = append(uniqued, *current)
				continue
			}
			if compareThesaurusEntry(current.entries, previous.entries) != 0 {
				current.entries.nextentry = previous.entries
				previous.entries = current.entries
			}
		}
		compiled = uniqued
	}
	d.lexemes = compiled
	return nil
}

// compileSubstitutes ports compileTheSubstitute: each replacement word is
// normalized unless the rule marked it with a leading '*'.
func (d *nativeThesaurusDictionary) compileSubstitutes(ctx context.Context, rules []thesaurusRule) error {
	d.substitutes = make([]thesaurusSubstitute, len(rules))
	for ruleIndex, rule := range rules {
		substitute := thesaurusSubstitute{lastlexeme: uint16(len(rule.samples) - 1)}
		for _, sample := range rule.substitute {
			if err := ctx.Err(); err != nil {
				return err
			}
			if sample.useAsIs {
				substitute.lexemes = append(substitute.lexemes, sample.text)
				continue
			}
			lexemes, err := d.subdict.Lexize(ctx, sample.text)
			if err != nil {
				return err
			}
			if lexemes == nil {
				return sqlError("F0000", "thesaurus substitute word \""+sample.text+"\" isn't recognized by subdictionary (rule "+itoa(ruleIndex+1)+")")
			}
			if len(lexemes) == 0 {
				return sqlError("F0000", "thesaurus substitute word \""+sample.text+"\" is a stop word (rule "+itoa(ruleIndex+1)+")")
			}
			substitute.lexemes = append(substitute.lexemes, lexemes...)
		}
		if len(substitute.lexemes) == 0 {
			return sqlError("F0000", "thesaurus substitute phrase is empty (rule "+itoa(ruleIndex+1)+")")
		}
		d.substitutes[ruleIndex] = substitute
	}
	return nil
}

// Match begins a phrase match. A nil result means the word cannot start any
// thesaurus entry.
func (d *nativeThesaurusDictionary) Match(ctx context.Context, word string) model.TextSearchPhrase {
	phrase := &nativeThesaurusPhrase{dictionary: d}
	phrase.step(ctx, word)
	if phrase.info == nil {
		return nil
	}
	return phrase
}

// Lexize reports what ts_lexize observes. PostgreSQL's driver calls lexize a
// second time with isend set when getnext is true, and keeps the first result
// when the second call yields NULL, so the substitution completed on the single
// word always stands.
func (d *nativeThesaurusDictionary) Lexize(ctx context.Context, token string) ([]string, error) {
	phrase := &nativeThesaurusPhrase{dictionary: d}
	if !phrase.step(ctx, token) {
		return nil, nil
	}
	return phrase.last, nil
}

// LexizeVariants reports the thesaurus substitution as one variant; the
// thesaurus never splits a single word into variants.
func (d *nativeThesaurusDictionary) LexizeVariants(ctx context.Context, token string) ([][]string, error) {
	lexemes, err := d.Lexize(ctx, token)
	if err != nil || lexemes == nil {
		return nil, err
	}
	return [][]string{lexemes}, nil
}

// nativeThesaurusPhrase carries the candidate set across one phrase match.
// stored mirrors DictSubState.private_state and info the running candidate set.
type nativeThesaurusPhrase struct {
	dictionary *nativeThesaurusDictionary
	stored     *thesaurusEntry
	info       *thesaurusEntry
	pos        uint16
	last       []string
	wantMore   bool
}

// Result returns the substitution completed on the most recent word.
func (p *nativeThesaurusPhrase) Result() []string { return p.last }

// Extend consumes the next word of the phrase, reporting whether the
// dictionary needs yet another word before it can decide.
func (p *nativeThesaurusPhrase) Extend(ctx context.Context, word string) bool {
	p.step(ctx, word)
	return p.wantMore
}

// step ports one thesaurus_lexize call. It reports whether a substitution is
// available and records in wantMore whether PostgreSQL would ask for another
// word.
func (p *nativeThesaurusPhrase) step(ctx context.Context, word string) bool {
	if p.wantMore {
		// A later call after checkMatch already reported more candidates: with
		// no further word it yields no new substitution, the driver keeps the
		// prior result, and matching stops.
		p.wantMore = false
		return p.last != nil
	}
	curpos := uint16(0)
	if p.stored != nil {
		curpos = p.stored.posinsubst + 1
	}
	p.pos = curpos + 1
	p.stored = p.info
	p.info = nil

	variants, err := lexizeTextSearchVariants(ctx, p.dictionary.subdict, word)
	if err != nil {
		p.wantMore = false
		return false
	}
	if variants == nil {
		p.wantMore = false
		return false
	}
	accumulator := (*thesaurusEntry)(nil)
	if len(variants) == 0 {
		accumulator = p.dictionary.findVariant(accumulator, p.stored, curpos, []*thesaurusEntry{p.dictionary.findLexeme(true, "")})
	} else {
		for _, variant := range variants {
			candidates := make([]*thesaurusEntry, 0, len(variant))
			for _, lexeme := range variant {
				entry := p.dictionary.findLexeme(false, lexeme)
				if entry == nil {
					candidates = nil
					break
				}
				candidates = append(candidates, entry)
			}
			if candidates == nil {
				continue
			}
			accumulator = p.dictionary.findVariant(accumulator, p.stored, curpos, candidates)
		}
	}
	p.info = accumulator
	if p.info == nil {
		p.wantMore = false
		return false
	}
	if result, more := p.dictionary.checkMatch(p.info, curpos); result != nil {
		p.last = result
		p.wantMore = more
		return true
	}
	// No substitution finishes here, so PostgreSQL asks for the next word.
	p.wantMore = true
	return p.last != nil
}

// findLexeme ports findTheLexeme, using the sorted lexeme array as the index.
func (d *nativeThesaurusDictionary) findLexeme(null bool, text string) *thesaurusEntry {
	index := sort.Search(len(d.lexemes), func(i int) bool {
		if left, right := d.lexemes[i].null, null; left != right {
			return right
		}
		return d.lexemes[i].text >= text
	})
	if index < len(d.lexemes) && d.lexemes[index].null == null && d.lexemes[index].text == text {
		return d.lexemes[index].entries
	}
	return nil
}

// findVariant ports findVariant: in accumulates the variants found so far,
// stored is the previous call's candidate set, and curpos is the zero-based
// position of the word being matched inside a rule.
func (d *nativeThesaurusDictionary) findVariant(in, stored *thesaurusEntry, curpos uint16, newin []*thesaurusEntry) *thesaurusEntry {
	for {
		ptr := newin[0]
		index := 0
		for ; index < len(newin); index++ {
			for newin[index] != nil && newin[index].idsubst < ptr.idsubst {
				newin[index] = newin[index].nextentry
			}
			if newin[index] == nil {
				return in
			}
			if newin[index].idsubst > ptr.idsubst {
				ptr = newin[index]
				index = -1
				continue
			}
			for newin[index].idsubst == ptr.idsubst {
				if newin[index].posinsubst == curpos && newin[index].tnvariant == uint16(len(newin)) {
					ptr = newin[index]
					break
				}
				newin[index] = newin[index].nextentry
				if newin[index] == nil {
					return in
				}
			}
			if newin[index].idsubst != ptr.idsubst {
				ptr = newin[index]
				index = -1
				continue
			}
		}
		if index == len(newin) && matchThesaurusID(stored, ptr.idsubst) && (in == nil || !matchThesaurusID(in, ptr.idsubst)) {
			ptr.nextvariant = in
			in = ptr
		}
		for i := range newin {
			newin[i] = newin[i].nextentry
		}
	}
}

// checkMatch ports checkMatch: the first candidate whose rule ends at curpos
// supplies the substitution, and more reports whether other candidates remain.
func (d *nativeThesaurusDictionary) checkMatch(info *thesaurusEntry, curpos uint16) ([]string, bool) {
	more := false
	for entry := info; entry != nil; entry = entry.nextvariant {
		if entry.nextvariant != nil {
			more = true
		}
		if entry.idsubst < uint32(len(d.substitutes)) && d.substitutes[entry.idsubst].lastlexeme == curpos {
			return d.substitutes[entry.idsubst].lexemes, more
		}
	}
	return nil, false
}

func matchThesaurusID(stored *thesaurusEntry, idsubst uint32) bool {
	if stored == nil {
		return true
	}
	for entry := stored; entry != nil; entry = entry.nextvariant {
		if entry.idsubst == idsubst {
			return true
		}
	}
	return false
}

func compareThesaurusEntry(a, b *thesaurusEntry) int {
	if a == nil || b == nil {
		return 0
	}
	if a.idsubst != b.idsubst {
		if a.idsubst > b.idsubst {
			return 1
		}
		return -1
	}
	if a.posinsubst != b.posinsubst {
		if a.posinsubst > b.posinsubst {
			return 1
		}
		return -1
	}
	if a.tnvariant != b.tnvariant {
		if a.tnvariant > b.tnvariant {
			return 1
		}
		return -1
	}
	return 0
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}

var _ model.TextSearchDictionaryKernel = (*nativeThesaurusDictionary)(nil)
var _ model.TextSearchPhraseKernel = (*nativeThesaurusDictionary)(nil)
var _ model.TextSearchPhrase = (*nativeThesaurusPhrase)(nil)

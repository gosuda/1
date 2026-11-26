package builtin

import (
	"context"
	"sort"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// nativeIspellDictionary is PostgreSQL's Ispell/Hunspell dictionary. It stores
// a base-form word list with affix-flag sets and a compiled affix list, and
// reproduces affix stripping, cross-product prefix/suffix composition, and
// compound splitting from src/backend/tsearch/spell.c.
//
// PostgreSQL stores the word list and affix rules in prefix trees purely as a
// lookup optimization over sorted parallel arrays. This implementation keeps
// the same observable behavior with binary search over sorted slices: the
// semantics come from NormalizeSubWord/FindWord/CheckAffix, not from the tree
// shape. Affix matching scans outward from the word's start (prefix) or end
// (suffix), which is equivalent to PostgreSQL's GETWCHAR direction.
type nativeIspellDictionary struct {
	words   []ispellWord
	affixes []ispellAffix
	// prefixes and suffixes hold indices into affixes, so affix matching needs
	// no per-token allocation.
	prefixes []int
	suffixes []int
	// compoundFlags maps each declared compound flag to the FF_* bits its
	// command granted. COMPOUNDFLAG grants BEGIN|MIDDLE|LAST while
	// COMPOUNDBEGIN/MIDDLE/END each grant one position.
	compoundFlags map[string]uint8
	flagMode      ispellFlagMode
	useCompound   bool
	// aliases is the AF flag-alias table. Index 0 is the empty flag set, so
	// useAliases (not len(aliases)) decides whether alias indirection applies.
	aliases    []string
	useAliases bool
}

// ispellWord is one base form with its raw (undecoded) affix-flag set.
type ispellWord struct {
	word  string
	flags string
}

type ispellFlagMode uint8

const (
	ispellFlagChar ispellFlagMode = iota
	ispellFlagLong
	ispellFlagNum
)

// Affix position bits, matching spell.h's FF_* constants exactly. Note that
// FF_COMPOUNDFLAG is the MASK of BEGIN|MIDDLE|LAST rather than a separate bit,
// so declaring a word as a compound flag grants all three positions.
const (
	ispellCompoundOnly     uint8 = 0x01
	ispellCompoundBegin    uint8 = 0x02
	ispellCompoundMiddle   uint8 = 0x04
	ispellCompoundLast     uint8 = 0x08
	ispellCompoundFlag     uint8 = ispellCompoundBegin | ispellCompoundMiddle | ispellCompoundLast
	ispellCompoundPermit   uint8 = 0x10
	ispellCompoundForbid   uint8 = 0x20
	ispellCrossProduct     uint8 = 0x40
	ispellCompoundFlagMask uint8 = 0x0f
)

type ispellAffix struct {
	flag      string
	flagflags uint8
	find      string
	repl      string
	suffix    bool
	// replFlags is the '/'-suffixed flag set on a Hunspell replacement; the
	// produced word must satisfy it as well.
	replFlags  string
	condSimple bool
	cond       []ispellCondChar
}

// ispellCondChar is one position of an affix condition. PostgreSQL compiles a
// condition to either the fast Regis subset or a full regex; the pinned affix
// files use the Regis subset plus Hunspell's `*` repetition, so the condition is
// represented as a sequence of positions and evaluated directly.
type ispellCondChar struct {
	none  bool
	chars string
	// repeat marks a `x*` position, which matches zero or more occurrences.
	repeat bool
}

// newNativeIspellDictionary compiles the pinned .dict and .affix files.
func newNativeIspellDictionary(ctx context.Context, dictionaryFile, affixFile string) (*nativeIspellDictionary, error) {
	dict, ok := nativeTextSearchData[dictionaryFile+".dict"]
	if !ok {
		return nil, sqlError("F0000", "could not open dictionary file \""+dictionaryFile+".dict\"")
	}
	affixData, ok := nativeTextSearchData[affixFile+".affix"]
	if !ok {
		return nil, sqlError("F0000", "could not open affix file \""+affixFile+".affix\"")
	}
	dictionary := &nativeIspellDictionary{flagMode: ispellFlagChar, compoundFlags: make(map[string]uint8, 8)}
	if err := dictionary.importDictionary(ctx, dict); err != nil {
		return nil, err
	}
	if err := dictionary.importAffixes(ctx, affixData); err != nil {
		return nil, err
	}
	if err := dictionary.validateWordFlags(); err != nil {
		return nil, err
	}
	sort.SliceStable(dictionary.words, func(i, j int) bool { return dictionary.words[i].word < dictionary.words[j].word })
	if err := dictionary.sortAffixes(ctx); err != nil {
		return nil, err
	}
	return dictionary, nil
}

// validateWordFlags reproduces NISortDictionary's flag validation. With an AF
// alias table each word's whole flag field must be a decimal alias index;
// otherwise every flag must be decodable in the affix file's FLAG mode. Both are
// load-time errors in PostgreSQL, so a mismatched .dict/.affix pair is rejected
// rather than silently answered with wrong lexemes.
func (d *nativeIspellDictionary) validateWordFlags() error {
	for _, entry := range d.words {
		if entry.flags == "" {
			continue
		}
		if d.useAliases {
			index := 0
			for i := 0; i < len(entry.flags); i++ {
				ch := entry.flags[i]
				if ch < '0' || ch > '9' {
					return sqlError("F0000", "invalid affix alias \""+entry.flags+"\"")
				}
				index = index*10 + int(ch-'0')
			}
			if index >= len(d.aliases) {
				return sqlError("F0000", "invalid affix alias \""+entry.flags+"\"")
			}
			continue
		}
		switch d.flagMode {
		case ispellFlagNum:
			// FM_NUM flags are decimal, optionally comma separated.
			for _, flag := range splitFlagsByMode(entry.flags, d.flagMode) {
				if !isDecimal(flag) {
					return sqlError("F0000", "invalid affix flag \""+entry.flags+"\"")
				}
			}
		case ispellFlagLong:
			if len(entry.flags)%2 != 0 {
				return sqlError("F0000", "invalid affix flag \""+entry.flags+"\" with \"long\" flag value")
			}
		}
	}
	return nil
}

// isDecimal reports whether text is a non-empty run of decimal digits.
func isDecimal(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

// importDictionary mirrors NIImportDictionary: the word is the text before the
// first '/', the flag set is the single-byte printable run after it, and the
// word is truncated at the first space and lower-cased.
func (d *nativeIspellDictionary) importDictionary(ctx context.Context, data []byte) error {
	for _, line := range splitNativeTextSearchLines(data) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if line == "" {
			continue
		}
		word, flags := line, ""
		if slash := strings.IndexByte(line, '/'); slash >= 0 {
			word = line[:slash]
			// Only single-byte printable non-space characters are flags; the
			// first other byte terminates the flag set.
			flags = line[slash+1:]
			for i := 0; i < len(flags); i++ {
				if flags[i] < 0x20 || flags[i] == 0x7f || flags[i] >= 0x80 {
					flags = flags[:i]
					break
				}
			}
		}
		if space := strings.IndexAny(word, " \t\r\f\v"); space >= 0 {
			word = word[:space]
		}
		if word == "" {
			continue
		}
		d.words = append(d.words, ispellWord{word: strings.ToLower(word), flags: flags})
	}
	return nil
}

// importAffixes detects the old Ispell format and delegates to the Hunspell
// reader, exactly as NIImportAffixes does.
func (d *nativeIspellDictionary) importAffixes(ctx context.Context, data []byte) error {
	lines := splitNativeTextSearchLines(data)
	oldFormat, newFormat := false, false
	for _, line := range lines {
		if err := ctx.Err(); err != nil {
			return err
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		lower := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(lower, "compoundwords"):
			oldFormat = true
			continue
		case strings.HasPrefix(lower, "suffixes"), strings.HasPrefix(lower, "prefixes"):
			oldFormat = true
			continue
		case strings.HasPrefix(lower, "flag"):
			// A `flag` line with a single-flag payload is old format; otherwise
			// it is a Hunspell command line.
			if isOldAffixFlagCommand(trimmed) {
				oldFormat = true
				continue
			}
			newFormat = true
			continue
		case strings.HasPrefix(trimmed, "COMPOUNDFLAG"), strings.HasPrefix(trimmed, "COMPOUNDMIN"),
			strings.HasPrefix(trimmed, "COMPOUNDBEGIN"), strings.HasPrefix(trimmed, "COMPOUNDMIDDLE"),
			strings.HasPrefix(trimmed, "COMPOUNDEND"), strings.HasPrefix(trimmed, "COMPOUNDPERMITFLAG"),
			strings.HasPrefix(trimmed, "COMPOUNDFORBIDFLAG"), strings.HasPrefix(trimmed, "ONLYINCOMPOUND"),
			strings.HasPrefix(trimmed, "PFX"), strings.HasPrefix(trimmed, "SFX"), strings.HasPrefix(trimmed, "AF "):
			newFormat = true
			continue
		}
	}
	if oldFormat && newFormat {
		return sqlError("F0000", "affix file contains both old-style and new-style commands")
	}
	if newFormat {
		return d.importHunspellAffixes(ctx, lines)
	}
	return d.importIspellAffixes(ctx, lines)
}

// isOldAffixFlagCommand reports whether a `flag` line is the old-format
// single-flag declaration. NIImportAffixes only treats it as old format when
// the payload is one character followed by end of line, '#', ':', or space.
func isOldAffixFlagCommand(line string) bool {
	rest := strings.TrimSpace(line[4:])
	rest = strings.TrimPrefix(rest, "*")
	rest = strings.TrimPrefix(rest, "~")
	rest = strings.TrimPrefix(rest, "\\")
	if rest == "" {
		return false
	}
	// A multi-byte first character means this is a Hunspell command line.
	if rest[0] >= 0x80 {
		return false
	}
	if len(rest) == 1 {
		return true
	}
	next := rest[1]
	return next == ' ' || next == '\t' || next == '#' || next == ':'
}

// importIspellAffixes reads the old `prefixes`/`suffixes` sections.
func (d *nativeIspellDictionary) importIspellAffixes(ctx context.Context, lines []string) error {
	suffixes, prefixes := false, false
	flag, flagflags := "", uint8(0)
	for _, line := range lines {
		if err := ctx.Err(); err != nil {
			return err
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "compoundwords") {
			// The compound flag is the last whitespace-delimited token, found
			// case-insensitively as 'l'/'L' in the original-cased line.
			if idx := indexCaseInsensitive(trimmed, 'l'); idx >= 0 {
				fields := strings.Fields(trimmed[idx:])
				if len(fields) > 1 && len(fields[1]) == 1 {
					d.compoundFlags[fields[1]] |= ispellCompoundFlag
					d.useCompound = true
				}
			}
			continue
		}
		if lower == "suffixes" {
			suffixes, prefixes = true, false
			continue
		}
		if lower == "prefixes" {
			suffixes, prefixes = false, true
			continue
		}
		if strings.HasPrefix(lower, "flag") {
			flag, flagflags = "", 0
			rest := strings.TrimSpace(trimmed[4:])
			if strings.HasPrefix(rest, "*") {
				flagflags |= ispellCrossProduct
				rest = rest[1:]
			} else if strings.HasPrefix(rest, "~") {
				flagflags |= ispellCompoundOnly
				rest = rest[1:]
			}
			rest = strings.TrimPrefix(rest, "\\")
			if rest != "" {
				flag = rest[:1]
			}
			continue
		}
		if !suffixes && !prefixes {
			continue
		}
		mask, find, repl, ok := parseIspellAffixEntry(strings.ToLower(trimmed))
		if !ok {
			continue
		}
		affix, err := newIspellAffix(flag, flagflags, mask, find, repl, suffixes)
		if err != nil {
			return err
		}
		d.affixes = append(d.affixes, affix)
	}
	return nil
}

// importHunspellAffixes reads the new PFX/SFX format including FLAG modes and AF
// aliases.
func (d *nativeIspellDictionary) importHunspellAffixes(ctx context.Context, lines []string) error {
	// PostgreSQL's first `AF` line declares the alias COUNT and allocates the
	// table; each later `AF <text>` line fills the next slot, so aliases are
	// 1-based and the count line must not itself become an alias.
	aliases := make([]string, 0, 32)
	aliasCountDeclared := false
	pending := 0
	var pendingAffix ispellAffix
	for _, line := range lines {
		if err := ctx.Err(); err != nil {
			return err
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}
		if pending > 0 {
			if len(fields) < 4 {
				return sqlError("F0000", "invalid affix entry \""+trimmed+"\"")
			}
			affix := pendingAffix
			affix.find = hunspellStrip(strings.ToLower(fields[2]))
			// A Hunspell replacement may carry extra flags after '/': the text
			// before the slash is the affix and the rest are flags the produced
			// word must also satisfy. Those flags are looked up in the
			// compound-flag table exactly as the affix file declares them, so
			// only the replacement text is lower-cased.
			rawRepl := hunspellStrip(fields[3])
			if slash := strings.IndexByte(rawRepl, '/'); slash >= 0 {
				affix.replFlags = rawRepl[slash+1:]
				rawRepl = rawRepl[:slash]
			}
			affix.repl = strings.ToLower(rawRepl)
			// The mask is lower-cased with the other affix fields.
			cond := ""
			if len(fields) > 4 {
				cond = strings.ToLower(fields[4])
			}
			if err := affix.compileCondition(cond); err != nil {
				return err
			}
			d.affixes = append(d.affixes, affix)
			pending--
			continue
		}
		switch {
		case fields[0] == "FLAG":
			if len(fields) < 2 {
				return sqlError("F0000", "invalid FLAG command \""+trimmed+"\"")
			}
			switch strings.ToLower(fields[1]) {
			case "long":
				d.flagMode = ispellFlagLong
			case "num":
				d.flagMode = ispellFlagNum
			default:
				d.flagMode = ispellFlagChar
			}
		case fields[0] == "AF":
			if len(fields) < 2 {
				continue
			}
			if !aliasCountDeclared {
				aliasCountDeclared = true
				// Alias index 0 is always the empty flag set.
				aliases = append(aliases, "")
				continue
			}
			aliases = append(aliases, fields[1])
		case strings.HasPrefix(fields[0], "COMPOUND") || fields[0] == "ONLYINCOMPOUND":
			if len(fields) < 2 {
				continue
			}
			var bits uint8
			switch fields[0] {
			case "COMPOUNDFLAG":
				bits = ispellCompoundFlag
			case "COMPOUNDBEGIN":
				bits = ispellCompoundBegin
			case "COMPOUNDMIDDLE":
				bits = ispellCompoundMiddle
			case "COMPOUNDLAST", "COMPOUNDEND":
				bits = ispellCompoundLast
			case "ONLYINCOMPOUND":
				bits = ispellCompoundOnly
			case "COMPOUNDPERMITFLAG":
				bits = ispellCompoundPermit
			case "COMPOUNDFORBIDFLAG":
				bits = ispellCompoundForbid
			default:
				continue
			}
			d.compoundFlags[fields[1]] |= bits
			d.useCompound = true
		case fields[0] == "PFX" || fields[0] == "SFX":
			// Header form: PFX <flag> <Y|N> <count>. The third field is the
			// cross-product column (parsed as `find` upstream) and the fourth
			// is the entry count.
			if len(fields) < 4 {
				return sqlError("F0000", "invalid affix header \""+trimmed+"\"")
			}
			count := 0
			for i := 0; i < len(fields[3]); i++ {
				if fields[3][i] < '0' || fields[3][i] > '9' {
					return sqlError("F0000", "invalid affix header \""+trimmed+"\"")
				}
				count = count*10 + int(fields[3][i]-'0')
			}
			flagflags := uint8(0)
			if strings.EqualFold(fields[2], "Y") {
				flagflags = ispellCrossProduct
			}
			pendingAffix = ispellAffix{flag: fields[1], flagflags: flagflags, suffix: fields[0] == "SFX"}
			pending = count
		}
	}
	if pending != 0 {
		return sqlError("F0000", "affix file ended before its affix entries")
	}
	d.aliases = aliases
	d.useAliases = aliasCountDeclared
	return nil
}

// hunspellStrip maps the Hunspell zero placeholder to the empty replacement.
func hunspellStrip(field string) string {
	if field == "0" {
		return ""
	}
	return field
}

// indexCaseInsensitive returns the byte index of the first occurrence of c in
// either case, or -1.
func indexCaseInsensitive(text string, c byte) int {
	upper := c - ('a' - 'A')
	for i := 0; i < len(text); i++ {
		if text[i] == c || text[i] == upper {
			return i
		}
	}
	return -1
}

// parseIspellAffixEntry implements parse_affentry's state machine: a leading
// mask up to '>', then the find/replace pair separated by '-', ',' or whitespace.
func parseIspellAffixEntry(line string) (mask, find, repl string, ok bool) {
	const (
		waitMask = iota
		inMask
		waitFind
		inFind
		waitRepl
		inRepl
	)
	state := waitMask
	var maskBuilder, findBuilder, replBuilder strings.Builder
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch state {
		case waitMask:
			if ch == '#' {
				return "", "", "", false
			}
			if !isTSSpace(ch) {
				maskBuilder.WriteByte(ch)
				state = inMask
			}
		case inMask:
			if ch == '>' {
				state = waitFind
				continue
			}
			if !isTSSpace(ch) {
				maskBuilder.WriteByte(ch)
			}
		case waitFind:
			if ch == '-' {
				state = inFind
			} else if isTSAlpha(ch) || ch == '\'' {
				replBuilder.WriteByte(ch)
				state = inRepl
			} else if !isTSSpace(ch) {
				return "", "", "", false
			}
		case inFind:
			if ch == ',' {
				state = waitRepl
			} else if isTSAlpha(ch) {
				findBuilder.WriteByte(ch)
			} else if !isTSSpace(ch) {
				return "", "", "", false
			}
		case waitRepl:
			if ch == '-' {
				i = len(line)
				continue
			}
			if isTSAlpha(ch) {
				replBuilder.WriteByte(ch)
				state = inRepl
			} else if !isTSSpace(ch) {
				return "", "", "", false
			}
		case inRepl:
			if ch == '#' {
				i = len(line)
				continue
			}
			if isTSAlpha(ch) {
				replBuilder.WriteByte(ch)
			} else if !isTSSpace(ch) {
				return "", "", "", false
			}
		}
	}
	mask, find, repl = maskBuilder.String(), findBuilder.String(), replBuilder.String()
	return mask, find, repl, mask != "" && (find != "" || repl != "")
}

func isTSAlpha(ch byte) bool {
	return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z'
}

// newIspellAffix builds one affix rule, compiling its match condition.
func newIspellAffix(flag string, flagflags uint8, mask, find, repl string, suffix bool) (ispellAffix, error) {
	affix := ispellAffix{flag: flag, flagflags: flagflags, find: find, repl: repl, suffix: suffix}
	return affix, affix.compileCondition(mask)
}

// compileCondition accepts the Regis subset (literals and single-character
// classes such as `[^AEIOU]Y`) and the `.` wildcard.
func (a *ispellAffix) compileCondition(mask string) error {
	if mask == "." || mask == "" {
		a.condSimple = true
		a.cond = nil
		return nil
	}
	chars := []ispellCondChar{}
	for i := 0; i < len(mask); {
		if mask[i] == '[' {
			end := strings.IndexByte(mask[i:], ']')
			if end < 0 {
				return sqlError("F0000", "invalid affix condition \""+mask+"\"")
			}
			body := mask[i+1 : i+end]
			class := ispellCondChar{}
			if strings.HasPrefix(body, "^") {
				class.none = true
				body = body[1:]
			}
			class.chars = body
			chars = append(chars, class)
			i += end + 1
			continue
		}
		if !isTSAlpha(mask[i]) {
			return sqlError("F0000", "invalid affix condition \""+mask+"\"")
		}
		class := ispellCondChar{chars: mask[i : i+1]}
		// `x*` means the preceding character may repeat any number of times,
		// including zero.
		if i+1 < len(mask) && mask[i+1] == '*' {
			class.repeat = true
			chars = append(chars, class)
			i += 2
			continue
		}
		// Hunspell extends a condition with `{n}` repetition, e.g. `k{1}` means
		// the preceding position must occur exactly n times.
		if i+1 < len(mask) && mask[i+1] == '{' {
			end := strings.IndexByte(mask[i+1:], '}')
			if end < 0 {
				return sqlError("F0000", "invalid affix condition \""+mask+"\"")
			}
			count := 0
			digits := mask[i+2 : i+1+end]
			if digits == "" {
				return sqlError("F0000", "invalid affix condition \""+mask+"\"")
			}
			for j := 0; j < len(digits); j++ {
				if digits[j] < '0' || digits[j] > '9' {
					return sqlError("F0000", "invalid affix condition \""+mask+"\"")
				}
				count = count*10 + int(digits[j]-'0')
			}
			if count < 1 {
				return sqlError("F0000", "invalid affix condition \""+mask+"\"")
			}
			for range count {
				chars = append(chars, class)
			}
			i += end + 2
			continue
		}
		chars = append(chars, class)
		i++
	}
	a.condSimple = false
	a.cond = chars
	return nil
}

// sortAffixes indexes affixes by kind and sorts the compound flag set.
func (d *nativeIspellDictionary) sortAffixes(ctx context.Context) error {
	for i := range d.affixes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.affixes[i].suffix {
			d.suffixes = append(d.suffixes, i)
		} else {
			d.prefixes = append(d.prefixes, i)
		}
	}
	// PostgreSQL matches affixes by walking outward from the word's end (suffix)
	// or start (prefix), so shallower affixes are found first. Depth is the
	// strippable length, which is len(repl) here.
	byDepth := func(indices []int) {
		sort.SliceStable(indices, func(i, j int) bool {
			left, right := &d.affixes[indices[i]], &d.affixes[indices[j]]
			if len(left.repl) != len(right.repl) {
				return len(left.repl) < len(right.repl)
			}
			return left.repl < right.repl
		})
	}
	byDepth(d.prefixes)
	byDepth(d.suffixes)
	return nil
}

// LexizeVariants mirrors NINormalizeWord: affix normalization first, then
// compound splitting when the affix file enables it.
func (d *nativeIspellDictionary) LexizeVariants(ctx context.Context, token string) ([][]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if token == "" {
		return nil, nil
	}
	word := strings.ToLower(token)
	if len(word) > 256 {
		return nil, nil
	}
	forms, err := d.normalizeSubWord(ctx, word, 0)
	if err != nil {
		return nil, err
	}
	variants := make([][]string, 0, len(forms))
	for _, form := range forms {
		variants = append(variants, []string{form})
	}
	if d.useCompound {
		compound, err := d.normalizeCompound(ctx, word)
		if err != nil {
			return nil, err
		}
		variants = append(variants, compound...)
	}
	if len(variants) == 0 {
		return nil, nil
	}
	return variants, nil
}

// Lexize implements TextSearchDictionaryKernel. Compound splits already arrive
// as separate variant groups from LexizeVariants, so the flattened lexemes are
// the same sequence PostgreSQL's dispell_lexize emits.
func (d *nativeIspellDictionary) Lexize(ctx context.Context, token string) ([]string, error) {
	variants, err := d.LexizeVariants(ctx, token)
	if err != nil || variants == nil {
		return nil, err
	}
	return flattenTextSearchVariants(variants), nil
}

// normalizeCompound reproduces the compound branch of NINormalizeWord. Every
// split point where the leading text normalizes to a dictionary word with a
// compatible compound position is explored recursively, and each complete split
// of two or more stems becomes one variant group. Flattening those groups in
// order yields the lexeme order PostgreSQL's dispell_lexize emits.
func (d *nativeIspellDictionary) normalizeCompound(ctx context.Context, word string) ([][]string, error) {
	if len(word) < 2 {
		return nil, nil
	}
	var variants [][]string
	if err := d.walkCompounds(ctx, word, 0, nil, &variants); err != nil {
		return nil, err
	}
	return variants, nil
}

// walkCompounds extends the current stem list with every split of word starting
// at start. A branch completes only when it consumes the whole word and carries
// more than one stem, matching NINormalizeWord's nstem > 1 requirement.
func (d *nativeIspellDictionary) walkCompounds(ctx context.Context, word string, start int, stems []string, out *[][]string) error {
	for level := start; level < len(word); level++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		var position uint8
		switch {
		case start == 0:
			position = ispellCompoundBegin
		case level == len(word)-1:
			position = ispellCompoundLast
		default:
			position = ispellCompoundMiddle
		}
		forms, err := d.normalizeSubWord(ctx, word[start:level+1], position)
		if err != nil {
			return err
		}
		for _, form := range forms {
			next := make([]string, len(stems), len(stems)+1)
			copy(next, stems)
			next = append(next, form)
			if level == len(word)-1 {
				if len(next) > 1 {
					*out = append(*out, next)
				}
				continue
			}
			if err := d.walkCompounds(ctx, word, level+1, next, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// normalizeSubWord mirrors NormalizeSubWord. flag carries the compound position
// requested by the caller; zero means an ordinary word.
func (d *nativeIspellDictionary) normalizeSubWord(ctx context.Context, word string, flag uint8) ([]string, error) {
	if word == "" || len(word) > 256 {
		return nil, nil
	}
	forms := make([]string, 0, 4)
	if d.findWord(word, "", flag) {
		forms = append(forms, word)
	}
	for _, index := range d.prefixes {
		affix := &d.affixes[index]
		if flag == 0 && d.affixCompoundOnly(affix) {
			continue
		}
		newword, ok := affix.apply(word)
		if !ok {
			continue
		}
		if d.findWord(newword, affix.flag, flag) {
			forms = addIspellForm(forms, newword)
		}
	}
	for _, index := range d.suffixes {
		suffix := &d.affixes[index]
		if flag == 0 && d.affixCompoundOnly(suffix) {
			continue
		}
		newword, baselen, ok := suffix.applySuffix(word)
		if !ok {
			continue
		}
		if d.findWord(newword, suffix.flag, flag) {
			forms = addIspellForm(forms, newword)
		}
		for _, prefixIndex := range d.prefixes {
			prefix := &d.affixes[prefixIndex]
			if flag == 0 && d.affixCompoundOnly(prefix) {
				continue
			}
			result, _, ok := prefix.applySuffixPrefix(newword, baselen)
			if !ok {
				continue
			}
			// A crossing pair contributes no required flag; otherwise the
			// prefix's own flag must be present in the base form.
			required := prefix.flag
			if prefix.flagflags&suffix.flagflags&ispellCrossProduct != 0 {
				required = ""
			}
			if d.findWord(result, required, flag) {
				forms = addIspellForm(forms, result)
			}
		}
	}
	return forms, ctx.Err()
}

// addIspellForm appends form unless it repeats the previous entry, matching
// addToResult's adjacent de-duplication.
func addIspellForm(forms []string, form string) []string {
	if len(forms) != 0 && forms[len(forms)-1] == form {
		return forms
	}
	return append(forms, form)
}

// apply builds the word a prefix produces. Following CheckAffix, `repl` is the
// text present on the inflected word that gets stripped and `find` is the text
// appended to form the base form.
func (a *ispellAffix) apply(word string) (string, bool) {
	if a.suffix || len(word) < len(a.repl) || !strings.HasPrefix(word, a.repl) {
		return "", false
	}
	result := a.find + word[len(a.repl):]
	if !a.conditionMatches(result) {
		return "", false
	}
	return result, true
}

// applySuffix builds the word a suffix produces and reports the length of the
// unchanged leading part, which the prefix stage needs.
func (a *ispellAffix) applySuffix(word string) (string, int, bool) {
	if !a.suffix || len(word) < len(a.repl) || !strings.HasSuffix(word, a.repl) {
		return "", 0, false
	}
	kept := len(word) - len(a.repl)
	result := word[:kept] + a.find
	if !a.conditionMatches(result) {
		return "", 0, false
	}
	return result, kept, true
}

// applySuffixPrefix composes a prefix onto a suffix-stripped result. baselen is
// the suffix stage's unchanged length; when the prefix is itself the whole
// remaining word CheckAffix abandons the combination.
func (a *ispellAffix) applySuffixPrefix(word string, baselen int) (string, int, bool) {
	if a.suffix || len(word) < len(a.repl) || !strings.HasPrefix(word, a.repl) {
		return "", 0, false
	}
	if baselen+len(a.find) <= len(a.repl) {
		return "", 0, false
	}
	result := a.find + word[len(a.repl):]
	if !a.conditionMatches(result) {
		return "", 0, false
	}
	return result, baselen, true
}

// conditionMatches applies the affix condition. Prefix conditions are anchored
// at the start and suffix conditions at the end, matching NIAddAffix's
// `^mask`/`mask$` wrapping.
func (a *ispellAffix) conditionMatches(word string) bool {
	if a.condSimple {
		return true
	}
	if len(word) < len(a.cond) {
		return false
	}
	offset := 0
	if a.suffix {
		offset = len(word) - len(a.cond)
	}
	var match func(position int, text []byte) bool
	match = func(position int, text []byte) bool {
		if position == len(a.cond) {
			// A trailing repeat may absorb whatever text remains.
			return len(text) == 0 || a.cond[position-1].repeat
		}
		class := a.cond[position]
		consumed := func(ch byte) bool {
			contains := strings.IndexByte(class.chars, ch) >= 0
			return class.none != contains
		}
		if class.repeat {
			for skip := len(text); skip >= 0; skip-- {
				if match(position+1, text[skip:]) {
					return true
				}
				if skip == 0 {
					break
				}
				if len(text) < skip || !consumed(text[skip-1]) {
					break
				}
			}
			return false
		}
		if len(text) == 0 || !consumed(text[0]) {
			return false
		}
		return match(position+1, text[1:])
	}
	return match(0, []byte(word[offset:]))
}

// findWord mirrors FindWord: the word must be a base form and, when affixflag
// is non-empty, its flag set must contain that flag. flagapplies applies the
// compound-context rules.
func (d *nativeIspellDictionary) findWord(word, affixflag string, flag uint8) bool {
	if word == "" {
		return false
	}
	index := sort.Search(len(d.words), func(i int) bool { return d.words[i].word >= word })
	if index == len(d.words) || d.words[index].word != word {
		return false
	}
	entry := &d.words[index]
	if flag == 0 {
		// A compound-only word cannot stand alone.
		if d.wordCompoundFlags(entry)&ispellCompoundOnly != 0 {
			return false
		}
	} else if d.wordCompoundFlags(entry)&flag == 0 {
		return false
	}
	return d.affixFlagInUse(entry, affixflag)
}

// wordCompoundFlags maps the entry's declared flags onto compound positions.
func (d *nativeIspellDictionary) wordCompoundFlags(entry *ispellWord) uint8 {
	var result uint8
	for _, flag := range d.decodeFlags(entry.flags) {
		result |= d.compoundFlags[flag]
	}
	// A compound-only word with no compound flag still counts as a compound
	// flag, and that suppresses the only-in-compound restriction.
	if result&ispellCompoundOnly != 0 && result&ispellCompoundFlagMask == 0 {
		result |= ispellCompoundFlag
	} else if result&ispellCompoundOnly != 0 {
		result &^= ispellCompoundOnly
	}
	return result & ispellCompoundFlagMask
}

// affixReplCompoundFlags maps the '/'-suffixed flags of a Hunspell replacement
// onto the compound bits their declarations granted, mirroring the
// getCompoundAffixFlagValue lookup NIImportOOAffixes performs when it folds
// those flags into the affix's own flagflags.
func (d *nativeIspellDictionary) affixReplCompoundFlags(flags string) uint8 {
	var result uint8
	for _, flag := range d.decodeFlags(flags) {
		result |= d.compoundFlags[flag]
	}
	return result
}

// affixCompoundOnly reports whether an affix is marked ONLYINCOMPOUND, either on
// the affix rule itself (the old-format `flag ~X` form) or on its replacement
// (the Hunspell `repl/flags` form). CheckAffix's `flagflags == 0` branch refuses
// such an affix for an ordinary word, so a standalone token can never be
// normalized through it; compound positions keep using the affix.
func (d *nativeIspellDictionary) affixCompoundOnly(affix *ispellAffix) bool {
	if affix.flagflags&ispellCompoundOnly != 0 {
		return true
	}
	return d.affixReplCompoundFlags(affix.replFlags)&ispellCompoundOnly != 0
}

// splitFlagsByMode splits raw flag text into individual flags according to the
// affix file's FLAG mode.
func splitFlagsByMode(flags string, mode ispellFlagMode) []string {
	switch mode {
	case ispellFlagLong:
		out := make([]string, 0, len(flags)/2+1)
		for i := 0; i+1 < len(flags); i += 2 {
			out = append(out, flags[i:i+2])
		}
		return out
	case ispellFlagNum:
		return strings.Split(flags, ",")
	default:
		out := make([]string, 0, len(flags))
		for i := range len(flags) {
			out = append(out, flags[i:i+1])
		}
		return out
	}
}

// decodeAliasFlags splits the raw flag text an AF alias expands to. Alias text
// is always a plain flag run, never another index.
func (d *nativeIspellDictionary) decodeAliasFlags(flags string) []string {
	return splitFlagsByMode(flags, d.flagMode)
}

// affixFlagInUse mirrors IsAffixFlagInUse: an empty affixflag matches anything,
// otherwise the entry's set must contain it.
func (d *nativeIspellDictionary) affixFlagInUse(entry *ispellWord, affixflag string) bool {
	if affixflag == "" {
		return true
	}
	for _, flag := range d.decodeFlags(entry.flags) {
		if flag == affixflag {
			return true
		}
	}
	return false
}

// decodeFlags splits an entry's raw flag set according to the affix file's
// FLAG mode, matching getNextFlagFromString. When the affix file declared an AF
// alias table, the whole field is a 1-based index into it instead.
func (d *nativeIspellDictionary) decodeFlags(flags string) []string {
	if flags == "" {
		return nil
	}
	if d.useAliases {
		index := 0
		for i := 0; i < len(flags); i++ {
			if flags[i] < '0' || flags[i] > '9' {
				return nil
			}
			index = index*10 + int(flags[i]-'0')
		}
		if index >= len(d.aliases) {
			return nil
		}
		// Alias index 0 is the empty flag set.
		if index == 0 || d.aliases[index] == "" {
			return nil
		}
		return d.decodeAliasFlags(d.aliases[index])
	}
	return splitFlagsByMode(flags, d.flagMode)
}

var _ model.TextSearchDictionaryKernel = (*nativeIspellDictionary)(nil)
var _ model.TextSearchVariantKernel = (*nativeIspellDictionary)(nil)

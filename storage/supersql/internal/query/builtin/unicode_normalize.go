package builtin

import (
	"context"
	"math/bits"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type unicodeNormalizationForm struct {
	compatibility bool
	compose       bool
}

func compileUnicodeNormalization(signature Signature) scalarKernel {
	switch signature.ID {
	case 4350:
		return normalizeUnicodeText
	case 4351:
		return isNormalizedUnicodeText
	default:
		return nil
	}
}

func parseUnicodeNormalizationForm(ctx context.Context, text string) (unicodeNormalizationForm, error) {
	if err := ctx.Err(); err != nil {
		return unicodeNormalizationForm{}, err
	}
	end := 0
	for end < min(len(text), 5) && text[end] != 0 {
		end++
	}
	// PostgreSQL parses the form as a C string before inspecting the input.
	name := text[:end]
	var folded [4]byte
	if len(name) == 3 || len(name) == 4 {
		for i := range len(name) {
			folded[i] = name[i]
			if folded[i] >= 'a' && folded[i] <= 'z' {
				folded[i] -= 'a' - 'A'
			}
		}
		switch string(folded[:len(name)]) {
		case "NFC":
			return unicodeNormalizationForm{compose: true}, nil
		case "NFD":
			return unicodeNormalizationForm{}, nil
		case "NFKC":
			return unicodeNormalizationForm{compatibility: true, compose: true}, nil
		case "NFKD":
			return unicodeNormalizationForm{compatibility: true}, nil
		}
	}
	return unicodeNormalizationForm{}, sqlError("22023", "invalid normalization form")
}

func normalizeUnicodeText(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	budget := callBudget.OutputBytes
	form, err := parseUnicodeNormalizationForm(ctx, args[1].StrVal)
	if err != nil {
		return model.Value{}, err
	}
	input := args[0].StrVal
	characters, err := textCharacters(ctx, input)
	if err != nil {
		return model.Value{}, err
	}
	if characters == int64(len(input)) {
		return ownedText(ctx, budget, input)
	}
	var work unicodeNormalizationWork
	var size int64
	_, err = walkUnicodeNormalized(ctx, input, form, &work, func(code rune) (bool, error) {
		size += int64(utf8.RuneLen(code))
		return true, outputBudget(size, budget)
	})
	if err != nil {
		return model.Value{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	// The sizing pass leaves only fixed scratch live beside this exact-size output.
	var output strings.Builder
	output.Grow(int(size))
	_, err = walkUnicodeNormalized(ctx, input, form, &work, func(code rune) (bool, error) {
		output.WriteRune(code)
		return true, nil
	})
	if err != nil {
		return model.Value{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	return model.StringVal(output.String()), nil
}

func isNormalizedUnicodeText(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
	form, err := parseUnicodeNormalizationForm(ctx, args[1].StrVal)
	if err != nil {
		return model.Value{}, err
	}
	input := args[0].StrVal
	var work unicodeNormalizationWork
	quick := unicodeQCYes
	var previousClass uint8
	for offset := 0; offset < len(input); {
		if err := work.step(ctx); err != nil {
			return model.Value{}, err
		}
		code, width := utf8.DecodeRuneInString(input[offset:])
		// Malformed UTF8 and embedded NUL cannot enter PostgreSQL text datums.
		// Raw Call follows the existing text-kernel rejection of those inputs.
		if code == 0 || code == utf8.RuneError && width == 1 {
			return model.Value{}, sqlError("22021", "invalid UTF8 text datum")
		}
		class := lookupUnicodeNorm(code).combiningClass
		check := unicodeQuickCheck(code, form.compatibility, form.compose)
		if check == unicodeQCNo || class != 0 && previousClass > class {
			quick = unicodeQCNo
		} else if check == unicodeQCMaybe && quick == unicodeQCYes {
			quick = unicodeQCMaybe
		}
		previousClass = class
		offset += width
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if quick != unicodeQCMaybe {
		return model.BoolVal(quick == unicodeQCYes), nil
	}
	position := 0
	matched, err := walkUnicodeNormalized(ctx, input, form, &work, func(code rune) (bool, error) {
		if position == len(input) {
			return false, nil
		}
		original, width := utf8.DecodeRuneInString(input[position:])
		if code != original {
			return false, nil
		}
		position += width
		return true, nil
	})
	if err != nil {
		return model.Value{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	return model.BoolVal(matched && position == len(input)), nil
}

type unicodeNormalizationWork struct {
	remaining int
}

func (work *unicodeNormalizationWork) step(ctx context.Context) error {
	if work.remaining == 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		work.remaining = cancellationChunk
	}
	work.remaining--
	return nil
}

type unicodeDecompositionCursor struct {
	input         string
	offset        int
	compatibility bool
	codes         [unicodeMaxDecomposition]rune
	index, count  int
}

func (cursor *unicodeDecompositionCursor) peek(ctx context.Context, work *unicodeNormalizationWork) (rune, bool, error) {
	if cursor.index == cursor.count {
		if cursor.offset == len(cursor.input) {
			return 0, false, nil
		}
		code, width := utf8.DecodeRuneInString(cursor.input[cursor.offset:])
		cursor.offset += width
		cursor.index, cursor.count = 0, 0
		if err := cursor.decompose(ctx, work, code); err != nil {
			return 0, false, err
		}
	}
	return cursor.codes[cursor.index], true, nil
}

func (cursor *unicodeDecompositionCursor) next(ctx context.Context, work *unicodeNormalizationWork) (rune, bool, error) {
	if err := work.step(ctx); err != nil {
		return 0, false, err
	}
	code, ok, err := cursor.peek(ctx, work)
	if ok {
		cursor.index++
	}
	return code, ok, err
}

func (cursor *unicodeDecompositionCursor) decompose(ctx context.Context, work *unicodeNormalizationWork, code rune) error {
	if err := work.step(ctx); err != nil {
		return err
	}
	if index := code - 0xac00; index >= 0 && index < 19*21*28 {
		cursor.codes[cursor.count] = 0x1100 + index/(21*28)
		cursor.codes[cursor.count+1] = 0x1161 + index%(21*28)/28
		cursor.count += 2
		if trailing := index % 28; trailing != 0 {
			cursor.codes[cursor.count] = 0x11a7 + trailing
			cursor.count++
		}
		return nil
	}
	entry := lookupUnicodeNorm(code)
	count := entry.decompositionLen(cursor.compatibility)
	if count == 0 {
		cursor.codes[cursor.count] = code
		cursor.count++
		return nil
	}
	for i := range count {
		if err := cursor.decompose(ctx, work, entry.decompositionAt(i)); err != nil {
			return err
		}
	}
	return nil
}

type unicodeCombiningCode struct {
	code  rune
	class uint8
}

type unicodeOrderedCursor struct {
	source        unicodeDecompositionCursor
	start, replay unicodeDecompositionCursor
	short         [32]unicodeCombiningCode
	index, count  int
	classes       [4]uint64
	markCount     int64
	remaining     int64
	class         uint8
	long          bool
}

func (cursor *unicodeOrderedCursor) next(ctx context.Context, work *unicodeNormalizationWork) (rune, bool, error) {
	for {
		if cursor.index < cursor.count {
			code := cursor.short[cursor.index].code
			cursor.index++
			return code, true, work.step(ctx)
		}
		if cursor.long {
			for cursor.remaining > 0 {
				code, _, err := cursor.replay.next(ctx, work)
				if err != nil {
					return 0, false, err
				}
				cursor.remaining--
				if cursor.class == 0 || lookupUnicodeNorm(code).combiningClass == cursor.class {
					return code, true, nil
				}
			}
			if cursor.advanceClass() {
				continue
			}
			cursor.long = false
		}
		code, ok, err := cursor.source.peek(ctx, work)
		if err != nil || !ok {
			return 0, ok, err
		}
		if lookupUnicodeNorm(code).combiningClass == 0 {
			return cursor.source.next(ctx, work)
		}
		if err := cursor.collectMarks(ctx, work); err != nil {
			return 0, false, err
		}
	}
}

func (cursor *unicodeOrderedCursor) collectMarks(ctx context.Context, work *unicodeNormalizationWork) error {
	cursor.start = cursor.source
	cursor.index, cursor.count = 0, 0
	cursor.markCount = 0
	cursor.classes = [4]uint64{}
	var previous uint8
	ordered := true
	for {
		code, ok, err := cursor.source.peek(ctx, work)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		class := lookupUnicodeNorm(code).combiningClass
		if class == 0 {
			break
		}
		if _, _, err := cursor.source.next(ctx, work); err != nil {
			return err
		}
		if cursor.markCount < int64(len(cursor.short)) {
			cursor.short[cursor.markCount] = unicodeCombiningCode{code: code, class: class}
		}
		cursor.markCount++
		cursor.classes[class/64] |= uint64(1) << (class % 64)
		ordered = ordered && previous <= class
		previous = class
	}
	if cursor.markCount <= int64(len(cursor.short)) {
		cursor.count = int(cursor.markCount)
		if ordered {
			return nil
		}
		// Insertion is bounded to the inline run; equal classes retain input order.
		for i := 1; i < cursor.count; i++ {
			entry := cursor.short[i]
			j := i
			for j > 0 && cursor.short[j-1].class > entry.class {
				if err := work.step(ctx); err != nil {
					return err
				}
				cursor.short[j] = cursor.short[j-1]
				j--
			}
			cursor.short[j] = entry
		}
		return nil
	}
	cursor.long = true
	if ordered {
		cursor.class = 0
		cursor.classes = [4]uint64{}
		cursor.replay = cursor.start
		cursor.remaining = cursor.markCount
	} else {
		// Replay once per occupied class: at most 255 linear scans, independent
		// of run length. No combining-run allocation competes with output bytes.
		cursor.advanceClass()
	}
	return nil
}

func (cursor *unicodeOrderedCursor) advanceClass() bool {
	for word, mask := range cursor.classes {
		if mask == 0 {
			continue
		}
		bit := bits.TrailingZeros64(mask)
		cursor.classes[word] &^= uint64(1) << bit
		cursor.class = uint8(word*64 + bit)
		cursor.replay = cursor.start
		cursor.remaining = cursor.markCount
		return true
	}
	return false
}

func walkUnicodeNormalized(ctx context.Context, input string, form unicodeNormalizationForm, work *unicodeNormalizationWork, emit func(rune) (bool, error)) (bool, error) {
	ordered := unicodeOrderedCursor{source: unicodeDecompositionCursor{input: input, compatibility: form.compatibility}}
	var pending rune
	hasPending := false
	for {
		first := pending
		if !hasPending {
			code, ok, err := ordered.next(ctx, work)
			if err != nil || !ok {
				return !ok, err
			}
			first = code
		}
		hasPending = false
		if !form.compose {
			if ok, err := emit(first); err != nil || !ok {
				return ok, err
			}
			continue
		}
		replay := ordered
		starter := first
		lastClass := -1
		var scanned, retained int64
		for {
			code, ok, err := ordered.next(ctx, work)
			if err != nil {
				return false, err
			}
			if !ok {
				break
			}
			class := int(lookupUnicodeNorm(code).combiningClass)
			if lastClass < class {
				if composite, ok := composeUnicodePair(starter, code); ok {
					starter = composite
					scanned++
					continue
				}
			}
			if class == 0 {
				pending, hasPending = code, true
				break
			}
			lastClass = class
			retained++
			scanned++
		}
		if ok, err := emit(starter); err != nil || !ok {
			return ok, err
		}
		if retained == 0 {
			continue
		}
		// The final starter precedes its uncomposed marks. Replay only this span
		// to emit those marks without retaining an arbitrarily long run.
		starter, lastClass = first, -1
		for range scanned {
			code, _, err := replay.next(ctx, work)
			if err != nil {
				return false, err
			}
			class := int(lookupUnicodeNorm(code).combiningClass)
			if lastClass < class {
				if composite, ok := composeUnicodePair(starter, code); ok {
					starter = composite
					continue
				}
			}
			lastClass = class
			if ok, err := emit(code); err != nil || !ok {
				return ok, err
			}
		}
	}
}

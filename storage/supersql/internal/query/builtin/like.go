package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileLike(signature Signature, options ResolveOptions) scalarKernel {
	var negate, insensitive bool
	switch signature.ID {
	case 850, 858, 1569, 1571, 1631, 2005, 2007:
	case 851, 859, 1570, 1572, 1632, 2006, 2008:
		negate = true
	case 1633, 1635, 1660:
		insensitive = true
	case 1634, 1636, 1661:
		negate, insensitive = true, true
	default:
		return nil
	}
	text := signature.Arguments[0] != 17
	collation := options.Collation
	if collation == 100 {
		collation = options.DefaultCollation
	}
	indeterminate := text && collation != 950 && collation != 951
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		if indeterminate {
			return model.Value{}, sqlError("42P22", "LIKE requires a resolved C or POSIX collation")
		}
		var matched bool
		var err error
		if text {
			if _, err := textCharacters(ctx, args[0].StrVal); err != nil {
				return model.Value{}, err
			}
			if _, err := textCharacters(ctx, args[1].StrVal); err != nil {
				return model.Value{}, err
			}
			matched, err = matchLike(ctx, args[0].StrVal, args[1].StrVal, likeMode{characters: true, insensitive: insensitive})
		} else {
			matched, err = matchLike(ctx, args[0].BytesVal, args[1].BytesVal, likeMode{})
		}
		if err != nil {
			return model.Value{}, err
		}
		return model.BoolVal(matched != negate), nil
	}
}

type likeMode struct {
	characters  bool
	insensitive bool
}

// A PostgreSQL % search returns TRUE, ABORT, or an error, never FALSE.
// Reaching another % therefore discards the earlier search position. Keeping
// only the latest one bounds space to O(1) and work to O(text bytes * pattern bytes).
func matchLike[T ~string | ~[]byte](ctx context.Context, text, pattern T, mode likeMode) (bool, error) {
	position, pat := 0, 0
	retryText, retryPattern := 0, -1
	var progress likeProgress
	for position < len(text) {
		if err := progress.check(ctx); err != nil {
			return false, err
		}
		if pat == len(pattern) {
			if retryPattern < 0 {
				return false, nil
			}
			retryText = likeNextCharacter(text, retryText, mode.characters)
			position, pat = retryText, retryPattern
			continue
		}
		switch pattern[pat] {
		case '%':
			var err error
			position, pat, err = advanceLikePercent(ctx, text, pattern, position, pat+1, mode, &progress)
			if err != nil {
				return false, err
			}
			if pat == len(pattern) {
				return true, nil
			}
			retryText, retryPattern = position, pat
			continue
		case '_':
			position = likeNextCharacter(text, position, mode.characters)
			pat++
			continue
		case '\\':
			pat++
			if pat == len(pattern) {
				return false, sqlError("22025", "LIKE pattern must not end with escape character")
			}
		}
		if likeFoldASCII(text[position], mode.insensitive) == likeFoldASCII(pattern[pat], mode.insensitive) {
			// Literals advance in byte lockstep; wildcard retries stay character-aligned.
			position++
			pat++
			continue
		}
		if retryPattern < 0 {
			return false, nil
		}
		retryText = likeNextCharacter(text, retryText, mode.characters)
		position, pat = retryText, retryPattern
	}
	// Exhausted text cannot expose an otherwise unreachable trailing escape.
	for pat < len(pattern) && pattern[pat] == '%' {
		if err := progress.check(ctx); err != nil {
			return false, err
		}
		pat++
	}
	return pat == len(pattern), nil
}

func advanceLikePercent[T ~string | ~[]byte](ctx context.Context, text, pattern T, position, pat int, mode likeMode, progress *likeProgress) (int, int, error) {
	for pat < len(pattern) && (pattern[pat] == '%' || pattern[pat] == '_') {
		if err := progress.check(ctx); err != nil {
			return position, pat, err
		}
		if pattern[pat] == '_' {
			if position == len(text) {
				return position, pat, nil
			}
			position = likeNextCharacter(text, position, mode.characters)
		}
		pat++
	}
	// Percent lookahead checks escapes even if an adjacent underscore used the last character.
	if pat+1 == len(pattern) && pattern[pat] == '\\' {
		return position, pat, sqlError("22025", "LIKE pattern must not end with escape character")
	}
	return position, pat, nil
}

func likeNextCharacter[T ~string | ~[]byte](input T, position int, characters bool) int {
	position++
	if characters {
		for position < len(input) && input[position]&0xc0 == 0x80 {
			position++
		}
	}
	return position
}

func likeFoldASCII(value byte, insensitive bool) byte {
	if insensitive && value >= 'A' && value <= 'Z' {
		return value + ('a' - 'A')
	}
	return value
}

type likeProgress int

func (p *likeProgress) check(ctx context.Context) error {
	*p--
	if *p > 0 {
		return nil
	}
	*p = cancellationChunk
	return ctx.Err()
}

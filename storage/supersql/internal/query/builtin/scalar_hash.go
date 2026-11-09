package builtin

import (
	"context"
	"encoding/binary"
	"math"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileScalarHash(signature Signature, options ResolveOptions) scalarKernel {
	switch signature.ID {
	case 400, 425, 441, 442, 443, 444, 445, 447, 448, 449, 450, 451, 452, 453, 455, 949, 972, 1080:
	default:
		return nil
	}
	extended := signature.Result == 20
	collation := options.Collation
	if collation == 100 {
		collation = options.DefaultCollation
	}
	collationInfo, collationKnown := LookupCollation(collation)
	deterministic := collationKnown && collation != 100 && collationInfo.Deterministic
	return func(ctx context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		seed := uint64(0)
		if extended {
			seed = uint64(args[1].IntVal)
		}
		var hash uint64
		var err error
		switch signature.Arguments[0] {
		case 21, 23, 20, 26:
			value := args[0].IntVal
			word := uint32(value)
			if signature.Arguments[0] == 21 {
				word = uint32(int32(int16(value)))
			} else if signature.Arguments[0] == 20 {
				word = valueHashInt64Word(value)
			}
			hash = pgHashUint32Extended(word, seed)
		case 700, 701:
			value := args[0].FloatVal
			if signature.Arguments[0] == 700 {
				value = float64(float32(value))
			}
			hash = seed
			if value != 0 {
				bits := math.Float64bits(value)
				if math.IsNaN(value) {
					// PostgreSQL's C NAN is the positive quiet NaN without payload.
					bits = 0x7ff8000000000000
				}
				var data [8]byte
				binary.LittleEndian.PutUint64(data[:], bits)
				hash, err = pgHashBytesExtended(ctx, data[:], seed)
			}
		case 19, 25, 1042:
			text := args[0].StrVal
			if signature.Arguments[0] == 19 {
				for start := 0; start < len(text); {
					if err := ctx.Err(); err != nil {
						return model.Value{}, err
					}
					end := start + min(cancellationChunk, len(text)-start)
					if offset := strings.IndexByte(text[start:end], 0); offset >= 0 {
						text = text[:start+offset]
						break
					}
					start = end
				}
			} else {
				if !deterministic {
					return model.Value{}, sqlError("42P22", "string hashing requires a resolved deterministic collation")
				}
				if signature.Arguments[0] == 1042 {
					text, err = trimCharacterSpaces(ctx, text)
					if err != nil {
						return model.Value{}, err
					}
				}
			}
			hash, err = pgHashStringExtended(ctx, text, seed)
		}
		if err != nil {
			return model.Value{}, err
		}
		if extended {
			return model.Int64Val(int64(hash)), nil
		}
		return model.Int64Val(int64(int32(hash))), nil
	}
}

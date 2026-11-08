package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// ParseRelationOID recognizes only PostgreSQL's unpadded decimal form or "-".
// Non-numeric input belongs to qualified-name resolution, including whitespace.
func ParseRelationOID(ctx context.Context, text string, maxBytes uint64) (model.OID, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if uint64(len(text)) > maxBytes {
		return 0, false, sqlError("54000", "relation reference exceeds budget")
	}
	if text == "-" {
		return 0, true, nil
	}
	if text == "" {
		return 0, false, nil
	}
	var value uint32
	overflow := false
	for i := range len(text) {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, false, err
			}
		}
		if text[i] < '0' || text[i] > '9' {
			return 0, false, nil
		}
		digit := uint32(text[i] - '0')
		if value > (math.MaxUint32-digit)/10 {
			overflow = true
		}
		if !overflow {
			value = value*10 + digit
		}
	}
	if overflow {
		return 0, true, sqlError("22003", "OID out of range")
	}
	return model.OID(value), true, nil
}

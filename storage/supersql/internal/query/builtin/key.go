package builtin

import (
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// MissingKeyImplementationError identifies a SQL key codec backlog item, not a
// product exclusion. Arrays and other richer datums require separate codecs.
type MissingKeyImplementationError struct{ Type model.Type }

func (e *MissingKeyImplementationError) Error() string {
	return fmt.Sprintf("SQL key implementation missing for type %d", e.Type.ID)
}

// KeyEncoder implements canonical SQL ordering for supported scalar types,
// JSONB, and recursively declared arrays and composites.
// Text uses C/POSIX byte ordering; bpchar ignores trailing U+0020. The default
// profile is C. Locale collations remain unsupported.
// Instances are immutable and safe to share between transaction and scan paths.
type KeyEncoder struct {
	defaultCollation       model.CollationID
	types                  TypeResolver
	informationSchemaTypes InformationSchemaTypeResolver
}

var _ storage.KeyEncoder = (*KeyEncoder)(nil)

// NewKeyEncoder accepts default (0 or 100), C (950), or POSIX (951).
func NewKeyEncoder(defaultCollation model.CollationID) (*KeyEncoder, error) {
	if defaultCollation == 0 || defaultCollation == 100 {
		defaultCollation = 950
	}
	if defaultCollation != 950 && defaultCollation != 951 {
		return nil, sqlError("42P22", "SQL keys require C or POSIX collation")
	}
	return &KeyEncoder{defaultCollation: defaultCollation}, nil
}

// WithTypes returns an encoder pinned to immutable declared-type semantics.
func (e *KeyEncoder) WithTypes(types TypeResolver) *KeyEncoder {
	if e == nil {
		return nil
	}
	copy := *e
	copy.types = types
	return &copy
}

// WithResolveOptions returns an encoder pinned to every immutable type resolver.
func (e *KeyEncoder) WithResolveOptions(options ResolveOptions) *KeyEncoder {
	if e == nil {
		return nil
	}
	copy := *e
	copy.types = options.Types
	copy.informationSchemaTypes = options.InformationSchemaTypes
	return &copy
}

// EncodePrefix borrows layout and values for this call and returns one owned
// buffer with capacity equal to length. An empty tuple encodes to an empty key;
// only storage.KeyBound.Unbounded denotes an unbounded endpoint. Field markers
// order NULL independently of direction, and complete fields are prefix-free.
func (e *KeyEncoder) EncodePrefix(ctx context.Context, layout []storage.KeyColumn, values []model.Value, maxBytes uint64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e == nil || len(values) > len(layout) {
		return nil, storage.ErrInvalidScanPlan
	}
	maxBytes = min(maxBytes, uint64(math.MaxInt))
	if uint64(len(values)) > maxBytes {
		return nil, columnar.ErrBatchBudget
	}
	var size uint64
	var prepared keyPreparation
	for i := range layout {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		column := &layout[i]
		if column.ColumnID == 0 {
			return nil, storage.ErrInvalidScanPlan
		}
		for j := range i {
			if j%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if layout[j].ColumnID == column.ColumnID {
				return nil, storage.ErrInvalidScanPlan
			}
		}
		if err := e.validateKeyLayout(ctx, model.TypeLayout{Type: column.Type, Children: column.Children}, 0); err != nil {
			return nil, err
		}
		if i < len(values) {
			fieldSize, err := e.measureKeyColumn(ctx, column, &values[i], maxBytes-size, &prepared)
			if err != nil {
				return nil, err
			}
			size += fieldSize
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := make([]byte, 0, int(size))
	for i := range values {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		column, value := &layout[i], &values[i]
		if value.IsNull {
			marker := byte(2)
			if column.NullsFirst {
				marker = 0
			}
			key = append(key, marker)
			continue
		}
		key = append(key, 1)
		mask := byte(0)
		if column.Descending {
			mask = 0xff
		}
		var err error
		key, err = e.appendNestedKeyValue(ctx, key, model.TypeLayout{Type: column.Type, Children: column.Children}, value, mask, 0, &prepared)
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return key, nil
}

// IsOIDType identifies PostgreSQL's unsigned 32-bit object-reference types.
func IsOIDType(id model.TypeID) bool {
	switch id {
	case 26, 24, 2202, 2203, 2204, 2205, 2206, 3734, 3769, 4089, 4096, 4191:
		return true
	default:
		return false
	}
}

func (e *KeyEncoder) validateKeyType(typ model.Type) error {
	if typ.ID == 0 || !typ.Storage.Valid() {
		return storage.ErrInvalidScanPlan
	}
	physical := model.TypeAny
	switch typ.ID {
	case 16, 17, 20, 21, 23, 25, 650, 700, 701, 774, 790, 829, 869, 1082, 1083, 1114, 1184, 1186, 1266, 1560, 1562, 3614, 3615:
		physical = primitiveStorage(typ.ID)
	case 18:
		if typ.Storage != model.TypeString || typ.Collation != 0 {
			return sqlError("42804", "invalid internal char key layout")
		}
		return nil
	case 19, 1042, 1043:
		physical = model.TypeString
	case 1700:
		physical = model.TypeNumeric
	case 3802:
		physical = model.TypeJSONB
	case 2950:
		physical = model.TypeUUID
	default:
		if _, known := LookupRangeFamily(typ.ID); known {
			physical = model.TypeBytes
		}
		if IsOIDType(typ.ID) {
			physical = model.TypeInt64
		}
	}
	if physical == model.TypeAny {
		return &MissingKeyImplementationError{Type: typ}
	}
	if typ.Storage != physical {
		return sqlError("42804", "SQL key type and storage do not match")
	}
	if physical != model.TypeString {
		if typ.Collation != 0 {
			return sqlError("42804", "nontext SQL key has a collation")
		}
		return nil
	}
	collation := typ.Collation
	if collation == 100 {
		collation = cmp.Or(e.defaultCollation, model.CollationID(950))
	}
	if collation != 950 && collation != 951 {
		return sqlError("42P22", "SQL text key requires a resolved C or POSIX collation")
	}
	return nil
}

func keyTextLength(ctx context.Context, typ model.Type, text string) (int, error) {
	end := len(text)
	if typ.ID == 1042 {
		for end > 0 && text[end-1] == ' ' {
			if end%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return 0, err
				}
			}
			end--
		}
	}
	return end, nil
}

func keyFieldSize(ctx context.Context, typ model.Type, value *model.Value, budget uint64) (uint64, error) {
	if value.Type != typ.Storage {
		return 0, sqlError("42804", "SQL key value storage does not match its type")
	}
	if budget < 1 {
		return 0, columnar.ErrBatchBudget
	}
	if value.IsNull {
		return 1, nil
	}
	if structuredByteKey(typ.ID) {
		return structuredByteKeySize(ctx, typ.ID, value.BytesVal, budget)
	}
	if typ.ID == 18 {
		if budget < 2 {
			return 0, columnar.ErrBatchBudget
		}
		_, err := charValueByte(value.StrVal)
		return 2, err
	}
	if typ.ID == 1266 {
		if budget < 13 {
			return 0, columnar.ErrBatchBudget
		}
		_, _, err := DecodeTimetz(value.BytesVal)
		return 13, err
	}
	size := uint64(1)
	switch typ.Storage {
	case model.TypeBoolean:
		if value.IntVal != 0 && value.IntVal != 1 {
			return 0, sqlError("42804", "invalid boolean physical value")
		}
		size++
	case model.TypeInt64:
		if IsOIDType(typ.ID) {
			if value.IntVal < 0 || value.IntVal > math.MaxUint32 {
				return 0, sqlError("22003", "OID out of range")
			}
		} else if err := integerRange(value.IntVal, typ.ID); err != nil {
			return 0, err
		}
		size += 8
	case model.TypeTimestamp:
		if typ.ID == 1082 && !validDateDatum(value.IntVal) {
			return 0, sqlError("22008", "date key out of range")
		}
		if typ.ID == 1083 && (value.IntVal < 0 || value.IntVal > intervalMicrosPerDay) {
			return 0, sqlError("22008", "time key out of range")
		}
		timestampType := typ.ID == 1114 || typ.ID == 1184
		if timestampType && finiteTimestamp(value.IntVal) && value.IntVal < timestampMinimum {
			return 0, sqlError("22008", "timestamp key out of range")
		}
		size += 8
	case model.TypeInterval:
		size += 16
	case model.TypeFloat64:
		if typ.ID == 700 && !math.IsNaN(value.FloatVal) && float64(float32(value.FloatVal)) != value.FloatVal {
			return 0, sqlError("42804", "float4 key is not rounded to binary32")
		}
		size += 8
	case model.TypeUUID:
		size += 16
		if size > budget {
			return 0, columnar.ErrBatchBudget
		}
		if _, err := parseUUIDText(ctx, value.StrVal); err != nil {
			return 0, err
		}
	case model.TypeString, model.TypeBytes:
		length := len(value.BytesVal)
		if typ.Storage == model.TypeString {
			var err error
			length, err = keyTextLength(ctx, typ, value.StrVal)
			if err != nil {
				return 0, err
			}
		}
		size += uint64(length) + 2
		if size > budget {
			return 0, columnar.ErrBatchBudget
		}
		if typ.Storage == model.TypeString {
			if _, err := textCharacters(ctx, value.StrVal[:length]); err != nil {
				return 0, err
			}
			return size, nil
		}
		for i, b := range value.BytesVal {
			if i%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return 0, err
				}
			}
			if b == 0 {
				size++
				if size > budget {
					return 0, columnar.ErrBatchBudget
				}
			}
		}
	}
	if size > budget {
		return 0, columnar.ErrBatchBudget
	}
	return size, nil
}

func appendKeyValue(ctx context.Context, key []byte, column *storage.KeyColumn, value *model.Value) ([]byte, error) {
	mask := byte(0)
	if column.Descending {
		mask = 0xff
	}
	if structuredByteKey(column.Type.ID) {
		return appendStructuredByteKey(ctx, key, column.Type.ID, value.BytesVal, mask)
	}
	if column.Type.ID == 18 {
		character, err := charValueByte(value.StrVal)
		if err != nil {
			return nil, err
		}
		return append(key, character^mask), nil
	}
	if column.Type.ID == 1266 {
		return appendTimetzKey(key, value.BytesVal, column.Descending)
	}
	if value.Type == model.TypeInterval {
		return appendIntervalKey(key, value.IntervalVal, column.Descending), nil
	}
	if value.Type == model.TypeUUID {
		data, err := parseUUIDText(ctx, value.StrVal)
		if err != nil {
			return nil, err
		}
		for _, b := range data {
			key = append(key, b^mask)
		}
		return key, nil
	}
	if value.Type == model.TypeString || value.Type == model.TypeBytes {
		length := len(value.BytesVal)
		if value.Type == model.TypeString {
			var err error
			length, err = keyTextLength(ctx, column.Type, value.StrVal)
			if err != nil {
				return nil, err
			}
		}
		for i := range length {
			if i%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			var b byte
			if value.Type == model.TypeString {
				b = value.StrVal[i]
			} else {
				b = value.BytesVal[i]
			}
			key = append(key, b^mask)
			if b == 0 {
				key = append(key, 0xff^mask)
			}
		}
		// 00 ff escapes zero; 00 00 ends the field, before every extension.
		return append(key, mask, mask), nil
	}
	if value.Type == model.TypeBoolean {
		return append(key, byte(value.IntVal)^mask), nil
	}
	bits := uint64(value.IntVal) ^ (uint64(1) << 63)
	if value.Type == model.TypeFloat64 {
		f := value.FloatVal
		if math.IsNaN(f) {
			bits = math.MaxUint64
		} else {
			f = cmp.Or(f, 0) // PostgreSQL equality coalesces both zero signs.
			bits = math.Float64bits(f)
			if bits>>63 != 0 {
				bits = ^bits
			} else {
				bits ^= uint64(1) << 63
			}
		}
	}
	if column.Descending {
		bits = ^bits
	}
	return binary.BigEndian.AppendUint64(key, bits), nil
}

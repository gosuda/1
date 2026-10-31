package builtin

import (
	"context"
	"math"
	"math/bits"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type rangeHash struct {
	family          RangeFamilyInfo
	subtype         Callable
	multi, extended bool
}

func resolveRangeHash(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	if err := ctx.Err(); err != nil {
		return Callable{}, err
	}
	extended := signature.ID == 3417 || signature.ID == 4279
	multi := signature.ID == 4278 || signature.ID == 4279
	if signature.ID != 3902 && signature.ID != 3417 && !multi {
		return Callable{}, sqlError("42883", "unknown range hash function")
	}
	count, output := 1, model.TypeID(23)
	if extended {
		count, output = 2, 20
	}
	if len(arguments) != count || int(signature.ArgumentCount) != count || result.Type.ID != output || result.Type.Storage != model.TypeInt64 || len(result.Children) != 0 {
		return Callable{}, sqlError("42804", "range hash signature mismatch")
	}
	family, err := rangeCallFamily(signature, arguments, result, options)
	if err != nil {
		return Callable{}, err
	}
	input := family.Range
	if multi {
		input = family.Multirange
	}
	if arguments[0].Type.ID != input || arguments[0].Type.Storage != model.TypeBytes || arguments[0].Type.Collation != 0 || len(arguments[0].Children) != 0 {
		return Callable{}, sqlError("42804", "range hash requires its declared range family")
	}
	if extended {
		seed := arguments[1]
		if seed.Type.ID != 20 || seed.Type.Storage != model.TypeInt64 || len(seed.Children) != 0 {
			return Callable{}, sqlError("42804", "range hash seed must be int8")
		}
	}
	for _, argument := range arguments {
		if err := validateConcreteLayout(ctx, argument); err != nil {
			return Callable{}, err
		}
	}
	if err := validateConcreteLayout(ctx, result); err != nil {
		return Callable{}, err
	}
	codec := rangeHash{family: family, multi: multi, extended: extended}
	header := uint64(unsafe.Sizeof(codec)) + 2*uint64(unsafe.Sizeof(uintptr(0)))
	if header > maximum {
		return Callable{}, sqlError("54000", "range hash metadata budget exceeded")
	}
	layout, known := resolvedTypeLayout(options, family.representation())
	if !known {
		return Callable{}, sqlError("42704", "range subtype does not exist")
	}
	layout.Type.Collation = family.Collation
	options.Collation = family.Collation
	codec.subtype, err = resolveNestedHashField(ctx, layout, extended, false, options, maximum-header)
	if err != nil {
		return Callable{}, err
	}
	return rangeTypedCallable(signature, arguments, result, codec.hash, header+codec.subtype.OwnedBytes()), nil
}

func rangeHashFlags(value rangeDatum) uint32 {
	if value.empty {
		return uint32(rangeEmptyFlag)
	}
	var flags byte
	if value.lower.infinite {
		flags |= rangeLowerInfiniteFlag
	} else if value.lower.inclusive {
		flags |= rangeLowerInclusiveFlag
	}
	if value.upper.infinite {
		flags |= rangeUpperInfiniteFlag
	} else if value.upper.inclusive {
		flags |= rangeUpperInclusiveFlag
	}
	return uint32(flags)
}

func (codec *rangeHash) component(ctx context.Context, value rangeDatum, seed model.Value, budget CallBudget) (uint64, error) {
	var hashes [2]uint64
	operands := [2]model.Value{{}, seed}
	count := 1
	if codec.extended {
		count = 2
	}
	if !value.empty {
		for i, bound := range [2]rangeBound{value.lower, value.upper} {
			if bound.infinite {
				continue
			}
			operands[0] = borrowRangeBoundValue(codec.family, bound)
			result, err := codec.subtype.Call(ctx, operands[:count], budget.WithOutput(0))
			if err != nil {
				return 0, err
			}
			wrongWidth := !codec.extended && (result.IntVal < math.MinInt32 || result.IntVal > math.MaxInt32)
			if result.IsNull || result.Type != model.TypeInt64 || wrongWidth {
				return 0, sqlError("XX000", "range subtype hash returned an invalid integer")
			}
			hashes[i] = uint64(result.IntVal)
		}
	}
	flags := rangeHashFlags(value)
	if !codec.extended {
		return uint64(bits.RotateLeft32(pgHashUint32(flags)^uint32(hashes[0]), 1) ^ uint32(hashes[1])), nil
	}
	hash := pgHashUint32Extended(flags, uint64(seed.IntVal)) ^ hashes[0]
	hash = uint64(bits.RotateLeft32(uint32(hash>>32), 1))<<32 | uint64(bits.RotateLeft32(uint32(hash), 1))
	return hash ^ hashes[1], nil
}

func (codec *rangeHash) hash(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	seed := model.Int64Val(0)
	if codec.extended {
		seed = arguments[1]
	}
	var hash uint64
	if !codec.multi {
		value, err := decodeRange(ctx, codec.family, arguments[0])
		if err != nil {
			return model.Value{}, err
		}
		hash, err = codec.component(ctx, value, seed, budget)
		if err != nil {
			return model.Value{}, err
		}
	} else {
		cursor, err := newMultirangeCursor(codec.family, arguments[0])
		if err != nil {
			return model.Value{}, err
		}
		hash = 1
		for {
			value, ok, err := cursor.next(ctx)
			if err != nil {
				return model.Value{}, err
			}
			if !ok {
				break
			}
			part, err := codec.component(ctx, value, seed, budget)
			if err != nil {
				return model.Value{}, err
			}
			hash = hash*31 + part
		}
	}
	if !codec.extended {
		return model.Int64Val(int64(int32(hash))), nil
	}
	return model.Int64Val(int64(hash)), nil
}

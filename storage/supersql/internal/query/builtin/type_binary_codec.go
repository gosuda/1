package builtin

import (
	"context"
	"encoding/binary"
	"math"
	"slices"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// TypeBinaryCodec is a snapshot-pinned unary PostgreSQL binary value codec.
// Input consumes the field payload without its outer int32 length. Output
// returns that payload as bytea. Neither side accepts a text fallback.
type TypeBinaryCodec struct {
	Input      TypeScalarCall
	Output     TypeScalarCall
	OwnedBytes uint64
}

type binaryTypeCodec struct {
	layout      model.TypeLayout
	options     ResolveOptions
	declared    *ResolvedType
	information InformationSchemaTypeBinding
	logical     bool
	children    []*binaryTypeCodec
	family      RangeFamilyInfo
	rangeOps    *rangeTypeOps
	send        Callable
	owned       uint64
}

func ResolveTypeBinaryCodec(ctx context.Context, layout model.TypeLayout, options ResolveOptions, maximum uint64) (TypeBinaryCodec, error) {
	if err := layout.Validate(); err != nil {
		return TypeBinaryCodec{}, err
	}
	ownedLayout, used, err := model.CopyTypeLayout(ctx, layout, maximum)
	if err != nil {
		return TypeBinaryCodec{}, err
	}
	codec, err := resolveBinaryTypeCodec(ctx, ownedLayout, options, maximum-used, 0)
	if err != nil {
		return TypeBinaryCodec{}, err
	}
	codec.owned += used
	return TypeBinaryCodec{
		OwnedBytes: codec.owned,
		Input: func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
			if len(values) != 1 || values[0].Type != model.TypeBytes || values[0].IsNull {
				return model.Value{}, sqlError("42804", "binary type input requires one non-null bytea value")
			}
			return codec.decode(ctx, values[0].BytesVal, budget, 0)
		},
		Output: func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
			if len(values) != 1 || values[0].Type != layout.Type.Storage || values[0].IsNull {
				return model.Value{}, sqlError("42804", "binary type output requires one non-null typed value")
			}
			return codec.encode(ctx, values[0], budget, 0)
		},
	}, nil
}

func resolveBinaryTypeCodec(ctx context.Context, layout model.TypeLayout, options ResolveOptions, maximum uint64, depth int) (*binaryTypeCodec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if depth > 64 {
		return nil, sqlError("54000", "binary type nesting limit exceeded")
	}
	metadata := uint64(unsafe.Sizeof(binaryTypeCodec{}))
	if metadata > maximum {
		return nil, sqlError("54000", "binary type codec metadata budget exceeded")
	}
	codec := &binaryTypeCodec{layout: layout, options: options, owned: metadata}
	if options.Types != nil {
		codec.declared, _ = options.Types.ResolveType(layout.Type.ID)
	}
	if codec.declared != nil && !sameArrayConstructionLayout(layout, codec.declared.Layout()) {
		return nil, sqlError("42804", "binary type layout differs from its declaration")
	}
	if codec.declared == nil && options.InformationSchemaTypes != nil {
		if binding, known := options.InformationSchemaTypes.ResolveInformationSchemaType(layout.Type.ID); known {
			if !sameArrayConstructionLayout(layout, binding.Layout) {
				return nil, sqlError("42804", "binary type layout differs from its logical catalog declaration")
			}
			codec.information, codec.logical = binding, true
		}
	}
	if codec.declared != nil && codec.declared.Kind() == model.TypeKindDomain {
		child, err := resolveBinaryTypeCodec(ctx, codec.declared.BaseLayout(), options, maximum-metadata, depth+1)
		if err != nil {
			return nil, err
		}
		codec.children, codec.owned = []*binaryTypeCodec{child}, metadata+child.owned
		return codec, nil
	}
	if codec.logical && codec.information.Kind == model.TypeKindDomain {
		child, err := resolveBinaryTypeCodec(ctx, codec.information.BaseLayout, options, maximum-metadata, depth+1)
		if err != nil {
			return nil, err
		}
		codec.children, codec.owned = []*binaryTypeCodec{child}, metadata+child.owned
		return codec, nil
	}
	if layout.Type.Storage == model.TypeArray {
		if _, _, err := arrayCodecLayout(layout, options); err != nil {
			return nil, err
		}
	}
	if layout.Type.Storage == model.TypeComposite && layout.Type.ID != 2249 && (codec.declared == nil || codec.declared.Kind() != model.TypeKindComposite || !sameArrayConstructionLayout(layout, codec.declared.Layout())) {
		return nil, sqlError("42804", "binary composite layout has no resolved declaration")
	}
	switch layout.Type.Storage {
	case model.TypeArray, model.TypeComposite:
		codec.children = make([]*binaryTypeCodec, len(layout.Children))
		codec.owned += uint64(len(layout.Children)) * uint64(unsafe.Sizeof((*binaryTypeCodec)(nil)))
		if codec.owned > maximum {
			return nil, sqlError("54000", "binary type codec metadata budget exceeded")
		}
		for i, childLayout := range layout.Children {
			child, err := resolveBinaryTypeCodec(ctx, childLayout, options, maximum-codec.owned, depth+1)
			if err != nil {
				return nil, err
			}
			codec.children[i] = child
			codec.owned += child.owned
		}
		return codec, nil
	}
	if family, known := LookupRangeFamily(layout.Type.ID); known {
		codec.family = family
	} else if codec.declared != nil {
		codec.family, _ = codec.declared.RangeFamily()
	}
	if codec.family.Range != 0 {
		subtype, ok := binarySubtypeLayout(options, codec.family)
		if !ok {
			return nil, sqlError("42704", "range subtype does not exist")
		}
		child, err := resolveBinaryTypeCodec(ctx, subtype, options, maximum-codec.owned, depth+1)
		if err != nil {
			return nil, err
		}
		codec.children = []*binaryTypeCodec{child}
		codec.owned += child.owned
		ops, used, err := newRangeTypeOps(ctx, codec.family, options, maximum-codec.owned)
		if err != nil {
			return nil, err
		}
		codec.rangeOps, codec.owned = &ops, codec.owned+used
		return codec, nil
	}
	if codec.declared != nil && codec.declared.Kind() == model.TypeKindEnum {
		return codec, nil
	}
	info, known := LookupType(layout.Type.ID)
	if !known || info.Send == 0 {
		return nil, sqlError("42804", "type has no resolved binary IO metadata")
	}
	result := model.TypeLayout{Type: model.Type{ID: 17, Modifier: -1, Storage: model.TypeBytes}}
	send, err := ResolveTyped(ctx, info.Send, []model.TypeLayout{layout}, result, options, maximum-codec.owned)
	if err != nil {
		return nil, err
	}
	codec.send = send
	codec.owned += send.OwnedBytes()
	return codec, nil
}

func binarySubtypeLayout(options ResolveOptions, family RangeFamilyInfo) (model.TypeLayout, bool) {
	if options.Types != nil {
		if resolved, ok := options.Types.ResolveType(family.Subtype); ok {
			return resolved.Layout(), true
		}
	}
	if options.InformationSchemaTypes != nil {
		if binding, ok := options.InformationSchemaTypes.ResolveInformationSchemaType(family.Subtype); ok {
			return binding.Layout, true
		}
	}
	storage := family.SubtypeStorage
	if storage == model.TypeAny {
		storage = primitiveStorage(family.Subtype)
	}
	if storage == model.TypeAny {
		return model.TypeLayout{}, false
	}
	return model.TypeLayout{Type: model.Type{ID: family.Subtype, Modifier: -1, Storage: storage, Collation: family.Collation}}, true
}

func (codec *binaryTypeCodec) encode(ctx context.Context, value model.Value, budget CallBudget, depth int) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if depth > 64 || value.Type != codec.layout.Type.Storage || value.IsNull {
		return model.Value{}, sqlError("42804", "value differs from binary type layout")
	}
	if codec.declared != nil && codec.declared.Kind() == model.TypeKindDomain {
		checked, err := codec.declared.validateDomain(ctx, value, budget)
		if err != nil {
			return model.Value{}, err
		}
		return codec.children[0].encode(ctx, checked, budget, depth+1)
	}
	if codec.logical && codec.information.Kind == model.TypeKindDomain {
		if err := validateInformationDomainValue(codec.information.Domain, value); err != nil {
			return model.Value{}, err
		}
		return codec.children[0].encode(ctx, value, budget, depth+1)
	}
	if codec.declared != nil && codec.declared.Kind() == model.TypeKindEnum {
		label, err := codec.declared.FormatEnumValue(value)
		if err != nil {
			return model.Value{}, err
		}
		return ownedBytes(ctx, budget.OutputBytes, []byte(label))
	}
	switch codec.layout.Type.Storage {
	case model.TypeArray:
		return codec.encodeArray(ctx, value, budget, depth+1)
	case model.TypeComposite:
		return codec.encodeComposite(ctx, value, budget, depth+1)
	}
	if codec.family.Range != 0 {
		return codec.encodeRangeFamily(ctx, value, budget, depth+1)
	}
	return codec.send.Call(ctx, []model.Value{value}, budget)
}

func (codec *binaryTypeCodec) decode(ctx context.Context, data []byte, budget CallBudget, depth int) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if depth > 64 || uint64(len(data)) > budget.MemoryBytes || int64(len(data)) > budget.OutputBytes {
		return model.Value{}, sqlError("54000", "binary value exceeds invocation budget")
	}
	if codec.declared != nil && codec.declared.Kind() == model.TypeKindDomain {
		value, err := codec.children[0].decode(ctx, data, budget, depth+1)
		if err != nil {
			return model.Value{}, err
		}
		return codec.declared.validateDomain(ctx, value, budget)
	}
	if codec.logical && codec.information.Kind == model.TypeKindDomain {
		value, err := codec.children[0].decode(ctx, data, budget, depth+1)
		if err != nil {
			return model.Value{}, err
		}
		if err := validateInformationDomainValue(codec.information.Domain, value); err != nil {
			return model.Value{}, err
		}
		return value, nil
	}
	if codec.declared != nil && codec.declared.Kind() == model.TypeKindEnum {
		return codec.declared.ParseEnumLabel(string(data))
	}
	switch codec.layout.Type.Storage {
	case model.TypeArray:
		return codec.decodeArray(ctx, data, budget, depth+1)
	case model.TypeComposite:
		return codec.decodeComposite(ctx, data, budget, depth+1)
	}
	if codec.family.Range != 0 {
		return codec.decodeRangeFamily(ctx, data, budget, depth+1)
	}
	return codec.decodePrimitive(ctx, data, budget)
}

func (codec *binaryTypeCodec) encodeArray(ctx context.Context, value model.Value, budget CallBudget, depth int) (model.Value, error) {
	if len(codec.children) != 1 || model.ValidateArrayShape(value.Dimensions, uint64(len(value.ArrayVal))) != nil || len(value.Dimensions) > math.MaxInt32 {
		return model.Value{}, sqlError("22P03", "invalid binary array value")
	}
	output := make([]byte, 0, min(int(budget.OutputBytes), 64))
	output = binary.BigEndian.AppendUint32(output, uint32(len(value.Dimensions)))
	hasNull := uint32(0)
	for _, item := range value.ArrayVal {
		if item.IsNull {
			hasNull = 1
			break
		}
	}
	output = binary.BigEndian.AppendUint32(output, hasNull)
	output = binary.BigEndian.AppendUint32(output, uint32(codec.layout.Children[0].Type.ID))
	for _, dimension := range value.Dimensions {
		output = binary.BigEndian.AppendUint32(output, uint32(dimension.Length))
		output = binary.BigEndian.AppendUint32(output, uint32(dimension.LowerBound))
	}
	var err error
	for i, item := range value.ArrayVal {
		if i&1023 == 0 {
			if err = ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		output, err = codec.appendField(ctx, output, codec.children[0], item, budget, depth)
		if err != nil {
			return model.Value{}, err
		}
	}
	return codec.binaryResult(ctx, output, budget)
}

func (codec *binaryTypeCodec) encodeComposite(ctx context.Context, value model.Value, budget CallBudget, depth int) (model.Value, error) {
	if len(value.ArrayVal) != len(codec.children) || len(codec.children) > math.MaxInt32 {
		return model.Value{}, sqlError("22P03", "invalid binary composite value")
	}
	output := binary.BigEndian.AppendUint32(nil, uint32(len(codec.children)))
	var err error
	for i, child := range codec.children {
		output = binary.BigEndian.AppendUint32(output, uint32(codec.layout.Children[i].Type.ID))
		output, err = codec.appendField(ctx, output, child, value.ArrayVal[i], budget, depth)
		if err != nil {
			return model.Value{}, err
		}
	}
	return codec.binaryResult(ctx, output, budget)
}

func (codec *binaryTypeCodec) appendField(ctx context.Context, output []byte, child *binaryTypeCodec, value model.Value, budget CallBudget, depth int) ([]byte, error) {
	if value.IsNull {
		return binary.BigEndian.AppendUint32(output, math.MaxUint32), nil
	}
	remaining, err := budget.After(uint64(len(output)))
	if err != nil {
		return nil, err
	}
	remaining.OutputBytes -= int64(len(output))
	encoded, err := child.encode(ctx, value, remaining, depth)
	if err != nil {
		return nil, err
	}
	if len(encoded.BytesVal) > math.MaxInt32 || uint64(len(output))+4+uint64(len(encoded.BytesVal)) > uint64(budget.OutputBytes) {
		return nil, sqlError("54000", "binary value exceeds invocation budget")
	}
	output = binary.BigEndian.AppendUint32(output, uint32(len(encoded.BytesVal)))
	return append(output, encoded.BytesVal...), nil
}

func (codec *binaryTypeCodec) decodeArray(ctx context.Context, data []byte, budget CallBudget, depth int) (model.Value, error) {
	reader := binaryValueReader{data: data}
	ndim, err := reader.int32()
	if err != nil || ndim < 0 || ndim > 64 {
		return model.Value{}, sqlError("22P03", "invalid binary array dimensions")
	}
	hasNull, err := reader.int32()
	if err != nil || hasNull < 0 || hasNull > 1 {
		return model.Value{}, sqlError("22P03", "invalid binary array null flag")
	}
	element, err := reader.uint32()
	if err != nil || model.TypeID(element) != codec.layout.Children[0].Type.ID {
		return model.Value{}, sqlError("42804", "binary array element type differs from declared layout")
	}
	dimensions := make([]model.ArrayDimension, int(ndim))
	count := uint64(1)
	if ndim == 0 {
		count = 0
	}
	for i := range dimensions {
		length, e1 := reader.int32()
		lower, e2 := reader.int32()
		if e1 != nil || e2 != nil || length < 0 || length > 0 && count > math.MaxInt32/uint64(length) {
			return model.Value{}, sqlError("22P03", "invalid binary array dimensions")
		}
		dimensions[i] = model.ArrayDimension{Length: length, LowerBound: lower}
		count *= uint64(length)
	}
	if count > uint64(int(^uint(0)>>1)) || count > budget.MemoryBytes/uint64(unsafe.Sizeof(model.Value{})) {
		return model.Value{}, sqlError("54000", "binary array exceeds invocation memory budget")
	}
	values := make([]model.Value, int(count))
	sawNull := false
	for i := range values {
		payload, null, err := reader.field()
		if err != nil {
			return model.Value{}, err
		}
		if null {
			values[i] = model.NullVal(codec.layout.Children[0].Type.Storage)
			sawNull = true
			continue
		}
		childBudget, err := budget.After(uint64(len(data)))
		if err != nil {
			return model.Value{}, err
		}
		values[i], err = codec.children[0].decode(ctx, payload, childBudget, depth)
		if err != nil {
			return model.Value{}, err
		}
	}
	if reader.remaining() != 0 || hasNull == 0 && sawNull || model.ValidateArrayShape(dimensions, count) != nil {
		return model.Value{}, sqlError("22P03", "invalid binary array value")
	}
	return model.Value{Type: model.TypeArray, ArrayVal: values, Dimensions: dimensions}, nil
}

func (codec *binaryTypeCodec) decodeComposite(ctx context.Context, data []byte, budget CallBudget, depth int) (model.Value, error) {
	reader := binaryValueReader{data: data}
	count, err := reader.int32()
	if err != nil || count < 0 || int(count) != len(codec.children) {
		return model.Value{}, sqlError("22P03", "binary composite field count differs from declared layout")
	}
	values := make([]model.Value, len(codec.children))
	for i, child := range codec.children {
		typeID, err := reader.uint32()
		if err != nil || model.TypeID(typeID) != codec.layout.Children[i].Type.ID {
			return model.Value{}, sqlError("42804", "binary composite field type differs from declared layout")
		}
		payload, null, err := reader.field()
		if err != nil {
			return model.Value{}, err
		}
		if null {
			values[i] = model.NullVal(codec.layout.Children[i].Type.Storage)
			continue
		}
		childBudget, err := budget.After(uint64(len(data)))
		if err != nil {
			return model.Value{}, err
		}
		values[i], err = child.decode(ctx, payload, childBudget, depth)
		if err != nil {
			return model.Value{}, err
		}
	}
	if reader.remaining() != 0 {
		return model.Value{}, sqlError("22P03", "trailing data in binary composite")
	}
	return model.Value{Type: model.TypeComposite, ArrayVal: values}, nil
}

func (codec *binaryTypeCodec) encodeRangeFamily(ctx context.Context, value model.Value, budget CallBudget, depth int) (model.Value, error) {
	if codec.layout.Type.ID == codec.family.Range {
		datum, err := decodeRange(ctx, codec.family, value)
		if err != nil {
			return model.Value{}, err
		}
		return codec.rangeWire(ctx, datum, budget, depth)
	}
	cursor, err := newMultirangeCursor(codec.family, value)
	if err != nil {
		return model.Value{}, err
	}
	output := binary.BigEndian.AppendUint32(nil, cursor.remaining)
	for {
		datum, more, err := cursor.next(ctx)
		if err != nil {
			return model.Value{}, err
		}
		if !more {
			break
		}
		remaining, err := budget.After(uint64(len(output)))
		if err != nil {
			return model.Value{}, err
		}
		wire, err := codec.rangeWire(ctx, datum, remaining, depth)
		if err != nil {
			return model.Value{}, err
		}
		if len(wire.BytesVal) > math.MaxInt32 {
			return model.Value{}, sqlError("54000", "binary range value exceeds protocol limit")
		}
		output = binary.BigEndian.AppendUint32(output, uint32(len(wire.BytesVal)))
		output = append(output, wire.BytesVal...)
	}
	return codec.binaryResult(ctx, output, budget)
}

func (codec *binaryTypeCodec) rangeWire(ctx context.Context, datum rangeDatum, budget CallBudget, depth int) (model.Value, error) {
	flags := byte(0)
	if datum.empty {
		return codec.binaryResult(ctx, []byte{rangeEmptyFlag}, budget)
	}
	if datum.lower.infinite { flags |= rangeLowerInfiniteFlag } else if datum.lower.inclusive { flags |= rangeLowerInclusiveFlag }
	if datum.upper.infinite { flags |= rangeUpperInfiniteFlag } else if datum.upper.inclusive { flags |= rangeUpperInclusiveFlag }
	output := []byte{flags}
	for _, bound := range []rangeBound{datum.lower, datum.upper} {
		if bound.infinite {
			continue
		}
		remaining, err := budget.After(uint64(len(output)))
		if err != nil {
			return model.Value{}, err
		}
		wire, err := codec.children[0].encode(ctx, borrowRangeBoundValue(codec.family, bound), remaining, depth)
		if err != nil {
			return model.Value{}, err
		}
		if len(wire.BytesVal) > math.MaxInt32 {
			return model.Value{}, sqlError("54000", "binary range bound exceeds protocol limit")
		}
		output = binary.BigEndian.AppendUint32(output, uint32(len(wire.BytesVal)))
		output = append(output, wire.BytesVal...)
	}
	return codec.binaryResult(ctx, output, budget)
}

func (codec *binaryTypeCodec) decodeRangeFamily(ctx context.Context, data []byte, budget CallBudget, depth int) (model.Value, error) {
	if codec.layout.Type.ID == codec.family.Range {
		datum, err := codec.rangeFromWire(ctx, data, budget, depth)
		if err != nil {
			return model.Value{}, err
		}
		datum, err = codec.rangeOps.canonical(ctx, datum, budget)
		if err != nil {
			return model.Value{}, err
		}
		return encodeRange(ctx, codec.family, datum, budget)
	}
	reader := binaryValueReader{data: data}
	count, err := reader.int32()
	if err != nil || count < 0 {
		return model.Value{}, sqlError("22P03", "invalid binary multirange count")
	}
	values := make([]rangeDatum, int(count))
	for i := range values {
		payload, null, err := reader.field()
		if err != nil || null {
			return model.Value{}, sqlError("22P03", "invalid binary multirange component")
		}
		values[i], err = codec.rangeFromWire(ctx, payload, budget, depth)
		if err != nil || values[i].empty {
			return model.Value{}, sqlError("22P03", "invalid binary multirange component")
		}
		values[i], err = codec.rangeOps.canonical(ctx, values[i], budget)
		if err != nil {
			return model.Value{}, err
		}
	}
	if reader.remaining() != 0 {
		return model.Value{}, sqlError("22P03", "trailing data in binary multirange")
	}
	return normalizeMultirange(ctx, codec.rangeOps, values, budget)
}

func (codec *binaryTypeCodec) rangeFromWire(ctx context.Context, data []byte, budget CallBudget, depth int) (rangeDatum, error) {
	result := rangeDatum{lower: rangeBound{side: rangeLower}, upper: rangeBound{side: rangeUpper}}
	if len(data) == 0 || data[0]&^byte(31) != 0 {
		return result, invalidRangeValue()
	}
	flags := data[0]
	if flags&rangeEmptyFlag != 0 {
		if flags != rangeEmptyFlag || len(data) != 1 {
			return result, invalidRangeValue()
		}
		result.empty = true
		return result, nil
	}
	result.lower.infinite, result.upper.infinite = flags&rangeLowerInfiniteFlag != 0, flags&rangeUpperInfiniteFlag != 0
	result.lower.inclusive, result.upper.inclusive = flags&rangeLowerInclusiveFlag != 0, flags&rangeUpperInclusiveFlag != 0
	if result.lower.infinite && result.lower.inclusive || result.upper.infinite && result.upper.inclusive {
		return result, invalidRangeValue()
	}
	reader := binaryValueReader{data: data[1:]}
	for _, bound := range []*rangeBound{&result.lower, &result.upper} {
		if bound.infinite {
			continue
		}
		payload, null, err := reader.field()
		if err != nil || null {
			return result, invalidRangeValue()
		}
		value, err := codec.children[0].decode(ctx, payload, budget, depth)
		if err != nil {
			return result, err
		}
		*bound, err = rangeBoundFromValue(codec.family, value, bound.side, bound.inclusive)
		if err != nil {
			return result, err
		}
	}
	if reader.remaining() != 0 {
		return result, invalidRangeValue()
	}
	return result, nil
}

func (codec *binaryTypeCodec) binaryResult(ctx context.Context, data []byte, budget CallBudget) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if uint64(len(data)) > budget.MemoryBytes || int64(len(data)) > budget.OutputBytes {
		return model.Value{}, sqlError("54000", "binary value exceeds invocation budget")
	}
	return model.BytesVal(data), nil
}

func (codec *binaryTypeCodec) decodePrimitive(ctx context.Context, data []byte, budget CallBudget) (model.Value, error) {
	id := codec.layout.Type.ID
	widthError := func(width int) error {
		if len(data) != width { return sqlError("22P03", "incorrect binary data length") }
		return nil
	}
	switch {
	case id == 16:
		if err := widthError(1); err != nil || data[0] > 1 { if err != nil { return model.Value{}, err }; return model.Value{}, sqlError("22P03", "invalid binary boolean") }
		return model.BoolVal(data[0] != 0), nil
	case id == 21:
		if err := widthError(2); err != nil { return model.Value{}, err }
		return model.Int64Val(int64(int16(binary.BigEndian.Uint16(data)))), nil
	case id == 23:
		if err := widthError(4); err != nil { return model.Value{}, err }
		return model.Int64Val(int64(int32(binary.BigEndian.Uint32(data)))), nil
	case id == 20 || id == 790:
		if err := widthError(8); err != nil { return model.Value{}, err }
		return model.Int64Val(int64(binary.BigEndian.Uint64(data))), nil
	case IsOIDType(id) || id == 28 || id == 29:
		if err := widthError(4); err != nil { return model.Value{}, err }
		return model.Int64Val(int64(binary.BigEndian.Uint32(data))), nil
	case id == 700:
		if err := widthError(4); err != nil { return model.Value{}, err }
		return model.Float64Val(float64(math.Float32frombits(binary.BigEndian.Uint32(data)))), nil
	case id == 701:
		if err := widthError(8); err != nil { return model.Value{}, err }
		return model.Float64Val(math.Float64frombits(binary.BigEndian.Uint64(data))), nil
	case id == 17:
		return model.BytesVal(slices.Clone(data)), nil
	case id == 18:
		if err := widthError(1); err != nil { return model.Value{}, err }
		return model.StringVal(string(data)), nil
	case id == 19:
		call := compileNameIO(Signature{ID: 38})
		return call(ctx, []model.Value{model.StringVal(string(data))}, budget)
	case id == 25 || id == 1042 || id == 1043 || id == 2275 || id == 705:
		text := string(data)
		characters, err := textCharacters(ctx, text)
		if err != nil {
			return model.Value{}, err
		}
		if id == 1042 || id == 1043 {
			return characterTypmod(ctx, text, characters, int64(codec.layout.Type.Modifier), false, id, budget.OutputBytes)
		}
		return ownedText(ctx, budget.OutputBytes, text)
	case id == 1082:
		if err := widthError(4); err != nil { return model.Value{}, err }
		return model.Value{Type: model.TypeTimestamp, IntVal: int64(int32(binary.BigEndian.Uint32(data)))}, nil
	case id == 1083 || id == 1114 || id == 1184:
		if err := widthError(8); err != nil { return model.Value{}, err }
		value := int64(binary.BigEndian.Uint64(data))
		if id == 1083 && (value < 0 || value > intervalMicrosPerDay) {
			return model.Value{}, sqlError("22008", "time out of range")
		}
		return model.Value{Type: model.TypeTimestamp, IntVal: value}, nil
	case id == 1266:
		if err := widthError(12); err != nil { return model.Value{}, err }
		if _, _, err := DecodeTimetz(data); err != nil { return model.Value{}, err }
		return model.BytesVal(slices.Clone(data)), nil
	case id == 1186:
		if err := widthError(16); err != nil { return model.Value{}, err }
		return model.Value{Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: int64(binary.BigEndian.Uint64(data)), Days: int32(binary.BigEndian.Uint32(data[8:])), Months: int32(binary.BigEndian.Uint32(data[12:]))}}, nil
	case id == 1700:
		return decodeNumericWire(ctx, data, codec.layout.Type.Modifier, budget)
	case id == 2950:
		if err := widthError(16); err != nil { return model.Value{}, err }
		return model.Value{Type: model.TypeUUID, StrVal: formatUUID(*(*[16]byte)(data))}, nil
	case id == 114:
		return jsonTextInput(ctx, []model.Value{model.StringVal(string(data))}, budget)
	case id == 3802:
		if len(data) == 0 || data[0] != 1 { return model.Value{}, sqlError("22P03", "unsupported jsonb binary version") }
		return jsonbInput(ctx, []model.Value{model.StringVal(string(data[1:]))}, budget)
	case id == 600 || id == 601 || id == 602 || id == 603 || id == 604 || id == 628 || id == 718:
		if err := ValidateGeometryValue(ctx, id, data); err != nil { return model.Value{}, err }
		return model.BytesVal(slices.Clone(data)), nil
	case id == 650 || id == 869:
		return decodeNetworkWire(ctx, data, id == 650, budget)
	case id == 829 || id == 774:
		width := 6; if id == 774 { width = 8 }
		if err := widthError(width); err != nil { return model.Value{}, err }
		return model.BytesVal(slices.Clone(data)), nil
	case id == 1560 || id == 1562:
		bits, err := decodeBitString(data)
		if err != nil { return model.Value{}, err }
		if codec.layout.Type.Modifier >= 0 && (id == 1560 && bits.bitLen != int(codec.layout.Type.Modifier) || id == 1562 && bits.bitLen > int(codec.layout.Type.Modifier)) {
			return model.Value{}, sqlError("22001", "bit string length exceeds type modifier")
		}
		return model.BytesVal(slices.Clone(data)), nil
	case id == 3614:
		if _, err := decodeTSVector(ctx, data); err != nil { return model.Value{}, err }
		return model.BytesVal(slices.Clone(data)), nil
	case id == 3615:
		if _, _, err := decodeTSQuery(ctx, data); err != nil { return model.Value{}, err }
		return model.BytesVal(slices.Clone(data)), nil
	case id == 4072:
		if _, err := DecodeJSONPathBinary(ctx, data, uint64(len(data))); err != nil { return model.Value{}, err }
		return model.BytesVal(slices.Clone(data)), nil
	default:
		return model.Value{}, sqlError("42804", "type binary receive is not implemented")
	}
}

type binaryValueReader struct {
	data   []byte
	offset int
}

func (reader *binaryValueReader) remaining() int { return len(reader.data) - reader.offset }
func (reader *binaryValueReader) uint32() (uint32, error) {
	if reader.remaining() < 4 { return 0, sqlError("22P03", "truncated binary value") }
	value := binary.BigEndian.Uint32(reader.data[reader.offset:])
	reader.offset += 4
	return value, nil
}
func (reader *binaryValueReader) int32() (int32, error) {
	value, err := reader.uint32()
	return int32(value), err
}
func (reader *binaryValueReader) field() ([]byte, bool, error) {
	size, err := reader.int32()
	if err != nil { return nil, false, err }
	if size == -1 { return nil, true, nil }
	if size < 0 || int64(size) > int64(reader.remaining()) { return nil, false, sqlError("22P03", "invalid binary field length") }
	value := reader.data[reader.offset:reader.offset+int(size)]
	reader.offset += int(size)
	return value, false, nil
}

func decodeNetworkWire(ctx context.Context, data []byte, cidr bool, budget CallBudget) (model.Value, error) {
	if len(data) < 4 || (data[0] != 2 && data[0] != 3) || data[2] > 1 || int(data[3])+4 != len(data) {
		return model.Value{}, sqlError("22P03", "invalid binary network value")
	}
	if cidr != (data[2] != 0) {
		return model.Value{}, sqlError("22P03", "binary network kind differs from declared type")
	}
	internal := make([]byte, len(data)-2)
	internal[0], internal[1] = data[0], data[1]
	copy(internal[2:], data[4:])
	if _, err := decodeNetwork(internal, cidr); err != nil { return model.Value{}, err }
	if uint64(len(internal)) > budget.MemoryBytes || int64(len(internal)) > budget.OutputBytes { return model.Value{}, sqlError("54000", "binary value exceeds invocation budget") }
	return model.BytesVal(internal), ctx.Err()
}

func decodeNumericWire(ctx context.Context, data []byte, modifier int32, budget CallBudget) (model.Value, error) {
	if len(data) < 8 || len(data)%2 != 0 {
		return model.Value{}, sqlError("22P03", "invalid binary numeric value")
	}
	ndigits := int(int16(binary.BigEndian.Uint16(data)))
	weight := int(int16(binary.BigEndian.Uint16(data[2:])))
	sign := binary.BigEndian.Uint16(data[4:])
	scale := int(binary.BigEndian.Uint16(data[6:]))
	if ndigits < 0 || ndigits > numericWorkDigits || len(data) != 8+2*ndigits || scale > numericMaxScale {
		return model.Value{}, sqlError("22P03", "invalid binary numeric value")
	}
	special := numericFinite
	switch sign {
	case 0x0000, 0x4000:
	case 0xC000:
		special = numericNaN
	case 0xD000:
		special = numericPositiveInfinity
	case 0xF000:
		special = numericNegativeInfinity
	default:
		return model.Value{}, sqlError("22P03", "invalid binary numeric sign")
	}
	if special != numericFinite {
		if ndigits != 0 || weight != 0 || scale != 0 { return model.Value{}, sqlError("22P03", "invalid binary numeric special value") }
		return numericValue(ctx, decimal{special: special}, budget.OutputBytes)
	}
	groups := make([]uint16, ndigits)
	for i := range groups {
		groups[i] = binary.BigEndian.Uint16(data[8+2*i:])
		if groups[i] > 9999 { return model.Value{}, sqlError("22P03", "invalid binary numeric digit") }
	}
	integerGroups := max(weight+1, 0)
	fractionGroups := (scale + 3) / 4
	var text strings.Builder
	if sign == 0x4000 { text.WriteByte('-') }
	if integerGroups == 0 {
		text.WriteByte('0')
	} else {
		for group := 0; group < integerGroups; group++ {
			value := uint16(0)
			index := group
			if index < len(groups) { value = groups[index] }
			if group == 0 { text.WriteString(formatBase10000(value, false)) } else { text.WriteString(formatBase10000(value, true)) }
		}
	}
	if scale > 0 {
		text.WriteByte('.')
		for group := 0; group < fractionGroups; group++ {
			index := integerGroups + group
			if weight < -1 { index += weight + 1 }
			value := uint16(0)
			if index >= 0 && index < len(groups) { value = groups[index] }
			text.WriteString(formatBase10000(value, true))
		}
		result := text.String()
		result = result[:len(result)-(fractionGroups*4-scale)]
		text.Reset(); text.WriteString(result)
	}
	workspace, err := budget.NewWorkspace(ctx)
	if err != nil { return model.Value{}, err }
	defer workspace.Close()
	value, err := parseNumeric(ctx, workspace, text.String())
	if err != nil { return model.Value{}, err }
	value, err = numericTypmod(ctx, workspace, value, int64(modifier))
	if err != nil { return model.Value{}, err }
	return numericValue(ctx, value, budget.OutputBytes)
}

func formatBase10000(value uint16, padded bool) string {
	var buffer [4]byte
	for i := 3; i >= 0; i-- { buffer[i] = byte(value%10) + '0'; value /= 10 }
	if padded { return string(buffer[:]) }
	start := 0
	for start < 3 && buffer[start] == '0' { start++ }
	return string(buffer[start:])
}

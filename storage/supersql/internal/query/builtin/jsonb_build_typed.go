package builtin

import (
	"context"
	"strconv"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type jsonbBuildCodec struct {
	signature Signature
	array     bool
	plans     []jsonbDatumPlan
}

type jsonbDatumPlan struct {
	typ        model.Type
	output     Callable
	children   []jsonbDatumPlan
	fieldNames []string
}

func resolveJSONBBuild(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64, array bool) (Callable, error) {
	element, err := validateVariadicLayouts(ctx, signature, arguments, result, maximum, 0, array, options)
	if err != nil {
		return Callable{}, err
	}
	jsonResult := signature.Result == 114 || signature.Result == 3802
	if result.Type.ID != signature.Result || result.Type.Storage != model.TypeJSONB || !jsonResult {
		return Callable{}, sqlError("42804", "JSON constructor requires its declared JSON result type")
	}
	count := len(arguments)
	if array {
		count = 1
	}
	metadata := uint64(unsafe.Sizeof(jsonbBuildCodec{})) + 2*uint64(unsafe.Sizeof(uintptr(0))) + uint64(len(arguments))*uint64(unsafe.Sizeof(model.Type{}))
	if metadata > maximum {
		return Callable{}, sqlError("54000", "JSONB constructor metadata budget exceeded")
	}
	remaining := maximum - metadata
	planBytes := uint64(count) * uint64(unsafe.Sizeof(jsonbDatumPlan{}))
	if planBytes > remaining {
		return Callable{}, sqlError("54000", "JSONB constructor metadata budget exceeded")
	}
	remaining -= planBytes
	codec := &jsonbBuildCodec{signature: signature, array: array, plans: make([]jsonbDatumPlan, count)}
	types := make([]model.Type, len(arguments))
	for i, argument := range arguments {
		types[i] = argument.Type
	}
	for i := range codec.plans {
		layout := element
		if !array {
			layout = arguments[i]
		}
		if err := codec.plans[i].resolve(ctx, layout, options, &remaining); err != nil {
			return Callable{}, err
		}
	}
	return Callable{signature: signature, resultStorage: model.TypeJSONB, argumentTypes: types, metadataBytes: maximum - remaining, kernel: codec.call}, nil
}

func (plan *jsonbDatumPlan) resolve(ctx context.Context, layout model.TypeLayout, options ResolveOptions, remaining *uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	resolvedLayout := layout
	settled := false
	for depth := 0; depth < 64; depth++ {
		if info, known := LookupType(resolvedLayout.Type.ID); known && info.Base != 0 {
			resolvedLayout.Type.ID = info.Base
			continue
		}
		if options.Types != nil {
			if resolved, known := options.Types.ResolveType(resolvedLayout.Type.ID); known && resolved.Kind() == model.TypeKindDomain {
				if !sameArrayConstructionLayout(resolvedLayout, resolved.Layout()) {
					return sqlError("42804", "JSONB domain argument differs from its declaration")
				}
				resolvedLayout = resolved.BaseLayout()
				continue
			}
		}
		if options.InformationSchemaTypes != nil {
			if binding, known := options.InformationSchemaTypes.ResolveInformationSchemaType(resolvedLayout.Type.ID); known && binding.Kind == model.TypeKindDomain {
				if !sameArrayConstructionLayout(resolvedLayout, binding.Layout) {
					return sqlError("42804", "JSONB domain argument differs from its declaration")
				}
				resolvedLayout = binding.BaseLayout
				continue
			}
		}
		settled = true
		break
	}
	if !settled {
		return sqlError("54000", "JSONB domain base chain exceeds limit")
	}
	plan.typ = resolvedLayout.Type
	if resolvedLayout.Type.Storage == model.TypeArray || resolvedLayout.Type.Storage == model.TypeComposite {
		return plan.resolveNested(ctx, resolvedLayout, options, remaining)
	}
	switch resolvedLayout.Type.ID {
	case 16, 114, 3802:
		return nil
	case 1082, 1114, 1184:
		options.DateStyle = DateISO
	}
	info, known := lookupResolvedTypeIO(options, resolvedLayout)
	if !known || info.Output == 0 {
		return sqlError("42804", "JSONB argument has no resolved output metadata")
	}
	cstring := model.TypeLayout{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}}
	output, err := ResolveTyped(ctx, info.Output, []model.TypeLayout{resolvedLayout}, cstring, options, *remaining)
	if err != nil {
		return err
	}
	if output.OwnedBytes() > *remaining {
		return sqlError("54000", "JSONB constructor metadata budget exceeded")
	}
	*remaining -= output.OwnedBytes()
	plan.output = output
	return nil
}

func (plan *jsonbDatumPlan) emit(writer *jsonbBuildWriter, argument []model.Value, key bool) error {
	if err := writer.ctx.Err(); err != nil {
		return err
	}
	value := argument[0]
	if value.IsNull {
		if key {
			return sqlError("22004", "null value not allowed for object key")
		}
		return writer.raw("null")
	}
	if value.Type != plan.typ.Storage {
		return sqlError("42804", "JSONB argument storage differs from its SQL type")
	}
	container := plan.typ.Storage == model.TypeArray || plan.typ.Storage == model.TypeComposite
	json := plan.typ.ID == 114 || plan.typ.ID == 3802
	if key && (container || json) {
		return sqlError("22023", "key value must be scalar, not array, composite, or json")
	}
	if plan.typ.Storage == model.TypeArray {
		if err := model.ValidateArrayShape(value.Dimensions, uint64(len(value.ArrayVal))); err != nil {
			return sqlError("2202E", "invalid array dimensions")
		}
		if len(value.Dimensions) == 0 {
			return writer.raw("[]")
		}
		position := 0
		return plan.emitArray(writer, value, 0, &position)
	}
	if plan.typ.Storage == model.TypeComposite {
		if len(value.ArrayVal) != len(plan.children) {
			return sqlError("42804", "JSONB composite value differs from its descriptor")
		}
		if err := writer.raw("{"); err != nil {
			return err
		}
		for i := range plan.children {
			if i != 0 {
				separator := ","
				if writer.lineFeeds == plan {
					separator = ",\n "
				}
				if err := writer.raw(separator); err != nil {
					return err
				}
			}
			if err := writer.quoted(plan.fieldNames[i]); err != nil {
				return err
			}
			if err := writer.raw(":"); err != nil {
				return err
			}
			if err := plan.children[i].emit(writer, value.ArrayVal[i:i+1], false); err != nil {
				return err
			}
		}
		return writer.raw("}")
	}
	if plan.typ.ID == 16 {
		if value.IntVal != 0 && value.IntVal != 1 {
			return sqlError("42804", "invalid boolean physical value")
		}
		text := "false"
		if value.IntVal != 0 {
			text = "true"
		}
		if key {
			return writer.quoted(text)
		}
		return writer.raw(text)
	}
	if json {
		if plan.typ.ID == 114 && !writer.canonicalize {
			if _, _, err := scanJSONText(writer.ctx, value.StrVal); err != nil {
				return err
			}
			return writer.raw(value.StrVal)
		}
		text, err := CanonicalJSONB(writer.ctx, value.StrVal, writer.budget)
		if err != nil {
			return err
		}
		if writer.budget.Allocator != nil {
			claim, err := writer.budget.Allocator.Reserve(uint64(len(text)))
			if err != nil {
				return err
			}
			defer claim.Release()
		}
		return writer.raw(text)
	}
	limit := min(writer.budget.OutputBytes, int64(min(writer.budget.MemoryBytes, uint64(^uint64(0)>>1))))
	if writer.budget.Allocator != nil {
		limit = min(limit, int64(min(writer.budget.Allocator.AvailableBytes(), uint64(^uint64(0)>>1))))
	}
	text, err := plan.output.Call(writer.ctx, argument, writer.budget.WithOutput(limit))
	if err != nil {
		return err
	}
	if uint64(len(text.StrVal)) > writer.budget.MemoryBytes {
		return sqlError("54000", "builtin invocation memory budget exceeded")
	}
	if writer.budget.Allocator != nil {
		claim, err := writer.budget.Allocator.Reserve(uint64(len(text.StrVal)))
		if err != nil {
			return err
		}
		defer claim.Release()
	}
	if text.IsNull || text.Type != model.TypeString {
		return sqlError("XX000", "JSONB argument output did not return cstring")
	}
	switch plan.typ.ID {
	case 20, 21, 23, 700, 701, 1700:
		if !key && !strings.ContainsAny(text.StrVal, "Nn") {
			return writer.raw(text.StrVal)
		}
	case 1114, 1184:
		// XSD differs from ISO output by its date/time separator and mandatory
		// timezone minutes. The fixed-size buffer covers PostgreSQL timestamps.
		var scratch [timestamptzOutputMaxBytes + 3]byte
		if len(text.StrVal) > len(scratch)-3 {
			return sqlError("22008", "timestamp out of range")
		}
		length := copy(scratch[:], text.StrVal)
		for i := range length {
			if scratch[i] == ' ' && i+1 < length && scratch[i+1] >= '0' && scratch[i+1] <= '9' {
				scratch[i] = 'T'
				break
			}
		}
		if plan.typ.ID == 1184 {
			end := length
			if strings.HasSuffix(text.StrVal, " BC") {
				end -= 3
			}
			if end >= 3 && (scratch[end-3] == '+' || scratch[end-3] == '-') {
				copy(scratch[end+3:], scratch[end:length])
				copy(scratch[end:end+3], ":00")
				length += 3
			}
		}
		return writer.quoted(string(scratch[:length]))
	}
	return writer.quoted(text.StrVal)
}

func (plan *jsonbDatumPlan) emitArray(writer *jsonbBuildWriter, value model.Value, dimension int, position *int) error {
	if err := writer.raw("["); err != nil {
		return err
	}
	for i := range int(value.Dimensions[dimension].Length) {
		if i != 0 {
			separator := ","
			if writer.lineFeeds == plan && dimension == 0 {
				separator = ",\n "
			}
			if err := writer.raw(separator); err != nil {
				return err
			}
		}
		if dimension+1 < len(value.Dimensions) {
			if err := plan.emitArray(writer, value, dimension+1, position); err != nil {
				return err
			}
		} else {
			if err := plan.children[0].emit(writer, value.ArrayVal[*position:*position+1], false); err != nil {
				return err
			}
			*position++
		}
	}
	return writer.raw("]")
}

func (plan *jsonbDatumPlan) resolveNested(ctx context.Context, layout model.TypeLayout, options ResolveOptions, remaining *uint64) error {
	if layout.Type.Storage == model.TypeArray {
		if _, _, err := arrayCodecLayout(layout, options); err != nil {
			return err
		}
	} else if _, builtinType := LookupType(layout.Type.ID); !builtinType {
		if options.Types == nil {
			return sqlError("42804", "JSONB composite argument has no resolved declaration")
		}
		resolved, declared := options.Types.ResolveType(layout.Type.ID)
		if !declared || resolved.Kind() != model.TypeKindComposite || !sameArrayConstructionLayout(layout, resolved.Layout()) {
			return sqlError("42804", "JSONB composite argument differs from its declaration")
		}
	}
	bytes := uint64(len(layout.Children)) * uint64(unsafe.Sizeof(jsonbDatumPlan{}))
	if bytes > *remaining {
		return sqlError("54000", "JSONB constructor metadata budget exceeded")
	}
	if layout.Type.Storage == model.TypeComposite {
		names, err := jsonbCompositeNameBytes(ctx, layout, *remaining-bytes)
		if err != nil {
			return err
		}
		bytes += names
	}
	*remaining -= bytes
	plan.children = make([]jsonbDatumPlan, len(layout.Children))
	if layout.Type.Storage == model.TypeComposite {
		plan.fieldNames = make([]string, len(layout.Children))
		for i := range plan.fieldNames {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(layout.FieldNames) == 0 {
				var scratch [24]byte
				scratch[0] = 'f'
				plan.fieldNames[i] = string(strconv.AppendInt(scratch[:1], int64(i+1), 10))
			} else {
				plan.fieldNames[i] = strings.Clone(layout.FieldNames[i])
			}
		}
	}
	for i, child := range layout.Children {
		if err := plan.children[i].resolve(ctx, child, options, remaining); err != nil {
			return err
		}
	}
	return nil
}

func jsonbCompositeNameBytes(ctx context.Context, layout model.TypeLayout, maximum uint64) (uint64, error) {
	anonymous := layout.Type.ID == 2249 && len(layout.FieldNames) == 0
	if !anonymous && len(layout.FieldNames) != len(layout.Children) {
		return 0, sqlError("42804", "JSONB composite conversion requires attribute names")
	}
	bytes := uint64(len(layout.Children)) * uint64(unsafe.Sizeof(""))
	if bytes > maximum {
		return 0, sqlError("54000", "JSONB constructor metadata budget exceeded")
	}
	for i := range layout.Children {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var size uint64
		if anonymous {
			size = jsonbAnonymousNameBytes(i + 1)
		} else {
			size = uint64(len(layout.FieldNames[i]))
		}
		if size > maximum-bytes {
			return 0, sqlError("54000", "JSONB constructor metadata budget exceeded")
		}
		bytes += size
	}
	return bytes, nil
}

func jsonbAnonymousNameBytes(index int) uint64 {
	size := uint64(2)
	for index >= 10 {
		size++
		index /= 10
	}
	return size
}

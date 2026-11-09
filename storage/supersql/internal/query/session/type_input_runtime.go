package session

import (
	"context"
	"errors"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type typeInputRuntime struct {
	relations *relationRuntime
	options   builtin.ResolveOptions
	temporal  model.TemporalRuntime
	native    model.NativeScalarRuntime
}

func newTypeInputRuntime(relations model.RelationRuntime, options builtin.ResolveOptions, temporal model.TemporalRuntime, native model.NativeScalarRuntime) model.TypeInputRuntime {
	runtime, _ := relations.(*relationRuntime)
	if runtime == nil {
		return nil
	}
	native.TypeInput = nil
	return &typeInputRuntime{relations: runtime, options: options, temporal: temporal, native: native}
}

func (runtime *typeInputRuntime) ValidateTypeInput(ctx context.Context, input, typeName string, maxOutput int64, maxMemory uint64) (*model.Error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if uint64(len(typeName)) > maxMemory {
		return nil, &model.Error{Code: "54000", Message: "type name exceeds input validation memory budget", Cause: model.ErrValueBudget}
	}
	layout, info, modifiers, err := runtime.resolveTypeName(ctx, typeName, maxMemory)
	if err != nil {
		return nil, err
	}
	native := runtime.native
	native.TypeInput = runtime
	if len(modifiers) != 0 {
		if info.ModifierInput == 0 {
			return nil, &model.Error{Code: "42601", Message: "type modifier is not allowed for type " + info.Name}
		}
		modifier, err := builtin.Resolve(info.ModifierInput, runtime.options)
		if err != nil {
			return nil, err
		}
		values := make([]model.Value, len(modifiers))
		for i, text := range modifiers {
			values[i] = model.StringVal(text)
		}
		argument := model.Value{Type: model.TypeArray, ArrayVal: values, Dimensions: []model.ArrayDimension{{Length: int32(len(values)), LowerBound: 1}}}
		result, err := modifier.Call(ctx, []model.Value{argument}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: maxMemory, Temporal: runtime.temporal, Native: native})
		if err != nil {
			return nil, err
		}
		if result.IsNull || result.IntVal < -1<<31 || result.IntVal > 1<<31-1 {
			return nil, &model.Error{Code: "XX000", Message: "type modifier input returned an invalid modifier"}
		}
		layout.Type.Modifier = int32(result.IntVal)
		if layout.Type.Storage == model.TypeArray {
			if len(layout.Children) != 1 {
				return nil, model.ErrInvalidTypeLayout
			}
			layout.Children[0].Type.Modifier = int32(result.IntVal)
		}
	}
	codec, err := builtin.ResolveTypeTextCodec(ctx, layout, runtime.options, maxMemory)
	if err != nil {
		return nil, err
	}
	_, err = codec.Input(ctx, []model.Value{model.StringVal(input)}, builtin.CallBudget{OutputBytes: maxOutput, MemoryBytes: maxMemory, Temporal: runtime.temporal, Native: native})
	if err == nil {
		return nil, nil
	}
	state, soft := softTypeInputError(err)
	if !soft {
		return nil, err
	}
	copy := *state
	copy.Cause = nil
	return &copy, nil
}

func softTypeInputError(err error) (*model.Error, bool) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, model.ErrValueBudget) {
		return nil, false
	}
	var state *model.Error
	if !errors.As(err, &state) || state.Code == "" {
		return nil, false
	}
	soft := len(state.Code) == 5 && (strings.HasPrefix(state.Code, "22") || state.Code == "23502" || state.Code == "23514")
	return state, soft
}

func (runtime *typeInputRuntime) resolveTypeName(ctx context.Context, text string, maximum uint64) (model.TypeLayout, builtin.TypeInfo, []string, error) {
	base, modifiers, array, err := splitInputTypeName(text)
	if err != nil {
		return model.TypeLayout{}, builtin.TypeInfo{}, nil, err
	}
	base = canonicalInputTypeName(base)
	parts, err := builtin.ParseQualifiedName(ctx, base, maximum)
	if err != nil || len(parts) == 0 || len(parts) > 2 {
		return model.TypeLayout{}, builtin.TypeInfo{}, nil, &model.Error{Code: "42601", Message: "invalid type name", Cause: err}
	}
	var info builtin.TypeInfo
	var declaredArray model.TypeLayout
	var layout model.TypeLayout
	found := false
	builtinAllowed := len(parts) == 1 || parts[0] == "pg_catalog"
	if builtinAllowed {
		for candidate := range builtin.TypesNamed(parts[len(parts)-1]) {
			info, found = candidate, true
			break
		}
		if found {
			layout, found = builtin.BuiltinTypeLayout(info.ID, -1)
		}
	}
	if !found {
		layout, declaredArray, found, err = runtime.resolveDeclaredType(ctx, parts)
		if err != nil {
			return model.TypeLayout{}, builtin.TypeInfo{}, nil, err
		}
		if found {
			info, _ = builtin.LookupType(layout.Type.ID)
		}
	}
	if !found {
		return model.TypeLayout{}, builtin.TypeInfo{}, nil, &model.Error{Code: "42704", Message: "type does not exist"}
	}
	if array {
		if declaredArray.Type.ID != 0 {
			layout = declaredArray
		} else {
			arrayInfo, known := builtin.LookupType(info.Array)
			if info.Array == 0 || !known {
				return model.TypeLayout{}, builtin.TypeInfo{}, nil, &model.Error{Code: "42704", Message: "array type does not exist"}
			}
			layout = model.TypeLayout{Type: model.Type{ID: arrayInfo.ID, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{layout}}
		}
	}
	return layout, info, modifiers, nil
}

func (runtime *typeInputRuntime) resolveDeclaredType(ctx context.Context, parts []string) (model.TypeLayout, model.TypeLayout, bool, error) {
	namespaces := runtime.relations.path
	if len(parts) == 2 {
		namespaces = parts[:1]
	}
	for _, namespace := range namespaces {
		schema, err := runtime.relations.catalog.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: namespace})
		if errors.Is(err, catalog.ErrUnknownObject) {
			continue
		}
		if err != nil {
			return model.TypeLayout{}, model.TypeLayout{}, false, err
		}
		published, err := runtime.relations.catalog.Resolve(ctx, model.NamespaceName{Parent: schema.Descriptor.ID(), Kind: model.ObjectType, Name: parts[len(parts)-1]})
		if errors.Is(err, catalog.ErrUnknownObject) {
			continue
		}
		if err != nil {
			return model.TypeLayout{}, model.TypeLayout{}, false, err
		}
		if runtime.options.Types == nil {
			return model.TypeLayout{}, model.TypeLayout{}, false, &model.Error{Code: "55000", Message: "declared type runtime is unavailable"}
		}
		resolved, known := runtime.options.Types.ResolveType(model.TypeID(published.Descriptor.OID()))
		if !known {
			return model.TypeLayout{}, model.TypeLayout{}, false, &model.Error{Code: "XX000", Message: "declared type is absent from the pinned type runtime"}
		}
		layout := resolved.Layout()
		var arrayLayout model.TypeLayout
		metadata, declared := published.Descriptor.DeclaredType()
		if declared && metadata.Array != 0 {
			array, err := runtime.relations.catalog.Descriptor(ctx, metadata.Array)
			if err != nil {
				return model.TypeLayout{}, model.TypeLayout{}, false, err
			}
			resolvedArray, known := runtime.options.Types.ResolveType(model.TypeID(array.Descriptor.OID()))
			if !known {
				return model.TypeLayout{}, model.TypeLayout{}, false, &model.Error{Code: "XX000", Message: "declared array type is absent from the pinned type runtime"}
			}
			arrayLayout = resolvedArray.Layout()
		}
		return layout, arrayLayout, true, nil
	}
	return model.TypeLayout{}, model.TypeLayout{}, false, nil
}

func splitInputTypeName(text string) (string, []string, bool, error) {
	text = strings.TrimSpace(text)
	array := false
	if strings.HasSuffix(text, "[]") {
		array = true
		text = strings.TrimSpace(text[:len(text)-2])
	}
	var modifiers []string
	if strings.HasSuffix(text, ")") {
		open := strings.LastIndexByte(text, '(')
		if open < 0 {
			return "", nil, false, &model.Error{Code: "42601", Message: "invalid type modifier"}
		}
		for _, value := range strings.Split(text[open+1:len(text)-1], ",") {
			value = strings.TrimSpace(value)
			if value == "" {
				return "", nil, false, &model.Error{Code: "42601", Message: "invalid type modifier"}
			}
			modifiers = append(modifiers, value)
		}
		text = strings.TrimSpace(text[:open])
	}
	return text, modifiers, array, nil
}

func canonicalInputTypeName(name string) string {
	switch strings.ToLower(strings.Join(strings.Fields(name), " ")) {
	case "smallint":
		return "int2"
	case "integer", "int":
		return "int4"
	case "bigint":
		return "int8"
	case "real":
		return "float4"
	case "double precision":
		return "float8"
	case "decimal":
		return "numeric"
	case "boolean":
		return "bool"
	case "character varying", "varchar":
		return "varchar"
	case "character", "char":
		return "bpchar"
	case "timestamp without time zone":
		return "timestamp"
	case "timestamp with time zone":
		return "timestamptz"
	case "time without time zone":
		return "time"
	case "time with time zone":
		return "timetz"
	default:
		return name
	}
}

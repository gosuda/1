package model

import (
	"context"
	"strings"
	"unsafe"
)

// CopyTypeLayout owns visible child layouts and field names without retaining
// spare capacity. The byte count excludes the returned root header.
func CopyTypeLayout(ctx context.Context, source TypeLayout, maxBytes uint64) (TypeLayout, uint64, error) {
	var used uint64
	result, err := copyTypeLayout(ctx, source, maxBytes, &used, 0, false, false)
	if err != nil {
		return TypeLayout{}, 0, err
	}
	return result, used, nil
}

// CopyDeclaredTypeLayout is CopyTypeLayout for declared signature positions —
// routine parameters and results — where a pseudo-type identity is a legal
// childless marker resolved per call rather than a physical layout.
func CopyDeclaredTypeLayout(ctx context.Context, source TypeLayout, maxBytes uint64) (TypeLayout, uint64, error) {
	var used uint64
	result, err := copyTypeLayout(ctx, source, maxBytes, &used, 0, false, true)
	if err != nil {
		return TypeLayout{}, 0, err
	}
	return result, used, nil
}

// CopyPhysicalTypeLayout owns child headers without retaining SQL field names.
func CopyPhysicalTypeLayout(ctx context.Context, source TypeLayout, maxBytes uint64) (TypeLayout, uint64, error) {
	var used uint64
	result, err := copyTypeLayout(ctx, source, maxBytes, &used, 0, true, false)
	if err != nil {
		return TypeLayout{}, 0, err
	}
	return result, used, nil
}

func copyTypeLayout(ctx context.Context, source TypeLayout, maximum uint64, used *uint64, depth int, physical, declared bool) (TypeLayout, error) {
	if err := ctx.Err(); err != nil {
		return TypeLayout{}, err
	}
	if physical {
		source.FieldNames = nil
	}
	if declared && IsPseudoTypeID(source.Type.ID) {
		if err := source.validateDeclaredMarker(); err != nil {
			return TypeLayout{}, err
		}
	} else if err := source.validateNode(depth); err != nil {
		return TypeLayout{}, err
	}
	result := TypeLayout{Type: source.Type}
	nameHeader := uint64(unsafe.Sizeof(""))
	if uint64(len(source.FieldNames)) > (maximum-*used)/nameHeader {
		return TypeLayout{}, ErrValueBudget
	}
	*used += uint64(len(source.FieldNames)) * nameHeader
	if len(source.FieldNames) != 0 {
		result.FieldNames = make([]string, len(source.FieldNames))
	}
	for i, name := range source.FieldNames {
		if err := ctx.Err(); err != nil {
			return TypeLayout{}, err
		}
		if uint64(len(name)) > maximum-*used {
			return TypeLayout{}, ErrValueBudget
		}
		*used += uint64(len(name))
		if strings.IndexByte(name, 0) >= 0 {
			return TypeLayout{}, ErrInvalidTypeLayout
		}
		result.FieldNames[i] = strings.Clone(name)
	}
	if len(source.Children) == 0 {
		return result, nil
	}
	size := uint64(unsafe.Sizeof(TypeLayout{}))
	if uint64(len(source.Children)) > (maximum-*used)/size {
		return TypeLayout{}, ErrValueBudget
	}
	*used += uint64(len(source.Children)) * size
	result.Children = make([]TypeLayout, len(source.Children))
	for i, child := range source.Children {
		copied, err := copyTypeLayout(ctx, child, maximum, used, depth+1, physical, declared)
		if err != nil {
			return TypeLayout{}, err
		}
		result.Children[i] = copied
	}
	return result, nil
}

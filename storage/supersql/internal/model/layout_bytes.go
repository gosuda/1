package model

import (
	"context"
	"strings"
	"unsafe"
)

// TypeLayoutBytes validates and measures visible child headers and field names
// without retaining them. Like CopyTypeLayout, it excludes the caller-owned root.
func TypeLayoutBytes(ctx context.Context, source TypeLayout, maxBytes uint64) (uint64, error) {
	return typeLayoutBytes(ctx, source, maxBytes, 0, false)
}

// PhysicalTypeLayoutBytes measures child headers, excluding SQL field names.
func PhysicalTypeLayoutBytes(ctx context.Context, source TypeLayout, maxBytes uint64) (uint64, error) {
	return typeLayoutBytes(ctx, source, maxBytes, 0, true)
}

func typeLayoutBytes(ctx context.Context, source TypeLayout, maximum uint64, depth int, physical bool) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if physical {
		source.FieldNames = nil
	}
	if err := source.validateNode(depth); err != nil {
		return 0, err
	}
	header := uint64(unsafe.Sizeof(TypeLayout{}))
	if uint64(len(source.Children)) > maximum/header {
		return 0, ErrValueBudget
	}
	used := uint64(len(source.Children)) * header
	nameHeader := uint64(unsafe.Sizeof(""))
	if uint64(len(source.FieldNames)) > (maximum-used)/nameHeader {
		return 0, ErrValueBudget
	}
	used += uint64(len(source.FieldNames)) * nameHeader
	for _, name := range source.FieldNames {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if uint64(len(name)) > maximum-used {
			return 0, ErrValueBudget
		}
		used += uint64(len(name))
		if strings.IndexByte(name, 0) >= 0 {
			return 0, ErrInvalidTypeLayout
		}
	}
	for _, child := range source.Children {
		size, err := typeLayoutBytes(ctx, child, maximum-used, depth+1, physical)
		if err != nil {
			return 0, err
		}
		used += size
	}
	return used, nil
}

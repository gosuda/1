package model_test

import (
	"context"
	"errors"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestTypeLayoutBytesMatchesOwnedVisibleChildren(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	children := make([]model.TypeLayout, 2, 100)
	children[0] = integer
	children[1] = model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{integer}}
	layout := model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: -1}, Children: children}
	want := 3 * uint64(unsafe.Sizeof(model.TypeLayout{}))
	size, err := model.TypeLayoutBytes(t.Context(), layout, want)
	if err != nil || size != want {
		t.Fatalf("layout bytes=%d, %v; want %d", size, err, want)
	}
	_, copied, err := model.CopyTypeLayout(t.Context(), layout, want)
	if err != nil || copied != size {
		t.Fatalf("copy admission=%d, %v; measured=%d", copied, err, size)
	}
	_, err = model.TypeLayoutBytes(t.Context(), layout, want-1)
	if !errors.Is(err, model.ErrValueBudget) {
		t.Fatalf("undersized layout admission=%v", err)
	}
}

func TestTypeLayoutBytesRejectsInvalidOrCanceledLayouts(t *testing.T) {
	for _, layout := range []model.TypeLayout{
		{},
		{Type: model.Type{ID: 1007, Storage: model.TypeArray}},
		{Type: model.Type{ID: 23, Storage: model.TypeInt64}, Children: []model.TypeLayout{{Type: model.Type{ID: 23, Storage: model.TypeInt64}}}},
	} {
		_, err := model.TypeLayoutBytes(t.Context(), layout, 1<<20)
		if !errors.Is(err, model.ErrInvalidTypeLayout) {
			t.Fatalf("invalid layout accepted: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := model.TypeLayoutBytes(ctx, model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64}}, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission=%v", err)
	}
}

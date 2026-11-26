package model_test

import (
	"context"
	"errors"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestOwnedValueCopyPreservesNestedPayloadWithinBudget(t *testing.T) {
	value := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: -3}}, ArrayVal: []model.Value{model.BytesVal(make([]byte, 1, 1<<20))}}
	value.ArrayVal[0].BytesVal[0] = 7
	size := uint64(unsafe.Sizeof(model.Value{})) + uint64(unsafe.Sizeof(model.ArrayDimension{})) + 1
	if measured, err := model.ValueBytes(t.Context(), value, size); err != nil || measured != size {
		t.Fatalf("measured payload=%d error=%v, want %d", measured, err, size)
	}
	if _, err := model.ValueBytes(t.Context(), value, size-1); !errors.Is(err, model.ErrValueBudget) {
		t.Fatalf("short measurement budget: %v", err)
	}
	if _, _, err := model.CopyValue(t.Context(), value, size-1); !errors.Is(err, model.ErrValueBudget) {
		t.Fatalf("short copy budget: %v", err)
	}
	owned, used, err := model.CopyValue(t.Context(), value, size)
	if err != nil {
		t.Fatal(err)
	}
	if used != size {
		t.Fatalf("copy charged %d bytes, want %d", used, size)
	}
	value.Dimensions[0].LowerBound = 4
	value.ArrayVal[0].BytesVal[0] = 9
	if owned.Dimensions[0].LowerBound != -3 || owned.ArrayVal[0].BytesVal[0] != 7 {
		t.Fatal("owned nested value changed with its source")
	}
}

func TestOwnedValueCopyRejectsCyclesAndCancellation(t *testing.T) {
	children := make([]model.Value, 1)
	children[0] = model.Value{Type: model.TypeArray, ArrayVal: children}
	if _, err := model.ValueBytes(t.Context(), children[0], 1<<20); !errors.Is(err, model.ErrValueDepth) {
		t.Fatalf("cyclic measurement: %v", err)
	}
	if _, _, err := model.CopyValue(t.Context(), children[0], 1<<20); !errors.Is(err, model.ErrValueDepth) {
		t.Fatalf("cyclic value: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := model.ValueBytes(ctx, model.Int64Val(1), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scalar measurement: %v", err)
	}
	if _, _, err := model.CopyValue(ctx, model.Int64Val(1), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scalar copy: %v", err)
	}
}

package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestCopyPushdownRequestEnforcesCompletePayloadBudget(t *testing.T) {
	request := PushdownRequest{
		Layout:    []ColumnLayout{{ID: 1, Type: model.Type{ID: 17, Storage: model.TypeBytes}, HasMissingValue: true, MissingValue: model.Value{Type: model.TypeBytes, BytesVal: []byte("retained default")}}},
		ColumnIDs: []model.ColumnID{1},
		Spans:     []KeySpan{{Lower: KeyBound{Values: []model.Value{{Type: model.TypeBytes, BytesVal: []byte("lower bound")}}}, Upper: KeyBound{Unbounded: true}}},
	}
	_, used, err := CopyPushdownRequest(context.Background(), request, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := CopyPushdownRequest(context.Background(), request, used); err != nil {
		t.Fatalf("exact budget: %v", err)
	}
	failed, allocated, err := CopyPushdownRequest(context.Background(), request, used-1)
	if !errors.Is(err, model.ErrValueBudget) {
		t.Fatalf("short budget: %v", err)
	}
	if allocated != 0 || failed.Layout != nil || failed.Spans != nil {
		t.Fatal("failed copy exposed a partial request")
	}
}

func TestCopyPushdownRequestRejectsCyclicLayout(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite}, Children: make([]model.TypeLayout, 1)}
	layout.Children[0] = layout
	request := PushdownRequest{Layout: []ColumnLayout{{ID: 1, Type: layout.Type, Children: layout.Children}}}
	_, _, err := CopyPushdownRequest(context.Background(), request, 1<<20)
	if !errors.Is(err, model.ErrInvalidTypeLayout) {
		t.Fatalf("cyclic layout: %v", err)
	}
}

func TestCopyKeyLayoutOwnsNestedPhysicalDeclarations(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	record := model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: -1}, Children: []model.TypeLayout{integer}, FieldNames: []string{"value"}}
	source := []KeyColumn{{ColumnID: 1, Type: model.Type{ID: 2287, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{record}}}
	owned, used, err := CopyKeyLayout(t.Context(), source, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := CopyKeyLayout(t.Context(), source, used); err != nil {
		t.Fatalf("exact nested metadata budget: %v", err)
	}
	if partial, bytes, err := CopyKeyLayout(t.Context(), source, used-1); !errors.Is(err, model.ErrValueBudget) || partial != nil || bytes != 0 {
		t.Fatalf("short nested metadata budget returned %v, %d, %v", partial, bytes, err)
	}
	source[0].Children[0].Children[0].Type.ID = 20
	source[0].Children[0].FieldNames[0] = "changed"
	child := owned[0].Children[0]
	if child.Children[0].Type != integer.Type || len(child.FieldNames) != 0 {
		t.Fatalf("owned physical key declaration changed: %+v", child)
	}
}

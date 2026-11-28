package model_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// Declared signature layouts may carry pseudo-type marker leaves inside a
// composite or array. ValidateDeclaredLayout admits them recursively, so
// CopyDeclaredTypeLayout must agree rather than rejecting marker children.
func TestCopyDeclaredTypeLayoutAdmitsNestedPseudoMarkers(t *testing.T) {
	marker := model.TypeLayout{Type: model.Type{ID: 2283, Storage: model.TypeAny, Modifier: -1}}
	composite := model.TypeLayout{
		Type:       model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: -1},
		Children:   []model.TypeLayout{marker, {Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}},
		FieldNames: []string{"a", "b"},
	}
	if err := composite.ValidateDeclaredLayout(); err != nil {
		t.Fatalf("declared layout rejected: %v", err)
	}
	copied, _, err := model.CopyDeclaredTypeLayout(t.Context(), composite, 1<<20)
	if err != nil {
		t.Fatalf("declared copy rejected nested marker: %v", err)
	}
	if len(copied.Children) != 2 || copied.Children[0].Type.ID != 2283 {
		t.Fatalf("declared copy lost marker child: %+v", copied)
	}
}

// Physical layouts still reject pseudo-type markers anywhere in the tree:
// a marker is a declared identity, never a stored or executed layout.
func TestPhysicalCopyRejectsNestedPseudoMarkers(t *testing.T) {
	marker := model.TypeLayout{Type: model.Type{ID: 2283, Storage: model.TypeAny, Modifier: -1}}
	composite := model.TypeLayout{
		Type:     model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: -1},
		Children: []model.TypeLayout{marker},
	}
	if _, _, err := model.CopyTypeLayout(t.Context(), composite, 1<<20); err == nil {
		t.Fatal("physical copy admitted nested pseudo-type marker")
	}
	if _, _, err := model.CopyPhysicalTypeLayout(t.Context(), composite, 1<<20); err == nil {
		t.Fatal("physical-column copy admitted nested pseudo-type marker")
	}
}

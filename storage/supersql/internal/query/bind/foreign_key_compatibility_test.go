package bind

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestForeignKeyEqualityAcceptsCompatibleIntegerFamily(t *testing.T) {
	b := &binder{ctx: t.Context(), options: Options{Limits: ir.Limits{MaxDepth: 8}}}
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	bigint := model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString}}
	if !foreignKeyEqualityCompatible(b, integer, bigint) {
		t.Fatal("integer-family foreign key equality was rejected")
	}
	if foreignKeyEqualityCompatible(b, integer, text) {
		t.Fatal("unrelated key families were accepted")
	}
}

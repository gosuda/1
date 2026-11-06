package bind

import (
	"bytes"

	"google.golang.org/protobuf/reflect/protoreflect"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// Parser source offsets do not participate in grouping-expression identity.
func equivalentAST(a, b protoreflect.Message) bool {
	return equivalentScopedAST(a, b, nil)
}

func equivalentScopedAST(a, b protoreflect.Message, scope *scope) bool {
	if !a.IsValid() || !b.IsValid() {
		return a.IsValid() == b.IsValid()
	}
	if a.Descriptor() != b.Descriptor() {
		return false
	}
	if scope != nil {
		if left, ok := a.Interface().(*zpgparse.ColumnRef); ok {
			right, ok := b.Interface().(*zpgparse.ColumnRef)
			return ok && (equivalentAST(a, b) || sameGroupingColumns(left, right, scope))
		}
	}
	fields := a.Descriptor().Fields()
	for i := range fields.Len() {
		field := fields.Get(i)
		switch field.Name() {
		case "location", "stmt_location", "stmt_len", "rexpr_list_start", "rexpr_list_end", "list_start", "list_end":
			continue
		}
		if a.Has(field) != b.Has(field) {
			return false
		}
		if !a.Has(field) {
			continue
		}
		av, bv := a.Get(field), b.Get(field)
		if field.IsList() {
			al, bl := av.List(), bv.List()
			if al.Len() != bl.Len() {
				return false
			}
			for j := range al.Len() {
				if !equivalentValue(field, al.Get(j), bl.Get(j), scope) {
					return false
				}
			}
			continue
		}
		if !equivalentValue(field, av, bv, scope) {
			return false
		}
	}
	return true
}
func equivalentValue(field protoreflect.FieldDescriptor, a, b protoreflect.Value, scope *scope) bool {
	if field.Kind() == protoreflect.MessageKind {
		return equivalentScopedAST(a.Message(), b.Message(), scope)
	}
	if field.Kind() == protoreflect.BytesKind {
		return bytes.Equal(a.Bytes(), b.Bytes())
	}
	return a.Interface() == b.Interface()
}

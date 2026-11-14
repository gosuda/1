package builtin_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestOperatorMetadataResolvesExecutableOverload(t *testing.T) {
	var selected builtin.Operator
	for operator := range builtin.OperatorsNamed("+") {
		if operator.Left == 20 && operator.Right == 20 {
			selected = operator
			break
		}
	}
	if selected.ID == 0 {
		t.Fatal("bigint addition operator missing")
	}
	resolved, ok := builtin.LookupOperator(selected.ID)
	if !ok || resolved != selected {
		t.Fatal("operator identity lookup changed selected overload")
	}
	callable, err := builtin.Resolve(selected.Function, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := callable.Call(t.Context(), []model.Value{model.Int64Val(17), model.Int64Val(25)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || result.Type != model.TypeInt64 || result.IntVal != 42 {
		t.Fatalf("resolved addition: %+v %v", result, err)
	}
}

func TestCastMetadataPreservesNarrowingAndBinaryCoercion(t *testing.T) {
	narrowing, ok := builtin.LookupCast(20, 23)
	if !ok || narrowing.Context != 'a' || narrowing.Method != 'f' {
		t.Fatalf("integer narrowing cast: %+v %v", narrowing, ok)
	}
	callable, err := builtin.Resolve(narrowing.Function, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = callable.Call(t.Context(), []model.Value{model.Int64Val(1 << 31)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	var sqlError *model.Error
	if !errors.As(err, &sqlError) || sqlError.Code != "22003" {
		t.Fatalf("resolved narrowing did not preserve overflow: %v", err)
	}
	binary, ok := builtin.LookupCast(1043, 25)
	if !ok || binary.Method != 'b' || binary.Function != 0 || binary.Context != 'i' {
		t.Fatalf("varchar/text binary cast: %+v %v", binary, ok)
	}
}

func TestTypeMetadataResolvesInputAndArrayCollation(t *testing.T) {
	integer, ok := builtin.LookupType(23)
	if !ok {
		t.Fatal("int4 type missing")
	}
	input, err := builtin.Resolve(integer.Input, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	value, err := input.Call(t.Context(), []model.Value{model.StringVal("0x10")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || value.IntVal != 16 {
		t.Fatalf("catalog input function: %+v %v", value, err)
	}
	var text builtin.TypeInfo
	for candidate := range builtin.TypesNamed("text") {
		text = candidate
		break
	}
	array, ok := builtin.LookupType(text.Array)
	if !ok || array.Element != text.ID || array.Collation != 100 || array.Category != 'A' {
		t.Fatalf("array identity/collation: %+v %v", array, ok)
	}
}

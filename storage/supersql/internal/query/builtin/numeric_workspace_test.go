package builtin_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type numericInvocationAllocator struct {
	columnar.Allocator
	query *runtime.Query
}

func (allocator numericInvocationAllocator) AvailableBytes() uint64 {
	usage := allocator.query.Usage().Work
	return uint64(usage.Limit - usage.Live)
}

func TestNumericWorkspaceIsIndependentOfOutputQuota(t *testing.T) {
	query := numericRangeQuery(t, 1024)
	allocator := numericInvocationAllocator{Allocator: query.Allocator(runtime.WorkMemory), query: query}
	budget := builtin.CallBudget{MemoryBytes: 64 << 10, Allocator: allocator}
	compare := callable(t, "numeric_cmp", 1700, 1700)
	value, err := compare.Call(t.Context(), []model.Value{numericDatum("1.25"), numericDatum("1.250")}, budget)
	if err != nil || value.IsNull || value.IntVal != 0 {
		t.Fatalf("zero-output comparison = %v, %v", value, err)
	}
	_, err = compare.Call(t.Context(), []model.Value{numericDatum(strings.Repeat("9", 9000)), numericDatum("1")}, budget)
	if !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("unadmitted numeric workspace = %v", err)
	}
	if usage := query.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("numeric workspace leaked: %+v", usage)
	}
}

func TestNumericArrayComparisonAdmitsOnlyReachedElements(t *testing.T) {
	query := numericRangeQuery(t, 1024)
	allocator := numericInvocationAllocator{Allocator: query.Allocator(runtime.WorkMemory), query: query}
	compare := arrayComparisonCallable(t, 382, 1700, model.TypeNumeric, builtin.ResolveOptions{})
	huge := numericDatum(strings.Repeat("9", 9000))
	left := comparisonArray(numericDatum("1"), huge)
	right := comparisonArray(numericDatum("2"), huge)
	budget := builtin.CallBudget{MemoryBytes: 64 << 10, Allocator: allocator}
	value, err := compare.Call(t.Context(), []model.Value{left, right}, budget)
	if err != nil || value.IntVal >= 0 {
		t.Fatalf("unreached element consumed workspace: %v, %v", value, err)
	}
	right.ArrayVal[0] = numericDatum("1")
	_, err = compare.Call(t.Context(), []model.Value{left, right}, budget)
	if !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("reached large element bypassed workspace quota: %v", err)
	}
	if usage := query.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("nested workspace leaked: %+v", usage)
	}
}

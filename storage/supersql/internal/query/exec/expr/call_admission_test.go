package expr_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestScalarCallbackAdmitsTemporaryAndReturnedPayloadBeforeExecution(t *testing.T) {
	q := allocationQuery(t)
	f := newFixture(t)
	root := f.function(900990, 25, false, ir.Effects{Volatility: ir.Volatile})
	options := f.options()
	calls := 0
	options.Functions = resolverFunc(func(expr.Function) (expr.ScalarCall, error) {
		return func(ctx context.Context, _ []model.Value, budget builtin.CallBudget) (model.Value, error) {
			calls++
			if budget.OutputBytes < 256 {
				t.Fatalf("callback budget=%d; want room for 256 bytes", budget.OutputBytes)
			}
			probe, err := q.TryReserve(runtime.WorkMemory, 1)
			if probe != nil {
				probe.Release()
			}
			if !errors.Is(err, runtime.ErrBudget) {
				t.Fatalf("callback ran without admission: %v", err)
			}
			claim, err := budget.Allocator.Reserve(0)
			if err != nil {
				t.Fatalf("suballocating admitted callback memory: %v", err)
			}
			defer claim.Release()
			if _, err := claim.GrowUpTo(budget.MemoryBytes); err != nil {
				t.Fatal(err)
			}
			extra, err := budget.Allocator.Reserve(1)
			if extra != nil {
				extra.Release()
			}
			if !errors.Is(err, expr.ErrLimit) {
				t.Fatalf("suballocation exceeded callback limit: %v", err)
			}
			claim.Release()
			return model.StringVal(strings.Repeat("x", 256)), ctx.Err()
		}, nil
	})
	program := compile(t, f.graph(), []ir.ScalarID{root}, options)
	state, err := expr.NewState(program, expr.StateOptions{WorkAllocator: q.Allocator(runtime.WorkMemory), OutputAllocator: q.Allocator(runtime.OutputMemory)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := state.Close(); err != nil {
			t.Error(err)
		}
	}()
	blocker, err := q.TryReserve(runtime.WorkMemory, q.Usage().Work.Limit-8192)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	input, out := columnar.Batch{Length: 1}, columnar.Batch{}
	if err := state.Eval(t.Context(), &input, nil, &out, expr.ScalarCalls); err != nil {
		t.Fatal(err)
	}
	result, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], 0, 4096)
	if err != nil || result.StrVal != strings.Repeat("x", 256) || calls != 1 {
		t.Fatalf("admitted callback result=%v calls=%d error=%v", result, calls, err)
	}
	blocker.Release()
	if usage := q.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("callback retained work claim: %+v", usage)
	}
}

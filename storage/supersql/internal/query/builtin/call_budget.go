package builtin

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/decimalint"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// InvocationAllocator reports the unclaimed portion of one synchronous budget.
type InvocationAllocator interface {
	columnar.Allocator
	AvailableBytes() uint64
}


// CallBudget separates returned payload limits from total invocation memory.
// Allocator is borrowed for the synchronous call; nil enforces MemoryBytes
// locally without shared admission. Nested calls must exclude live outer buffers.
type CallBudget struct {
	OutputBytes int64
	MemoryBytes uint64
	Allocator   InvocationAllocator
	Temporal    model.TemporalRuntime
	Relations   model.RelationRuntime
	Settings    model.SettingsRuntime
	TextSearch  model.TextSearchRuntime
	Sequences  model.SequenceRuntime
	Advisory   model.AdvisoryLockRuntime
	Native      model.NativeScalarRuntime
}

func (budget CallBudget) NewWorkspace(ctx context.Context) (*decimalint.Workspace, error) {
	return decimalint.NewWorkspace(ctx, budget.Allocator, budget.MemoryBytes)
}

func (budget CallBudget) OutputLimit(workspace *decimalint.Workspace) int64 {
	used := workspace.UsedBytes()
	if used >= budget.MemoryBytes {
		return 0
	}
	remaining := budget.MemoryBytes - used
	if budget.Allocator != nil {
		remaining = min(remaining, budget.Allocator.AvailableBytes())
	}
	return min(budget.OutputBytes, int64(min(remaining, uint64(math.MaxInt64))))
}

// WithOutput changes only the output quota, as required by fixed-width calls.
func (budget CallBudget) WithOutput(bytes int64) CallBudget {
	budget.OutputBytes = min(budget.OutputBytes, bytes)
	return budget
}

// After excludes simultaneously live outer allocations from a nested call.
func (budget CallBudget) After(bytes uint64) (CallBudget, error) {
	if bytes > budget.MemoryBytes {
		return CallBudget{}, sqlError("54000", "builtin invocation memory budget exceeded")
	}
	budget.MemoryBytes -= bytes
	return budget, nil
}

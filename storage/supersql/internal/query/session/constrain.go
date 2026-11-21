package session

import (
	"math"

	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

// constrainEnvironment propagates the session's effective work_mem and
// temp_file_limit to an invocation. work_mem sizes each operator's working
// memory for sorting, hashing, aggregation, windowing, and spooling both
// upward and downward within the query and session ceilings, while keeping
// unconfigured (zero) operator fields disabled and preserving per-operator
// headroom invariants.
func constrainEnvironment(environment *exec.Environment, workBytes, spillBytes int64) {
	if environment == nil || workBytes <= 0 || spillBytes < 0 {
		return
	}
	const maxOperatorBudget = uint64(math.MaxInt) / 128
	work := min(uint64(workBytes), maxOperatorBudget)
	if environment.Query != nil {
		if queryLimit := environment.Query.Usage().Work.Limit; queryLimit > 0 {
			work = min(work, uint64(queryLimit))
		}
	}
	spill := uint64(spillBytes)

	environment.Sort.WorkBytes = scaleWorkBudget(environment.Sort.WorkBytes, work)
	environment.Sort.RunBytes = scaleWorkBudget(environment.Sort.RunBytes, max(work/8, 1))
	environment.Join.WorkBytes = scaleWorkBudget(environment.Join.WorkBytes, work)
	environment.Join.HashBytes = scaleWorkBudget(environment.Join.HashBytes, max(work/2, 1))
	environment.Join.MemoryBytes = scaleWorkBudget(environment.Join.MemoryBytes, max(work/16, 1))
	environment.Join.PartitionBytes = scaleWorkBudget(environment.Join.PartitionBytes, work)
	environment.Aggregate.WorkBytes = scaleWorkBudget(environment.Aggregate.WorkBytes, work)
	environment.Aggregate.GroupBytes = scaleWorkBudget(environment.Aggregate.GroupBytes, max(work/4, 1))
	environment.Aggregate.PartitionMemoryBytes = scaleWorkBudget(environment.Aggregate.PartitionMemoryBytes, max(work/32, 1))
	environment.Set.WorkBytes = scaleWorkBudget(environment.Set.WorkBytes, work)
	environment.Set.GroupBytes = scaleWorkBudget(environment.Set.GroupBytes, max(work/2, 1))
	environment.Set.PartitionMemoryBytes = scaleWorkBudget(environment.Set.PartitionMemoryBytes, max(work/16, 1))
	environment.Window.WorkBytes = scaleWorkBudget(environment.Window.WorkBytes, work)
	environment.Window.PartitionMemoryBytes = scaleWorkBudget(environment.Window.PartitionMemoryBytes, max(work/4, 1))
	environment.Window.MaxPartitionBytes = scaleWorkBudget(environment.Window.MaxPartitionBytes, work)
	environment.Apply.WorkBytes = scaleWorkBudget(environment.Apply.WorkBytes, max(work/4, 1))
	environment.Recursive.Spool.MemoryBytes = scaleWorkBudget(environment.Recursive.Spool.MemoryBytes, max(work/4, 1))
	environment.Recursive.Seen.MemoryBytes = scaleWorkBudget(environment.Recursive.Seen.MemoryBytes, max(work/8, 1))
	environment.Recursive.Seen.WorkBytes = scaleWorkBudget(environment.Recursive.Seen.WorkBytes, max(work/2, 1))
	environment.Exchange.WorkBytes = scaleWorkBudget(environment.Exchange.WorkBytes, work)
	environment.Modify.WorkBytes = scaleWorkBudget(environment.Modify.WorkBytes, work)
	// Modify stages its inputs with an external sort and a distinct key set whose
	// workspaces are derived from WorkBytes; each must still fit inside the query
	// account that work_mem bounds.
	environment.Modify.SortRunBytes = scaleWorkBudget(environment.Modify.SortRunBytes, max(environment.Modify.WorkBytes/8, 1))
	environment.Modify.SpoolMemoryBytes = scaleWorkBudget(environment.Modify.SpoolMemoryBytes, max(work/4, 1))
	environment.Spool.MemoryBytes = scaleWorkBudget(environment.Spool.MemoryBytes, max(work/4, 1))

	// Spill capacity is a separate session limit; only lower it.
	environment.Set.MaxSpillBytes = min(environment.Set.MaxSpillBytes, spill)
	environment.Spool.MaxBytes = min(environment.Spool.MaxBytes, spill)
	if spill == 0 {
		environment.Spool.MaxBytes = 0
	}
}

func scaleWorkBudget(current, target uint64) uint64 {
	if current == 0 {
		return 0
	}
	return max(target, 1)
}


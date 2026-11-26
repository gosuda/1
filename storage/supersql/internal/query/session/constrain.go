package session

import (
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

// constrainEnvironment propagates the session's effective work_mem and
// temp_file_limit to an invocation. work_mem bounds each query workspace, which
// in PostgreSQL means the per-operator memory used for sorting, hashing,
// aggregation and spilling; the query's own account is clamped alongside so a
// configured ceiling can never exceed the session limit. Operator budgets that
// exceed work_mem are reduced rather than rejected, and every reduction keeps a
// positive budget because EffectiveWorkMemoryBytes is validated before use.
func constrainEnvironment(environment *exec.Environment, workBytes, spillBytes int64) {
	if environment == nil || workBytes <= 0 || spillBytes < 0 {
		return
	}
	work := uint64(workBytes)
	spill := uint64(spillBytes)

	environment.Sort.WorkBytes = min(environment.Sort.WorkBytes, work)
	environment.Join.WorkBytes = min(environment.Join.WorkBytes, work)
	environment.Join.MemoryBytes = min(environment.Join.MemoryBytes, work)
	environment.Join.HashBytes = min(environment.Join.HashBytes, work)
	environment.Join.PartitionBytes = min(environment.Join.PartitionBytes, work)
	environment.Aggregate.WorkBytes = min(environment.Aggregate.WorkBytes, work)
	environment.Aggregate.GroupBytes = min(environment.Aggregate.GroupBytes, work)
	environment.Aggregate.PartitionMemoryBytes = min(environment.Aggregate.PartitionMemoryBytes, work)
	environment.Set.WorkBytes = min(environment.Set.WorkBytes, work)
	environment.Set.GroupBytes = min(environment.Set.GroupBytes, work)
	environment.Set.PartitionMemoryBytes = min(environment.Set.PartitionMemoryBytes, work)
	environment.Window.WorkBytes = min(environment.Window.WorkBytes, work)
	environment.Window.PartitionMemoryBytes = min(environment.Window.PartitionMemoryBytes, work)
	environment.Window.MaxPartitionBytes = min(environment.Window.MaxPartitionBytes, work)
	environment.Apply.WorkBytes = min(environment.Apply.WorkBytes, work)
	environment.Recursive.Spool.MemoryBytes = min(environment.Recursive.Spool.MemoryBytes, work)
	environment.Recursive.Seen.MemoryBytes = min(environment.Recursive.Seen.MemoryBytes, work)
	environment.Recursive.Seen.WorkBytes = min(environment.Recursive.Seen.WorkBytes, work)
	environment.Exchange.WorkBytes = min(environment.Exchange.WorkBytes, work)
	environment.Modify.WorkBytes = min(environment.Modify.WorkBytes, work)
	// Modify stages its inputs with an external sort and a distinct key set whose
	// workspaces are derived from WorkBytes; each must still fit inside the query
	// account that work_mem bounds.
	environment.Modify.SortRunBytes = min(environment.Modify.SortRunBytes, max(environment.Modify.WorkBytes/8, 1))
	environment.Modify.SpoolMemoryBytes = min(environment.Modify.SpoolMemoryBytes, work)
	environment.Spool.MemoryBytes = min(environment.Spool.MemoryBytes, work)

	// Spill capacity is a separate session limit; only lower it.
	environment.Set.MaxSpillBytes = min(environment.Set.MaxSpillBytes, spill)
	environment.Spool.MaxBytes = min(environment.Spool.MaxBytes, spill)
	if spill == 0 {
		environment.Spool.MaxBytes = 0
	}
}

package locks

import (
	"errors"
	"slices"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	ErrDeadlockDetected = &model.Error{Code: "40P01", Message: "deadlock detected"}
	errWaitGraphConfig  = errors.New("invalid lock wait graph limits")
	errWaitGraphOwner   = errors.New("invalid lock wait owner")
	errWaitGraphLimit   = &model.Error{Code: "53200", Message: "lock wait graph capacity exceeded"}
)

type WaitOwner uint64

type WaitGraphLimits struct {
	MaxOwners int
	MaxEdges  int
	MaxBytes  uint64
}

func DefaultWaitGraphLimits() WaitGraphLimits {
	return WaitGraphLimits{MaxOwners: 4096, MaxEdges: 1 << 16, MaxBytes: 4 << 20}
}

type waitEdges struct {
	blockers []WaitOwner
	bytes    uint64
}

// WaitGraph coordinates waits owned by otherwise independent lock registries.
// A caller registers only while blocked and must EndWait before returning.
type WaitGraph struct {
	mu       sync.Mutex
	limits   WaitGraphLimits
	waits    map[WaitOwner]waitEdges
	edges    int
	usedBytes uint64
}

func NewWaitGraph(limits WaitGraphLimits) (*WaitGraph, error) {
	if limits.MaxOwners <= 0 || limits.MaxEdges <= 0 || limits.MaxBytes < uint64(unsafe.Sizeof(waitEdges{})) {
		return nil, errWaitGraphConfig
	}
	return &WaitGraph{limits: limits, waits: make(map[WaitOwner]waitEdges)}, nil
}

func (graph *WaitGraph) BeginWait(waiter WaitOwner, blockers []WaitOwner) error {
	if graph == nil || waiter == 0 {
		return nil
	}
	owned := slices.Clone(blockers)
	slices.Sort(owned)
	owned = slices.Compact(owned)
	owned = slices.DeleteFunc(owned, func(blocker WaitOwner) bool { return blocker == 0 || blocker == waiter })
	if len(owned) == 0 {
		graph.EndWait(waiter)
		return nil
	}
	graph.mu.Lock()
	defer graph.mu.Unlock()
	prior, existed := graph.waits[waiter]
	owners := len(graph.waits)
	if !existed {
		owners++
	}
	edges := graph.edges - len(prior.blockers) + len(owned)
	bytes := uint64(unsafe.Sizeof(waitEdges{})) + uint64(len(owned))*uint64(unsafe.Sizeof(WaitOwner(0)))
	used := graph.usedBytes - prior.bytes + bytes
	if owners > graph.limits.MaxOwners || edges > graph.limits.MaxEdges || used > graph.limits.MaxBytes {
		return errWaitGraphLimit
	}
	if graph.reaches(waiter, waiter, owned, make(map[WaitOwner]bool, owners)) {
		return ErrDeadlockDetected
	}
	graph.waits[waiter] = waitEdges{blockers: owned, bytes: bytes}
	graph.edges, graph.usedBytes = edges, used
	return nil
}

func (graph *WaitGraph) reaches(current, target WaitOwner, proposed []WaitOwner, visiting map[WaitOwner]bool) bool {
	if visiting[current] {
		return false
	}
	visiting[current] = true
	defer delete(visiting, current)
	blockers := proposed
	if current != target {
		blockers = graph.waits[current].blockers
	}
	for _, blocker := range blockers {
		if blocker == target || graph.reaches(blocker, target, proposed, visiting) {
			return true
		}
	}
	return false
}

func (graph *WaitGraph) EndWait(waiter WaitOwner) {
	if graph == nil || waiter == 0 {
		return
	}
	graph.mu.Lock()
	defer graph.mu.Unlock()
	prior, exists := graph.waits[waiter]
	if !exists {
		return
	}
	delete(graph.waits, waiter)
	graph.edges -= len(prior.blockers)
	graph.usedBytes -= prior.bytes
}

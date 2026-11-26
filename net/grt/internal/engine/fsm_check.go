package engine

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
)

// The generated FSM tables are the specification of record. A transition
// the engine performs outside them is an engine/IR drift bug: it is counted
// here and asserted to be zero by the engine and simulation test suites.
var (
	fsmViolationCount atomic.Uint64
	fsmViolationMu    sync.Mutex
	fsmViolationSeen  []string // distinct edges, capped at maxFSMViolationKinds
)

const maxFSMViolationKinds = 32

// fsmViolation records an assignment to a state outside the adjacency of
// the verified machine.
func fsmViolation(machine, from, to uint8) {
	recordFSMViolation(func() string {
		return fmt.Sprintf("%s: illegal edge %d -> %d", fsmMachineNames[machine], from, to)
	})
}

// fsmRefusal records an event the engine expected to be legal but the
// table did not accept in the current state.
func fsmRefusal(machine, from, ev uint8) {
	recordFSMViolation(func() string {
		return fmt.Sprintf("%s: event %d refused in state %d", fsmMachineNames[machine], ev, from)
	})
}

// fsmPanicOnViolation turns drift into an immediate panic with a stack
// trace; tests enable it via GRT_FSM_PANIC=1 when hunting a violation.
var fsmPanicOnViolation bool

func recordFSMViolation(describe func() string) {
	fsmViolationCount.Add(1)
	msg := describe()
	if fsmPanicOnViolation {
		panic("fsm drift: " + msg)
	}
	fsmViolationMu.Lock()
	defer fsmViolationMu.Unlock()
	if len(fsmViolationSeen) < maxFSMViolationKinds && !slices.Contains(fsmViolationSeen, msg) {
		fsmViolationSeen = append(fsmViolationSeen, msg)
	}
}

// FSMViolations reports how many state changes fell outside the verified
// transition tables since process start, with the distinct offending edges.
func FSMViolations() (count uint64, kinds []string) {
	fsmViolationMu.Lock()
	defer fsmViolationMu.Unlock()
	return fsmViolationCount.Load(), slices.Clone(fsmViolationSeen)
}

// fireConn applies ev through the generated total ConnLifecycle table, the
// only writer of core.state. Non-transition dispositions leave the state
// unchanged; callers act on the returned action bits.
func (c *Conn) fireConn(ev uint8) fsmStepResult {
	r := fsmConnStep(c.core.state, ev)
	if r.disp == fsmDispTransition {
		c.core.state = connState(r.next)
	}
	return r
}

// mustFireConn is fireConn for call sites whose own guards already imply
// the event is legal; a refusal there is recorded as drift.
func (c *Conn) mustFireConn(ev uint8) fsmStepResult {
	from := c.core.state
	r := c.fireConn(ev)
	if r.disp != fsmDispTransition && r.disp != fsmDispStay {
		fsmRefusal(fsmMachineConnLifecycle, uint8(from), ev)
	}
	return r
}

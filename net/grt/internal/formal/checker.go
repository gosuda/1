package formal

import "fmt"

// CompiledCell is one entry in a compiled [State][Event] transition matrix.
type CompiledCell struct {
	Next       uint8
	Disp       Disposition
	ActionMask uint16
}

// CompiledMachine holds the verified, total transition table and bitmasks
// derived from a MachineSpec.
type CompiledMachine struct {
	Spec           MachineSpec
	Table          [][]CompiledCell
	ValidNextMask  []uint16
	PredicateMasks map[string]uint16
	ActionBits     map[string]uint16
}

// MachineReport summarizes the formal verification checks for one FSM.
type MachineReport struct {
	MachineName          string
	NumStates            int
	NumEvents            int
	TotalCells           int
	ExplicitTransitions  int
	ReachableStates      int
	TerminalStates       int
	Deterministic        bool
	Total                bool
	AbsorbingTerminals   bool
	TerminationReachable bool
}

// CompileAndVerifyMachine compiles a MachineSpec into a total transition matrix
// and verifies:
//   - D01: Deterministic transitions (no duplicate (State, Event) pairs)
//   - T01: Total input handling across all |States| x |Events| (Section 20.2)
//   - R01: Full state reachability from Initial via BFS
//   - A01: Terminal state absorption (Section 20.1)
//   - L01: Absence of non-terminating livelock traps when terminal states exist
func CompileAndVerifyMachine(m MachineSpec) (*CompiledMachine, *MachineReport, error) {
	numStates := len(m.States)
	numEvents := len(m.Events)
	if numStates == 0 || numStates > 16 {
		return nil, nil, fmt.Errorf("%s: invalid state count %d (must be 1..16 for uint16 bitmask)", m.Name, numStates)
	}
	if numEvents == 0 || numEvents > 32 {
		return nil, nil, fmt.Errorf("%s: invalid event count %d", m.Name, numEvents)
	}
	if int(m.Initial) >= numStates {
		return nil, nil, fmt.Errorf("%s: initial state %d out of range", m.Name, m.Initial)
	}

	predMasks := make(map[string]uint16, len(m.Predicates))
	for _, p := range m.Predicates {
		if _, exists := predMasks[p.Name]; exists {
			return nil, nil, fmt.Errorf("%s: duplicate predicate %q", m.Name, p.Name)
		}
		predMasks[p.Name] = 0
	}

	terminalCount := 0
	for i, st := range m.States {
		if int(st.ID) != i {
			return nil, nil, fmt.Errorf("%s: state %q has ID %d, want dense index %d", m.Name, st.Name, st.ID, i)
		}
		if st.Terminal {
			terminalCount++
		}
		for _, pName := range st.Predicates {
			mask, ok := predMasks[pName]
			if !ok {
				return nil, nil, fmt.Errorf("%s: state %q references undeclared predicate %q", m.Name, st.Name, pName)
			}
			predMasks[pName] = mask | (uint16(1) << uint(i))
		}
	}

	for i, ev := range m.Events {
		if int(ev.ID) != i {
			return nil, nil, fmt.Errorf("%s: event %q has ID %d, want dense index %d", m.Name, ev.Name, ev.ID, i)
		}
	}

	actionBits := make(map[string]uint16, len(m.Actions))
	usedBits := uint16(0)
	for _, act := range m.Actions {
		if act.Bit == 0 || act.Bit&(act.Bit-1) != 0 {
			return nil, nil, fmt.Errorf("%s: action %q bit 0x%x is not a single power-of-two bit", m.Name, act.Name, act.Bit)
		}
		if usedBits&act.Bit != 0 {
			return nil, nil, fmt.Errorf("%s: action %q reuses bit 0x%x", m.Name, act.Name, act.Bit)
		}
		usedBits |= act.Bit
		actionBits[act.Name] = act.Bit
	}

	table := make([][]CompiledCell, numStates)
	defined := make([][]bool, numStates)
	validNext := make([]uint16, numStates)
	for s := range numStates {
		table[s] = make([]CompiledCell, numEvents)
		defined[s] = make([]bool, numEvents)
		validNext[s] = uint16(1) << uint(s) // self-transition is always valid for idempotent stays
		defDisp := m.DefaultDisp
		if m.States[s].Terminal {
			defDisp = DispIgnore
		}
		for e := range numEvents {
			table[s][e] = CompiledCell{
				Next:       uint8(s),
				Disp:       defDisp,
				ActionMask: 0,
			}
		}
	}

	for _, tr := range m.Transitions {
		if int(tr.From) >= numStates {
			return nil, nil, fmt.Errorf("%s: transition from invalid state %d", m.Name, tr.From)
		}
		if int(tr.Event) >= numEvents {
			return nil, nil, fmt.Errorf("%s: transition on invalid event %d", m.Name, tr.Event)
		}
		if int(tr.To) >= numStates {
			return nil, nil, fmt.Errorf("%s: transition to invalid state %d", m.Name, tr.To)
		}
		if defined[tr.From][tr.Event] {
			return nil, nil, fmt.Errorf("%s: non-deterministic duplicate transition for (state=%s, event=%s)",
				m.Name, m.States[tr.From].Name, m.Events[tr.Event].Name)
		}
		defined[tr.From][tr.Event] = true

		var actMask uint16
		for _, aName := range tr.Actions {
			bit, ok := actionBits[aName]
			if !ok {
				return nil, nil, fmt.Errorf("%s: transition (%s, %s) references unknown action %q",
					m.Name, m.States[tr.From].Name, m.Events[tr.Event].Name, aName)
			}
			actMask |= bit
		}

		table[tr.From][tr.Event] = CompiledCell{
			Next:       tr.To,
			Disp:       tr.Disp,
			ActionMask: actMask,
		}
		validNext[tr.From] |= uint16(1) << uint(tr.To)
	}

	// Verify A01: Terminal state absorption.
	for s, st := range m.States {
		if !st.Terminal {
			continue
		}
		for e := range numEvents {
			cell := table[s][e]
			if int(cell.Next) != s || cell.ActionMask != 0 {
				return nil, nil, fmt.Errorf("%s: terminal state %s is not absorbing on event %s (next=%d, actions=0x%x)",
					m.Name, st.Name, m.Events[e].Name, cell.Next, cell.ActionMask)
			}
		}
	}

	// Verify R01: Full state reachability from m.Initial via BFS.
	visited := make([]bool, numStates)
	queue := make([]uint8, 0, numStates)
	visited[m.Initial] = true
	queue = append(queue, m.Initial)
	for head := 0; head < len(queue); head++ {
		cur := queue[head]
		for e := range numEvents {
			nxt := table[cur][e].Next
			if !visited[nxt] {
				visited[nxt] = true
				queue = append(queue, nxt)
			}
		}
	}
	reachable := len(queue)
	if reachable != numStates {
		for s, ok := range visited {
			if !ok {
				return nil, nil, fmt.Errorf("%s: unreachable state %s from initial state %s",
					m.Name, m.States[s].Name, m.States[m.Initial].Name)
			}
		}
	}

	// Verify L01: If the machine has terminal states, every state must be able
	// to reach at least one terminal state (absence of inescapable livelock traps).
	termReachable := true
	if terminalCount > 0 {
		canTerminate := make([]bool, numStates)
		bQueue := make([]uint8, 0, numStates)
		for s, st := range m.States {
			if st.Terminal {
				canTerminate[s] = true
				bQueue = append(bQueue, uint8(s))
			}
		}
		for head := 0; head < len(bQueue); head++ {
			target := bQueue[head]
			for s := range numStates {
				if canTerminate[s] {
					continue
				}
				for e := range numEvents {
					if table[s][e].Next == target {
						canTerminate[s] = true
						bQueue = append(bQueue, uint8(s))
						break
					}
				}
			}
		}
		for s, ok := range canTerminate {
			if !ok {
				return nil, nil, fmt.Errorf("%s: state %s has no path to any terminal state (livelock trap)",
					m.Name, m.States[s].Name)
			}
		}
	}

	cm := &CompiledMachine{
		Spec:           m,
		Table:          table,
		ValidNextMask:  validNext,
		PredicateMasks: predMasks,
		ActionBits:     actionBits,
	}
	rep := &MachineReport{
		MachineName:          m.Name,
		NumStates:            numStates,
		NumEvents:            numEvents,
		TotalCells:           numStates * numEvents,
		ExplicitTransitions:  len(m.Transitions),
		ReachableStates:      reachable,
		TerminalStates:       terminalCount,
		Deterministic:        true,
		Total:                true,
		AbsorbingTerminals:   true,
		TerminationReachable: termReachable,
	}
	return cm, rep, nil
}

// CompileAndVerifyAll compiles and formally verifies all 11 GRT/1.1 machines.
func CompileAndVerifyAll() ([]*CompiledMachine, []*MachineReport, error) {
	specs := AllMachines()
	compiled := make([]*CompiledMachine, 0, len(specs))
	reports := make([]*MachineReport, 0, len(specs))
	for _, sp := range specs {
		cm, rep, err := CompileAndVerifyMachine(sp)
		if err != nil {
			return nil, nil, err
		}
		compiled = append(compiled, cm)
		reports = append(reports, rep)
	}
	return compiled, reports, nil
}

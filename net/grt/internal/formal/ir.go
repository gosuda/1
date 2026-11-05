package formal

// Disposition classifies how a state machine handles an (State, Event) pair.
// Every machine specifies a default disposition so the generated [State][Event]
// matrix is total (GRT/1.1 Section 20.2).
type Disposition uint8

const (
	DispTransition Disposition = iota
	DispStay
	DispIgnore
	DispProtocolError
	DispInvalidState
)

func (d Disposition) String() string {
	switch d {
	case DispTransition:
		return "TRANSITION"
	case DispStay:
		return "STAY"
	case DispIgnore:
		return "IGNORE"
	case DispProtocolError:
		return "PROTOCOL_ERROR"
	case DispInvalidState:
		return "INVALID_STATE"
	}
	return "UNKNOWN"
}

// StateSpec declares a state within a finite-state machine.
// ID must match the dense 0..N-1 slice index so bitmask predicates
// map state s to bit (1 << s).
type StateSpec struct {
	ID          uint8
	Name        string
	EngineConst string
	Terminal    bool
	Predicates  []string
}

// EventSpec declares a discrete transition trigger (wire frame, timer, or API call).
type EventSpec struct {
	ID   uint8
	Name string
}

// ActionFlag declares an atomic side-effect bit emitted by a transition.
type ActionFlag struct {
	Name string
	Bit  uint16
}

// PredicateSpec declares an O(1) bitmask classifier over a machine's states.
type PredicateSpec struct {
	Name   string
	GoFunc string
	Doc    string
}

// TransitionSpec declares a deterministic state transition.
type TransitionSpec struct {
	From    uint8
	Event   uint8
	To      uint8
	Disp    Disposition
	Actions []string
	SpecRef string
}

// MachineSpec is the canonical Intermediate Representation (IR) of one
// orthogonal GRT/1.1 state machine. It drives both formal verification
// (explicit-state model checking and TLA+ generation) and zero-allocation
// Go code generation.
type MachineSpec struct {
	Name        string
	GoPrefix    string
	StateType   string
	SpecSection string
	Initial     uint8
	DefaultDisp Disposition
	States      []StateSpec
	Events      []EventSpec
	Predicates  []PredicateSpec
	Actions     []ActionFlag
	Transitions []TransitionSpec
}

// GateTerm is one boolean precondition in a conjunctive gate (Section 22.3).
type GateTerm struct {
	Name string
	Bit  uint16
	Doc  string
}

// GateSpec defines a conjunctive boolean gate (G = AND_i term_i) that guards
// packet emission, transactional commit, path promotion, or stream retirement.
type GateSpec struct {
	Name        string
	GoFunc      string
	MaskConst   string
	SpecSection string
	Doc         string
	Terms       []GateTerm
}

// InvariantSpec catalogs a normative GRT/1.1 invariant (Sections 27, 34, 38)
// verified by the composed explicit-state model checker and exported to TLA+.
type InvariantSpec struct {
	ID          string
	Category    string
	SpecSection string
	Summary     string
}

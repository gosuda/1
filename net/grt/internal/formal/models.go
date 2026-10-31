package formal

import "fmt"

// ModelReport summarizes the exhaustive BFS state-space exploration of a
// composed multi-FSM protocol model.
type ModelReport struct {
	ModelName          string
	ReachableStates    int
	TransitionsChecked int
	InvariantsVerified []string
}

// VerifyAllComposedModels runs exhaustive explicit-state model checking across
// all five composed multi-component GRT/1.1 models.
func VerifyAllComposedModels() ([]ModelReport, error) {
	builders := []func() (ModelReport, error){
		verifyConnCryptoModel,
		verifyPathMigrationModel,
		verifyStreamLifecycleModel,
		verifyMobileParkingModel,
		verifyLedgerAccountingModel,
	}
	reports := make([]ModelReport, 0, len(builders))
	for _, fn := range builders {
		rep, err := fn()
		if err != nil {
			return nil, err
		}
		reports = append(reports, rep)
	}
	return reports, nil
}

// ---------------------------------------------------------------------------
// Composed Model 1: Connection Lifecycle x Cipher x 3-Slot Key Refresh x I/O Pins
//
// The lifecycle component is not re-implemented here: every lifecycle step
// is a cell of the compiled ConnLifecycle table, the same table the engine
// executes through fireConn. Crypto registers react to the step's action
// bits, so an IR edit that breaks a cross-component invariant fails here.
// ---------------------------------------------------------------------------

// Lifecycle state and event IDs from connLifecycleMachine.
const (
	ccCFinishing  = 4
	ccSFinishing  = 7
	ccEstablished = 8
	ccClosing     = 9
	ccDraining    = 10
	ccQuiescing   = 11
	ccDead        = 12

	ccEvPinsDrained = 13
	ccEvLocalFatal  = 14
)

type connCryptoState struct {
	connSt       uint8 // ConnLifecycle state ID
	cipherSt     uint8 // 0=unresolved, 1=locked
	lockedSuite  uint8 // 0=none, 1=AES, 2=XChaCha
	activeSlot   uint8 // epochSlotState
	nextSlot     uint8 // epochSlotState
	prevSlot     uint8 // epochSlotState
	activeEpoch  uint8 // 0..3 bounded epoch counter
	pnStage      uint8 // 0=normal, 1=soft_limit, 2=hard_limit
	ioPins       uint8 // 0..2 outstanding async I/O pins
	secretsWiped bool
}

func checkConnCryptoInvariants(s connCryptoState) error {
	// I01: a locked cipher always names its suite.
	if s.cipherSt == 1 && s.lockedSuite == 0 {
		return fmt.Errorf("I01 violated: cipher LOCKED without suite")
	}
	// I04: key-bearing non-quiescing states hold installed epoch keys.
	strictlyKeyed := s.connSt == ccCFinishing || s.connSt == ccSFinishing ||
		s.connSt == ccEstablished || s.connSt == ccClosing || s.connSt == ccDraining
	if strictlyKeyed && (s.cipherSt != 1 || s.activeSlot != 1) {
		return fmt.Errorf("I04 violated: state %d requires installed keys but cipher=%d activeSlot=%d",
			s.connSt, s.cipherSt, s.activeSlot)
	}
	// I22: PREV is never seal-eligible.
	if s.prevSlot == 1 {
		return fmt.Errorf("I22 violated: PREV slot in seal-eligible INSTALLED state")
	}
	// I21: no state remains ESTABLISHED at the hard AEAD limit.
	if s.connSt == ccEstablished && s.pnStage == 2 {
		return fmt.Errorf("I21 violated: connection remained ESTABLISHED at hard AEAD PN limit")
	}
	// A08: DEAD only after every I/O pin drained and secrets were wiped.
	if s.connSt == ccDead && (s.ioPins != 0 || !s.secretsWiped) {
		return fmt.Errorf("A08 violated: reached DEAD with ioPins=%d secretsWiped=%v", s.ioPins, s.secretsWiped)
	}
	return nil
}

// connCryptoStep is one successor with the lifecycle actions that produced
// it (zero for environment steps).
type connCryptoStep struct {
	next    connCryptoState
	actions uint16
}

// applyLifecycleCell applies a compiled ConnLifecycle cell and the crypto
// effects of its action bits. Guards that live outside the lifecycle
// (I/O fences) are enforced here, mirroring the engine's callers.
func applyLifecycleCell(cm *CompiledMachine, s connCryptoState, ev int) []connCryptoStep {
	cell := cm.Table[s.connSt][ev]
	if cell.Disp != DispTransition {
		return nil
	}
	if ev == ccEvPinsDrained && s.ioPins != 0 {
		return nil
	}
	n := s
	n.connSt = cell.Next
	if cell.ActionMask&cm.ActionBits["ActZeroizeSecrets"] != 0 {
		n.activeSlot, n.nextSlot, n.prevSlot = 0, 0, 0
		n.secretsWiped = true
	}
	if cell.ActionMask&cm.ActionBits["ActInstallEpoch1"] == 0 {
		return []connCryptoStep{{n, cell.ActionMask}}
	}
	var out []connCryptoStep
	for _, suite := range []uint8{1, 2} {
		k := n
		k.cipherSt = 1
		k.lockedSuite = suite
		k.activeSlot = 1
		k.activeEpoch = 1
		out = append(out, connCryptoStep{k, cell.ActionMask})
	}
	return out
}

func nextConnCryptoStates(cm *CompiledMachine, s connCryptoState) []connCryptoStep {
	var out []connCryptoStep
	quiet := cm.PredicateMasks["QuietTerminal"]&(1<<s.connSt) != 0

	// Environment: async I/O pins are acquired only while emission is
	// allowed and always drain eventually.
	if !quiet && s.ioPins < 2 {
		n := s
		n.ioPins++
		out = append(out, connCryptoStep{next: n})
	}
	if s.ioPins > 0 {
		n := s
		n.ioPins--
		out = append(out, connCryptoStep{next: n})
	}

	// Every lifecycle event, via the compiled table.
	for ev := range cm.Spec.Events {
		out = append(out, applyLifecycleCell(cm, s, ev)...)
	}

	// Key refresh and PN progression in ESTABLISHED.
	if s.connSt == ccEstablished {
		if s.nextSlot == 0 && s.activeEpoch < 3 {
			n := s
			n.nextSlot = 1
			out = append(out, connCryptoStep{next: n})
		}
		if s.nextSlot == 1 && s.activeEpoch < 3 {
			n := s
			n.prevSlot = 2
			n.activeSlot = 1
			n.nextSlot = 0
			n.activeEpoch++
			n.pnStage = 0
			out = append(out, connCryptoStep{next: n})
		}
		if s.prevSlot == 2 {
			n := s
			n.prevSlot = 0
			out = append(out, connCryptoStep{next: n})
		}
		if s.pnStage == 0 {
			n := s
			n.pnStage = 1
			out = append(out, connCryptoStep{next: n})
		}
		if s.pnStage == 1 {
			// Reaching the hard limit without a commit is a local fatal
			// condition: the engine fires EV_LOCAL_FATAL in the same step.
			h := s
			h.pnStage = 2
			out = append(out, applyLifecycleCell(cm, h, ccEvLocalFatal)...)
		}
	}
	return out
}

func verifyConnCryptoModel() (ModelReport, error) {
	return verifyConnCryptoModelFor(connLifecycleMachine())
}

func verifyConnCryptoModelFor(lifecycle MachineSpec) (ModelReport, error) {
	cm, _, err := CompileAndVerifyMachine(lifecycle)
	if err != nil {
		return ModelReport{}, err
	}
	emitClose := cm.ActionBits["ActEmitClose"]
	liveKeyed := cm.PredicateMasks["LiveKeyed"]
	quietMask := cm.PredicateMasks["QuietTerminal"]

	initSt := connCryptoState{}
	visited := map[connCryptoState]bool{initSt: true}
	queue := []connCryptoState{initSt}
	transitions := 0

	for head := 0; head < len(queue); head++ {
		cur := queue[head]
		if err := checkConnCryptoInvariants(cur); err != nil {
			return ModelReport{}, err
		}
		for _, st := range nextConnCryptoStates(cm, cur) {
			nxt := st.next
			transitions++
			if cur.lockedSuite != 0 && nxt.lockedSuite != cur.lockedSuite {
				return ModelReport{}, fmt.Errorf("I01 violated: lockedSuite changed from %d to %d", cur.lockedSuite, nxt.lockedSuite)
			}
			if nxt.activeEpoch < cur.activeEpoch {
				return ModelReport{}, fmt.Errorf("I19 violated: activeEpoch decreased from %d to %d", cur.activeEpoch, nxt.activeEpoch)
			}
			// I23: DRAINING/QUIESCING/DEAD never emit and never re-enter
			// CLOSING; CLOSING is entered only from a live keyed state.
			if quietMask&(1<<cur.connSt) != 0 && (st.actions&emitClose != 0 || nxt.connSt == ccClosing) {
				return ModelReport{}, fmt.Errorf("I23 violated: quiet state %d emits or re-enters CLOSING", cur.connSt)
			}
			if nxt.connSt == ccClosing && cur.connSt != ccClosing && liveKeyed&(1<<cur.connSt) == 0 {
				return ModelReport{}, fmt.Errorf("I23 violated: CLOSING entered from non-live state %d", cur.connSt)
			}
			// I24: DEAD is absorbing.
			if cur.connSt == ccDead && nxt.connSt != ccDead {
				return ModelReport{}, fmt.Errorf("I24 violated: DEAD left for %d", nxt.connSt)
			}
			if !visited[nxt] {
				visited[nxt] = true
				queue = append(queue, nxt)
			}
		}
	}
	return ModelReport{
		ModelName:          "ModelConnCrypto",
		ReachableStates:    len(visited),
		TransitionsChecked: transitions,
		InvariantsVerified: []string{"I01", "I04", "I19", "I21", "I22", "I23", "I24", "A08"},
	}, nil
}

// ---------------------------------------------------------------------------
// Composed Model 2: Path Validation x Anti-Amplification x Cutover x PMTU/ECN
// ---------------------------------------------------------------------------

type pathMigrationState struct {
	activeSt     uint8 // psActive(4)
	candSt       uint8 // psFree(0), psValidating(1), psReachable(2), psReady(3), psFailed(6)
	oldSt        uint8 // psFree(0), psOld(5)
	candRxPkts   uint8 // 0..2
	candTxPkts   uint8 // 0..6
	candRxUnits  uint8 // 0..2 (in 1200B units)
	candTxUnits  uint8 // 0..6 (in 1200B units)
	candBase1200 bool
	cutoverSeq   uint8 // 0..2
	cutoverPN    uint8 // 0..3
	ecnKilled    bool
	pmtuProbing  bool
}

func checkPathMigrationInvariants(s pathMigrationState) error {
	// M07: Active path is always psActive(4).
	if s.activeSt != 4 {
		return fmt.Errorf("M07 violated: active path state=%d", s.activeSt)
	}
	// M02: When candidate is unvalidated (psValidating=1), tx <= 3 * rx for both packets and bytes.
	if s.candSt == 1 {
		if s.candTxPkts > 3*s.candRxPkts || s.candTxUnits > 3*s.candRxUnits {
			return fmt.Errorf("M02 violated: unvalidated candidate txPkts=%d rxPkts=%d txUnits=%d rxUnits=%d",
				s.candTxPkts, s.candRxPkts, s.candTxUnits, s.candRxUnits)
		}
	}
	// M06: psReady(3) requires candBase1200 == true.
	if s.candSt == 3 && !s.candBase1200 {
		return fmt.Errorf("M06 violated: candidate in READY without 1200B base-size proof")
	}
	// I18: Any completed migration (cutoverSeq > 0) must have ecnKilled == true.
	if s.cutoverSeq > 0 && !s.ecnKilled {
		return fmt.Errorf("I18 violated: migration completed (seq=%d) without disabling sender ECN", s.cutoverSeq)
	}
	return nil
}

func nextPathMigrationStates(s pathMigrationState) []pathMigrationState {
	var out []pathMigrationState

	// Candidate allocation from peer packet (adds 1 rx pkt and 1 rx unit)
	if s.candSt == 0 {
		n := s
		n.candSt = 1
		n.candRxPkts = 1
		n.candRxUnits = 1
		n.candTxPkts = 0
		n.candTxUnits = 0
		n.candBase1200 = false
		out = append(out, n)
	}

	// While candidate is VALIDATING(1):
	if s.candSt == 1 {
		// Peer sends another authenticated packet on candidate
		if s.candRxPkts < 2 {
			rx := s
			rx.candRxPkts++
			rx.candRxUnits++
			out = append(out, rx)
		}
		// Send challenge on candidate only when 3x amp guard permits (M02)
		if s.candTxPkts+1 <= 3*s.candRxPkts && s.candTxUnits+1 <= 3*s.candRxUnits {
			tx := s
			tx.candTxPkts++
			tx.candTxUnits++
			out = append(out, tx)
		}
		// Small response arrives (after at least 1 challenge sent) -> REACHABLE(2), not READY
		if s.candTxPkts > 0 {
			sm := s
			sm.candSt = 2
			out = append(out, sm)

			// Full 1200B response arrives -> READY(3) with candBase1200 = true
			rd := s
			rd.candSt = 3
			rd.candBase1200 = true
			out = append(out, rd)
		}
		// Validation timeout -> FAILED(6)
		fl := s
		fl.candSt = 6
		out = append(out, fl)
	}

	// From REACHABLE(2), 1200B exchange completes -> READY(3)
	if s.candSt == 2 {
		rd := s
		rd.candSt = 3
		rd.candBase1200 = true
		out = append(out, rd)

		fl := s
		fl.candSt = 6
		out = append(out, fl)
	}

	// From READY(3), promote candidate to ACTIVE if cutoverSeq < 2
	if s.candSt == 3 && s.cutoverSeq < 2 {
		pr := s
		pr.activeSt = 4
		pr.oldSt = 5
		pr.candSt = 0
		pr.candRxPkts = 0
		pr.candTxPkts = 0
		pr.candRxUnits = 0
		pr.candTxUnits = 0
		pr.candBase1200 = false
		pr.cutoverSeq++
		pr.cutoverPN++
		pr.ecnKilled = true
		pr.pmtuProbing = false
		out = append(out, pr)
	}

	// Late old-path packet with PN < cutoverPN: must NEVER re-promote OLD (M09)
	if s.oldSt == 5 {
		stay := s
		out = append(out, stay)

		// Old path retention timer expires -> FREE(0)
		rel := s
		rel.oldSt = 0
		out = append(out, rel)
	}

	// Failed candidate slot reclaimed
	if s.candSt == 6 {
		rel := s
		rel.candSt = 0
		rel.candRxPkts = 0
		rel.candTxPkts = 0
		rel.candRxUnits = 0
		rel.candTxUnits = 0
		rel.candBase1200 = false
		out = append(out, rel)
	}

	// I15: PMTU probe on ACTIVE path with target > 2*M clamps pacing reservation to 2*M
	// so probe can always be emitted and resolved without deadlock.
	if !s.pmtuProbing {
		probe := s
		probe.pmtuProbing = true
		out = append(out, probe)
	}
	if s.pmtuProbing {
		resolved := s
		resolved.pmtuProbing = false
		out = append(out, resolved)
	}

	return out
}

func verifyPathMigrationModel() (ModelReport, error) {
	initSt := pathMigrationState{activeSt: 4}
	visited := map[pathMigrationState]bool{initSt: true}
	queue := []pathMigrationState{initSt}
	transitions := 0

	for head := 0; head < len(queue); head++ {
		cur := queue[head]
		if err := checkPathMigrationInvariants(cur); err != nil {
			return ModelReport{}, err
		}
		for _, nxt := range nextPathMigrationStates(cur) {
			transitions++
			// M01: Candidate cannot jump from FREE(0) directly to READY(3) or promote without challenge/response.
			if cur.candSt == 0 && nxt.cutoverSeq > cur.cutoverSeq {
				return ModelReport{}, fmt.Errorf("M01 violated: unvalidated tuple promoted directly")
			}
			// M09: cutoverSeq and cutoverPN are strictly non-decreasing.
			if nxt.cutoverSeq < cur.cutoverSeq || nxt.cutoverPN < cur.cutoverPN {
				return ModelReport{}, fmt.Errorf("M09 violated: cutover barrier regressed")
			}
			if !visited[nxt] {
				visited[nxt] = true
				queue = append(queue, nxt)
			}
		}
	}
	return ModelReport{
		ModelName:          "ModelPathMigration",
		ReachableStates:    len(visited),
		TransitionsChecked: transitions,
		InvariantsVerified: []string{"I18", "M01", "M02", "M06", "M07", "M09"},
	}, nil
}

// ---------------------------------------------------------------------------
// Composed Model 3: Stream Slot x Send Half x Recv Half x Flow Control & Retire
// ---------------------------------------------------------------------------

type streamModelState struct {
	slotSt       uint8 // slotEmpty(0)..slotExhausted(5)
	sendSt       uint8 // sendOpen(0)..sendDone(3)
	recvSt       uint8 // recvOpen(0)..recvDone(3)
	initiator    bool  // true=local initiator, false=peer initiator
	gen          uint8 // 0..2 (2 represents MaxGeneration ceiling)
	txHigh       uint8 // 0..2
	sendLimit    uint8 // 1..2
	rxHigh       uint8 // 0..2
	recvLimit    uint8 // 1..2
	sendFinalSet bool
	sendFinal    uint8
	recvFinalSet bool
	recvFinal    uint8
}

func checkStreamModelInvariants(s streamModelState) error {
	// I11: Flow control bounds on both halves.
	if s.txHigh > s.sendLimit {
		return fmt.Errorf("I11 violated: txHigh=%d > sendLimit=%d", s.txHigh, s.sendLimit)
	}
	if s.rxHigh > s.recvLimit {
		return fmt.Errorf("I11 violated: rxHigh=%d > recvLimit=%d", s.rxHigh, s.recvLimit)
	}
	// I13: RETIRE_SENT(2) and RETIRED_WAIT_OPEN(4) require both local halves to be terminal (RESET=2 or DONE=3).
	sendTerm := s.sendSt == 2 || s.sendSt == 3
	recvTerm := s.recvSt == 2 || s.recvSt == 3
	if (s.slotSt == 2 || s.slotSt == 4) && (!sendTerm || !recvTerm) {
		return fmt.Errorf("I13 violated: slotSt=%d with non-terminal halves send=%d recv=%d",
			s.slotSt, s.sendSt, s.recvSt)
	}
	return nil
}

func nextStreamModelStates(s streamModelState) []streamModelState {
	// Exhausted slot is absorbing.
	if s.slotSt == 5 {
		return []streamModelState{s}
	}
	var out []streamModelState

	// Open slot from EMPTY(0) or RETIRED_WAIT_OPEN(4)
	if s.slotSt == 0 || s.slotSt == 4 {
		if s.slotSt == 4 && s.gen >= 2 {
			ex := s
			ex.slotSt = 5
			out = append(out, ex)
			return out
		}
		for _, initRole := range []bool{true, false} {
			op := s
			if s.slotSt == 4 {
				op.gen++
			}
			op.slotSt = 1
			op.initiator = initRole
			op.sendSt = 0
			op.recvSt = 0
			op.txHigh = 0
			op.sendLimit = 1
			op.rxHigh = 0
			op.recvLimit = 1
			op.sendFinalSet = false
			op.sendFinal = 0
			op.recvFinalSet = false
			op.recvFinal = 0
			out = append(out, op)
		}
		return out
	}

	// Active or RetirePending: halves can progress
	if s.slotSt == 1 || s.slotSt == 3 {
		// Credit increase
		if s.sendLimit < 2 {
			lim := s
			lim.sendLimit = 2
			out = append(out, lim)
		}
		if s.recvLimit < 2 {
			lim := s
			lim.recvLimit = 2
			out = append(out, lim)
		}
		// Send half progression
		if s.sendSt == 0 {
			if s.txHigh < s.sendLimit {
				w := s
				w.txHigh++
				out = append(out, w)
			}
			fin := s
			fin.sendSt = 1
			fin.sendFinalSet = true
			fin.sendFinal = s.txHigh
			out = append(out, fin)

			rst := s
			rst.sendSt = 2
			rst.sendFinalSet = true
			rst.sendFinal = s.txHigh
			out = append(out, rst)
		}
		if s.sendSt == 1 {
			done := s
			done.sendSt = 3
			out = append(out, done)

			rst := s
			rst.sendSt = 2
			out = append(out, rst)
		}
		// Recv half progression
		if s.recvSt == 0 {
			if s.rxHigh < s.recvLimit {
				r := s
				r.rxHigh++
				out = append(out, r)
			}
			fin := s
			fin.recvSt = 1
			fin.recvFinalSet = true
			fin.recvFinal = s.rxHigh
			out = append(out, fin)

			done := s
			done.recvSt = 3
			done.recvFinalSet = true
			done.recvFinal = s.rxHigh
			out = append(out, done)

			rst := s
			rst.recvSt = 2
			rst.recvFinalSet = true
			rst.recvFinal = s.rxHigh
			out = append(out, rst)
		}
		if s.recvSt == 1 {
			done := s
			done.recvSt = 3
			out = append(out, done)

			rst := s
			rst.recvSt = 2
			out = append(out, rst)
		}
	}

	sendTerm := s.sendSt == 2 || s.sendSt == 3
	recvTerm := s.recvSt == 2 || s.recvSt == 3

	// Initiator retirement: ACTIVE(1) -> RETIRE_SENT(2) when both halves terminal
	if s.slotSt == 1 && s.initiator && sendTerm && recvTerm {
		ret := s
		ret.slotSt = 2
		out = append(out, ret)
	}
	// Peer retirement: ACTIVE(1) -> RETIRE_PENDING(3) on peer STREAM_RETIRE
	if s.slotSt == 1 && !s.initiator {
		pend := s
		pend.slotSt = 3
		out = append(out, pend)
	}
	// Peer RETIRE_PENDING(3) -> RETIRED_WAIT_OPEN(4) once both local halves are terminal
	if s.slotSt == 3 && sendTerm && recvTerm {
		wait := s
		wait.slotSt = 4
		out = append(out, wait)
	}
	// Initiator RETIRE_SENT(2) -> EMPTY(0) with gen++ or EXHAUSTED(5) on RETIRE_ACK
	if s.slotSt == 2 {
		if s.gen >= 2 {
			ex := s
			ex.slotSt = 5
			out = append(out, ex)
		}
		if s.gen < 2 {
			emp := s
			emp.gen++
			emp.slotSt = 0
			emp.sendFinalSet = false
			emp.recvFinalSet = false
			out = append(out, emp)
		}
	}
	return out
}

func verifyStreamLifecycleModel() (ModelReport, error) {
	initSt := streamModelState{sendLimit: 1, recvLimit: 1}
	visited := map[streamModelState]bool{initSt: true}
	queue := []streamModelState{initSt}
	transitions := 0

	for head := 0; head < len(queue); head++ {
		cur := queue[head]
		if err := checkStreamModelInvariants(cur); err != nil {
			return ModelReport{}, err
		}
		for _, nxt := range nextStreamModelStates(cur) {
			transitions++
			// I10: Generation never decreases.
			if nxt.gen < cur.gen {
				return ModelReport{}, fmt.Errorf("I10 violated: stream generation decreased from %d to %d", cur.gen, nxt.gen)
			}
			// I12: Within the same generation and active slot, final_size never changes once bound.
			if nxt.gen == cur.gen && nxt.slotSt == cur.slotSt {
				if cur.sendFinalSet && nxt.sendFinal != cur.sendFinal {
					return ModelReport{}, fmt.Errorf("I12 violated: sendFinal mutated from %d to %d", cur.sendFinal, nxt.sendFinal)
				}
				if cur.recvFinalSet && nxt.recvFinal != cur.recvFinal {
					return ModelReport{}, fmt.Errorf("I12 violated: recvFinal mutated from %d to %d", cur.recvFinal, nxt.recvFinal)
				}
			}
			if !visited[nxt] {
				visited[nxt] = true
				queue = append(queue, nxt)
			}
		}
	}
	return ModelReport{
		ModelName:          "ModelStreamLifecycle",
		ReachableStates:    len(visited),
		TransitionsChecked: transitions,
		InvariantsVerified: []string{"I10", "I11", "I12", "I13"},
	}, nil
}

// ---------------------------------------------------------------------------
// Composed Model 4: Mobile Parking Lease x Fixed Expiry x Wake Readiness
// ---------------------------------------------------------------------------

type mobileParkingState struct {
	parkSt        uint8 // parkIdle(0)..parkAwakePend(4)
	leaseGranted  bool
	initialExpiry uint8 // 0=none, 1=fixed_t1
	pathReady     bool
	creditBacked  bool
	bulkSending   bool
}

func checkMobileParkingInvariants(s mobileParkingState) error {
	// M17: Bulk application sending is forbidden while parkSt != parkIdle(0) or before pathReady && creditBacked.
	if s.bulkSending && (s.parkSt != 0 || !s.pathReady || !s.creditBacked) {
		return fmt.Errorf("M17 violated: bulkSending=true in parkSt=%d pathReady=%v creditBacked=%v",
			s.parkSt, s.pathReady, s.creditBacked)
	}
	return nil
}

func nextMobileParkingStates(s mobileParkingState) []mobileParkingState {
	var out []mobileParkingState

	switch s.parkSt {
	case 0: // IDLE
		// Can send bulk data only if pathReady && creditBacked
		if s.pathReady && s.creditBacked && !s.bulkSending {
			b := s
			b.bulkSending = true
			out = append(out, b)
		}
		if s.bulkSending {
			stop := s
			stop.bulkSending = false
			out = append(out, stop)
		}
		// Request parking when quiescent (!bulkSending)
		if !s.bulkSending {
			req := s
			req.parkSt = 1
			out = append(out, req)
		}
	case 1: // REQ_WAIT
		// Grant accepted: fixes initialExpiry = 1
		gr := s
		gr.parkSt = 2
		gr.leaseGranted = true
		gr.initialExpiry = 1
		out = append(out, gr)

		// Grant refused: returns to IDLE
		ref := s
		ref.parkSt = 0
		out = append(out, ref)

		// Uncertain grant followed by wake
		wk := s
		wk.parkSt = 3
		out = append(out, wk)
	case 2: // LIVE
		// Duplicate grant/request arrives: MUST keep initialExpiry = 1 (M15)
		dup := s
		out = append(out, dup)

		// Network changes while parked (path loses readiness until revalidated)
		if s.pathReady {
			chg := s
			chg.pathReady = false
			out = append(out, chg)
		}
		// Initiate wake
		wk := s
		wk.parkSt = 3
		out = append(out, wk)
	case 3: // WAKING
		// Revalidate path
		if !s.pathReady {
			rdy := s
			rdy.pathReady = true
			out = append(out, rdy)
		}
		// Restore credit backing
		if !s.creditBacked {
			cb := s
			cb.creditBacked = true
			out = append(out, cb)
		}
		// Server sends PARK_AWAKE only when pathReady && creditBacked
		if s.pathReady && s.creditBacked {
			aw := s
			aw.parkSt = 4
			out = append(out, aw)

			done := s
			done.parkSt = 0
			done.leaseGranted = false
			done.initialExpiry = 0
			out = append(out, done)
		}
	case 4: // AWAKE_PEND
		done := s
		done.parkSt = 0
		done.leaseGranted = false
		done.initialExpiry = 0
		out = append(out, done)
	}
	return out
}

func verifyMobileParkingModel() (ModelReport, error) {
	initSt := mobileParkingState{pathReady: true, creditBacked: true}
	visited := map[mobileParkingState]bool{initSt: true}
	queue := []mobileParkingState{initSt}
	transitions := 0

	for head := 0; head < len(queue); head++ {
		cur := queue[head]
		if err := checkMobileParkingInvariants(cur); err != nil {
			return ModelReport{}, err
		}
		for _, nxt := range nextMobileParkingStates(cur) {
			transitions++
			// M15: While leaseGranted remains true, initialExpiry must never change.
			if cur.leaseGranted && nxt.leaseGranted && nxt.initialExpiry != cur.initialExpiry {
				return ModelReport{}, fmt.Errorf("M15 violated: lease expiry mutated from %d to %d",
					cur.initialExpiry, nxt.initialExpiry)
			}
			if !visited[nxt] {
				visited[nxt] = true
				queue = append(queue, nxt)
			}
		}
	}
	return ModelReport{
		ModelName:          "ModelMobileParking",
		ReachableStates:    len(visited),
		TransitionsChecked: transitions,
		InvariantsVerified: []string{"M15", "M17"},
	}, nil
}

// ---------------------------------------------------------------------------
// Composed Model 5: Sent Ledger x In-Flight Accounting
//
// Two packet-number records step through the compiled SentPacket table;
// bytes_in_flight moves only on the ActChargeInFlight/ActReleaseInFlight
// bits of the cell taken. An IR edge that forgets to release (or releases
// twice) breaks I14 here.
// ---------------------------------------------------------------------------

const (
	sentEvSubmitIO   = 1
	sentEvIORejected = 4
	sentEvAck        = 5
	sentStSubmitted  = 1
	sentStBurned     = 3
)

type ledgerAccountingState struct {
	pkt      [2]uint8 // SentPacket state IDs
	inFlight uint8
}

func stepLedgerPacket(
	spec MachineSpec,
	cm *CompiledMachine,
	cur ledgerAccountingState,
	i, ev int,
	charge, release, ackInvalid, ioPending uint16,
) (ledgerAccountingState, bool, error) {
	st := cur.pkt[i]
	cell := cm.Table[st][ev]
	bit := uint16(1) << st
	if ev == sentEvAck {
		// I08: an ACK covering RESERVED/BURNED/UNUSED is ACK_RANGE_ERROR.
		if ackInvalid&bit != 0 && cell.Disp != DispProtocolError {
			return cur, false, fmt.Errorf("I08 violated: ACK accepted in state %s", spec.States[st].Name)
		}
		// A01: an ACK covering SUBMITTED waits for I/O acceptance.
		if ioPending&bit != 0 && (cell.Disp == DispProtocolError || cell.Next != st) {
			return cur, false, fmt.Errorf("A01 violated: ACK on %s is %s -> %s",
				spec.States[st].Name, cell.Disp, spec.States[cell.Next].Name)
		}
	}
	// A04: a rejected submission burns its PN without charging flight.
	badReject := cell.Next != sentStBurned || cell.ActionMask&charge != 0
	if st == sentStSubmitted && ev == sentEvIORejected && badReject {
		return cur, false, fmt.Errorf("A04 violated: SUBMITTED rejection -> %s actions=0x%x",
			spec.States[cell.Next].Name, cell.ActionMask)
	}
	if cell.Disp != DispTransition && cell.Disp != DispStay {
		return cur, false, nil
	}
	n := cur
	n.pkt[i] = cell.Next
	if cell.ActionMask&charge != 0 {
		n.inFlight++
	}
	if cell.ActionMask&release != 0 {
		if n.inFlight == 0 {
			return cur, false, fmt.Errorf("I14 violated: release with zero in flight from %s", spec.States[st].Name)
		}
		n.inFlight--
	}
	return n, true, nil
}

func checkLedgerAccountingInvariants(s ledgerAccountingState) error {
	var want uint8
	for _, st := range s.pkt {
		if st == 2 { // EMITTED
			want++
		}
	}
	if s.inFlight != want {
		return fmt.Errorf("I14 violated: inFlight=%d but emitted count=%d (pkt=%v)", s.inFlight, want, s.pkt)
	}
	return nil
}

func verifyLedgerAccountingModel() (ModelReport, error) {
	return verifyLedgerAccountingModelFor(sentPacketMachine())
}

func verifyLedgerAccountingModelFor(spec MachineSpec) (ModelReport, error) {
	cm, _, err := CompileAndVerifyMachine(spec)
	if err != nil {
		return ModelReport{}, err
	}
	charge := cm.ActionBits["ActChargeInFlight"]
	release := cm.ActionBits["ActReleaseInFlight"]
	ackInvalid := cm.PredicateMasks["AckInvalid"]
	ioPending := cm.PredicateMasks["IoPending"]

	initSt := ledgerAccountingState{pkt: [2]uint8{spec.Initial, spec.Initial}}
	visited := map[ledgerAccountingState]bool{initSt: true}
	queue := []ledgerAccountingState{initSt}
	transitions := 0

	for head := 0; head < len(queue); head++ {
		cur := queue[head]
		if err := checkLedgerAccountingInvariants(cur); err != nil {
			return ModelReport{}, err
		}
		for i := range cur.pkt {
			for ev := range spec.Events {
				n, ok, err := stepLedgerPacket(spec, cm, cur, i, ev, charge, release, ackInvalid, ioPending)
				if err != nil {
					return ModelReport{}, err
				}
				if !ok {
					continue
				}
				transitions++
				if !visited[n] {
					visited[n] = true
					queue = append(queue, n)
				}
			}
		}
	}
	return ModelReport{
		ModelName:          "ModelLedgerAccounting",
		ReachableStates:    len(visited),
		TransitionsChecked: transitions,
		InvariantsVerified: []string{"I08", "I14", "A01", "A04"},
	}, nil
}

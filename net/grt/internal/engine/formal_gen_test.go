package engine

import (
	"testing"

	"gosuda.org/grt/internal/wire"
)

func TestGeneratedFSMAlignmentAndZeroAlloc(t *testing.T) {
	// 1. Verify ConnLifecycle bitmask predicates match all 13 states.
	for s := stNew; s <= stDead; s++ {
		wantKey := s == stCFinishing || s == stSFinishing || s == stEstablished ||
			s == stClosing || s == stDraining || s == stQuiescing
		if got := s.keyBearing(); got != wantKey {
			t.Fatalf("state %s keyBearing() = %v, want %v", s, got, wantKey)
		}
		wantTerm := s == stClosing || s == stDraining || s == stQuiescing || s == stDead
		if got := s.terminating(); got != wantTerm {
			t.Fatalf("state %s terminating() = %v, want %v", s, got, wantTerm)
		}
	}

	// Verify DEAD is absorbing across all events.
	for ev := uint8(0); ev <= fsmConn_EV_SERVER_AUTH_FAILED; ev++ {
		res := fsmConnStep(stDead, ev)
		if connState(res.next) != stDead || res.actions != 0 {
			t.Fatalf("stDead not absorbing on ev=%d: %+v", ev, res)
		}
	}

	// Verify valid transitions on ConnLifecycle.
	if !fsmConnCanTransition(stNew, stCBuild) || !fsmConnCanTransition(stCFinishing, stEstablished) {
		t.Fatal("expected legal ConnLifecycle transitions to be permitted")
	}
	if fsmConnCanTransition(stDead, stEstablished) || fsmConnCanTransition(stNew, stEstablished) {
		t.Fatal("expected illegal ConnLifecycle jump to be rejected")
	}

	// 2. Verify SentPacket ACK verifiability bitmask.
	for _, st := range []sentStatus{sentEmitted, sentAcked, sentLost, sentAbandoned} {
		if !fsmSentIsAckVerifiable(st) {
			t.Fatalf("expected sentStatus %d to be ACK-verifiable", st)
		}
	}
	for _, st := range []sentStatus{sentReserved, sentSubmitted, sentBurned, sentUnused} {
		if fsmSentIsAckVerifiable(st) {
			t.Fatalf("expected sentStatus %d to reject ACK", st)
		}
	}

	// 3. Verify StreamSlot, StreamSend, StreamRecv retirement gate.
	if !fsmEvalStreamHalvesDone(false, sendDone, false, recvDone, false) {
		t.Fatal("expected DONE/DONE halves to satisfy retirement gate")
	}
	if fsmEvalStreamHalvesDone(false, sendOpen, false, recvDone, false) {
		t.Fatal("expected OPEN/DONE halves to block retirement gate")
	}

	// 4. Verify zero allocations across generated hotpath evaluators.
	allocs := testing.AllocsPerRun(1000, func() {
		r1 := fsmConnStep(stEstablished, fsmConn_EV_LOCAL_CLOSE)
		_ = fsmConnIsKeyBearing(connState(r1.next))
		_ = fsmSentIsAckVerifiable(sentEmitted)
		_ = fsmEvalStreamHalvesDone(false, sendDone, false, recvReset, false)
		_ = fsmEvalAmpGate(false, 1200, 1200, 1200, 1, 1, wire.AmpFactor)
		_ = fsmGateTxData(fsmGateMaskTxData)
		_ = fsmGateRxCommit(fsmGateMaskRxCommit)
	})
	if allocs != 0 {
		t.Fatalf("expected 0 allocs/op on generated FSM hotpath, got %v", allocs)
	}
}

func BenchmarkGeneratedFSMHotpath(b *testing.B) {
	b.ReportAllocs()
	var acc uint8
	for i := 0; i < b.N; i++ {
		step := fsmConnStep(stEstablished, fsmConn_EV_LOCAL_CLOSE)
		if fsmConnIsKeyBearing(connState(step.next)) &&
			fsmSentIsAckVerifiable(sentEmitted) &&
			fsmEvalStreamHalvesDone(false, sendDone, false, recvDone, false) &&
			fsmEvalAmpGate(false, 1200, 1200, 1200, 1, 1, wire.AmpFactor) {
			acc ^= step.next
		}
	}
	if acc == 255 {
		b.Fatal("unreachable")
	}
}

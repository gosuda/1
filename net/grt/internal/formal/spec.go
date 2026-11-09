package formal

// AllMachines returns the canonical Intermediate Representation (IR) for all 11
// orthogonal GRT/1.1 finite-state machines, bound directly to internal/engine
// state types.
func AllMachines() []MachineSpec {
	return []MachineSpec{
		connLifecycleMachine(),
		cipherSelectionMachine(),
		streamSlotMachine(),
		streamSendMachine(),
		streamRecvMachine(),
		epochSlotMachine(),
		pathValidationMachine(),
		pathRoleMachine(),
		sentPacketMachine(),
		parkLeaseMachine(),
		ecnValidationMachine(),
	}
}

func connLifecycleMachine() MachineSpec {
	return MachineSpec{
		Name:        "ConnLifecycle",
		GoPrefix:    "fsmConn",
		StateType:   "connState",
		SpecSection: "20.1-20.3",
		Initial:     0,
		DefaultDisp: DispIgnore,
		States: []StateSpec{
			{ID: 0, Name: "NEW", EngineConst: "stNew", Predicates: []string{"HandshakePhase"}},
			{ID: 1, Name: "C_BUILD", EngineConst: "stCBuild", Predicates: []string{"HandshakePhase"}},
			{ID: 2, Name: "C_WAIT_REPLY", EngineConst: "stCWaitReply", Predicates: []string{"HandshakePhase"}},
			{ID: 3, Name: "C_VERIFY", EngineConst: "stCVerify", Predicates: []string{"HandshakePhase"}},
			{ID: 4, Name: "C_FINISHING", EngineConst: "stCFinishing", Predicates: []string{"KeyBearing", "LiveKeyed", "AppSendEligible"}},
			{ID: 5, Name: "S_COLLECT", EngineConst: "stSCollect", Predicates: []string{"HandshakePhase"}},
			{ID: 6, Name: "S_BUILD", EngineConst: "stSBuild", Predicates: []string{"HandshakePhase"}},
			{ID: 7, Name: "S_FINISHING", EngineConst: "stSFinishing", Predicates: []string{"KeyBearing", "LiveKeyed", "HandshakePhase"}},
			{ID: 8, Name: "ESTABLISHED", EngineConst: "stEstablished", Predicates: []string{"KeyBearing", "LiveKeyed", "AppSendEligible"}},
			{ID: 9, Name: "CLOSING", EngineConst: "stClosing", Predicates: []string{"KeyBearing", "Terminating"}},
			{ID: 10, Name: "DRAINING", EngineConst: "stDraining", Predicates: []string{"KeyBearing", "Terminating", "QuietTerminal"}},
			{ID: 11, Name: "QUIESCING", EngineConst: "stQuiescing", Predicates: []string{"KeyBearing", "Terminating", "QuietTerminal"}},
			{ID: 12, Name: "DEAD", EngineConst: "stDead", Terminal: true, Predicates: []string{"Terminating", "QuietTerminal"}},
		},
		Events: []EventSpec{
			{ID: 0, Name: "EV_CLIENT_START"},
			{ID: 1, Name: "EV_CLIENT_HELLO_BUILT"},
			{ID: 2, Name: "EV_RETRY_RECEIVED"},
			{ID: 3, Name: "EV_SERVER_HELLO_REASSEMBLED"},
			{ID: 4, Name: "EV_SERVER_AUTH_VERIFIED"},
			{ID: 5, Name: "EV_HANDSHAKE_DONE_RX"},
			{ID: 6, Name: "EV_SERVER_ACCEPT"},
			{ID: 7, Name: "EV_CLIENT_AUTH_REASSEMBLED"},
			{ID: 8, Name: "EV_SERVER_HELLO_BUILT"},
			{ID: 9, Name: "EV_FINISH_VERIFIED"},
			{ID: 10, Name: "EV_LOCAL_CLOSE"},
			{ID: 11, Name: "EV_REMOTE_CLOSE"},
			{ID: 12, Name: "EV_CLOSE_TIMEOUT"},
			{ID: 13, Name: "EV_IO_PINS_DRAINED"},
			{ID: 14, Name: "EV_LOCAL_FATAL"},
			{ID: 15, Name: "EV_PROTOCOL_ERROR"},
			{ID: 16, Name: "EV_SERVER_AUTH_FAILED"},
		},
		Predicates: []PredicateSpec{
			{Name: "KeyBearing", GoFunc: "fsmConnIsKeyBearing", Doc: "reports whether the connection state holds installed epoch keys (Section 20.1)."},
			{Name: "LiveKeyed", GoFunc: "fsmConnIsLiveKeyed", Doc: "reports whether the state is key-bearing and not terminating: APP_CLOSE and authenticated protocol errors enter CLOSING only from here (Section 20.2 rules 4-5)."},
			{Name: "Terminating", GoFunc: "fsmConnIsTerminating", Doc: "reports whether the connection is in CLOSING, DRAINING, QUIESCING, or DEAD."},
			{Name: "AppSendEligible", GoFunc: "fsmConnIsAppSendEligible", Doc: "reports whether application STREAM/DATAGRAM frames may be emitted."},
			{Name: "HandshakePhase", GoFunc: "fsmConnIsHandshakePhase", Doc: "reports whether the connection is still in pre-established handshake progression."},
			{Name: "QuietTerminal", GoFunc: "fsmConnIsQuietTerminal", Doc: "reports whether the connection must suppress all further wire emissions."},
		},
		Actions: []ActionFlag{
			{Name: "ActInstallEpoch1", Bit: 1 << 0},
			{Name: "ActEmitClose", Bit: 1 << 1},
			{Name: "ActArmCloseTimer", Bit: 1 << 2},
			{Name: "ActZeroizeSecrets", Bit: 1 << 3},
			{Name: "ActWakeApp", Bit: 1 << 4},
		},
		Transitions: connLifecycleTransitions(),
	}
}

// connLifecycleTransitions is the Section 20.3 table plus the Section 20.2
// global rules that apply uniformly across state classes.
func connLifecycleTransitions() []TransitionSpec {
	const (
		stNew, stCBuild, stCWaitReply, stCVerify, stCFinishing = 0, 1, 2, 3, 4
		stSCollect, stSBuild, stSFinishing, stEstablished      = 5, 6, 7, 8
		stClosing, stDraining, stQuiescing, stDead             = 9, 10, 11, 12
	)
	const (
		evClientStart, evHelloBuilt, evRetry, evServerHello, evAuthOK = 0, 1, 2, 3, 4
		evHandshakeDone, evAccept, evClientFlight, evServerBuilt      = 5, 6, 7, 8
		evFinish, evLocalClose, evRemoteClose, evCloseTimeout         = 9, 10, 11, 12
		evPinsDrained, evLocalFatal, evProtocolError, evAuthFail      = 13, 14, 15, 16
	)
	t := []TransitionSpec{
		{From: stNew, Event: evClientStart, To: stCBuild, Disp: DispTransition, SpecRef: "20.3"},
		{From: stNew, Event: evAccept, To: stSCollect, Disp: DispTransition, SpecRef: "20.3"},
		{From: stCBuild, Event: evHelloBuilt, To: stCWaitReply, Disp: DispTransition, SpecRef: "20.3"},
		{From: stCWaitReply, Event: evRetry, To: stCWaitReply, Disp: DispStay, SpecRef: "6.3, 20.3"},
		{From: stCWaitReply, Event: evServerHello, To: stCVerify, Disp: DispTransition, SpecRef: "20.3"},
		{From: stCVerify, Event: evAuthOK, To: stCFinishing, Disp: DispTransition, Actions: []string{"ActInstallEpoch1", "ActWakeApp"}, SpecRef: "20.3"},
		{From: stCVerify, Event: evAuthFail, To: stCWaitReply, Disp: DispTransition, SpecRef: "20.3"},
		{From: stCFinishing, Event: evHandshakeDone, To: stEstablished, Disp: DispTransition, Actions: []string{"ActWakeApp"}, SpecRef: "20.3"},
		{From: stSCollect, Event: evClientFlight, To: stSBuild, Disp: DispTransition, SpecRef: "20.3"},
		{From: stSBuild, Event: evServerBuilt, To: stSFinishing, Disp: DispTransition, Actions: []string{"ActInstallEpoch1"}, SpecRef: "20.3"},
		{From: stSFinishing, Event: evFinish, To: stEstablished, Disp: DispTransition, Actions: []string{"ActWakeApp"}, SpecRef: "20.3"},
		{From: stClosing, Event: evCloseTimeout, To: stQuiescing, Disp: DispTransition, SpecRef: "20.3"},
		{From: stDraining, Event: evCloseTimeout, To: stQuiescing, Disp: DispTransition, SpecRef: "20.3"},
		{From: stQuiescing, Event: evPinsDrained, To: stDead, Disp: DispTransition, Actions: []string{"ActZeroizeSecrets", "ActWakeApp"}, SpecRef: "20.3"},
	}
	// Rules 4-5: in a live key-bearing state APP_CLOSE and authenticated
	// protocol errors enter CLOSING, a valid peer CLOSE enters DRAINING.
	// CLOSING/DRAINING/QUIESCING ignore all three (default disposition):
	// a draining endpoint never re-enters CLOSING and its deadline never
	// extends.
	for _, s := range []uint8{stCFinishing, stSFinishing, stEstablished} {
		t = append(t,
			TransitionSpec{From: s, Event: evLocalClose, To: stClosing, Disp: DispTransition, Actions: []string{"ActEmitClose", "ActArmCloseTimer", "ActWakeApp"}, SpecRef: "20.2"},
			TransitionSpec{From: s, Event: evProtocolError, To: stClosing, Disp: DispTransition, Actions: []string{"ActEmitClose", "ActArmCloseTimer", "ActWakeApp"}, SpecRef: "20.2"},
			TransitionSpec{From: s, Event: evRemoteClose, To: stDraining, Disp: DispTransition, Actions: []string{"ActArmCloseTimer", "ActWakeApp"}, SpecRef: "20.2"},
		)
	}
	// Without a safe sending key the same events enter QUIESCING silently.
	for _, s := range []uint8{stNew, stCBuild, stCWaitReply, stCVerify, stSCollect, stSBuild} {
		t = append(t,
			TransitionSpec{From: s, Event: evLocalClose, To: stQuiescing, Disp: DispTransition, Actions: []string{"ActWakeApp"}, SpecRef: "20.2"},
			TransitionSpec{From: s, Event: evProtocolError, To: stQuiescing, Disp: DispTransition, Actions: []string{"ActWakeApp"}, SpecRef: "20.2"},
		)
	}
	// Rules 3 and 5: local faults, establishment/idle/lease expiry, AEAD
	// and invalid-authentication limits stop emission from any state that
	// is not already quiescing.
	for s := uint8(stNew); s <= stDraining; s++ {
		t = append(t, TransitionSpec{From: s, Event: evLocalFatal, To: stQuiescing, Disp: DispTransition, Actions: []string{"ActWakeApp"}, SpecRef: "20.2"})
	}
	return t
}

func cipherSelectionMachine() MachineSpec {
	return MachineSpec{
		Name:        "CipherSelection",
		GoPrefix:    "fsmCipher",
		StateType:   "uint8",
		SpecSection: "4.6",
		Initial:     0,
		DefaultDisp: DispProtocolError,
		States: []StateSpec{
			{ID: 0, Name: "UNRESOLVED", EngineConst: "cipherUnresolved"},
			{ID: 1, Name: "LOCKED", EngineConst: "cipherLocked", Predicates: []string{"Selected"}},
			{ID: 2, Name: "FAILED", EngineConst: "cipherFailed", Terminal: true, Predicates: []string{"Terminal"}},
		},
		Events: []EventSpec{
			{ID: 0, Name: "EV_SELECT_VALID_SUITE"},
			{ID: 1, Name: "EV_CONFIRM_MATCHING_SUITE"},
			{ID: 2, Name: "EV_SUITE_MISMATCH_OR_UNSUPPORTED"},
		},
		Predicates: []PredicateSpec{
			{Name: "Selected", GoFunc: "fsmCipherIsSelected", Doc: "reports whether the connection's AEAD cipher suite is locked for the connection lifetime."},
			{Name: "Terminal", GoFunc: "fsmCipherIsTerminal", Doc: "reports whether cipher negotiation has permanently failed."},
		},
		Actions: []ActionFlag{
			{Name: "ActPinSuite", Bit: 1 << 0},
			{Name: "ActRejectHandshake", Bit: 1 << 1},
		},
		Transitions: []TransitionSpec{
			{From: 0, Event: 0, To: 1, Disp: DispTransition, Actions: []string{"ActPinSuite"}, SpecRef: "4.6"},
			{From: 0, Event: 2, To: 2, Disp: DispTransition, Actions: []string{"ActRejectHandshake"}, SpecRef: "4.6"},
			{From: 1, Event: 1, To: 1, Disp: DispStay, SpecRef: "4.6"},
			{From: 1, Event: 2, To: 2, Disp: DispTransition, Actions: []string{"ActRejectHandshake"}, SpecRef: "4.6"},
		},
	}
}

func streamSlotMachine() MachineSpec {
	return MachineSpec{
		Name:        "StreamSlot",
		GoPrefix:    "fsmSlot",
		StateType:   "slotState",
		SpecSection: "12.1, 21.1",
		Initial:     0,
		DefaultDisp: DispIgnore,
		States: []StateSpec{
			{ID: 0, Name: "EMPTY", EngineConst: "slotEmpty", Predicates: []string{"OpenEligible"}},
			{ID: 1, Name: "ACTIVE", EngineConst: "slotActive", Predicates: []string{"ActiveOrRetiring", "HoldsBuffers"}},
			{ID: 2, Name: "RETIRE_SENT", EngineConst: "slotRetireSent", Predicates: []string{"ActiveOrRetiring", "RetireInFlight"}},
			{ID: 3, Name: "RETIRE_PENDING", EngineConst: "slotRetirePending", Predicates: []string{"ActiveOrRetiring", "RetireInFlight", "HoldsBuffers"}},
			{ID: 4, Name: "RETIRED_WAIT_OPEN", EngineConst: "slotRetiredWaitOpen", Predicates: []string{"OpenEligible"}},
			{ID: 5, Name: "EXHAUSTED", EngineConst: "slotExhausted", Terminal: true, Predicates: []string{"Terminal"}},
		},
		Events: []EventSpec{
			{ID: 0, Name: "EV_OPEN"},
			{ID: 1, Name: "EV_LOCAL_HALVES_DONE_INITIATOR"},
			{ID: 2, Name: "EV_RX_RETIRE_PEER"},
			{ID: 3, Name: "EV_LOCAL_HALVES_DONE_PEER"},
			{ID: 4, Name: "EV_RX_RETIRE_ACK_NEXT_GEN"},
			{ID: 5, Name: "EV_RX_RETIRE_ACK_MAX_GEN"},
			{ID: 6, Name: "EV_NEXT_GEN_OPEN_MAX_REACHED"},
		},
		Predicates: []PredicateSpec{
			{Name: "OpenEligible", GoFunc: "fsmSlotIsOpenEligible", Doc: "reports whether the slot can open a new stream generation."},
			{Name: "ActiveOrRetiring", GoFunc: "fsmSlotIsActiveOrRetiring", Doc: "reports whether the slot is currently occupied by an active or retiring generation."},
			{Name: "RetireInFlight", GoFunc: "fsmSlotIsRetireInFlight", Doc: "reports whether retirement handshake frames are in progress."},
			{Name: "HoldsBuffers", GoFunc: "fsmSlotHoldsBuffers", Doc: "reports whether the slot may retain active send/recv buffers."},
			{Name: "Terminal", GoFunc: "fsmSlotIsTerminal", Doc: "reports whether the slot has exhausted all 2^32 generations."},
		},
		Actions: []ActionFlag{
			{Name: "ActInitHalves", Bit: 1 << 0},
			{Name: "ActQueueRetire", Bit: 1 << 1},
			{Name: "ActQueueRetireAck", Bit: 1 << 2},
			{Name: "ActBumpGen", Bit: 1 << 3},
		},
		Transitions: []TransitionSpec{
			{From: 0, Event: 0, To: 1, Disp: DispTransition, Actions: []string{"ActInitHalves"}, SpecRef: "21.1"},
			{From: 1, Event: 1, To: 2, Disp: DispTransition, Actions: []string{"ActQueueRetire"}, SpecRef: "21.1"},
			{From: 1, Event: 2, To: 3, Disp: DispTransition, SpecRef: "21.1"},
			{From: 3, Event: 3, To: 4, Disp: DispTransition, Actions: []string{"ActQueueRetireAck"}, SpecRef: "21.1"},
			{From: 4, Event: 0, To: 1, Disp: DispTransition, Actions: []string{"ActBumpGen", "ActInitHalves"}, SpecRef: "21.1"},
			{From: 4, Event: 6, To: 5, Disp: DispTransition, SpecRef: "21.1"},
			{From: 2, Event: 4, To: 0, Disp: DispTransition, Actions: []string{"ActBumpGen"}, SpecRef: "21.1"},
			{From: 2, Event: 5, To: 5, Disp: DispTransition, SpecRef: "21.1"},
		},
	}
}

func streamSendMachine() MachineSpec {
	return MachineSpec{
		Name:        "StreamSend",
		GoPrefix:    "fsmSend",
		StateType:   "sendState",
		SpecSection: "12.5, 21.1",
		Initial:     0,
		DefaultDisp: DispIgnore,
		States: []StateSpec{
			{ID: 0, Name: "OPEN", EngineConst: "sendOpen", Predicates: []string{"Writable", "Retransmittable"}},
			{ID: 1, Name: "FIN", EngineConst: "sendFin", Predicates: []string{"FinalSizeBound", "Retransmittable"}},
			{ID: 2, Name: "RESET", EngineConst: "sendReset", Predicates: []string{"FinalSizeBound", "Terminal"}},
			{ID: 3, Name: "DONE", EngineConst: "sendDone", Terminal: true, Predicates: []string{"FinalSizeBound", "Terminal"}},
		},
		Events: []EventSpec{
			{ID: 0, Name: "EV_APP_WRITE"},
			{ID: 1, Name: "EV_APP_FIN"},
			{ID: 2, Name: "EV_ALL_BYTES_AND_FIN_ACKED"},
			{ID: 3, Name: "EV_RESET_OR_STOP_SENDING"},
		},
		Predicates: []PredicateSpec{
			{Name: "Writable", GoFunc: "fsmSendIsWritable", Doc: "reports whether the send half accepts new application writes."},
			{Name: "Retransmittable", GoFunc: "fsmSendIsRetransmittable", Doc: "reports whether lost ranges on this half should be retransmitted."},
			{Name: "FinalSizeBound", GoFunc: "fsmSendIsFinalSizeBound", Doc: "reports whether final_size is frozen for this send half."},
			{Name: "Terminal", GoFunc: "fsmSendIsTerminal", Doc: "reports whether the send half has reached a terminal state (DONE or RESET)."},
		},
		Actions: []ActionFlag{
			{Name: "ActFreezeFinalSize", Bit: 1 << 0},
			{Name: "ActQueueReset", Bit: 1 << 1},
			{Name: "ActCheckSlotRetire", Bit: 1 << 2},
		},
		Transitions: []TransitionSpec{
			{From: 0, Event: 0, To: 0, Disp: DispStay, SpecRef: "12.5"},
			{From: 0, Event: 1, To: 1, Disp: DispTransition, Actions: []string{"ActFreezeFinalSize"}, SpecRef: "12.5"},
			{From: 0, Event: 3, To: 2, Disp: DispTransition, Actions: []string{"ActFreezeFinalSize", "ActQueueReset", "ActCheckSlotRetire"}, SpecRef: "12.5"},
			{From: 1, Event: 2, To: 3, Disp: DispTransition, Actions: []string{"ActCheckSlotRetire"}, SpecRef: "12.5"},
			{From: 1, Event: 3, To: 2, Disp: DispTransition, Actions: []string{"ActQueueReset", "ActCheckSlotRetire"}, SpecRef: "12.5"},
			{From: 2, Event: 2, To: 3, Disp: DispTransition, Actions: []string{"ActCheckSlotRetire"}, SpecRef: "12.5"},
		},
	}
}

func streamRecvMachine() MachineSpec {
	return MachineSpec{
		Name:        "StreamRecv",
		GoPrefix:    "fsmRecv",
		StateType:   "recvState",
		SpecSection: "12.6, 21.1",
		Initial:     0,
		DefaultDisp: DispIgnore,
		States: []StateSpec{
			{ID: 0, Name: "OPEN", EngineConst: "recvOpen", Predicates: []string{"AcceptsData"}},
			{ID: 1, Name: "FIN", EngineConst: "recvFin", Predicates: []string{"AcceptsData", "FinalSizeBound"}},
			{ID: 2, Name: "RESET", EngineConst: "recvReset", Terminal: true, Predicates: []string{"FinalSizeBound", "Terminal"}},
			{ID: 3, Name: "DONE", EngineConst: "recvDone", Terminal: true, Predicates: []string{"FinalSizeBound", "Terminal"}},
		},
		Events: []EventSpec{
			{ID: 0, Name: "EV_RX_DATA"},
			{ID: 1, Name: "EV_RX_FIN_WITH_GAPS"},
			{ID: 2, Name: "EV_RX_CONTIGUOUS_TO_FINAL"},
			{ID: 3, Name: "EV_RX_RESET_STREAM"},
		},
		Predicates: []PredicateSpec{
			{Name: "AcceptsData", GoFunc: "fsmRecvAcceptsData", Doc: "reports whether incoming STREAM frames may insert into the reassembly buffer."},
			{Name: "FinalSizeBound", GoFunc: "fsmRecvIsFinalSizeBound", Doc: "reports whether the peer's final_size is known and immutable."},
			{Name: "Terminal", GoFunc: "fsmRecvIsTerminal", Doc: "reports whether the receive half has reached a terminal state (DONE or RESET)."},
		},
		Actions: []ActionFlag{
			{Name: "ActRecordFinalSize", Bit: 1 << 0},
			{Name: "ActDiscardReassembly", Bit: 1 << 1},
			{Name: "ActCheckSlotRetire", Bit: 1 << 2},
		},
		Transitions: []TransitionSpec{
			{From: 0, Event: 0, To: 0, Disp: DispStay, SpecRef: "12.6"},
			{From: 0, Event: 1, To: 1, Disp: DispTransition, Actions: []string{"ActRecordFinalSize"}, SpecRef: "12.6"},
			{From: 0, Event: 2, To: 3, Disp: DispTransition, Actions: []string{"ActRecordFinalSize", "ActCheckSlotRetire"}, SpecRef: "12.6"},
			{From: 0, Event: 3, To: 2, Disp: DispTransition, Actions: []string{"ActRecordFinalSize", "ActDiscardReassembly", "ActCheckSlotRetire"}, SpecRef: "12.6"},
			{From: 1, Event: 0, To: 1, Disp: DispStay, SpecRef: "12.6"},
			{From: 1, Event: 2, To: 3, Disp: DispTransition, Actions: []string{"ActCheckSlotRetire"}, SpecRef: "12.6"},
			{From: 1, Event: 3, To: 2, Disp: DispTransition, Actions: []string{"ActDiscardReassembly", "ActCheckSlotRetire"}, SpecRef: "12.6"},
		},
	}
}

func epochSlotMachine() MachineSpec {
	return MachineSpec{
		Name:        "EpochSlot",
		GoPrefix:    "fsmEpochSlot",
		StateType:   "epochSlotState",
		SpecSection: "19.1, 21.2",
		Initial:     0,
		DefaultDisp: DispIgnore,
		States: []StateSpec{
			{ID: 0, Name: "EMPTY", EngineConst: "epochSlotEmpty"},
			{ID: 1, Name: "INSTALLED", EngineConst: "epochSlotInstalled", Predicates: []string{"DecryptEligible", "SealEligible"}},
			{ID: 2, Name: "GRACE", EngineConst: "epochSlotGrace", Predicates: []string{"DecryptEligible"}},
		},
		Events: []EventSpec{
			{ID: 0, Name: "EV_STAGE_OR_INSTALL"},
			{ID: 1, Name: "EV_DEMOTE_TO_PREV_GRACE"},
			{ID: 2, Name: "EV_GRACE_EXPIRED_OR_SUPERSEDED"},
		},
		Predicates: []PredicateSpec{
			{Name: "DecryptEligible", GoFunc: "fsmEpochSlotIsDecryptEligible", Doc: "reports whether the key slot may authenticate inbound DATA packets."},
			{Name: "SealEligible", GoFunc: "fsmEpochSlotIsSealEligible", Doc: "reports whether the key slot may seal outbound DATA packets."},
		},
		Actions: []ActionFlag{
			{Name: "ActInstallKeys", Bit: 1 << 0},
			{Name: "ActArmGraceTimer", Bit: 1 << 1},
			{Name: "ActZeroizeKeys", Bit: 1 << 2},
		},
		Transitions: []TransitionSpec{
			{From: 0, Event: 0, To: 1, Disp: DispTransition, Actions: []string{"ActInstallKeys"}, SpecRef: "19.4"},
			{From: 1, Event: 1, To: 2, Disp: DispTransition, Actions: []string{"ActArmGraceTimer"}, SpecRef: "19.5"},
			{From: 1, Event: 2, To: 0, Disp: DispTransition, Actions: []string{"ActZeroizeKeys"}, SpecRef: "19.5"},
			{From: 2, Event: 2, To: 0, Disp: DispTransition, Actions: []string{"ActZeroizeKeys"}, SpecRef: "19.5"},
		},
	}
}

func pathValidationMachine() MachineSpec {
	return MachineSpec{
		Name:        "PathValidation",
		GoPrefix:    "fsmPath",
		StateType:   "pathSt",
		SpecSection: "17.1-17.7, 21.3",
		Initial:     0,
		DefaultDisp: DispIgnore,
		States: []StateSpec{
			{ID: 0, Name: "FREE", EngineConst: "psFree"},
			{ID: 1, Name: "VALIDATING", EngineConst: "psValidating", Predicates: []string{"ControlSendEligible", "ValidationInFlight"}},
			{ID: 2, Name: "REACHABLE", EngineConst: "psReachable", Predicates: []string{"ControlSendEligible", "ReturnRoutable", "ValidationInFlight"}},
			{ID: 3, Name: "READY", EngineConst: "psReady", Predicates: []string{"ControlSendEligible", "ReturnRoutable", "BaseMTUValidated"}},
			{ID: 4, Name: "ACTIVE", EngineConst: "psActive", Predicates: []string{"ControlSendEligible", "ReturnRoutable", "BaseMTUValidated", "BulkDataEligible"}},
			{ID: 5, Name: "OLD", EngineConst: "psOld", Predicates: []string{"ControlSendEligible", "ReturnRoutable", "BaseMTUValidated"}},
			{ID: 6, Name: "FAILED", EngineConst: "psFailed", Predicates: []string{"FailedOrFree"}},
		},
		Events: []EventSpec{
			{ID: 0, Name: "EV_ALLOCATE_CANDIDATE"},
			{ID: 1, Name: "EV_BOOTSTRAP_ACTIVE"},
			{ID: 2, Name: "EV_RX_SMALL_RESPONSE"},
			{ID: 3, Name: "EV_RX_1200B_RESPONSE"},
			{ID: 4, Name: "EV_PROMOTE_TO_ACTIVE"},
			{ID: 5, Name: "EV_DEMOTE_TO_OLD"},
			{ID: 6, Name: "EV_VALIDATION_OR_PATH_FAIL"},
			{ID: 7, Name: "EV_RELEASE_SLOT"},
			{ID: 8, Name: "EV_REVALIDATE"},
			{ID: 9, Name: "EV_RETAINED_REVALIDATION_EXPIRED"},
			{ID: 10, Name: "EV_FALLBACK_TO_RETAINED"},
		},
		Predicates: []PredicateSpec{
			{Name: "ControlSendEligible", GoFunc: "fsmPathIsControlSendEligible", Doc: "reports whether validation/control frames may be emitted on the path."},
			{Name: "BulkDataEligible", GoFunc: "fsmPathIsBulkDataEligible", Doc: "reports whether application STREAM/DATAGRAM payload may be emitted on the path."},
			{Name: "ReturnRoutable", GoFunc: "fsmPathIsReturnRoutable", Doc: "reports whether return routability has been proven via PATH_RESPONSE."},
			{Name: "BaseMTUValidated", GoFunc: "fsmPathIsBaseMTUValidated", Doc: "reports whether bidirectional 1200-byte base MTU is confirmed (Section 17.2)."},
			{Name: "ValidationInFlight", GoFunc: "fsmPathIsValidationInFlight", Doc: "reports whether PATH_CHALLENGE retries remain active on this path."},
			{Name: "FailedOrFree", GoFunc: "fsmPathIsFailedOrFree", Doc: "reports whether the path slot is failed or unallocated."},
		},
		Actions: []ActionFlag{
			{Name: "ActIssueChallenge", Bit: 1 << 0},
			{Name: "ActLiftAmpGuard", Bit: 1 << 1},
			{Name: "ActConfirm1200MTU", Bit: 1 << 2},
			{Name: "ActInstallActive", Bit: 1 << 3},
			{Name: "ActAbandonInFlight", Bit: 1 << 4},
		},
		Transitions: []TransitionSpec{
			{From: 0, Event: 0, To: 1, Disp: DispTransition, Actions: []string{"ActIssueChallenge"}, SpecRef: "17.3"},
			{From: 0, Event: 1, To: 4, Disp: DispTransition, Actions: []string{"ActLiftAmpGuard", "ActConfirm1200MTU", "ActInstallActive"}, SpecRef: "17.2"},
			{From: 1, Event: 2, To: 2, Disp: DispTransition, Actions: []string{"ActLiftAmpGuard", "ActIssueChallenge"}, SpecRef: "17.2"},
			{From: 1, Event: 3, To: 3, Disp: DispTransition, Actions: []string{"ActLiftAmpGuard", "ActConfirm1200MTU"}, SpecRef: "17.2"},
			{From: 2, Event: 3, To: 3, Disp: DispTransition, Actions: []string{"ActConfirm1200MTU"}, SpecRef: "17.2"},
			{From: 3, Event: 4, To: 4, Disp: DispTransition, Actions: []string{"ActInstallActive"}, SpecRef: "17.4"},
			{From: 4, Event: 5, To: 5, Disp: DispTransition, SpecRef: "17.5"},
			{From: 1, Event: 6, To: 6, Disp: DispTransition, Actions: []string{"ActAbandonInFlight"}, SpecRef: "17.3"},
			{From: 2, Event: 6, To: 6, Disp: DispTransition, Actions: []string{"ActAbandonInFlight"}, SpecRef: "17.3"},
			{From: 3, Event: 6, To: 6, Disp: DispTransition, Actions: []string{"ActAbandonInFlight"}, SpecRef: "17.3"},
			{From: 5, Event: 6, To: 6, Disp: DispTransition, Actions: []string{"ActAbandonInFlight"}, SpecRef: "17.5"},
			{From: 5, Event: 7, To: 0, Disp: DispTransition, Actions: []string{"ActAbandonInFlight"}, SpecRef: "17.5"},
			{From: 6, Event: 7, To: 0, Disp: DispTransition, SpecRef: "17.5"},
			// Parked-path wake / resume needs fresh proof (17.7 ACTIVE/OLD row).
			{From: 4, Event: 8, To: 1, Disp: DispTransition, Actions: []string{"ActIssueChallenge"}, SpecRef: "17.7"},
			{From: 5, Event: 8, To: 1, Disp: DispTransition, Actions: []string{"ActIssueChallenge"}, SpecRef: "17.7"},
			// An expired revalidation of a retained path returns it to OLD;
			// its retention deadline still bounds its lifetime.
			{From: 1, Event: 9, To: 5, Disp: DispTransition, SpecRef: "17.5"},
			{From: 2, Event: 9, To: 5, Disp: DispTransition, SpecRef: "17.5"},
			// Controlled fallback to a still-authorized retained path.
			{From: 5, Event: 10, To: 4, Disp: DispTransition, Actions: []string{"ActInstallActive"}, SpecRef: "17.5"},
		},
	}
}

func pathRoleMachine() MachineSpec {
	return MachineSpec{
		Name:        "PathRole",
		GoPrefix:    "fsmPathRole",
		StateType:   "pathRole",
		SpecSection: "17.1",
		Initial:     0,
		DefaultDisp: DispIgnore,
		States: []StateSpec{
			{ID: 0, Name: "FREE", EngineConst: "pathFree"},
			{ID: 1, Name: "CANDIDATE", EngineConst: "pathCandidate", Predicates: []string{"Allocated"}},
			{ID: 2, Name: "ACTIVE", EngineConst: "pathActive", Predicates: []string{"Allocated", "Primary"}},
			{ID: 3, Name: "OLD", EngineConst: "pathOld", Predicates: []string{"Allocated"}},
		},
		Events: []EventSpec{
			{ID: 0, Name: "EV_BIND_CANDIDATE"},
			{ID: 1, Name: "EV_BIND_INITIAL_ACTIVE"},
			{ID: 2, Name: "EV_PROMOTE_CANDIDATE"},
			{ID: 3, Name: "EV_DEMOTE_ACTIVE"},
			{ID: 4, Name: "EV_RECLAIM"},
			{ID: 5, Name: "EV_FALLBACK_TO_RETAINED"},
		},
		Predicates: []PredicateSpec{
			{Name: "Allocated", GoFunc: "fsmPathRoleIsAllocated", Doc: "reports whether the path table slot holds a live role."},
			{Name: "Primary", GoFunc: "fsmPathRoleIsPrimary", Doc: "reports whether the path table slot is the connection's single ACTIVE path."},
		},
		Actions: []ActionFlag{
			{Name: "ActResetPathState", Bit: 1 << 0},
		},
		Transitions: []TransitionSpec{
			{From: 0, Event: 0, To: 1, Disp: DispTransition, SpecRef: "17.1"},
			{From: 0, Event: 1, To: 2, Disp: DispTransition, SpecRef: "17.1"},
			{From: 1, Event: 2, To: 2, Disp: DispTransition, SpecRef: "17.4"},
			{From: 1, Event: 4, To: 0, Disp: DispTransition, Actions: []string{"ActResetPathState"}, SpecRef: "17.3"},
			{From: 2, Event: 3, To: 3, Disp: DispTransition, SpecRef: "17.5"},
			{From: 3, Event: 4, To: 0, Disp: DispTransition, Actions: []string{"ActResetPathState"}, SpecRef: "17.5"},
			{From: 3, Event: 5, To: 2, Disp: DispTransition, SpecRef: "17.5"},
		},
	}
}

func sentPacketMachine() MachineSpec {
	return MachineSpec{
		Name:        "SentPacket",
		GoPrefix:    "fsmSent",
		StateType:   "sentStatus",
		SpecSection: "13.3, 22.1, 29.5",
		Initial:     7, // sentUnused
		DefaultDisp: DispProtocolError,
		States: []StateSpec{
			{ID: 0, Name: "RESERVED", EngineConst: "sentReserved", Predicates: []string{"AckInvalid"}},
			{ID: 1, Name: "SUBMITTED", EngineConst: "sentSubmitted", Predicates: []string{"IoPending"}},
			{ID: 2, Name: "EMITTED", EngineConst: "sentEmitted", Predicates: []string{"AckVerifiable", "LiveInFlight"}},
			{ID: 3, Name: "BURNED", EngineConst: "sentBurned", Predicates: []string{"AckInvalid", "Resolved"}},
			{ID: 4, Name: "ACKED", EngineConst: "sentAcked", Predicates: []string{"AckVerifiable", "Resolved"}},
			{ID: 5, Name: "LOST", EngineConst: "sentLost", Predicates: []string{"AckVerifiable", "Resolved"}},
			{ID: 6, Name: "ABANDONED", EngineConst: "sentAbandoned", Predicates: []string{"AckVerifiable", "Resolved"}},
			{ID: 7, Name: "UNUSED", EngineConst: "sentUnused", Predicates: []string{"AckInvalid"}},
		},
		Events: []EventSpec{
			{ID: 0, Name: "EV_RESERVE_PN"},
			{ID: 1, Name: "EV_SUBMIT_IO"},
			{ID: 2, Name: "EV_EMIT_SYNC"},
			{ID: 3, Name: "EV_IO_ACCEPTED"},
			{ID: 4, Name: "EV_ABORT_OR_IO_REJECTED"},
			{ID: 5, Name: "EV_RX_VALID_ACK"},
			{ID: 6, Name: "EV_LOSS_DETECTED"},
			{ID: 7, Name: "EV_ABANDON_EPOCH_OR_PATH"},
			{ID: 8, Name: "EV_RING_EVICT"},
		},
		Predicates: []PredicateSpec{
			{Name: "AckVerifiable", GoFunc: "fsmSentIsAckVerifiable", Doc: "reports whether an ACK covering this PN status is valid in the emission ledger (Section 13.3)."},
			{Name: "AckInvalid", GoFunc: "fsmSentIsAckInvalid", Doc: "reports whether an ACK covering this PN status must trigger ACK_RANGE_ERROR."},
			{Name: "LiveInFlight", GoFunc: "fsmSentIsLiveInFlight", Doc: "reports whether the packet is currently emitted and unresolved."},
			{Name: "IoPending", GoFunc: "fsmSentIsIoPending", Doc: "reports whether the packet awaits asynchronous I/O submission completion."},
			{Name: "Resolved", GoFunc: "fsmSentIsResolved", Doc: "reports whether the packet has reached a post-emission resolution state."},
		},
		Actions: []ActionFlag{
			{Name: "ActChargeInFlight", Bit: 1 << 0},
			{Name: "ActReleaseInFlight", Bit: 1 << 1},
			{Name: "ActRequeueItems", Bit: 1 << 2},
			{Name: "ActRetireItems", Bit: 1 << 3},
		},
		Transitions: []TransitionSpec{
			{From: 7, Event: 0, To: 0, Disp: DispTransition, SpecRef: "13.3"},
			{From: 0, Event: 1, To: 1, Disp: DispTransition, SpecRef: "22.1"},
			{From: 0, Event: 2, To: 2, Disp: DispTransition, Actions: []string{"ActChargeInFlight"}, SpecRef: "13.3"},
			{From: 0, Event: 4, To: 3, Disp: DispTransition, SpecRef: "13.3"},
			{From: 1, Event: 3, To: 2, Disp: DispTransition, Actions: []string{"ActChargeInFlight"}, SpecRef: "29.5"},
			{From: 1, Event: 4, To: 3, Disp: DispTransition, Actions: []string{"ActRequeueItems"}, SpecRef: "29.5"},
			// An ACK can overtake the I/O completion of a SUBMITTED packet; it
			// waits for acceptance instead of raising ACK_RANGE_ERROR (A01).
			{From: 1, Event: 5, To: 1, Disp: DispIgnore, SpecRef: "29.5"},
			{From: 2, Event: 5, To: 4, Disp: DispTransition, Actions: []string{"ActReleaseInFlight", "ActRetireItems"}, SpecRef: "13.3"},
			{From: 2, Event: 6, To: 5, Disp: DispTransition, Actions: []string{"ActReleaseInFlight", "ActRequeueItems"}, SpecRef: "14.3"},
			{From: 2, Event: 7, To: 6, Disp: DispTransition, Actions: []string{"ActReleaseInFlight", "ActRequeueItems"}, SpecRef: "17.5"},
			// A queued socket adapter commits emission optimistically and
			// reports rejection later; the PN burns and its items requeue.
			{From: 2, Event: 4, To: 3, Disp: DispTransition, Actions: []string{"ActReleaseInFlight", "ActRequeueItems"}, SpecRef: "29.5"},
			// Late ACK on already-resolved packets is idempotent (never double-releases in-flight)
			{From: 4, Event: 5, To: 4, Disp: DispStay, SpecRef: "13.3"},
			{From: 5, Event: 5, To: 4, Disp: DispTransition, Actions: []string{"ActRetireItems"}, SpecRef: "13.3"},
			{From: 6, Event: 5, To: 6, Disp: DispStay, SpecRef: "13.3"},
			// Ring eviction recycles resolved entries back to UNUSED
			{From: 3, Event: 8, To: 7, Disp: DispTransition, SpecRef: "13.3"},
			{From: 4, Event: 8, To: 7, Disp: DispTransition, SpecRef: "13.3"},
			{From: 5, Event: 8, To: 7, Disp: DispTransition, SpecRef: "13.3"},
			{From: 6, Event: 8, To: 7, Disp: DispTransition, SpecRef: "13.3"},
		},
	}
}

func parkLeaseMachine() MachineSpec {
	return MachineSpec{
		Name:        "ParkLease",
		GoPrefix:    "fsmPark",
		StateType:   "parkSt",
		SpecSection: "35.3, 35.9",
		Initial:     0,
		DefaultDisp: DispIgnore,
		States: []StateSpec{
			{ID: 0, Name: "IDLE", EngineConst: "parkIdle"},
			{ID: 1, Name: "REQ_WAIT", EngineConst: "parkReqWait", Predicates: []string{"LeaseOrHandshakeActive", "AppTxBlocked"}},
			{ID: 2, Name: "LIVE", EngineConst: "parkLive", Predicates: []string{"LeaseOrHandshakeActive", "AppTxBlocked", "TimersSuppressed"}},
			{ID: 3, Name: "WAKING", EngineConst: "parkWaking", Predicates: []string{"LeaseOrHandshakeActive", "AppTxBlocked"}},
			{ID: 4, Name: "AWAKE_PEND", EngineConst: "parkAwakePend", Predicates: []string{"LeaseOrHandshakeActive"}},
		},
		Events: []EventSpec{
			{ID: 0, Name: "EV_CLIENT_SEND_REQUEST"},
			{ID: 1, Name: "EV_GRANT_ACCEPTED"},
			{ID: 2, Name: "EV_GRANT_REFUSED_OR_CANCEL"},
			{ID: 3, Name: "EV_INITIATE_WAKE"},
			{ID: 4, Name: "EV_SERVER_SEND_AWAKE"},
			{ID: 5, Name: "EV_WAKE_COMPLETED"},
		},
		Predicates: []PredicateSpec{
			{Name: "LeaseOrHandshakeActive", GoFunc: "fsmParkIsActive", Doc: "reports whether a parking request, live lease, or wake exchange is active."},
			{Name: "AppTxBlocked", GoFunc: "fsmParkIsAppTxBlocked", Doc: "reports whether parking suppresses ordinary bulk application transmission (Section 35.6)."},
			{Name: "TimersSuppressed", GoFunc: "fsmParkAreTimersSuppressed", Doc: "reports whether idle/keepalive/PTO probing is muted under a live park lease."},
		},
		Actions: []ActionFlag{
			{Name: "ActRecordFixedExpiry", Bit: 1 << 0},
			{Name: "ActQueueWake", Bit: 1 << 1},
			{Name: "ActQueueAwake", Bit: 1 << 2},
			{Name: "ActRestoreTimers", Bit: 1 << 3},
		},
		Transitions: []TransitionSpec{
			{From: 0, Event: 0, To: 1, Disp: DispTransition, SpecRef: "35.9"},
			{From: 0, Event: 1, To: 2, Disp: DispTransition, Actions: []string{"ActRecordFixedExpiry"}, SpecRef: "35.3"},
			{From: 1, Event: 1, To: 2, Disp: DispTransition, Actions: []string{"ActRecordFixedExpiry"}, SpecRef: "35.9"},
			{From: 1, Event: 2, To: 0, Disp: DispTransition, Actions: []string{"ActRestoreTimers"}, SpecRef: "35.9"},
			{From: 1, Event: 3, To: 3, Disp: DispTransition, Actions: []string{"ActQueueWake"}, SpecRef: "35.9"},
			{From: 2, Event: 3, To: 3, Disp: DispTransition, Actions: []string{"ActQueueWake"}, SpecRef: "35.8"},
			{From: 2, Event: 4, To: 4, Disp: DispTransition, Actions: []string{"ActQueueAwake"}, SpecRef: "35.8"},
			{From: 3, Event: 4, To: 4, Disp: DispTransition, Actions: []string{"ActQueueAwake"}, SpecRef: "35.8"},
			{From: 3, Event: 5, To: 0, Disp: DispTransition, Actions: []string{"ActRestoreTimers"}, SpecRef: "35.8"},
			{From: 4, Event: 5, To: 0, Disp: DispTransition, Actions: []string{"ActRestoreTimers"}, SpecRef: "35.8"},
		},
	}
}

func ecnValidationMachine() MachineSpec {
	return MachineSpec{
		Name:        "ECNValidation",
		GoPrefix:    "fsmECN",
		StateType:   "ecnOutcome",
		SpecSection: "15.4",
		Initial:     0,
		DefaultDisp: DispIgnore,
		States: []StateSpec{
			{ID: 0, Name: "UNKNOWN", EngineConst: "ecnUnknown", Predicates: []string{"MarkAllowed"}},
			{ID: 1, Name: "CAPABLE", EngineConst: "ecnCapable", Predicates: []string{"MarkAllowed", "Validated"}},
			{ID: 2, Name: "BROKEN", EngineConst: "ecnBroken", Terminal: true, Predicates: []string{"Disabled"}},
		},
		Events: []EventSpec{
			{ID: 0, Name: "EV_THREE_CONSISTENT_MARKS_ACKED"},
			{ID: 1, Name: "EV_INVALID_COUNTER_OR_MIGRATION"},
		},
		Predicates: []PredicateSpec{
			{Name: "MarkAllowed", GoFunc: "fsmECNIsMarkAllowed", Doc: "reports whether ECT(0) marking is permitted on the path (subject to probe cap in UNKNOWN)."},
			{Name: "Validated", GoFunc: "fsmECNIsValidated", Doc: "reports whether ECN feedback has been validated by three marked acknowledgments."},
			{Name: "Disabled", GoFunc: "fsmECNIsDisabled", Doc: "reports whether ECN marking is permanently disabled on the path."},
		},
		Actions: []ActionFlag{
			{Name: "ActEnableSteadyECT0", Bit: 1 << 0},
			{Name: "ActDisableECT0", Bit: 1 << 1},
		},
		Transitions: []TransitionSpec{
			{From: 0, Event: 0, To: 1, Disp: DispTransition, Actions: []string{"ActEnableSteadyECT0"}, SpecRef: "15.4"},
			{From: 0, Event: 1, To: 2, Disp: DispTransition, Actions: []string{"ActDisableECT0"}, SpecRef: "15.4"},
			{From: 1, Event: 1, To: 2, Disp: DispTransition, Actions: []string{"ActDisableECT0"}, SpecRef: "15.4"},
		},
	}
}

// AllGates returns the conjunctive boolean hotpath gates (Section 22.3).
func AllGates() []GateSpec {
	return []GateSpec{
		{
			Name:        "TxDataGate",
			GoFunc:      "fsmGateTxData",
			MaskConst:   "fsmGateMaskTxData",
			SpecSection: "22.3",
			Doc:         "guards emission of ack-eliciting DATA packets carrying application payload.",
			Terms: []GateTerm{
				{Name: "ConnAppEligible", Bit: 1 << 0, Doc: "connection state permits application DATA emission"},
				{Name: "PathBulkEligible", Bit: 1 << 1, Doc: "path is ACTIVE with validated 1200B base MTU"},
				{Name: "NotParkedBlocked", Bit: 1 << 2, Doc: "no active parking lease/request blocks bulk TX"},
				{Name: "BelowHardKeyLimit", Bit: 1 << 3, Doc: "active epoch packet number is below 2^32 hard AEAD limit"},
				{Name: "LiveTableAdmitted", Bit: 1 << 4, Doc: "sent-packet live table is below MaxLiveSent capacity"},
				{Name: "CwndAdmitted", Bit: 1 << 5, Doc: "bytes_in_flight + packet_bytes <= cwnd"},
				{Name: "PaceAdmitted", Bit: 1 << 6, Doc: "pacing token bucket permits immediate release"},
				{Name: "AmpAdmitted", Bit: 1 << 7, Doc: "anti-amplification 3x byte and 3x packet guards permit send"},
			},
		},
		{
			Name:        "RxCommitGate",
			GoFunc:      "fsmGateRxCommit",
			MaskConst:   "fsmGateMaskRxCommit",
			SpecSection: "22.2-22.3",
			Doc:         "guards atomic transactional commit of an inbound DATA packet.",
			Terms: []GateTerm{
				{Name: "ConnKeyBearing", Bit: 1 << 0, Doc: "connection is in a key-bearing state and not QUIESCING/DEAD"},
				{Name: "EpochSlotDecryptEligible", Bit: 1 << 1, Doc: "matched key slot is INSTALLED or within GRACE window"},
				{Name: "ReplayFresh", Bit: 1 << 2, Doc: "packet number is >= replay floor and not previously committed"},
				{Name: "AEADVerified", Bit: 1 << 3, Doc: "AEAD authentication tag succeeded over canonical header AAD"},
				{Name: "FramesValid", Bit: 1 << 4, Doc: "all frames passed syntactic, state, and flow-control preflight"},
				{Name: "BufferAdmitted", Bit: 1 << 5, Doc: "stream reassembly and control retention quotas admit the transaction"},
			},
		},
		{
			Name:        "MigrationCutoverGate",
			GoFunc:      "fsmGateMigrationCutover",
			MaskConst:   "fsmGateMaskMigrationCutover",
			SpecSection: "17.4-17.7",
			Doc:         "guards promotion of a CANDIDATE path to the connection's ACTIVE path.",
			Terms: []GateTerm{
				{Name: "MigrationNegotiated", Bit: 1 << 0, Doc: "migration is permitted or triggered by authenticated rebinding"},
				{Name: "CandidateReady1200", Bit: 1 << 1, Doc: "candidate path is READY (challenge token + 1200B bidirectional proof)"},
				{Name: "SequenceMonotonic", Bit: 1 << 2, Doc: "migration_seq strictly exceeds highest accepted sequence"},
				{Name: "CutoverBarrierValid", Bit: 1 << 3, Doc: "packet number satisfies cutover_pn ordering barrier"},
				{Name: "NotParked", Bit: 1 << 4, Doc: "connection is not in an active sleep lease without wake"},
			},
		},
		{
			Name:        "StreamRetireGate",
			GoFunc:      "fsmGateStreamRetire",
			MaskConst:   "fsmGateMaskStreamRetire",
			SpecSection: "21.1",
			Doc:         "guards emission of STREAM_RETIRE or RETIRE_ACK for a stream slot.",
			Terms: []GateTerm{
				{Name: "SlotOccupied", Bit: 1 << 0, Doc: "slot is ACTIVE (for initiator RETIRE) or RETIRE_PENDING (for peer RETIRE_ACK)"},
				{Name: "SendHalfTerminal", Bit: 1 << 1, Doc: "send half is nil, SEND_DONE, or SEND_RESET"},
				{Name: "RecvHalfTerminal", Bit: 1 << 2, Doc: "recv half is nil, RECV_DONE, RECV_RESET, or application-stopped"},
			},
		},
		{
			Name:        "AmpSendGate",
			GoFunc:      "fsmGateAmpSend",
			MaskConst:   "fsmGateMaskAmpSend",
			SpecSection: "6.1, 17.3",
			Doc:         "guards emission on an unvalidated path under the 3x byte and 3x packet anti-amplification rule.",
			Terms: []GateTerm{
				{Name: "BytesWithin3x", Bit: 1 << 0, Doc: "tx_bytes + packet_bytes <= 3 * rx_bytes (or path validated)"},
				{Name: "PktsWithin3x", Bit: 1 << 1, Doc: "tx_pkts + 1 <= 3 * rx_pkts (or path validated)"},
			},
		},
		{
			Name:        "KeyCommitGate",
			GoFunc:      "fsmGateKeyCommit",
			MaskConst:   "fsmGateMaskKeyCommit",
			SpecSection: "19.4, 21.2",
			Doc:         "guards installation of STAGED keys into ACTIVE upon KEY_COMMIT / KEY_CONFIRM.",
			Terms: []GateTerm{
				{Name: "RefreshStaged", Bit: 1 << 0, Doc: "NEXT key slot holds verified STAGED keys for epoch E+1"},
				{Name: "EpochNextExact", Bit: 1 << 1, Doc: "frame epoch equals active_epoch + 1"},
				{Name: "RefreshIDMatched", Bit: 1 << 2, Doc: "refresh_id matches the active refresh transaction"},
				{Name: "TranscriptMACValid", Bit: 1 << 3, Doc: "32-byte commit/confirm HMAC verifies in constant time"},
			},
		},
	}
}

// AllInvariants returns the catalog of normative invariants verified across
// the component FSMs and composed explicit-state models.
func AllInvariants() []InvariantSpec {
	return []InvariantSpec{
		{ID: "I01", Category: "Crypto", SpecSection: "4.6, 27.1", Summary: "Cipher suite is selected at most once and never mutates across migration, resumption, or rekey."},
		{ID: "I02", Category: "Crypto", SpecSection: "7.5, 27.1", Summary: "Every AEAD seal uses a strictly monotonic per-connection packet number; no (key, nonce) pair is ever reused."},
		{ID: "I03", Category: "Wire", SpecSection: "5.2, 27.1", Summary: "Outer header bytes are authenticated as exact AEAD AAD; any header mutation fails decryption before state change."},
		{ID: "I04", Category: "Lifecycle", SpecSection: "20.1, 27.1", Summary: "Application STREAM/DATAGRAM frames are never emitted or accepted outside key-bearing states."},
		{ID: "I05", Category: "Transaction", SpecSection: "22.2, 27.1", Summary: "Failed AEAD or malformed frame preflight mutates zero transport, replay, flow-control, or path credit state."},
		{ID: "I06", Category: "Replay", SpecSection: "13.2, 27.1", Summary: "Replay window bits below the sliding floor are cleared before window advancement to prevent modulo aliasing."},
		{ID: "I07", Category: "Replay", SpecSection: "13.1, 27.1", Summary: "ACK frames never imply cumulative delivery outside their explicit [low, high] intervals."},
		{ID: "I08", Category: "Ledger", SpecSection: "13.3, 27.1", Summary: "An ACK covering a packet number >= next_pn or a RESERVED/BURNED entry triggers ACK_RANGE_ERROR."},
		{ID: "I09", Category: "Handshake", SpecSection: "7.6, 27.1", Summary: "Server never transitions to ESTABLISHED before client FINISH MAC verifies; client never confirms before HANDSHAKE_DONE."},
		{ID: "I10", Category: "Stream", SpecSection: "12.1, 27.1", Summary: "Stream slot generation increments strictly upon retirement completion and halts at SLOT_EXHAUSTED (2^32-1)."},
		{ID: "I11", Category: "FlowControl", SpecSection: "12.4, 27.1", Summary: "Emitted stream offset never exceeds send.limit and connTxUsed never exceeds connTxLim."},
		{ID: "I12", Category: "Stream", SpecSection: "12.5-12.6", Summary: "Stream final_size is bound at most once on send and receive halves; conflicting FIN/RESET offsets trigger FINAL_SIZE_ERROR."},
		{ID: "I13", Category: "Stream", SpecSection: "21.1, 27.1", Summary: "STREAM_RETIRE and RETIRE_ACK are emitted only when both local halves of the slot are terminal."},
		{ID: "I14", Category: "Congestion", SpecSection: "14.1, 27.1", Summary: "Each in-flight packet charges bytes_in_flight once on emission and releases at most once across ACK, LOST, or ABANDONED."},
		{ID: "I15", Category: "PMTU", SpecSection: "15.3, 17.2", Summary: "MTU_PROBE targets above 2*M clamp pacing token reservation to 2*M so binary search up to 4096B never deadlocks."},
		{ID: "I16", Category: "Congestion", SpecSection: "15.1, 27.1", Summary: "Congestion window halves at most once per recovery boundary ordinal."},
		{ID: "I17", Category: "ECN", SpecSection: "15.4, 27.1", Summary: "Decreasing or impossible peer ECN counters immediately transition path ECN state to ecnBroken and disable ECT(0)."},
		{ID: "I18", Category: "ECN", SpecSection: "15.4, 27.1", Summary: "Any path migration or tuple change permanently disables sender ECN marking for the remainder of the connection."},
		{ID: "I19", Category: "KeyRefresh", SpecSection: "19.1, 27.1", Summary: "At most three epoch slots (PREV, ACTIVE, NEXT) exist simultaneously; epoch numbers increase by exactly +1."},
		{ID: "I20", Category: "KeyRefresh", SpecSection: "19.4, 27.1", Summary: "KEY_COMMIT is valid only when NEXT is staged with matching refresh_id and valid transcript MAC."},
		{ID: "I21", Category: "KeyRefresh", SpecSection: "19.6, 27.1", Summary: "Reaching the 2^32 hard packet-number limit in an epoch without key rotation immediately closes with AEAD_LIMIT_REACHED."},
		{ID: "I22", Category: "KeyRefresh", SpecSection: "19.5, 27.1", Summary: "PREV epoch keys are zeroized upon grace timer expiry and never seal outbound packets."},
		{ID: "I23", Category: "Lifecycle", SpecSection: "20.3, 27.1", Summary: "DRAINING, QUIESCING, and DEAD states never emit wire packets."},
		{ID: "I24", Category: "Lifecycle", SpecSection: "20.1, 27.1", Summary: "DEAD is an absorbing terminal state for all events."},
		{ID: "A01", Category: "AsyncIO", SpecSection: "29.5, 34.1", Summary: "An ACK covering a SUBMITTED packet waits for I/O acceptance rather than validating an unaccepted wire emission."},
		{ID: "A04", Category: "AsyncIO", SpecSection: "29.5, 34.1", Summary: "Rejected async send transitions SUBMITTED to BURNED and rolls back pacing/amplification charges while burning the PN."},
		{ID: "A08", Category: "AsyncIO", SpecSection: "20.3, 34.1", Summary: "Transition from QUIESCING to DEAD is blocked while any zero-copy or crypto I/O pin remains outstanding."},
		{ID: "M01", Category: "Mobility", SpecSection: "17.3, 38.1", Summary: "A fresh authenticated packet on a new tuple never validates or promotes that tuple without PATH_CHALLENGE/PATH_RESPONSE."},
		{ID: "M02", Category: "Mobility", SpecSection: "17.3, 38.1", Summary: "Unvalidated candidate output never exceeds 3*rxBytes or 3*rxPkts."},
		{ID: "M06", Category: "Mobility", SpecSection: "17.2, 38.1", Summary: "Transition to READY/ACTIVE requires both challenge token verification and bidirectional 1200-byte datagram proof."},
		{ID: "M07", Category: "Mobility", SpecSection: "17.1, 38.1", Summary: "At most one ACTIVE, one CANDIDATE, and one OLD path exist simultaneously."},
		{ID: "M09", Category: "Mobility", SpecSection: "17.4, 38.1", Summary: "A pre-cutover old-path packet (pn < cutover_pn) never reverses an accepted migration or re-promotes OLD."},
		{ID: "M15", Category: "Parking", SpecSection: "35.4, 38.1", Summary: "Repeated PARK_REQUEST or PARK_GRANT frames never extend the initially recorded lease expiry."},
		{ID: "M16", Category: "Parking", SpecSection: "35.6, 38.1", Summary: "Parking preserves all live streams, unread bytes, replay bitmaps, and key epochs."},
		{ID: "M17", Category: "Parking", SpecSection: "35.8, 38.1", Summary: "Waking never resumes bulk application TX before path readiness and credit backing are confirmed."},
		{ID: "M21", Category: "Memory", SpecSection: "12.2, 38.1", Summary: "Frames carrying stale or future stream generations never allocate state or mutate the active generation."},
	}
}

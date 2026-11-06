package engine

//go:generate go run ../../cmd/grtgen -engine-out z_formal_gen.go -tla-dir ../../spec/formal

import (
	"errors"
	"strconv"
)

// connState is the section-20 lifecycle FSM.
type connState uint8

const (
	stNew connState = iota
	stCBuild
	stCWaitReply
	stCVerify
	stCFinishing
	stSCollect
	stSBuild
	stSFinishing
	stEstablished
	stClosing
	stDraining
	stQuiescing
	stDead
)

func (s connState) String() string {
	switch s {
	case stNew:
		return "NEW"
	case stCBuild:
		return "C_BUILD"
	case stCWaitReply:
		return "C_WAIT_REPLY"
	case stCVerify:
		return "C_VERIFY"
	case stCFinishing:
		return "C_FINISHING"
	case stSCollect:
		return "S_COLLECT"
	case stSBuild:
		return "S_BUILD"
	case stSFinishing:
		return "S_FINISHING"
	case stEstablished:
		return "ESTABLISHED"
	case stClosing:
		return "CLOSING"
	case stDraining:
		return "DRAINING"
	case stQuiescing:
		return "QUIESCING"
	case stDead:
		return "DEAD"
	}
	return "?"
}

// live reports whether the state bears installed epoch keys.
func (s connState) keyBearing() bool {
	return fsmConnIsKeyBearing(s)
}

// closing reports whether the state is one of the terminal paths.
func (s connState) terminating() bool {
	return fsmConnIsTerminating(s)
}

// role is immutable per connection.
type role uint8

const (
	roleClient role = iota
	roleServer
)

// closeCode is the section-24 error registry; the wire field is u32 so
// peer-supplied values above the named range are preserved, not clamped.
type closeCode uint32

const (
	codeNoError              closeCode = 0
	codeInternal             closeCode = 1
	codeProtocolViolation    closeCode = 2
	codeFrameEncoding        closeCode = 3
	codeFlowControl          closeCode = 4
	codeStreamState          closeCode = 5
	codeFinalSize            closeCode = 6
	codeStreamData           closeCode = 7
	codeAckRange             closeCode = 8
	codeConnectionID         closeCode = 9
	codeKeyUpdate            closeCode = 10
	codeAEADLimit            closeCode = 11
	codeCryptoBufferExceeded closeCode = 12
	codeTransportParameter   closeCode = 13
	codeApplicationAbort     closeCode = 14
	codeLocalResource        closeCode = 15
	codePathState            closeCode = 16
	codeMobileState          closeCode = 17
)

// TransportError is the close error surfaced to the application.
type TransportError struct {
	Code   closeCode
	Remote bool
	// Diag is a hash of locally logged diagnostics, never attacker text.
	Diag [32]byte
}

func (e *TransportError) Error() string {
	if e.Remote {
		return "grt: peer closed with code " + codeName(e.Code)
	}
	return "grt: closed with code " + codeName(e.Code)
}

func codeName(c closeCode) string {
	names := [...]string{
		"NO_ERROR", "INTERNAL_ERROR", "PROTOCOL_VIOLATION",
		"FRAME_ENCODING_ERROR", "FLOW_CONTROL_ERROR",
		"STREAM_STATE_ERROR", "FINAL_SIZE_ERROR", "STREAM_DATA_ERROR",
		"ACK_RANGE_ERROR", "CONNECTION_ID_ERROR", "KEY_UPDATE_ERROR",
		"AEAD_LIMIT_REACHED", "CRYPTO_BUFFER_EXCEEDED",
		"TRANSPORT_PARAMETER_ERROR", "APPLICATION_ABORT",
		"LOCAL_RESOURCE_ERROR", "PATH_STATE_ERROR", "MOBILE_STATE_ERROR",
	}
	if int(c) < len(names) {
		return names[c]
	}
	// Peer-supplied codes outside the named range keep their value so a
	// u32 abort code is never lost in the surfaced error.
	return "CODE_" + strconv.FormatUint(uint64(c), 10)
}

// errClose wraps a close code as an internal Go error for control flow.
type errClose struct{ code closeCode }

func (e *errClose) Error() string { return "grt: close " + codeName(e.code) }

var (
	errHandshakeFailed = errors.New("grt: handshake failed")
	errClosed          = errors.New("grt: connection closed")
	errWouldBlock      = errors.New("grt: would block")
	errStreamReset     = errors.New("grt: stream reset")
	errPeerStop        = errors.New("grt: peer requested stream stop")
	errMobileState     = errors.New("grt: mobile/parking state error")
)

// Exported aliases of the sentinel errors for the public package.
var (
	ErrClosed          = errClosed
	ErrWouldBlock      = errWouldBlock
	ErrStreamReset     = errStreamReset
	ErrPeerStop        = errPeerStop
	ErrHandshakeFailed = errHandshakeFailed
)

// Exported aliases for public API exposure of lifecycle state.
type (
	State     = connState
	CloseCode = closeCode
)

const (
	StateNew         State = stNew
	StateCBuild      State = stCBuild
	StateCWaitReply  State = stCWaitReply
	StateCVerify     State = stCVerify
	StateCFinishing  State = stCFinishing
	StateSCollect    State = stSCollect
	StateSBuild      State = stSBuild
	StateSFinishing  State = stSFinishing
	StateEstablished State = stEstablished
	StateClosing     State = stClosing
	StateDraining    State = stDraining
	StateQuiescing   State = stQuiescing
	StateDead        State = stDead
)

// timerKind indexes the connection's named deadlines.
type timerKind uint8

const (
	tkLoss timerKind = iota // loss/PTO
	tkIdle
	tkHandshake
	tkAckDelay
	tkClose // close or drain deadline
	tkPacer // pacing rearm
	tkPathValid
	tkPathRetention
	tkRefresh
	tkKeepalive
	tkGrace    // PREVIOUS-epoch erasure (section 19.5)
	tkParkCtrl // parking request/wake resend + deadlines (section 35)
	tkFlight   // bootstrap flight pacing step (section 6.4)
	nTimerKinds
)

// slotState is the section-21.1 stream slot lifecycle.
type slotState uint8

const (
	slotEmpty slotState = iota
	slotActive
	slotRetireSent
	slotRetirePending
	slotRetiredWaitOpen
	slotExhausted
)

// sentStatus is the section-13.3 emission ledger status (Compact 3-bit codes).
type sentStatus uint8

const (
	sentReserved  sentStatus = 0
	sentSubmitted sentStatus = 1
	sentEmitted   sentStatus = 2
	sentBurned    sentStatus = 3
	sentAcked     sentStatus = 4
	sentLost      sentStatus = 5
	sentAbandoned sentStatus = 6
	sentUnused    sentStatus = 7
)

// epochSlotState is the key-slot lifecycle (section 19).
type epochSlotState uint8

const (
	epochSlotEmpty epochSlotState = iota
	epochSlotInstalled
	epochSlotGrace
)

// pathRole for the three path slots.
type pathRole uint8

const (
	pathFree pathRole = iota
	pathCandidate
	pathActive
	pathOld // retained after promotion
)

// parkSt is the section-35 parking lease component state. The client
// occupies parkReqWait/parkLive/parkWaking; the server occupies
// parkLive/parkWaking/parkAwakePend. parkIdle means no live lease.
type parkSt uint8

const (
	parkIdle      parkSt = iota // no live lease activity
	parkReqWait                 // client: request result uncertain
	parkLive                    // lease granted/active (both roles)
	parkWaking                  // wake validation/recovery in progress
	parkAwakePend               // server: PARK_AWAKE emitted, awaiting its ACK
)

// cipher negotiation states (section 7.4 semantics).
const (
	cipherUnresolved uint8 = iota
	cipherLocked
	cipherFailed
)

// ECN outcome codes (section 17.3).
type ecnOutcome uint8

const (
	ecnUnknown ecnOutcome = iota
	ecnCapable
	ecnBroken
)

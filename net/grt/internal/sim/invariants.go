package sim

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"
)

// InvariantID identifies a specific protocol invariant from the GRT/1.1 specification.
type InvariantID string

const (
	// INV01_StreamDataIntegrity: All stream bytes delivered must match sent bytes exactly.
	INV01_StreamDataIntegrity InvariantID = "INV-01-STREAM-INTEGRITY"
	// INV02_CipherDeterminism: Client and server must deterministically agree on negotiated cipher.
	INV02_CipherDeterminism InvariantID = "INV-02-CIPHER-DETERMINISM"
	// INV03_EpochMonotonicity: Key refresh epochs must monotonically increase without gap or regression.
	INV03_EpochMonotonicity InvariantID = "INV-03-EPOCH-MONOTONICITY"
	// INV04_ReplayDefense: 0-RTT early data replays must be detected and rejected.
	INV04_ReplayDefense InvariantID = "INV-04-REPLAY-DEFENSE"
	// INV05_FlowControlBound: Stream byte transfers must respect flow control limits.
	INV05_FlowControlBound InvariantID = "INV-05-FLOW-CONTROL-BOUND"
	// INV06_CutoverMonotonicity: Path migration must strictly adhere to make-before-break cutover.
	INV06_CutoverMonotonicity InvariantID = "INV-06-CUTOVER-MONOTONICITY"
	// INV07_TerminationQuiescence: Quiesced or closed connections must release resources cleanly.
	INV07_TerminationQuiescence InvariantID = "INV-07-TERMINATION-QUIESCENCE"
)

// StreamRecorder records bytes sent and received on a stream for integrity checking.
type StreamRecorder struct {
	mu       sync.Mutex
	sent     bytes.Buffer
	received bytes.Buffer
}

func (sr *StreamRecorder) RecordSent(b []byte) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.sent.Write(b)
}

func (sr *StreamRecorder) RecordReceived(b []byte) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.received.Write(b)
}

func (sr *StreamRecorder) Verify(t testing.TB, sid uint64) {
	t.Helper()
	sr.mu.Lock()
	defer sr.mu.Unlock()

	sentBytes := sr.sent.Bytes()
	recvBytes := sr.received.Bytes()

	if len(sentBytes) != len(recvBytes) {
		t.Fatalf("[%s] stream %d byte count mismatch: sent %d, recv %d",
			INV01_StreamDataIntegrity, sid, len(sentBytes), len(recvBytes))
	}

	hSent := sha256.Sum256(sentBytes)
	hRecv := sha256.Sum256(recvBytes)
	if hSent != hRecv {
		t.Fatalf("[%s] stream %d SHA-256 hash mismatch: sent %x, recv %x",
			INV01_StreamDataIntegrity, sid, hSent, hRecv)
	}
}

// InvariantTracker monitors invariants across a simulation run.
type InvariantTracker struct {
	mu       sync.Mutex
	streams  map[uint64]*StreamRecorder
	epochs   []uint32
	ciphers  []uint16
	failures []string
}

// NewInvariantTracker creates an InvariantTracker.
func NewInvariantTracker() *InvariantTracker {
	return &InvariantTracker{
		streams: make(map[uint64]*StreamRecorder),
	}
}

// StreamRecorderFor returns the recorder for a stream ID.
func (it *InvariantTracker) StreamRecorderFor(sid uint64) *StreamRecorder {
	it.mu.Lock()
	defer it.mu.Unlock()
	sr, ok := it.streams[sid]
	if !ok {
		sr = &StreamRecorder{}
		it.streams[sid] = sr
	}
	return sr
}

// RecordEpoch records an observed epoch transition.
func (it *InvariantTracker) RecordEpoch(epoch uint32) {
	it.mu.Lock()
	defer it.mu.Unlock()
	if len(it.epochs) > 0 {
		prev := it.epochs[len(it.epochs)-1]
		if epoch <= prev {
			it.failures = append(it.failures,
				fmt.Sprintf("[%s] non-monotonic epoch transition: %d -> %d",
					INV03_EpochMonotonicity, prev, epoch))
		}
	}
	it.epochs = append(it.epochs, epoch)
}

// RecordCipherAgreement verifies client and server negotiated the same cipher.
func (it *InvariantTracker) RecordCipherAgreement(clientCipher, serverCipher uint16) {
	it.mu.Lock()
	defer it.mu.Unlock()
	if clientCipher != serverCipher {
		it.failures = append(it.failures,
			fmt.Sprintf("[%s] cipher mismatch: client=%d server=%d",
				INV02_CipherDeterminism, clientCipher, serverCipher))
	}
}

// AssertAll verifies all tracked invariants and fails the test if any were violated.
func (it *InvariantTracker) AssertAll(t testing.TB) {
	t.Helper()
	it.mu.Lock()
	defer it.mu.Unlock()

	for _, failure := range it.failures {
		t.Error(failure)
	}

	for sid, sr := range it.streams {
		sr.Verify(t, sid)
	}

	if len(it.failures) > 0 {
		t.Fatalf("sim: %d invariant assertion(s) failed", len(it.failures))
	}
}

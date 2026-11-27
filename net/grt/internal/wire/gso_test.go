package wire

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"testing"
)

func makeTestPacket(totalLen int, payloadLen int) []byte {
	pkt := make([]byte, totalLen)
	binary.BigEndian.PutUint16(pkt[0:], Magic)
	pkt[2] = Version
	pkt[3] = ClassData
	binary.BigEndian.PutUint64(pkt[4:], 0x12345678) // CID
	binary.BigEndian.PutUint32(pkt[12:], 0)         // epoch
	binary.BigEndian.PutUint64(pkt[16:], 1)         // PN
	binary.BigEndian.PutUint16(pkt[24:], uint16(payloadLen))
	return pkt
}

func testKey(s uint16) BatchCompatibilityKey {
	return BatchCompatibilityKey{
		ConnGen:        1,
		PathSlot:       0,
		PathGen:        1,
		Local:          netip.MustParseAddrPort("10.0.0.1:5000"),
		Remote:         netip.MustParseAddrPort("10.0.0.2:4433"),
		CID:            0x12345678,
		SelectedCipher: CipherXChaCha20Poly1305,
		KeyEpoch:       0,
		KeyGen:         1,
		PMTUGen:        1,
		SegmentSize:    s,
		ECN:            0,
		PacingClass:    0,
	}
}

// Section 34.1 Case 1: GSO with valid children and short final child.
func TestGSOValidWithShortFinalChild(t *testing.T) {
	key := testKey(1200)
	b, err := NewGSOBuilder(key)
	if err != nil {
		t.Fatalf("NewGSOBuilder: %v", err)
	}

	p1 := makeTestPacket(1200, 1200-32)
	p2 := makeTestPacket(1200, 1200-32)
	p3 := makeTestPacket(1200, 1200-32)
	p4 := makeTestPacket(600, 600-32)

	if err := b.AddChild(p1, key, false); err != nil {
		t.Fatalf("child 1: %v", err)
	}
	if err := b.AddChild(p2, key, false); err != nil {
		t.Fatalf("child 2: %v", err)
	}
	if err := b.AddChild(p3, key, false); err != nil {
		t.Fatalf("child 3: %v", err)
	}
	if err := b.AddChild(p4, key, false); err != nil {
		t.Fatalf("child 4 (short final): %v", err)
	}

	superbuf, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(superbuf) != 1200*3+600 {
		t.Fatalf("unexpected superbuffer length: %d", len(superbuf))
	}

	// GRO normalize must recover all 4 children
	children, err := GRONormalize(superbuf, 1200)
	if err != nil {
		t.Fatalf("GRONormalize: %v", err)
	}
	if len(children) != 4 {
		t.Fatalf("expected 4 children, got %d", len(children))
	}
	if len(children[3]) != 600 {
		t.Fatalf("expected child 3 len 600, got %d", len(children[3]))
	}
}

// Section 34.1 Case 2: Short middle child or post-tag padding rejected.
func TestGSORejectShortMiddleChild(t *testing.T) {
	key := testKey(1200)
	b, err := NewGSOBuilder(key)
	if err != nil {
		t.Fatal(err)
	}

	p1 := makeTestPacket(1200, 1200-32)
	p2 := makeTestPacket(600, 600-32) // short middle child!
	p3 := makeTestPacket(1200, 1200-32)

	if err := b.AddChild(p1, key, false); err != nil {
		t.Fatal(err)
	}
	if err := b.AddChild(p2, key, false); err != nil {
		t.Fatal(err)
	}
	// Adding p3 after short child p2 must fail with ErrShortMiddleChild
	if err := b.AddChild(p3, key, false); !errors.Is(err, ErrShortMiddleChild) {
		t.Fatalf("expected ErrShortMiddleChild, got %v", err)
	}
}

func TestGSORejectPostTagPadding(t *testing.T) {
	key := testKey(1200)
	b, err := NewGSOBuilder(key)
	if err != nil {
		t.Fatal(err)
	}

	// Packet total length 1200, but payload_length claimed as 1100 (100-byte raw post-tag padding!)
	p := makeTestPacket(1200, 1100)
	if err := b.AddChild(p, key, false); !errors.Is(err, ErrPostTagPadding) {
		t.Fatalf("expected ErrPostTagPadding, got %v", err)
	}
}

// Section 34.1 Case 3: 64*1200-byte aggregate rejected by conservative byte bound.
func TestGSOAggregateByteCap(t *testing.T) {
	key := testKey(1200)
	b, err := NewGSOBuilder(key)
	if err != nil {
		t.Fatal(err)
	}

	p := makeTestPacket(1200, 1200-32)
	added := 0
	var lastErr error
	for i := 0; i < 64; i++ {
		if err := b.AddChild(p, key, false); err != nil {
			lastErr = err
			break
		}
		added++
	}

	// 54 * 1200 = 64800 <= 65507. The 55th packet would be 66000 > 65507.
	if added != 54 {
		t.Fatalf("expected 54 children admitted before 65507 byte limit, got %d", added)
	}
	if !errors.Is(lastErr, ErrGSOByteLimitExceeded) {
		t.Fatalf("expected ErrGSOByteLimitExceeded, got %v", lastErr)
	}
}

// Section 30.2: Incompatible batch key and mixed congestion exemption.
func TestGSOIncompatibleBatchKey(t *testing.T) {
	key := testKey(1200)
	b, _ := NewGSOBuilder(key)
	p := makeTestPacket(1200, 1200-32)

	// Different CID
	keyDiff := key
	keyDiff.CID = 0x9999
	if err := b.AddChild(p, keyDiff, false); !errors.Is(err, ErrIncompatibleBatchKey) {
		t.Fatalf("expected ErrIncompatibleBatchKey, got %v", err)
	}

	// Mixed congestion exemption
	if err := b.AddChild(p, key, true); !errors.Is(err, ErrMixedCongestionExemption) {
		t.Fatalf("expected ErrMixedCongestionExemption, got %v", err)
	}
}

// Section 34.1 Case 6: GRO without segmentation metadata treated as one UDP datagram.
func TestGROWithoutMetadataNotMagicScanned(t *testing.T) {
	// Aggregate formed by 2 packets without metadata (segmentSize = 0)
	p1 := makeTestPacket(1200, 1200-32)
	p2 := makeTestPacket(1200, 1200-32)
	raw := append(p1, p2...)

	children, err := GRONormalize(raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 {
		t.Fatalf("expected exactly 1 child when segmentSize=0 (no magic scan), got %d", len(children))
	}
	if len(children[0]) != len(raw) {
		t.Fatalf("expected single unsegmented buffer of %d bytes, got %d", len(raw), len(children[0]))
	}
}

func TestGROMalformedChild(t *testing.T) {
	bad := make([]byte, 2400)
	// Missing GRT magic header
	_, err := GRONormalize(bad, 1200)
	if !errors.Is(err, ErrTruncatedGROChild) {
		t.Fatalf("expected ErrTruncatedGROChild, got %v", err)
	}
}

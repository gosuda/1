package wire

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// buildOne serializes a single frame (header + body + zero alignment pad).
func buildOne(typ, flags uint16, body []byte) []byte {
	fb := NewFrameBuilder(make([]byte, 0, 4096))
	if !fb.Add(typ, flags, len(body), func(d []byte) { copy(d, body) }) {
		panic("build")
	}
	return fb.Bytes()
}

// TestFrameRoundTrip: builder output reparses to the identical frame list.
func TestFrameRoundTrip(t *testing.T) {
	fb := NewFrameBuilder(make([]byte, 0, 4096))
	body1 := []byte{1, 2, 3}
	var ack AckFrame
	ack.Count = 1
	ack.Ranges[0] = [2]uint64{9, 12}
	ackBody := make([]byte, AckBodyLen(1))
	BuildAckBody(ackBody, &ack)
	if !fb.AddBytes(FramePing, 0, body1) ||
		!fb.AddBytes(FrameAck, 0, ackBody) ||
		!fb.AddBytes(FrameStream, FrameFlagStreamFin, bytes.Repeat([]byte{7}, 24)) {
		t.Fatal("build failed")
	}
	out := make([]Frame, MaxFrames)
	frames, eliciting, err := ParseFrames(fb.Bytes(), out)
	if err != nil {
		t.Fatal(err)
	}
	if !eliciting {
		t.Fatal("eliciting not set")
	}
	if len(frames) != 3 || frames[0].Type != FramePing ||
		frames[1].Type != FrameAck || frames[2].Type != FrameStream {
		t.Fatalf("frames = %+v", frames)
	}
	if !bytes.Equal(frames[0].Body, body1) {
		t.Fatal("ping body mismatch")
	}
	if frames[2].Flags != FrameFlagStreamFin {
		t.Fatal("FIN flag lost")
	}
}

// TestFrameGrammarRejections: the strict grammar rejects each malformed
// class with its dedicated error (spec section 11).
func TestFrameGrammarRejections(t *testing.T) {
	ping := buildOne(FramePing, 0, nil)
	bodied := buildOne(FramePing, 0, make([]byte, 24))
	cases := []struct {
		name string
		b    []byte
		want error
	}{
		{"partial header", ping[:4], ErrFramePartial},
		{"truncated body", bodied[:len(bodied)-3], ErrFrameTruncated},
	}
	// Nonzero alignment padding: a 1-byte body pads to 8.
	padded := buildOne(FramePing, 0, []byte{0x55})
	padded[len(padded)-1] = 1
	cases = append(cases, struct {
		name string
		b    []byte
		want error
	}{"nonzero pad", padded, ErrFramePadding})
	for _, tc := range cases {
		if _, _, err := ParseFrames(tc.b, make([]Frame, MaxFrames)); err != tc.want {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// TestFrameCountBound: more than MaxFrames headers is an error even when
// every frame is individually well-formed.
func TestFrameCountBound(t *testing.T) {
	var b []byte
	for i := 0; i <= MaxFrames; i++ {
		b = append(b, buildOne(FramePing, 0, nil)...)
	}
	if _, _, err := ParseFrames(b, make([]Frame, MaxFrames)); err != ErrFrameCount {
		t.Fatalf("err = %v, want ErrFrameCount", err)
	}
}

// TestUnknownFramePolicy: unknown critical types are fatal; bit-15
// (skippable) types are dropped but still mark the packet eliciting.
func TestUnknownFramePolicy(t *testing.T) {
	crit := buildOne(0x0050, 0, make([]byte, 8)) // unknown, bit15 clear
	if _, _, err := ParseFrames(crit, make([]Frame, MaxFrames)); err != ErrFrameCritical {
		t.Fatalf("unknown critical: err = %v", err)
	}
	skip := buildOne(0x8050, 0, make([]byte, 8)) // unknown, bit15 set
	frames, eliciting, err := ParseFrames(skip, make([]Frame, MaxFrames))
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 || !eliciting {
		t.Fatalf("skippable: n=%d eliciting=%v", len(frames), eliciting)
	}
}

// TestFrameFlags: nonzero flags are only legal on STREAM (bit 0 = FIN).
func TestFrameFlags(t *testing.T) {
	bad := buildOne(FramePing, 1, nil)
	if _, _, err := ParseFrames(bad, make([]Frame, MaxFrames)); err != ErrFrameFlags {
		t.Fatalf("ping flags: err = %v", err)
	}
	strm := buildOne(FrameStream, 2, make([]byte, 16)) // flag bit 1
	if _, _, err := ParseFrames(strm, make([]Frame, MaxFrames)); err != ErrFrameFlags {
		t.Fatalf("stream flag bit1: err = %v", err)
	}
	fin := buildOne(FrameStream, FrameFlagStreamFin, make([]byte, 16))
	if _, _, err := ParseFrames(fin, make([]Frame, MaxFrames)); err != nil {
		t.Fatalf("FIN flag rejected: %v", err)
	}
}

// TestAckBodyGrammar: §13.1 ordering — descending highs, no overlap, no
// touching ranges, count in [1,8], reserved zero.
func TestAckBodyGrammar(t *testing.T) {
	body := make([]byte, AckBodyLen(2))
	binary.BigEndian.PutUint16(body[4:], 2)
	// Range0 [20,24], range1 [10,15]: legal (15+1 < 20).
	binary.BigEndian.PutUint64(body[32:], 20)
	binary.BigEndian.PutUint64(body[40:], 24)
	binary.BigEndian.PutUint64(body[48:], 10)
	binary.BigEndian.PutUint64(body[56:], 15)
	a, ok := ParseAckBody(body)
	if !ok || a.Count != 2 {
		t.Fatal("valid 2-range ACK rejected")
	}
	// Touching: range1 [16,19] touches range0's low 20.
	binary.BigEndian.PutUint64(body[48:], 16)
	binary.BigEndian.PutUint64(body[56:], 19)
	if _, ok := ParseAckBody(body); ok {
		t.Fatal("touching ranges accepted")
	}
	// Overlap.
	binary.BigEndian.PutUint64(body[48:], 18)
	binary.BigEndian.PutUint64(body[56:], 22)
	if _, ok := ParseAckBody(body); ok {
		t.Fatal("overlapping ranges accepted")
	}
	// lo > hi.
	binary.BigEndian.PutUint64(body[32:], 24)
	binary.BigEndian.PutUint64(body[40:], 20)
	binary.BigEndian.PutUint64(body[48:], 10)
	binary.BigEndian.PutUint64(body[56:], 15)
	if _, ok := ParseAckBody(body); ok {
		t.Fatal("lo>hi accepted")
	}
	// Count 0 and count > 8.
	binary.BigEndian.PutUint16(body[4:], 0)
	if _, ok := ParseAckBody(body); ok {
		t.Fatal("count 0 accepted")
	}
	big := make([]byte, AckBodyLen(9))
	binary.BigEndian.PutUint16(big[4:], 9)
	if _, ok := ParseAckBody(big); ok {
		t.Fatal("count 9 accepted")
	}
	// Reserved field must be zero.
	binary.BigEndian.PutUint16(body[4:], 1)
	binary.BigEndian.PutUint64(body[32:], 1)
	binary.BigEndian.PutUint64(body[40:], 5)
	binary.BigEndian.PutUint16(body[6:], 1)
	if _, ok := ParseAckBody(body[:AckBodyLen(1)]); ok {
		t.Fatal("nonzero reserved accepted")
	}
}

// TestHeaderStrict: magic, version, class, flags/reserved-zero, and the
// class-specific length rules (spec section 5).
func TestHeaderStrict(t *testing.T) {
	var b [HeaderBytes]byte
	BuildHeader(b[:], ClassData, 0xA5, 3, 99, 64, true)
	pkt := append(b[:], make([]byte, 64)...)
	h, ok := ParseHeader(pkt)
	if !ok || h.Class != ClassData || h.DestCID != 0xA5 || h.Epoch != 3 ||
		h.PacketNumber != 99 || h.PayloadLength != 64 || !h.AckEliciting {
		t.Fatalf("valid header rejected: %+v", h)
	}
	if _, ok := ParseHeader(pkt[:HeaderBytes-1]); ok {
		t.Fatal("short header accepted")
	}
	pkt[0] ^= 1
	if _, ok := ParseHeader(pkt); ok {
		t.Fatal("bad magic accepted")
	}
	pkt[0] ^= 1
	pkt[2] = 0x99
	if _, ok := ParseHeader(pkt); ok {
		t.Fatal("bad version accepted")
	}
	pkt[2] = Version
	pkt[3] = 0x77
	if _, ok := ParseHeader(pkt); ok {
		t.Fatal("bad class accepted")
	}
	pkt[3] = ClassData
	// Reserved field must be zero.
	pkt[31] = 1
	if _, ok := ParseHeader(pkt); ok {
		t.Fatal("nonzero reserved accepted")
	}
	pkt[31] = 0
	// DATA datagram length must equal 32 + payload_length exactly.
	if _, ok := ParseHeader(pkt[:len(pkt)-1]); ok {
		t.Fatal("datagram/payload_length mismatch accepted")
	}
	// payload_length < 2*TagBytes cannot carry the minimum plaintext.
	binary.BigEndian.PutUint16(pkt[24:], 2*TagBytes-1)
	if _, ok := ParseHeader(pkt[:HeaderBytes+2*TagBytes-1]); ok {
		t.Fatal("sub-minimum payload accepted")
	}
}

// TestStreamIDLayout: bit-level field packing of section 12.1 handles.
func TestStreamIDLayout(t *testing.T) {
	sid := MakeStreamID(0x3, 0x2A, true, StreamInitiatorServer)
	if sid.Generation() != 3 || sid.Slot() != 0x2A || !sid.Uni() ||
		sid.Initiator() != StreamInitiatorServer {
		t.Fatalf("sid fields: gen=%d slot=%d uni=%v init=%d",
			sid.Generation(), sid.Slot(), sid.Uni(), sid.Initiator())
	}
	if sid.Class() != 3 { // uni<<1 | initiator
		t.Fatalf("class = %d, want 3", sid.Class())
	}
	if !sid.Valid() {
		t.Fatal("valid sid rejected")
	}
	if MakeStreamID(0, MaxStreamSlots, false, 0).Valid() {
		t.Fatal("slot == 1024 accepted")
	}
	max := MakeStreamID(MaxGeneration, MaxStreamSlots-1, true, 1)
	if !max.Valid() || max.Generation() != MaxGeneration {
		t.Fatal("generation bound wrong")
	}
}

// TestBodyParserStrict: every fixed-size body decoder rejects wrong
// lengths and nonzero reserved fields.
func TestBodyParserStrict(t *testing.T) {
	if _, ok := ParseU64(make([]byte, 9)); ok {
		t.Fatal("ParseU64 accepted len 9")
	}
	if _, _, _, ok := ParseResetStream(make([]byte, 19)); ok {
		t.Fatal("ParseResetStream accepted len 19")
	}
	rst := make([]byte, 20)
	rst[19] = 1 // reserved tail must be zero
	if _, _, _, ok := ParseResetStream(rst); ok {
		t.Fatal("ParseResetStream accepted nonzero reserved")
	}
	ms := make([]byte, 8)
	BuildMaxSlots(ms, 2, 7)
	class, lim, ok := ParseMaxSlots(ms)
	if !ok || class != 2 || lim != 7 {
		t.Fatal("MAX_SLOTS round-trip failed")
	}
	ms[2] = 1
	if _, _, ok := ParseMaxSlots(ms); ok {
		t.Fatal("MAX_SLOTS accepted nonzero reserved")
	}
	var cl [24]byte
	BuildClose(cl[:], 4, 0x11, [16]byte{9})
	code, off, _, ok := ParseClose(cl[:])
	if !ok || code != 4 || off != 0x11 {
		t.Fatal("CLOSE round-trip failed")
	}
	if _, _, _, ok := ParseClose(cl[:23]); ok {
		t.Fatal("short CLOSE accepted")
	}
}

func TestPaddingBodyMustBeZero(t *testing.T) {
	var buf [32]byte
	fb := NewFrameBuilder(buf[:0])
	if !fb.Pad(24) {
		t.Fatal("Pad failed")
	}
	var out [MaxFrames]Frame
	if _, _, err := ParseFrames(fb.Bytes(), out[:0]); err != nil {
		t.Fatalf("valid PADDING rejected: %v", err)
	}
	bad := append([]byte(nil), fb.Bytes()...)
	bad[FrameHeaderBytes] = 1
	if _, _, err := ParseFrames(bad, out[:0]); err != ErrFramePadding {
		t.Fatalf("nonzero PADDING body got %v, want ErrFramePadding", err)
	}
}

func FuzzParseFrames(f *testing.F) {
	var buf [64]byte
	fb := NewFrameBuilder(buf[:0])
	fb.Add(FramePing, 0, 0, nil)
	fb.Pad(32)
	f.Add(fb.Bytes())
	f.Fuzz(func(t *testing.T, pt []byte) {
		var out [MaxFrames]Frame
		frames, _, err := ParseFrames(pt, out[:0])
		if err != nil {
			return
		}
		for i := range frames {
			fr := &frames[i]
			switch fr.Type {
			case FrameStream:
				_, _, _, _ = ParseStreamBody(fr.Body)
			case FrameAck:
				_, _ = ParseAckBody(fr.Body)
			case FrameResetStream:
				_, _, _, _ = ParseResetStream(fr.Body)
			case FrameClose:
				_, _, _, _ = ParseClose(fr.Body)
			case FrameCryptoFragment:
				_, _ = ParseCryptoFrag(fr.Body)
			}
		}
	})
}

func FuzzParseFragment(f *testing.F) {
	frags := BuildFragments(make([]byte, 128), ClassClientFlight, 1, [AttemptIDBytes]byte{1}, [TokenBytes]byte{})
	if len(frags) > 0 && len(frags[0]) > HeaderBytes {
		f.Add(frags[0][HeaderBytes:])
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = ParseFragment(b)
	})
}

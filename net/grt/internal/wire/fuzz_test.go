package wire

import (
	"bytes"
	"testing"
)

func FuzzParseFramesAndBodies(f *testing.F) {
	// Seed valid frame sequences.
	fb := NewFrameBuilder(make([]byte, 0, 1024))
	var ack AckFrame
	ack.Count = 1
	ack.Ranges[0] = [2]uint64{10, 20}
	ackBuf := make([]byte, AckBodyLen(1))
	BuildAckBody(ackBuf, &ack)
	_ = fb.AddBytes(FramePing, 0, nil)
	_ = fb.AddBytes(FrameAck, 0, ackBuf)
	_ = fb.AddBytes(FrameStream, FrameFlagStreamFin, make([]byte, 24))
	f.Add(fb.Bytes())
	f.Add(buildOne(FrameClose, 0, make([]byte, 24)))
	f.Add(buildOne(FrameDatagram, 0, make([]byte, 16)))

	f.Fuzz(func(t *testing.T, data []byte) {
		var scratch [MaxFrames]Frame
		frames, _, err := ParseFrames(data, scratch[:])
		if err == nil {
			for i := range frames {
				b := frames[i].Body
				_, _ = ParseAckBody(b)
				_, _, _, _ = ParseStreamBody(b)
				_, _ = ParseU64(b)
				_, _, _, _ = ParseResetStream(b)
				_, _, _ = ParseStopSending(b)
				_, _, _ = ParseMaxStreamData(b)
				_, _, _ = ParseMaxSlots(b)
				_, _, _ = ParseDatagram(b)
				_, _, _, _ = ParseClose(b)
				_, _, _ = ParseFinish(b)
				_, _ = ParseHandshakeDone(b)
				_, _ = Parse16(b)
				_, _, _, _ = ParsePathIntent(b)
				_, _, _ = ParsePathSelected(b)
				_, _, _, _, _ = ParseNewCID(b)
				_, _ = ParseNewToken(b)
				_, _, _ = ParseNewTicket(b)
				_, _ = ParseCryptoFrag(b)
				_, _, _, _ = ParseKeyCommit(b)
				_, _ = ParseKeyRequest(b)
				_, _, _ = ParseParkRequest(b)
				_, _, _, _ = ParseParkGrant(b)
				_, _, _ = ParseParkWake(b)
			}
		}

		// Also exercise body decoders directly on raw fuzz bytes.
		if a, ok := ParseAckBody(data); ok {
			out := make([]byte, AckBodyLen(a.Count))
			BuildAckBody(out, &a)
			if !bytes.Equal(out, data) {
				t.Fatalf("ParseAckBody round-trip mismatch")
			}
		}
		_, _, _, _ = ParseStreamBody(data)
		_, _ = ParseU64(data)
		_, _, _, _ = ParseResetStream(data)
		_, _, _ = ParseStopSending(data)
		_, _, _ = ParseMaxStreamData(data)
		_, _, _ = ParseMaxSlots(data)
		_, _, _ = ParseDatagram(data)
		_, _, _, _ = ParseClose(data)
		_, _, _ = ParseFinish(data)
		_, _ = ParseHandshakeDone(data)
		_, _ = Parse16(data)
		_, _, _, _ = ParsePathIntent(data)
		_, _, _ = ParsePathSelected(data)
		_, _, _, _, _ = ParseNewCID(data)
		_, _ = ParseNewToken(data)
		_, _, _ = ParseNewTicket(data)
		_, _ = ParseCryptoFrag(data)
		_, _, _, _ = ParseKeyCommit(data)
		_, _ = ParseKeyRequest(data)
		_, _, _ = ParseParkRequest(data)
		_, _, _, _ = ParseParkGrant(data)
		_, _, _ = ParseParkWake(data)
	})
}

func FuzzParseHeaderFragmentAndFlights(f *testing.F) {
	var hdr [HeaderBytes]byte
	BuildHeader(hdr[:], ClassData, 0x1234, 1, 7, 64, true)
	f.Add(append(hdr[:], make([]byte, 64)...))

	cm := ClientMessage{
		Offer:             [MaxCipherOffers]uint16{CipherXChaCha20Poly1305, CipherAES256GCM},
		ClientInitialCID:  1,
		OriginalDCID:      2,
		AuthMode:          AuthPinned,
		Authority:         "example.com:4433",
		ExpectedServerKey: [32]byte{1},
	}
	if enc, err := EncodeClientMessage(&cm); err == nil {
		f.Add(enc)
	}

	var pBuf [ParamsBytes]byte
	defP := DefaultParams()
	EncodeParams(pBuf[:], &defP)
	f.Add(pBuf[:])

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseHeader(data)
		if len(data) >= HeaderBytes {
			_, _ = ParseFragment(data[HeaderBytes:])
		}
		_, _ = ParseFragment(data)
		_, _ = DecodeToken(data)
		if p, ok := DecodeParams(data); ok {
			var re [ParamsBytes]byte
			EncodeParams(re[:], &p)
			if !bytes.Equal(re[:], data) {
				t.Fatalf("DecodeParams round-trip mismatch")
			}
		}
		if parsedCM, _, _, err := ParseClientMessage(data); err == nil {
			re, err2 := EncodeClientMessage(&parsedCM)
			if err2 != nil || !bytes.Equal(re, data) {
				t.Fatalf("ParseClientMessage round-trip mismatch: err=%v", err2)
			}
		}
		_, _ = ParseServerPrefix(data)
		var sm ServerMessage
		_ = ParseServerBody(data, &sm)
	})
}

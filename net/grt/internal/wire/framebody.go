package wire

import "encoding/binary"

// Typed frame-body decoders. Each returns ok=false on a length/shape
// violation. Bodies alias the authenticated plaintext; these decoders do not
// authenticate anything themselves.

// AckFrame is a parsed ACK body.
type AckFrame struct {
	AckDelayUS uint32
	ECT0       uint64
	ECT1       uint64
	CE         uint64
	Ranges     [MaxAckRanges][2]uint64 // inclusive [low,high], sorted by desc high
	Count      int
}

// ParseAckBody validates the §13.1 encoding including range ordering.
func ParseAckBody(b []byte) (AckFrame, bool) {
	var a AckFrame
	if len(b) < 32 {
		return a, false
	}
	a.AckDelayUS = binary.BigEndian.Uint32(b[0:])
	rc := int(binary.BigEndian.Uint16(b[4:]))
	if binary.BigEndian.Uint16(b[6:]) != 0 || rc < 1 || rc > MaxAckRanges {
		return a, false
	}
	if len(b) != 32+16*rc {
		return a, false
	}
	a.ECT0 = binary.BigEndian.Uint64(b[8:])
	a.ECT1 = binary.BigEndian.Uint64(b[16:])
	a.CE = binary.BigEndian.Uint64(b[24:])
	prevLow := uint64(0)
	prevHi := uint64(0)
	for i := 0; i < rc; i++ {
		lo := binary.BigEndian.Uint64(b[32+i*16:])
		hi := binary.BigEndian.Uint64(b[32+i*16+8:])
		if lo > hi {
			return a, false
		}
		if i > 0 {
			// Sorted by descending high; ranges must neither overlap nor
			// touch: next.high+1 < previous.low.
			if hi >= prevHi || hi+1 >= prevLow {
				return a, false
			}
		}
		a.Ranges[i] = [2]uint64{lo, hi}
		prevLow = lo
		prevHi = hi
	}
	a.Count = rc
	return a, true
}

// AckBodyLen returns the encoded ACK body length for n ranges.
func AckBodyLen(n int) int { return 32 + 16*n }

// BuildAckBody serializes an ACK frame body.
func BuildAckBody(dst []byte, a *AckFrame) {
	binary.BigEndian.PutUint32(dst[0:], a.AckDelayUS)
	binary.BigEndian.PutUint16(dst[4:], uint16(a.Count))
	binary.BigEndian.PutUint16(dst[6:], 0)
	binary.BigEndian.PutUint64(dst[8:], a.ECT0)
	binary.BigEndian.PutUint64(dst[16:], a.ECT1)
	binary.BigEndian.PutUint64(dst[24:], a.CE)
	for i := 0; i < a.Count; i++ {
		binary.BigEndian.PutUint64(dst[32+i*16:], a.Ranges[i][0])
		binary.BigEndian.PutUint64(dst[32+i*16+8:], a.Ranges[i][1])
	}
}

// FrameStream fields. Body = stream_id:8 || offset:8 || data.
func ParseStreamBody(b []byte) (sid, off uint64, data []byte, ok bool) {
	if len(b) < 16 {
		return 0, 0, nil, false
	}
	return binary.BigEndian.Uint64(b[0:]), binary.BigEndian.Uint64(b[8:]), b[16:], true
}

func BuildStreamHead(dst []byte, sid, off uint64) {
	binary.BigEndian.PutUint64(dst[0:], sid)
	binary.BigEndian.PutUint64(dst[8:], off)
}

// ParseU64 decodes a single-u64 body (STREAM_OPEN, MAX_DATA, STREAM_RETIRE,
// RETIRE_ACK, RETIRE_CID, MTU_PROBE).
func ParseU64(b []byte) (uint64, bool) {
	if len(b) != 8 {
		return 0, false
	}
	return binary.BigEndian.Uint64(b), true
}

// ParseResetStream decodes RESET_STREAM: sid:8, final:8, err:4, zero:4.
func ParseResetStream(b []byte) (sid, final uint64, code uint32, ok bool) {
	if len(b) != 20 || binary.BigEndian.Uint32(b[16:]) != 0 {
		return 0, 0, 0, false
	}
	return binary.BigEndian.Uint64(b[0:]), binary.BigEndian.Uint64(b[8:]),
		binary.BigEndian.Uint32(b[12:]), true
}

func BuildResetStream(dst []byte, sid, final uint64, code uint32) {
	binary.BigEndian.PutUint64(dst[0:], sid)
	binary.BigEndian.PutUint64(dst[8:], final)
	binary.BigEndian.PutUint32(dst[12:], code)
	binary.BigEndian.PutUint32(dst[16:], 0)
}

// ParseStopSending decodes STOP_SENDING: sid:8, err:4, zero:4.
func ParseStopSending(b []byte) (sid uint64, code uint32, ok bool) {
	if len(b) != 16 || binary.BigEndian.Uint32(b[12:]) != 0 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint64(b[0:]), binary.BigEndian.Uint32(b[8:]), true
}

func BuildStopSending(dst []byte, sid uint64, code uint32) {
	binary.BigEndian.PutUint64(dst[0:], sid)
	binary.BigEndian.PutUint32(dst[8:], code)
	binary.BigEndian.PutUint32(dst[12:], 0)
}

// ParseMaxStreamData decodes MAX_STREAM_DATA / STREAM_BLOCKED:
// sid:8, limit:8.
func ParseMaxStreamData(b []byte) (sid, limit uint64, ok bool) {
	if len(b) != 16 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint64(b[0:]), binary.BigEndian.Uint64(b[8:]), true
}

func BuildMaxStreamData(dst []byte, sid, limit uint64) {
	binary.BigEndian.PutUint64(dst[0:], sid)
	binary.BigEndian.PutUint64(dst[8:], limit)
}

// ParseMaxSlots decodes MAX_SLOTS / SLOTS_BLOCKED: class:1, zero:3, limit:4.
func ParseMaxSlots(b []byte) (class uint8, limit uint32, ok bool) {
	if len(b) != 8 {
		return 0, 0, false
	}
	if b[1] != 0 || b[2] != 0 || b[3] != 0 {
		return 0, 0, false
	}
	return b[0], binary.BigEndian.Uint32(b[4:]), true
}

func BuildMaxSlots(dst []byte, class uint8, limit uint32) {
	dst[0] = class
	clear(dst[1:4])
	binary.BigEndian.PutUint32(dst[4:], limit)
}

// ParseDatagram decodes DATAGRAM: context_id:8 || data.
func ParseDatagram(b []byte) (ctx uint64, data []byte, ok bool) {
	if len(b) < 8 {
		return 0, nil, false
	}
	return binary.BigEndian.Uint64(b[0:]), b[8:], true
}

// ParseFinish decodes FINISH: mac:32 || client_reset_token:16.
func ParseFinish(b []byte) (mac [32]byte, token [16]byte, ok bool) {
	if len(b) != 48 {
		return mac, token, false
	}
	copy(mac[:], b[0:32])
	copy(token[:], b[32:48])
	return mac, token, true
}

// ParseHandshakeDone decodes HANDSHAKE_DONE: mac:32.
func ParseHandshakeDone(b []byte) (mac [32]byte, ok bool) {
	if len(b) != 32 {
		return mac, false
	}
	copy(mac[:], b)
	return mac, true
}

// ParseNewCID decodes NEW_CID: seq:8, retire_prior_to:8, cid:8, token:16.
func ParseNewCID(b []byte) (seq, retirePrior, cid uint64, token [16]byte, ok bool) {
	if len(b) != 40 {
		return 0, 0, 0, token, false
	}
	seq = binary.BigEndian.Uint64(b[0:])
	retirePrior = binary.BigEndian.Uint64(b[8:])
	cid = binary.BigEndian.Uint64(b[16:])
	copy(token[:], b[24:40])
	if retirePrior > seq || cid == 0 {
		return seq, retirePrior, cid, token, false
	}
	return seq, retirePrior, cid, token, true
}

func BuildNewCID(dst []byte, seq, retirePrior, cid uint64, token [16]byte) {
	binary.BigEndian.PutUint64(dst[0:], seq)
	binary.BigEndian.PutUint64(dst[8:], retirePrior)
	binary.BigEndian.PutUint64(dst[16:], cid)
	copy(dst[24:], token[:])
}

// ParseClose decodes CLOSE: err:4, offending_frame:2, zero:2, reason_hash:16.
func ParseClose(b []byte) (code uint32, offending uint16, hash [16]byte, ok bool) {
	if len(b) != 24 || binary.BigEndian.Uint16(b[6:]) != 0 {
		return 0, 0, hash, false
	}
	code = binary.BigEndian.Uint32(b[0:])
	offending = binary.BigEndian.Uint16(b[4:])
	copy(hash[:], b[8:24])
	return code, offending, hash, true
}

func BuildClose(dst []byte, code uint32, offending uint16, reasonHash [16]byte) {
	binary.BigEndian.PutUint32(dst[0:], code)
	binary.BigEndian.PutUint16(dst[4:], offending)
	binary.BigEndian.PutUint16(dst[6:], 0)
	copy(dst[8:], reasonHash[:])
}

// Parse16 decodes a fixed 16-byte body (PATH_CHALLENGE, PATH_RESPONSE).
func Parse16(b []byte) (v [16]byte, ok bool) {
	if len(b) != 16 {
		return v, false
	}
	copy(v[:], b)
	return v, true
}

// ParseNewTicket decodes NEW_TICKET: metadata:204 || ticket:224.
func ParseNewTicket(b []byte) (meta [TicketMetaBytes]byte, ticket [TicketBytes]byte, ok bool) {
	if len(b) != TicketMetaBytes+TicketBytes {
		return meta, ticket, false
	}
	copy(meta[:], b[0:TicketMetaBytes])
	copy(ticket[:], b[TicketMetaBytes:])
	return meta, ticket, true
}

// ParseNewToken decodes NEW_TOKEN: address_token:64.
func ParseNewToken(b []byte) (tok [TokenBytes]byte, ok bool) {
	if len(b) != TokenBytes {
		return tok, false
	}
	copy(tok[:], b)
	return tok, true
}

// ParsePathIntent decodes PATH_INTENT: seq:8, cutover_pn:8, flags:4, zero:4.
func ParsePathIntent(b []byte) (seq, cutover uint64, flags uint32, ok bool) {
	if len(b) != 24 || binary.BigEndian.Uint32(b[20:]) != 0 {
		return 0, 0, 0, false
	}
	seq = binary.BigEndian.Uint64(b[0:])
	cutover = binary.BigEndian.Uint64(b[8:])
	flags = binary.BigEndian.Uint32(b[16:])
	if flags&^3 != 0 {
		return 0, 0, 0, false
	}
	return seq, cutover, flags, true
}

// ParsePathSelected decodes PATH_SELECTED: seq:8, status:2, zero:2, zero:4.
func ParsePathSelected(b []byte) (seq uint64, status uint16, ok bool) {
	if len(b) != 16 || binary.BigEndian.Uint16(b[10:]) != 0 || binary.BigEndian.Uint32(b[12:]) != 0 {
		return 0, 0, false
	}
	seq = binary.BigEndian.Uint64(b[0:])
	status = binary.BigEndian.Uint16(b[8:])
	if status > 2 {
		return seq, status, false
	}
	return seq, status, true
}

// CryptoFrag is a parsed CRYPTO_FRAGMENT body (spec section 19.2).
type CryptoFrag struct {
	RefreshID   [16]byte
	Kind        uint8 // 1 OFFER, 2 ANSWER
	TotalLength uint32
	MessageHash [32]byte
	Index       uint16
	Count       uint16
	Data        []byte
}

// CryptoFragPrefix is the fixed CRYPTO_FRAGMENT body prefix length.
const CryptoFragPrefix = 60

// CryptoFragChunk is the non-final fragment data size.
const CryptoFragChunk = 512

func ParseCryptoFrag(b []byte) (CryptoFrag, bool) {
	var c CryptoFrag
	if len(b) < CryptoFragPrefix+1 {
		return c, false
	}
	copy(c.RefreshID[:], b[0:16])
	c.Kind = b[16]
	if c.Kind != 1 && c.Kind != 2 {
		return c, false
	}
	if b[17] != 0 || b[18] != 0 || b[19] != 0 {
		return c, false
	}
	c.TotalLength = binary.BigEndian.Uint32(b[20:])
	copy(c.MessageHash[:], b[24:56])
	c.Index = binary.BigEndian.Uint16(b[56:])
	c.Count = binary.BigEndian.Uint16(b[58:])
	c.Data = b[CryptoFragPrefix:]
	if c.TotalLength < 1 || c.TotalLength > MaxRefreshMessage {
		return c, false
	}
	want := (c.TotalLength + CryptoFragChunk - 1) / CryptoFragChunk
	if c.Count < 1 || c.Count > 8 || uint32(c.Count) != want {
		return c, false
	}
	if c.Index >= c.Count {
		return c, false
	}
	var dlen uint32
	if c.Index == c.Count-1 {
		dlen = c.TotalLength - uint32(c.Index)*CryptoFragChunk
	} else {
		dlen = CryptoFragChunk
	}
	if uint32(len(c.Data)) != dlen {
		return c, false
	}
	return c, true
}

// BuildCryptoFrag serializes a CRYPTO_FRAGMENT body.
func BuildCryptoFrag(dst []byte, c *CryptoFrag) {
	copy(dst[0:], c.RefreshID[:])
	dst[16] = c.Kind
	clear(dst[17:20])
	binary.BigEndian.PutUint32(dst[20:], c.TotalLength)
	copy(dst[24:], c.MessageHash[:])
	binary.BigEndian.PutUint16(dst[56:], c.Index)
	binary.BigEndian.PutUint16(dst[58:], c.Count)
	copy(dst[CryptoFragPrefix:], c.Data)
}

// ParseKeyCommit decodes KEY_COMMIT/KEY_CONFIRM:
// epoch:4, zero:4, refresh_id:16, mac:32.
func ParseKeyCommit(b []byte) (epoch uint32, refreshID [16]byte, mac [32]byte, ok bool) {
	if len(b) != 56 || binary.BigEndian.Uint32(b[4:]) != 0 {
		return 0, refreshID, mac, false
	}
	epoch = binary.BigEndian.Uint32(b[0:])
	copy(refreshID[:], b[8:24])
	copy(mac[:], b[24:56])
	return epoch, refreshID, mac, true
}

func BuildKeyCommit(dst []byte, epoch uint32, refreshID [16]byte, mac [32]byte) {
	binary.BigEndian.PutUint32(dst[0:], epoch)
	binary.BigEndian.PutUint32(dst[4:], 0)
	copy(dst[8:], refreshID[:])
	copy(dst[24:], mac[:])
}

// ParseKeyRequest decodes KEY_REQUEST: requested_epoch:4, zero:4.
func ParseKeyRequest(b []byte) (uint32, bool) {
	if len(b) != 8 || binary.BigEndian.Uint32(b[4:]) != 0 {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[0:]), true
}

// ParseParkRequest decodes PARK_REQUEST: request_id:8, duration_ms:4, zero:4.
func ParseParkRequest(b []byte) (reqID uint64, durMS uint32, ok bool) {
	if len(b) != 16 || binary.BigEndian.Uint32(b[12:]) != 0 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint64(b[0:]), binary.BigEndian.Uint32(b[8:]), true
}

// ParseParkGrant decodes PARK_GRANT: request_id:8, duration_ms:4, status:2, zero:2.
func ParseParkGrant(b []byte) (reqID uint64, durMS uint32, status uint16, ok bool) {
	if len(b) != 16 || binary.BigEndian.Uint16(b[14:]) != 0 {
		return 0, 0, 0, false
	}
	status = binary.BigEndian.Uint16(b[12:])
	if status > 2 {
		return 0, 0, 0, false
	}
	return binary.BigEndian.Uint64(b[0:]), binary.BigEndian.Uint32(b[8:]), status, true
}

// ParseParkWake decodes PARK_WAKE / PARK_AWAKE: request_id:8, wake_seq:8.
func ParseParkWake(b []byte) (reqID, wakeSeq uint64, ok bool) {
	if len(b) != 16 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint64(b[0:]), binary.BigEndian.Uint64(b[8:]), true
}

// BuildPathIntent serializes PATH_INTENT: seq:8, cutover_pn:8, flags:4, zero:4.
func BuildPathIntent(dst []byte, seq, cutover uint64, flags uint32) {
	binary.BigEndian.PutUint64(dst[0:], seq)
	binary.BigEndian.PutUint64(dst[8:], cutover)
	binary.BigEndian.PutUint32(dst[16:], flags)
	binary.BigEndian.PutUint32(dst[20:], 0)
}

// BuildPathSelected serializes PATH_SELECTED: seq:8, status:2, zero:2, zero:4.
func BuildPathSelected(dst []byte, seq uint64, status uint16) {
	binary.BigEndian.PutUint64(dst[0:], seq)
	binary.BigEndian.PutUint16(dst[8:], status)
	binary.BigEndian.PutUint16(dst[10:], 0)
	binary.BigEndian.PutUint32(dst[12:], 0)
}

// BuildParkRequest serializes PARK_REQUEST: request_id:8, duration_ms:4, zero:4.
func BuildParkRequest(dst []byte, reqID uint64, durMS uint32) {
	binary.BigEndian.PutUint64(dst[0:], reqID)
	binary.BigEndian.PutUint32(dst[8:], durMS)
	binary.BigEndian.PutUint32(dst[12:], 0)
}

// BuildParkGrant serializes PARK_GRANT: request_id:8, duration_ms:4, status:2, zero:2.
func BuildParkGrant(dst []byte, reqID uint64, durMS uint32, status uint16) {
	binary.BigEndian.PutUint64(dst[0:], reqID)
	binary.BigEndian.PutUint32(dst[8:], durMS)
	binary.BigEndian.PutUint16(dst[12:], status)
	binary.BigEndian.PutUint16(dst[14:], 0)
}

// BuildParkWake serializes PARK_WAKE / PARK_AWAKE: request_id:8, wake_seq:8.
func BuildParkWake(dst []byte, reqID, wakeSeq uint64) {
	binary.BigEndian.PutUint64(dst[0:], reqID)
	binary.BigEndian.PutUint64(dst[8:], wakeSeq)
}

// SingletonCount is the number of at-most-once-per-packet frame types.
const SingletonCount = 16

// SingletonIndex maps a frame type to its dense singleton-table index, or
// reports false when duplicates are permitted. Covers the complete
// section-11 singleton list including PATH_CHALLENGE and PATH_RESPONSE.
// KEY_REQUEST is exempt ("a deduplicated request stream is permitted").
func SingletonIndex(typ uint16) (int, bool) {
	switch typ {
	case FrameAck:
		return 0, true
	case FrameClose:
		return 1, true
	case FrameFinish:
		return 2, true
	case FrameHandshakeDone:
		return 3, true
	case FrameKeyCommit:
		return 4, true
	case FrameKeyConfirm:
		return 5, true
	case FrameMTUProbe:
		return 6, true
	case FrameNewToken:
		return 7, true
	case FramePathChallenge:
		return 8, true
	case FramePathIntent:
		return 9, true
	case FramePathResponse:
		return 10, true
	case FramePathSelected:
		return 11, true
	case FrameParkRequest:
		return 12, true
	case FrameParkGrant:
		return 13, true
	case FrameParkWake:
		return 14, true
	case FrameParkAwake:
		return 15, true
	}
	return 0, false
}

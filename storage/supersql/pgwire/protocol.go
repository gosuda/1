package pgwire

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/gosuda/supersql-alpha"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

const (
	protocolVersion30 int32 = 196608 // (3 << 16) | 0
	sslRequestCode    int32 = 80877103
	cancelRequestCode int32 = 80877102
	gssEncRequestCode int32 = 80877104

	maxStartupPacketBytes   = 64 * 1024
	maxPasswordMessageBytes = 64 * 1024
	maxMessageBytes         = 64 * 1024 * 1024
	frameReadChunkBytes     = 4 * 1024

	authTypeOk                int32 = 0
	authTypeCleartextPassword int32 = 3
	authTypeSASL              int32 = 10
	authTypeSASLContinue      int32 = 11
	authTypeSASLFinal         int32 = 12

	txStatusIdle   byte = 'I'
	txStatusInTx   byte = 'T'
	txStatusFailed byte = 'E'
)

// columnDesc describes a single result column for RowDescription ('T').
type columnDesc struct {
	Name       string
	TableOID   uint32
	AttrNum    int16
	TypeOID    uint32
	TypeSize   int16
	TypeMod    int32
	FormatCode int16
}

// wireError represents a PostgreSQL ErrorResponse ('E') diagnostic.
type wireError struct {
	Severity   string
	Code       string
	Message    string
	Detail     string
	Hint       string
	Position   int
	Table      string
	Column     string
	Constraint string
}

func toWireError(err error, severity string) wireError {
	if severity == "" {
		severity = "ERROR"
	}
	if err == nil {
		return wireError{Severity: severity, Code: "XX000", Message: "unknown error"}
	}
	var we *wireError
	if errors.As(err, &we) {
		out := *we
		if out.Severity == "" {
			out.Severity = severity
		}
		if out.Code == "" {
			out.Code = "XX000"
		}
		return out
	}
	var sqlErr *supersql.Error
	if errors.As(err, &sqlErr) {
		code := sqlErr.Code
		if code == "" {
			code = "XX000"
		}
		return wireError{
			Severity:   severity,
			Code:       code,
			Message:    sqlErr.Error(),
			Detail:     sqlErr.Detail,
			Hint:       sqlErr.Hint,
			Position:   sqlErr.Position,
			Table:      sqlErr.Table,
			Column:     sqlErr.Column,
			Constraint: sqlErr.Constraint,
		}
	}
	var parseErr *zpgparse.Error
	if errors.As(err, &parseErr) {
		code := parseErr.Code
		if code == "" {
			code = "42601"
		}
		return wireError{
			Severity: severity,
			Code:     code,
			Message:  parseErr.Message,
			Detail:   parseErr.Detail,
			Hint:     parseErr.Hint,
			Position: parseErr.Cursorpos,
		}
	}
	if errors.Is(err, context.Canceled) {
		return wireError{
			Severity: severity,
			Code:     "57014",
			Message:  "canceling statement due to user request",
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return wireError{
			Severity: severity,
			Code:     "57014",
			Message:  "canceling statement due to statement timeout",
		}
	}
	code := supersql.SQLState(err)
	if code == "" {
		code = "XX000"
	}
	return wireError{
		Severity: severity,
		Code:     code,
		Message:  err.Error(),
	}
}

func (e *wireError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s (SQLSTATE %s)", e.Severity, e.Message, e.Code)
}

// startupPacket is the raw length-prefixed initial packet sent by a client
// before switching to regular typed frontend messages.
type startupPacket struct {
	Code   int32
	Body   []byte
	Params map[string]string
	PID    int32
	Secret int32
}

func readMessageBody(r io.Reader, bodyLen int) ([]byte, error) {
	if bodyLen == 0 {
		return nil, nil
	}
	if bodyLen < 0 {
		return nil, fmt.Errorf("invalid message body length: %d", bodyLen)
	}
	buf := make([]byte, 0, min(bodyLen, frameReadChunkBytes))
	remaining := bodyLen
	for remaining > 0 {
		if len(buf) == cap(buf) {
			nextCap := min(bodyLen, max(cap(buf)*2, frameReadChunkBytes))
			grown := make([]byte, len(buf), nextCap)
			copy(grown, buf)
			buf = grown
		}
		end := min(cap(buf), len(buf)+remaining)
		n, err := r.Read(buf[len(buf):end])
		if n > 0 {
			buf = buf[:len(buf)+n]
			remaining -= n
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if remaining == 0 {
					return buf, nil
				}
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
	}
	return buf, nil
}

func readStartupPacket(r io.Reader) (startupPacket, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return startupPacket{}, err
	}
	rawLen := binary.BigEndian.Uint32(lenBuf[:])
	if rawLen < 8 || rawLen > maxStartupPacketBytes {
		return startupPacket{}, fmt.Errorf("invalid startup packet length: %d", rawLen)
	}
	payload, err := readMessageBody(r, int(rawLen)-4)
	if err != nil {
		return startupPacket{}, err
	}
	code := int32(binary.BigEndian.Uint32(payload[0:4]))
	pkt := startupPacket{
		Code: code,
		Body: payload[4:],
	}
	switch code {
	case sslRequestCode, gssEncRequestCode:
		if len(pkt.Body) != 0 {
			return startupPacket{}, fmt.Errorf("unexpected payload length %d for request code %d", len(pkt.Body), code)
		}
	case cancelRequestCode:
		if len(pkt.Body) != 8 {
			return startupPacket{}, fmt.Errorf("invalid cancel request payload length: %d", len(pkt.Body))
		}
		pkt.PID = int32(binary.BigEndian.Uint32(pkt.Body[0:4]))
		pkt.Secret = int32(binary.BigEndian.Uint32(pkt.Body[4:8]))
	case protocolVersion30:
		params, err := parseStartupParams(pkt.Body)
		if err != nil {
			return startupPacket{}, err
		}
		pkt.Params = params
	}
	return pkt, nil
}

func parseStartupParams(body []byte) (map[string]string, error) {
	params := make(map[string]string)
	pos := 0
	for pos < len(body) {
		if body[pos] == 0 {
			return params, nil
		}
		key, next, ok := readCString(body, pos)
		if !ok {
			return nil, errors.New("unterminated startup parameter key")
		}
		val, next2, ok := readCString(body, next)
		if !ok {
			return nil, errors.New("unterminated startup parameter value")
		}
		params[key] = val
		pos = next2
	}
	return params, nil
}

func readFrontendMessage(r *bufio.Reader) (byte, []byte, error) {
	return readFrontendMessageWithLimit(r, maxMessageBytes)
}

func readFrontendMessageWithLimit(r *bufio.Reader, maxBytes int) (byte, []byte, error) {
	msgType, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return 0, nil, err
	}
	rawLen := binary.BigEndian.Uint32(lenBuf[:])
	limit := maxBytes
	if msgType == 'p' && limit > maxPasswordMessageBytes {
		limit = maxPasswordMessageBytes
	}
	if rawLen < 4 || rawLen > uint32(limit) {
		return 0, nil, &wireError{
			Severity: "FATAL",
			Code:     "08P01",
			Message:  fmt.Sprintf("invalid frontend message length: %d", rawLen),
		}
	}
	body, err := readMessageBody(r, int(rawLen)-4)
	if err != nil {
		return 0, nil, err
	}
	return msgType, body, nil
}

func readCString(buf []byte, pos int) (string, int, bool) {
	for i := pos; i < len(buf); i++ {
		if buf[i] == 0 {
			return string(buf[pos:i]), i + 1, true
		}
	}
	return "", pos, false
}

// Frame builders: each appends a complete PostgreSQL v3 backend message.

func appendMessage(dst []byte, msgType byte, body []byte) []byte {
	dst = append(dst, msgType)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(4+len(body)))
	dst = append(dst, lenBuf[:]...)
	dst = append(dst, body...)
	return dst
}

func encodeAuthMessage(authType int32) []byte {
	var body [4]byte
	binary.BigEndian.PutUint32(body[:], uint32(authType))
	return appendMessage(make([]byte, 0, 9), 'R', body[:])
}

func encodeAuthSASL(mechanisms ...string) []byte {
	size := 4 + 1
	for _, m := range mechanisms {
		size += len(m) + 1
	}
	body := make([]byte, 4, size)
	binary.BigEndian.PutUint32(body[0:4], uint32(authTypeSASL))
	for _, m := range mechanisms {
		body = append(body, m...)
		body = append(body, 0)
	}
	body = append(body, 0)
	return appendMessage(make([]byte, 0, 5+len(body)), 'R', body)
}

func encodeAuthSASLContinue(data string) []byte {
	body := make([]byte, 4, 4+len(data))
	binary.BigEndian.PutUint32(body[0:4], uint32(authTypeSASLContinue))
	body = append(body, data...)
	return appendMessage(make([]byte, 0, 5+len(body)), 'R', body)
}

func encodeAuthSASLFinal(data string) []byte {
	body := make([]byte, 4, 4+len(data))
	binary.BigEndian.PutUint32(body[0:4], uint32(authTypeSASLFinal))
	body = append(body, data...)
	return appendMessage(make([]byte, 0, 5+len(body)), 'R', body)
}

func encodeParameterStatus(name, value string) []byte {
	body := make([]byte, 0, len(name)+len(value)+2)
	body = append(body, name...)
	body = append(body, 0)
	body = append(body, value...)
	body = append(body, 0)
	return appendMessage(make([]byte, 0, 5+len(body)), 'S', body)
}

func encodeBackendKeyData(pid, secret int32) []byte {
	var body [8]byte
	binary.BigEndian.PutUint32(body[0:4], uint32(pid))
	binary.BigEndian.PutUint32(body[4:8], uint32(secret))
	return appendMessage(make([]byte, 0, 13), 'K', body[:])
}

func encodeReadyForQuery(txStatus byte) []byte {
	return []byte{'Z', 0, 0, 0, 5, txStatus}
}

func encodeEmptyQueryResponse() []byte {
	return []byte{'I', 0, 0, 0, 4}
}

func encodeParseComplete() []byte {
	return []byte{'1', 0, 0, 0, 4}
}

func encodeBindComplete() []byte {
	return []byte{'2', 0, 0, 0, 4}
}

func encodeCloseComplete() []byte {
	return []byte{'3', 0, 0, 0, 4}
}

func encodeNoData() []byte {
	return []byte{'n', 0, 0, 0, 4}
}

func encodePortalSuspended() []byte {
	return []byte{'s', 0, 0, 0, 4}
}

func encodeCommandComplete(tag string) []byte {
	body := make([]byte, 0, len(tag)+1)
	body = append(body, tag...)
	body = append(body, 0)
	return appendMessage(make([]byte, 0, 5+len(body)), 'C', body)
}

func encodeParameterDescription(oids []uint32) []byte {
	body := make([]byte, 2+4*len(oids))
	binary.BigEndian.PutUint16(body[0:2], uint16(len(oids)))
	for i, oid := range oids {
		binary.BigEndian.PutUint32(body[2+4*i:2+4*i+4], oid)
	}
	return appendMessage(make([]byte, 0, 5+len(body)), 't', body)
}

func encodeRowDescription(cols []columnDesc) []byte {
	body := make([]byte, 2, 2+len(cols)*32)
	binary.BigEndian.PutUint16(body[0:2], uint16(len(cols)))
	var fieldMeta [18]byte
	for _, col := range cols {
		body = append(body, col.Name...)
		body = append(body, 0)
		binary.BigEndian.PutUint32(fieldMeta[0:4], col.TableOID)
		binary.BigEndian.PutUint16(fieldMeta[4:6], uint16(col.AttrNum))
		binary.BigEndian.PutUint32(fieldMeta[6:10], col.TypeOID)
		binary.BigEndian.PutUint16(fieldMeta[10:12], uint16(col.TypeSize))
		binary.BigEndian.PutUint32(fieldMeta[12:16], uint32(col.TypeMod))
		binary.BigEndian.PutUint16(fieldMeta[16:18], uint16(col.FormatCode))
		body = append(body, fieldMeta[:]...)
	}
	return appendMessage(make([]byte, 0, 5+len(body)), 'T', body)
}

func encodeDataRow(fields [][]byte) []byte {
	size := 2
	for _, f := range fields {
		size += 4 + len(f)
	}
	body := make([]byte, 2, size)
	binary.BigEndian.PutUint16(body[0:2], uint16(len(fields)))
	var lenBuf [4]byte
	for _, f := range fields {
		if f == nil {
			binary.BigEndian.PutUint32(lenBuf[:], 0xFFFFFFFF) // -1 = NULL
			body = append(body, lenBuf[:]...)
			continue
		}
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(f)))
		body = append(body, lenBuf[:]...)
		body = append(body, f...)
	}
	return appendMessage(make([]byte, 0, 5+len(body)), 'D', body)
}

func encodeErrorResponse(e wireError) []byte {
	severity := e.Severity
	if severity == "" {
		severity = "ERROR"
	}
	code := e.Code
	if code == "" {
		code = "XX000"
	}
	body := make([]byte, 0, 64+len(e.Message))
	appendField := func(codeByte byte, val string) {
		if val == "" {
			return
		}
		body = append(body, codeByte)
		body = append(body, val...)
		body = append(body, 0)
	}
	appendField('S', severity)
	appendField('V', severity)
	appendField('C', code)
	appendField('M', e.Message)
	appendField('D', e.Detail)
	appendField('H', e.Hint)
	if e.Position > 0 {
		appendField('P', strconv.Itoa(e.Position))
	}
	appendField('t', e.Table)
	appendField('c', e.Column)
	appendField('n', e.Constraint)
	body = append(body, 0)
	return appendMessage(make([]byte, 0, 5+len(body)), 'E', body)
}

func encodeNotificationResponse(pid int32, channel, payload string) []byte {
	body := make([]byte, 4, 4+len(channel)+1+len(payload)+1)
	binary.BigEndian.PutUint32(body[0:4], uint32(pid))
	body = append(body, channel...)
	body = append(body, 0)
	body = append(body, payload...)
	body = append(body, 0)
	return appendMessage(make([]byte, 0, 5+len(body)), 'A', body)
}

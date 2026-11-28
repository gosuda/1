package certmon

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	dnsTypeCNAME  uint16 = 5
	dnsTypeDNAME  uint16 = 39
	dnsTypeCAA    uint16 = 257
	dnsClassIN    uint16 = 1
	dnsRcodeOK    uint8  = 0
	dnsRcodeNXDOM uint8  = 3
	maxAliasHops         = 8
	maxDNSPacket         = 65535
)

var (
	errCAAValueInvalid        = errors.New("CAA value is not valid UTF-8")
	errCAATagInvalid          = errors.New("invalid CAA property tag")
	errCAAAliasLoop           = errors.New("CAA alias loop")
	errCAAResponseInvalid     = errors.New("CAA DNS response is invalid")
	errCAAConflictingAliases  = errors.New("CAA response contains conflicting aliases")
	errCAARecordsAndAlias     = errors.New("CAA response contains both alias and CAA records")
	errCAANegativeWithRecords = errors.New("CAA negative answer contains CAA records")
	errCAAAliasInvalid        = errors.New("invalid CAA alias target")
	errCAAAliasLimit          = errors.New("CAA alias chain exceeds limit")
)

func canonicalCAA(records []CAARecord) ([]CAARecord, error) {
	result := slices.Clone(records)
	if result == nil {
		result = []CAARecord{}
	}
	for i := range result {
		if !utf8.ValidString(result[i].Value) {
			return nil, errCAAValueInvalid
		}
		result[i].Tag = strings.ToLower(result[i].Tag)
		if result[i].Tag == "" || len(result[i].Tag) > 255 {
			return nil, errCAATagInvalid
		}
		for _, c := range result[i].Tag {
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
				return nil, errCAATagInvalid
			}
		}
	}
	slices.SortFunc(result, func(a, b CAARecord) int {
		if a.Flags != b.Flags {
			return int(a.Flags) - int(b.Flags)
		}
		if cmp := strings.Compare(a.Tag, b.Tag); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Value, b.Value)
	})
	return slices.Compact(result), nil
}

// observeCAA implements RFC 8659 section 3: each CAA(domain) lookup follows
// CNAME/DNAME aliases; if empty, the tree climb continues with the original
// domain's parent, not the alias target's parent.
func (m *monitor) observeCAA(ctx context.Context) CAAObservation {
	obs := CAAObservation{ObservedAt: time.Now().UTC(), Resolver: m.resolver}
	for owner := m.hostname; owner != ""; owner = parentDomain(owner) {
		records, foundAt, err := m.lookupCAA(ctx, owner)
		if err != nil {
			obs.Error = err.Error()
			return obs
		}
		if len(records) > 0 {
			obs.Owner = foundAt
			obs.Records = records
			return obs
		}
	}
	obs.Records = []CAARecord{}
	return obs
}

func parentDomain(name string) string {
	idx := strings.IndexByte(name, '.')
	if idx < 0 || idx+1 >= len(name) {
		return ""
	}
	return name[idx+1:]
}

func (m *monitor) lookupCAA(ctx context.Context, owner string) ([]CAARecord, string, error) {
	seen := map[string]bool{}
	current := owner
	for range maxAliasHops {
		if seen[current] {
			return nil, "", errCAAAliasLoop
		}
		seen[current] = true

		reply, err := exchangeDNS(ctx, m.resolver, current, dnsTypeCAA)
		if err != nil {
			return nil, "", err
		}
		records, alias, err := parseCAAAnswer(reply, current)
		if err != nil {
			return nil, "", err
		}
		if len(records) > 0 || alias == "" {
			return records, current, nil
		}
		next := normalizeASCIIHostname(alias)
		if next == "" {
			return nil, "", errCAAAliasInvalid
		}
		current = next
	}
	return nil, "", errCAAAliasLimit
}

type dnsMessage struct {
	id        uint16
	response  bool
	truncated bool
	aa        bool
	ra        bool
	rcode     uint8
	qname     string
	qtype     uint16
	qclass    uint16
	answers   []dnsRR
}

type dnsRR struct {
	name  string
	rtype uint16
	class uint16
	rdata []byte
	msg   []byte
	off   int
}

func exchangeDNS(ctx context.Context, resolver, qname string, qtype uint16) (*dnsMessage, error) {
	id := randomDNSID()
	query, err := encodeDNSQuery(id, qname, qtype)
	if err != nil {
		return nil, err
	}
	raw, err := exchangeUDP(ctx, resolver, query)
	if err != nil {
		return nil, err
	}
	reply, err := decodeDNSMessage(raw)
	if err != nil {
		return nil, err
	}
	if reply.id != id {
		return nil, errCAAResponseInvalid
	}
	if reply.truncated {
		raw, err = exchangeTCP(ctx, resolver, query)
		if err != nil {
			return nil, err
		}
		reply, err = decodeDNSMessage(raw)
		if err != nil {
			return nil, err
		}
		if reply.id != id || reply.truncated {
			return nil, errCAAResponseInvalid
		}
	}
	return reply, nil
}

func randomDNSID() uint16 {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint16(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint16(b[:])
}

func exchangeUDP(ctx context.Context, resolver string, query []byte) ([]byte, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", resolver)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func exchangeTCP(ctx context.Context, resolver string, query []byte) ([]byte, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", resolver)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	frame := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(query)))
	copy(frame[2:], query)
	if _, err := conn.Write(frame); err != nil {
		return nil, err
	}
	var lenBuf [2]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return nil, err
	}
	respLen := int(binary.BigEndian.Uint16(lenBuf[:]))
	if respLen == 0 || respLen > maxDNSPacket {
		return nil, errCAAResponseInvalid
	}
	resp := make([]byte, respLen)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func encodeDNSQuery(id uint16, qname string, qtype uint16) ([]byte, error) {
	buf := make([]byte, 12, 64)
	binary.BigEndian.PutUint16(buf[0:2], id)
	binary.BigEndian.PutUint16(buf[2:4], 0x0100) // RD=1
	binary.BigEndian.PutUint16(buf[4:6], 1)      // QDCOUNT=1
	for _, label := range strings.Split(strings.TrimSuffix(qname, "."), ".") {
		if len(label) == 0 || len(label) > 63 {
			return nil, errHostnameInvalid
		}
		buf = append(buf, byte(len(label)))
		buf = append(buf, label...)
	}
	buf = append(buf, 0)
	buf = binary.BigEndian.AppendUint16(buf, qtype)
	buf = binary.BigEndian.AppendUint16(buf, dnsClassIN)
	return buf, nil
}

func decodeDNSMessage(msg []byte) (*dnsMessage, error) {
	if len(msg) < 12 {
		return nil, errCAAResponseInvalid
	}
	flags := binary.BigEndian.Uint16(msg[2:4])
	qdCount := int(binary.BigEndian.Uint16(msg[4:6]))
	anCount := int(binary.BigEndian.Uint16(msg[6:8]))
	if qdCount != 1 {
		return nil, errCAAResponseInvalid
	}
	out := &dnsMessage{
		id:        binary.BigEndian.Uint16(msg[0:2]),
		response:  flags&0x8000 != 0,
		aa:        flags&0x0400 != 0,
		truncated: flags&0x0200 != 0,
		ra:        flags&0x0080 != 0,
		rcode:     uint8(flags & 0x000F),
	}
	off := 12
	qname, next, err := decodeDNSName(msg, off)
	if err != nil || next+4 > len(msg) {
		return nil, errCAAResponseInvalid
	}
	out.qname = strings.ToLower(qname)
	out.qtype = binary.BigEndian.Uint16(msg[next : next+2])
	out.qclass = binary.BigEndian.Uint16(msg[next+2 : next+4])
	off = next + 4

	out.answers = make([]dnsRR, 0, anCount)
	for range anCount {
		name, nOff, err := decodeDNSName(msg, off)
		if err != nil || nOff+10 > len(msg) {
			return nil, errCAAResponseInvalid
		}
		rtype := binary.BigEndian.Uint16(msg[nOff : nOff+2])
		rclass := binary.BigEndian.Uint16(msg[nOff+2 : nOff+4])
		rdlen := int(binary.BigEndian.Uint16(msg[nOff+8 : nOff+10]))
		rdataOff := nOff + 10
		if rdataOff+rdlen > len(msg) {
			return nil, errCAAResponseInvalid
		}
		out.answers = append(out.answers, dnsRR{
			name:  strings.ToLower(name),
			rtype: rtype,
			class: rclass,
			rdata: msg[rdataOff : rdataOff+rdlen],
			msg:   msg,
			off:   rdataOff,
		})
		off = rdataOff + rdlen
	}
	return out, nil
}

func decodeDNSName(msg []byte, off int) (string, int, error) {
	var labels []string
	cur := off
	end := -1
	jumps := 0
	for {
		if cur >= len(msg) {
			return "", 0, errCAAResponseInvalid
		}
		b := msg[cur]
		if b == 0 {
			if end < 0 {
				end = cur + 1
			}
			break
		}
		if b&0xC0 == 0xC0 {
			if cur+1 >= len(msg) {
				return "", 0, errCAAResponseInvalid
			}
			ptr := int(binary.BigEndian.Uint16(msg[cur:cur+2]) & 0x3FFF)
			if ptr >= len(msg) {
				return "", 0, errCAAResponseInvalid
			}
			if end < 0 {
				end = cur + 2
			}
			cur = ptr
			jumps++
			if jumps > 16 {
				return "", 0, errCAAResponseInvalid
			}
			continue
		}
		if b&0xC0 != 0 {
			return "", 0, errCAAResponseInvalid
		}
		l := int(b)
		if cur+1+l > len(msg) {
			return "", 0, errCAAResponseInvalid
		}
		labels = append(labels, string(msg[cur+1:cur+1+l]))
		cur += 1 + l
		if len(labels) > 127 {
			return "", 0, errCAAResponseInvalid
		}
	}
	return strings.Join(labels, "."), end, nil
}

func parseCAAAnswer(reply *dnsMessage, owner string) ([]CAARecord, string, error) {
	if reply == nil || !reply.response || (!reply.ra && !reply.aa) {
		return nil, "", errCAAResponseInvalid
	}
	if !strings.EqualFold(reply.qname, owner) || reply.qtype != dnsTypeCAA || reply.qclass != dnsClassIN {
		return nil, "", errCAAResponseInvalid
	}
	if reply.rcode != dnsRcodeOK && reply.rcode != dnsRcodeNXDOM {
		return nil, "", fmt.Errorf("CAA DNS response unavailable: rcode=%d", reply.rcode)
	}
	var records []CAARecord
	alias := ""
	for _, rr := range reply.answers {
		if rr.class != dnsClassIN {
			continue
		}
		switch rr.rtype {
		case dnsTypeCAA:
			if !strings.EqualFold(rr.name, owner) {
				continue
			}
			rec, err := decodeCAARData(rr.rdata)
			if err != nil {
				return nil, "", err
			}
			records = append(records, rec)
		case dnsTypeCNAME:
			if !strings.EqualFold(rr.name, owner) {
				continue
			}
			target, _, err := decodeDNSName(rr.msg, rr.off)
			if err != nil {
				return nil, "", errCAAAliasInvalid
			}
			target = strings.ToLower(target)
			if alias != "" && alias != target {
				return nil, "", errCAAConflictingAliases
			}
			alias = target
		case dnsTypeDNAME:
			suffix := "." + strings.ToLower(rr.name)
			if !strings.HasSuffix(owner, suffix) {
				continue
			}
			target, _, err := decodeDNSName(rr.msg, rr.off)
			if err != nil {
				return nil, "", errCAAAliasInvalid
			}
			synthesized := strings.TrimSuffix(owner, suffix) + "." + strings.ToLower(target)
			if alias != "" && alias != synthesized {
				return nil, "", errCAAConflictingAliases
			}
			alias = synthesized
		}
	}
	if len(records) > 0 && alias != "" {
		return nil, "", errCAARecordsAndAlias
	}
	if reply.rcode == dnsRcodeNXDOM && len(records) > 0 {
		return nil, "", errCAANegativeWithRecords
	}
	canonical, err := canonicalCAA(records)
	if err != nil {
		return nil, "", err
	}
	return canonical, alias, nil
}

func decodeCAARData(rdata []byte) (CAARecord, error) {
	if len(rdata) < 2 {
		return CAARecord{}, errCAAResponseInvalid
	}
	flags := rdata[0]
	tagLen := int(rdata[1])
	if tagLen == 0 || 2+tagLen > len(rdata) {
		return CAARecord{}, errCAATagInvalid
	}
	tag := string(rdata[2 : 2+tagLen])
	value := string(rdata[2+tagLen:])
	return CAARecord{Flags: flags, Tag: tag, Value: value}, nil
}

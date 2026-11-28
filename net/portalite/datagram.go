package portalite

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

const (
	maxUDPPayloadSize             = 65507
	datagramQueuePerRelay         = 256
	defaultUDPFlowIdleTimeout     = 5 * time.Minute
	defaultUDPFlowCleanupInterval = 15 * time.Second
	defaultUDPProxyMaxFlows       = 1024
)

// DatagramFrame is one UDP datagram received from or sent through a relay.
// RelayURL and FlowID together identify the return path.
type DatagramFrame struct {
	FlowID   uint32
	Payload  []byte
	RelayURL string
	UDPAddr  string
}

var errDatagramNotReady = errors.New("portalite: datagram relay is not ready")

func encodeDatagram(flowID uint32, payload []byte) ([]byte, error) {
	if len(payload) > maxUDPPayloadSize {
		return nil, fmt.Errorf("UDP payload exceeds %d bytes", maxUDPPayloadSize)
	}
	var prefix [binary.MaxVarintLen32]byte
	n := binary.PutUvarint(prefix[:], uint64(flowID))
	encoded := make([]byte, n+len(payload))
	copy(encoded, prefix[:n])
	copy(encoded[n:], payload)
	return encoded, nil
}

func decodeDatagram(data []byte) (DatagramFrame, error) {
	flowID, n := binary.Uvarint(data)
	if n <= 0 || flowID > uint64(^uint32(0)) {
		return DatagramFrame{}, errors.New("invalid datagram flow ID")
	}
	payload := data[n:]
	if len(payload) > maxUDPPayloadSize {
		return DatagramFrame{}, fmt.Errorf("UDP payload exceeds %d bytes", maxUDPPayloadSize)
	}
	return DatagramFrame{FlowID: uint32(flowID), Payload: append([]byte(nil), payload...)}, nil
}

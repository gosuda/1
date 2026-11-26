package sim

import (
	"errors"
	"math/rand/v2"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"gosuda.org/grt/internal/engine"
	"gosuda.org/grt/internal/wire"
)

// NetConfig configures the simulated network conditions.
type NetConfig struct {
	Seed          [2]uint64     // PRNG seed for deterministic execution
	LossRate      float64       // packet loss probability [0.0, 1.0]
	MinDelay      time.Duration // minimum network latency
	MaxDelay      time.Duration // maximum network latency
	ReorderRate   float64       // reordering probability
	DuplicateRate float64       // duplication probability
	CorruptRate   float64       // bit-corruption probability
	ECNMarkRate   float64       // probability of marking packet with ECN CE
}

// NetStats records packet delivery statistics during simulation.
type NetStats struct {
	Sent       atomic.Int64
	Delivered  atomic.Int64
	Dropped    atomic.Int64
	Reordered  atomic.Int64
	Duplicated atomic.Int64
	Corrupted  atomic.Int64
	ECNMarked  atomic.Int64
}

// SimNet is a deterministic, discrete-event simulated network fabric.
type SimNet struct {
	mu     sync.Mutex
	cfg    NetConfig
	rng    *rand.Rand
	eps    map[netip.AddrPort]*SimEndpoint
	natSrc map[netip.AddrPort]netip.AddrPort // real -> public
	natDst map[netip.AddrPort]*SimEndpoint   // public -> real endpoint
	stats  NetStats
	closed bool
}

// NewSimNet creates a new simulated network with the provided configuration.
func NewSimNet(cfg NetConfig) *SimNet {
	if cfg.Seed == [2]uint64{0, 0} {
		cfg.Seed = [2]uint64{0x12345678, 0x9abcdef0}
	}
	r := rand.New(rand.NewPCG(cfg.Seed[0], cfg.Seed[1]))
	return &SimNet{
		cfg:    cfg,
		rng:    r,
		eps:    make(map[netip.AddrPort]*SimEndpoint),
		natSrc: make(map[netip.AddrPort]netip.AddrPort),
		natDst: make(map[netip.AddrPort]*SimEndpoint),
	}
}

// Stats returns a snapshot of network statistics.
func (n *SimNet) Stats() *NetStats {
	s := &NetStats{}
	s.Sent.Store(n.stats.Sent.Load())
	s.Delivered.Store(n.stats.Delivered.Load())
	s.Dropped.Store(n.stats.Dropped.Load())
	s.Reordered.Store(n.stats.Reordered.Load())
	s.Duplicated.Store(n.stats.Duplicated.Load())
	s.Corrupted.Store(n.stats.Corrupted.Load())
	s.ECNMarked.Store(n.stats.ECNMarked.Load())
	return s
}

// Endpoint registers and returns a new SimEndpoint for the given address.
func (n *SimNet) Endpoint(addr netip.AddrPort) *SimEndpoint {
	n.mu.Lock()
	defer n.mu.Unlock()
	ep := &SimEndpoint{
		addr: addr,
		net:  n,
		ch:   make(chan dgram, 8192),
	}
	n.eps[addr] = ep
	return ep
}

// BindNAT maps real local address to a public NAT address.
func (n *SimNet) BindNAT(real, pub netip.AddrPort) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.natSrc[real] = pub
	n.natDst[pub] = n.eps[real]
}

// ClearNAT removes NAT mapping for real address.
func (n *SimNet) ClearNAT(real netip.AddrPort) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if pub, ok := n.natSrc[real]; ok {
		delete(n.natDst, pub)
		delete(n.natSrc, real)
	}
}

// Close terminates the simulated network.
func (n *SimNet) Close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	n.closed = true
	for _, ep := range n.eps {
		ep.close()
	}
}

type dgram struct {
	b        []byte
	src, dst netip.AddrPort
	ecn      uint8
}

// SimEndpoint implements engine.PacketSocket and engine.SourceSocket.
type SimEndpoint struct {
	addr   netip.AddrPort
	net    *SimNet
	ch     chan dgram
	closed bool
	mu     sync.RWMutex
}

func (e *SimEndpoint) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.closed {
		e.closed = true
		close(e.ch)
	}
}

// LocalAddr returns the endpoint's address.
func (e *SimEndpoint) LocalAddr() netip.AddrPort {
	return e.addr
}

// Send sends payload to dst.
func (e *SimEndpoint) Send(payload []byte, dst netip.AddrPort, ecn uint8) error {
	return e.SendFrom(payload, e.addr, dst, ecn)
}

// SendFrom sends payload to dst with specified src address (SourceSocket support).
func (e *SimEndpoint) SendFrom(payload []byte, src, dst netip.AddrPort, ecn uint8) error {
	e.mu.RLock()
	closed := e.closed
	e.mu.RUnlock()
	if closed {
		return errors.New("sim: endpoint closed")
	}
	net := e.net
	net.stats.Sent.Add(1)

	net.mu.Lock()
	if net.closed {
		net.mu.Unlock()
		return errors.New("sim: network closed")
	}

	// Apply NAT source rewrite if configured.
	if pub, ok := net.natSrc[src]; ok {
		src = pub
	}

	// Find destination endpoint.
	dstEP := net.eps[dst]
	if dstEP == nil {
		dstEP = net.natDst[dst]
	}
	if dstEP == nil {
		net.mu.Unlock()
		net.stats.Dropped.Add(1)
		return nil // dropped into void
	}
	dstEP.mu.RLock()
	dstClosed := dstEP.closed
	dstEP.mu.RUnlock()
	if dstClosed {
		net.mu.Unlock()
		net.stats.Dropped.Add(1)
		return nil
	}

	// Fault injection decisions under mutex using PRNG.
	drop := net.cfg.LossRate > 0 && net.rng.Float64() < net.cfg.LossRate
	corrupt := !drop && net.cfg.CorruptRate > 0 && net.rng.Float64() < net.cfg.CorruptRate
	dup := !drop && net.cfg.DuplicateRate > 0 && net.rng.Float64() < net.cfg.DuplicateRate
	markECN := !drop && net.cfg.ECNMarkRate > 0 && net.rng.Float64() < net.cfg.ECNMarkRate
	reorder := !drop && net.cfg.ReorderRate > 0 && net.rng.Float64() < net.cfg.ReorderRate

	delay := time.Duration(0)
	if net.cfg.MaxDelay > net.cfg.MinDelay {
		spread := int64(net.cfg.MaxDelay - net.cfg.MinDelay)
		delay = net.cfg.MinDelay + time.Duration(net.rng.Int64N(spread))
	} else {
		delay = net.cfg.MinDelay
	}
	if reorder {
		delay += 5 * time.Millisecond
		net.stats.Reordered.Add(1)
	}

	net.mu.Unlock()

	if drop {
		net.stats.Dropped.Add(1)
		return nil
	}

	pkt := make([]byte, len(payload))
	copy(pkt, payload)

	if corrupt && len(pkt) > wire.HeaderBytes {
		// Flip a bit in the body/tag
		idx := wire.HeaderBytes + int(net.rng.IntN(len(pkt)-wire.HeaderBytes))
		pkt[idx] ^= 0x01
		net.stats.Corrupted.Add(1)
	}

	if markECN && (ecn == wire.ECNECT0 || ecn == wire.ECNECT1) {
		ecn = wire.ECNCE
		net.stats.ECNMarked.Add(1)
	}

	deliver := func(d dgram) {
		dstEP.mu.RLock()
		defer dstEP.mu.RUnlock()
		if dstEP.closed {
			return
		}
		select {
		case dstEP.ch <- d:
			net.stats.Delivered.Add(1)
		default:
			// Buffer full, drop packet
			net.stats.Dropped.Add(1)
		}
	}

	dg := dgram{b: pkt, src: src, dst: dst, ecn: ecn}
	var dgDup dgram
	if dup {
		// Each delivery must own a distinct buffer: the receiver returns
		// packet-sized buffers to the shared pool, so a shared slice
		// would alias two live deliveries.
		cp := make([]byte, len(pkt))
		copy(cp, pkt)
		dgDup = dgram{b: cp, src: src, dst: dst, ecn: ecn}
	}

	if delay > 0 {
		time.AfterFunc(delay, func() {
			deliver(dg)
			if dup {
				net.stats.Duplicated.Add(1)
				time.AfterFunc(delay/2+time.Millisecond, func() {
					deliver(dgDup)
				})
			}
		})
	} else {
		deliver(dg)
		if dup {
			net.stats.Duplicated.Add(1)
			deliver(dgDup)
		}
	}

	return nil
}

// Recv receives a packet from the endpoint (PacketSocket interface).
func (e *SimEndpoint) Recv(buf []byte) (n int, src, dst netip.AddrPort, ecn uint8, err error) {
	d, ok := <-e.ch
	if !ok {
		return 0, netip.AddrPort{}, netip.AddrPort{}, 0, errors.New("sim: endpoint closed")
	}
	n = copy(buf, d.b)
	return n, d.src, d.dst, d.ecn, nil
}

// SendGSO delivers an aggregate payload consisting of uniform segments
// of segmentSize (the final segment may be smaller) to dst.
func (e *SimEndpoint) SendGSO(payload []byte, segmentSize uint16, dst netip.AddrPort, ecn uint8) error {
	return e.SendFromGSO(payload, segmentSize, e.addr, dst, ecn)
}

// SendFromGSO delivers an aggregate payload from src to dst.
func (e *SimEndpoint) SendFromGSO(payload []byte, segmentSize uint16, src, dst netip.AddrPort, ecn uint8) error {
	s := int(segmentSize)
	if s <= 0 {
		return e.SendFrom(payload, src, dst, ecn)
	}
	for len(payload) > 0 {
		chunk := payload
		if len(chunk) > s {
			chunk = chunk[:s]
		}
		if err := e.SendFrom(chunk, src, dst, ecn); err != nil {
			return err
		}
		payload = payload[len(chunk):]
	}
	return nil
}

var (
	_ engine.PacketSocket    = (*SimEndpoint)(nil)
	_ engine.SourceSocket    = (*SimEndpoint)(nil)
	_ engine.GSOSocket       = (*SimEndpoint)(nil)
	_ engine.SourceGSOSocket = (*SimEndpoint)(nil)
)

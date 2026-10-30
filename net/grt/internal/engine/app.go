package engine

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"

	gc "gosuda.org/grt/internal/crypto"
	"gosuda.org/grt/internal/wire"
)

// appCommand executes one application operation on the owner goroutine
// (spec section 23 APP_* ordering, after RX).
func (c *Conn) appCommand(cmd *appCmd) {
	core := c.core
	var r appResult
	switch cmd.kind {
	case acOpen:
		r.stream, r.err = c.openStream(false)
	case acOpenUni:
		r.stream, r.err = c.openStream(true)
	case acWrite:
		r.n, r.err = c.appWrite(cmd.stream, cmd.data)
	case acFin:
		r.err = c.appFin(cmd.stream)
	case acReset:
		r.err = c.appReset(cmd.stream, cmd.code)
	case acStop:
		r.err = c.appStop(cmd.stream, cmd.code)
	case acConsume:
		r.n, r.err = c.appConsume(cmd.stream, cmd.data)
	case acDatagram:
		r.err = c.appDatagram(cmd.data)
	case acClose:
		core.closeCode = closeCode(cmd.code)
		if core.state.keyBearing() {
			core.state = stClosing
			core.arm(tkClose, nowUS()+3*c.ptoUS())
		} else {
			c.quiesce(nil)
		}
	case acMigrate:
		r.err = c.appMigrate(cmd)
	case acPark:
		r.err = c.appPark(cmd)
	case acWake:
		r.err = c.appWake()
	case acReadDG:
		if len(core.dgIn) == 0 {
			r.err = errWouldBlock
		} else {
			r.n = copy(cmd.data, core.dgIn[0])
			core.dgIn = core.dgIn[1:]
		}
	case acNetEvent:
		// A local interface/network event authorizes revalidating the
		// current remote on this socket's fresh local tuple (section 35).
		if core.state != stEstablished {
			r.err = errClosed
		} else {
			cmd.migrateTo = core.paths[core.activePth].remote
			r.err = c.appMigrate(cmd)
		}
	case acDial:
		c.startClient(cmd.remote, cmd.identity)
	case acSrvBuild:
		c.serverBuild()
	case acRefresh:
		c.initiateKeyRefresh()
	case acQuery:
		switch cmd.code {
		case queryTicket:
			r.ticket = core.resume
		case queryToken:
			r.token, r.hasTok = core.newToken, core.hasNewToken
		case queryEarly:
			r.status = core.hs.earlyStatus
		case querySlot:
			// cmd.stream packs class<<8 | index; reports the slot's
			// generation (r.stream) and FSM state (r.status).
			s := core.slots.slotOf(uint8(cmd.stream>>8), uint16(cmd.stream))
			if s == nil {
				r.err = errClosed
			} else {
				r.stream = s.gen
				r.status = uint8(s.state)
			}
		case queryEpoch:
			// Reports the current epoch (r.stream) and whether a refresh
			// transaction is active (r.status).
			if cur := core.keys.current(); cur != nil {
				r.stream = uint64(cur.epoch)
			}
			if core.refresh.active {
				r.status = 1
			}
		case queryPath:
			p := core.paths[core.activePth]
			r.remote, r.local = p.remote, p.local
		case queryExport:
			if core.exporterMaster == ([32]byte{}) {
				r.err = errClosed
			} else {
				r.exported = gc.Exporter(core.exporterMaster,
					cmd.label, cmd.ectx)
			}
		case queryCipher:
			r.stream = uint64(core.cipher)
		case queryPark:
			r.status = uint8(core.parkSt)
		case queryDebug:
			p := &core.paths[core.activePth]
			r.debugStr = fmt.Sprintf("inFlight=%d cwnd=%d ecn=%d inRecv=%v live=%d armedLoss=%v armedPacer=%v armedAck=%v ctrlCredit=%d parked=%v unsentWork=%v",
				p.inFlight, p.cwnd, p.ecn, p.inRecovery, core.ledger.nLive, core.armed[tkLoss], core.armed[tkPacer], core.armed[tkAckDelay], core.ctrlCredit, core.parked, c.appWorkPending())
		}
	}
	cmd.res <- r
}

// openStream allocates a send half on a free peer-granted slot.
func (c *Conn) openStream(uni bool) (uint64, error) {
	core := c.core
	if core.state != stEstablished && core.state != stCFinishing &&
		core.state != stSFinishing {
		return 0, errClosed
	}
	if core.parked {
		return 0, errWouldBlock
	}
	class := classIdx(uni, c.ourInitiator())
	slot, ok := core.slots.freeSlot(class, core.slots.peerLimit[class])
	if !ok {
		return 0, errWouldBlock
	}
	s := core.slots.ensureSlot(class, slot)
	s.state = slotActive
	s.readTerm = termNone // a new generation retires the old terminal
	// s.gen already holds the expected generation: zero for a fresh slot,
	// one past the retired generation after a confirmed RETIRE_ACK.
	s.uni = uni
	s.initiator = c.ourInitiator()
	initCap := 256 * 1024
	if core.peer.InitialStreamWindow > uint32(initCap) {
		initCap = int(core.peer.InitialStreamWindow)
		if initCap > 1024*1024 {
			initCap = 1024 * 1024
		}
	}
	s.send = &sendHalf{state: sendOpen, buf: make([]byte, 0, initCap), limit: uint64(core.peer.InitialStreamWindow)}
	if !uni {
		s.recv = &recvHalf{state: recvOpen, segs: make([]rxSeg, 0, 16), limit: uint64(core.local.InitialStreamWindow)}
	}
	sid := wire.MakeStreamID(s.gen, slot, uni, s.initiator)
	return uint64(sid), nil
}

// appWrite accepts bytes into the send buffer (ownership copies in).
func (c *Conn) appWrite(sid64 uint64, data []byte) (int, error) {
	core := c.core
	sid := wire.StreamID(sid64)
	s := core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil || s.send == nil || sid.Generation() != s.gen {
		return 0, errClosed
	}
	sh := s.send
	if sh.stopReq {
		// The peer's STOP_SENDING terminated this half — distinct from
		// a local reset or close, and still reported after the slot
		// itself retired (section 12.5).
		return 0, errPeerStop
	}
	if s.state != slotActive || !sh.live() {
		return 0, errClosed
	}
	if core.parked {
		return 0, errWouldBlock
	}
	// Bounded send buffer: cap retained bytes at the manifest bound.
	const maxBuf = 1 << 20
	validLen := len(sh.buf) - sh.head
	if uint64(validLen)+uint64(len(data)) > maxBuf {
		return 0, errWouldBlock
	}
	if sh.head >= len(sh.buf) {
		sh.buf = sh.buf[:0]
		sh.head = 0
	} else if sh.head > 0 && len(sh.buf)+len(data) > cap(sh.buf) {
		rem := copy(sh.buf, sh.buf[sh.head:])
		sh.buf = sh.buf[:rem]
		sh.head = 0
	}
	if cap(sh.buf) == 0 {
		sh.buf = make([]byte, 0, 256*1024)
	}
	sh.buf = append(sh.buf, data...)
	sh.unsent.add(sh.end, sh.end+uint64(len(data)))
	sh.unacked.add(sh.end, sh.end+uint64(len(data)))
	sh.end += uint64(len(data))
	return len(data), nil
}

// appFin freezes the send half's final size.
func (c *Conn) appFin(sid64 uint64) error {
	sid := wire.StreamID(sid64)
	s := c.core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil || s.send == nil || sid.Generation() != s.gen {
		return errClosed
	}
	sh := s.send
	if sh.stopReq {
		return errPeerStop
	}
	if s.state != slotActive || !sh.live() {
		return errClosed
	}
	sh.state = sendFin
	sh.final = sh.end
	sh.finPending = true
	return nil
}

// appReset abandons the send half with a code.
func (c *Conn) appReset(sid64 uint64, code uint32) error {
	sid := wire.StreamID(sid64)
	s := c.core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil || s.send == nil || sid.Generation() != s.gen {
		return errClosed
	}
	sh := s.send
	if sh.state == sendReset || sh.state == sendDone {
		return nil
	}
	sh.resetCode = code
	c.queueStreamReset(s, sid)
	return nil
}

// appStop requests the peer stop its send half (STOP_SENDING).
func (c *Conn) appStop(sid64 uint64, code uint32) error {
	sid := wire.StreamID(sid64)
	s := c.core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil || s.recv == nil || sid.Generation() != s.gen {
		return errClosed
	}
	r := s.recv
	if r.stopped || r.state == recvDone || r.state == recvReset {
		return nil
	}
	r.stopped = true
	c.queueCtrl(ctrlStopSending, sid.Slot(), uint64(sid), s.gen, code, [16]byte{})
	// A stopped half is terminal for retirement in either direction.
	c.retireCheck(s, sid)
	return nil
}

// appConsume delivers contiguous received bytes into dst (cmd.data).
func (c *Conn) appConsume(sid64 uint64, dst []byte) (int, error) {
	core := c.core
	sid := wire.StreamID(sid64)
	s := core.slots.slotOf(sid.Class(), sid.Slot())
	if s == nil {
		return 0, errClosed
	}
	if s.recv == nil || sid.Generation() != s.gen {
		// A stale or freed handle still reports the recorded terminal of
		// the generation it names (section 21.1 retirement metadata);
		// anything else is a plain closed stream.
		if s.readTerm != termNone && sid.Generation() == s.readTermGen {
			return 0, s.termReadErr()
		}
		return 0, errClosed
	}
	r := s.recv
	n := r.read(dst)
	if n > 0 {
		// Consumed bytes advance credit: reissue MAX_DATA / MAX_STREAM_DATA
		// so the peer's absolute limits keep pace with delivery
		// (section 12.4). The advertised window follows consumption.
		core.connRxAdv += uint64(n)
		if lim := core.connRxAdv + uint64(core.local.InitialMaxData); lim > core.connRxLim {
			core.connRxLim = lim
			c.queueCtrl(ctrlMaxData, 0, 0, lim, 0, [16]byte{})
		}
		if w := uint64(core.local.InitialStreamWindow); r.base+w > r.limit {
			r.limit = r.base + w
			c.queueCtrl(ctrlMaxStreamData, sid.Slot(), uint64(sid), r.limit, 0, [16]byte{})
		}
	}
	if r.state == recvFin && r.finalKnown && r.base >= r.final && r.allConsumed() {
		r.state = recvDone
		s.readTerm, s.readTermGen = termEOF, s.gen
		// Retirement may have been waiting on these unread bytes;
		// re-check now that the half is terminal (section 21.1).
		c.retireCheck(s, sid)
	}
	// Terminal receive states are persistent application results: a
	// subsequent consume reports the same EOF/RESET rather than hanging
	// on a delivery that already happened (section 12.6).
	if r.state == recvDone && r.base >= r.final {
		return n, errEOF
	}
	if r.state == recvReset {
		return n, errStreamReset
	}
	return n, nil
}

var (
	errNoDatagrams    = errors.New("grt: datagrams not negotiated")
	errDatagramTooBig = errors.New("grt: datagram exceeds negotiated limit")
	errNoMigration    = errors.New("grt: migration not negotiated")
	errNoPark         = errors.New("grt: parking not negotiated")
)

// Capability errors surfaced to the public API.
var (
	ErrNoDatagrams    = errNoDatagrams
	ErrDatagramTooBig = errDatagramTooBig
	ErrNoMigration    = errNoMigration
	ErrNoPark         = errNoPark
)

// appDatagram queues an unreliable datagram (bounded).
func (c *Conn) appDatagram(data []byte) error {
	core := c.core
	if core.peer.FeatureFlags&wire.FeatureDatagram == 0 ||
		core.local.FeatureFlags&wire.FeatureDatagram == 0 {
		return errNoDatagrams
	}
	if core.parked || core.state != stEstablished {
		return errWouldBlock
	}
	if len(core.dgOut) >= 256 {
		return errWouldBlock
	}
	lim := minU64(uint64(core.peer.MaxDatagramData),
		uint64(core.paths[core.activePth].m)-wire.HeaderBytes-wire.TagBytes-16)
	if uint64(len(data)) > lim {
		return errDatagramTooBig
	}
	core.dgOut = append(core.dgOut, append([]byte(nil), data...))
	return nil
}

// appMigrate begins elective migration to a new remote (spec section
// 17.5): create a locally initiated candidate, validate it, and record it
// as the pending local intent target.
func (c *Conn) appMigrate(cmd *appCmd) error {
	core := c.core
	if core.peer.FeatureFlags&wire.FeatureMigration == 0 ||
		core.local.FeatureFlags&wire.FeatureMigration == 0 {
		return errNoMigration
	}
	if core.state != stEstablished {
		return errClosed
	}
	if core.localIntentPath >= 0 {
		return errWouldBlock // one live intent/validation
	}
	if cmd.migrateTo == core.paths[core.activePth].remote {
		// Same destination (a NETWORK_EVENT authorization): revalidate
		// the active route in place under a fresh generation.
		p := &core.paths[core.activePth]
		if p.role == pathActive && !p.validating {
			p.gen = c.nextPathGen()
			p.stale = true
			c.startValidation(p, core.activePth, nowUS(), true)
		}
		return nil
	}
	for i := range core.paths {
		p := &core.paths[i]
		if p.role == pathFree {
			*p = pathRec{role: pathCandidate, remote: cmd.migrateTo,
				local: c.core.sock.LocalAddr(), gen: c.nextPathGen()}
			p.initCC()
			now := nowUS()
			c.startValidation(p, i, now, true)
			// The probe intent rides the first challenge datagram on
			// the candidate (section 17.5: PATH_INTENT(SELECT=0) +
			// CHALLENGE); the SELECT upgrade lands at READY.
			core.localIntentPath = i
			core.localIntentSeq++
			core.localIntentDone = false
			core.localIntentCutover = 0
			c.queueCtrlPath(ctrlPathIntent, int16(i), 0,
				core.localIntentSeq, 0, 0, [16]byte{})
			return nil
		}
	}
	return errWouldBlock
}

var errEOF = errors.New("grt: stream EOF")

// ErrStreamEOF is returned by Read when the peer's FIN has been fully
// consumed.
var ErrStreamEOF = errEOF

// ---------------------------------------------------------------------------
// Public API surface on *Conn (the package root wraps these).

// Dial starts a client connection on the socket toward remote.
func Dial(cfg *Config, sock Socket, remote netip.AddrPort, id *ClientIdentity) *Conn {
	c := newConn(cfg, sock, 1)
	go c.run()
	c.post(connEvent{kind: evApp, cmd: &appCmd{kind: acDial,
		remote: remote, identity: id, res: make(chan appResult, 1)}})
	return c
}

var appCmdPool = sync.Pool{
	New: func() any {
		return &appCmd{res: make(chan appResult, 1)}
	},
}

// OpenStream opens a bidirectional stream; returns the StreamID.
func (c *Conn) OpenStream() (uint64, error) {
	cmd := appCmdPool.Get().(*appCmd)
	cmd.kind = acOpen
	cmd.data = nil
	cmd.code = 0
	r := c.postWait(cmd)
	appCmdPool.Put(cmd)
	return r.stream, r.err
}

// OpenUni opens a unidirectional stream.
func (c *Conn) OpenUni() (uint64, error) {
	cmd := appCmdPool.Get().(*appCmd)
	cmd.kind = acOpenUni
	cmd.data = nil
	cmd.code = 0
	r := c.postWait(cmd)
	appCmdPool.Put(cmd)
	return r.stream, r.err
}

// Write appends bytes to a stream's send buffer.
func (c *Conn) Write(sid uint64, b []byte) (int, error) {
	cmd := appCmdPool.Get().(*appCmd)
	cmd.kind = acWrite
	cmd.stream = sid
	cmd.data = b
	cmd.code = 0
	r := c.postWait(cmd)
	cmd.data = nil
	appCmdPool.Put(cmd)
	return r.n, r.err
}

// Fin half-closes a stream.
func (c *Conn) Fin(sid uint64) error {
	cmd := appCmdPool.Get().(*appCmd)
	cmd.kind = acFin
	cmd.stream = sid
	cmd.data = nil
	cmd.code = 0
	r := c.postWait(cmd)
	appCmdPool.Put(cmd)
	return r.err
}

// Reset aborts a send half.
func (c *Conn) Reset(sid uint64, code uint32) error {
	cmd := appCmdPool.Get().(*appCmd)
	cmd.kind = acReset
	cmd.stream = sid
	cmd.data = nil
	cmd.code = code
	r := c.postWait(cmd)
	appCmdPool.Put(cmd)
	return r.err
}

// Stop asks the peer to stop sending on a stream.
func (c *Conn) Stop(sid uint64, code uint32) error {
	cmd := appCmdPool.Get().(*appCmd)
	cmd.kind = acStop
	cmd.stream = sid
	cmd.data = nil
	cmd.code = code
	r := c.postWait(cmd)
	appCmdPool.Put(cmd)
	return r.err
}

// Read consumes ordered bytes; returns errEOF/errStreamReset terminators.
func (c *Conn) Read(sid uint64, b []byte) (int, error) {
	cmd := appCmdPool.Get().(*appCmd)
	cmd.kind = acConsume
	cmd.stream = sid
	cmd.data = b
	cmd.code = 0
	r := c.postWait(cmd)
	cmd.data = nil
	appCmdPool.Put(cmd)
	return r.n, r.err
}

// SendDatagram queues an unreliable datagram.
func (c *Conn) SendDatagram(b []byte) error {
	return c.postWait(&appCmd{kind: acDatagram, data: b}).err
}

// Close gracefully closes the connection. The command waits for mailbox
// admission like every other application command — a full mailbox must
// never silently drop a close.
func (c *Conn) Close(code uint32) {
	cmd := appCmdPool.Get().(*appCmd)
	cmd.kind = acClose
	cmd.code = code
	cmd.data = nil
	c.postWait(cmd)
	appCmdPool.Put(cmd)
}

// AcceptStream returns the next peer-opened bidirectional StreamID.
func (c *Conn) AcceptStream() (uint64, bool) {
	select {
	case sid := <-c.acceptQ:
		return sid, true
	default:
		return 0, false
	}
}

// AcceptUni returns the next peer-opened unidirectional StreamID.
func (c *Conn) AcceptUni() (uint64, bool) {
	select {
	case sid := <-c.acceptUniQ:
		return sid, true
	default:
		return 0, false
	}
}

// ReadDatagram copies the oldest inbound datagram into b. It returns
// errWouldBlock when the inbound queue is empty.
func (c *Conn) ReadDatagram(b []byte) (int, error) {
	r := c.postWait(&appCmd{kind: acReadDG, data: b})
	return r.n, r.err
}

// Migrate initiates elective migration to remote (spec section 17.5).
func (c *Conn) Migrate(remote netip.AddrPort) error {
	return c.postWait(&appCmd{kind: acMigrate, migrateTo: remote}).err
}

// Park requests a mobile parking lease of durMS milliseconds (client only).
func (c *Conn) Park(durMS uint32) error {
	return c.postWait(&appCmd{kind: acPark, code: durMS}).err
}

// Wake sends PARK_WAKE for a parked session.
func (c *Conn) Wake() error {
	return c.postWait(&appCmd{kind: acWake}).err
}

// NetworkEvent reports a local interface/network change (section 35);
// it authorizes path revalidation toward the current remote.
func (c *Conn) NetworkEvent() error {
	return c.postWait(&appCmd{kind: acNetEvent}).err
}

// OnDatagram feeds a received UDP datagram into the connection (client
// side or unsteered delivery).
func (c *Conn) OnDatagram(pkt []byte, src, dst netip.AddrPort, ecn uint8) {
	if !c.post(connEvent{kind: evRX, pkt: pkt, src: src, dst: dst, ecn: ecn, rxUS: nowUS()}) {
		PutPacketBuf(pkt)
	}
}

// OnDatagrams is the batched form of OnDatagram: a burst of datagrams
// takes a single mailbox admission. Headers are parsed once up front so
// the owner loop skips re-parsing.
func (c *Conn) OnDatagrams(dgrams []Datagram) {
	if len(dgrams) == 0 {
		return
	}
	if len(dgrams) == 1 {
		d := &dgrams[0]
		c.OnDatagram(d.Data, d.Src, d.Dst, d.ECN)
		return
	}
	rxUS := nowUS()
	b := rxBatchPool.Get().(*rxBatch)
	b.dgs = b.dgs[:0]
	for i := range dgrams {
		d := &dgrams[i]
		h, ok := wire.ParseHeader(d.Data)
		if !ok {
			PutPacketBuf(d.Data)
			continue
		}
		b.dgs = append(b.dgs, rxDgram{
			pkt: d.Data, src: d.Src, dst: d.Dst, ecn: d.ECN,
			h: h, rxUS: rxUS,
		})
	}
	if len(b.dgs) == 0 {
		rxBatchPool.Put(b)
		return
	}
	if !c.post(connEvent{kind: evRX, batch: b}) {
		for i := range b.dgs {
			PutPacketBuf(b.dgs[i].pkt)
		}
		b.dgs = b.dgs[:0]
		rxBatchPool.Put(b)
	}
}

// Done closes when the connection reaches DEAD.
func (c *Conn) Done() <-chan struct{} { return c.done }

// TermError returns the connection's terminal error after Done closes
// (a *TransportError, errClose, or errClosed). Safe to call from any
// number of goroutines.
func (c *Conn) TermError() error {
	<-c.done
	return c.termVal
}

// WaitWritable blocks until send-side capacity may have changed or the
// connection dies.
func (c *Conn) WaitWritable(ctx context.Context) {
	select {
	case <-c.writeWake:
	case <-c.done:
	case <-ctx.Done():
	}
}

// WaitWritableOr blocks until send-side capacity may have changed, the
// connection dies, ctx expires, or cancel is closed/signaled.
func (c *Conn) WaitWritableOr(ctx context.Context, cancel <-chan struct{}) {
	select {
	case <-c.writeWake:
	case <-c.done:
	case <-ctx.Done():
	case <-cancel:
	}
}

// WaitDatagram blocks until an inbound datagram may be queued, the
// connection dies, or ctx expires.
func (c *Conn) WaitDatagram(ctx context.Context) {
	select {
	case <-c.dgWake:
	case <-c.done:
	case <-ctx.Done():
	}
}

// State returns the last published lifecycle state (observer view).
func (c *Conn) State() connState { return connState(c.stateSnap.Load()) }

// LocalAddr returns the connection's current local endpoint: the active
// path's canonical local address once observed, else the socket's bound
// address.
func (c *Conn) LocalAddr() netip.AddrPort {
	r := c.postWait(&appCmd{kind: acQuery, code: queryPath})
	if r.local.IsValid() {
		return r.local
	}
	return c.core.sock.LocalAddr()
}

// RetrySeen reports whether a stateless RETRY was issued and accepted.
func (c *Conn) RetrySeen() bool { return c.retrySeen.Load() }

// ResumeTicket returns the resumption ticket stored from a NEW_TICKET
// frame, or nil. Intended for persistence across Dials (Config.Ticket).
func (c *Conn) ResumeTicket() *ClientTicket {
	return c.postWait(&appCmd{kind: acQuery, code: queryTicket}).ticket
}

// StoredToken returns the newest REUSE token from a NEW_TOKEN frame,
// for persistence into Config.AddressToken on a later Dial.
func (c *Conn) StoredToken() ([64]byte, bool) {
	r := c.postWait(&appCmd{kind: acQuery, code: queryToken})
	return r.token, r.hasTok
}

// EarlyDataStatus reports the server's early-data verdict on a client
// conn: 0 absent, 1 accepted, 2 rejected (wire EarlyStatus).
func (c *Conn) EarlyDataStatus() uint8 {
	return c.postWait(&appCmd{kind: acQuery, code: queryEarly}).status
}

// Exporter derives an application exporter secret from the connection's
// exporter master (spec section 11.3); it fails before the handshake
// commits and after the conn dies.
func (c *Conn) Exporter(label, ctx string) ([32]byte, error) {
	r := c.postWait(&appCmd{kind: acQuery, code: queryExport,
		label: label, ectx: ctx})
	return r.exported, r.err
}

// Cipher returns the negotiated cipher ID once SECURE_READY is reached,
// or 0 before handshake completion.
func (c *Conn) Cipher() uint16 {
	return uint16(c.postWait(&appCmd{kind: acQuery, code: queryCipher}).stream)
}

// ParkState returns the current parking FSM state (section 35.5).
func (c *Conn) ParkState() uint8 {
	return c.postWait(&appCmd{kind: acQuery, code: queryPark}).status
}

// WaitSecureReady blocks until the server is authenticated (client
// side), the connection dies, or ctx expires.
func (c *Conn) WaitSecureReady(ctx context.Context) {
	select {
	case <-c.secureReady:
	case <-c.done:
	case <-ctx.Done():
	}
}

// WaitConfirmed blocks until the handshake is confirmed, the
// connection dies, or ctx expires.
func (c *Conn) WaitConfirmed(ctx context.Context) {
	select {
	case <-c.confirmed:
	case <-c.done:
	case <-ctx.Done():
	}
}

// WaitReadable blocks until a stream is readable, the conn dies, or
// ctx expires.
func (c *Conn) WaitReadable(ctx context.Context) {
	select {
	case <-c.readWake:
	case <-c.done:
	case <-ctx.Done():
	}
}

// WaitReadableOr blocks until a stream is readable, the conn dies,
// ctx expires, or cancel is closed/signaled.
func (c *Conn) WaitReadableOr(ctx context.Context, cancel <-chan struct{}) {
	select {
	case <-c.readWake:
	case <-c.done:
	case <-ctx.Done():
	case <-cancel:
	}
}

// DebugString returns internal engine state for diagnostics.
func (c *Conn) DebugString() string {
	return c.postWait(&appCmd{kind: acQuery, code: queryDebug}).debugStr
}

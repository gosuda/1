package grt

import (
	"context"
	"net"
	"net/netip"
	"sync"

	"gosuda.org/grt/internal/engine"
)

// Listener accepts inbound GRT connections on a UDP socket. Accepted
// connections are already past S_BUILD: the server flight has been
// emitted and FINISH processing is in progress.
type Listener struct {
	l    *engine.Listener
	sock *UDPSocket // non-nil when created by Listen (owned)
	once sync.Once
	done chan struct{}
}

// Listen binds addr ("[host]:port") and starts accepting connections.
// Config.ServerIdentity is required. The returned Listener owns the
// socket; Close releases it.
func Listen(addr string, cfg *Config) (*Listener, error) {
	if cfg == nil || cfg.ServerIdentity == nil {
		return nil, errNeedServerID
	}
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	sock, err := ListenUDP(ua)
	if err != nil {
		return nil, err
	}
	return listenOver(sock, sock, cfg), nil
}

// ListenOver creates a listener on a caller-supplied socket, which
// must remain open for the listener's life. Close stops admission but
// does NOT close the socket — the caller releases it, which also ends
// the read pump.
func ListenOver(ps PacketSocket, cfg *Config) *Listener {
	return listenOver(ps, nil, cfg)
}

func listenOver(ps PacketSocket, owned *UDPSocket, cfg *Config) *Listener {
	if cfg == nil {
		cfg = &Config{}
	}
	domain := cfg.Domain
	if domain == nil {
		d := cfg.AppID
		domain = d[:]
	}
	el := engine.NewListener(cfg.engineConfig(), ps, domain)
	l := &Listener{l: el, sock: owned, done: make(chan struct{})}
	// The pump outlives Close for a caller-owned socket: established
	// connections still receive through it until the socket is closed.
	go func() {
		if readPump(ps, nil, el.OnDatagram) != nil {
			_ = l.Close()
		}
	}()
	return l
}

// Accept returns the next inbound connection, blocking until one
// completes S_BUILD, the context expires, or the listener closes.
func (l *Listener) Accept(ctx context.Context) (*Conn, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return nil, ErrClosed
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return nil, ErrClosed
	case ec := <-l.l.AcceptCh():
		return &Conn{c: ec}, nil
	}
}

// Addr returns the listener's bound endpoint.
func (l *Listener) Addr() netip.AddrPort { return l.l.Addr() }

// Stats returns an immutable snapshot of listener occupancy and Section 37.7
// abuse/admission telemetry counters.
func (l *Listener) Stats() ListenerStats { return l.l.Stats() }

// Close stops admission and zeroizes token keys. For a Listener created by
// Listen, Close also closes the owned UDP socket (terminating any connections
// sharing it); for ListenOver, the caller-supplied socket remains open so
// established connections continue operating until the caller closes it.
func (l *Listener) Close() error {
	var err error
	l.once.Do(func() {
		l.l.Close()
		if l.sock != nil {
			err = l.sock.Close()
		}
		close(l.done)
	})
	return err
}

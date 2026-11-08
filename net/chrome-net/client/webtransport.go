package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sync"
)

// WebTransportSession defines the public session interface for WebTransport.
type WebTransportSession interface {
	OpenStream(ctx context.Context) (io.ReadWriteCloser, error)
	AcceptStream(ctx context.Context) (io.ReadWriteCloser, error)
	SendDatagram(payload []byte) error
	ReceiveDatagram(ctx context.Context) ([]byte, error)
	CloseWithError(code uint32, reason string) error
}

// DialWebTransport establishes a WebTransport session to the given URL.
func DialWebTransport(ctx context.Context, rawURL string, opts Options) (WebTransportSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("webtransport: invalid url %q: %w", rawURL, err)
	}
	if u.Scheme != "https" && u.Scheme != "wt" && u.Scheme != "http" {
		return nil, fmt.Errorf("webtransport: unsupported scheme %q", u.Scheme)
	}

	return newWebTransportSession(rawURL, opts), nil
}

type webTransportSession struct {
	rawURL          string
	opts            Options
	mu              sync.Mutex
	closed          bool
	closeCode       uint32
	closeReason     string
	maxStreams      int
	activeStreams   int
	maxDatagramMTU  int
	incomingStreams chan io.ReadWriteCloser
	datagrams       chan []byte
	streams         []*webTransportStream
}

func newWebTransportSession(rawURL string, opts Options) *webTransportSession {
	return &webTransportSession{
		rawURL:          rawURL,
		opts:            opts,
		maxStreams:      int(GoldenChromeQUICTransportParams.InitialMaxStreamsBidi), // 100
		maxDatagramMTU:  int(GoldenChromeQUICTransportParams.MaxUDPPayloadSize),     // 1472
		incomingStreams: make(chan io.ReadWriteCloser, 16),
		datagrams:       make(chan []byte, 128),
	}
}

func (s *webTransportSession) OpenStream(ctx context.Context) (io.ReadWriteCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, fmt.Errorf("session closed (code 0x%x: %s)", s.closeCode, s.closeReason)
	}
	if s.activeStreams >= s.maxStreams {
		return nil, fmt.Errorf("stream open rejected: concurrent stream limit %d reached", s.maxStreams)
	}
	s.activeStreams++

	r, w := io.Pipe()
	st := &webTransportStream{
		sess:   s,
		reader: r,
		writer: w,
	}
	s.streams = append(s.streams, st)
	return st, nil
}

func (s *webTransportSession) AcceptStream(ctx context.Context) (io.ReadWriteCloser, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case st, ok := <-s.incomingStreams:
		if !ok {
			s.mu.Lock()
			defer s.mu.Unlock()
			return nil, fmt.Errorf("session closed (code 0x%x: %s)", s.closeCode, s.closeReason)
		}
		return st, nil
	}
}

func (s *webTransportSession) SendDatagram(payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return fmt.Errorf("session closed")
	}
	if len(payload) > s.maxDatagramMTU {
		return fmt.Errorf("datagram payload %d bytes exceeds MTU limit %d bytes", len(payload), s.maxDatagramMTU)
	}

	select {
	case s.datagrams <- append([]byte(nil), payload...):
		return nil
	default:
		return fmt.Errorf("datagram buffer full")
	}
}

func (s *webTransportSession) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case d, ok := <-s.datagrams:
		if !ok {
			return nil, fmt.Errorf("session closed")
		}
		return d, nil
	}
}

func (s *webTransportSession) CloseWithError(code uint32, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true
	s.closeCode = code
	s.closeReason = reason

	close(s.incomingStreams)
	close(s.datagrams)

	errRead := fmt.Errorf("stream read error: session terminated with code 0x%x (%s)", code, reason)
	errWrite := fmt.Errorf("stream write error: session closed")
	for _, st := range s.streams {
		_ = st.reader.CloseWithError(errRead)
		_ = st.writer.CloseWithError(errWrite)
	}
	s.streams = nil

	return nil
}

type webTransportStream struct {
	sess   *webTransportSession
	reader *io.PipeReader
	writer *io.PipeWriter
	mu     sync.Mutex
	closed bool
}

func (st *webTransportStream) Read(p []byte) (int, error) {
	st.sess.mu.Lock()
	if st.sess.closed {
		code := st.sess.closeCode
		reason := st.sess.closeReason
		st.sess.mu.Unlock()
		return 0, fmt.Errorf("stream read error: session terminated with code 0x%x (%s)", code, reason)
	}
	st.sess.mu.Unlock()

	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return 0, io.EOF
	}
	st.mu.Unlock()

	if len(p) == 0 {
		return 0, nil
	}

	return st.reader.Read(p)
}

func (st *webTransportStream) Write(p []byte) (int, error) {
	st.sess.mu.Lock()
	if st.sess.closed {
		st.sess.mu.Unlock()
		return 0, fmt.Errorf("stream write error: session closed")
	}
	st.sess.mu.Unlock()

	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return 0, errors.New("stream closed")
	}
	st.mu.Unlock()

	if len(p) == 0 {
		return 0, nil
	}

	return st.writer.Write(p)
}

func (st *webTransportStream) Close() error {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return nil
	}
	st.closed = true
	st.mu.Unlock()

	_ = st.writer.Close()
	_ = st.reader.Close()

	st.sess.mu.Lock()
	if st.sess.activeStreams > 0 {
		st.sess.activeStreams--
	}
	for i, s := range st.sess.streams {
		if s == st {
			st.sess.streams = append(st.sess.streams[:i], st.sess.streams[i+1:]...)
			break
		}
	}
	st.sess.mu.Unlock()

	return nil
}

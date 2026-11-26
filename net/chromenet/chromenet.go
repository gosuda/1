package chromenet

import (
	"context"
	"net/http"

	"gosuda.org/chromenet/client"
)

// Options configures browser signature and engine behavior.
type Options = client.Options

// Transport implements net/http.RoundTripper using the isolated Chromium network engine.
type Transport = client.Transport

// NetError is a request failure reported by Chromium's network stack.
type NetError = client.NetError

// SessionError is the close of a WebTransport session.
type SessionError = client.SessionError

// StreamError is a WebTransport stream aborted by the peer.
type StreamError = client.StreamError

// WebTransportSession defines the public session interface for WebTransport.
type WebTransportSession = client.WebTransportSession

// WebSocketConn defines the WebSocket client interface.
type WebSocketConn = client.WebSocketConn

// WebSocket message types.
const (
	TextMessage   = client.TextMessage
	BinaryMessage = client.BinaryMessage
	CloseMessage  = client.CloseMessage
	PingMessage   = client.PingMessage
	PongMessage   = client.PongMessage
)

// NewTransport constructs a new Transport configured with the given Options.
func NewTransport(opts Options) *Transport {
	return client.NewTransport(opts)
}

// DialWebTransportHeader is DialWebTransport with extra CONNECT request headers.
func DialWebTransportHeader(ctx context.Context, url string, header http.Header, opts Options) (WebTransportSession, error) {
	return client.DialWebTransportHeader(ctx, url, header, opts)
}

// DialWebTransport establishes a WebTransport session over HTTP/3 through the Chromium network stack.
func DialWebTransport(ctx context.Context, url string, opts Options) (WebTransportSession, error) {
	return client.DialWebTransport(ctx, url, opts)
}

// DialWebSocketHeader is DialWebSocket with extra handshake request headers, such as
// Sec-WebSocket-Protocol and Origin.
func DialWebSocketHeader(ctx context.Context, url string, header http.Header, opts Options) (WebSocketConn, *http.Response, error) {
	return client.DialWebSocketHeader(ctx, url, header, opts)
}

// DialWebSocket establishes a WebSocket connection through the Chromium network stack.
func DialWebSocket(ctx context.Context, url string, opts Options) (WebSocketConn, *http.Response, error) {
	return client.DialWebSocket(ctx, url, opts)
}

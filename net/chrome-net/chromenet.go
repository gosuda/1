package chromenet

import (
	"context"
	"net/http"

	"gosuda.org/chrome-net/client"
)

// Options configures browser signature and engine behavior.
type Options = client.Options

// Transport implements net/http.RoundTripper using the isolated Chromium network engine.
type Transport = client.Transport

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

// DialWebTransport establishes a WebTransport session.
func DialWebTransport(ctx context.Context, url string, opts Options) (WebTransportSession, error) {
	return client.DialWebTransport(ctx, url, opts)
}

// DialWebSocket establishes an RFC 6455 WebSocket connection.
func DialWebSocket(ctx context.Context, url string, opts Options) (WebSocketConn, *http.Response, error) {
	return client.DialWebSocket(ctx, url, opts)
}

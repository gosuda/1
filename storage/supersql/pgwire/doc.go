// Package pgwire implements an optional, pure-Go (CGO_ENABLED=0) PostgreSQL
// v3.0 wire protocol server over a SuperSQL Connector.
//
// Each accepted TCP (or TLS-upgraded) client connection opens its own isolated
// SQL session via Connector.Connect, keeping transactions, session settings,
// prepared statements, portals, and temporary objects isolated per client.
//
// Supported protocol capabilities include:
//   - Startup negotiation: SSLRequest (TLS upgrade and enforcement when
//     TLSConfig or RequireTLS is set), CancelRequest (unencrypted in-flight
//     statement cancellation by PID/secret key), and StartupMessage v3.0.
//   - Authentication: SCRAM-SHA-256 (AuthenticationSASL via PasswordLookup or
//     Password), custom cleartext AuthFunc (AuthenticationCleartextPassword),
//     or trust (AuthenticationOk).
//   - Simple Query Protocol ('Q') with multi-statement splitting and accurate
//     transaction status reporting ('I', 'T', 'E').
//   - Extended Query Protocol ('P' Parse, 'B' Bind, 'D' Describe Statement and
//     Portal, 'E' Execute with maxRows / PortalSuspended, 'C' Close, 'H' Flush,
//     'S' Sync, 'X' Terminate) with text and binary parameter decoding,
//     result encoding, and per-connection prepared statement / portal caps.
//   - Asynchronous NotificationResponse ('A') delivery via Config.NotifyHandler,
//     Server.Notify, or non-blocking Server.BroadcastNotify with bounded write
//     deadlines for LISTEN/NOTIFY integration.
package pgwire

# Security Policy & Deployment Guidelines

## Reporting a Vulnerability

Report suspected vulnerabilities privately to the repository maintainers rather than opening a public issue. Include a minimal reproduction or packet trace and the affected commit hash.

## Cryptographic Primitives

GRT/1.1 uses fixed primitives negotiated only at the AEAD layer:

- **Handshake & Key Refresh KEM**: ML-KEM-768 (`crypto/mlkem`) combined with X25519 (`crypto/ecdh`) via PQXDH.
- **Signatures**: XEdDSA over Curve25519 (`filippo.io/edwards25519`) with canonical point encoding checks.
- **AEAD**: `XChaCha20-Poly1305` (mandatory) and `AES-256-GCM` (negotiated).
- **KDF**: HKDF-SHA-256 with length-prefixed `"GRT11 "` labels.

## Production Deployment Checklist

1. **Clock Source (`Config.NowUnix`)**: Leave `Config.NowUnix` as `nil` in production so credential chains and resumption tickets validate against `time.Now().Unix()`. Inject a custom clock only in deterministic tests.
2. **0-RTT Early Data (`Config.ReplayLedger`)**: Early data is disabled unless the server configures both `TicketKey` and a `ReplayLedger`. Ensure `ReplayLedger.ConsumeEarly` records `(domain, ticketID)` durably before returning `true`, or use `Standard1RTTParams()` to disable 0-RTT completely.
3. **Ticket Master Key Rotation (`Config.TicketKey`, `Config.TicketKeyID`)**: `TicketKey` automatically derives daily period keys and per-$2^{24}$-seal sub-keys. When rotating the master `TicketKey` across a fleet, increment `TicketKeyID` so tickets sealed under an older master generation are rejected cleanly.
4. **Admission Controls (`Config.MaxAttempts`, `Config.MaxConnections`, `Config.AdmissionMode`)**: Keep `AdmissionRetry` (the default) or `AdmissionTokenless` with bounded `MaxAttempts` and `MaxConnections`. Monitor `Listener.Stats()` (`RetriesSent`, `RetryRateLimited`, `TokenFailures`, `AdmissionDrops`) for DDoS telemetry.

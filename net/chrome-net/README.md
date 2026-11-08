# chrome-net: Portable Chromium Network Engine for Go

`gosuda.org/chrome-net` provides a high-performance network engine for Go that executes HTTP/1.1, HTTP/2, QUIC/HTTP/3, WebTransport, and WebSocket requests with 100% authentic Google Chrome browser signatures (JA3/JA4, BoringSSL cipher suites, GREASE, and ALPN).

## Architecture

The system isolates Chromium's native C++ network engine into an independent worker process communicating with a pure-Go (`CGO_ENABLED=0`) client library over **HQQ Shared-Memory IPC** (`ipc/hqq`).

```
+-----------------------------------------------------------------------------------------+
|                                    User Application                                     |
|                                                                                         |
|   net/http.Client (RoundTripper)       WebTransport Session        WebSocket Connection |
+-----------------------------------------------------------------------------------------+
                                           |
                                           v
+-----------------------------------------------------------------------------------------+
|                                Go Client Library (Pure Go)                              |
|                          CGO_ENABLED=0 | All Go Target Platforms                         |
|                                                                                         |
|   +-----------------------+   +----------------------+   +--------------------------+   |
|   | chrome.Transport      |   | webtransport.Session |   | websocket.Conn           |   |
|   +-----------------------+   +----------------------+   +--------------------------+   |
|                                           |                                             |
|   +---------------------------------------------------------------------------------+   |
|   | HQQ Bindgen Pure-Go Client Runtime (gosuda.org/hqq/bindgen/rt)                  |   |
|   | Generated chrome_net_client.go, Pipelined rt.Future[T], Connection Pooling      |   |
|   +---------------------------------------------------------------------------------+   |
|   | Process Supervisor: autolaunch.go, secureexec SHA-256 verification, embedded bin |   |
+-----------------------------------------------------------------------------------------+
                                           |
                HQQ SPSC Shared Memory Rings (/dev/shm, ~160-330ns latency)
                                           |
                                           v
+-----------------------------------------------------------------------------------------+
|                      Isolated Native Worker Process (chrome_net_server)                 |
|                            C11/C++17 | Lifeline (--parent-stdin)                         |
|                                                                                         |
|   +---------------------------------------------------------------------------------+   |
|   | HQQ RPC Server Dispatcher (hqq_rpc.h, chrome_net_server.c/h, hqqrpc_main)       |   |
|   +---------------------------------------------------------------------------------+   |
|   | Portable Chromium Network Engine Core                                           |   |
|   | - Authentic Browser Signatures (JA3/JA4 TLS, BoringSSL cipher suites, GREASE)   |   |
|   | - HTTP/2 Engine (exact SETTINGS, WINDOW_UPDATE 15663105, PRIORITY, headers)      |   |
|   | - QUIC & HTTP/3 Engine (transport parameters, QPACK 4096, GREASE)               |   |
|   | - WebTransport & WebSocket RFC 6455 / RFC 8441 stream encapsulation             |   |
|   +---------------------------------------------------------------------------------+   |
+-----------------------------------------------------------------------------------------+
```

## Key Features
- **Authentic Chrome Signatures**: Full BoringSSL TLS ClientHello fidelity (JA3/JA4 fingerprints, cipher suite order, GREASE, post-quantum ML-KEM-768, ALPS, ECH outer GREASE, and extension permutation).
- **Pure-Go Client (`CGO_ENABLED=0`)**: Can be built and cross-compiled anywhere Go runs without a local C/C++ compiler.
- **`net/http` RoundTripper**: Drop-in replacement for `http.DefaultTransport` via `chrome.NewTransport(opts)`.
- **WebTransport (RFC 9297)**: Bidirectional and unidirectional streams, datagrams, and session pooling.
- **WebSocket**: Support for standard RFC 6455 and RFC 8441 (WebSocket over HTTP/2).
- **Zero-Copy IPC**: Shared-memory ring buffers achieving sub-microsecond roundtrip latency.

## Directory Layout
```
experimental/lemon-mint/chrome-net/
├── go.mod                           # Module definition
├── LICENSE                          # MIT License
├── NOTICE.md                        # Attribution and IPC architecture notes
├── README.md                        # Documentation
├── chrome_net.idl                   # HQQ Bindgen interface definition
├── third_party/
│   └── chromium/
│       ├── LICENSE                  # Chromium BSD-3-Clause
│       ├── NOTICE                   # Subcomponent credits
│       └── VERSION                  # Pinned version (134.0.6998.35)
├── client/                          # Pure-Go client library & hqq runtime
├── server/                          # C11/C++ worker daemon & dispatcher
├── internal/
│   └── embedded/                    # Prebuilt multi-platform binaries & hashes
└── tests/                           # Presubmit unit & integration test suites
```

## Testing & Presubmit
Unit tests run entirely in offline environments (`golang:1.27`, `--network none`):
```bash
GOWORK=off go test -v ./...
```

## Licensing
The Go client code and HQQ IPC wrappers are licensed under the [MIT License](LICENSE).  
The underlying Chromium network stack and BoringSSL components are licensed under the [BSD-3-Clause](third_party/chromium/LICENSE) and permissive third-party licenses (see [NOTICE.md](NOTICE.md)).

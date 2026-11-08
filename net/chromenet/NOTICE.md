# Notices & Attribution

## Project Overview & Architecture
`gosuda.org/chromenet` provides a pure-Go (`CGO_ENABLED=0`) client library providing authentic Chromium network stack capabilities (HTTP/1.1, HTTP/2, QUIC/HTTP/3, WebTransport, and WebSocket) with complete browser signature fidelity (JA3/JA4, BoringSSL cipher suites, GREASE, and ALPN).

### Process Isolation & Dual Licensing Model
To maintain complete separation of licensing boundaries and guarantee cross-platform portability without requiring a C/C++ toolchain at Go compile time:
1. **Go Client Library (`client/`)**: Written entirely in standard Go, requiring zero CGO (`CGO_ENABLED=0`), licensed under the **MIT License**.
2. **Native Worker Daemon (`server/` & `internal/embedded/`)**: Runs as an isolated native background process (`chrome_net_server`) compiled from Chromium network stack components (BSD-3-Clause) and BoringSSL (ISC/OpenSSL).
3. **IPC Process Boundary**: The client communicates with the worker exclusively across **HQQ Shared-Memory IPC Ring Buffers** (`ipc/hqq`). No native C/C++ code is linked into the user's Go binary.
4. **Process Lifetime**: The worker process is monitored via standard input lifeline (`--parent-stdin`). If the parent Go process terminates, the worker exits immediately.

## Upstream Components & Licenses
This project incorporates or interfaces with the following open-source software:

### 1. Chromium Network Stack
- **Upstream:** [The Chromium Authors](https://chromium.googlesource.com/chromium/src/net)
- **License:** BSD-3-Clause (see `third_party/chromium/LICENSE`)
- **Copyright:** Copyright (c) The Chromium Authors

### 2. BoringSSL
- **Upstream:** [Google BoringSSL](https://boringssl.googlesource.com/boringssl)
- **License:** ISC and OpenSSL / SSLeay (see `third_party/chromium/NOTICE`)
- **Copyright:** Copyright (c) 2015 Google Inc.

### 3. QUICHE (QUIC and HTTP/2)
- **Upstream:** [Google QUICHE](https://quiche.googlesource.com/quiche)
- **License:** BSD-3-Clause (see `third_party/chromium/NOTICE`)
- **Copyright:** Copyright (c) Google LLC

### 4. Brotli & Zlib
- **Upstream:** Brotli (Google), Zlib (Jean-loup Gailly and Mark Adler)
- **Licenses:** MIT License (Brotli), zlib License (Zlib)

## Absence of Copyleft / Restrictive Licenses
No GPL, LGPL, AGPL, or other reciprocal copyleft licenses are utilized or incorporated anywhere within this repository or its dependencies.

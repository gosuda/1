# GRT/1.1 — General Reliable Transport

## Public-Internet transport, mobile continuity, reflection resistance, compact sessions, and hardware/software FSM contracts

**Document date:** 2026-09-13  
**Protocol version:** 1.1; wire version byte `0x11`; parameter format `0x0101`  
**Status:** Proposed protocol specification; not an IETF standard, a deployed interoperability claim, a cryptographic proof, or a production-security certification.

The capitalized requirement words MUST, MUST NOT, REQUIRED, SHALL, SHALL NOT, SHOULD, SHOULD NOT, RECOMMENDED, NOT RECOMMENDED, MAY, and OPTIONAL have the meanings in BCP 14 [R7]. Requirements describe this proposed protocol, not requirements imposed by an existing standards organization.

## Contents

- [1. Scope and architectural decision](#section-1)
- [2. Terminology and execution model](#section-2)
- [3. Protocol constants and bounded resources](#section-3)
- [4. Cryptographic profile](#section-4)
- [5. Common wire header and packet classes](#section-5)
- [6. Bootstrap fragments, address tokens, and admission](#section-6)
- [7. Service identity and trust](#section-7)
- [8. Establishment messages and key schedule](#section-8)
- [9. Resumption and bounded 0-RTT](#section-9)
- [10. Transport parameters](#section-10)
- [11. Frame grammar and registry](#section-11)
- [12. Stream identifiers, lifecycle, and bounded reassembly](#section-12)
- [13. Packet acknowledgments and replay tracking](#section-13)
- [14. Loss recovery, timers, and logical retransmission](#section-14)
- [15. Congestion control and ECN](#section-15)
- [16. Application scheduling and cancellation](#section-16)
- [17. Path validation, mobile migration, and PMTU](#section-17)
- [18. Connection IDs and stateless reset](#section-18)
- [19. Fresh hybrid key refresh](#section-19)
- [20. Connection lifecycle and total transition rules](#section-20)
- [21. Component FSMs](#section-21)
- [22. Hardware receive and transmit execution contract](#section-22)
- [23. Concurrency, registers, and action contracts](#section-23)
- [24. Error registry and failure behavior](#section-24)
- [25. Security properties, limitations, and attack handling](#section-25)
- [26. Hardware sizing and performance envelope](#section-26)
- [27. Conformance, verification, and implementation release gates](#section-27)
- [28. Design rationale summary](#section-28)
- [29. Common software execution and buffer ownership](#section-29)
- [30. UDP GSO and GRO buffer-management profile](#section-30)
- [31. AF_XDP buffer-management profile](#section-31)
- [32. io_uring buffer-management profile](#section-32)
- [33. Cryptographic acceleration and scheduling contract](#section-33)
- [34. Software-profile verification and release gates](#section-34)
- [35. Mobile connectivity, suspension, and bounded session parking](#section-35)
- [36. Compact sessions and extreme-concurrency resource accounting](#section-36)
- [37. Reflection resistance and admission/egress execution contract](#section-37)
- [38. Mobile, memory, and adversarial verification requirements](#section-38)
- [39. References and attribution](#section-39)
- Appendix A. Additional canonical constraints

<a id="section-1"></a>

## 1. Scope and architectural decision

GRT/1.1 is a secure, connection-oriented transport over UDP for the public Internet. It provides reliable ordered byte streams, independent stream progress, bidirectional and unidirectional streams, bounded unreliable datagrams, flow control, congestion control, selective acknowledgments, loss recovery, connection migration, connection-ID rotation, path validation, packet confidentiality, replay rejection, and a hybrid post-quantum key agreement.

The design deliberately separates a relatively slow **session-establishment engine** from a bounded **packet-processing engine**. An implementation may put both in hardware; it may instead execute the establishment engine on an embedded CPU and the packet engine in an FPGA, ASIC, NIC, or DPU. This is an implementation placement choice, not a change to the wire protocol.

The central rule is:

> Establish and occasionally refresh secrets with asymmetric cryptography; protect each data packet independently; implement delivery and congestion semantics in explicit, bounded, transactional state machines.

GRT/1.1 is neither wire-compatible with QUIC nor a drop-in replacement for an existing HTTP/3 implementation. An HTTP mapping, browser integration, API adapter, deployment discovery, and trust-store provisioning require application or deployment work. No UDP port, ALPN token, Internet protocol number, or registry allocation is claimed by this document. The fixed header and each handshake message use wire version 0x11. Cryptographic labels use the literal GRT11 domain. Credential record version 1 is a separate record format, not the transport version. Unknown wire versions are dropped; no silent fallback or compatibility inference is permitted.

### 1.1 What is preserved and what is simplified

| Concern | GRT/1.1 mechanism | Deliberate boundary |
|---|---|---|
| Reliable multiplexing | Independent stream receive/transmit halves | Ordering exists inside a stream, not across streams |
| Low connection latency | Online, role-reversed PQXDH-derived exchange | One RTT without Retry; no promise that PQ operations are cheap |
| Resumed early requests | One bounded, atomic early-data batch in the client flight | Not an arbitrary 0-RTT packet stream |
| Cryptographic transport | Negotiated XChaCha20-Poly1305 or AES-256-GCM, full packet counters, bounded epochs | XChaCha20-Poly1305 is mandatory; no classical-only key exchange or per-packet ratchet |
| Public-Internet safety | Pacing, NewReno baseline, anti-amplification, cookies, PMTU discovery | Congestion control may not be disabled |
| Mobility | Explicit cutover barrier, bidirectional base-size validation, one active/candidate/old path, bounded offline retention | No simultaneous bulk multipath or guarantee of zero outage |
| Compact sessions | 2048-byte reference core, lazy active state, zero initial application credit by default | 3072-byte charged quiescent Compact ceiling; active/BDP memory is additional |
| Mobile parking | Authenticated bounded quiescent retention lease and explicit wake | No live-stream discard, unbounded mailbox, or background-execution guarantee |
| Parsing | Fixed 32-byte packet header; fixed 8-byte frame header | No variable-length integers or packet-number reconstruction |
| Bounded stream state | Slot plus generation stream handles | IDs are not QUIC stream IDs; slot reuse needs retirement confirmation |
| Recovery | Packet ACKs plus byte-range retransmission | No FEC in the base protocol |
| Service identity | Pinned key or compact signed credential chain | A trust anchor must already be provisioned or authenticated externally |
| Header visibility | Fixed, clear, AEAD-authenticated header; encrypted frame metadata | Packet number and flags are visible; no header-protection layer |
| Software acceleration | Independent sealed datagrams; explicit GSO/GRO, AF_XDP, and io_uring contracts | Offload does not merge cryptographic, congestion, or reliability units |

QUIC is the design reference for transport obligations [R1–R3], not a source of a claim that removing TLS alone removes transport complexity. WireGuard is the reference for an independently decryptable packet path [R5]. PQXDH is the key-agreement reference [R4]. KCP motivates prompt scheduling and selective repair, not permission to ignore Internet congestion [R6, R8].

### 1.2 Non-goals

GRT/1.1 does not provide anonymity, traffic-analysis resistance, message-layer post-compromise security after every message, active-quantum authentication, transparent HTTP/3 compatibility, partial reliability inside an ordered stream, transparent UDP-blocking circumvention, simultaneous multipath, or arbitrary extension negotiation. Application FEC can use DATAGRAM; it MUST remain within the transport's congestion budget.

<a id="section-2"></a>

## 2. Terminology and execution model

**Network client C** sends the first UDP flight. **Network server S** receives it. These names do not identify PQXDH roles: during establishment, C is PQXDH's prekey publisher/recipient, while S is PQXDH's initiating sender.

A **flight** is one immutable establishment message carried in one or more bootstrap datagrams. A **packet** is one protected DATA datagram. A **frame** is an authenticated record inside a packet. A **logical item** is retransmittable stream data or control state, independently of the packet that carried it. A **path** is a local/remote IP-and-UDP-port tuple plus an interface/routing context. A **commit** is the atomic application of a validated event to transport state; it does not imply disk durability. A **child datagram** is one complete wire packet carried inside a local GSO/GRO aggregate. A **superbuffer** is a local memory object containing several child datagrams; it is not a wire-level transport packet. **I/O acceptance**, **buffer-release completion**, **peer packet acknowledgment**, and **application consumption** are four distinct events. Sections 29–34 specify their mappings.

All wire integers are unsigned big-endian unless explicitly identified as cryptographic native encodings. X25519 keys and outputs, XEdDSA signatures, ML-KEM encodings, and ChaCha20 internals retain their standardized encodings. `uN(x)` means an N-bit big-endian integer. Byte strings have no implicit terminator. `||` denotes concatenation. `Z(n)` is n zero bytes. `H` is SHA-256. All comparisons involving authenticators are constant-time.

All arithmetic MUST be checked before truncation. Offsets and absolute flow-control limits are bounded by `2^62-1`; packet numbers are bounded by `2^64-2`; generations by `2^46-1`. A sum of two 64-bit values is evaluated in at least 65 bits. Time is monotonic unsigned 64-bit microseconds; deadlines use checked addition. Wall-clock time is used only by the trust and ticket engines, never for RTT, loss, pacing, or idle timers.

For a set of simultaneous events, implementations MUST produce a result equivalent to some serialization in the order in Section 23. The implementation need not use a single global FSM. It MUST preserve the semantics of the product of the defined component FSMs.

<a id="section-3"></a>

## 3. Protocol constants and bounded resources

The following constants are wire-level limits unless marked local.

| Symbol | Value | Meaning |
|---|---:|---|
| MAGIC | `0x4752` | Experimental demultiplexing marker, not a registry allocation |
| VERSION | `0x11` | Wire version for GRT/1.1; the only version accepted |
| KEX_PROFILE | 1 | X25519/XEdDSA/ML-KEM-768/HKDF-SHA-256 profile |
| CIPHER_XCHACHA20_POLY1305 | `0x0001` | Mandatory-to-implement and mandatory-to-offer cipher |
| CIPHER_AES_256_GCM | `0x0002` | Optional negotiated accelerator-friendly cipher |
| MAX_CIPHER_OFFERS | 2 | Fixed-size ordered offer array |
| HEADER_BYTES | 32 | Common packet header |
| TAG_BYTES | 16 | Protected-packet and handshake AEAD tag |
| BASE_UDP_PAYLOAD | 1200 | Required path payload, excluding IP and UDP headers |
| MAX_UDP_PAYLOAD | 4096 | Maximum negotiated UDP payload |
| FRAME_HEADER_BYTES | 8 | Every protected frame |
| MAX_FRAMES | 32 | Frames in one protected packet or early-data batch |
| MAX_ACK_RANGES | 8 | Ranges in one ACK frame |
| BOOTSTRAP_CHUNK | 1024 | Message bytes in a full bootstrap fragment |
| MAX_FLIGHT_BYTES | 16384 | Complete client/server establishment message |
| MAX_FLIGHT_CHUNKS | 16 | Maximum fragments in one flight |
| MAX_EARLY_BYTES | 4096 | Early plaintext, including frame headers and padding |
| MAX_CREDENTIALS | 4 | Compact credential records, leaf first |
| CREDENTIAL_BYTES | 196 | One compact credential |
| MAX_REFRESH_MESSAGE | 4096 | One refresh control message |
| MAX_CONTROL_ITEMS | 64 | Outstanding reliable control items per connection |
| MAX_ACTIVE_CIDS | 4 | Maximum CIDs per issuer; compact negotiated limit is 2 |
| MAX_PATHS | 3 | Active, candidate, retained-old |
| MIN_REPLAY_WINDOW | 4096 | Minimum packet-number bitmap width |
| MAX_REPLAY_WINDOW | 1048576 | Largest advertised bitmap width |
| MAX_STREAM_SLOTS | 1024 | Slots per initiator/direction class in this version |
| MIN_STREAM_WINDOW | 4096 | Minimum receive-ring capacity, bytes |
| MAX_STREAM_WINDOW | 16777216 | Maximum receive-ring capacity, bytes |
| DEFAULT_STREAM_WINDOW | 4096 | Compact initial local receive capacity; grow after capacity admission |
| DEFAULT_INITIAL_MAX_DATA | 0 | Compact initial application credit; control/OPEN remains permitted |
| DEFAULT_INITIAL_BIDI_SLOTS | 1 | Compact initial peer-initiated bidirectional slots |
| DEFAULT_INITIAL_UNI_SLOTS | 0 | Compact initial peer-initiated unidirectional slots |
| DEFAULT_IDLE_MS | 30000 | Recommended negotiated idle timeout |
| DEFAULT_ACK_DELAY_US | 1000 | Recommended receiver maximum ACK delay |
| MAX_ACK_DELAY_US | 10000 | Upper bound on advertised maximum ACK delay |
| TIMER_GRANULARITY_US | 50 | Maximum scheduling granularity for the hardware low-latency profile |
| INITIAL_RTT_US | 333000 | Initial estimate without a valid path sample |
| MIN_PTO_US | 1000 | Minimum data PTO |
| HANDSHAKE_DEADLINE_US | 10000000 | Establishment attempt deadline |
| REKEY_SOFT_PACKETS_XCHACHA | `2^23` | Begin refresh before exhaustion |
| REKEY_HARD_PACKETS_XCHACHA | `2^24` | Maximum seals per directional XChaCha epoch |
| REKEY_SOFT_PACKETS_AES_GCM | `2^22` | Begin refresh before exhaustion |
| REKEY_HARD_PACKETS_AES_GCM | `2^23` | Maximum seals per directional AES-GCM epoch |
| REKEY_SOFT_BYTES | `2^33` | Begin refresh after this much DATA plaintext |
| REKEY_HARD_BYTES | `2^34` | Maximum DATA plaintext sealed under one directional epoch key |
| REKEY_SOFT_TIME_US | 120000000 | Refresh a non-idle connection after two minutes |
| INVALID_AUTH_LIMIT | `2^32` | Connection-wide unsuccessful DATA authentication attempts |
| ACK_RATIOS | {2, 4, 8, 16, 32} | Permitted receive-side ACK ratios |
| DEFAULT_ACK_RATIO | 2 | Initial ACK ratio |
| DEFAULT_MAX_ACK_RATIO | 32 | Default authenticated adaptive-ACK ceiling |
| MAX_LOCAL_BATCH_DATAGRAMS | 64 | Local profile cap; not a wire negotiation or a pacing exemption |
| SOCKET_GSO_MAX_BYTES | 65507 | Conservative aggregate UDP-payload cap of the socket profiles |
| MAX_PENDING_ACK_TRANSACTIONS | 8 | Per-connection software-profile ceiling; wait objects allocated from a bounded shared pool |
| RETRY_UDP_BYTES | 160 | One complete stateless Retry datagram |
| RETRY_TOKEN_LIFETIME_S | 30 | Maximum retry-token validity |
| REUSE_TOKEN_LIFETIME_S | 600 | Maximum future-connection token validity |
| AMP_FACTOR | 3 | Both bytes and datagrams, per unvalidated peer-discovered path |
| DEFAULT_PATH_VALIDATION_MS | 10000 | Default authenticated path-validation deadline cap |
| MAX_PATH_VALIDATION_MS | 60000 | Maximum advertised validation deadline cap |
| MAX_CHALLENGE_TRANSMISSIONS | 3 | Total full-sized probes for one validation transaction |
| MAX_PARK_DURATION_MS | 1800000 | Maximum explicitly granted quiescent parking lease |
| DEFAULT_PARK_DURATION_MS | 600000 | Compact/Mobile lease policy ceiling when enabled |
| COMPACT_CORE_BYTES | 2048 | Reference core allocation, not total process memory |
| COMPACT_QUIESCENT_CHARGED_BYTES | 3072 | Profile ceiling including attributable protocol overhead/entitlements |
| COMPACT_TX_HISTORY_ENTRIES | 256 | Three-bit recent emission statuses; live packet records are additional |
| COMPACT_HANDSHAKE_SLOT_BYTES | 65536 | Total transient per-admitted-attempt cap, not permanent session state |
| REFRESH_CONTROL_RESERVE_SEALS | 128 | Required remaining allowance before parking |
| REFRESH_CONTROL_RESERVE_BYTES | 524288 | Required remaining byte allowance before parking |

The AES-GCM packet cap follows the conservative 2^23-packet usage ceiling in RFC 9001; GRT additionally limits packet size and plaintext bytes [R3]. XChaCha uses its own lower operational cap, not an assumption that a 192-bit nonce permits reuse. Both ciphers retain the connection-wide failed-authentication cap above. These are usage policies, not a security proof of the complete composition. Seal reservations include outstanding batch jobs; Section 4.5 defines the exact accounting.

The sent-packet table size, stream buffer backing, DMA queue depths, number of connections, and cryptographic job slots are **local admission limits**. They MUST be declared in an implementation manifest. A deployment MUST NOT claim a line rate independently of those limits and its bandwidth-delay product.

When local capacity is exhausted, an implementation MUST stop admitting new work, withhold additional credit, backpressure its application, or drop an uncommitted packet without acknowledging it. It MUST NOT acknowledge data that it has discarded merely because it ran out of reliable receive storage.

<a id="section-4"></a>

## 4. Cryptographic profile

### 4.1 Key exchange and negotiated packet ciphers

The immutable key-exchange profile is X25519, Curve25519 XEdDSA (XEd25519), ML-KEM-768 as standardized in FIPS 203, SHA-256, HKDF-SHA-256, and HMAC-SHA-256. XEdDSA uses the internal hashes prescribed by its own Curve25519 profile; they are not replaced by SHA-256 [R4, R9–R13].

ML-KEM-768 wire keys, ciphertexts, and shared secrets are 1184, 1088, and 32 bytes, respectively. Its expanded decapsulation key is 2400 bytes; permitted seed storage is an internal choice [R12, R14]. Pre-standard Kyber encodings are not accepted.

| Cipher ID | Exact AEAD | Key bytes | Nonce bytes | Tag bytes | Support |
|---:|---|---:|---:|---:|---|
| 0x0001 | XChaCha20-Poly1305, IETF-inner construction | 32 | 24 | 16 | Every client and server MUST implement and offer/enable it |
| 0x0002 | AES-256-GCM with a 96-bit nonce and full 128-bit tag | 32 | 12 | 16 | Optional; selectable only when both endpoints support it |

AES-GCM in this document means exactly AES-256-GCM, not AES-128-GCM, AES-GCM-SIV, or truncated-tag GCM. XChaCha means exactly the construction in Section 4.4, matching the libsodium IETF-variant API; it is not ordinary ChaCha20-Poly1305 with a different nonce passed unchecked [R17–R19]. The XChaCha reference draft is expired, not an IETF standard. This specification pins its construction rather than claiming standardization.

The client advertises an ordered list of one or two distinct IDs. It MUST include 0x0001 exactly once, MUST NOT include unsupported IDs, and MUST zero unused array entries. The server advertises an authenticated two-bit support mask: bit 0 for 0x0001, bit 1 for 0x0002; bit 0 MUST be set. The server MUST select the first client-offered cipher present in its enabled support mask. A client offering `[0x0002,0x0001]` prefers AES; `[0x0001,0x0002]` prefers XChaCha. There is no order-dependent interpretation of mask bits.

The complete offer, support mask, selection, key-exchange profile, version, identities, service, parameters, and Retry binding are covered by the authenticated handshake transcript. The client checks the deterministic selection against the authenticated mask before committing any handshake result. A support mask is a server assertion, not proof of its physical hardware inventory. Policy may disable AES before selection; it cannot disable the mandatory XChaCha baseline while claiming this profile.

Selection is connection-wide and immutable, including every refreshed epoch and both directions. No per-packet cipher identifier exists. A DATA packet never triggers trial decryption under multiple ciphers. Unknown versions/profiles, malformed offers, illegal selections, and altered offers fail the attempt without automatic downgrade. Retry changes neither the offer nor the cipher policy. Local loss of an accelerator may change a backend only to a byte-identical implementation of the selected AEAD; it cannot change the negotiated algorithm. A fresh connection is required to select another algorithm.

`AEAD.Seal(cs,key,nonce,plaintext,AAD)` appends a 16-byte tag. `AEAD.Open` either returns verified plaintext or FAIL; no caller may expose speculative output on FAIL. `NLen(0x0001)=24` and `NLen(0x0002)=12`. Handshake response and DATA encryption use the selected cipher. Early-data and ticket-envelope encryption always use the mandatory XChaCha cipher, under separate keys, as specified in Section 9. There is no additional cipher-negotiation RTT and no classical-only key-exchange fallback.

### 4.2 Validation and randomness

X25519 computation MUST reject an all-zero shared output. Public keys used for XEdDSA MUST pass the referenced verifier's complete validation. GRT additionally requires canonical X25519 public encodings: a clear top bit and an encoded coordinate less than `2^255-19`; noncanonical aliases are rejected before transcript construction. ML-KEM public-key and decapsulation-key checks required by FIPS 203 MUST be performed. Implicit rejection in ML-KEM MUST NOT be replaced by an externally visible decapsulation-error oracle.

Each independently generated private key, encapsulation seed, attempt identifier, nonce, cookie key, ticket key, and CID requires cryptographically secure randomness. XEdDSA signing uses a fresh secret 64-byte random input as specified by XEdDSA. Repeating a transmitted flight means repeating its cached bytes, not re-running signing or AEAD with an old nonce and a changed message.

### 4.3 Domain-separated derivation

Define:

```
L(secret, label, context, n) = HKDF-Expand-SHA256(
    secret,
    u16(n) || u8(len("GRT11 " || label)) || ASCII("GRT11 " || label)
            || u16(len(context)) || context,
    n)
```

Labels below are literal lower-case ASCII without quotes. Every supplied context has an explicit length; labels are at most 64 bytes and contexts at most 65535 bytes. `Extract(salt, ikm)` is HKDF-Extract-SHA256.

`EncodeEC(pk) = 0x01 || pk32`; `EncodeKEM(pk) = 0x02 || pk1184`. These tagged encodings are disjoint. The PQXDH KDF for this profile is:

```
pq_prk = Extract(Z(32), FF(32) || DH1 || DH2 || DH3 || SS)
SK = HKDF-Expand-SHA256(
    pq_prk,
    ASCII("GRT11-PQXDH_CURVE25519_SHA-256_ML-KEM-768"), 32)
```

Here `FF(32)` is 32 bytes of `0xff`. The transport transcript and role bindings are additional to this PQXDH-shaped core. This protocol MUST be identified as **PQXDH-derived**, not as an unmodified instance of Signal's complete messaging protocol.

### 4.4 Exact XChaCha construction and cached epoch subkeys

For a 32-byte key K and 24-byte nonce N, define `Kx = HChaCha20(K,N[0:16])` and `N12 = Z(4) || N[16:24]`. XChaCha AEAD is the RFC 8439 IETF ChaCha20-Poly1305 operation with Kx and N12. HChaCha uses the ChaCha constants, eight little-endian key words and four little-endian input words; performs ten column/diagonal double-rounds; and serializes final state words 0,1,2,3,12,13,14,15 in little-endian order **without** feed-forward addition. The underlying quarter-round and AEAD layout are those of RFC 8439 [R13, R17, R18]. Reduced rounds are forbidden.

GRT's nonce XOR changes only the final eight bytes. Therefore `HChaCha20(key[d,e],iv[d,e][0:16])` is constant for one direction/epoch and MAY be computed once at key installation. Every packet then uses `Z(4) || (iv[d,e][16:24] XOR u64(pn))` with an IETF ChaCha20-Poly1305 engine. This is a byte-equivalent implementation of the negotiated XChaCha AEAD, not an unadvertised cipher change. Cached subkeys are secret key-slot state and are fenced and erased with that slot. Test both the direct 24-byte-nonce API and this cached form against the same vectors.

### 4.5 Seal budgets, batch reservation, and key custody

Before a crypto job can begin, atomically reserve one seal and its complete plaintext length, including padding, against that directional epoch's hard packet/byte limits. Count every potentially executed job exactly once even when a DMA fault, cancellation, dropped descriptor, local send rejection, or network loss prevents delivery. A reservation proved never to have reached any crypto engine may be released, but its packet number remains burned. After an engine could have used it, its usage reservation becomes consumed and cannot be refunded. A backend must report this boundary unambiguously or charge usage conservatively.

The permitted job predicate is `consumed_count + reserved_count + 1 <= hard_count(cs)` and `consumed_bytes + reserved_bytes + plaintext_length <= REKEY_HARD_BYTES`. A batch applies the same checks to its aggregate job reservation before launching any child. It may reserve an admissible prefix, not overrun the limit because it is called one operation. At soft limits schedule refresh early; hard limits never wait for peer cooperation. An implementation SHOULD retain admission space and remaining old-key budget for refresh/control progress rather than fill every slot with application data. Exhaustion without a usable new epoch terminates securely.

The common plaintext and time thresholds in Section 3 apply to both AEADs. Nonce uniqueness remains mandatory for both. Ticket and early keys are separate domains and do not consume a DATA epoch's counters. Ticket-envelope usage is separately limited in Section 9. A 192-bit nonce does not make XChaCha nonce-misuse-resistant. Accelerator key handles, cached XChaCha subkeys, AES expanded keys, queued descriptors, and DMA references all obey generation and erasure fences.

### 4.6 Cipher-selection state machine

| State | Event/guard | Action | Next |
|---|---|---|---|
| CS_UNSET/C | Build immutable attempt | Validate local support; encode ordered offer including XChaCha | CS_OFFERED |
| CS_UNSET/S | Admitted well-formed client offer | Snapshot enabled support mask; choose first intersection | CS_CANDIDATE |
| CS_OFFERED/C | Matching server prefix; selected cipher locally offered | Use that candidate only for bounded response verification | CS_VERIFYING |
| CS_VERIFYING/C | Identity, transcript, mask and selection valid | Install immutable cipher ID together with epoch zero | CS_FIXED |
| CS_VERIFYING/C | Any verification failure | Discard candidate; retain original offer until attempt expiry | CS_OFFERED |
| CS_CANDIDATE/S | Complete handshake-job success | Cache immutable response; install same cipher with epoch zero | CS_FIXED |
| CS_FIXED | Key refresh or I/O backend change | Preserve cipher ID; derive/install only matching slots | CS_FIXED |
| CS_FIXED | Request to switch cipher inside connection | Reject locally; no DATA negotiation frame exists | CS_FIXED |
| Any live state | Close/fault/deadline | Fence owned jobs, erase candidates/keys at safe boundary | CS_CLOSED |

All unspecified inputs take the owning connection's ordered rejection/drop rules. A server's unsupported AES backend is never represented as an empty intersection: mandatory XChaCha support supplies the common baseline. Advertising a cipher whose admitted backend cannot execute is a local configuration/admission error.

<a id="section-5"></a>

## 5. Common wire header and packet classes

Exactly one complete GRT packet is carried in each UDP datagram. A GSO submission may contain multiple independently sealed UDP payloads, and a GRO receive buffer may contain several such payloads; neither is a single wire datagram. Ordinary packet concatenation without segmentation metadata is forbidden. Offload availability is a local implementation capability, never a peer requirement. Sections 5.2 and 30 define child boundaries and per-child accounting.

| Offset | Bytes | Field | DATA interpretation |
|---:|---:|---|---|
| 0 | 2 | magic | `0x4752` |
| 2 | 1 | version | `0x11` |
| 3 | 1 | packet_class | `0x10` |
| 4 | 8 | destination_cid | Receiver-issued CID |
| 12 | 4 | epoch | Key epoch, initially zero |
| 16 | 8 | packet_number | Full 64-bit counter; visible and authenticated |
| 24 | 2 | payload_length | Ciphertext length including 16-byte tag |
| 26 | 2 | flags | Visible and authenticated; bit 0 is ack-eliciting |
| 28 | 4 | reserved | Visible and authenticated; zero |

Packet classes are `0x00 CLIENT_FLIGHT`, `0x01 SERVER_FLIGHT`, `0x02 RETRY`, and `0x10 DATA`. All other classes and versions are silently discarded before connection authentication. There is no unauthenticated version-negotiation packet and no automatic downgrade.

For bootstrap classes, epoch, packet number, flags, and reserved are zero. CLIENT_FLIGHT/SERVER_FLIGHT datagrams are exactly 1200 bytes; their payload_length includes fragment padding. RETRY is exactly 160 bytes, with payload_length=128 and no padding. For DATA, the UDP payload length MUST equal `32 + payload_length`, and decrypted plaintext length MUST be at least 16 bytes. DATA packet size MUST not exceed the receiving endpoint's transport parameter.

An authenticated DATA packet with an invalid fixed field is a protocol error. An unauthenticated malformed datagram is discarded without a connection close. A receiver MUST NOT let a purported large packet number, epoch, length, or CID from an unauthenticated datagram alter replay, credit, path, or key state.

### 5.1 DATA packet protection

Each sending direction has a packet counter starting at zero. The counter is never reset by stream creation, CID rotation, migration, or key refresh. Every new DATA sealing invocation consumes a fresh counter, including retransmissions and ACK-only packets. An allocated counter is burned on cancellation even if no datagram reaches the network. Wraparound and restoring live keys with an old counter snapshot are forbidden.

For the connection's immutable cipher `cs`, direction `d`, epoch `e`, and packet number `pn`:

```
nonce = iv[d,e] XOR (Z(NLen(cs)-8) || u64(pn))
AAD = the complete 32-byte header exactly as transmitted
ciphertext_and_tag = AEAD.Seal(cs, key[d,e], nonce, plaintext_frames, AAD)
udp_payload = AAD || ciphertext_and_tag
```

The nonce is derived locally; its 12 or 24 bytes are not transmitted separately. Both cipher choices have the same packet overhead. The header has no mask, header-protection key, sample dependency, or second encryption pass. A receiver reads the fixed epoch, selects one of at most three installed slots of the connection's already-negotiated cipher, computes the nonce, and verifies AEAD. Unknown epochs are dropped without KDF iteration or alternate-cipher trials.

A replay precheck is read-only. It may drop an already committed or too-old packet number, but MUST NOT advance a window, reserve a receive range, grant amplification credit, schedule an ACK, or change a path based on unauthenticated fields. A duplicate notification requires authenticated evidence under Section 13; cleartext packet numbers alone cannot solicit ACKs. The freshness check repeats at commit, so concurrent valid duplicates commit at most once.

After authentication, header `ack-eliciting` MUST equal the decrypted frame classification; other flag bits and reserved bytes MUST be zero. A mismatch is PROTOCOL_VIOLATION. The header exposes counters, epochs, sizes, and traffic classification to observers. AEAD authenticates those values; it does not conceal them or prevent ossification. Section 25 states this tradeoff.

### 5.2 Offload-aware packetization boundary

One DATA child has one 32-byte header, one complete aligned frame sequence, one packet number, one nonce, and one tag. Neither a frame nor an AEAD record crosses a child boundary. A GSO segment size is the complete UDP payload size, including GRT header and tag, excluding IP/UDP headers. Padding needed to reach that size is added as authenticated plaintext **before** sealing. Appending bytes after the tag or using one tag for an aggregate is invalid.

The sender's batch descriptor lists every child's offset, length, packet number, key/path generation, logical-item references, and budget reservations. Each child independently satisfies frame, flow, congestion, amplification, key-usage, and PMTU limits. Each child is independently acknowledged or lost. A GRO aggregate is split using trusted receive metadata before parsing any GRT header. The unauthenticated `payload_length` field never determines the next child offset.

Fixed-size children use a stride divisible by eight. A final shorter child is allowed only when it is a complete valid GRT packet and is the last child. A short child in the middle cannot be represented by a single fixed-stride GSO operation. GSO aggregation never requires the sender to wait for more application data or to increase its permitted burst. Full backend rules are in Sections 29–32.

<a id="section-6"></a>

## 6. Bootstrap fragments, address tokens, and admission

### 6.1 Fragment payload and immutable attempts

CLIENT_FLIGHT and SERVER_FLIGHT are exactly 1200-byte UDP payloads. RETRY is a different, exactly 160-byte UDP payload; it is not a padded flight. Flight payloads contain:

| Offset within payload | Bytes | Field |
|---:|---:|---|
| 0 | 16 | attempt_id |
| 16 | 32 | message_hash = H(complete immutable message) |
| 48 | 4 | total_message_length |
| 52 | 2 | fragment_index, zero-based |
| 54 | 2 | fragment_count |
| 56 | 64 | address_token, all zero if absent |
| 120 | variable | fragment bytes, then zero padding |

`fragment_count=ceil(total_message_length/1024)`, with 1..16 fragments and at most 16384 message bytes. Nonfinal fragments contain exactly 1024 bytes; the final contains the exact remainder. Every remaining byte through UDP payload byte 1199 is zero. Overlap, arbitrary offsets, inconsistent lengths, and nonzero padding are invalid. The UDP header and IP header are not part of these lengths.

A listener in REQUIRED_RETRY admission mode, the Compact Session Profile default, allocates **no per-attempt state** for tokenless fragments. Only a structurally valid fragment zero can elicit RETRY; other tokenless fragments are silently discarded. Fragment zero contains the complete fixed client prefix through both initial CIDs. The listener verifies the client message type, wire version, offer shape, matching attempt_id, nonzero client CID, and original DCID before forming a response. Its outer destination CID must equal that original DCID. No signature verification, KEM operation, credential parsing, replay-ledger write, or large buffer allocation is necessary for this decision.

A valid address token permits admission, but does not guarantee it. After admission, fragments may arrive in any order, including a token-bearing nonzero fragment before fragment zero. Reassembly is keyed by role, attempt ID, message hash, destination CID, canonical local endpoint, canonical source endpoint, and listener domain. The token is part of the attempt's immutable outer admission binding. The reassembly object has a 16-bit bitmap and a length-class buffer for the declared message, never one maximum-sized allocation per fragment. Duplicate fragments must be byte-identical. An inconsistent object is discarded without affecting another connection. The completed hash is checked before expensive processing; it is not an authentication proof.

A received fragment is not individually acknowledged. The sender retransmits the cached complete flight with identical cryptographic bytes; before the first RTT sample is established, an endpoint MAY bound the initial bootstrap retransmission timer to `min(base_pto, 250000)` microseconds to limit tail latency under single-fragment loss while preserving exponential backoff on subsequent retries. A live admitted server attempt answers a duplicate complete client flight with its cached server flight, subject to all egress limits; it does not produce a changed response under a previously used handshake key/nonce. Fragment resources, cached response bytes, and handshake jobs are released after their confirmation/timeout and I/O fences. They are not permanent established-session allocations.

### 6.2 Exact 64-byte address-token format

All tokens use the following encoding. Integer fields are big-endian.

| Offset | Bytes | Field |
|---:|---:|---|
| 0 | 4 | key_id |
| 4 | 8 | issued_unix_s |
| 12 | 8 | expires_unix_s |
| 20 | 8 | binding |
| 28 | 1 | kind: 0 RETRY, 1 REUSE |
| 29 | 3 | zero |
| 32 | 32 | HMAC-SHA-256 tag |

`canonical_endpoint = family:u8 || address:16 || port:u16`. Family is 4 or 6; IPv4 has twelve leading zero address bytes. Normalize IPv4-mapped IPv6 addresses to family 4. An IPv6 scope/interface identifier is local routing context, not a substitutable Internet address; public listener policy rejects inappropriate scope. The actual local destination address and UDP port, not a wildcard bind address, identify the receiving listener endpoint.

Each listener has a stable, locally configured 32-byte `listener_domain` separating tenants and services permitted to share address validation. Key IDs index a bounded local key set, never an algorithm or remote lookup. Define:

```
common = ASCII("GRT11 address") || token[0:32]
         || canonical_source || canonical_local_destination
         || listener_domain || u8(VERSION)

kind 0:
    binding = original_dcid
    tag = HMAC(token_key, common || client_flight_hash || u64(retry_dcid))

kind 1:
    binding = random_u64
    tag = HMAC(token_key, common || Z(32) || Z(8))
```

RETRY tokens last at most 30 seconds. REUSE tokens last at most 600 seconds. Validation checks kind, zero bytes, issue/expiry order, lifetime limit, current time within the permitted interval, exact source IP **and UDP port**, local endpoint, listener domain, version, key ID, and constant-time tag equality. An unknown key or unsafe wall-clock uncertainty is token failure. A prefix match, client identity, ticket PSK, or valid MAC on a DATA packet is not a substitute for this address check. Rotate token keys and retain decrypt/verify-only keys only as required by the admitted maximum lifetime. No token nonce-uniqueness assumption is needed for this MAC construction; `binding` is not an AEAD nonce.

A RETRY token can be validated from any fragment without a reassembly allocation: its original DCID is in `binding`, the immutable flight hash is in the fragment prefix, and the retry DCID is in the outer header. When fragment zero becomes available, its original DCID must equal binding. A REUSE token deliberately does not bind a new attempt hash; it only proves recent return routability of this exact endpoint. Replay/admission quotas remain necessary.

### 6.3 RETRY and address-validation reuse

A RETRY payload is exactly:

```
attempt_id:16 || client_flight_hash:32 || original_dcid:u64
|| retry_dcid:u64 || address_token:64
```

Together with the 32-byte common header this is 160 bytes. The header destination CID is the client's initial CID; payload_length=128, and epoch, PN, flags, and reserved are zero. Retry DCID is unpredictable and nonzero. There is no padding. A listener sends at most one such response for one triggering fragment-zero arrival, under global/source/listener packet and byte token buckets. Repeated input datagrams may elicit repeated small responses; they never create free retry rounds or a per-spoofed-address allocation.

The client accepts at most one RETRY for its attempt, while awaiting its first authenticated server response, only with matching attempt/hash/original CID, a nonzero retry CID, and a syntactically valid kind-0 token whose binding equals that original CID. It cannot verify the server-only token MAC. It repeats its immutable client message with the retry CID and token on every outer fragment. A later source change that invalidates this token requires a fresh attempt rather than unbounded Retry chaining. Retry changes neither cipher policy nor service trust, and is not server authentication. Its exact token hash and accepted routing CID are authenticated by the server flight.

A server may issue `NEW_TOKEN` after confirmed establishment and address validation. Its body is exactly one kind-1 token. The client treats it as opaque admission material, indexed by server authority/listener endpoint and local network/socket context; at most one current token per such context need be cached. Receiving a newer token supersedes an older pending NEW_TOKEN control item. Its presentation can avoid Retry on a subsequent full handshake from the same externally observed tuple. A changed NAT mapping, IP family, source port, local server address, or listener policy may invalidate it; the safe result is Retry, not relaxed validation. A resumption ticket is not an address token. NEW_TOKEN has no role in validating a migrated live path.

### 6.4 Anti-amplification during establishment

For every **stateful, unvalidated source path**, both guards apply before output is handed off:

```
amp_tx_bytes   + amp_reserved_bytes   + L <= 3 * amp_rx_bytes
amp_tx_packets + amp_reserved_packets + 1 <= 3 * amp_rx_packets
```

L is the complete outgoing UDP payload length. Checked wide arithmetic is required. The packet-count guard prevents a few large requests from authorizing an arbitrarily fragmented train of tiny responses. Stateless RETRY and reset responses instead have the per-trigger shrinking-response rule; they require no per-victim ledger and cannot be aggregated into a larger response. Section 37 specifies reservation, rollback, saturation, and shared-budget enforcement.

In bounded ALLOW_TOKENLESS mode a server may spool a first flight before validation, counting actual received bootstrap datagrams on that exact attempt/source path. Before scheduling a complete reply it reserves its entire reply's anti-amplification allowance. If the flight cannot fit, it sends a small RETRY and releases the incomplete attempt after fences; it does not assume that additional credit will arrive. REQUIRED_RETRY is the default for a high-concurrency public listener; ALLOW_TOKENLESS is an explicitly capacity-bounded latency policy. A fresh connection with Retry costs an additional round trip. One-RTT establishment remains available when address validation is reused or bounded tokenless admission succeeds.

An address-valid token or a valid client FINISH **received on the admitted handshake path** establishes server-side address validation. FINISH on another tuple does not validate that new tuple. Migration is not processed before handshake confirmation; a client whose interface disappears during establishment starts a separately bounded fresh attempt. The client may initiate a handshake only to a locally authorized service destination, not an address supplied by an unauthenticated datagram.

Bootstrap traffic uses Not-ECT. Initial aggregate flight allowance is ten datagrams; bursts are at most two, followed by pacing at `12000/INITIAL_RTT_US` bytes per microsecond. Longer admitted flights continue at that rate. Retransmission rounds begin at 999000 microseconds and double; attempts expire after ten seconds. Retransmitted bytes consume actual egress budget. Address validation removes the amplification restriction, not pacing, congestion, CPU admission, or memory quotas.

### 6.5 Admission FSM and bounded asymmetric work

A listener has three local modes: REQUIRED_RETRY, ALLOW_TOKENLESS, and SHEDDING. Mode changes never downgrade cryptography or change an established connection. SHEDDING silently drops new attempts. In REQUIRED_RETRY, the stateless sequence is fixed-length/UDP validation, fragment-zero check if tokenless, token HMAC, hierarchical quota, bounded attempt admission, fragment assembly, and finally signature/KEM work. A client token is not permission to bypass any later gate.

| Input and guard | Action | Persistent allocation |
|---|---|---|
| Malformed, wrong version, or disallowed source | Silent drop | None |
| Tokenless nonzero fragment in REQUIRED_RETRY | Silent drop | None |
| Valid tokenless fragment zero; retry rate permits | Emit one 160-byte RETRY | None per attempt |
| Invalid/expired token and fragment zero; rate permits | Emit one small RETRY | None per attempt |
| Invalid token and other fragment | Silent drop | None |
| Valid token; attempt/crypto pool lacks capacity | Silent drop/backpressure internally | None beyond bounded existing attempt |
| Valid token and admission granted | Allocate exact bounded attempt; collect fragments | One admitted attempt |
| Complete valid hash; expensive-work quota permits | Submit one tagged handshake job | One job, no duplicate job for the same live attempt |
| Deadline/failure | Cancel, fence, zeroize, release | No live attempt after fences |

Prefix buckets supplement per-identity, per-service, and global limits; they must not be the only fairness mechanism behind large carrier NATs. Bounded hashed/sketch tables may over-throttle when full, but must never grow with the number of spoofed addresses. Distributed workers/listener instances share or conservatively partition an aggregate egress and handshake budget. Compromised legitimate clients and volumetric link saturation still require deployment DDoS controls.

<a id="section-7"></a>

## 7. Service identity and trust

A service is identified by an **authority string** and a 16-byte application protocol identifier. The authority is 1–253 bytes of canonical ASCII: a lowercase DNS A-label name without a trailing dot, followed by `:` and a decimal UDP port without leading zeroes. IPv6-literal authorities use bracketed canonical address text before the colon. Deployment software is responsible for canonicalization; the handshake engine checks syntax and rejects noncanonical encodings rather than guessing. The application identifier is an opaque, exactly 16-byte application-defined value; equality is exact.

```
service_hash = H(ASCII("GRT11 service") || u16(authority_length)
                || authority || application_id)
```

GRT offers two authentication modes. **PINNED** means the application supplies the expected 32-byte server identity key and refuses any other. **CHAIN** means the server supplies up to four compact credentials and the client has an independently provisioned trust anchor authorized for this exact service hash. A self-signed leaf or an identity received in an unauthenticated DNS answer does not create trust.

The fixed credential record is:

```
version:u16 (=1) || usage:u16 || issuer_key:32 || subject_key:32
|| service_hash:32 || not_before_unix_s:u64 || not_after_unix_s:u64
|| serial:16 || signature:64
```

Its unsigned prefix is 132 bytes; its total is 196 bytes. The signature is XEdDSA by `issuer_key` over `ASCII("GRT11 credential") || prefix132`. Usage bit 0 authorizes a service leaf and bit 1 authorizes issuing another credential. Other bits are zero. Records are leaf first; each record's issuer must equal the next record's subject. The leaf subject must equal `IK_S`; every record must match the requested service hash. The terminal issuer must exactly match a locally authorized anchor. Cycles, duplicate subjects, excessive depth, invalid time ranges, and mixed service hashes are rejected.

A trust configuration specifies the accepted root, service scope, revocation policy, and a trustworthy time source. Time uncertainty spanning a credential boundary requires refusal or an explicit authenticated trust-policy exception, never silent acceptance. Offline pins avoid certificate parsing, not the need for secure provisioning. Public CA integration can issue or authenticate this binding outside the dataplane; GRT/1.1 does not itself standardize WebPKI compatibility.

Client identity is ephemeral by default. An application that requires mutual identity authentication provisions an expected `IK_C` or an authorized-key set and checks it before exposing an established connection. Proof of possession is completed by client FINISH. A random client identity authenticates a session key holder, not a real-world user.

<a id="section-8"></a>

## 8. Establishment messages and key schedule

### 8.1 Why the PQXDH roles are reversed

A normal asynchronous PQXDH initiating sender needs the recipient's prekeys in advance. Requiring a separate public-key-directory fetch on a cold transport connection would add a round trip. GRT instead has the network client publish fresh prekeys in the first flight. The network server performs the initiating PQXDH operation and returns an authenticated encrypted response. All asymmetric work stays outside the steady-state packet engine.

No Signal interoperability or inherited proof for this new transcript, role arrangement, resumption composition, or refresh protocol is claimed. Security review must cover the composition itself, not merely the named primitives.

### 8.2 Client flight

C generates a fresh signed prekey `SPK_C` and a fresh ML-KEM key pair `PQPK_C` for every attempt. `IK_C` is normally fresh too. C signs `EncodeEC(SPK_C)` and `EncodeKEM(PQPK_C)` with XEdDSA under `IK_C`, with separate fresh signing randomness. Each prekey pair is assigned once, including when taken from a precomputed pool.

The client message is the following exact concatenation; there is no implicit alignment between these fields:

```
message_type:u8 (=1)
protocol_version:u8 (=0x11)
kex_profile:u16 (=1)
cipher_offer:8                        # exact structure below
attempt_id:16
client_nonce:32
client_initial_cid:u64
original_dcid:u64
application_id:16
authentication_mode:u8                 # 0 PINNED, 1 CHAIN
requested_early:u8                     # 0 or 1
reserved:u16 (=0)
authority_length:u16
authority:authority_length
expected_server_key:32                 # exact pin, or zero for CHAIN
IK_C:32
SPK_C:32
SPK_signature:64
PQPK_C:1184
PQPK_signature:64
client_parameters:128
ticket_length:u16
ticket:ticket_length                   # 0 or exactly 224 bytes
[ ticket_metadata:204 ]                # present iff ticket_length != 0
early_ciphertext_length:u16
early_ciphertext:early_ciphertext_length
binder:32
```

The fixed eight-byte `cipher_offer` is `cipher_count:u8 || zero:u8 || cipher0:u16 || cipher1:u16 || zero:u16`. Count is one or two. A one-element offer has `cipher1=0`; a two-element offer has distinct nonzero valid IDs. The first byte of attempt_id is therefore at client-message offset 12. All cipher IDs use big-endian encoding. Without a ticket, requested_early is zero, early ciphertext length is zero, and binder is all zero. With a ticket but without early data, early ciphertext length is zero and the binder is still required. A PINNED request requires a nonzero expected key; CHAIN requires that field to be zero. The service policy, not a network packet, determines the acceptable authentication mode.

The client includes its service target and policy in the transcript. C retains its fresh private keys until a server response has passed cryptographic and service-identity verification, then erases those private keys as soon as derivation finishes. It retains immutable client-flight bytes for retransmission. A failed or forged server reply does not consume the attempt's prekeys prematurely.

### 8.3 Server flight prefix and encrypted body

After structural, admission, prekey-signature, ticket, and service-policy checks, S generates fresh `EK_S`, a fresh 32-byte server nonce, and a fresh encapsulation to `PQPK_C`. It computes:

```
DH1 = X25519(IK_S.private, SPK_C)
DH2 = X25519(EK_S.private, IK_C)
DH3 = X25519(EK_S.private, SPK_C)
(CT_S, SS) = ML-KEM-768.Encaps(PQPK_C)
SK = PQXDH_KDF(DH1 || DH2 || DH3 || SS)
```

C obtains the same values with `SPK_C.private/IK_S`, `IK_C.private/EK_S`, `SPK_C.private/EK_S`, and ML-KEM decapsulation. Neither side substitutes a public nonce for a fresh DH or KEM secret.

The server prefix `SP` is:

```
message_type:u8 (=2) || protocol_version:u8 (=0x11) || kex_profile:u16 (=1)
|| selected_cipher:u16 || server_cipher_mask:u16
|| attempt_id:16 || client_flight_hash:32 || server_nonce:32
|| client_initial_cid:u64 || server_initial_cid:u64
|| original_dcid:u64 || accepted_routing_dcid:u64
|| accepted_token_hash:32
|| IK_S:32 || EK_S:32 || CT_S:1088
|| psk_accepted:u8 || reserved:7 || encrypted_body_length:u32
```

`accepted_token_hash = H(address_token64)`, including the all-zero token when no token was used. `client_flight_hash` covers the entire client message, including early ciphertext and binder. The accepted routing CID must be the original CID or the CID from the single accepted Retry. The client rejects a mismatch with its actual attempt. The original complete cipher offer is immutable throughout Retry. The server-selected cipher and support mask in SP are unauthenticated candidates until the entire response verifies; a forged prefix never installs a cipher or changes an offer.

Let `P` be the valid resumption PSK if and only if `psk_accepted=1`, otherwise 32 zero bytes. A server may reject a ticket and continue a full handshake; it must then reject early data. The client rejects `psk_accepted=1` if it offered no corresponding ticket.

```
T0 = H(ASCII("GRT11 handshake") || client_message || SP)
HS = Extract(T0, SK || P)
HCTX = T0 || u16(selected_cipher)
hs_key = L(HS, "server flight key", HCTX, 32)
hs_iv  = L(HS, "server flight iv",  HCTX, NLen(selected_cipher))
sf_key = L(HS, "server finished",  T0, 32)
```

The body prefix `SB` is:

```
server_parameters:128
|| credential_count:u8 || early_status:u8 || reserved:u16 (=0)
|| credentials:credential_count*196
|| server_reset_token:16
|| early_plaintext_hash:32
```

`early_status` is 0 absent, 1 accepted, or 2 rejected. The hash is `H(early_plaintext)` when accepted and zero otherwise. PINNED uses zero credentials; CHAIN uses 1–4. The server Finished is:

```
server_finished = HMAC(sf_key, T0 || H(SB))
server_body_ciphertext = AEAD.Seal(selected_cipher, hs_key, hs_iv,
                                   SB || server_finished, AAD=SP)
server_message = SP || server_body_ciphertext
T1 = H(ASCII("GRT11 established") || client_message || server_message)
ROOT0 = L(HS, "root", T1, 32)
```

`encrypted_body_length` is `len(SB)+32+16`; it is known before deriving T0. The handshake encryption key is used for exactly one immutable server body at nonce `hs_iv`. Sending its cached ciphertext again is permitted. Recomputing a different body under that key and nonce is forbidden.

C MUST validate the server identity, credential policy, entire server transcript, transport parameters, cipher-offer binding, supported-mask/selection consistency, and Finished before exposing server-authenticated application bytes. S erases `EK_S.private`, `SK`, DH outputs, and SS after deriving and caching the server flight and required session secrets. C erases its one-time decapsulation key after the authenticated server flight has been accepted. Replayed client flights after cache expiry produce fresh server secrets and a different ROOT0.

### 8.4 Initial session keys and confirmation

For direction `d`, use literal labels `c2s` and `s2c`:

```
KCTX(e) = T1 || u16(selected_cipher) || u32(e)
key[d,e] = L(ROOTe, "<d> key", KCTX(e), 32)
iv[d,e]  = L(ROOTe, "<d> iv",  KCTX(e), NLen(selected_cipher))
refresh_secret[e] = L(ROOTe, "refresh secret", KCTX(e), 32)
finish_key = L(ROOT0, "client finished", T1, 32)
done_key   = L(ROOT0, "server done", T1, 32)
resumption_master = L(ROOT0, "resumption master", T1, 32)
exporter_master   = L(ROOT0, "exporter master", T1, 32)
```

The bracketed direction is substituted; for example the exact label is `c2s key`. Directional roots and packet counters are never interchanged. Application exporters use `L(exporter_master, "application exporter", H(application_label || context), 32)` with both application strings independently u16-length-prefixed before hashing.

C sends FINISH in epoch zero:

```
client_finish_mac = HMAC(finish_key, T1 || client_reset_token16)
FINISH.body = client_finish_mac:32 || client_reset_token:16
```

S authenticates it, installs the client's reset token for the client's initial CID, validates the path, and replies:

```
HANDSHAKE_DONE.body = HMAC(done_key, T1 || client_finish_mac)
```

C may send application frames in the same packet after FINISH and in later packets. Until HANDSHAKE_DONE is received, every outgoing packet containing application data MUST also contain FINISH. S processes application frames in S_FINISHING only in a transaction that contains a valid FINISH. A present but incorrect FINISH MAC inside an authenticated packet is PROTOCOL_VIOLATION; absence of FINISH causes the specified noncommitting drop. S may send server application data after it has produced its server flight, but before client FINISH only in response to an explicitly admitted early request, within amplification and congestion limits. C may accept authenticated application data after validating the server flight without waiting for HANDSHAKE_DONE.

FINISH and HANDSHAKE_DONE are reliable logical items. Losing their packets does not restart the key agreement. HANDSHAKE_DONE is repeated until its carrying packet is acknowledged. Repeated valid FINISH causes the same logical HANDSHAKE_DONE, protected using a fresh packet number. Private handshake keys and complete flight caches are erased/released after confirmation and their I/O/retransmission fences. Duplicate confirmed-handshake handling retains only the necessary bounded transcript hashes, finished-proof cache and logical response state, not permanent full flight buffers. A duplicate bootstrap flight after confirmed establishment may be dropped; DATA confirmation retransmission remains idempotent.

<a id="section-9"></a>

## 9. Resumption and bounded 0-RTT

### 9.1 Ticket metadata and opaque ticket

Tickets are issued only over an established connection. A NEW_TICKET frame carries an opaque 224-byte ticket plus 204-byte public metadata. The metadata is:

```
ticket_id:16 || expires_unix_s:u64 || replay_domain:16
|| service_hash:32 || max_early_plaintext:u32 || cached_server_parameters:128
```

The ticket PSK is `L(resumption_master, "ticket psk", ticket_id, 32)`. The server stores the PSK in the encrypted ticket; the client derives it when receiving NEW_TICKET and stores it with the ticket and metadata. Ticket plaintext is:

```
psk:32 || ticket_id:16 || issued_unix_s:u64 || expires_unix_s:u64
|| replay_domain:16 || service_hash:32 || metadata_hash:32
|| policy_hash:32 || max_early_plaintext:u32
```

That is 180 bytes. The opaque representation is `key_id:u32 || nonce:24 || AEAD.Seal(0x0001, ticket_key, nonce, plaintext180, AAD=ASCII("GRT11 ticket") || u32(key_id))`, totaling 224 bytes. The envelope cipher is always XChaCha20-Poly1305, independent of the connection cipher. A ticket key is 32 bytes. Nonces use a unique per-key 16-byte issuer prefix and an eight-byte persistent monotonic issuance counter; clustered issuers share a counter allocator or distinct prefixes. Snapshot restore requires a fresh key/prefix, not reuse. Rotate a ticket key before 2^24 envelopes or 24 hours of issuance, whichever comes first; retain decrypt-only keys only for the permitted ticket validity interval. Key IDs cannot ambiguously identify different live keys. Ticket-key nonces MUST never repeat. The maximum lifetime is 24 hours. Server ticket validation checks the metadata hash, target service, authentication policy, validity interval, and locally authorized replay domain.

### 9.2 Binder and early encryption

Define `CH0` as the client message through the optional ticket metadata, ending immediately before `early_ciphertext_length`. Define `CH_no_binder` as the complete client message except its final 32-byte binder.

```
ES = Extract(Z(32), ticket_psk)
early_key = L(ES, "early key", H(CH0), 32)
early_iv  = L(ES, "early iv",  H(CH0), 24)
binder_key = L(ES, "binder", H(CH0), 32)
early_ciphertext = AEAD.Seal(0x0001, early_key, early_iv, early_plaintext, AAD=CH0)
binder = HMAC(binder_key, H(CH_no_binder))
```

Early encryption always uses XChaCha20-Poly1305; no cached AES selection is assumed before this attempt is authenticated. CH0 includes the full new cipher offer and cached parameter metadata. Current DATA selection may differ from the previous connection without changing the early cipher or weakening replay policy. The early plaintext is a complete sequence of at most 32 ordinary frames and at most 4096 bytes. Only STREAM_OPEN, STREAM, and PADDING are allowed. Streams use generation zero and credits in the cached server parameters. The server MUST either commit the entire early batch or reject it entirely. Partial acceptance and ambiguous per-stream early offsets are forbidden.

The client never changes CH0, early plaintext, or binder while retrying one attempt. A new attempt uses new keys, nonces, and attempt ID. A ticket may be offered without early data to mix its PSK into a fresh hybrid handshake.

### 9.3 Replay policy and acceptance

Early data is opt-in at the application API and disabled by default. The application MUST explicitly declare the batch suitable for replay exposure. There is no claim of exactly-once business execution.

Before exposing early data, a server MUST atomically and durably consume `(replay_domain, ticket_id)` in a replay ledger shared by every server that can accept that ticket. The record binds the accepted attempt ID and client-flight hash. The record must remain through ticket expiry plus the deployment's maximum allowed clock uncertainty. Duplicate delivery to an already live attempt is handled by that attempt, never executed twice. An attempt whose prior execution outcome is unknown after a crash MUST have early data rejected, not guessed to be new.

Without this ledger, early acceptance is forbidden; ordinary 1-RTT establishment remains available. A deployment cannot replace shared replay protection with an uncoordinated per-machine cache and continue to claim the same protection.

Acceptance also requires a valid binder, valid prekeys, successful bounded preflight for every early frame, compatible current receive limits no smaller than the offered early use, and an authorized early-data application. Only after all those checks may the replay ledger be consumed and the transport state committed. Ledger consumption followed by a crash can lose an early operation; preventing duplicate execution is not guaranteeing execution.

An accepted server response cryptographically acknowledges the entire early batch by its hash. C marks the included logical stream ranges and opens as accepted. A rejected or absent early result requires re-queuing those same application bytes with the same stream handles and offsets as normal 1-RTT data. No second delivery is created. These bytes count exactly once toward logical flow-control use. Early flight bytes still count toward bootstrap congestion and amplification accounting.

A resumed connection still performs the complete fresh X25519/ML-KEM handshake. Early data itself depends on the stored PSK and does not gain the fresh exchange's secrecy retroactively. Exposure of ticket encryption keys or the ticket PSK can compromise recorded early data.

<a id="section-10"></a>

## 10. Transport parameters

Transport parameters are an exact 128-byte structure; unused bytes are zero. They are authenticated in their respective handshake transcript.

| Offset | Type | Field |
|---:|---|---|
| 0 | u16 | parameter_version = 0x0101 |
| 2 | u16 | feature_flags: DATAGRAM=1, EARLY=2, ECN=4, MIGRATION=8, PARK=16; other bits zero |
| 4 | u16 | max_udp_payload, 1200..4096, multiple of 8 |
| 6 | u16 | max_frames = 32 |
| 8 | u32 | idle_timeout_ms, 1000..600000 |
| 12 | u32 | max_ack_delay_us, 0..10000 |
| 16 | u64 | initial_max_data, 0..`2^62-1` |
| 24 | u32 | initial_stream_window, power of two, 4096..16777216 |
| 28 | u32 | receive_packet_history, power of two, 4096..1048576 |
| 32 | u16 | initial_bidi_slots, 1..1024 |
| 34 | u16 | initial_uni_slots, 0..1024 |
| 36 | u16 | maximum_bidi_slots, initial..1024 |
| 38 | u16 | maximum_uni_slots, initial..1024 |
| 40 | u16 | active_cid_limit, 2..4; compact default 2 |
| 42 | u16 | max_ack_ranges = 8 |
| 44 | u32 | max_datagram_data, 0..4032 |
| 48 | u32 | max_crypto_message = 4096 |
| 52 | u32 | max_pending_control = 64 |
| 56 | u16 | max_ack_ratio, one of 2,4,8,16,32; this receiver's authenticated ceiling |
| 58 | u16 | initial_ack_ratio, one of 2,4,8,16,32, not greater than max_ack_ratio |
| 60 | u32 | max_park_duration_ms: 0 disables parking; otherwise 1000..1800000 |
| 64 | u32 | max_path_validation_ms: 1000..60000; default 10000 |
| 68 | u16 | candidate_path_limit = 1 |
| 70 | u16 | retained_old_path_limit = 1 |
| 72 | bytes[56] | reserved = zero |

Feature use requires the flag at both endpoints. A PARK flag with a zero lease limit, or a nonzero lease limit without PARK, is TRANSPORT_PARAMETER_ERROR. Path validation uses the smaller advertised max_path_validation_ms as its deadline cap. Lease negotiation cannot enlarge that cap or bypass MIGRATION policy. A server's EARLY flag is its offer for future resumption; the current early result is separately authenticated. The current DATAGRAM payload limit is the lesser of the negotiated receiver limit and the available path packet capacity. A zero DATAGRAM limit disables datagrams regardless of flags. An implementation MUST implement parsing for all core frames even when the negotiated policy disallows using a feature.

Each receiver's advertised values govern what its peer may send to it. The negotiated idle timeout is the smaller of the two advertised timeouts, but a connection is not declared idle earlier than three current base PTOs after its last activity. A live accepted parking lease replaces ordinary idle expiry only for its explicitly defined suspended interval in Section 35; a local OFFLINE hint alone does not extend it. max_ack_delay describes the peer's allowed ACK delay and is not the sender's timer granularity. The two ACK-ratio fields describe the advertising receiver's own ACK-generation behavior. Every implementation handles ACK ratios through 32; an endpoint may adapt within its authenticated ceiling without a new frame or a new handshake. This is not a sender-controlled request to violate the receiver's time bound. GSO, GRO, AF_XDP, io_uring, queue depth, and zero-copy support are never negotiated transport parameters.

Parameters that allocate usable memory/credit MUST be backed by capacity before they are advertised. Maximum slot ceilings alone do not grant OPEN permission; only the initial/current granted slots require corresponding metadata admission. Section 36 distinguishes cold ceilings, grants, and physical allocation. Transport parameters do not silently grow during a connection. MAX_DATA, MAX_STREAM_DATA, and MAX_SLOTS are the only credit-increase mechanisms. A local receive representation may grow behind these grants; no receive-window growth may precede capacity admission. Compact/Mobile defaults are specified in Section 36.

<a id="section-11"></a>

## 11. Frame grammar and registry

Every frame starts at an 8-byte boundary relative to the plaintext start:

```
frame_type:u16 || flags:u16 || body_length:u32 || body:body_length
|| zero_alignment_padding:((-body_length) mod 8)
```

The padding is 0–7 bytes, not included in body_length. Frame headers are included in the 32-frame limit. A parser MUST reject integer overflow, a frame crossing the plaintext end, nonzero alignment padding, or a nonempty partial final header. A PADDING frame body consists entirely of zero bytes and can provide additional minimum plaintext or PMTU-probe padding. Every plaintext is an integral number of aligned frames.

Unknown frame types with bit 15 clear are critical and cause FRAME_ENCODING_ERROR after authentication. Unknown frame types with bit 15 set are skipped and are ack-eliciting. Known frame flags must be zero except where specified. A receiver MUST NOT allocate body_length bytes merely because a frame says to do so; its enclosing authenticated packet already bounds the body.

| Type | Name | Body | Ack-eliciting | Logical reliability |
|---:|---|---|---|---|
| 0x0000 | PADDING | zero bytes | No | None |
| 0x0001 | PING | empty | Yes | Probe, not intrinsically reliable |
| 0x0002 | ACK | Section 13 | No | Regenerated, not retransmitted |
| 0x0003 | FINISH | MAC32, client reset token16 | Yes | Until handshake confirmation |
| 0x0004 | HANDSHAKE_DONE | MAC32 | Yes | Until packet ACK |
| 0x0010 | STREAM_OPEN | stream_id:u64 | Yes | Until packet ACK |
| 0x0011 | STREAM | stream_id:u64, offset:u64, data | Yes | Byte-range reliable; flag bit 0 = FIN |
| 0x0012 | RESET_STREAM | stream_id:u64, final_size:u64, error:u32, zero:u32 | Yes | Until packet ACK |
| 0x0013 | STOP_SENDING | stream_id:u64, error:u32, zero:u32 | Yes | Until packet ACK or receive half terminal |
| 0x0014 | MAX_DATA | absolute_limit:u64 | Yes | Highest unsatisfied value |
| 0x0015 | MAX_STREAM_DATA | stream_id:u64, absolute_limit:u64 | Yes | Highest unsatisfied value |
| 0x0016 | MAX_SLOTS | stream_class:u8, zero:3, absolute_limit:u32 | Yes | Highest unsatisfied value |
| 0x0017 | DATA_BLOCKED | absolute_limit:u64 | Yes | Advisory, not reliable |
| 0x0018 | STREAM_BLOCKED | stream_id:u64, absolute_limit:u64 | Yes | Advisory, not reliable |
| 0x0019 | SLOTS_BLOCKED | stream_class:u8, zero:3, absolute_limit:u32 | Yes | Advisory, not reliable |
| 0x001A | STREAM_RETIRE | stream_id:u64 | Yes | Until RETIRE_ACK |
| 0x001B | RETIRE_ACK | stream_id:u64 | Yes | Regenerated on duplicate retirement |
| 0x0020 | DATAGRAM | context_id:u64, data | Yes | Unreliable |
| 0x0021 | PATH_CHALLENGE | challenge:16 | Yes | Challenge retry state |
| 0x0022 | PATH_RESPONSE | challenge:16 | Yes | Regenerated on challenge |
| 0x0023 | NEW_CID | sequence:u64, retire_prior_to:u64, cid:u64, reset_token:16 | Yes | Until packet ACK |
| 0x0024 | RETIRE_CID | sequence:u64 | Yes | Until packet ACK |
| 0x0025 | NEW_TICKET | metadata:204, ticket:224 | Yes | Until packet ACK |
| 0x0026 | CLOSE | error:u32, offending_frame:u16, zero:u16, reason_hash:16 | No | Bounded close timer |
| 0x0027 | NEW_TOKEN | address_token:64, kind REUSE | Yes | Latest pending token until ACK or superseded |
| 0x0028 | PATH_INTENT | migration_seq:u64, cutover_pn:u64, intent_flags:u32, zero:u32 | Yes | Until matching selection result/deadline |
| 0x0029 | PATH_SELECTED | migration_seq:u64, status:u16, zero:u16, zero:u32 | Yes | ACCEPTED until ACK/superseded; other results regenerated |
| 0x0030 | CRYPTO_FRAGMENT | Section 19 | Yes | Fragment logical item |
| 0x0031 | KEY_COMMIT | epoch:u32, zero:u32, refresh_id:16, MAC:32 | Yes | Until KEY_CONFIRM |
| 0x0032 | KEY_CONFIRM | epoch:u32, zero:u32, refresh_id:16, MAC:32 | Yes | Until packet ACK |
| 0x0033 | KEY_REQUEST | requested_epoch:u32, zero:u32 | Yes | Until request is fulfilled |
| 0x0034 | MTU_PROBE | probe_id:u64 | Yes | Probe state, not reliable data |
| 0x0040 | PARK_REQUEST | request_id:u64, duration_ms:u32, zero:u32 | Yes | Until result/deadline |
| 0x0041 | PARK_GRANT | request_id:u64, duration_ms:u32, status:u16, zero:u16 | Yes | Regenerated result; fixed lease expiry |
| 0x0042 | PARK_WAKE | request_id:u64, wake_seq:u64 | Yes | Until matching PARK_AWAKE/deadline |
| 0x0043 | PARK_AWAKE | request_id:u64, wake_seq:u64 | Yes | Until packet ACK |

At most one ACK, CLOSE, FINISH, HANDSHAKE_DONE, KEY_COMMIT, KEY_CONFIRM, MTU_PROBE, NEW_TOKEN, PATH_INTENT, PATH_SELECTED, and each PARK_* frame may appear in a packet. At most one PATH_CHALLENGE and one PATH_RESPONSE may appear, and they may coexist. PATH_INTENT/SELECTED encodings and placement are defined in Section 17; parking controls in Section 35. None is permitted in early data. PARK_AWAKE is never coalesced with application frames. A CLOSE packet contains exactly one CLOSE frame and no PADDING. An MTU_PROBE packet may contain only MTU_PROBE, PING, and PADDING. Other frames may repeat subject to transactional validation. A STREAM_OPEN and subsequent frames for its new stream may share a packet; their semantic evaluation follows frame order in the transaction's tentative state.

STREAM's FIN bit declares `final_size=offset+len(data)`. A zero-length STREAM is allowed only with FIN. DATAGRAM may have zero application bytes; its `context_id:u64` field is reserved for future application-layer demultiplexing extensions and MUST be zero (`0`) in baseline GRT/1.1 (`FRAME_ENCODING_ERROR` on non-zero values during `RX_PARSE`). Unknown optional frames are not allowed in early data. Internal alignment padding has no application meaning.

<a id="section-12"></a>

## 12. Stream identifiers, lifecycle, and bounded reassembly

### 12.1 Handle encoding

A stream handle is:

```
stream_id = (generation << 18) | (slot << 2) | (unidirectional << 1) | initiator
```

`initiator=0` is the network client; `initiator=1` is the network server. `unidirectional` is 0 or 1. `slot` is a 16-bit field, but must be below the negotiated slot limit and never 1024 or larger in this protocol version. `generation` is 46 bits. `stream_class = stream_id & 3`. Generation zero is the first use of a slot. Generations increase by exactly one after retirement, never wrap, and are not reused within the connection.

Each class has a bounded logical slot table; an implementation may use sparse pages and inline generation-zero defaults. Active records store generation, lifecycle, transmit/receive state and bounded buffer descriptors; retired generation tombstones remain until connection close. Maximum slot ceilings do not require eager physical allocation. Granting usable slots does require metadata-capacity admission. There is no need to retain an unbounded map of historical stream IDs. Handles are opaque application tokens, not chronological ordering numbers.

Unidirectional streams have only the initiator's send half and the peer's receive half. A STREAM in the reverse direction, or a MAX_STREAM_DATA/STOP_SENDING directed at a nonexistent half, is STREAM_STATE_ERROR. In a bidirectional stream, both directions use the same handle but have distinct byte offsets, flow accounting, FIN, reset, and receive rings.

### 12.2 Opening without an extra stream round trip

Only the handle's initiator may send STREAM_OPEN. It can be in the same packet as initial data. Until the initiator has received an acknowledgment for an OPEN-carrying packet, every packet that the initiator sends containing data or half-control for that stream MUST also carry the same STREAM_OPEN before those frames. This lets out-of-order packets create the context without waiting for a missing first packet. The non-initiating endpoint MUST NOT repeat STREAM_OPEN; it may send reverse-half data or control only after accepting the initiator's OPEN. The initiator already has its local context and can therefore receive that reverse-half traffic before its OPEN has been acknowledged.

For an EMPTY slot, generation must equal its expected generation. For an ACTIVE slot, repeating its same OPEN is idempotent. A frame for a generation below the retained slot generation is stale and ignored without changing byte credit. In RETIRED_WAIT_OPEN, all same-generation frames are also stale except STREAM_RETIRE, which regenerates RETIRE_ACK; an old repeated OPEN never recreates the retired stream. EXHAUSTED similarly ignores already-retired same-generation traffic. A future generation is an error unless a valid next-generation OPEN is permitted by RETIRED_WAIT_OPEN. A data frame for a not-yet-open generation is an error; the required repeated OPEN removes any legitimate network-reordering reason for that case.

Local creation requires a permitted peer slot, an available local slot context, and backing storage for any local receive half. Peer initial_bidi_slots and initial_uni_slots govern peer-initiated admission at that receiving endpoint. MAX_SLOTS can increase those absolute limits, never decrease them and never exceed the authenticated maximum. It carries the exact initiator/direction class being authorized; a receiver may grant only classes initiated by its peer.

### 12.3 Reliable bytes and final size

A receiver maintains, per receive half:

```
base                 # first byte not yet consumed by application
contiguous_end       # one past contiguous authenticated buffered bytes
highest_received_end # high-water offset, including declared final size
advertised_limit     # absolute MAX_STREAM_DATA value
final_size_known, final_size
reset_received, reset_error
ring_capacity R, data_ring[R], present_bitmap[R]  # logical representation; admitted sparse backing is permitted
```

R is initialized from initial_stream_window. It is a local power-of-two receive capacity, and may grow up to MAX_STREAM_WINDOW after backing admission and an atomic representation transition. Before moving storage, fence all references or retain the old pinned pages until their leases finish; concurrent RX staging is revalidated against the representation generation. Only after the new capacity is installed may MAX_STREAM_DATA expose it. Shrinkage is permitted only if outstanding grants, retained bytes, and pins still fit; a credit limit never decreases. Thus a Compact 4096-byte initial window need not be a permanent throughput ceiling. Offset `x` maps to index `x & (R-1)`. A byte may be stored only when `base <= x < base+R` and `x < advertised_limit`. Receiver credit advancement MUST ensure the advertised limit never exceeds `base+R`. The initial limit is R. Bytes below base are duplicate already-consumed bytes and require no storage.

A packet transaction tests every overlap with retained bytes: overlapping retained bytes must match. Different retained bytes at one offset are STREAM_DATA_ERROR. Already released bytes are ignored and are not retroactively compared. New present bits and bytes become visible together only at commit. The first contiguous gap stops delivery on that stream alone. Adjacent stream data and datagrams remain eligible.

If final size is unknown, a FIN or RESET_STREAM establishes it. A later different final size, data past the final size, or a final size below highest_received_end is FINAL_SIZE_ERROR. A FIN does not discard missing bytes. A RESET abandons missing bytes, notifies the application once, and makes that receive half terminal after the notification and discard complete.

For each frame, compute `new_high=max(old_high, offset+data_length)` or `max(old_high, reset_final_size)`. The increment is charged to connection receive credit, even if the frame contains only the last byte after a large gap. Duplicates and overlaps do not consume the same logical credit again.

### 12.4 Flow control

Connection flow control is the cumulative sum of per-half received high-water increments across all stream generations. It does not count packets, headers, ACKs, or retransmitted copies. A packet preflight MUST check the sum of all increments in that packet, not check each frame against the same old connection total independently.

Sender and receiver connection totals and absolute MAX_DATA limits are monotonically nondecreasing. Sender data can be emitted only if both its stream absolute limit and its connection absolute limit permit the high-water increments. Retransmission at an already charged offset requires congestion budget but no additional logical flow credit.

The sender reserves flow credit before sealing; once assigned to accepted application offsets, that reservation remains charged even if the packet seal is canceled. The sender keeps those bytes pending for later transmission. RESET's final size covers at least that reserved high-water mark. This avoids rollback races between parallel packet builders. A sender never reserves beyond accepted application data or the peer's advertised limit.

Credit updates are monotonic. A smaller MAX_DATA or same-generation MAX_STREAM_DATA is ignored; a limit exceeding `2^62-1` is an error. No receiver can revoke a previously granted limit. MAX_STREAM_DATA for a retired generation is ignored. A credit frame for an impossible future or unopened generation is STREAM_STATE_ERROR unless the same packet opened it.

An application-consumption event clears released ring bits, advances base, and may advertise `base+R`. A connection credit policy may return consumed or reset-discarded logical bytes, but every grant must be backed by storage or by the fact that the credited range is explicitly discarded. The receiver SHOULD transmit a new credit value when at least one quarter of the corresponding window has been released, and MUST make progress when a peer is blocked and releasable capacity exists.

BLOCKED frames are rate-limited to one per newly observed limit or one current PTO, whichever is less frequent. A sender does not rely on BLOCKED to request a grant that the receiver otherwise never sends.

### 12.5 Send-half state machine

| State | Event and guard | Action | Next |
|---|---|---|---|
| SEND_OPEN | APP_WRITE within local capacity | Append immutable bytes; no network credit assumed | SEND_OPEN |
| SEND_OPEN | APP_FIN | Freeze final size at accepted byte count; queue FIN after credit permits | SEND_FIN |
| SEND_OPEN / SEND_FIN | APP_RESET or effective STOP_SENDING | Abandon unsent/retransmittable bytes; queue RESET with consistent reserved high-water/final size | SEND_RESET |
| SEND_OPEN / SEND_FIN | ACK of a byte range | Mark exact range; release only ACKed, unpinned storage | Same |
| SEND_FIN | All bytes and FIN acknowledged | Signal local send completion once | SEND_DONE |
| SEND_RESET | ACK of RESET | Release remaining unpinned data; signal reset completion | SEND_DONE |
| SEND_DONE | Duplicate ACK or STOP_SENDING | Ignore; no new RESET required | SEND_DONE |

Application writes after APP_FIN or APP_RESET fail locally. Once a FIN's final size has been emitted, RESET must preserve that final size. A peer STOP_SENDING is only a request to terminate its corresponding send half; the first effective request wins. A local error code chosen for the resulting RESET is not required to equal the peer's code, but it must not change across retransmissions.

### 12.6 Receive-half state machine

| State | Event and guard | Action | Next |
|---|---|---|---|
| RECV_OPEN | Valid STREAM without FIN | Commit bytes; advance contiguous_end; notify readable | RECV_OPEN |
| RECV_OPEN | Valid FIN | Fix final size and commit bytes | RECV_FIN |
| RECV_OPEN / RECV_FIN | Valid RESET | Fix/check final size; discard incomplete data; notify reset | RECV_RESET |
| RECV_FIN | base reaches final_size | Deliver EOF once | RECV_DONE |
| RECV_RESET | Reset notification consumed; buffers unpinned | Complete receive half | RECV_DONE |
| Any live receive state | APP_STOP | Queue STOP_SENDING; continue validating flow/final size | Same |
| RECV_DONE | Valid stale same-generation duplicate | Ignore semantic bytes; do not redeliver | RECV_DONE |

Receiving all bytes but not consuming them is not RECV_DONE. Retirement cannot reclaim unread application bytes.

### 12.7 Slot retirement and reuse

Only the stream initiator requests retirement. It does so only after every existing local half is done. STREAM_RETIRE is reliable until RETIRE_ACK. The other endpoint records a pending retirement and waits until its own halves are done; it then sends RETIRE_ACK and enters RETIRED_WAIT_OPEN. Unread bytes prevent this step. It may free old payload buffers but retains the slot generation and retirement response.

The initiator receives RETIRE_ACK, releases the old slot's payload state, increments the expected generation, and may immediately send the next generation's OPEN. The non-initiator accepts exactly that next OPEN after it has committed the retirement response. A duplicate old RETIRE receives the same old RETIRE_ACK until next-generation OPEN arrives. After next-generation OPEN, all older-generation frames are stale and ignored.

The RETIRE_ACK need not itself be separately acknowledged to allow peer slot reuse: receipt of the next OPEN is implicit proof that it arrived. Its carrying packet still participates in congestion accounting. This prevents both an extra per-stream open RTT and an unbounded closed-stream tombstone table. A slot whose generation is exhausted is permanently retired; the application may use another slot or a new connection.

<a id="section-13"></a>

## 13. Packet acknowledgments and replay tracking

### 13.1 ACK encoding

An ACK body is:

```
ack_delay_us:u32 || range_count:u16 || reserved:u16 (=0)
|| ect0_count:u64 || ect1_count:u64 || ce_count:u64
|| range[range_count] { low:u64, high:u64 }
```

`1 <= range_count <= 8`. Each interval is inclusive. Ranges are sorted by descending high. Each low is at most its high. Consecutive ranges neither overlap nor touch: `next.high+1 < previous.low`. The largest acknowledged packet is the first range's high. ack_delay_us measures time from committing that packet to constructing this ACK, clamped at the receiver's advertised delay. ECN counters count committed, distinct DATA packets, not bootstrap fragments or duplicates; they are zero if ECN was not negotiated.

ACK does not acknowledge anything outside its explicit intervals. There is no cumulative implication that every packet below the largest number arrived. This is essential when an ACK includes only the newest eight ranges.

### 13.2 Receive bitmap and ACK scheduling

Each direction has a receive replay bitmap of the negotiated local width W. It records committed packet numbers. Advancing the largest committed number slides the bitmap; numbers older than `max(0, largest-W+1)` are too old and dropped. No replay state advances before AEAD and transactional commit. Parallel authenticated packets recheck the bitmap under the commit arbiter.

Maintain `ack_ratio` in the authenticated set/ceiling, initially initial_ack_ratio. Count **child datagrams**, not receive completions or GRO aggregates. An ACK becomes due after ack_ratio newly committed ack-eliciting packets or at the deadline of the first unacknowledged eliciting commit plus max_ack_delay, whichever comes first. A new arrival cannot push that deadline out.

A newly observed gap, a newly filled gap, a new out-of-order range, authenticated handshake/key progress, a path challenge requiring response, or a newly committed CE-marked eliciting packet sets an immediate ACK obligation. Multiple such events can coalesce into one current ACK at the next scheduler opportunity; immediate does not require a separate ACK for every descriptor already in the same burst. ACK-only arrivals do not independently solicit ACKs, even when reordered. Zero max_ack_delay means immediate scheduling, not permission to bypass anti-amplification, control-rate limits, or egress availability.

A receiver MAY adapt its ratio among 2,4,8,16,32 up to its ceiling using authenticated traffic rate and resource pressure. It MUST NOT increase max_ack_delay, restart an existing deadline, ignore loss/CE overrides, or wait for a GRO/GSO batch to fill. The reference local controller samples newly committed eliciting packets every 10000 microseconds, computes a filtered rate `r=(3*old_r+sample_rate)/4`, chooses the largest allowed ratio not exceeding `max(2,floor(r*max_ack_delay_us/2000000))` for r in packets/second, and resets to 2 after one RTT without eliciting data or any gap/CE event. This controller is a recommendation, not a peer-visible rate formula; fixed ratio 2 remains conforming. Packet-threshold recovery and byte-counted congestion growth are unchanged.

The ACK generator extracts up to `max_ack_ranges` (at most eight) contiguous runs in the receive bitmap. The first range MUST be the newest contiguous run anchored at `largest_observed`. When more than `max_ack_ranges` disjoint runs exist, the generator MAY prioritize runs that newly resolved a gap since the previous emitted ACK before filling remaining slots in descending packet-number order. Older committed ranges may be omitted; omission never grants permission to discard reliable application data. A sender that fails to receive acknowledgment of an old transmission retransmits its still-unacknowledged logical bytes in a fresh packet number. It does not need a historical ACK range to preserve eventual delivery.

Receiving a duplicate may schedule a current ACK at most once per current max_ack_delay interval, with a 50-microsecond floor, but does not trigger an immediate-ACK storm. ACK-only packets do not cause ACKs solely because they arrived. ACK state need not be reliable because the logical sender state remains until acknowledged.

### 13.3 Sent ledger and ACK validation

The sender maintains a bounded recent packet-number ledger with status RESERVED, SUBMITTED, EMITTED, BURNED, ACKED, LOST, or administratively ABANDONED, and a sent-item table for every live retransmittable reference. The emission ledger includes ACK-only packet numbers so acknowledgments of numbers never emitted can be detected within the retained range.

An ACK with any high greater than or equal to next_packet_number is ACK_RANGE_ERROR. Within retained emission history, an ACK that covers RESERVED, BURNED, or never-emitted numbers is also an error. An ACK covering SUBMITTED entries waits for those entries' I/O acceptance results under Sections 22.1 and 29.5; it is not rejected merely because a send CQE has not yet been reaped. The whole authenticated RX transaction remains uncommitted during that bounded wait. If no wait slot is available, drop that transaction without ACK rather than produce a false protocol error. A definitive nonacceptance makes the entry BURNED and a subsequent authenticated ACK for it invalid; an ambiguous post-handoff outcome remains EMITTED and uses normal loss recovery. Below the verification floor, an endpoint ignores unverifiable acknowledgments rather than inferring an error or freeing forgotten live data. ABANDONED is never an ACK of its logical data and never a new-path congestion-growth event. A retained live sent-packet entry remains verifiable proof of emission even below that floor and its ACK is processed normally. The live-item table MUST NOT forget reliable obligations merely because the validation history advances.

ACK processing iterates over bounded local tables or a bounded bitmap intersection, not every integer in an attacker-supplied 64-bit range. Processing the same acknowledgment twice cannot decrement bytes_in_flight twice, increase cwnd twice, release a DMA reference twice, or deliver a second completion.

A packet ACK means that the complete packet transaction was accepted by the peer. For STREAM, this means bytes were retained or already delivered; for DATAGRAM it means network reception, not application retention. ACKed logical control versions supersede retransmission of those versions. A later higher credit value supersedes a lower pending value regardless of whether the lower packet was lost.

<a id="section-14"></a>

## 14. Loss recovery, timers, and logical retransmission

### 14.1 Sent-packet metadata

Every emitted ack-eliciting packet has at least:

```
packet_number:u64, epoch:u32, path_slot:u8, flags:u8,
wire_bytes:u16, send_time_us:u64, path_send_ordinal:u64,
item_ref_start:u32, item_ref_count:u16, descriptor_generation:u32,
status:{IN_FLIGHT,ACKED,LOST}, counted_in_flight:bool,
was_mtu_probe:bool, ect_mark:u2
```

Implementation storage may pack fields differently. wire_bytes is the UDP payload length. Packet numbers identify transmissions; stream offsets identify application bytes. Retransmitting a lost STREAM packet's bytes uses a fresh packet number, current key epoch, current path budget, and current PMTU segmentation.

### 14.2 RTT estimation

For an ACK that newly acknowledges its largest packet, take a sample only if that packet was ack-eliciting, its timestamp is retained, and the ACK arrived on the same currently validated path as that transmission. Migration-crossing ACKs may free bytes but do not produce mixed-path RTT samples.

```
latest = max(1, now - send_time)
min_rtt = min(min_rtt, latest)
delay = min(ack_delay_us, peer_max_ack_delay_us)
adjusted = latest
if latest >= min_rtt + delay:
    adjusted = latest - delay
```

For the first sample, `srtt=adjusted`, `rttvar=ceil(adjusted/2)`. Otherwise, using the old srtt:

```
rttvar = ceil((3*rttvar + abs(old_srtt-adjusted))/4)
srtt   = ceil((7*old_srtt + adjusted)/8)
```

No application timer can rewrite a retained send timestamp. For an inline device, timestamps identify its committed egress acceptance. For an asynchronous software adapter, the conservative release timestamp is captured immediately before publication to the backend and retained when acceptance is confirmed; the later CQE-observation time is not used as the send time. Unaccepted submissions do not yield RTT samples. A documented hardware timestamp may replace the estimate only before that packet contributes to RTT/loss state. Application enqueue time is never a send timestamp. All children in an unshaped GSO request may share one conservative release timestamp; invented per-child wire times are forbidden.

### 14.3 Loss declaration

Loss is evaluated separately for each local sending path, using that path's monotonically increasing emission ordinal. A packet is packet-threshold-lost when a newly acknowledged packet on the same path has an ordinal at least three greater. Using global packet-number distance across migrated paths for this test is forbidden.

A packet can be time-threshold-lost only after a later packet on that same path has been acknowledged and:

```
now - packet.send_time >= max(50, ceil(9*max(srtt, latest_rtt)/8))
```

No later same-path ACK means a tail may need PTO, not a speculative time-threshold declaration. Before a first RTT sample, latest_rtt is INITIAL_RTT_US and min_rtt has an explicit unset flag. Declaration removes its counted in-flight bytes once and requeues still-live logical items once. An ACK of a previously declared-lost packet is permitted and may resolve a retransmission, but cannot restore already-freed storage or create duplicate credit.

### 14.4 Probe timeout

For a path with outstanding ack-eliciting traffic:

```
base_pto = max(1000, srtt + max(4*rttvar, 50) + peer_max_ack_delay_us)
pto_duration = min(base_pto * 2^pto_count, 60000000)
pto_deadline = last_ack_eliciting_send_time + pto_duration
```

Before a sample, use srtt=333000 and rttvar=166500. Multiplication saturates at 60 seconds. A newly acknowledged ack-eliciting packet resets that path's pto_count to zero. The next loss timer is the earliest eligible time-loss deadline or PTO; stale timer callbacks cannot act on reused contexts.

At PTO, send at most two ack-eliciting probe packets. Prefer the oldest unresolved head-of-stream range, handshake/refresh progress, or PING. Probe packets may exceed cwnd by at most the two-probe allowance for that expiration, but count fully in bytes_in_flight, obey amplification limits, and are paced. A PTO does not automatically mark every outstanding packet lost or collapse cwnd.

An ACK/credit-only control exchange can otherwise deadlock when all application bytes are acknowledged and a MAX_DATA packet is lost. Such reliable control packets are ack-eliciting and arm PTO even without application data.

### 14.5 Retransmission selection

ACK, PADDING, old packet headers, and old ciphertext are never retransmitted as logical payload. PING is regenerated as needed. STREAM payload storage remains available until its logical bytes are acknowledged, abandoned by reset/close, and unpinned from DMA. Multiple packets carrying the same bytes share logical ownership and cannot cause duplicate completion.

The mandatory baseline priority is: handshake/key/path progress; ready ACK; retransmittable credit/control; lost stream ranges starting at the lowest unresolved offset in their stream; new stream data; unreliable datagrams. Fairness rules in Section 16 prevent endless control or retransmission traffic from monopolizing all service. No block-oriented FEC wait, fixed batch-fill delay, or Nagle-style small-write hold is introduced by the protocol.

<a id="section-15"></a>

## 15. Congestion control and ECN

### 15.1 Mandatory baseline

The interoperable baseline is a paced NewReno-style controller. Each active path owns its congestion state. For current maximum datagram size M:

```
initial_cwnd = min(10*M, max(2*M, 14720))
minimum_cwnd = 2*M
ssthresh = 2^62-1
bytes_in_flight = 0
```

All ack-eliciting packets and all packets containing PADDING count as in-flight until acknowledged or lost. Pure ACK/CLOSE packets without PADDING do not; minimum plaintext can instead be met by the size of ACK or CLOSE itself. MTU probes are charged. Unreliable DATAGRAM packets are charged. Retransmissions are charged. Crypto-control frames are charged. Padding cannot create unaccounted bandwidth.

Before normal emission, `bytes_in_flight + reserved_in_flight + packet_size <= cwnd`. At most the explicit PTO probes bypass this inequality. Reserved budget is released on cancellation; emitted budget transfers atomically into bytes_in_flight. The sender's local sent-table capacity is a separate admission gate even when cwnd permits more bytes.

An ACK contributes congestion growth only for newly acknowledged, previously in-flight bytes sent after the recovery boundary and while the sender was not application- or flow-control-limited. ACK-only packets cannot inflate cwnd.

In slow start, add the eligible newly acknowledged bytes to cwnd. In congestion avoidance, accumulate eligible bytes in `ca_accumulator`; while it is at least the current cwnd, subtract current cwnd and increase cwnd by M. The per-event work is bounded by the finite acknowledged ledger; a hardware implementation may use an equivalent checked quotient/remainder operation. Packet splitting cannot multiply the growth.

On a new loss or validated CE congestion event outside the current recovery interval, set `ssthresh=max(floor(cwnd/2),2*M)`, `cwnd=ssthresh`, and `recovery_boundary=largest emitted path ordinal`. Further events for packets at or before that boundary do not halve again. An ACK of an ack-eliciting packet beyond the boundary exits RECOVERY into congestion avoidance. A single ACK containing several lost-packet discoveries constitutes one recovery event.

### 15.2 Persistent congestion

After at least one valid RTT sample, persistent congestion exists when two ack-eliciting packets on the same path are declared lost, their send-time separation exceeds `3*base_pto`, and every ack-eliciting transmission between them is declared lost, with no intervening acknowledged transmission. A merely outstanding unknown packet does not satisfy this condition.

Set cwnd to minimum_cwnd and reenter slow start, with a finite ssthresh retained from the congestion event. This decision depends on a loss interval, not simply on counting PTO expirations.

### 15.3 Pacing

Use a two-datagram token bucket. The token rate is `5*cwnd / (4*max(srtt,50))` bytes per microsecond. Implementations use fixed-point arithmetic with at least 32 fractional bits or a mathematically equivalent deadline scheduler. Idle time can accumulate at most two M of credit. Because the bucket capacity is capped at `2*M` while an `MTU_PROBE` target during binary search (Section 17.2) may exceed `2*M` (for example, probing 2648 or 4096 bytes while `M=1200`), an `MTU_PROBE` datagram reserves and spends `min(target_size, 2*M)` from the pacing bucket; at most one `MTU_PROBE` may be outstanding on a path at any time, and its full `target_size` is still charged against `bytes_in_flight` and anti-amplification limits. A plain UDP_SEGMENT operation does not provide per-child pacing: the sum of the children immediately released must fit these available tokens. The 64-child software batch bound does not enlarge this two-datagram burst bucket. Larger staged batches require a verified downstream scheduler that enforces the same per-child release envelope; setting one skb transmit timestamp or enabling GSO alone is not sufficient. Section 30.5 defines both cases. Timers must not cause a burst equal to the whole congestion window after a delayed interrupt.

Pure ACK and CLOSE packets may bypass the cwnd gate and pacing bucket, but not amplification checks, per-peer control rate limits, or egress availability. A receiver must send a due ACK at the next available permitted opportunity. PACKET ACK delay due to CPU scheduling should not be hidden by a false small ack_delay value.

The low-latency hardware profile uses scheduling granularity at most 50 microseconds and max_ack_delay of 1000 microseconds or less. A software implementation may use a coarser scheduling implementation, but cannot claim conformance to that latency profile. There is no universal guaranteed RTT or throughput.

### 15.4 ECN

If both endpoints negotiate ECN, the initial path may test ECT(0) on its first ten ack-eliciting DATA packets. GRT never sends ECT(1). Receivers report cumulative counts of distinct committed DATA packets by actual received ECN marking. Duplicate packets do not increment counters.

The sender retains cumulative numbers sent ECT(0), newly acknowledged ECT(0), and the latest accepted cumulative receiver counters. For a newer largest-ACK report, counters must not decrease; ect1 must remain zero; `ect0+ce` must be at least total newly acknowledged ECT(0) packets and no greater than total ECT(0) packets emitted. These checks permit counters to include received packets omitted from ACK ranges.

After three distinct marked packets have been acknowledged with consistent feedback, ECN becomes validated. Missing, impossible, or inconsistent feedback disables ECN on that path; send Not-ECT thereafter and continue loss-based control. A report with an older largest acknowledged packet cannot roll counters back. A validated CE increase causes a congestion event, with the usual once-per-recovery boundary rule. It is not a reliable delivery acknowledgment for packets absent from ACK ranges.

On any migration or detected NAT-tuple change, this version disables sender ECN for the remainder of the connection. Cumulative receive counters continue to be reported, but old-path CE feedback cannot change the new path's cwnd. This intentionally avoids multi-path ECN attribution state; it is a documented capability restriction, not a claim of full ECN optimization across migration.

<a id="section-16"></a>

## 16. Application scheduling and cancellation

Streams are independent scheduling entities. The baseline uses deficit round robin over eligible stream send halves with quantum `2*M` and equal weight. A deployment MAY use local application priorities, but each continuously eligible nonblocked stream MUST receive service eventually. A priority system cannot claim transport-level avoidance of stream starvation unless it supplies a finite service bound.

Control traffic is limited to one full control-only DATA packet per four emitted DATA packets while application traffic is eligible, except ACK deadlines, handshake completion, active path validation, key-commit progress, PTO probes, and CLOSE. Lost control items are coalesced by their semantic identity; MAX_DATA does not create an unbounded queue of obsolete values. DATAGRAM shares the same connection/path congestion budget and is scheduled after ready reliable work by default; applications can assign it a finite weighted share.

The application API has OPEN, WRITE, FIN, RESET, STOP_RECV, SEND_DATAGRAM, CONSUME, CLOSE, and optional MIGRATE, REQUEST_PARK, WAKE, and NETWORK_EVENT. Mobile commands obey Section 35 and remain separate from wire authentication. WRITE acceptance transfers immutable-buffer ownership or copies the bytes; a buffer cannot be modified while referenced by the transport. WRITE acceptance is not network delivery. FIN freezes a half's final size. CONSUME alone returns ordered receive capacity. SEND_DATAGRAM may be dropped before emission if its local deadline expires; no deadline can silently abandon reliable STREAM bytes.

A cancellation or connection close completes every application operation exactly once with a local result. A transport ACK does not mean the remote application read the data, committed a transaction, or survived a subsequent crash.

<a id="section-17"></a>

## 17. Path validation, mobile migration, and PMTU

### 17.1 Path identity, limits, and authority

A path is the exact pair of local and peer IP/UDP endpoints plus a local interface/network-handle generation and routing/security context. IPv4 and IPv6 paths are distinct. An IPv4-mapped address is normalized before lookup. NAT64, VPN replacement, a new cellular attachment, and a changed local interface generation must not inherit reachability, PMTU, RTT, or congestion state merely because an application reports the same network name.

There is one application-sending ACTIVE path, at most one CANDIDATE, and at most one retained OLD path. Candidate and old records are allocated from separately capped pools only while needed. This is make-before-break migration, not simultaneous bulk multipath or link aggregation. Probes/control may use a candidate while reliable application traffic continues on ACTIVE. After local cutover, new application packets use only the new ACTIVE path; already submitted old-path ciphertext may still drain.

The path state alphabet is FREE, VALIDATING, REACHABLE, READY, ACTIVE, OLD, FAILED. REACHABLE means return routability has been established but the 1200-byte bidirectional base-size test is incomplete. READY satisfies both. State and allocation are independent: a FAILED record with I/O pins cannot yet become FREE. A matching path-generation check is mandatory for every response, timeout, crypto job, GSO operation, and DMA completion.

Explicit migration requires both MIGRATION flags. Fresh authenticated NAT rebinding must still be handled when an application disables elective migration, but the peer does not gain an arbitrary destination-advertisement facility. This version carries no network address in PATH_INTENT or PATH_SELECTED. An observed source address is only a **candidate**, never an authenticated instruction to send bulk traffic there. A server-address relocation not reachable through the existing connection requires independently authorized service discovery or a new connection; simultaneous loss of both peers' last reachable addresses cannot be repaired without rendezvous.

An application/OS event may authorize a new local interface to the previously authenticated service destination. This is local routing authority, not peer address validation. It may create a bounded locally initiated probe allowance under Section 37.4. A received packet, PATH_INTENT, DNS answer, or asserted OLD_UNUSABLE flag cannot create that allowance. Public adapters reject multicast, broadcast, unspecified destinations and unauthorized loopback, link-local, private, or metadata-service addresses according to a locally provisioned destination policy. Authorized private deployments can use private addresses explicitly.

### 17.2 PMTU and base-size validation

A usable path must carry 1200-byte UDP payloads without IP fragmentation. IPv4 sends set Don't Fragment; IPv6 senders do not add Fragment headers. A backend that cannot enforce the necessary fragmentation/PMTU contract must use a conforming fallback or report UNSUPPORTED_PATH. No silent 576-byte transport mode exists. Incoming fragmented traffic may be rejected by deployment policy.

PATH_CHALLENGE and PATH_RESPONSE packets are padded to 1200 bytes when the applicable amplification/probe and pacing allowance permits. A PATH_RESPONSE proves return routability only if its unpredictable token is outstanding for that exact path/generation. READY additionally requires that the matching challenge was emitted in a datagram at least 1200 bytes long and the matching response arrived in a datagram at least 1200 bytes long. A smaller permitted response can establish REACHABLE, not READY. Send another full-sized challenge after budget becomes available. A tiny challenge/response exchange cannot validate the base MTU. An initial completed handshake provides base-size evidence from its 1200-byte flights.

Each path has `pmtu_generation:u32`. Any decrease or increase increments it. Not-yet-submitted packets/batches using an old generation are canceled or rebuilt before submission, with fresh PNs for any new seal. Already submitted bytes are immutable and retain their original path association. A changed default route cannot silently reroute an owned GSO batch to another path while retaining its old congestion and amplification accounting. The adapter must bind egress or reject that submission.

Start M=1200. The upper bound is the minimum of 4096, the peer's receive maximum, and the local interface's UDP ceiling. A probe uses MTU_PROBE plus optional PING/PADDING, no reliable application bytes. Its explicit authenticated ACK confirms that complete size, while its pacing token cost is clamped to `min(target_size, 2*M)` so binary search targets above `2*M` do not deadlock against the `2*M` pacing bucket ceiling (Section 15.3). Validated ICMP Packet Too Big may lower a tentative ceiling, never raise it; match quoted tuple, packet context, and interface generation before using it. Unauthenticated ICMP alone does not close the connection or prove link failure.

Choose probe targets on eight-byte boundaries, taking the greatest multiple of eight at or below the search midpoint. Test a failed target at most three times with at least `max(1 second,10*srtt)` spacing. Only after a current/base-size packet succeeds on the same path can a larger failure be classified as MTU-specific rather than ordinary congestion loss. Three PTOs without successful large traffic trigger two charged 1200-byte confirmation probes. If base probes succeed and large packets fail, reduce M to 1200 and resegment live bytes. If the base also fails, use path-failure recovery. A new path never inherits the old path's successful large-MTU evidence [R15].

### 17.3 Challenge transaction and anti-amplification

PATH_CHALLENGE and PATH_RESPONSE each contain exactly 16 bytes. A validation transaction has one unpredictable challenge token, a path generation, first-send time, expiry, and an attempt counter. Retries reuse the **logical challenge token** but send fresh packet numbers/ciphertext. Keeping that token stable within a bounded transaction avoids invalidating a legitimate reordered response on every timeout. A new transaction/generation gets a fresh token. At most one local challenge is outstanding per candidate; challenge tokens are never looked up without connection and path scope.

Initialize `validation_pto=max(100000, estimated_srtt+max(4*estimated_rttvar,50000))` microseconds. With no defensible estimate use INITIAL_RTT_US and half of it for variance. A previous-path estimate may bound scheduling conservatively but is not installed as a measured new-path RTT. The first probe is immediate when gates permit; retry after validation_pto and then twice that interval, for at most three challenge transmissions in total. The transaction deadline is the earlier of the negotiated maximum validation interval and the first send plus seven validation_pto. Expiry never extends on duplicates. A failed candidate does not close a healthy ACTIVE path.

A responder echoes a valid challenge on the **same receiving path**, not the default active route. It may coalesce its own challenge and that response in one padded datagram. At most one response is generated for a given token in one scheduling opportunity. Repeated fresh authenticated requests remain bounded by path/global control buckets; they cannot force one allocation or one immediate output per input frame. A wrong-token, wrong-tuple, expired, old-generation, or replayed response does not validate, reset a timer, or promote anything.

For peer-discovered candidates, only a successfully committed, fresh authenticated DATA packet adds received-byte/packet amplification credit. Candidate creation and this credit are part of that packet's transaction. A pre-auth replay probe is read-only; a malformed authenticated packet that does not commit creates no fresh credit. No old-path budget, token, CID, ticket, or shared traffic key transfers to a different tuple. Until return routability is proven, both the three-times byte and three-times packet guards apply, including ACK, CLOSE, retries, padding and coalesced frames. Before READY, only bounded validation/ACK/key-progress/mobile-control frames may be emitted on the path, never STREAM/DATAGRAM payload.

Receiving application bytes on an unvalidated candidate is not by itself prohibited: a fresh fully authenticated packet can be retained transactionally under existing stream credit, without sending bulk data back or selecting the path. This accommodates rebinding detected from ordinary traffic. The receiver still requires allocation admission and must drop the whole transaction without ACK if it cannot retain its reliable data.

### 17.4 Explicit migration intent and ordering barrier

PATH_INTENT (0x0028) has an exact 24-byte body:

```
migration_seq:u64 || cutover_pn:u64 || intent_flags:u32 || zero:u32
```

Sequence numbers are independent per sender, begin at 1, strictly increase for a new requested path, and never wrap. Flag bit 0 is SELECT; bit 1 is OLD_UNUSABLE, an advisory hint only. Other bits and zero are zero. A probe-only intent has SELECT=0 and cutover_pn=0. A SELECT intent has a nonzero cutover_pn not greater than its carrying packet number. For a given sequence, SELECT can change only from 0 to 1. Once SELECT=1 has been transmitted, its cutover_pn and flags are immutable across retries. A contradictory authenticated repeated intent is PATH_STATE_ERROR. Older completed sequences are ignored, with a cached current result regenerated when appropriate; they never roll routing backward.

Before constructing SELECT, the sender freezes **new** old-path application handoffs and cancels all not-yet-submitted old-path application packets. It sets cutover_pn to next_packet_number **after** every previously allocated old-path packet number, then allocates the SELECT packet on the candidate. Existing SUBMITTED packets may complete normally and retain their old attribution. Any newly sealed application packet after this barrier has PN at least cutover_pn and is scheduled only on the selected path once locally READY. A retransmitted SELECT retains its original cutoff but uses a fresh PN. GSO parent completion cannot invalidate this ordering barrier.

PATH_SELECTED (0x0029) has an exact 16-byte body:

```
migration_seq:u64 || status:u16 || zero:u16 || zero:u32
```

Status 0 ACCEPTED, 1 DECLINED, or 2 BUSY is allowed. ACCEPTED is sent on the selected path only after the receiver has made it READY and atomically selected it. DECLINED/BUSY do not validate a destination or revoke an existing active path. A repeated matching SELECT regenerates the current result without a second promotion. PATH_SELECTED is a regenerated control result, not proof that the peer application consumed data; ACCEPTED is reliable until packet ACK or a newer accepted intent supersedes it. The sender repeats SELECT until an ACCEPTED result or its bounded migration deadline; a packet ACK alone is not acceptance of a path-selection request.

The receiver associates an intent with its authenticated **ingress path** and stores it in that path's bounded record. It does not use cutover_pn as an ACK, replay-window advance, or memory-allocation size. An ACCEPTED sequence installs a `peer_cutover_floor` and pins the selected tuple/generation. Packets on OLD may still resolve data/ACK obligations, but never automatically select OLD again, even with a newer PN. Returning deliberately to OLD requires a newer intent and revalidation or an explicitly authorized hard-failure fallback. An unfamiliar tuple whose only evidence predates peer_cutover_floor cannot auto-promote. This barrier limits reversal by late pre-cutover packets without assuming that kernel completion order equals wire order.

Implicit NAT rebinding has no mandatory intent because the sender might not know its external tuple changed. A fresh non-probing packet on an unfamiliar tuple can begin validation; automatic selection then requires a response on that tuple and fresh evidence newer than both the active-path watermark at candidate creation and peer_cutover_floor. Probing-only packets (ACK, PADDING, PATH_CHALLENGE/RESPONSE, probe-only PATH_INTENT with SELECT=0, NEW_CID, RETIRE_CID, NEW_TOKEN, and mobile lease control) do not by themselves request implicit promotion. A PING is non-probing for this rebinding decision, so an idle keepalive can discover and repair NAT rebinding without requiring application bytes. A known OLD tuple is never auto-promoted by this rule. An endpoint should send an explicit intent on an OS-reported local interface change rather than wait for an implicit heuristic.

### 17.5 Make-before-break, hard handoff, and rollback

The following loss-free exchange illustrates elective handoff; every arrow is independently protected and charged. Application data can continue on Wi-Fi until the cutover barrier:

```
Client cellular candidate                          Server
  PATH_INTENT(seq,SELECT=0) + CHALLENGE(C) [1200] -->
                         <-- RESPONSE(C) + CHALLENGE(S) [1200]
  [candidate locally READY; freeze Wi-Fi submissions]
  RESPONSE(S) + PATH_INTENT(seq,SELECT=1,cutoff) [1200] -->
  [new application sends may use cellular]          [validate/select]
                         <-- PATH_SELECTED(seq,ACCEPTED) [+ ACK]
```

This does not require a fresh identity handshake or new stream IDs. The client can switch after its round-trip validation; server return traffic switches after its own response proof. Lost control is retried with fresh PNs, and stream retransmission remains byte-based. Prevalidation before Wi-Fi disappears can hide much of this work. No protocol can guarantee zero outage if both links are unavailable, the OS suspends execution, or all validation packets are lost.

For a hard local link loss, stop new handoffs to the lost interface immediately; release only not-yet-submitted reservations, not kernel/NIC-owned buffers. Promote an already READY authorized candidate if available. Otherwise create one candidate, probe immediately under its bounded allowance, and retain application/session state while validating. OLD_UNUSABLE can expedite local policy but never bypass validation, PMTU, amplification, or congestion. Do not continue a bulk retransmission storm to a dead interface. Already submitted packets may be acknowledged later and are resolved exactly once.

Promotion is a transaction: commit the selected path/generation; install fresh PMTU/congestion/RTT state; change the scheduler route; publish a PATH_CHANGED event; leave global PN, replay history, stream offsets, cipher, flow limits, and reliable obligations unchanged. It is independent of key refresh, which can proceed using whichever valid path is currently selected.

After promotion, retain OLD for `min(3*old_base_pto,10 seconds)` for late ACK attribution and possible controlled fallback. Live old-path packets can be administratively detached once their unresolved logical items have been queued on the new scheduler. Detachment removes old counted flight bytes once, marks the transmission ABANDONED locally, does not grow/halve the new cwnd, and never resolves the underlying reliable item. A small existing live record may still process a late ACK; after its verification history is gone an unverifiable ACK is ignored. Physical I/O pins remain until actual completion. ACKs crossing paths do not provide new-path RTT or congestion growth.

A second handoff cannot create a fourth path record. If OLD still has live references, detach bookkeeping as above and retain only exact operation tombstones until fences, or reject/defer the new candidate. Tombstones belong to a globally bounded operation pool and must not be disguised as unlimited additional paths. A failed elective candidate returns to the original ACTIVE path without reversing application delivery. A failed post-cutover path can fall back only to a still-authorized retained path, with a **new** intent/validation as needed; it does not reset stream offsets or restore an old packet counter.

### 17.6 Congestion continuity and anti-flapping

Every newly selected path starts at M=1200 and initial_cwnd, with a fresh measured RTT estimator. The full-sized challenge exchange may initialize a conservative round-trip sample without subtracting application ACK delay. Previously observed high bandwidth is not inherited from a different network. A recently retained tuple restores its previous congestion ceiling rather than granting repeated fresh initial windows. Migration-crossing ACKs may resolve bytes but cannot increase the new path's cwnd. ECN is disabled for the rest of the connection on a tuple change under Section 15.4.

Routine elective promotions should be separated by at least `max(one current base PTO,1 second)` and a local hysteresis decision. A hard local failure may override hysteresis, not security gates. Independently, the connection owns a **migration startup bucket** that persists across every path switch: capacity 12000 bytes, refill 12000 bytes/second, initially full. Until a new path has one valid same-path application/probe RTT sample and has remained ACTIVE for one current srtt, every application/PTO byte sent on it also consumes this bucket. Thereafter ordinary congestion/pacing controls apply. Repeated candidate promotion cannot mint free initial-window bursts. A local implementation may lower this startup rate or use a stricter verified aggregate policy, but may not remove the bounded cross-migration startup control.

Locally initiated candidate validation uses a separate persistent connection probe bucket: capacity three 1200-byte datagrams, refill one such datagram/second. Per-interface and global probe buckets also apply. Peer-requested validation consumes committed input credit and a separate global control budget. The two buckets do not refill on candidate deletion, CID rotation, key refresh, or interface flapping. Link metrics and policy hints are local input, never authenticated claims about network capacity.

### 17.7 Path events and total component transitions

| State | Event and guard | Atomic actions | Next |
|---|---|---|---|
| FREE | Authorized local candidate; pool/probe allowance available | Bind exact route/generation; create one challenge | VALIDATING |
| FREE | Fresh authenticated unfamiliar ingress; admission/transaction succeeds | Create candidate and add its committed credit | VALIDATING |
| VALIDATING | Matching small response | Mark return routability; keep base-size test pending | REACHABLE |
| VALIDATING / REACHABLE | Matching response with both 1200-byte proofs | Record proof and bounded RTT sample | READY |
| VALIDATING / REACHABLE | Retry due; attempts <3 and deadline not expired | Send same logical challenge under fresh PN and all gates | Same |
| VALIDATING / REACHABLE | Retry/deadline exhausted | Cancel unsent jobs, retain owned-operation fences | FAILED |
| READY | Local selection or valid peer SELECT/implicit-rebinding ordering guard | Promote; old ACTIVE becomes OLD; no stream/key reset | ACTIVE |
| READY | No selection before candidate deadline or policy declines | Release unneeded candidate after fences | FAILED |
| ACTIVE | Another READY path selected | Stop new app handoffs; retain attribution | OLD |
| ACTIVE / OLD | Parked-path wake or resume requires fresh proof; app gates closed | Increment validation generation; clear reachability/base-size proof, reset PMTU and unvalidated credit; allocate bounded challenge state | VALIDATING |
| ACTIVE | Verified local interface loss | Stop new handoffs; begin bounded recovery | FAILED |
| OLD | Retention expires / controlled eviction | Detach old flight, requeue unresolved items, fence operations | FAILED |
| FAILED | All path references fenced and no protected validation obligation | Release record; increment allocation generation on reuse | FREE |
| Any | Wrong response, stale callback, lower completed intent | No promotion, timer extension, or budget grant | Same |

A failed guard does not execute the success action. Resource exhaustion drops an uncommitted packet or returns local BUSY; it does not forge a peer protocol error. Contradictory authenticated frame encodings use PATH_STATE_ERROR. Mobile availability/parking states in Section 35 are orthogonal to this path FSM.

<a id="section-18"></a>

## 18. Connection IDs and stateless reset

C chooses a nonzero random client initial CID and a nonzero random original destination CID. S chooses a distinct nonzero server initial CID for the connection and binds it in its response. After establishment, each packet's destination CID is one issued by its receiver. CID values are opaque; their bit pattern must not disclose a long-term identity. A deployment-specific load-balancer encoding must retain adequate unpredictability and privacy.

Each endpoint begins its issued-CID sequence at zero. It may issue no more live CIDs than the peer advertises in active_cid_limit, between two and four; the receiving table is sized/admitted accordingly. Keep one spare usable CID for elective handoff where possible, and replace it only within the bound. NEW_CID sequences strictly increase; duplicate sequences require byte-identical CID and token fields. retire_prior_to cannot exceed sequence. A receiver stores at most its advertised active_cid_limit peer-issued CIDs, never more than four. RETIRE_CID cannot retire the only usable CID until a replacement has been supplied. Sequence reuse with different data, CID reuse across active connections, or a retirement request for a never-issued sequence is CONNECTION_ID_ERROR.

CID rotation does not reset packet numbers, stream state, replay history, or congestion state. Explicit migration SHOULD select a fresh peer-issued CID if available. The absence of a spare cannot force packet-counter reuse or erase the old CID before owned packets and permitted late traffic are fenced; an emergency handoff may reuse the current CID with its disclosed linkability cost. No CID scheme can conceal a migration from an observer that already correlates both paths through timing or application identifiers.

A stateless reset token is a nonzero 16-byte secret delivered only in an authenticated encrypted message. A server may derive it as the first 16 bytes of HMAC-SHA256 under a durable reset secret over `ASCII("GRT11 reset") || cid`. Derivation key persistence and rotation must be coordinated with live CIDs. The token for the client's initial CID is delivered in FINISH, not in the plaintext client flight.

For an unknown, locally recognizable retired CID in a plausible DATA datagram of at least 65 bytes, a server MAY emit one rate-limited reset datagram of 32–64 bytes, always shorter than the triggering datagram. It consists of unpredictable bytes ending in the token. Its first two bytes MUST differ from MAGIC so a reset cannot itself trigger another reset response. Sending resets for arbitrary unrecognized random CIDs is not required.

On a failed normal parse/decryption, a receiver may recognize a reset by a constant-time match of the final 16 bytes against a live peer-issued reset token. This check also covers reset datagrams without MAGIC. A matching reset transitions directly to DRAINING without a reply. Unknown tokens, short datagrams, and ordinary decryption failures do not close a connection. Token lookup and reset generation are rate-limited independently of the application packet path.

<a id="section-19"></a>

## 19. Fresh hybrid key refresh

### 19.1 Motivation and ownership

Packet keys are stable within an epoch and permit arbitrary out-of-order decryption within the replay window. Refresh is a bounded control transaction; it is not a per-message Double Ratchet and is not driven by receiving the previous data packet.

Only the network client initiates refresh. The server requests it using KEY_REQUEST. This eliminates simultaneous-offer resolution. There is at most one active transaction. At most three epoch slots exist: PREVIOUS, CURRENT, and STAGED. A second refresh cannot start until the previous transaction is confirmed and the PREVIOUS slot is erased.

A client begins refresh at any soft count/byte/time threshold, or on an effective KEY_REQUEST. A server sends KEY_REQUEST before its own directional threshold, allowing at least the soft-to-hard budget margin. Time alone does not force an idle connection to send periodic packets; it refreshes before resuming sustained application transmission. Hard packet/byte limits apply even when the peer does not cooperate. On exhaustion, stop sealing under that key and fail the connection rather than reuse or overrun it.

### 19.2 Reliable refresh fragments

CRYPTO_FRAGMENT body is:

```
refresh_id:16 || message_kind:u8 || reserved:3
|| total_length:u32 || message_hash:32
|| fragment_index:u16 || fragment_count:u16 || fragment_data
```

The prefix is 60 bytes. Kinds are 1 OFFER and 2 ANSWER. Refresh chunks are exactly 512 bytes except the last; at most eight chunks are allowed. The total length is 1..4096, fragment_count is the ceiling division by 512, and fragments carry no additional padding inside fragment_data. Frame-level alignment is external. The containing DATA packet provides authentication.

A receiver acknowledges a CRYPTO_FRAGMENT packet only after retaining its message bytes and fragment bitmap or recognizing an identical committed duplicate. The sender retains message bytes until a valid semantic response proves receipt of the whole offer/answer, not merely until unrelated data is acknowledged. If a receiver cannot retain the bounded object, it drops the packet without ACK or closes for a negotiated-limit violation; it cannot ACK and later forget an unfinished refresh object.

### 19.3 Offer, answer, and derivation

C creates new X25519 pair X_C, new ML-KEM-768 pair Q_C, refresh_id, and client refresh nonce N_C. The OFFER is:

```
new_epoch:u32 || selected_cipher:u16 || reserved:u16 (=0) || refresh_id:16
|| N_C:32 || X_C:32 || Q_C:1184
```

Length is 1272 bytes. new_epoch must equal CURRENT.epoch+1 without overflow. selected_cipher must equal the immutable connection cipher; refresh cannot renegotiate it. The OFFER is sent under CURRENT. S validates it, creates X_S and N_S, encapsulates `(CT_R,SS_R)` to Q_C, and computes `D_R=X25519(X_S.private,X_C)`.

The ANSWER prefix `AP` is:

```
new_epoch:u32 || selected_cipher:u16 || reserved:u16 (=0) || refresh_id:16
|| N_S:32 || X_S:32 || CT_R:1088
```

Length is 1176 bytes. Define:

```
RT = H(ASCII("GRT11 refresh") || T1 || OFFER || AP)
RPRK = Extract(refresh_secret[current], D_R || SS_R)
ROOTnew = L(RPRK, "refreshed root", RT, 32)
rk = L(ROOTnew, "refresh confirmation", RT, 32)
answer_mac = HMAC(rk, ASCII("answer") || RT)
ANSWER = AP || answer_mac                 # 1208 bytes
commit_mac = HMAC(rk, ASCII("commit") || RT)
confirm_mac = HMAC(rk, ASCII("confirm") || RT)
```

The answer is sent under the old CURRENT key and cached unchanged. S installs the new directional packet keys as STAGED before sending it. C reconstructs D_R and SS_R, verifies answer_mac, installs STAGED, then erases its fresh private inputs after derivation. Both OFFER and ANSWER must carry the connection-selected cipher, zero reserved bits, and the exact expected epoch/refresh ID; mismatches are KEY_UPDATE_ERROR. Epoch packet keys use KCTX(new_epoch)=T1||u16(selected_cipher)||u32(new_epoch), as in Section 8.4; ROOTnew already binds RT.

An old traffic-key compromise alone does not reveal future fresh DH/KEM secrets to a passive recorder. An attacker who can actively use compromised state may still impersonate and interfere with refresh; this is not unconditional post-compromise healing. Refresh-secret and traffic-key separation must be maintained in storage and KDF usage.

### 19.4 Commit protocol and reordering

C sends KEY_COMMIT under the STAGED epoch, with the exact new epoch, refresh ID, and commit MAC. Every packet C sends under STAGED before confirmation MUST contain KEY_COMMIT; its only additional frames may be ACK, PING, and PADDING. Other application traffic continues under old CURRENT while it is within its limits.

S accepts a STAGED packet only if it contains a valid KEY_COMMIT; other pre-commit STAGED packets are dropped without ACK. On commit, S atomically makes STAGED current for receive and send, makes old CURRENT previous, records the completed transaction, and sends KEY_CONFIRM under the new epoch. S can coalesce application data with KEY_CONFIRM.

C accepts a new-epoch packet during COMMITTING only if it contains valid KEY_CONFIRM. On that commit, it promotes STAGED to CURRENT and old CURRENT to PREVIOUS. S MUST include KEY_CONFIRM in every application-data packet under the new epoch until a KEY_CONFIRM-carrying packet is acknowledged. C retransmits KEY_COMMIT using fresh global packet numbers if necessary. Duplicate valid commits receive the cached logical confirmation, even after S has already promoted.

Late old-epoch DATA packets can still be accepted using PREVIOUS keys for the grace interval; they do not revert CURRENT. ACK ranges are connection-wide and can acknowledge old-epoch transmissions. There is no packet-number reset and no per-epoch ACK space.

### 19.5 Erasure and failure

PREVIOUS keys are retained for three PTOs measured at promotion, then erased after in-flight crypto/DMA jobs referencing them complete or are canceled behind a fence. Remaining old-epoch transmissions are declared lost once and their logical items are requeued under the current epoch. Retaining a packet's logical data does not require retaining its old encryption key.

Offer/answer/commit retransmission uses the existing recovery engine. An uncompleted refresh has a deadline of max(10 seconds, six current base PTOs). Expiry, inability to stage keys, or impending hard key-limit exhaustion closes the connection with KEY_UPDATE_ERROR or AEAD_LIMIT_REACHED. Unauthenticated garbage, unknown epochs, or stale callback IDs never install or erase keys.

After completion, cache only the refresh ID, epoch, offer/answer hashes, and confirmation values needed for idempotent duplicate handling. Erase old roots, old refresh secrets, temporary DH/KEM secrets, and completed private refresh keys after the grace/fence requirements. Exporter and resumption masters are separate secrets; their retention policies are explicit and are not claimed to refresh automatically with every packet epoch.

<a id="section-20"></a>

## 20. Connection lifecycle and total transition rules

### 20.1 State alphabet

Connection states are NEW, C_BUILD, C_WAIT_REPLY, C_VERIFY, C_FINISHING, S_COLLECT, S_BUILD, S_FINISHING, ESTABLISHED, CLOSING, DRAINING, QUIESCING, and DEAD. Role is immutable. A listener is a separate admission object, not one shared mutable connection context.

Timers carry `(connection_slot, connection_generation, timer_kind, timer_generation)`. Cryptographic jobs additionally carry attempt ID or refresh ID and key-slot generation. A callback whose token does not equal the current owner is STALE and has no semantic effect.

### 20.2 Global rules, applied before specific transitions

The following rules apply in the order shown and make the transition relation total:

1. A stale job, descriptor, application handle, or timer is ignored, apart from releasing resources still owned by that exact stale job.
2. In DEAD, semantic events are ignored; exact owned late cleanup, if any, cannot refer to a reused slot. In QUIESCING and DRAINING, matching I/O-result, buffer-release and fence completions still discharge ownership; no new packet is handed to a backend. Already submitted operations may complete or drain and are never treated as cancellable merely by changing the connection state. DRAINING also handles its drain deadline and fault cleanup. QUIESCING cannot become DEAD before all these ownership fences complete.
3. A local fatal hardware fault stops emission and enters QUIESCING. Application operations complete with failure once; affected contexts are never reused before their fences.
4. A valid peer CLOSE or valid stateless reset in a key-bearing live state enters DRAINING. An authenticated protocol violation enters CLOSING when a safe sending key remains, otherwise QUIESCING.
5. APP_CLOSE enters CLOSING in a key-bearing live state and QUIESCING otherwise. An establishment deadline before secure progress enters QUIESCING; if keys are installed but ESTABLISHED has not been reached, the same deadline enters CLOSING instead. Idle expiry enters QUIESCING silently unless an effective granted parking lease explicitly replaces that idle deadline under Section 35; lease expiry itself enters QUIESCING and cannot be extended by duplicate packets.
6. Events in the specific table below take their described transition. If a guard is false, follow the explicit guard-failure behavior in the owning section; never execute its success action.
7. An otherwise admissible duplicate is idempotent. An otherwise unauthorized application command returns a local INVALID_STATE without changing the network state. Unauthenticated/unexpected bootstrap input is dropped. An authenticated frame illegal in the current state is dropped when explicitly allowed below, otherwise closes with PROTOCOL_VIOLATION. Remaining timer/scheduler events leave the connection state unchanged and act only on their owning component FSM.

A total mapping is not permission to collapse distinct guard failures into one fatal branch: resource unavailability, failed authentication, stale callbacks, and authenticated protocol errors have different outcomes.

### 20.3 Specific connection transitions

| Current | Event and guard | Atomic actions | Next |
|---|---|---|---|
| NEW, client | APP_CONNECT; local admission succeeds | Assign generation/attempt/CIDs; start 10-second deadline; submit prekey work | C_BUILD |
| NEW, server | Admitted CLIENT_FLIGHT fragment | Allocate bounded reassembly slot; record source/correlation; start deadline | S_COLLECT |
| C_BUILD | PREKEY_JOB_OK; generation matches | Construct and cache immutable client flight; schedule initial fragments | C_WAIT_REPLY |
| C_BUILD | PREKEY_JOB_FAIL | Fail connect; cancel owned jobs | QUIESCING |
| C_WAIT_REPLY | Matching RETRY; no Retry previously accepted | Store route/token; resend same message bytes | C_WAIT_REPLY |
| C_WAIT_REPLY | Complete matching server flight | Pin reply bytes; submit verification job | C_VERIFY |
| C_WAIT_REPLY | FLIGHT_TIMEOUT; before deadline | Reemit cached flight subject to backoff/admission | C_WAIT_REPLY |
| C_VERIFY | SERVER_VERIFY_OK | Install epoch zero, transcript, peer parameters; erase one-time private keys; queue FINISH; signal SECURE_READY | C_FINISHING |
| C_VERIFY | SERVER_VERIFY_FAIL | Discard forged/bad reply; retain own attempt until deadline | C_WAIT_REPLY |
| S_COLLECT | Complete client flight; hash valid | Pin bytes; run admission/trust/ticket/PQ checks | S_BUILD |
| S_COLLECT | Bad/inconsistent unauthenticated fragment | Discard that reassembly object; do not affect other contexts | QUIESCING |
| S_BUILD | CLIENT_JOB_OK | Atomically commit any permitted early batch; cache server flight; install epoch zero; schedule reply | S_FINISHING |
| S_BUILD | CLIENT_JOB_FAIL | Erase temporary keys and fail attempt without an unauthenticated alert | QUIESCING |
| S_FINISHING | Matching repeated client flight | Reemit cached server flight within budgets; never regenerate it | S_FINISHING |
| S_FINISHING | DATA transaction with valid FINISH | Validate source; commit FINISH and following frames; queue HANDSHAKE_DONE; signal CONNECTED | ESTABLISHED |
| S_FINISHING | DATA lacking FINISH | Drop without semantic commit or ACK | S_FINISHING |
| C_FINISHING | Valid HANDSHAKE_DONE transaction | Commit it and coalesced frames; signal CONFIRMED | ESTABLISHED |
| C_FINISHING | Other valid DATA permitted after server authentication | Commit legal frames; retain FINISH retransmission obligation | C_FINISHING |
| C_FINISHING | Application send; credit/capacity permit | Queue/send with repeated FINISH as required | C_FINISHING |
| ESTABLISHED | Valid DATA transaction | Commit stream/ACK/control changes exactly once | ESTABLISHED |
| ESTABLISHED | Duplicate FINISH | Regenerate cached logical HANDSHAKE_DONE if needed | ESTABLISHED |
| ESTABLISHED | Duplicate HANDSHAKE_DONE | Idempotent ACK policy only | ESTABLISHED |
| CLOSING | Authenticated incoming traffic; response rate allows | Send CLOSE at most once per base PTO; do not process application frames | CLOSING |
| CLOSING | Close deadline | Stop emission; cancel/fence outstanding work | QUIESCING |
| DRAINING | Drain deadline | Cancel/fence remaining work | QUIESCING |
| QUIESCING | All matching fences complete and references zero | Zeroize keys/buffers required by policy; revoke descriptors | DEAD |

CLOSING and DRAINING deadlines are three current PTOs from entry and do not extend when an attacker sends more packets. A pure CLOSE packet is not an ACK promise. A close may be lost; the peer's idle timer is the ultimate cleanup mechanism.

### 20.4 Post-handshake guard details

Application frames are not permitted in C_WAIT_REPLY/C_VERIFY. DATA arriving before usable epoch-zero keys may be dropped without buffering. Handshake-loss recovery must tolerate that drop. S does not treat an ACK alone as client FINISH or source validation. A forged authentication failure is never converted into an authenticated CLOSE response.

SECURE_READY means that C has verified server authentication and may use the secure connection while confirmation is in progress. It is not a claim that the server has received FINISH. The API MUST expose this distinction when applications require confirmation before side effects.

### 20.5 Idle and derived-state events

Idle activity advances on a newly committed authenticated receive packet, or on the first emitted ack-eliciting packet after such a receive. Repeated retransmissions without incoming progress cannot extend idle forever. Duplicate or invalid packets do not refresh idle activity. The idle deadline is last_activity plus the larger of the negotiated idle timeout and three current base PTOs. Availability and memory FSMs in Sections 35–36 are orthogonal to the connection lifecycle. A local network failure preserves established reliable state until its authenticated idle/lease bound but cannot invent extra lifetime. Keepalive is optional and disabled by default; when enabled, a bounded PING may be sent before that deadline, but it does not indefinitely renew idle without peer response. Closing/draining deadlines are never extended by this rule.

After every commit, implementations reevaluate derived conditions such as all-bytes-ACKed, receive-base-at-FIN, pending-retirement-with-all-halves-done, and old-key-grace-with-zero-pins. The resulting event is queued once or folded into the same atomic transaction. No state waits for a transition edge that already occurred before the pending flag was set. A present but incorrect authenticated HANDSHAKE_DONE or FINISH proof causes PROTOCOL_VIOLATION; an incorrect authenticated refresh confirmation proof causes KEY_UPDATE_ERROR. Missing a required proof in a preconfirmation packet is the explicitly specified noncommitting drop.

<a id="section-21"></a>

## 21. Component FSMs

### 21.1 Slot lifecycle

| State | Event/guard | Action | Next |
|---|---|---|---|
| EMPTY | Local OPEN with available peer credit, or valid peer OPEN at expected generation | Allocate bounded halves; initialize offsets/limits | ACTIVE |
| ACTIVE | Same-generation repeated OPEN | No reinitialization | ACTIVE |
| ACTIVE | Local halves terminal; local endpoint is initiator | Queue STREAM_RETIRE | RETIRE_SENT |
| ACTIVE | Peer RETIRE; peer is initiator | Set retirement_pending; wait for local halves | RETIRE_PENDING |
| RETIRE_PENDING | All local halves terminal and buffers unpinned | Queue RETIRE_ACK; retain generation | RETIRED_WAIT_OPEN |
| RETIRE_SENT | Valid RETIRE_ACK for current generation | Release old buffers; increment generation or exhaust slot | EMPTY / EXHAUSTED |
| RETIRED_WAIT_OPEN | Duplicate same-generation RETIRE | Regenerate same RETIRE_ACK | RETIRED_WAIT_OPEN |
| RETIRED_WAIT_OPEN | Valid OPEN at exactly generation+1 | Advance generation; initialize fresh halves | ACTIVE |
| Any | Authenticated frame from older generation | Ignore old semantic work, maintain packet ACK behavior | Same |
| Any | Illegal future generation/direction/slot | STREAM_STATE_ERROR | Connection close |
| EXHAUSTED | Any local OPEN | Fail locally; no generation wrap | EXHAUSTED |

An ACTIVE peer RETIRE whose local halves are already terminal may take RETIRE_PENDING and RETIRED_WAIT_OPEN in one atomic action. It cannot expose partially freed state to the packet parser.

### 21.2 Refresh lifecycle

States are K_IDLE, K_WAIT_ANSWER (client), K_WAIT_COMMIT (server), K_COMMITTING (client), and K_RETIRING. A fatal refresh failure closes the owning connection.

| State/role | Event/guard | Action | Next |
|---|---|---|---|
| K_IDLE/C | Soft limit or KEY_REQUEST; previous slot free | Create OFFER; reserve STAGED slot; send reliable fragments | K_WAIT_ANSWER |
| K_IDLE/S | Complete valid OFFER for current+1 | Generate/cache ANSWER; install STAGED; send fragments | K_WAIT_COMMIT |
| K_WAIT_ANSWER/C | Complete valid ANSWER and MAC | Install STAGED; send KEY_COMMIT under STAGED | K_COMMITTING |
| K_WAIT_ANSWER/C | Repeated KEY_REQUEST | Coalesce; no second offer | Same |
| K_WAIT_COMMIT/S | Duplicate OFFER with identical hash | Reuse cached ANSWER; no new encapsulation for same transaction | Same |
| K_WAIT_COMMIT/S | Valid new-epoch KEY_COMMIT | Promote keys; retain previous; send KEY_CONFIRM | K_RETIRING |
| K_COMMITTING/C | PTO or lost commit | Send same logical commit with new packet number | Same |
| K_COMMITTING/C | Valid new-epoch KEY_CONFIRM | Promote keys; ACK normally; start old-key grace | K_RETIRING |
| K_RETIRING/S | Duplicate valid KEY_COMMIT | Regenerate same logical confirm under new key | Same |
| K_RETIRING | Previous grace complete, reference fences done, and transaction confirmed | Erase previous; retain duplicate hashes; release refresh buffers | K_IDLE |
| K_RETIRING/C | New refresh required before old slot free | Set one pending request; throttle if hard limit approaches | Same |
| K_RETIRING/S | Next OFFER while old slot cannot be freed | Do not retain/ACK its fragment transaction; retry later via sender recovery | Same |
| Any live refresh state | Same ID with inconsistent offer/answer content | KEY_UPDATE_ERROR | Connection close |
| Any | Unknown epoch without installed key, bad authentication, or stale callback | Drop/ignore | Same |

For the server, an ACK of KEY_CONFIRM confirms the transaction. Receipt of a valid subsequent OFFER under the new CURRENT epoch also proves that the client received confirmation, but does not waive the previous-key grace/fence requirement. For the client, receiving valid KEY_CONFIRM is confirmation. This rule avoids a lost confirmation ACK deadlocking future refresh.

### 21.3 Path lifecycle

The complete authoritative path transition table is Section 17.7. States are FREE, VALIDATING, REACHABLE, READY, ACTIVE, OLD, FAILED. Validation, PMTU readiness, route selection and physical allocation reclamation are separate conditions. Mobility/parking use the orthogonal availability FSM in Section 35; compacting uses Section 36.8. A response event must pass exact token, tuple, generation, expiry and base-size guards before reaching READY. Failed guards are nonmutating self-transitions unless their section explicitly requires a local failure or authenticated protocol error.

### 21.4 Congestion lifecycle

States are CC_SLOW_START, CC_RECOVERY, and CC_AVOIDANCE. Migration administrative detachment is not a congestion-loss signal for a different path. The persistent startup bucket in Section 17.6 is an additional gate, not a replacement for this controller. New path enters CC_SLOW_START. Eligible ACKs increase cwnd according to the current state. A new loss/CE event outside recovery enters CC_RECOVERY and applies one reduction. An eligible post-boundary ACK enters CC_AVOIDANCE. A persistent-congestion event enters CC_SLOW_START with cwnd=2*M. ACKs of already-accounted packets are self-transitions without growth. Application-limited and flow-limited events never increase cwnd by themselves.

### 21.5 Reliable-item ownership

A logical item is LIVE or RESOLVED and has a pending-send flag, a finite set of in-flight packet references, and a DMA pin count. Loss adds a pending-send obligation only while LIVE. An ACK that proves logical receipt makes it RESOLVED and clears pending send. Other transmissions of that item can remain physically in flight without making the item live again. Reliable-source payload is released only when RESOLVED/abandoned and all crypto, I/O, DMA, and application-lease pins on that source are zero. A separate sealed-output buffer may be returned as soon as its own I/O pins are released, even while reliable-source bytes remain live; conversely, an ACK cannot return sealed memory still owned by the kernel/NIC. These are independent ownership state machines (Section 29.3).

A reliable control item is identified by its semantic key: frame type plus stream ID, CID sequence, refresh fragment ID, or connection scope. Monotonic values replace lower queued values. There is no packet-to-packet retransmission chain.

<a id="section-22"></a>

## 22. Hardware receive and transmit execution contract

### 22.1 Receive pipeline FSM

The RX pipeline states are RX_IDLE, RX_HEADER, RX_KEY, RX_REPLAY_PROBE, RX_AUTH, RX_PARSE, RX_PREFLIGHT, RX_ACK_WAIT, RX_STAGE, RX_COMMIT, RX_NOTIFY, RX_RELEASE, and RX_DROP.

| State | Required operation | Success | Failure |
|---|---|---|---|
| RX_IDLE | Accept one complete child UDP descriptor after any GRO/SG normalization | RX_HEADER | Stay idle on no descriptor |
| RX_HEADER | Check fixed bounds/class/CID; dispatch bootstrap separately | RX_KEY | RX_DROP |
| RX_KEY | Look up exact live context generation, immutable selected cipher, and installed epoch | RX_REPLAY_PROBE | RX_DROP |
| RX_REPLAY_PROBE | Read-only number/window check; pin key/context | RX_AUTH | RX_DROP if provably duplicate/too old |
| RX_AUTH | Verify AEAD into quarantined plaintext memory | RX_PARSE | RX_DROP; increment bounded auth-failure counter |
| RX_PARSE | Bounded frame scan, syntax and flag classification | RX_PREFLIGHT | Authenticated close event then RX_DROP |
| RX_PREFLIGHT | Tentative all-frame semantics; checked sums; identify pending I/O ACK dependencies | RX_STAGE if none; RX_ACK_WAIT if needed | Drop without ACK for capacity; close for authenticated error |
| RX_ACK_WAIT | Retain authenticated transaction in one of at most eight wait slots; prioritize matching I/O results | RX_PREFLIGHT after dependencies settle | Drop without ACK if wait capacity unavailable; cleanup on close |
| RX_STAGE | Reserve/recheck needed storage; complete copy/DMA into invisible storage; compare overlap | RX_COMMIT | Release reservations; no semantic ACK |
| RX_COMMIT | Revalidate context generation/replay; atomically publish all effects | RX_NOTIFY | Roll back staged visibility/reservations |
| RX_NOTIFY | Enqueue application/timer/scheduler notifications exactly once | RX_RELEASE | Backpressure; do not undo a committed packet |
| RX_RELEASE | Release packet scratch, key pins, and descriptor ownership | RX_IDLE | Wait for matching owned cleanup |
| RX_DROP | Release only owned resources; optional permitted diagnostic | RX_IDLE | Wait for owned cleanup |

A commit may take several hardware clocks; atomic means that other observers cannot see an intermediate state. A per-connection commit lock, versioned shadow record, or equivalent mechanism is REQUIRED. A single-cycle update of a multi-kilobyte bitmap is not assumed.

The baseline serializes one connection's semantic preflight, staging reservation, and commit, not merely its final register writes. RX_ACK_WAIT is outside that serialization interval and holds no lock needed by I/O acceptance. An implementation allowing concurrent speculative transactions must bind each preflight to a semantic-state version, exclusively reserve all physical staging ranges that might be written, and revalidate the complete read set/version before publication. A changed credit, stream generation, final size, receive base, ACK ledger, key role, or path state invalidates stale preflight and requires rollback/re-evaluation. Another transaction cannot commit a byte into storage still writable by an earlier speculative DMA. Close/fault cancellation fences that DMA before the storage is reused. A context-allocation generation check alone is insufficient for this same-generation semantic race.

Before commit, application DMA addresses MUST NOT reference quarantined plaintext. Even a correct AEAD tag is insufficient to release bytes before stream/flow/final-size validation. DMA staging may write previously unused physical ring locations, but it cannot set their visible valid bits or overwrite a different committed byte. A failed staging transaction never leaves newly visible valid bits.

The commit action publishes new stream bytes, present bits, flow high-water totals, control state, replay acceptance, ECN counters, and ACK-generation eligibility as one transaction. ACK processing inside the same packet is included. An authenticated CLOSE packet is handled through its dedicated lifecycle action rather than requiring application buffer capacity.

Pure DATAGRAM queue overflow may discard datagram payload while still committing the packet, because DATAGRAM is explicitly unreliable. This exception never applies to STREAM or incomplete refresh messages.

### 22.2 Transmit pipeline FSM

States are TX_IDLE, TX_SELECT, TX_RESERVE, TX_READ, TX_SEAL, TX_READY, TX_SUBMIT, TX_ACCEPT_WAIT, TX_EMIT_COMMIT, TX_RELEASE, and TX_CANCEL. The physical-output buffer has a separate release FSM; TX_RELEASE does not mean the kernel/NIC has released it.

| State | Required operation | Success | Failure / deferral |
|---|---|---|---|
| TX_IDLE | Select ready connection/path work | TX_SELECT | Stay idle |
| TX_SELECT | Choose at most 32 frames; fit child PMTU; record logical references | TX_RESERVE | Defer blocked item |
| TX_RESERVE | Atomically reserve flow, packet budget, ledger, buffer, and cipher usage; allocate fresh PN | TX_READ | Release revocable reservations; no partial admission |
| TX_READ | Pin immutable source ranges; assemble aligned frames and any pre-seal padding | TX_SEAL | TX_CANCEL |
| TX_SEAL | Mark crypto invocation potentially executed; selected AEAD seals one child | TX_READY | TX_CANCEL; PN/used crypto budget never roll back |
| TX_READY | Revalidate path, PMTU, key, backend and owner generations; optionally gather compatible children | TX_SUBMIT when legal pacing/budget available | Hold boundedly, split batch, or TX_CANCEL before handoff |
| TX_SUBMIT | Publish a complete immutable child or batch to backend; mark ledger SUBMITTED and record release timestamp | TX_ACCEPT_WAIT; an inline acceptance may proceed directly to TX_EMIT_COMMIT | TX_CANCEL only on proven no-handoff |
| TX_ACCEPT_WAIT | Await matched per-request acceptance result; keep reservation and I/O pins | TX_EMIT_COMMIT on acceptance or ambiguous post-handoff delivery | TX_CANCEL only on definitive no-acceptance; otherwise hold/fence |
| TX_EMIT_COMMIT | Set EMITTED once; retain release/egress timestamp, assign path ordinal, transfer reserved budgets to in-flight/sent counters | TX_RELEASE | Fatal context fault: quiesce, never reuse PN |
| TX_RELEASE | Drop construction pins; retain required logical-source and physical-output I/O references separately | TX_IDLE | Matching cleanup remains independently pending |
| TX_CANCEL | Set never-emitted PN BURNED; release revocable packet budgets; preserve pending logical bytes and any externally owned memory | TX_IDLE after accounting | Wait for exact I/O/crypto ownership fence before recycling storage |

A per-connection release sequencer publishes DATA in increasing allocated PN order, with burned gaps allowed. Reserving a number does not imply it was submitted. Parallel crypto engines return results to a bounded ready queue. A missing lower-number result cannot block urgent control indefinitely: before handoff it may be canceled and burned, its live logical work requeued. No higher-number release may overtake an unresolved lower-number submission unless the adapter proves equivalent ordered acceptance. The baseline io_uring adapter permits only one unresolved acceptance operation per connection; notification-only buffer-release work may remain outstanding independently.

Publication is irreversible with respect to buffer ownership until the backend's matching completion or cancellation fence says otherwise. A timeout, user cancellation, PMTU change, or connection close is not such a fence. A published AF_XDP descriptor is conservatively accepted at its ring publication boundary. An ordinary socket send is accepted when its full payload is accepted by the call; an io_uring socket send becomes accepted when the operation result reports that same full acceptance. Sections 29–32 define exact error and completion semantics. A later NIC/kernel/network drop is recovery loss, never permission to reuse a packet number or seal nonce.

The transport records a conservative release timestamp before asynchronous publication, not when a late CQE is reaped. It arms network recovery only for entries resolved as EMITTED. While a result is pending, reserved bytes remain unavailable; a local I/O-stall watchdog may quiesce the adapter but cannot fabricate a peer loss or release memory. A successful acceptance result transfers each child's reservations exactly once even if its buffer-release notification or an authenticated ACK has already become observable in another queue.

For a batch, crypto preparation is per-child and backend acceptance is per operation. A successful GSO send accepts every child in that request; sendmmsg may instead accept a prefix of **messages**, each of which can itself be a complete GSO request. Partial preparation may publish a valid ready prefix before any backend exposure. It must never leave a shortened child in the middle of a fixed-stride aggregate. Subsequent sections specify how to split or cancel the remaining children without resealing accepted data.

PN allocation never rolls back. Logical flow reservations remain charged as described in Section 12.4. Consumed crypto usage includes failed/aborted invocations, whereas never-accepted bytes do not become congestion flight bytes. Pacing consumed by a definitive rejected operation may be returned only up to the current two-M bucket cap; it does not accumulate a compensatory future burst.

A crypto job pins the exact immutable key-slot generation it actually reads. Once ciphertext is complete, a pure ciphertext I/O descriptor need not pin a secret key if no later engine accesses that key, but it must retain the necessary immutable packet/owner metadata. An inline crypto descriptor does pin the key until its completion fence. Erasure can never race a remaining secret read, and an output buffer cannot be overwritten while its backend owns it.

### 22.3 Fundamental gates

A normal DATA emission is legal only if all these gates pass together:

```
connection and availability/parking state permit this frame class
AND selected epoch is installed and within hard limits
AND packet number is fresh
AND packet fits current path PMTU and peer maximum
AND this exact path is validated OR both 3x byte/packet budgets permit control OR a separately authorized bounded local probe applies
AND stream high-water reservations fit stream and connection limits
AND in-flight reservation fits cwnd OR this is an authorized bounded PTO probe
AND pacing tokens/deadline permit emission OR this is a permitted ACK/CLOSE bypass
AND sent-ledger and item-reference capacity is available
AND immutable payload DMA ownership is established
AND backend/connection/path/PMTU generations still match
AND every child of an immediately released batch passes the gates independently
```

Resource and release reservations have separate phases. A child may hold source, output, metadata, key-use and preparation-credit reservations while waiting for future pacing eligibility. Pacing tokens are reserved only for a permitted handoff, not for every precomputed child. At release, test `bytes_in_flight + other_live_cwnd_reservations + this_release_bytes <= cwnd` for ordinary charged traffic, excluding this release's own existing reservation from the "other" term. Apply analogous exclusion for amplification accounting. Any now-inadmissible prepared child remains boundedly queued or is canceled. Key hard-limit checks at this stage verify the job's already-consumed/reserved allowance; they do not request an additional seal allowance. Ownership of every reservation transfers exactly once between phases.

These are conjunctive gates. Passing congestion control does not bypass flow control; passing authentication does not bypass path validation; reliable control does not bypass congestion; lack of a TLS record layer does not bypass service authentication.

### 22.4 Suggested host/device queues

A hardware implementation exposes bounded rings of commands and completions. Every entry has an explicit generation cookie. One possible normative abstract command format is:

```
opcode:u16 || flags:u16 || descriptor_length:u32
|| connection_slot:u32 || connection_generation:u32
|| stream_id:u64 || user_cookie:u64
|| address_or_value0:u64 || length_or_value1:u64 || value2:u64
```

Length is 56 bytes. The bus representation is little-endian for scalar fields; this is an abstract device interface, not an Internet wire format. WRITE uses an immutable buffer address, byte length, and accepted-offset expectation. CONSUME uses a byte count not greater than readable data. OPEN returns a generated stream handle. Completion records include the same user cookie, final local result, byte count, and connection generation. Descriptor lengths, physical address widths, IOMMU permissions, and queue sizes are validated before DMA.

The device cannot silently DMA from arbitrary addresses supplied by a network frame. Remote stream IDs index validated device-owned tables, never raw host pointers. Network credit and host descriptor availability are different quantities.

<a id="section-23"></a>

## 23. Concurrency, registers, and action contracts

### 23.1 Event ordering

For events with equal timestamp in one connection, the reference order is: fatal fault; authenticated CLOSE/reset; matching I/O acceptance results and ownership-release completions; completed RX transactions ordered by ingress completion sequence (including newly unblocked ACK waits); network/interface and suspend/resume events ordered by adapter sequence; completed key/trust jobs; application commands ordered by queue sequence; timer expirations ordered by timer-kind ID; transmit scheduling. A stale-event filter runs before this ordering.

Within an RX transaction, syntax is scanned before semantic preflight. Frames update a tentative state in frame order. No external application event can interleave with commit. A due loss timer after an ACK transaction sees that ACK's results and cannot requeue an already resolved item. Implementations with other parallel schedules must show observational equivalence to an allowed serialization.

### 23.2 Required logical registers

| Object | Required fields and widths |
|---|---|
| Connection | generation:u32; role:u1; state:u4; cipher_state:u3; selected_cipher:u16; next_pn:u64; rx_largest:u64 plus valid bit; replay_floor:u64; tx_flow_used:u64; rx_flow_used:u64; peer_max_data:u64; local_max_data:u64; last_activity:u64; auth_fail_count:u64; closing_deadline:u64 |
| Key slot, at most 3 | epoch:u32; slot_generation:u32; state:u2; cipher:u16; directional key/iv and optional cached XChaCha subkey; seal_count:u64; seal_plaintext_bytes:u64; reserved_seals:u64; reserved_plaintext_bytes:u64; pins:u32; activation_time:u64; erase_deadline:u64 |
| Path, at most 3 | generation:u32; state:u3; tuple; validated:u1; amp_rx/amp_tx/amp_reserved:u64 for both bytes and datagrams; cwnd/ssthresh/flight/reserved:u64; srtt/rttvar/min_rtt/latest_rtt:u64; recovery_ordinal:u64; next_path_ordinal:u64; pto_count:u8; M:u16; pmtu_generation:u32 |
| Stream slot | generation:u46; lifecycle:u3; initiator:u1; uni:u1; send_state:u3; recv_state:u3; offsets/limits:u64; FIN/reset flags; per-half ring references; retirement_pending:u1 |
| Timer | owner_slot:u32; owner_generation:u32; timer_kind:u8; timer_generation:u32; deadline_us:u64 |
| Packet scratch | context/key generation tokens; PN; flags; parsed-frame count; at most 32 tentative frame records; at most 4096 plaintext bytes; staged-reservation references |

Wider counters are permitted. Narrower encodings require a proof that representable limits and saturation are identical. Generation wrap requires retiring that local context slot until all possible references are gone; it is not safe merely because a new connection uses different packet keys.

### 23.3 Atomic action contracts

**RX_ACCEPT:** requires matching context/key/path generations, authenticated packet, fresh PN at commit time, valid complete frame grammar, successful all-frame preflight, admitted receive/path state, and completed staging. New-path ingress credit, candidate creation and mobility/lease control changes are committed with the same transaction, not before authentication or on a speculative parse. It atomically commits semantic state and replay acceptance. It never commits a replay bit for an uncommitted reliable payload.

**TX_RESERVE:** checks structural, flow, epoch-use, and bounded-memory admission; obtains packet-number ownership, ledger capacity, preparation-budget reservations, and immutable item pins. It does not require pacing tokens to be available for immediate release when bounded ahead-of-time sealing is enabled. Reversible packet budgets and irreversible logical-offset/nonce reservations are recorded separately. TX_READY/TX_SUBMIT recheck every actual emission gate; precomputation cannot bypass a later reduction in cwnd, amplification availability, or PMTU.

**IO_ACCEPT:** settles a matching SUBMITTED packet or complete accepted message into EMITTED exactly once; moves its congestion/amplification reservations into committed counters, records its retained release time and path ordinal, and unblocks dependent RX transactions. It never resolves reliable application bytes.

**IO_BUFFER_RELEASE:** decrements only the physical buffer pins proven releasable by a matching backend completion. It does not acknowledge delivery, refund consumed crypto usage, or alter stream offsets.

**ACK_APPLY:** validates explicit ACK intervals against bounded retained emission history; resolves each newly acknowledged packet once; then resolves logical items, updates RTT/CC as applicable, and creates no application delivery event at the remote endpoint.

**LOSS_APPLY:** changes a counted packet from IN_FLIGHT to LOST once, subtracts its in-flight count once, and marks unresolved items pending without duplicating an existing pending obligation. It does not reuse packet ciphertext.

**KEY_PROMOTE:** requires the appropriate valid commit/confirm proof and matching refresh ID. It swaps at most three key-slot roles atomically, sets grace deadlines, and leaves global packet counters/replay history unchanged.

**STREAM_RETIRE_COMMIT:** requires both local halves terminal and all relevant payload references fenced before freeing storage. It preserves the generation and enough retirement metadata to reject stale packets and respond idempotently.

**PATH_PROMOTE:** requires READY, authorized intent/ordering policy, a committed old-path cutover barrier, and available bounded path/operation state. It preserves global PN/replay/stream/cipher state and never transfers amplification credit or old cwnd.

**PARK_GRANT_COMMIT:** requires negotiated lease, semantic quiescence, fixed expiry, and reserved compact retention capacity. It freezes application admission, not physical I/O ownership. Wake reacquires credit backing before releasing the receive gate.

**CONNECTION_QUIESCE:** revokes application admission and egress permission, increments cancellation tokens, completes each local operation once, cancels or waits for all owned jobs, zeroizes secrets, and only then permits slot reuse.

<a id="section-24"></a>

## 24. Error registry and failure behavior

CLOSE carries a fixed error code and a hash of a locally logged diagnostic, not attacker-controlled text. Error codes are:

| Code | Name |
|---:|---|
| 0 | NO_ERROR |
| 1 | INTERNAL_ERROR |
| 2 | PROTOCOL_VIOLATION |
| 3 | FRAME_ENCODING_ERROR |
| 4 | FLOW_CONTROL_ERROR |
| 5 | STREAM_STATE_ERROR |
| 6 | FINAL_SIZE_ERROR |
| 7 | STREAM_DATA_ERROR |
| 8 | ACK_RANGE_ERROR |
| 9 | CONNECTION_ID_ERROR |
| 10 | KEY_UPDATE_ERROR |
| 11 | AEAD_LIMIT_REACHED |
| 12 | CRYPTO_BUFFER_EXCEEDED |
| 13 | TRANSPORT_PARAMETER_ERROR |
| 14 | APPLICATION_ABORT |
| 15 | LOCAL_RESOURCE_ERROR |
| 16 | PATH_STATE_ERROR |
| 17 | MOBILE_STATE_ERROR |

Invalid AEAD, unsupported unauthenticated versions, unknown epochs, malformed bootstrap packets, expired cookies, unrecognized CIDs, and failed service authentication are not reflected as authenticated transport errors. They are dropped or fail the local handshake as appropriate. An implementation must not reveal whether ML-KEM decapsulation internally used implicit rejection through distinct network alerts.

Resource exhaustion caused by a receiver's own admission mistake is not FLOW_CONTROL_ERROR by the peer. The endpoint may drop an uncommitted packet, backpressure new work, or close with LOCAL_RESOURCE_ERROR. It must not ACK discarded reliable bytes, and it must not falsely accuse a peer that respected advertised limits.

<a id="section-25"></a>

## 25. Security properties, limitations, and attack handling

GRT/1.1 targets confidentiality and integrity against network attackers, replay rejection within a live connection, authenticated service binding, and resistance to passive harvest-now-decrypt-later attacks for confirmed traffic when its hybrid components and ephemeral erasure assumptions hold. This is a design target requiring review, not a demonstrated theorem about this composition.

The PQXDH-derived exchange retains classical identity authentication. An active quantum adversary capable of breaking the identity/signature assumptions is outside the claimed authentication target. Replacing just the KEM does not change that fact. A future active-PQ authentication profile would need a separately specified PQ signature or authentication construction, trust representation, handshake size analysis, and proof.

Prekeys and encapsulation inputs are fresh per attempt. The response incorporates server randomness and fresh server DH/KEM material, so a repeated client flight cannot make the server seal a different response under a reused session key/nonce. Cached responses are immutable. No per-packet key ratchet is required for reordering, but current endpoint memory compromise can reveal current traffic keys and retained resumption/exporter secrets.

An ACK is authenticated but a malicious peer can still lie about delivery or congestion. No end-to-end transport can force a peer to consume data honestly. Bounds and validation prevent unauthenticated ACK injection and many inconsistent reports; they do not prove receiver truthfulness.

Packet numbers, epochs, class, CIDs, and lengths are not an anonymity layer. The header is deliberately clear, including packet numbers and ack-eliciting flags. A connection-global counter may help observers correlate CID rotations or migration paths; key refresh does not reset it. This loses metadata confidentiality/ossification resistance compared with a protected-number design. It does not remove AEAD integrity, but deployments must not describe the change as privacy-neutral. The plaintext client flight reveals service authority, application ID, and any stable client public identity. GRT/1.1 has no ECH-equivalent mechanism. That limitation must be explicit in deployments that require name or client-identity privacy.

Anti-amplification limits reflection bytes, not all denial of service. Valid source addresses and cheap-to-obtain tickets do not eliminate signature, KEM, memory, or scheduling attacks. Admission pools, default stateless Retry for Compact public listeners, work queues, deterministic parsing limits, and global/per-prefix rate limits are necessary. Hardware implementations require side-channel and fault analysis, constant-time primitive implementations, key isolation, DMA isolation, and tested zeroization.

Loss, delay, reordering, or an attacker that drops every packet can prevent progress. Reliability means retained obligations and eventual delivery under continued cooperative connectivity and available application capacity, not guaranteed delivery over a permanently failed path. A finite idle/close timeout can terminate an unresolved operation with an explicit error.

GRT does not claim that its reduced parser/state organization is automatically faster, smaller, safer, or more energy-efficient than every QUIC implementation. Its full-size packet numbers, fixed framing, bootstrap PQ payloads, stream-generation records, and key-refresh control have measurable costs. The trade is simpler bounded fast-path control, not removal of the fundamental memory and congestion requirements of reliable transport.

<a id="section-26"></a>

## 26. Hardware sizing and performance envelope

### 26.1 Payload efficiency

A DATA packet carrying one STREAM frame has a 32-byte common header, a 16-byte tag, an 8-byte frame header, and a 16-byte stream prefix: 72 bytes of transport overhead, plus at most seven bytes of alignment. At a 1200-byte UDP payload, one stream can carry 1128 application bytes exactly with no alignment remainder. At a 4096-byte UDP payload, it can carry 4024 bytes. IP, UDP, Ethernet, physical-layer framing, and ACK traffic are additional.

An ACK with one range uses 56 plaintext bytes and 104 total UDP payload bytes. Eight ranges use 168 plaintext bytes and 216 total UDP payload bytes. These values follow this specification's frame grammar, not QUIC's variable encodings. A FINISH body is 48 bytes, producing a 56-byte frame before any coalesced data.

The minimal pinned server flight is 1544 message bytes and uses two 1200-byte bootstrap datagrams. A client flight without ticket/early data is `1670 + authority_length` message bytes and also normally uses two. Four compact credentials make the server flight 2328 bytes, requiring three bootstrap datagrams. These are serialization calculations, not a measured handshake latency or CPU cost.

### 26.2 Bandwidth-delay storage

A sender sustaining rate R bits/s over round-trip time T seconds needs approximately `R*T/8` bytes of outstanding payload capacity, plus metadata and retransmission headroom. Examples, in decimal units:

| Rate | RTT | Approximate bandwidth-delay product |
|---:|---:|---:|
| 1 Gbit/s | 10 ms | 1.25 MB |
| 10 Gbit/s | 10 ms | 12.5 MB |
| 100 Gbit/s | 10 ms | 125 MB |
| 100 Gbit/s | 100 ms | 1.25 GB |

At 100 Gbit/s and 1200 UDP-payload bytes per packet, the payload-only packet-rate estimate is about 10.42 million packets/s. Ten milliseconds corresponds to about 104167 packets in flight before link overheads. A 4096-entry sent-packet table cannot support that case regardless of cipher throughput. Flow windows and table admission must be provisioned consistently.

A byte-valid bitmap adds R/8 bytes to an R-byte receive ring; a 64-KiB ring plus bitmap is 72 KiB before control metadata. This bounded organization can be mapped to external DRAM/HBM with on-chip descriptors and bitmaps or bitmap caches. It is not evidence that thousands of such streams fit in small on-chip SRAM.

### 26.3 Pipeline throughput budget

A 512-bit internal bus at 250 MHz has a raw width-times-frequency rate of 128 Gbit/s. This arithmetic does not establish 100-Gbit/s transport throughput. A design still needs sustained selected-AEAD throughput and Poly1305/GHASH completion, context lookup bandwidth, packet descriptors, bitmap access, retransmission storage traffic, ACK processing, egress readiness, and independent receive/transmit resources.

If a payload is written, retained, read for transmission, and read again for repair, those are real memory transfers. Whole-connection serialization at a slow context RAM port can dominate a wide cipher pipeline. Hot single-connection benchmarks, tiny-packet packet rates, large RTTs, sparse one-byte out-of-order fragments, and adversarial invalid packets all require separate capacity tests.

The establishment engine has a different budget: prekey generation/signature checking, three X25519 shared computations per side, ML-KEM encapsulation/decapsulation, credential validation, ticket ledger access, and key installation. Its connections-per-second rate cannot be inferred from steady-state bits per second. Precomputation pools may reduce response work but must enforce one-time assignment and secure erasure.

<a id="section-27"></a>

## 27. Conformance, verification, and implementation release gates

### 27.1 Wire and arithmetic tests

A conforming implementation must test every fixed-length boundary; all reserved fields; minimum and maximum frames; short/trailing/oversized frames; zero/nonzero alignment; every ACK range ordering error; 64-bit offset overflow; CID and generation mismatches; malformed bootstrap chunk counts and lengths; inconsistent duplicate fragments; failed credential chain scopes; and unknown critical versus skippable frames.

Golden packet vectors must include the clear authenticated header, selected cipher ID, plaintext, exact 12- or 24-byte AEAD nonce, ciphertext/tag, final datagram, and decrypted result for both ciphers. XChaCha vectors also compare direct and cached-HChaCha execution. Negative vectors modify every authenticated header class and cipher-negotiation field. GSO fixtures identify each child boundary and test independent authentication after splitting. A KDF vector must include explicit input bytes and every output label. Whole-handshake vectors must separately cover XEdDSA, X25519, ML-KEM, transcript bytes, Finished values, and tickets. A symmetric fixture with fabricated post-KEX secrets is useful, but is not a whole-handshake cryptographic test vector.

### 27.2 Loss and concurrency scenarios

Tests must include loss of every client/server-flight fragment; reordered fragments; duplicate client flights; Retry before/after a delayed reply; early accept/reject and crash-ledger behavior; lost FINISH; lost HANDSHAKE_DONE; overlapping STREAM ranges; FIN before missing data; RESET with a large final-size gap; per-packet aggregate flow increments; missing OPEN; retirement ACK loss; late old generations after slot reuse; and datagram-queue overflow without reliable-byte loss.

Refresh tests must lose each OFFER/ANSWER fragment, KEY_COMMIT, KEY_CONFIRM, and confirmation ACK; deliver old/new epoch packets in every crossing order; expire old-key grace while jobs remain pinned; replay a completed refresh; request another refresh before old-slot release; and approach hard sealing limits while the peer stops responding.

Hardware tests must cancel a seal after nonce allocation; delay an egress commit; wrap a ring; complete two authenticated duplicate packets concurrently; reuse a local context while a stale timer or crypto job is pending; fail DMA after preflight; and race ACK, timeout, reset, and application consumption. Every case must preserve the invariants below.

### 27.3 Mandatory invariants

```
I01: A directional (key, nonce) pair is never used for two sealing invocations.
I02: No application-visible byte originates from unverified AEAD plaintext.
I03: No replay bit/ACK promise commits for unretained reliable bytes.
I04: Every stream's delivered offsets form a prefix, except explicit reset termination.
I05: A stream's missing byte cannot block another eligible stream's delivery.
I06: Duplicate ACKs/loss events cannot double-decrement bytes_in_flight.
I07: Logical flow credit counts high-water increments, not retransmissions.
I08: Final size is immutable once known.
I09: Unvalidated path amplification never exceeds its budget.
I10: A nonce, packet counter, or stream generation never rolls back on cancellation.
I11: At most three epoch slots and one refresh transaction exist.
I12: Old-epoch traffic never reverses a committed key promotion.
I13: A stale timer/job/descriptor cannot mutate a recycled owner context.
I14: DMA-visible ownership outlives every DMA reference.
I15: ACK-only traffic cannot cause an unbounded ACK-of-ACK loop.
I16: Congestion, flow, pacing, amplification, PMTU, and local-capacity gates are distinct.
I17: Early execution requires the explicitly authorized replay-policy transaction.
I18: Stream slot reuse cannot alias delayed old-generation application bytes.
I19: A packet transaction either commits all reliable semantics or commits none.
I20: No unauthenticated field installs a key, expands credit, or migrates the active path.
I21: One wire datagram has exactly one AEAD record and one DATA packet number.
I22: A local aggregate never hides child bytes from congestion/amplification/key accounting.
I23: An I/O completion is never substituted for a peer ACK or application consumption.
I24: Every RX child is split from trusted metadata before interpreting its GRT length field.
I25: A zero-copy buffer is not recycled before all required ownership completions.
I26: ACKs for SUBMITTED packets wait for I/O results instead of falsely proving unsent data.
I27: Cipher offers/selections are transcript-bound and fixed for all epochs of a connection.
I28: Every conforming peer implements and advertises the XChaCha interoperability baseline.
I29: Every immediate GSO release obeys the same pacing envelope as individual sends.
I30: A PMTU/backend-generation change cannot rewrite already submitted ciphertext.
I31: One child failure cannot silently discard a different retained reliable obligation.
I32: RX application leases cannot starve the reserved I/O and control-progress pools.
I33: A provided-buffer ID or UMEM address is not reused while an earlier generation is owned.
I34: Counts of ACK/ECN/loss events refer to child packets, not SQEs, CQEs, or aggregates.
I35: In-flight seal reservations prevent concurrent jobs from exceeding a key usage cap.
```

### 27.4 Liveness assumptions and proof targets

Under fair scheduling, a cooperative peer, application consumption of required receive buffers, available retransmission storage, and a path that eventually delivers some packets and ACKs, every LIVE reliable item must eventually become RESOLVED or be terminated by an explicit connection/application error. Backpressure must not depend on an ACK for data that was never emitted. Retirement and key confirmation must have a retransmission path that does not require the very resource they are waiting to release.

Finite-control model checking should first use small packet/stream bounds while preserving wrap and reuse behavior, then refine parameterized invariants. Cryptographic analysis must cover the reversed PQXDH roles, signed prekey reuse assumptions, identity/service/ALPN-like binding, early PSK composition, response replay, refresh transcript, key-compromise impersonation, and active/passive quantum distinctions. RTL equivalence checking then connects the logical transitions to actual queues and RAM transactions.

Before deployment as a QUIC replacement, require at least two independent wire implementations, packet/KDF/whole-handshake interoperability vectors, fuzzing, adversarial loss/reordering tests, formal or independently reviewed security analysis, hardware side-channel/fault review, and realistic congestion coexistence tests. None of those gates is satisfied merely by having this document or by passing the accompanying mechanics tests.

<a id="section-28"></a>

## 28. Design rationale summary

GRT/1.1 reduces repeated work in the packet path, not the number of problems a public-Internet reliable transport must solve. Its fixed header and frame alignment trade bytes for deterministic parsing. Full packet numbers trade bytes for removal of reconstruction ambiguity. Slot/generation handles trade a retirement exchange for bounded lifetime state. An online reversed prekey exchange trades a larger first flight for no prerequisite prekey-directory RTT. Stable packet epochs trade per-message ratcheting for independent decryption, with explicit fresh hybrid refresh.

Low latency comes from prompt application-to-packet scheduling, short ACK deadlines, per-stream rather than connection-wide ordering, selective byte repair, accurate timers, pacing, and avoiding unnecessary handshake round trips. It does not come from disabling congestion control, acknowledging unstored data, ignoring PMTU, or treating post-quantum primitives as free.

<a id="section-29"></a>

## 29. Common software execution and buffer ownership

This section is normative for an implementation claiming the **Accelerated Software Profile**. Other implementations MAY use a different internal organization, but MUST preserve the wire semantics, cryptographic limits, congestion behavior, transactional receive rules, and ownership safety of the core specification. Hardware acceleration is optional. A peer never needs to know whether its correspondent uses sockets, a polling NIC, or a hardware pipeline.

### 29.1 Backend separation

Three execution profiles are defined:

| Profile | Network interface | Aggregation | Memory-release authority |
|---|---|---|---|
| Socket-GSO | UDP sockets, normally sendmsg/sendmmsg and recvmsg/recvmmsg | UDP_SEGMENT transmit; optional UDP_GRO receive | Completed copying send, or the separately specified zero-copy completion mechanism |
| AF_XDP | Queue-bound XSK with registered UMEM | A burst of independent packets; optional scatter/gather within one packet | RX ownership transfer and TX COMPLETION ring entries |
| io_uring | UDP socket operations submitted through SQ/CQ | Optional UDP_SEGMENT/UDP_GRO on the underlying socket | Operation CQEs and, when required, zero-copy notification CQEs |

GSO is a socket capability, not a mutually exclusive alternative to io_uring. AF_XDP multi-buffer operation is scatter/gather for one packet, not UDP segmentation for several packets. Implementations MUST NOT interchange these meanings.

Each adapter presents the transport engine with **individual child-datagram events**. An adapter may batch function calls, cryptographic jobs, memory reservations, or completion polling. None of those batching choices changes the unit of AEAD, replay admission, ACK reporting, packet loss, PMTU, or congestion accounting. A packet containing several frames still commits atomically; several packets in one receive buffer do not commit as a single transaction.

A conforming implementation declares its supported backend modes and verified capabilities at initialization. Configuration requesting unavailable zero-copy, segmentation, scatter/gather, or checksum features MUST either fail explicitly or fall back to a declared safe mode. Silent dependence on an unsupported NIC feature is forbidden. Kernel, driver, NIC, cryptographic provider, and firmware versions belong in the local implementation manifest, not the handshake.

### 29.2 Connection ownership, queue steering, and scheduling

External asynchronous agents MUST NOT directly mutate transport state. Network callbacks, I/O and crypto completions, timers, and application threads emit immutable generation-tagged events to the owning dataplane. Every externally visible transport-state change occurs through an authorized component-FSM transition and commit. A stale completion may still discharge its own exact resource obligation; it is not blindly discarded with an outstanding DMA/kernel reference. One connection has one logical owner at a time. Cross-core ownership transfer, when implemented, requires a mailbox/generation fence and must not overlap writers.

Run-to-completion means completing one bounded owner event/transaction, not blocking the worker while waiting for DMA, AEAD acceleration, a kernel CQE, or a network RTT. Such work enters a pending state and resumes from a matching event. Synchronous parser/AEAD/preflight stages may be fused into one hot-path function while preserving their logical failure/commit boundaries. No giant cross-product switch, per-stage heap allocation, or table dispatch is required. Bounded per-connection quanta protect other sessions on the worker.

The accelerated profile assigns each live connection context to one logical owner worker. That worker serializes stream, flow-control, ACK, loss, path, and key-promotion commits. Other workers MAY perform immutable parsing, cryptography, copying, and descriptor preparation, but publish results to the owner with the expected context generation. No asynchronous worker directly mutates live transport state.

A worker may own many connections. The design does not require one operating-system thread per connection. The hot path uses bounded pools, preallocated rings, checked indices, and bounded work quotas. It MUST NOT block on an application-held mutex, an unbounded allocator operation, storage I/O, a trust lookup, or a PQ cryptographic job. A portable non-accelerated implementation may use conventional allocation and locking without changing the protocol, but cannot claim these scheduling properties merely by implementing the wire format.

Ordinary NIC RSS normally follows network-flow fields; it MUST NOT be assumed to understand the GRT CID. A deployment that needs CID steering must supply and verify an appropriate steering program. NAT rebinding, migration, and multiple connections sharing a UDP tuple can defeat simple tuple-to-worker assumptions. A receiving queue worker that is not the connection owner forwards an owned descriptor or a copied packet to the owner. Returning the underlying memory remains the responsibility of the pool that owns it. Queue-bound AF_XDP redirect restrictions still apply [R21].

The owner first drains bounded I/O-result and release-completion work, then authenticated receive work, and then timers and transmit work in the order of Section 23. Separate quotas prevent a continuous receive stream from starving completions or loss timers. Opportunistic batching processes whatever work is already available; it MUST NOT wait for a minimum batch fill. A busy-poll implementation may avoid scheduler wakeups, but the specification makes no claim that a general-purpose operating system guarantees a 50-microsecond worst-case wakeup. An implementation claiming the low-latency timing profile MUST measure and declare its timer-dispatch bounds under its supported load.

### 29.3 Four independent lifetimes

Use distinct logical objects even when an implementation sometimes shares their backing pages:

| Object | Purpose | Retirement condition |
|---|---|---|
| S: reliable source | Immutable application bytes needed for new transmissions or retransmission | All covered obligations resolved or abandoned, and every source reference released |
| P: protected output | Exact header, ciphertext, and tag for one seal, or several seals in a superbuffer | Backend no longer owns any covered bytes; every crypto, DMA, and local view reference released |
| I: ingress storage | Kernel/NIC receive buffer and normalized child views | All child processing, deferred transactions, and direct application leases complete |
| R: retained receive data | Committed stream bytes and validity metadata | Application consumption or stream disposal permits reclamation, with no outstanding application lease |

A peer ACK can resolve S without releasing P, because the kernel might still be reading P. Conversely, a NIC completion can release P while S remains necessary for retransmission. Successful receive authentication does not release I when another child view still refers to the same parent. Application consumption of R does not automatically free an unrelated ingress buffer.

For a copying application write, source ownership transfers to transport-owned S before the API reports acceptance; the application may then reuse its original input. For a borrowed write, the application MUST keep the borrowed bytes immutable until an explicit source-release event. Acceptance, source-release, peer packet acknowledgment, reliable-byte acknowledgment, and peer application consumption are different notifications. GRT itself does not establish the last of these. The implementation API MUST name the distinctions, not report all of them as a generic completion.

In-place encryption MUST NOT overwrite the only retained copy of S. A practical baseline encrypts from S into P. Direct application-to-NIC loans are permitted only when their lifetime spans every required crypto and I/O reference, and the implementation still retains a valid retransmission representation. Retaining old ciphertext alone is not such a representation: network retransmission requires a fresh packet number and fresh packet protection.

On receive, the baseline copies authenticated stream bytes into R before RX_COMMIT and returns I promptly. A zero-copy receive optimization must provide the same byte-overlap checks, invisible staging, fixed receive-window bounds, and stable application leases as the ring model. Merely exposing an AF_XDP or provided-buffer pointer is not equivalent to a committed stream ring.

### 29.4 Lease records and generation fencing

Every shared memory object has an owner record containing at least:

```
object_id:u64
object_generation:u32
pool_id:u32
capacity:u32
state:u16
reference_classes: {application, crypto, io, dma, child_view, transaction}
reference_count[class]:u32
```

Reference increments are checked for overflow. A backend publication takes its I/O reference **before** the kernel/NIC can observe the descriptor. A release decrements only the reference owned by that exact operation. Release is idempotent at the operation-record level; duplicate or stale completions cannot decrement a new object's counter. Generation wrap requires complete pool quiescence or permanent retirement of the affected identifier.

A memory return is legal exactly when the object's semantic retention condition is satisfied, all class counts are zero, and every relevant backend fence has completed. A counter reaching zero is insufficient if a device can still DMA through a published descriptor. Conversely, a completion-ring slot becoming free does not return the memory described by an outstanding operation.

Queue publication uses the release/acquire ordering required by the corresponding kernel/NIC API. Cross-worker queues similarly publish initialized descriptors before ownership transfer. The protocol does not require atomic instructions for exclusively owner-local state. It requires an observable serialization equivalent to the specified FSMs.

The following local, little-endian 128-byte child descriptor is the reference adapter layout. It is not transmitted and is not part of the peer protocol. Implementations may use another internal layout while preserving every represented dependency.

| Offset | Bytes | Field |
|---:|---:|---|
| 0 | 8 | descriptor_id |
| 8 | 4 | descriptor_generation |
| 12 | 4 | connection_slot |
| 16 | 4 | connection_generation |
| 20 | 4 | path_generation |
| 24 | 4 | epoch |
| 28 | 2 | path_slot |
| 30 | 2 | key_slot |
| 32 | 4 | key_generation |
| 36 | 2 | selected_cipher |
| 38 | 2 | local_flags |
| 40 | 8 | packet_number |
| 48 | 8 | buffer_id |
| 56 | 4 | buffer_generation |
| 60 | 4 | buffer_offset |
| 64 | 4 | datagram_length |
| 68 | 4 | logical_reference_table_index |
| 72 | 2 | logical_reference_count |
| 74 | 2 | batch_child_index |
| 76 | 4 | batch_generation |
| 80 | 8 | batch_id |
| 88 | 8 | eligibility_time_us |
| 96 | 8 | backend_release_time_us |
| 104 | 8 | acceptance_observed_time_us |
| 112 | 8 | io_operation_id |
| 120 | 4 | adapter_generation |
| 124 | 2 | local_state |
| 126 | 2 | zero |

Opaque IDs are resolved through bounded owner tables. They are not blindly dereferenced as pointers. An operation that covers several children owns a vector of these descriptors and a distinct parent-buffer lease. Its immutable side record contains `pmtu_generation:u32`, `segment_size:u16`, `child_count:u16`, `total_bytes:u32`, the address/route/marking compatibility key, and the corresponding per-child reservation identities. A nonbatched send uses the same side record with child_count=1. The descriptor's path_generation is the path-lifetime generation; it is not substituted for pmtu_generation.

### 29.5 I/O acceptance, accounting, and ACK-before-completion

The core recognizes these separate events:

```
IO_PUBLISH(op, children, release_time)
IO_ACCEPT(op, accepted_children)
IO_REJECT(op, definitely_unaccepted_children, reason)
IO_RELEASE(op, released_buffer_leases)
PEER_ACK(packet_number)
```

`IO_PUBLISH` binds each child to its unique already-created preparation reservations, obtains the required immediate-release pacing reservation, rechecks the current congestion/amplification gates, and changes its ledger state to SUBMITTED. It does not allocate a second copy of a reservation already held by TX_RESERVE or charge a seal twice. Reservation prevents subsequent work from spending the same credit while acceptance is unresolved. `IO_ACCEPT` converts the relevant reservations to emitted accounting exactly once. `IO_REJECT` removes only reservations that are definitively unaccepted; consumed sealing quotas and packet numbers are not restored. `IO_RELEASE` changes memory ownership, never reliable delivery or congestion history. Adapters with a synchronous publication/acceptance boundary may perform the first two events in one owner transaction.

For a normal UDP send, full-length acceptance means the submitted datagrams entered the local send path, not that they reached the wire or peer. A later qdisc, device, or network drop is recovered as loss. The sender cannot reconstruct a failed UDP send by submitting a byte suffix. An unexpected short positive byte result violates the adapter's datagram assumptions: stop new admission on that adapter, preserve owned memory, and conservatively treat every possibly handed-off child as emitted until the adapter's failure boundary is resolved. Do not invent partial-child acceptance from a byte count.

A peer can receive and acknowledge a packet before the sender's asynchronous acceptance CQE is observed. When an otherwise valid authenticated packet ACKs a SUBMITTED entry, RX_PREFLIGHT records a dependency on that operation and moves the whole packet to RX_ACK_WAIT. It does not partly apply that packet's stream frames, flow credit, ACK ranges, or replay bitmap. At most eight such transactions per connection may wait in this profile; these slots and their ingress leases are admission-backed. Lack of capacity causes an unacknowledged local drop, not an ACK_RANGE_ERROR. Completion processing has priority so the wait normally ends promptly.

After acceptance resolves, repeat full semantic preflight against current state; do not reuse tentative credit calculations. An ACK for a definitively rejected/burned packet is invalid when still verifiable. An ambiguous post-handoff outcome is treated as emitted and eligible for normal ACK/loss processing. A local I/O watchdog may trigger adapter recovery, but cannot fabricate rejection or release a kernel-owned buffer merely because a CQE is late.

Only accepted children contribute to bytes_in_flight and path transmission ordinals. Reserved bytes are independently subtracted from available sending credit. Loss timers run against emitted entries, not unresolved local queue work. An accepted child retains the conservative timestamp captured immediately before backend publication, unless a supported more accurate timestamp was installed before any RTT/loss use. CQE observation time would underestimate network-plus-local-queue delay and MUST NOT replace it.

### 29.6 Progress reserves and teardown

Each backend reserves ingress descriptors, control-send descriptors, cryptographic jobs, and owner-mailbox slots sufficient to make progress on ACKs, key confirmation, close, and ownership reclamation. An application that holds every available receive lease must not also consume the only resources needed to return a lease or process a completion. Per-connection admission and per-worker quotas bound this condition; reliable credit is not enlarged merely because more UMEM or provided buffers exist.

Adapter teardown is a separate FSM:

```
ACTIVE → STOP_ADMISSION → CANCEL_REQUESTED → DRAINING_IO
       → DEVICE_OR_KERNEL_FENCED → RELEASE_POOLS → RETIRED
```

Cancellation requests are not completion fences. Connection close stops new handoffs but still permits matching send results, notifications, receive termination CQEs, and DMA completions to retire ownership. No UMEM unmap, registered-buffer removal, key erase, connection-slot reuse, or descriptor-generation reuse occurs while an operation can still access it. If a device/kernel reset is required, the adapter must use that API's documented ownership-return barrier. A timeout alone is not a barrier.

<a id="section-30"></a>

## 30. UDP GSO and GRO buffer-management profile

### 30.1 Independent packet construction

A transmit superbuffer is a concatenation of already complete child UDP payloads. For segment size S and total byte length L:

```
N = ceil(L / S)
child[i] = superbuffer[i*S : min((i+1)*S,L)]
```

Every child except possibly the last has length S. The last has length in 1..S, and must independently satisfy all GRT packet-length rules. S excludes IP and UDP headers. In this profile S is a multiple of eight, lies in 1200..4096 for padded DATA batches, and does not exceed the validated path/peer maximum. Smaller packets are sent without GSO or as a valid final child. A datagram is never split across two children.

For example, independently sealed lengths 1200, 1200, and 600 can form L=3000 with S=1200. Lengths 1200, 600, and 1200 cannot form that batch: segmentation would cut the second and third packets. A final child shorter than the minimum DATA encoding is invalid even if the segmentation API accepts it.

Each child has its own visible 32-byte header, packet number, plaintext frames, AEAD invocation, and 16-byte tag. Its payload_length is its own ciphertext-plus-tag length, not the superbuffer length. Padding is constructed as an authenticated PADDING frame **before sealing**. No raw zeros are appended after a tag to reach S. Frame counts, alignment, and padding/congestion rules apply to each child separately.

Ciphertext may be prepared in multiple I/O vectors, but the segmentation API sees their concatenated byte stream. An iovec boundary is not a UDP boundary. An implementation may use a contiguous superbuffer for simpler DMA or a verified scatter/gather representation; neither may change segmentation offsets.

### 30.2 Batch compatibility key

The baseline combines children only when all of these match:

```
connection generation; path slot and generation; address family;
source/destination IP and UDP port; interface/routing context;
CID; selected cipher; key epoch and key generation;
PMTU generation; segment size; DSCP and ECN policy;
pacing/release class; adapter generation and verified offload capabilities
```

This strict grouping is a local implementation rule that reduces stale-context hazards. A receiver must still demultiplex each child independently; a kernel GRO aggregate can contain packets using different CIDs or epochs on the same UDP tuple. It cannot assume the transmitter used the baseline grouping.

CID rotation, key promotion, PMTU change, migration, or adapter reconfiguration seals the current batch boundary. Unsubmitted incompatible work is rebuilt or submitted individually if it remains valid. Submitted bytes are immutable. Header fields, padding, tags, or segment size are never edited in place while the kernel/device might read the superbuffer.

Do not mix congestion-exempt ACK-only packets with congestion-charged packets in one baseline GSO release. ACK-only packets are normally sent separately or coalesced as frames into already needed eliciting packets. A PING introduced solely to permit padding creates a real eliciting, congestion-charged packet; it does not create a free segmentation filler.

### 30.3 Size and capability limits

The software-profile maximum is 64 child datagrams per submission and 65507 total UDP-payload bytes. The actual allowed count is the minimum of these bounds, the OS/driver/NIC limits, available descriptors, cryptographic reservations, and the pacing gate. At S=1200, at most 54 full-sized children fit the conservative byte cap; a 64-child array is not permission to submit 76800 bytes. At S=4096, at most 15 full children fit; an additional shorter final child is possible only within every bound.

These aggregate limits are local safety limits, not a negotiated maximum packet size and not a new wire MTU. A device may support less. The adapter probes or configures its verified mode at initialization and records its fallback. Kernel software segmentation is acceptable. The protocol MUST work with ordinary UDP datagrams when hardware GSO is unavailable [R20].

For per-message UDP_SEGMENT, populate the native Linux control-message structure with the ABI's host-endian 16-bit segment-size value; do not reuse a big-endian wire encoder. Validate control-buffer length and alignment. A per-socket setting affects concurrent sends and is therefore appropriate only when its ownership/configuration is serialized. The sendmsg adapter avoids UDP_CORK and MSG_MORE in the baseline so that hidden corking cannot merge unrelated submissions.

### 30.4 Submission and local retries

Before submission, reserve all resources for the whole immediately released batch and record each child's independent ledger entry. If any child fails the release gates, reduce the batch at a child boundary or delay it; do not partially seal an untracked trailing child. Send acceptance commits each accepted child separately, with a shared operation record solely for completion management.

For sendmsg, a successful full-length result accepts the complete superbuffer. For sendmmsg, a successful return counts accepted **message entries**, each of which may itself be a GSO superbuffer. The unsent suffix remains unaccepted. No assumption is made that an error applies to a particular child inside a previously accepted message [R25].

An unchanged sealed submission that was **definitively not accepted** may be retried byte-for-byte without invoking AEAD again. This is a local submission retry, not network retransmission. Its allocated number and key-usage charge remain consumed, and its descriptor is not reused for a different packet. If the plaintext, header, epoch, segmentation geometry, or logical obligation changes, cancel that sealed object and build a fresh-number packet. When acceptance is uncertain, never resubmit the old packet as though nonacceptance were known; conservatively account it as emitted and recover live logical items through normal fresh-number retransmission.

For copying sends, the full send result releases the caller's protected-output lease according to the socket API, while S remains independently retained. Zero-copy sends use an explicit notification mechanism; a send return alone does not release that output. The io_uring mapping is in Section 32. Traditional MSG_ZEROCOPY is not implicit in this profile and requires an additional error-queue completion adapter [R26].

### 30.5 Segmentation is not pacing

The default pacer retains the Section 15 token bucket with capacity 2*M. Before an unshaped GSO handoff, the **sum of all immediately released children** must fit current tokens, congestion availability, and anti-amplification availability. At a full two-datagram burst, a larger prepared batch remains queued; it is not emitted simply to reduce syscall count.

A deployment MAY use a downstream per-child pacer. It must demonstrate that the scheduler segments first and enforces the same release envelope on individual datagrams, including after an idle interval, queue contention, and timer delay. One skb-level launch time, one io_uring timeout, or ordinary UDP_SEGMENT support does not demonstrate this capability. Until such a scheduler is verified, the plain-GSO rule applies.

Downstream scheduled children reserve credit before publication. They may not be counted as newly eligible merely because software no longer sees their descriptor. The implementation manifest specifies bounded scheduled queue residence and how send timestamps, cancellation, PMTU changes, and hardware failures map to Section 29.5. It must not fill an arbitrarily long NIC queue to amortize one submission.

SIMD cryptographic batches may be larger than an immediately released network batch. Preparing 32 independent seals and releasing them two at a time is legal when all memory, nonce, and credit reservations are properly bounded. Batch size is an optimization parameter; network burst size remains a transport invariant.

### 30.6 GRO normalization and parent ownership

A recvmsg result with valid UDP_GRO ancillary metadata is a parent I buffer. The adapter reads the ABI-encoded segment size, verifies the total receive length and metadata, derives all child ranges, and bounds their count and arithmetic before presenting any child. Without that metadata, the result is exactly one UDP datagram. The receiver MUST NOT guess boundaries by scanning for the magic value.

For DATA children, validate the child's length, fixed header, payload_length, and negotiated packet maximum independently. A short final child is permitted only when it is a complete valid packet. Bootstrap children remain exactly 1200 bytes. MSG_TRUNC, unavailable payload bytes, or truncated/malformed ancillary metadata needed to recover boundaries causes the affected receive result to be discarded without transport acknowledgment. Receive buffers must be sized for the configured aggregate limit, not merely for one 4096-byte datagram.

The baseline admits at most 64 children per receive aggregate and declares a local aggregate-byte ceiling; a provision of 64*4096 bytes covers that profile ceiling. This is not a promise about every kernel's maximum GRO aggregation. A stack capable of larger aggregates must be configured within the admitted limit, given larger explicitly bounded local storage, or run with GRO disabled. A conforming wire packet is not made a protocol error because a local aggregate exceeded a local pool bound.

Normalization creates one parent lease plus child-view references. The parent returns to its pool only after the last child view, deferred RX transaction, crypto job, and application loan is released. A worker may copy child bytes into a dedicated staging buffer to drop its parent reference earlier. It may not overwrite a failed child's range while a vector crypto operation still reads adjacent ranges from the same parent.

Authentication or semantic failure of one child does not automatically invalidate authenticated neighboring children. Each child's RX_COMMIT is independent. If child 0 commits and child 1 later fails, child 0 is not rolled back. If the parent boundaries themselves cannot be established safely, none of its children is admitted.

### 30.7 Metadata preservation

The normalized descriptor includes the actual UDP length, local/remote tuple, interface context, receive-observation time, and usable ECN information. Per-child metadata must be correct, not synthesized for convenience. A common observation time may be used for a burst when finer timestamps are unavailable; it is not represented as a measured per-child wire-arrival time.

ECN requires particular care. A single aggregate control message may be used for all children only when the tested stack guarantees the represented marking is valid for all of them. Otherwise disable GRO for an ECN-enabled connection or disable ECN capability before negotiating it. An adapter must not turn unknown markings into invented ECT/CE counts. Existing migration-related ECN disablement remains unchanged.

Unvalidated ICMP or error-queue reports cannot directly install an arbitrary PMTU. A valid PMTU update increments the path generation used by prepared batches. EMSGSIZE for a definitely unaccepted GSO request permits safe cancellation/repacketization, but the aggregate size itself is never treated as the on-wire datagram size. Peer-visible retransmission always carries fresh packet numbers.

### 30.8 GSO parent FSM

| State | Event and guard | Action | Next |
|---|---|---|---|
| EMPTY | Compatible work available | Allocate bounded parent and child records | BUILDING |
| BUILDING | Child construction complete | Check geometry and independent seal inputs | SEALING |
| SEALING | All jobs succeed with matching generations | Freeze protected bytes; retain all required leases | READY |
| SEALING | Job fails or becomes stale | Burn consumed numbers/usage; wait for jobs before release | CANCELING |
| READY | All release gates pass | Publish operation and per-child SUBMITTED entries | SUBMITTED |
| READY | PMTU/path/key/configuration invalidates batch | Cancel or split only at complete child boundaries | CANCELING |
| SUBMITTED | Definitive acceptance result | Commit accepted child accounting; retain I/O-owned bytes | IO_OWNED or RELEASABLE |
| SUBMITTED | Definitive nonacceptance | Cancel unaccepted children; retain outstanding I/O pins if any | CANCELING |
| IO_OWNED | Matching buffer-release completion | Release exactly this operation's pins | RELEASABLE if no pins remain |
| CANCELING | All jobs/results/fences settled | Resolve reservations without restoring consumed nonces | RELEASABLE |
| RELEASABLE | No child view or other owner remains | Return parent memory and descriptor | EMPTY |

Unexpected results quiesce the adapter without prematurely returning memory. GSO parent retirement never resolves a reliable stream obligation by itself.

<a id="section-31"></a>

## 31. AF_XDP buffer-management profile

### 31.1 Capability and network-stack boundary

The AF_XDP adapter binds an XSK to a specific netdev queue and registers a bounded UMEM. It declares whether it uses native zero-copy or copy mode, whether multi-buffer packets are enabled, supported fragment counts, checksum metadata capabilities, and wakeup policy. Requesting zero-copy does not establish that the NIC/driver supports it; initialization must verify the resulting mode. Copy mode is a valid fallback but must be reported as such [R21].

AF_XDP delivers link/network packet storage rather than ordinary socket UDP payloads. The adapter or its surrounding stack must therefore supply correct Ethernet/IP/UDP handling, source addressing, routing, neighbor resolution, checksums, MTU handling, and applicable firewall/admission policy. The GRT parser is invoked only after a bounded IP/UDP validation layer establishes the payload boundary. IPv6 extension parsing is bounded; unsupported fragmentation is not silently interpreted as a complete GRT packet. UDP checksums are verified/generated as required for the address family, unless a verified metadata contract performs the exact operation in hardware.

The redirect program must distinguish its intended UDP traffic and leave unrelated traffic to the host stack or another explicitly configured owner. Ordinary host applications and AF_XDP may not independently own the same ingress packet. XSKMAP redirect is queue-constrained; a socket bound to another queue is not an arbitrary cross-queue delivery mechanism. Forward a normalized owned descriptor between workers after reception when required.

### 31.2 Rings and UMEM ownership

FILL and TX are user-produced rings; RX and COMPLETION are user-consumed rings. Their single-producer/single-consumer requirements apply to each ring even when UMEM is shared. Sharing memory does not make these rings multi-producer. The implementation assigns one ring owner or supplies explicit serialization outside the ring API [R21].

The reference UMEM-frame FSM is:

| State | Owner | Legal transition |
|---|---|---|
| FREE | Pool | To FILL_POSTED or TX_BUILD, once |
| FILL_POSTED | Kernel/NIC receive path | To RX_OWNED when RX delivery transfers the frame |
| RX_OWNED | Receive worker | To RX_HELD for crypto/child processing; or FREE after references drain |
| RX_HELD | Worker plus explicit leases | To FREE only after every view/job/loan is released |
| TX_BUILD | Transmit worker | To TX_POSTED after full packet construction and validation; or FREE on pre-publication cancel |
| TX_POSTED | Kernel/NIC transmit path | To TX_RETURNED on the matching COMPLETION event |
| TX_RETURNED | Pool after completion | To FREE when non-I/O references are also zero |

Advancing the RX-ring consumer index releases a ring slot; it does not post the frame back to FILL. Advancing the TX producer publishes access to the frame; it does not release it. A UMEM address may not simultaneously appear in FILL and TX, appear twice in FILL, or be reused while held by the application. Shared UMEM uses a global frame-lease table or disjoint ownership partitions to prevent aliasing between queues.

Canonicalize frame addresses according to the configured aligned/unaligned UMEM addressing mode before lookup. An address plus offset is not a new frame identity. Store a generation with every lease record; completion addresses resolve against the outstanding publication table, not an inferred new generation. Reusing an address before its old completion is processed defeats generation checking and is forbidden.

### 31.3 Single-buffer and multi-buffer packets

The single-buffer baseline requires enough headroom and data space for the complete link/IP/UDP/GRT packet. A 4096-byte UMEM chunk does not automatically accommodate a 4096-byte UDP payload once link headers and headroom are included. Advertise a smaller multiple-of-eight max_udp_payload, allocate a larger supported chunk, or enable a verified scatter/gather mode. Never advertise a receive limit that the configured packet buffer geometry cannot represent.

With supported multi-buffer operation, XDP_PKT_CONTD indicates continuation within **one network packet**. Accumulate fragments until the terminating descriptor, preserving all fragment leases. A user-space RX burst can stop partway through a packet even when the ring contains the remainder; parser state must persist across that burst boundary. No AEAD or transport commit occurs until the complete packet and its single tag are available [R21].

Bound the assembled length and fragment count by the configured limits. A continuation chain that exceeds either bound is discarded as one packet and every owned fragment is eventually returned. The implementation does not interpret each fragment as a separately numbered GRT datagram. On TX, publish a complete valid chain with the configured continuation flags; descriptor budgeting counts fragments, while congestion and AEAD budgeting count packets.

GRT permits gather/scatter crypto only if the provider authenticates the exact concatenated AAD and ciphertext with the specified tag. Otherwise gather into bounded P/I scratch memory. A fragmented packet cannot be partially delivered to a stream merely because its first fragment contains a complete frame.

### 31.4 Receive strategy and starvation control

The baseline receives into UMEM, authenticates in private/quarantined storage or in an exclusively owned frame, copies reliable payload into the stream's invisible R staging storage, and performs RX_COMMIT. It then releases the UMEM lease as soon as all child/job references are gone. This deliberate copy keeps application retention and network receive replenishment independent.

An application-readable UMEM loan is optional, not the baseline. It consumes a separately admitted lease quota, may not expose unauthenticated bytes, and must preserve ordered stream semantics and overlap verification. Holding UMEM until peer ACK is neither necessary nor correct for receive storage. Holding all receive frames until application consumption can starve FILL; a conforming adapter prevents that through quotas or copying.

Maintain separate or reserved quotas for FILL replenishment, control traffic, transmit output, handshake admission, and application loans. Poll RX, TX COMPLETION, and owner mailboxes fairly. A large incoming burst cannot indefinitely defer COMPLETION processing, because that would artificially exhaust output storage and block ACK/key progress.

### 31.5 Transmit acceptance and completion

For AF_XDP, each complete packet or fragment chain contains its own Ethernet/IP/UDP headers and independently protected GRT payload. There is no generic UDP_SEGMENT ancillary operation on an XSK. Publishing a burst of 16 complete packets is batching, not GSO; publishing three fragments for one packet is scatter/gather, not three GRT transmissions.

Before TX publication, validate lengths, addresses, flags, fragment termination, selected cipher, epoch, path/PMTU generation, checksums, and each packet's release gates. Take all frame references, then publish the complete chain with the required memory ordering. A needs-wakeup optimization may omit an unnecessary kick only according to the configured ring contract; it does not change ownership.

The profile conservatively treats valid TX publication as the emission-accounting boundary. The output is no longer safely mutable, so its children become EMITTED and consume their reservations at that point. Driver or device drops after publication are recovered as loss. This convention does not claim that the frame has already reached the physical link.

A TX COMPLETION entry transfers UMEM ownership back. It is **not** a packet ACK, proof of physical transmission, or proof that the descriptor was valid; the kernel documentation expressly allows completion return for invalid descriptors [R21]. Monitor invalid-descriptor/drop counters. An invalid-descriptor report triggers adapter diagnosis/quiescence, not release of reliable source obligations or rollback of previously committed packet-number use.

The S source buffer remains retained until its logical bytes are resolved, while P in UMEM can return as soon as the completion and remaining references permit. A retransmission reads S and constructs new protected output with a new packet number. Reusing a completed UMEM frame to hold that new output is legal only under a new lease generation.

### 31.6 Shutdown and device faults

Stopping polling is not a DMA fence. Before UMEM unregistration or memory reuse, stop admission, cancel only unpublished work, drain or terminate published operations using the driver/kernel contract, and establish that the device can no longer access the affected mappings. Interface-down, device reset, and queue reconfiguration are separate fault paths whose fence semantics must be declared. No fixed timeout permits recycling a frame still potentially visible to a DMA engine.

A stale completion cannot update a recycled connection. A completion still legitimately owning an old frame must nevertheless discharge that old frame's lease even when the connection has closed. Keep operation-lifetime ownership records separate from live connection lookup until cleanup completes.

<a id="section-32"></a>

## 32. io_uring buffer-management profile

### 32.1 Capability model and operation identities

The io_uring adapter uses ordinary UDP sockets and therefore may combine asynchronous submission with UDP_SEGMENT and UDP_GRO. It probes supported opcodes, registration features, and the actual socket/offload combination. This specification does not require a particular kernel release merely by name; a deployment records tested versions and fails or falls back when required behavior is absent.

Each operation has a unique 64-bit user_data token indexing an operation record with a generation. The record identifies the adapter generation, connection generation if applicable, buffer leases, child descriptors, expected result kind, and terminal-completion state. A token is not reused while any completion or notification can still refer to it. Generation wrap requires a fence or retirement.

The baseline permits at most one unresolved transmit **acceptance** operation per connection. Later buffer-release notifications may remain outstanding without blocking subsequent send acceptance. This bounds local uncertainty and packet-order distortion while preserving zero-copy pipeline depth across connections and across already accepted operations. More aggressive per-connection concurrency requires an implementation-specific bounded ordering analysis that still satisfies replay-window and Section 23 semantics.

### 32.2 Provided receive buffers

The baseline receive operation is multishot recvmsg with buffer selection. recvmsg, rather than a bare payload-only receive, is needed when source addresses, UDP_GRO boundaries, and ancillary metadata must be retained. IOSQE_BUFFER_SELECT is required by the multishot buffer-selection interface, and MSG_WAITALL is not used [R22].

A provided-buffer ring entry grants the kernel access to that buffer after the entry is published. The adapter assigns an ID, generation, capacity, and current lease state. Adding an entry to the ring without publishing its tail does not yet grant ownership; publishing it does. On a CQE with IORING_CQE_F_BUFFER, the selected buffer ID identifies the consumed buffer. Do not infer the selected buffer from submission order [R24].

Provided-buffer group and buffer IDs use the native API width and bounds (16-bit IDs in the specified Linux interface); an ID is interpreted only within its registered group and live generation. The registered ring size is a supported power of two, validated at registration. No local identifier truncation or wrap can alias an owned buffer.

The baseline uses whole-buffer consumption. Optional incremental-buffer and receive-bundle modes are outside this profile unless an adapter independently specifies their additional ownership and packet-boundary rules. Registering memory or using a provided-buffer ring is not itself proof of zero-copy NIC-to-user receive; these are allocation/ownership mechanisms, not a universal receive-offload guarantee.

The received memory starts with io_uring_recvmsg_out and associated name/control regions before the payload. Validate the result using the documented recvmsg helpers or an exactly equivalent bounds-checked decoder. CQE res is not blindly treated as the UDP payload length. Check the recorded name, control, and payload lengths against the selected buffer's capacity, and reject truncated payload or indispensable ancillary metadata before GRO normalization [R22].

IORING_CQE_F_MORE describes whether the multishot operation can produce more CQEs. It does not mean the selected buffer can be reused, nor that the current datagram continues in the next CQE. Inspect it even on error. A result without MORE terminates that operation; rearm at most one replacement after its state is recorded. A zero-length UDP payload is a received datagram to reject under the GRT grammar, not a TCP-style end-of-stream indication.

### 32.3 Receive buffer FSM and replenishment

| State | Event | Action | Next |
|---|---|---|---|
| FREE | Buffer selected for provision | Initialize ID, generation, address, capacity | PREPARED |
| PREPARED | Provided-ring publication | Transfer access to kernel | PROVIDED |
| PROVIDED | CQE selects buffer | Record exact returned extent and result flags | DELIVERED |
| DELIVERED | Valid recvmsg result | Normalize child views; acquire parent/view leases | HELD |
| DELIVERED | Invalid/truncated result | Discard without ACK; wait for any processing references | RETURNABLE |
| HELD | All views/jobs/waits/loans released | Remove transport references | RETURNABLE |
| RETURNABLE | Pool policy chooses reuse | Increment generation and reprovision | PREPARED |

Advancing the CQ head releases CQ storage, not the selected payload buffer. Re-providing a HELD buffer is forbidden even if its CQE has already been acknowledged to the ring. A GRO parent can stay HELD while one child waits for an asynchronous ACK dependency and the other children have completed; copy the waiting child to a bounded private buffer when appropriate to release the large parent earlier.

On ENOBUFS or a terminal receive error, record MORE/terminal status, replenish only actually returnable buffers, and rearm as necessary. Do not create a growing number of receive operations in response to a transient shortage. CQ draining has a reserved work quota; overflow, dropped CQEs, or loss of ownership accounting causes adapter quiescence rather than speculative buffer reuse. The implementation provisions CQ space for receive bursts and potentially multiple completions per outstanding zero-copy send.

### 32.4 Copying sends and GSO

Use SENDMSG when per-operation destination or UDP_SEGMENT ancillary data is required. The iovec array, control buffer, message metadata, and P output remain stable for the conservative lifetime through the send-result CQE; a provider-specific earlier metadata-release feature is not assumed by the baseline. Packet bytes remain immutable for every longer I/O lifetime required by the selected operation.

A full-length nonnegative send result accepts the whole submitted message. The adapter then applies IO_ACCEPT to each child and releases copying-send I/O references. A negative result rejects the operation only to the extent that the API documents definite nonacceptance. Error classification belongs to the exact syscall/opcode and publication stage, not errno alone. An adapter that cannot prove that none of a GSO request reached the send path accounts all potentially released children conservatively as emitted, retains any indicated I/O pins, and recovers logical items with fresh-number retransmission. Unexpected short UDP results use the fault rule of Section 29.5. A send-result CQE is not a peer ACK and never releases S merely because P was copied.

SENDMSG_ZC may be combined with UDP_SEGMENT only when the actual kernel/socket combination supports it. SEND_ZC without a message control buffer is not a substitute for per-call UDP_SEGMENT metadata; a serialized socket-level segmentation configuration is a separate supported mode. The implementation may choose copying SENDMSG for small packets or when zero-copy has excessive pin/notification overhead. This choice has no wire effect.

### 32.5 Zero-copy send results and notifications

A zero-copy send operation normally has a send-result CQE and, when indicated, an additional notification. The result's IORING_CQE_F_MORE bit tells the application that a notification will follow; a notification is marked IORING_CQE_F_NOTIF. Memory requiring that notification cannot be reused until it arrives. Not every request produces exactly two CQEs, so counting two unconditionally is incorrect [R23].

Process flags before interpreting res:

```
if flags & IORING_CQE_F_NOTIF:
    record matching notification; do not interpret res as sent bytes
else:
    record send result and acceptance classification
    expect_notification = bool(flags & IORING_CQE_F_MORE)

output_may_release = result_known
                  and (not expect_notification or notification_seen)
                  and all_non_io_references_are_zero
```

This rule applies on error as well as success. A negative result with MORE still requires the indicated release event. A copying fallback inside a zero-copy API does not permit the transport to ignore completion flags. Optional usage-report bits describe the zero-copy operation's behavior; they are not transport acknowledgments or byte-credit increments.

The baseline does not depend on cross-worker mailbox delivery preserving CQE order. If a matching notification reaches an ownership worker before the send result has been forwarded, record it in the operation record and retain the buffer until result_known. A notification for an unknown/reused generation is never allowed to free a current object. A duplicate result or duplicate notification is an adapter fault, not a second decrement.

| Send object state | Event | Acceptance effect | Memory effect |
|---|---|---|---|
| BUILDING | Publish SQE | SUBMITTED; retain reservations | Take I/O and metadata leases |
| WAIT_RESULT | Full send result with MORE | IO_ACCEPT children | Keep P pinned; wait notification |
| WAIT_RESULT | Full send result without MORE | IO_ACCEPT children | Release I/O lease when other guards permit |
| WAIT_RESULT | Definite error with MORE | IO_REJECT unaccepted children | Keep P pinned; wait notification |
| WAIT_RESULT | Definite error without MORE | IO_REJECT unaccepted children | Release I/O lease when other guards permit |
| WAIT_RESULT | Early forwarded notification | No acceptance decision | Record notification; retain P |
| WAIT_NOTIFICATION | Matching notification | No transport-state change | Release exactly this I/O lease |
| RELEASE_PENDING | Last non-I/O lease released | None | Return P; retire operation record |

A peer ACK may arrive in WAIT_RESULT or WAIT_NOTIFICATION. In WAIT_RESULT, use RX_ACK_WAIT as necessary. In WAIT_NOTIFICATION, the packet is already eligible for ordinary ACK processing, but P remains pinned. Reliable S can retire only under its own obligation/reference condition.

### 32.6 Cancellation, close, and ring destruction

An asynchronous-cancel CQE reports the cancel request's result; it does not automatically replace the target operation's terminal CQE or zero-copy notification. Keep all operation records and leases until the target's actual lifetime is settled. Close, timeout, or connection DRAINING does not erase this requirement.

A multishot receive may already have selected buffers when cancellation is requested. Process its remaining CQEs, preserve selected-buffer ownership, and observe termination before retiring the operation token. Removing a provided-buffer group or destroying the ring requires a documented kernel barrier that returns all relevant ownership. An implementation may quarantine memory during fault recovery rather than risk reuse, but must declare the bounded recovery/admission behavior.

Registered memory cannot be unregistered or reassigned to a different security context while a send, notification, selected receive, crypto operation, or application lease still refers to it. Deleting a connection lookup entry is not a substitute for these fences.

<a id="section-33"></a>

## 33. Cryptographic acceleration and scheduling contract

### 33.1 Portable and accelerated cipher implementations

Every client and server provides a functional XChaCha20-Poly1305 path even when AES hardware is absent or unavailable. An endpoint offering AES must have a conforming implementation capable of completing the selected connection. AES instructions, SIMD, multi-buffer software, an asynchronous crypto engine, a DPU, or an FPGA are implementation choices; none changes the selected algorithm or its encoding.

XChaCha's HChaCha subkey may be cached per directional epoch because the first 16 bytes of the constructed 24-byte nonce are constant for that epoch. The remaining eight bytes vary by packet number. Cache generation and key-slot references accompany every job; the cached subkey is secret and is erased with the parent epoch after all references drain. This optimization implements the exact specified XChaCha construction, not a negotiated downgrade to ordinary ChaCha.

AES-256-GCM requires exact 32-byte keys, 12-byte nonces, full tags, and the 32-byte GRT header as AAD. An accelerator advertised as supporting AES-GCM is usable only if its API supports the required nonce, AAD, payload, tag placement, lengths, and scatter/gather semantics. An IPsec/TLS-specific inline engine is not assumed to parse GRT. Generic AEAD lookaside acceleration and complete inline transport offload are different capabilities.

### 33.2 Job contract

Each crypto job binds:

```
operation_id and generation
connection generation
cipher ID; direction; epoch; key-slot generation
packet number; exact nonce bytes
AAD address/length (=32 for DATA)
input and output leases, offsets, and checked lengths
tag address/length (=16)
seal reservation identity or receive-authentication identity
```

Reserve nonce/key usage before publishing a seal job. A canceled job that may have executed still consumes its number and usage allowance. A batch crossing a key-use hard limit is split or rejected before publication; counters are never charged only once for the batch. Hardware or SIMD vector lanes cannot share a packet number merely because they share a key.

No provider may release unverified plaintext to RX_COMMIT or the application. Some APIs write tentative plaintext before tag verification; their output must remain quarantined until success. On failure, discard the tentative transaction and release/clear scratch according to local confidentiality policy. Authentication failure does not advance a replay window or let a claimed high packet number evict valid traffic.

Aliasing between input/output/AAD/tag ranges is permitted only when the provider explicitly supports that exact layout and no other owner can mutate or read speculative results. In particular, header bytes authenticated as AAD cannot be rewritten after crypto submission. An asynchronous completion checks every generation before publishing a result. A stale completion can discharge its old references but cannot modify a new connection or epoch.

### 33.3 Vectorization without mandatory queueing delay

Batch only work already available at the scheduling instant, subject to a bounded burst quota. Small batches and single packets are valid. A worker must not hold an interactive packet solely to fill a SIMD width, GSO superbuffer, TX ring burst, or io_uring submission batch. Under load, naturally accumulated work supplies larger batches.

The engine may group crypto jobs by cipher and key layout for efficiency, but it preserves each packet's owner, nonce, and completion identity. Completing crypto out of order does not by itself permit unbounded transmit reordering or mutation of owner state. The owner applies the Section 22/23 gates when ready output is actually released. Long-running handshake/PQ jobs use separate quotas so that session admission cannot starve established DATA protection.

Hardware acceleration may add DMA, pinning, queueing, and completion costs. The deployment measures the break-even packet size and concurrency rather than assuming that offload always reduces latency. Falling back from an accelerator to software is permitted for the same selected cipher and exact bytes, but outstanding jobs must be fenced; a possibly executed seal is not rerun with changed plaintext under the same nonce.

### 33.4 Backend switches and deployment manifest

Switching from sockets to io_uring, from copy to zero-copy, or from software AEAD to an accelerator is an implementation transition. It does not reset packet numbers, flow credits, epochs, stream handles, replay state, or congestion history. Fence old backend ownership and preserve context generations. A transport path change, when actually required by the switch, still follows path validation and PMTU rules.

The implementation manifest includes at least: packet/aggregate capacities; connection/stream admission; sent-packet and pending-ACK-wait capacity; source/receive/pin pools; per-worker queue depth; crypto provider and supported ciphers; key-use accounting; tested segmentation and metadata semantics; AF_XDP mode/chunk/fragment geometry; io_uring opcode and completion capabilities; CQ sizing; pacing mode; timestamp source; NUMA policy; teardown barriers; and supported timer-dispatch bounds. Measured throughput reports additionally name packet-size distribution, connection/stream count, RTT, loss, cipher, CPU cores, NIC queues, and copies per byte.

<a id="section-34"></a>

## 34. Software-profile verification and release gates

### 34.1 Required fault cases

The following cases are part of software-profile conformance testing, in addition to Section 27. A harness may emulate a kernel/NIC boundary, but a release must also exercise the actual selected backend.

| Case | Required observation |
|---|---|
| GSO with three valid children and short final child | Independent packet numbers/tags and ACK/loss records; no aggregate packet identity |
| Short middle child or post-tag padding | Rejected before submission; no silent segmentation across packets |
| 64*1200-byte socket aggregate | Rejected by conservative aggregate-byte bound despite child-count bound |
| GSO burst exceeds two-M tokens without child pacer | Delayed/split; never treated as an offload exemption |
| One corrupted tag in a valid GRO aggregate | No commit for that child; independently valid children may commit |
| GRO without reliable segmentation metadata | Treated as one UDP datagram or dropped; never magic-scanned |
| Mixed CID/epoch children in one receive aggregate | Independent lookup and generation checking |
| EAGAIN before acceptance | No emitted packet accounting; no nonce reuse with changed bytes |
| sendmmsg accepts a message prefix | Only that prefix's children accepted; no byte-offset retry |
| PMTU changes after sealing but before submission | Old prepared geometry invalidated; no in-flight bytes mutated |
| ACK arrives before send-result CQE | Bounded RX_ACK_WAIT, then full preflight; no false unsent-ACK error |
| Peer ACK arrives before zero-copy notification | Reliable obligation may resolve; protected output stays pinned |
| Negative send result with MORE | Reservation rejected when definite; output remains pinned until notification |
| Notification/result forwarded in reverse order | No early release and no double decrement |
| Multishot receive terminates or reports ENOBUFS | Correct MORE handling; bounded replenish/rearm |
| CQ consumer head advances with held receive buffer | Buffer remains unavailable for re-provision |
| AF_XDP RX burst ends mid-fragment chain | Continue assembly on later burst; no partial packet commit |
| AF_XDP completion for invalid TX descriptor | Memory returned safely; no inferred peer delivery |
| App holds receive leases to its quota | Control/completion progress remains possible; no unbacked receive credit |
| Crypto completes after close/key retirement begins | Old job releases its references; no new-context mutation |
| Reset/ring destruction with in-flight DMA or notifications | Memory reuse blocked until documented ownership barrier |

### 34.2 Safety properties for a model checker or RTL/software harness

At every publication/commit boundary, assert:

```
A01: no live key/direction/packet-number tuple has two different seal inputs
A02: no child has two emission-accounting commits
A03: no buffer is FREE while any owner or device fence remains
A04: no reliable source is freed while an unresolved byte obligation remains
A05: no aggregate completion substitutes for per-child ACK/loss accounting
A06: no DATA receive state changes before authentication and atomic commit
A07: no unknown or stale generation can mutate a live context
A08: no unshaped release exceeds the token-bucket envelope
A09: no pending acceptance reservation is spent twice
A10: no selected cipher changes after handshake commit
A11: no re-provided RX buffer still has a live child/transaction/application lease
A12: no cancellation result alone releases a target's outstanding I/O ownership
```

Formal verification should explore cross-products of close, retransmission, key update, migration, completion delay, local resource exhaustion, and packet reorder/loss. Testing only the happy-path cryptographic functions is insufficient. The reference mechanics and assertion templates accompanying this specification are bounded executable checks, not a complete implementation, a full handshake interoperability suite, an RTL synthesis result, or a proof of the cryptographic composition.

### 34.3 Capacity accounting

GSO reduces submission overhead; it does not reduce the number of outstanding logical packets, reliable source bytes, or replay/ACK decisions. A sender retaining a rate R bit/s for an RTT T seconds needs approximately R*T/8 bytes of outstanding source capacity before headroom, subject to flow-control and workload details. A 100-Gbit/s, 10-ms example is 125 MB, unchanged by placing the packets into superbuffers.

Size protected-output memory for sealed-but-unsubmitted work plus accepted output awaiting ownership return, not for network ACK delay unless the implementation unnecessarily couples those lifetimes. Size R for advertised receive credit and application consumption behavior. Size I/UMEM/provided buffers for posted receive capacity, processing leases, outstanding transmit leases, and explicitly reserved progress margins. Count overlapping partitions once: a single frame cannot simultaneously supply free FILL capacity and outstanding TX capacity.

Packet metadata capacity follows packet rate times the relevant retention interval, with additional allowance for delayed ACK/loss history and outstanding local operations. A GSO operation containing N children needs N packet records even if it needs only one syscall or SQE. AF_XDP scatter/gather consumes multiple descriptors for one packet; io_uring zero-copy may consume more than one CQE per request. Neither ring depth nor byte BDP alone is a sufficient sizing specification.

### 34.4 Integrated design constraints

The current specification incorporates the following constraints throughout its wire and execution definitions; they are not optional corrective modes:

| Constraint | Normative integration |
|---|---|
| One transport identity | GRT and General Reliable Transport throughout names, magic, labels, and artifacts |
| Mandatory interoperable cipher | XChaCha20-Poly1305 always offered/enabled; optional authenticated AES-256-GCM selection |
| Downgrade-resistant transcript binding | Ordered offer, support mask, selection, and service/role bindings authenticated together |
| Simple packet datapath | Clear fixed header as AAD; no separate header-protection job |
| Exact nonce domains | Cipher-specific IV length, separate early/ticket domains, immutable connection cipher |
| GSO-aware reliability | One independent AEAD/replay/ACK/loss/usage unit per child datagram |
| Congestion-safe batching | Existing per-child pacing and congestion requirements survive aggregation |
| Correct asynchronous memory release | Acceptance, output release, reliable ACK, and consumption remain distinct |
| Bounded unknown-send handling | SUBMITTED ledger and transactional ACK wait instead of false ACK violations |
| High-rate ACK behavior | Authenticated ratio ceiling, bounded delay, immediate loss/CE overrides |
| Safe software/hardware interchange | Same packet/FSM semantics; capabilities and completion barriers are explicit |

### 34.5 Security and implementation qualification

This is a complete integrated design document, not a claim that a new cryptographic protocol has already received independent security review. The PQXDH-derived online exchange, cipher negotiation, resumption, transcript construction, and refresh composition require independent analysis. Mandatory XChaCha is an interoperability decision; AES acceleration does not make the entire protocol a FIPS-validated construction. Active-quantum authentication, traffic-analysis resistance, and transparent compatibility with existing HTTP/3 deployments remain outside the claims in Section 25.

Before production deployment, provide full handshake and refresh vectors for both ciphers, independent implementations that interoperate, malformed-input and stateful fuzzing, actual-backend buffer-lifetime stress tests, congestion coexistence tests, recovery tests under reordering/loss, and hardware-specific DMA/teardown verification. Performance claims must come from the specified workload measurements. No line rate or latency bound follows merely from the availability of SIMD, GSO, AF_XDP, io_uring, or an AES engine.

<a id="section-35"></a>

## 35. Mobile connectivity, suspension, and bounded session parking

### 35.1 Mobile execution contract

An established transport session is identified by authenticated connection state and CIDs, not by a socket object, IP address, access point name, or radio type. Replacing an interface/socket adapter does not replace stream IDs, flow counters, traffic keys, or packet-number space. A network change is presented to the connection owner as a generation-tagged event. It never directly edits a peer address or bypasses the path FSM.

The adapter emits `NETWORK_AVAILABLE`, `NETWORK_PROPERTIES`, `NETWORK_LOSING`, `NETWORK_LOST`, `LOCAL_ADDRESS_CHANGED`, `APPLICATION_BACKGROUND`, `APPLICATION_FOREGROUND`, and `MONOTONIC_RESUME` with the network handle/generation, route context, source-address choices, interface UDP ceiling, permission status, and available metered/constrained hints. These are local policy facts, not cryptographic proof of endpoint reachability. A callback from an obsolete network incarnation is stale even if the same Wi-Fi SSID or interface index reappears.

The owner distinguishes a default-route change from actual loss of an explicitly bound old route. It must not tear down a healthy bound path merely because another network becomes preferred. Conversely, the mere existence of an interface does not demonstrate permission, Internet access, or reachability of this service. Link selection considers policy, measured path behavior, cost, power, and reachability; Wi-Fi is not unconditionally preferred over cellular. Network callbacks are coalesced by network generation and handled without a per-connection polling thread.

Android adapters can use ConnectivityManager/NetworkCallback and explicit Network-bound sockets; the platform's network-object lifetime and permission rules still apply [R28, R29]. Apple adapters may use NWPathMonitor as local path information, not as a proof that GRT traffic can reach its peer [R31]. These APIs belong outside the portable dataplane. A sandboxed mobile implementation normally uses UDP sockets; it does not require AF_XDP or io_uring. Platform support is capability-probed and ordinary socket I/O remains a conforming fallback.

A public listener normally receives on shared unconnected UDP sockets and dispatches by CID after validating the fixed header; a kernel filter locked to an old peer tuple cannot implement migration on its own. An adapter using connected UDP sockets must maintain a separately admitted candidate socket and an old-socket drain/fence, or an equivalent receive path that can admit new tuple evidence. It must not connect an existing socket to an unvalidated remote address as a shortcut. Each receive event retains its actual destination address/interface metadata, and each send is bound to its authorized path rather than whichever default route exists at submission time.

### 35.2 Initial connectivity and racing

Initial IPv6/IPv4 or interface racing is a bounded application/adapter policy, informed by Happy Eyeballs principles [R32], not a merger of different handshake attempts. At most two initial attempts for one user connection are in progress at once by default. Every raced attempt has fresh identity/prekeys, attempt ID, client nonce, original CID and initial CID; it owns its own immutable flight and admission budget. A one-time prekey is never assigned to two attempts. Selection occurs only after server authentication, never just the first UDP reply.

The losing attempt is canceled and fenced. Early application data must not be submitted to multiple raced attempts unless the application explicitly provides a cross-attempt deduplication contract; the default permits early data on only one attempt. Exactly-once business semantics are not provided. A family/interface that cannot carry the 1200-byte base is not usable for GRT. If all permitted UDP paths fail, report UDP_UNAVAILABLE or a bounded establishment failure. A fallback to TCP/another protocol is an explicit higher-layer choice with its own authentication; it is not an invisible GRT wire mode or a security downgrade.

### 35.3 Connection availability FSM

Availability is orthogonal to the Section 20 connection lifecycle. The connection may remain ESTABLISHED while its availability is OFFLINE or PARKED. All wire/input events still pass the ordinary connection, key, replay, and transaction guards.

| Availability | Event/guard | Actions | Next |
|---|---|---|---|
| ONLINE | Eligible alternative interface and policy requests prevalidation | Start one candidate; keep ACTIVE app sends | HANDOVER |
| HANDOVER | Candidate READY and cutover authorized | Execute Section 17 promotion/barrier | ONLINE |
| HANDOVER | Candidate fails while old path usable | Release candidate after fences | ONLINE |
| ONLINE / HANDOVER | All usable paths lost | Stop new handoffs; preserve obligations; arm one outage/idle deadline | OFFLINE |
| OFFLINE | Authorized route returns before expiry | Bind generation; validate immediately with bounded probes | HANDOVER |
| OFFLINE | Ordinary idle deadline expires | Fail pending application operations once; quiesce connection | Lifecycle QUIESCING |
| ONLINE | Client requests eligible parking | Freeze new app admission; send PARK_REQUEST | PARK_WAIT |
| PARK_WAIT | Matching GRANTED | Keep lease deadline; suspend app transmission; compact only after fences | PARKED |
| PARK_WAIT | Authenticated REFUSED/BUSY | Unfreeze app admission; retain ordinary idle semantics | ONLINE |
| PARK_WAIT | Result uncertain at request deadline | Keep app gate closed; normalize with PARK_WAKE using same request ID | WAKING |
| PARKED | Client wakes before conservative lease expiry | Reserve wake resources; revalidate route; send PARK_WAKE | WAKING |
| WAKING | Matching authenticated PARK_AWAKE and path READY | Release client app gate; ACK awake result | ONLINE |
| PARKED / WAKING | Lease expires without completion | Quiesce silently; later use a new handshake | Lifecycle QUIESCING |
| Any | Matching close/fatal event | Apply lifecycle priority; no late event resurrects session | Closing/draining/quiescing |

The server's PARKED/WAKING transitions follow the lease rules below, not an unsolicited local wake. Offline transitions do not create an automatic idle-timeout extension: both peers must still satisfy their negotiated idle timeout. Applications needing longer outages choose an authenticated larger idle timeout or successfully negotiate parking beforehand. A transport cannot infer from silence whether a mobile device is asleep, unreachable, or malicious.

While OFFLINE, reliable source bytes and already committed receive bytes remain retained, but writes are bounded by a configured outage queue limit; the Compact/Mobile default is 64 KiB of queued source data per active connection, or a smaller application limit. This is a cap, not preallocated memory. New writes may return WOULD_BLOCK. Existing reliability obligations may exceed this cap because of data admitted before loss; they must not be discarded. Unsent DATAGRAMs may expire by their application deadlines. Catching up after resume does not replay all missed PTO callbacks or release a whole congestion window at once.

### 35.4 Battery, keepalive, and timer semantics

Default keepalive is OFF. An application may request NAT-maintenance keepalive while execution and policy permit it; use a PING on the active authorized path, account for its ACK and normal congestion, and coalesce it with useful traffic. The recommended initial interval is 25 seconds with per-connection randomized phase; a deployment may adapt conservatively but should not go below 15 seconds merely to mask a broken network. No interval guarantees preservation of every NAT mapping. Keepalive stops during parking, deliberate offline policy, and suspended execution. It is not an always-running radio or an OS background-execution entitlement [R8, R30].

A platform clock used for transport deadlines must measure elapsed time across suspend. If the platform's normal monotonic clock pauses, the adapter uses a suspend-aware monotonic source or a trusted elapsed-time reconciliation before processing resume. It never moves transport time backward. Advance deadlines once to the observed current time and process a bounded set of resulting events; do not synthesize thousands of elapsed periodic ticks. Memory snapshots with active keys and rollbackable PNs are not a resume mechanism. Process loss/key-custody loss requires a new connection, even if a resumption ticket survives.

A resumed active connection revalidates a stale or changed path. It does not inherit a large cwnd/PMTU from pre-sleep conditions. Pure inactivity may release expanded cipher contexts, packet scratch, and unused operation objects, but never the minimal root/PN/replay/identity state or unfulfilled storage obligations. The OS may prevent execution or networking while the application sleeps; GRT cannot guarantee background packet reception or delivery [R30].

### 35.5 Parking scope and exact frame encodings

Parking is a mutually negotiated, **client-requested**, bounded retention lease for a quiescent transport session. Both PARK feature bits and positive max_park_duration_ms are required. The effective maximum is the smaller advertised value, at most 1800000 ms. Parking retains the session identity/key roots, PN and replay state, slot generations, CIDs and absolute credit counters, while suspending permission to open/send application streams until wake completes. It does not retain an arbitrary offline application mailbox and is not persistent storage. It does not require keepalive.

| Type | Frame | Exact body | Semantics |
|---:|---|---|---|
| 0x0040 | PARK_REQUEST | request_id:u64, duration_ms:u32, zero:u32 | Request; reliable until grant/refusal/deadline |
| 0x0041 | PARK_GRANT | request_id:u64, duration_ms:u32, status:u16, zero:u16 | Status 0 GRANTED, 1 REFUSED, 2 BUSY; regenerated result |
| 0x0042 | PARK_WAKE | request_id:u64, wake_seq:u64 | Client wake request; reliable until matching awake/deadline |
| 0x0043 | PARK_AWAKE | request_id:u64, wake_seq:u64 | Server wake confirmation; reliable until packet ACK |

All bodies are 16 bytes, header flags zero, and all these frames are ack-eliciting. At most one instance of each type may occur in one packet. IDs are nonzero monotonically increasing per connection, with no wrap; at most one lease and one wake sequence are live. Refused/BUSY grants have duration zero. GRANTED duration is positive, at most the request and effective maximum. Request duration outside 1000..effective_max is MOBILE_STATE_ERROR. A contradictory repeated ID/duration or result is an authenticated error; stale completed IDs are ignored and cannot extend a lease. The server caches one current/last result sufficient for duplicates, not a history of all leases.

The requester's precondition is no active stream (including unread receive data), no unacknowledged application bytes, no pending application DATAGRAM, no unfinished stream retirement, no key refresh/previous key slot, no elective candidate, and no accepted new application write. The server checks the same semantic quiescence before granting. Only bounded lease/ACK/CID-token housekeeping may remain. All old secret-reading jobs and physically owned buffers must be fenced before compacting their objects; requesting parking does not cancel kernel ownership.

A server grants only after reserving the compact core and associated non-revocable retention quota through the lease expiry. Before sending its first grant it freezes its own application admission and records the server lease expiry as `first_grant_handoff_time + granted_duration`. Duplicate grants use the same duration and never restart this deadline. It must not silently evict a granted lease under ordinary memory pressure; crashes/fatal faults remain possible and are not hidden as successful retention. The client computes its conservative expiry from `first_request_handoff_time + granted_duration`, so delayed/retried requests cannot let it assume a later expiry than the server promised. Implementations account for their declared clock-drift uncertainty by waking early; timestamps are not sent as a shared wall-clock deadline.

PARK_GRANT is regenerated on a repeated request and can be retransmitted within a bounded three-response control policy before quiet parking. Its semantic lease is effective independently of receiving a packet ACK. Once the server has recorded and sent a grant it keeps the lease even if the grant/ACK is lost. It may administratively retire grant-only packet accounting when becoming quiet, without creating a retransmission of application data. A client that never receives a grant must not assume parking succeeded or resume application transmission into a possibly parked peer. It retries the bounded request or uses PARK_WAKE with the same request ID to normalize the uncertain result before reopening its gate. A server that granted but then sees a fresh request to continue uses the wake procedure; silence never renews the lease.

### 35.6 Wake ordering and memory admission

PARK_WAKE also cancels/normalizes an uncertain PARK_REQUEST result. If the server did not grant that request but is otherwise active and has no different live lease, a nonstale request ID may be recorded as completed and answered with PARK_AWAKE after the same path/capacity guards. It creates no lease or expiry extension. A lower stale request ID cannot cancel a newer lease. REFUSED/BUSY are final for a request ID; a new parking attempt uses a new ID. A late grant received in WAKING cannot reverse the wake or extend an expiry.

The client must reacquire capacity for its still-outstanding advertised receive authorizations before requesting wake. It sends PARK_WAKE and a full-sized challenge on a locally authorized route; PATH_INTENT may identify a changed route. A stored old address is marked stale after parking, even when the new tuple text matches it. For an unchanged tuple, its cached path slot can enter VALIDATING with a new validation generation and zero unvalidated credit while application gates stay closed; this is not a second simultaneous active path. The wake control packet supplies fresh committed credit on the server. After full proof, the wake transaction can restore that same authorized tuple to ACTIVE without a new migration sequence. A changed tuple still uses Section 17 selection ordering. No old challenge or budget carries over. The server processes wake/validation in the bounded compact control path, authenticates the request ID, and reacquires its own credit backing before sending PARK_AWAKE. Pool exhaustion can delay wake until capacity is available, but cannot acknowledge application bytes without storage or extend the lease by receiving duplicate requests.

PARK_AWAKE is emitted only on a READY path after the client's return routability and server receive-capacity reservations are valid. It is never coalesced with application frames. For a known granted lease, the client accepts a matching result only before its conservative expiry and after its own path reaches READY. For an uncertain request with no received grant, it uses a fixed recovery deadline equal to the earlier of its ordinary idle deadline and the wake-start time plus the negotiated validation interval; it cannot infer the existence of a lease from silence. Repeated wake/result packets do not extend either deadline. It then opens its application send gate and ACKs the result. The server's receive gate is ready before sending the result; its application **send** gate remains closed until an ACK of an awake-carrying packet confirms client readiness. If the result/ACK is lost, repeated wake/result packets resolve the handshake without new lease IDs or new application delivery. A PARK_WAKE or PATH_RESPONSE alone is not permission to deliver application data to a parked peer.

On the client, accepted PARK_AWAKE completes the lease and starts ordinary idle accounting at that committed event; on the server, the ACK that opens its application-send gate completes the lease and restarts ordinary idle accounting. The completed request/wake IDs remain as bounded duplicate-suppression state. A still-pending side is limited by its original deadline, and packets accepted before expiry may complete its transition; a stale lease timer cannot close an already-completed wake generation. No lease extension is implicit.

Parking temporarily suspends application-send permission, not absolute flow-control arithmetic. Upon wake, the same grants, offsets and slot generations apply. No side may set MAX_DATA backward, reset PN, clear the replay bitmap, or treat a wake packet as a new cryptographic session. Reservation for future application bytes may be returned while both gates are suspended; it must be reacquired before reopening the relevant receive gate. Otherwise the implementation retains that reservation throughout parking. This rule is the only parking-related credit-backing suspension and cannot be used during an ordinary live/offline stream transfer.

If the soft refresh time elapsed during parking, wake control can use the retained current epoch within its hard limits, but fresh application output waits for the required hybrid refresh to complete. Before granting a lease each endpoint must have at least 128 remaining DATA seals and 524288 remaining plaintext-byte allowance under its current directional key, or refresh first. This control reserve is not an extra cryptographic allowance beyond the hard cap. Expiry of keys, invalid secure time, exhausted counters, lost state, or a valid reset requires a fresh handshake. Short mobile path handoffs do not inherently invoke a new KEM exchange.

### 35.7 App-visible outcomes and limitations

The API reports PATH_VALIDATING, PATH_CHANGED, TEMPORARILY_UNREACHABLE, PARK_GRANTED, WAKE_PENDING, WAKE_COMPLETE, and CONNECTION_EXPIRED as distinct local events. A PATH_CHANGED event is not stream reconnection and does not repeat OPEN/WRITE callbacks. The app can distinguish a transport that retained its reliable obligations during a brief outage from a newly resumed connection that cannot reproduce the old stream state.

UDP blocking, an unsupported 1200-byte path, or a denied OS network permission is reported as a distinct connectivity failure. An application may choose an independently authenticated fallback transport, but such a connection is not silently relabeled as the same GRT session and cannot replay non-idempotent writes without application policy. The GRT wire engine itself remains UDP-only.

Recommended Mobile profile defaults are a 120000-ms idle timeout, a 600000-ms maximum parking lease, keepalive OFF, one candidate, one old path, 4096-bit receive history, and the Compact initial stream/credit settings in Section 36. Applications with latency-sensitive active streams should keep them active rather than request quiescent parking. A long-lived idle application stream adds its metadata and outstanding-credit backing; it is not free merely because its payload ring currently contains no bytes.

### 35.8 Lease-control timers and bounded retries

The client selects a fixed `lease_control_pto=max(100000,current_base_pto)` microseconds at the first request handoff. PARK_REQUEST is first sent at t0, may be repeated at t0+pto and t0+3*pto, and has a result deadline of `min(t0+7*pto,t0+negotiated_max_path_validation_interval)`. Retransmissions retain the exact request ID/duration and use fresh PNs. It is not an unbounded stream of new lease requests. Expiry with an uncertain result enters WAKING as specified above, not ONLINE. An authenticated refusal/BUSY permits a later new request ID subject to local and shared control-rate policy, never a tight automatic retry loop.

A wake attempt chooses a new nonzero wake_seq once, snapshots the same pto formula, and sends at most three unsolicited PARK_WAKE transmissions at 0, pto, and 3*pto. Coalesce them with required path-validation work where possible, but do not retransmit a challenge earlier than its own timer or bypass either operation's deadline. The wake attempt deadline is the minimum of its 7*pto horizon, the negotiated validation cap, and the known conservative lease expiry or uncertain-result recovery deadline. After failure, keep application gates closed; a newly authorized network event/local retry may start a higher wake_seq only while the original lease or ordinary recovery lifetime remains valid. It never renews the original parking lease, returns to online by timeout, or refunds the persistent probe bucket.

The server emits each logical grant or awake result at most three times unsolicited under the same 0, pto, 3*pto spacing and the applicable fixed deadline. A fresh authenticated duplicate request may cause regeneration of the same result under ingress/control quotas, not a new lease or timer epoch. Result-only packet accounting may be administratively detached after its retry horizon while the bounded result/lease state remains; this never resolves application data. Ordinary ACK and key-progress scheduling remain separately subject to their existing deadlines.

If the server cannot hand off a pending grant within the request's local admission-work cap (no greater than the negotiated validation interval), it cancels its still-unpublished reservation and caches BUSY. Once the grant has been handed off, even with an as-yet-unknown send result, it conservatively treats the grant as potentially received and honors its fixed lease. A definitive local nonacceptance may release the ciphertext buffer according to its own ownership fence, but does not cause the server to issue a contradictory result for the same published grant. This prevents asynchronous submission ambiguity from revoking a lease the client could have received.

### 35.9 Server lease component FSM

This component is distinct from availability, packet TX/RX, memory, and path states. Before dispatch, apply lifecycle fatal/close priority and lease expiry. Any success action below is conditional on complete packet/owner transaction validation; a failed resource guard is BUSY/defer, not partial success. Unlisted events preserve lease state; stale IDs are ignored, while contradictory nonstale encodings produce MOBILE_STATE_ERROR. Buffer and key references are reclaimed by their own FSMs, never by a lease-state assignment alone.

| State | Event/guard | Atomic actions | Next |
|---|---|---|---|
| NONE / COMPLETE | Fresh PARK_REQUEST; eligible; retention capacity reserved | Freeze app admission; save ID/duration; stage grant; arm bounded work deadline | GRANT_PENDING |
| NONE / COMPLETE | Fresh request ineligible/unfunded | Cache immutable REFUSED/BUSY for this ID; do not suspend app gates | Same |
| GRANT_PENDING | First grant handoff | Fix lease expiry once; close application gates; mark potentially published | GRANTED |
| GRANT_PENDING | Work cap expires before any handoff | Cancel reversible retention grant; publish/cache BUSY; reopen unchanged app policy | NONE |
| GRANTED | Duplicate matching request | Regenerate identical grant under response quotas; do not change expiry | GRANTED |
| GRANT_PENDING / GRANTED | Valid matching PARK_WAKE | Cancel unpublished grant work if any; retain published lease bound; acquire wake/path/backing state | WAKE_PENDING |
| NONE / COMPLETE | Nonstale uncertain-result PARK_WAKE; no different live lease | Mark request normalized; freeze new app admission during guarded wake; use ordinary recovery lifetime | WAKE_PENDING |
| WAKE_PENDING | Route READY; receive backing obtained; all guards pass | Open receive gate; stage/handoff PARK_AWAKE; keep send gate closed | AWAKE_SENT |
| WAKE_PENDING | Duplicate wake / temporary resource shortage | Keep exact IDs, fixed deadline and existing reservations; no app output | WAKE_PENDING |
| AWAKE_SENT | Duplicate matching wake | Regenerate same result under quotas; no expiry extension | AWAKE_SENT |
| AWAKE_SENT | Fresh ACK of an emitted awake result before expiry | Open app send gate; retire lease deadline; start ordinary idle; retain last completed IDs | COMPLETE |
| GRANTED / WAKE_PENDING / AWAKE_SENT | Applicable fixed deadline expires without completion | Stop app admission; lifecycle quiescence; retain I/O fences | EXPIRED |
| Any | Lifecycle quiescence/fatal close | Stop lease output; cancel unsent work; discharge owned operations safely | EXPIRED |

An ACK of a PARK_REQUEST packet is not a lease grant. An ACK of a PARK_WAKE packet is not PARK_AWAKE. A received PATH_RESPONSE is not a PARK_AWAKE result. An output-buffer completion is none of these semantic confirmations. The four distinctions remain true when several controls share a GSO parent or arrive in one GRO buffer.

<a id="section-36"></a>

## 36. Compact sessions and extreme-concurrency resource accounting

### 36.1 What the memory bound means

The Compact Session Profile is a local software/hardware execution profile, not a different wire protocol, reduced authentication mode, or smaller nonce. Implementations intended for high session density MUST provide it and publish an allocation manifest. Throughput deployments may select larger authenticated limits, but must not attribute those sessions to the compact bound. Client and server use the same accounting rules.

The reference compact **core image is 2048 bytes**. A conforming Compact implementation must keep its charged quiescent per-connection protocol footprint at or below **3072 bytes**, including the core, confirmation-proof cache, separately allocated parameter copies, allocator/header overhead, attributable CID/reset indexes at actual table load factor, timers, and initial peer-stream-metadata admission entitlements. This is an implementation acceptance requirement, not a benchmark result for code in this package. A managed runtime that cannot meet it must report its actual footprint rather than excluding object headers.

The quiescent bound applies with no active stream, no pending reliable payload, no candidate/old path, no refresh/handshake object, no kernel/DMA-owned packet, and no active-data credit reservation outside a mutually parked send gate. Newly established Compact connections start with zero application receive credit, so they can meet this condition. A previously used connection may still have irrevocable outstanding credit; it retains and reports the corresponding reservation or enters mutually agreed parking before returning it. Saying that an application has no queued data is not sufficient to call that memory reclaimable.

The bound does not include application objects, process code, shared service trust configuration, or a fixed shared NIC/kernel packet pool, but **all excluded shared pools must appear separately in total process/device capacity**. Handshake scratch, current active traffic, retained stream bytes, retransmission sources, sparse-generation overflow, and kernel-owned output are additional explicitly charged terms. They cannot be moved to a per-connection side allocation and then omitted from a session-memory claim.

### 36.2 Reference 2048-byte local core image

The following is a local allocation blueprint, not a wire/DMA ABI requirement. `GRT11_COMPACT_LAYOUT.h` and the machine-readable layout describe fixed block boundaries. Implementations may pack or fuse logical registers differently while preserving widths and semantics. It does not contain a preallocated stream payload ring or an expanded AES key schedule.

| Offset | Bytes | Block and retained responsibility |
|---:|---:|---|
| 0 | 64 | Lifecycle, role/cipher, owner/incarnation, shared service/local-parameter references, sidecar handles |
| 64 | 224 | Current root and transcript, local/peer identity material or shared identity reference, exporter/resumption roots, epoch/seal counters |
| 288 | 128 | Peer authenticated parameter record; local canonical parameters normally interned by service/profile |
| 416 | 64 | Connection absolute flow/high-water/consumption accounting |
| 480 | 320 | One active path: exact route, anti-amplification byte/packet counters, CC/RTT/PMTU and validation state |
| 800 | 256 | Bounded issued/peer CID entries and reset-token storage, up to four of each |
| 1056 | 512 | Exact 4096-bit receive replay bitmap |
| 1568 | 128 | Compact transmit verification window: 32-byte header and 256 three-bit PN statuses |
| 1696 | 64 | Inline packed slot-generation/lifecycle tombstones; active halves are separately charged |
| 1760 | 96 | Migration sequencing/cutover and parking/outage context |
| 1856 | 64 | One scheduled timer entry and deadline/control bookkeeping |
| 1920 | 64 | Scheduler/control/index references and bounded counters |
| 1984 | 64 | Receive-largest/floor, ACK/ECN counters and compact receive metadata |

The current directional packet keys and IVs are derived from the retained root/T1/cipher and can live in an **active** key cache. All necessary confirmation MACs that cannot be regenerated after root replacement belong to the separately counted proof cache. A hot cipher implementation may keep HChaCha subkeys, AES round keys, GHASH tables, or library contexts; each is explicitly active-state memory, never silently present in every idle session. Once all secret-reading jobs are fenced, idle compaction zeroizes these expanded caches. Waking reconstructs identical keys; it does not change the epoch or reset usage counters. The long-term server credential/private key may be shared by service, but a unique client identity private key needed for refresh is retained per connection.

A maximum of three installed epochs remains mandatory. Previous/staged key objects and the PQ refresh transaction are lazy bounded sidecars. They cannot be erased to satisfy the idle bound while grace, confirmation, or hardware reference requirements remain unsatisfied. Immutable transcript hashes survive compaction; client/server handshake messages, KEM private scratch, certificate copies, and response flights are released when their actual obligations/fences permit.

### 36.3 Compact transport defaults and escalation

| Parameter/policy | Compact default | Mobile default | Throughput policy |
|---|---:|---:|---|
| initial_max_data | 0 | 0 | Explicit BDP/admission-backed value |
| initial_stream_window | 4096 | 4096 | Explicit admitted power of two up to 16 MiB |
| receive_packet_history | 4096 | 4096 | Explicit negotiated width up to 1048576 |
| initial_bidi_slots per peer | 1 | 1 | Explicit admitted value |
| initial_uni_slots per peer | 0 | 0 | Explicit admitted value |
| maximum_bidi/uni_slots | 1024/1024 | 1024/1024 | Same wire maxima; not initial allocation |
| active_cid_limit | 2 | 2 | 2..4 |
| idle_timeout_ms | 30000 | 120000 | Explicit service policy, at most 600000 |
| max_park_duration_ms | 600000 if enabled | 600000 | 0 or admitted maximum up to 1800000 |
| keepalive | Off | Off | Application policy |
| ahead-of-time sealed output | None when idle | None when idle | Globally capped active work |

Zero initial connection credit does not prevent STREAM_OPEN, handshake, ACK, path validation or key progress. A client opens a stream without payload; a server that accepts it reserves a small receive grant and emits MAX_DATA (and any necessary MAX_STREAM_DATA). The Compact default therefore can add a credit round trip before first request bytes; this is an explicit density/latency tradeoff. A Low-Latency preset can advertise 4096 bytes of initial_max_data, with that **additional** receive authorization reserved and reported. It must not advertise the same zero-credit quiescent total. An application can amortize setup by retaining a safely funded open stream.

A receiver grants MAX_SLOTS only after funding the maximum simultaneously open metadata it authorizes. The maximum_* slots fields are format ceilings, not an instruction to allocate 4096 full records at establishment. Initial permitted peer OPEN metadata is included in the quiescent admission budget; payload bytes remain limited by initial_max_data. Local application OPEN can fail for local resource exhaustion before any wire promise is made.

Receive-ring capacity may grow dynamically under Section 12.3, without changing the authenticated initial parameter: acquire larger backing first, migrate/fence its local storage representation, and only then advance MAX_STREAM_DATA/MAX_DATA. Never shrink the representable window below outstanding grants or pinned data. A 4096-byte initial window does not impose a permanent 4096-byte throughput ceiling. Replay width, unlike stream grants, remains the authenticated session width; the Compact profile does not silently switch to a narrower replay window under pressure.

With zero initial connection credit, application STREAM bytes are not eligible for immediate first-flight/early-data acceptance under those parameters. A deployment choosing application 0-RTT must fund a positive receive grant and obey Section 9's authenticated replay policy. Immediate post-handshake STREAM payload also needs funded positive credit, but ordinary 1-RTT data does not require the early-data replay ledger. DATAGRAM is separately bounded and does not consume stream MAX_DATA. Rejecting early data permits ordinary reliable retransmission after fresh credit; it must not silently discard application writes. The Compact and Low-Latency profiles expose this latency-versus-guaranteed-capacity tradeoff rather than claiming both costs disappear.

### 36.4 Sparse allocation and credit backing

The implementation represents only active streams and nondefault generation tombstones, using bounded arrays, radix pages, or arenas. Default unopened generation-zero slots need no per-slot payload or dynamic object. Once a slot has advanced its generation, enough state must remain to reject old traffic; its tombstone cannot be forgotten just because the stream closed. Repeatedly opening different slots can consume up to the negotiated grant's tombstone capacity. Fund that limit at MAX_SLOTS admission, account for it after retirement, and never allocate in proportion to an unvalidated remote ID or maximum 46-bit generation.

A receive half can use an in-order fast representation and allocate an exact bitmap/gap structure only when needed. Every representation must implement the same retained-byte overlap checks and bounded window; a received range cannot be marked present without retention. Physical pages may be sparse, but future granted bytes need either reserved uncommitted pages or an equivalent hard capacity guarantee. Virtual address reservation, anticipated application consumption, and typical low load are not backing guarantees.

Let U be retained unconsumed data, F the unused connection credit, and S the sum of currently usable unused stream credits plus admission for permitted unopened receive halves. A safe upper bound for new admissible bytes is `min(F,S)`; already admitted holes/high-water commitments and transaction staging must also be included by the implementation's exact accounting. A simple conservative allocator reserves full granted receive windows. A more compact allocator may reserve the smaller proven simultaneous bound, but must demonstrate that every legal peer frame sequence, including sparse end-of-window writes and RESET final sizes, fits its budget. Each high-water increment remains charged exactly once under Section 12.4.

Once bytes have been ACKed, neither memory pressure nor compaction permits dropping them before consumption/reset/retirement. When a source reference is resolved by an ACK, do not release a separately kernel-owned ciphertext buffer until its ownership event. Stream metadata remains until retirement and all application leases are fenced. Lazy allocation is not lazy correctness.

### 36.5 Send history, recovery, and packet memory

The Compact recent PN verification window has 256 entries of three bits plus base/next/largest/floor metadata. Suggested codes are RESERVED=0, SUBMITTED=1, EMITTED=2, BURNED=3, ACKED=4, LOST=5, ABANDONED=6, UNUSED=7. A status is meaningful only for its absolute PN/window position. The entry is not a complete sent-packet record. Live send timestamps, path ordinal, item references, flags, and counted-flight state remain in a separate bounded sent table until resolved/administratively detached. ACK-only history does not require a heap object per packet.

Sliding a recent history window never frees live reliable obligations. Below the verification floor, an unverifiable ACK is ignored, but any retained live packet is still processed normally. A larger active window can be allocated for a high-packet-rate connection; the memory appears in the active budget. SUBMITTED slots and ACK_WAIT dependencies cannot be evicted without recording equivalent exact operation state. If history cannot advance safely, apply TX backpressure rather than reuse a pending slot.

A recommended packed live packet descriptor is 48 bytes before logical-item references. Each source/item reference is separately counted. Do not keep an entire encrypted packet merely to retransmit its contents: retain logical source bytes and rebuild under fresh PN/current PMTU/key/path. A GSO parent can release output after actual I/O completion while the much smaller child recovery descriptors continue. Conversely, early ACK does not return an outstanding zero-copy parent. The retained source, ciphertext, descriptor, RX lease, and application lease totals are distinct.

At rate r bytes/s and RTT t seconds, reliable flight payload alone can approach r*t. No 2-KiB core removes this bandwidth-delay requirement. At 100 Gbit/s and 10 ms it is 125,000,000 payload bytes before retransmission/metadata/headroom. Many mostly idle sessions are compatible with a small core; a million simultaneously saturated high-BDP sessions are a different memory/network capacity problem.

### 36.6 Global arenas, timers, and progress guarantees

Use bounded per-worker/NUMA arenas for packet scratch, transient decrypted frames, active stream halves, retransmission references, candidate paths, key caches, handshake/rekey jobs, and adapter descriptors. A resource token moves between these arenas only through the owner FSM. Memory exhaustion is an admission event, not a reason to acknowledge discarded reliable bytes. Cold-path allocation/refill may run outside the fast path; an active fast path uses bounded pool operations rather than unbounded allocation, per-session OS threads, or one kernel socket per server session.

The reference scheduler has one linked timer-wheel/heap entry per connection for its earliest deadline, with other deadlines in its logical state or active sidecars. Recomputing a minimum is bounded by component count, not by all packets or all network sessions. No polling sweep of a million idle connections is required per microsecond tick. Parked sessions generally have only their lease expiry scheduled. A completely silent quiescent session does not allocate a KEM/refresh job merely because a time-only soft threshold passes; record the need to refresh and enforce it at reactivation, with Section 35's bounded wake-control exception. Packet/byte hard caps never defer. Active connections should spread refresh preparation with local jitter no later than the soft deadline, for example between 80% and 100% of the time threshold, and use a bounded fair cryptographic job pool. A scheduled rekey storm must not allocate one large work buffer per idle session. Timer coarsening for far-away idle/park deadlines cannot postpone active ACK/PTO obligations beyond the advertised contract.

Control progress uses **shared bounded reserve pools** for authenticated path/key/ACK/credit/close work; it does not preallocate a 4096-byte scratch packet for every session. Reserve sizing and fair service must cover the number of simultaneously admitted control jobs, not the number of total sessions. An unknown-CID flood cannot consume the entire authenticated progress reserve. Bounded per-connection scheduling quanta prevent one peer's thousands of frame/stream events from monopolizing a worker.

Before address validation the REQUIRED_RETRY listener has zero per-spoofed-attempt allocation. After token validation it uses a bounded shared attempt pool. A reference handshake slot has a **64-KiB total cap**, covering request/response caches, fragment bookkeeping, KEM/private scratch and job metadata; typical actual length-class allocations may be lower. This is transient, not 64 KiB per established client. Server precomputed cryptographic pools and replay ledgers have separate finite quotas. Returning a valid address token is not proof that the sender is entitled to unlimited CPU.

### 36.7 Explicit capacity equation and examples

An implementation reports both resident/committed memory and irrevocable reserved capacity:

```
M_total = N_quiescent * M_quiescent_charged
        + sum(active-stream metadata + live receive/source bytes + granted capacity)
        + sum(live packet/item descriptors + externally owned ciphertext/RX leases)
        + N_candidates * M_candidate + N_old * M_old
        + N_handshake_slots * M_handshake_slot
        + sum(active/previous/staged crypto state + refresh work)
        + shared NIC/kernel/UMEM/io_uring pools
        + shared service/index excess, worker state, application memory, and allocator slack
```

Index memory must appear exactly once; the portion already attributed in M_quiescent_charged is not added again. Reserved receive capacity is not double-counted when its pages become committed: the reservation becomes the backing allocation. Kernel queue occupancy, pinned pages, packet-pool huge pages, per-core free-list caches and client token/ticket caches are explicitly disclosed. A server multiplexing UDP sessions on shared sockets may avoid a socket per peer; a deployment choosing per-peer sockets counts their actual kernel memory.

| Quiescent sessions | 2048-byte core images only | 3072-byte Compact charged ceiling |
|---:|---:|---:|
| 100,000 | 195.3125 MiB | 292.96875 MiB |
| 1,000,000 | 1.90735 GiB | 2.86102 GiB |
| 10,000,000 | 19.07349 GiB | 28.61023 GiB |

These are arithmetic allocation budgets, not measured resident memory or guaranteed supported session counts. For example 10000 separately admitted 64-KiB handshake slots add 625 MiB. A million Low-Latency sessions each guaranteeing another 4096 receive bytes reserve a further 3.81470 GiB. Only 10000 concurrent such grants require about 39.0625 MiB. High occupancy, active percentage, RTT, key-refresh synchronization and queue depths must be stated with any performance claim.

### 36.8 Compaction and activation state machine

| Memory state | Event/guard | Actions | Next |
|---|---|---|---|
| CORE_ONLY | Authenticated/local active work and pool admission | Obtain necessary sidecar/credit tokens before semantic mutation | ACTIVATING |
| ACTIVATING | All bounded objects ready and generation matches | Install handles atomically; preserve all counters | HOT |
| ACTIVATING | Resource unavailable / stale request | Roll back reversible reservations; no false ACK or PN reuse | CORE_ONLY |
| HOT | No live stream/payload/owned output/candidate/refresh; credit accounted | Freeze reclaim candidates, wait for exact references | COMPACTING |
| COMPACTING | Fences complete; required roots/proofs/history retained | Zeroize/release expanded state; retain exact tombstones | CORE_ONLY |
| COMPACTING | New work arrives before release | Cancel compaction or complete it then reacquire serially | HOT / ACTIVATING |
| Any | Close/fatal event | Follow lifecycle quiescence; no reuse before device/kernel fence | QUIESCING |

The FSM can be fused with ordinary owner-thread code; it is not a requirement to allocate a new event object or perform an indirect function call at every logical state. Deterministic semantics and bounded work are mandatory; performance remains a measured property of the actual backend.

<a id="section-37"></a>

## 37. Reflection resistance and admission/egress execution contract

### 37.1 Threat boundary

The transport must withstand spoofed-source bootstrap floods without per-source session allocation, prevent unvalidated paths from eliciting disproportionate output, and keep authenticated peer churn within bounded memory/CPU/egress quotas. It does not promise to eliminate reflected packets, defeat a botnet whose members can receive challenges, survive arbitrary upstream bandwidth saturation, or authenticate a physical person. Address validation demonstrates return routability at one time; it is not perpetual ownership of an IP address [R1, R8].

A valid traffic key authenticates the **connection**, not the source address of an independently routed IP packet. A malicious authenticated client may spoof another source tuple. Therefore new-tuple probes, ACKs, close traffic, key control, and migration notifications remain constrained even when their AEAD tags verify. Long-term pins, the ticket binder, client identity, and a known CID never lift a new tuple's cap.

### 37.2 Budget identity and ingress credit

Each stateful unvalidated path has `rx_bytes, tx_bytes, reserved_bytes, rx_packets, tx_packets, reserved_packets` and its path generation. Budgets belong to the exact tuple/listener/connection path; all its CIDs, epochs, GSO parents and retransmissions share the same counters. A CID/key rotation is not a new allowance. A tuple change starts with zero, except for the bytes/one datagram of the fresh packet whose authenticated transaction creates that candidate. A listener's stateless shrinking RETRY/reset exception is distinct from a connection budget.

For live DATA, ingress credit is recorded exactly once at RX_COMMIT. The guard is complete AEAD verification, fresh global PN, valid frames, admitted candidate/storage, and completed transaction. Replays/duplicates, unknown epochs/CIDs, malformed authenticated packets, padding outside a datagram, uncommitted packets, and fabricated GRO segments add nothing. A packet arriving on ACTIVE cannot grant bytes to CANDIDATE. A single GRO completion containing k complete datagrams may add k separate credits on its actual ingress path; it never adds k times the superbuffer length.

Bootstrap tokenless admission, when explicitly enabled, counts actual valid received bootstrap datagrams rather than treating a claimed total_message_length as received bytes. Repeated physical arrivals may be counted; a declared 16-KiB flight with only one received fragment is not 16 KiB of credit. Stateless Retry needs no accumulated credit because a 160-byte response can only be elicited by a qualifying 1200-byte datagram.

### 37.3 Atomic output reservation and completion

For an unvalidated path define, using checked arithmetic:

```
bytes_available   = max(0, 3*rx_bytes   - tx_bytes   - reserved_bytes)
packets_available = max(0, 3*rx_packets - tx_packets - reserved_packets)

reserve(batch):
    require sum(child.udp_length) <= bytes_available
    require child_count <= packets_available
    require every child uses this exact path/generation and eligible frame class
    require per-child PMTU, congestion/pacing, key, memory and backend gates
    reserved_bytes += sum(child.udp_length)
    reserved_packets += child_count
```

A reservation has a unique operation token and settles once. Successful acceptance moves it from reserved to sent. Definitive nonacceptance returns it; ambiguous acceptance remains charged as a possibly emitted operation. An io_uring buffer-release notification is not a refund. AF_XDP completion is not a refund. Loss, timeout, peer ACK, and local application cancellation after handoff are not refunds. Never subtract sent bytes because a packet is later declared lost. A retried/newly sealed datagram requires a new reservation.

All children of one GSO request reserve **aggregate** allowance, not independent checks against the same old counter. A sendmmsg prefix accepts a prefix of messages, each possibly a complete GSO request. Only the definitively unaccepted suffix releases its reservation. A positive short UDP send result inconsistent with atomic-datagram semantics is an adapter fault/ambiguous handoff, never permission to reuse its memory or rebate its whole network budget. The three-times limits are checked again when queued output becomes eligible, including path/PMTU changes and connection close.

Implement counter arithmetic in at least 66 bits for intermediate products/sums or use proven saturating comparisons. Logical receive counters may clamp to `2^62-1`; sent/reserved counters cannot wrap. If representable accounting is exhausted or corrupted, stop that egress path/fail closed; do not reset counters and create new credit. Allocation generations must not wrap into live asynchronous references.

A receiver may use smaller response policies than these maxima. The path byte limit and packet-count limit together bound UDP traffic and prevent excessive packet-rate amplification; link-layer/encapsulation overhead and distributed attack volume remain deployment concerns. Packet/byte policers apply above and below worker sharding, not just inside each connection.

### 37.4 The narrowly scoped local-probe exception

A protocol that always requires prior received bytes could never originate its first challenge. There is therefore a separate **locally authorized probe operation**, not a bypass for peer-discovered paths. It is permitted only when the application/OS route authority selects a new local interface to an already authorized peer service endpoint, or a still-authorized retained endpoint of this connection. It cannot be created by a remote frame, spoofed source address, incoming ACK, arbitrary URL, or a token presented from a different tuple.

Before any response it allows at most three 1200-byte challenge/control datagrams for that bounded validation transaction. Each consumes the persistent connection/interface/global probe buckets in Section 17.6, normal pacing, and actual transmit accounting. It carries no application STREAM/DATAGRAM bytes and cannot be fragmented into additional sends. A response may be returned using received-credit accounting; that does not restore already consumed seed allowance. Locally initiated and response-funded reservations have distinct provenance and cannot debit both sources for one packet or refund twice.

A path becomes unrestricted by amplification only through the appropriate return-routability proof. Even then application traffic waits for READY and the ordinary congestion/startup gates. Validation expiry, deletion/recreation, a fake OLD_UNUSABLE hint, and a new CID do not regenerate local authority or refill buckets. Public server policy should not initiate probes toward arbitrary client-provided addresses. This exception is disclosed because an unconditional statement that every first outgoing client packet is bounded by previously received bytes would be false.

### 37.5 Small error traffic and reset safety

Unknown versions, invalid lengths, unknown CIDs, bad authentication, and unrecognized epochs normally receive silence. There is no unauthenticated version-negotiation responder, diagnostic echo service, or large error text. An authenticated CLOSE on a candidate uses only that candidate's own budget; when there is insufficient credit, it may be omitted and the local failure still proceeds. A valid peer reset is never answered.

A stateless reset remains 32..64 bytes and strictly shorter than its triggering plausible DATA datagram of at least 65 bytes. Its first bytes cannot be MAGIC. Only a locally recognizable retired CID is eligible; random unknown CIDs do not require a lookup in an unbounded retired-session table. A deployment can disable reset generation entirely and rely on idle timeout, especially when it lacks a bounded recognition mechanism. Source/listener/global reset packet limits are separate from Retry and authenticated progress quotas. Reflection of one small packet is still possible; the design prevents an amplification loop rather than claiming no response at all.

### 37.6 System-level DoS resource gates

The public listener enforces finite budgets for total live sessions, admitted handshakes, KEM/signature jobs, partial fragments, early-data ledger operations, migration candidates, parked leases, token/reset output, and bytes/packets per scheduling epoch. The gate precedes the corresponding expensive allocation or operation. Address validation and client authentication do not waive these gates. Reserve slots separately for established authenticated control progress, so new handshakes cannot indefinitely starve rekey or wake transactions.

Per-source and prefix limits require bounded tables with explicit collision/overflow behavior. Prefix-only bans are not sufficient behind carrier NAT; global admission and fair connection/identity scheduling remain necessary. Do not allocate one hash entry for every spoofed IPv6 address. Expiring entries are processed with bounded work and generation-safe timers, not an attacker-controlled full-table sweep on every packet. Multi-worker and multi-machine listeners conservatively partition or coordinate their aggregate rate/capacity allowance; multiplying a per-worker cap by worker count without reporting it is not a global cap.

A valid REUSE token can be replayed from its still-valid tuple to request many handshakes. It proves address reachability, not user entitlement or single use. Handshake deduplication covers only live immutable attempts; an attacker who chooses fresh attempt IDs remains constrained by admission. Early data additionally requires the durable shared replay ledger in Section 9, and may be disabled independently of resumption. No shared token, PSK, or anti-replay claim replaces these distinct controls.

### 37.7 Required abuse metrics

Expose at least stateless Retry/reset counts and bytes; token failures by nonsecret category; handshake admission/reassembly/crypto occupancy; per-path amplification stalls; probe-seed versus response-funded sends; candidate replacement/refusal; deferred wake; control-reserve pressure; canceled versus ambiguous submissions; and memory/credit reservation totals. Logs must not contain traffic keys, ticket PSKs, token MAC keys, decrypted application payload, or stable identifiers beyond the local logging policy. Metrics are bounded counters or sampled events, not an unbounded per-packet log allocation.

<a id="section-38"></a>

## 38. Mobile, memory, and adversarial verification requirements

### 38.1 Normative safety properties

The Section 27 and 34 invariants remain required. Implementations additionally verify these properties for both software and hardware schedules:

| ID | Property |
|---|---|
| M01 | A fresh authenticated packet on a new tuple cannot by itself validate or promote that tuple. |
| M02 | Peer-discovered unvalidated output never exceeds both 3x byte and 3x packet allowances, including queued reservations. |
| M03 | Local seed probes require local route authority, bounded persistent buckets, and no application data. |
| M04 | Replayed/failed/uncommitted input creates no live-path amplification credit. |
| M05 | CID/key rotation, GSO grouping, and candidate replacement never transfer or mint path credit. |
| M06 | Matching challenge tokens are checked against exact path and generation; base-size proof is independent of mere reachability. |
| M07 | At most one application-sending path, one candidate, and one retained old path exist; operation tombstones have a separate finite cap. |
| M08 | Selection never resets packet numbers, replay history, byte offsets, final sizes, or cipher choice. |
| M09 | A pre-cutover old-path datagram cannot reverse an accepted migration; OLD never auto-promotes. |
| M10 | Old-path ACKs resolve logical data at most once and never inflate new-path RTT/cwnd. |
| M11 | Kernel/device-owned GSO/XDP/io_uring bytes remain immutable and unreusable through path loss and shutdown. |
| M12 | One physical datagram consumes one nonce, one recovery identity, one path charge, and one packet allowance. |
| M13 | Tokenless REQUIRED_RETRY processing allocates zero per-attempt state and performs no asymmetric cryptography. |
| M14 | RETRY is exactly 160 bytes in response to a qualifying 1200-byte fragment; reset is smaller than its trigger. |
| M15 | Repeated lease requests/grants never extend the initially recorded expiry. |
| M16 | Parking never discards a live stream, outstanding reliable source, unread byte, replay bit, generation, or secret-reading pin. |
| M17 | A wake cannot reopen application receiving before credit backing and path readiness; an uncertain grant is resolved explicitly. |
| M18 | Quiescent memory claims include index load factor, object headers, timer storage, proof caches and irrevocable reservations. |
| M19 | Compacting a key cache regenerates identical keys/nonces without rolling counters back. |
| M20 | Network callbacks, timers, accelerators, and I/O threads mutate transport state only through owner events and authorized commits. |
| M21 | A sparse stream or old slot ID cannot allocate memory proportional to an attacker-selected offset/generation. |
| M22 | Resource pressure never converts a noncommitted packet into an ACK or silently revokes a granted credit/retention lease. |
| M23 | Suspend/resume does not shift transport time backward, replay every missed timer, or accumulate an unbounded pacing burst. |
| M24 | Source-tuple-bound tokens do not validate a different IP, port, IP family, local listener endpoint, or domain. |

### 38.2 Required deterministic and randomized scenarios

Test Wi-Fi-to-cellular and cellular-to-Wi-Fi prevalidation, hard break before probe/response/intent/selected result, NAT source-port rebinding, IPv6 temporary-address replacement, IPv4/IPv6 family changes, NAT64, VPN route replacement, interface-handle reuse, repeated link flapping, cellular metering-policy refusal, simultaneous local network events, and a candidate unable to carry 1200 bytes. Include a stale old-path packet with a PN higher than an earlier probe, and queued GSO children whose PN assignment precedes the cutover barrier but whose completion follows it.

Drop/reorder/duplicate every migration/parking control flight individually; lose PATH_SELECTED and its ACK; lose PARK_GRANT before client suspension; lose PARK_AWAKE and its ACK; expire the lease during validation; change NAT mapping while asleep; exhaust wake backing; and trigger key refresh/hard-limit approach during handoff/wake. A client that never learned whether parking succeeded must not send ordinary data into a silently parked server without the recovery exchange.

For reflection tests, spoof initial fragment sources, send only nonzero fragments, declare maximal flights with one received fragment, replay a token on another source port/listener, create authenticated spoofed migration sources, replay one valid DATA PN on many addresses, split a GSO request near a budget boundary, deliver a zero-copy notification before the owner's send-result event, and saturate all shared rate/admission tables. Verify both reserved and committed byte/packet totals, not just final sent bytes.

For memory tests, establish N quiescent sessions with zero initial application credit; measure the entire attributable footprint; then open permitted streams on all N simultaneously. Fill the end of each granted window before its beginning, exhaust slot generations across all permitted slots, retain application leases, and leave send buffers kernel-owned while remote ACKs arrive. Report resident pages, guaranteed reservations, kernel queues, peak handshake/rekey work, and late-operation tombstones separately. Empty source queues do not prove zero outstanding obligations.

### 38.3 Artifact and release boundary

The accompanying executable models exercise selected wire encodings, packet ciphers, GSO/software ownership, anti-amplification reservation, token binding, cutover barriers, parking deadlines, and memory-layout arithmetic. They are not the complete transport engine. Model tests and C layout static assertions are reported with their actual run results. Assertions/model coverage do not establish cryptographic proof, Linux integration correctness, absence of all races, OS background availability, RTL line rate, or measured million-session capacity.

Before production release, independently implement and interoperate the entire handshake/refresh including XEdDSA and ML-KEM, fuzz all authenticated parsers and control state combinations, test actual UDP GSO/GRO, AF_XDP and io_uring kernels/drivers, run adversarial schedulers and model checking, validate congestion coexistence under mobile loss/reordering, audit token/crypto composition, and measure the declared compact/high-throughput profiles. Every claimed bound needs a reproducible manifest and workload. A state-machine design makes these obligations explicit; it does not automatically prove them.

<a id="section-39"></a>

## 39. References and attribution

The external documents below specify existing primitives or provide design context. GRT's wire layouts, role arrangement, bounded stream handles, early-batch format, and refresh transaction are original design proposals in this specification. An external reference is not a claim that its authors reviewed or endorsed GRT.

[R1] RFC 9000, *QUIC: A UDP-Based Multiplexed and Secure Transport*, May 2021. `https://www.rfc-editor.org/rfc/rfc9000.html`

[R2] RFC 9002, *QUIC Loss Detection and Congestion Control*, May 2021. `https://www.rfc-editor.org/rfc/rfc9002.html`

[R3] RFC 9001, *Using TLS to Secure QUIC*, May 2021, particularly packet protection and AEAD usage bounds. `https://www.rfc-editor.org/rfc/rfc9001.html`

[R4] Signal, *The PQXDH Key Agreement Protocol*, Revision 3, updated January 23, 2024; retrieved September 13, 2026. `https://signal.org/docs/specifications/pqxdh/`

[R5] WireGuard, *Protocol & Cryptography*. `https://www.wireguard.com/protocol/`

[R6] skywind3000, *KCP — A Fast and Reliable ARQ Protocol*, upstream repository. `https://github.com/skywind3000/kcp`

[R7] RFC 8174, *Ambiguity of Uppercase vs Lowercase in RFC 2119 Key Words*. `https://www.rfc-editor.org/rfc/rfc8174.html`

[R8] RFC 8085, *UDP Usage Guidelines*. `https://www.rfc-editor.org/rfc/rfc8085.html`

[R9] RFC 7748, *Elliptic Curves for Security*. `https://www.rfc-editor.org/rfc/rfc7748.html`

[R10] Signal, *The XEdDSA and VXEdDSA Signature Schemes*, Revision 1, October 20, 2016. `https://signal.org/docs/specifications/xeddsa/`

[R11] RFC 5869, *HMAC-based Extract-and-Expand Key Derivation Function (HKDF)*. `https://www.rfc-editor.org/rfc/rfc5869.html`

[R12] NIST FIPS 203, *Module-Lattice-Based Key-Encapsulation Mechanism Standard*, August 13, 2024. The official landing page also identifies a November 17, 2025 planning note about potential updates; implementers must review official errata and validation guidance rather than substitute pre-standard Kyber. `https://csrc.nist.gov/pubs/fips/203/final`

[R13] RFC 8439, *ChaCha20 and Poly1305 for IETF Protocols*. `https://www.rfc-editor.org/rfc/rfc8439.html`

[R14] Open Quantum Safe, *ML-KEM parameter set summary* and upstream ML-KEM API constants. `https://openquantumsafe.org/liboqs/algorithms/kem/ml-kem.html`

[R15] RFC 8899, *Packetization Layer Path MTU Discovery for Datagram Transports*. `https://www.rfc-editor.org/rfc/rfc8899.html`

[R16] RFC 9221, *An Unreliable Datagram Extension to QUIC*, design context for separately unreliable application messages. `https://www.rfc-editor.org/rfc/rfc9221.html`

[R17] Libsodium, *XChaCha20-Poly1305 construction*, and upstream `crypto_aead_xchacha20poly1305_ietf_*` implementation; inspected September 13, 2026. The API construction is pinned, not a claim that all libraries expose identical APIs. `https://libsodium.gitbook.io/doc/secret-key_cryptography/aead/chacha20-poly1305/xchacha20-poly1305_construction` and `https://github.com/jedisct1/libsodium/blob/master/src/libsodium/crypto_aead/xchacha20poly1305/aead_xchacha20poly1305.c`

[R18] S. Arciszewski, *XChaCha: eXtended-nonce ChaCha and AEAD_XChaCha20_Poly1305*, `draft-irtf-cfrg-xchacha-03`, January 10, 2020. Expired draft, used solely as a fixed construction/test-vector reference, not an IETF standard. `https://datatracker.ietf.org/doc/html/draft-irtf-cfrg-xchacha-03`

[R19] NIST SP 800-38D, *Recommendation for Block Cipher Modes of Operation: Galois/Counter Mode (GCM) and GMAC*. Review the official publication/errata status before implementation. `https://csrc.nist.gov/pubs/sp/800/38/d/final`

[R20] Linux man-pages, `udp(7)`, UDP_SEGMENT/UDP_GRO API; inspected September 13, 2026. `https://man7.org/linux/man-pages/man7/udp.7.html`

[R21] Linux kernel documentation, *AF_XDP*, including UMEM ring ownership, zero-copy probing, multi-buffer packet boundaries, and completion semantics; inspected September 13, 2026. `https://docs.kernel.org/networking/af_xdp.html`

[R22] liburing, `io_uring_prep_recvmsg_multishot(3)` and `io_uring_recvmsg_out(3)`; inspected September 13, 2026. `https://man7.org/linux/man-pages/man3/io_uring_prep_recvmsg_multishot.3.html` and `https://man7.org/linux/man-pages/man3/io_uring_recvmsg_out.3.html`

[R23] liburing, `io_uring_prep_sendmsg_zc(3)` and `io_uring_prep_send_zc(3)`, operation versus notification CQEs; inspected September 13, 2026. `https://man7.org/linux/man-pages/man3/io_uring_prep_sendmsg_zc.3.html` and `https://man7.org/linux/man-pages/man3/io_uring_prep_send_zc.3.html`

[R24] liburing, `io_uring_buf_ring_add(3)` and `io_uring_register_buf_ring(3)`, provided-buffer publication and IDs; inspected September 13, 2026. `https://man7.org/linux/man-pages/man3/io_uring_buf_ring_add.3.html` and `https://man7.org/linux/man-pages/man3/io_uring_register_buf_ring.3.html`

[R25] Linux man-pages, `sendmmsg(2)`, message-prefix acceptance semantics. `https://man7.org/linux/man-pages/man2/sendmmsg.2.html`

[R26] Linux kernel documentation, *MSG_ZEROCOPY*, ownership completion and error-queue notification. `https://docs.kernel.org/networking/msg_zerocopy.html`

[R27] Linux kernel upstream, `net/ipv4/udp.c`, ancillary-data encoding and UDP send/receive implementation; inspected September 13, 2026. A moving upstream branch is informative implementation evidence, not a protocol dependency; a deployment manifest must pin its actual kernel build. `https://github.com/torvalds/linux/blob/master/net/ipv4/udp.c`

[R28] Android Developers, *Read network state*, network generations, callbacks and route-policy context. `https://developer.android.com/develop/connectivity/network-ops/reading-network-state`

[R29] Android Developers, *android.net.Network*, explicit network-bound socket API and lifecycle. `https://developer.android.com/reference/android/net/Network`

[R30] Android Developers, *Optimize for Doze and App Standby*, background/network restrictions. `https://developer.android.com/training/monitoring-device-state/doze-standby`

[R31] Apple Developer Documentation, *NWPathMonitor* and *NWPath*. Local path observations do not constitute GRT peer validation. `https://developer.apple.com/documentation/network/nwpathmonitor`

[R32] RFC 8305, *Happy Eyeballs Version 2: Better Connectivity Using Concurrency*. Used as connection-attempt policy context, not as a GRT wire requirement. `https://www.rfc-editor.org/rfc/rfc8305.html`

## Appendix A. Additional canonical constraints

A DATA packet containing PADDING MUST also contain an ack-eliciting frame. A pure ACK packet does not need PADDING because even a one-range ACK exceeds the minimum plaintext length. A CLOSE packet contains exactly one CLOSE frame and no PADDING. This constraint avoids a counted-in-flight padding-only packet that would not elicit any ACK or PTO progress. Alignment zero bytes inside another frame are not PADDING frames and do not change congestion classification.

The absolute maximum number of live paths, key slots, packet scratch bytes, frame records, fragments, credentials, slots, and control items is not an invitation to allocate all resources for every unauthenticated connection. The implementation's manifest fixes admission and queue depths separately. Cross-connection fairness and denial-of-service policy must be verified at the device level in addition to per-connection invariants.

Transport time counters are monotonic and independent of credential time. A platform suspend/resume that invalidates monotonic elapsed-time or key custody assumptions requires revalidation or a new connection; restoring a memory snapshot with old packet counters while continuing the same keys is forbidden.

## Appendix B. Single-source formal IR and verified code generation

Because GRT/1.1 decomposes protocol behavior into orthogonal finite-state machines with fixed-width integer registers, total transition tables, and conjunctive boolean gates (Sections 4.6, 12.5–12.7, 15, 17.7, 19, 20, 21, 22.3, 35.3, 35.9, and 36.8), an implementation MAY represent all protocol state machines, guards, gates, and normative invariants (`I01`–`I35`, `A01`–`A12`, `M01`–`M24`) in a single typed Intermediate Representation (IR) that drives both:

1. **Exhaustive Formal Verification**: Explicit-state reachability exploration, transition totality and determinism verification (Section 20.2), terminal state absorption (`DEAD`), and invariant verification across all component FSMs and composed multi-component protocol models, together with canonical TLA+ specification export (`spec/formal/GRT11.tla`).
2. **Zero-Allocation Hotpath Code Generation**: Direct emission of bitmask state classifiers (`O(1)` register mask operations), branch-flattened deterministic transition functions and lookup tables, conjunctive TX/RX/cutover gate evaluators, and zero-escape invariant verifiers into the transport engine (`internal/engine/z_formal_gen.go`).

The normative architecture, IR schema, verification obligations, and code generation rules are specified in `spec/FORMAL_CODEGEN_ARCH.md`.


## Appendix C. Test vectors

The reference implementation pins regression vectors in `internal/crypto/testdata/kat.json` and checks them in `internal/crypto/kat_test.go`. They were generated from that implementation, not derived independently; an independent implementation that reproduces them byte for byte agrees with it on the covered derivations (Section 27.1). Byte strings are lowercase hex; integers are decimal. The file covers:

- `kdf_label`: `L(secret, label, context, n)` (Section 4.3) for n = 32, 24, 12 and an empty context.
- `epoch_keys`: directional key and IV from an epoch root for both ciphers and both directions (Sections 5.1, 8.4).
- `ticket_psk`: `L(resumption_master, "ticket psk", ticket_id, 32)` (Section 9.1).
- `ticket_seal`: complete 224-byte ticket envelopes (Section 9.1).
- `token_retry`: a complete 64-byte kind-0 address token (Section 6.2).
- `aead_packet`: one DATA payload sealed under each cipher with the Section 5.1 nonce.
- `pqxdh_kdf`: the PQXDH-shaped KDF (Section 4.3).

`seq(b, n)` denotes the n bytes `b, b+1, …, b+n-1`.

**C.1 Ticket PSK.** `resumption_master = seq(0x80, 32)`, `ticket_id = seq(0xa0, 16)`:

```
psk = bcdaae06ac4bcbb957d7e7515b1c7eaa32e822a3188da24e73d7e6b68ca4c4e2
```

**C.2 Epoch-1 client-to-server keys, XChaCha20-Poly1305.** `root = seq(0x40, 32)`, `T1 = seq(0x60, 32)`, `cs = 0x0001`, `e = 1`, so `KCTX = T1 || 0001 || 00000001`:

```
key = a465b6a2b6535a496cf5df34d1e525d1bd0723903b4d737b47313eaabf245b2d
iv  = 72ae1cf5934c6babeb611220ad7798c1d236a071060b310a
```

**C.3 DATA payload, AES-256-GCM.** `key = seq(0x30, 32)`, `iv = seq(0x50, 12)`, `pn = 305419896` (`0x12345678`), `AAD = seq(0x70, 32)`, plaintext = ASCII `GRT/1.1 test vector packet payload`:

```
ciphertext||tag = 57120d978e2a4f7d83898613e300091cbdbb66d2f47ff0420e3d7579df640f0b
                  1c3f46c5e51e546d7beb3dd617fc2f08345d
```

The `ticket_seal` vectors also fix this implementation's server-private ticket-key rotation, which does not affect interoperability because only the issuing server opens its tickets. Envelope keys are `L(master, "ticket key", u32(generation) || u64(period) || u16(sub), 32)` with `period = floor(now_unix_s / 86400)` and `sub = (issuance_counter >> 24) mod 4096`, and the envelope `key_id` is `u32((period mod 2^20) << 12 | sub)`. Each derived key therefore seals at most 2^24 envelopes within one 24-hour period. The server accepts only the current and previous period.

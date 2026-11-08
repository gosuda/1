#ifndef CHROME_NET_SIGNATURES_H
#define CHROME_NET_SIGNATURES_H

#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>
#include <array>

namespace chrome_net {

// RFC 8701 GREASE constants
constexpr uint16_t kGreaseValues[] = {
    0x0a0a, 0x1a1a, 0x2a2a, 0x3a3a, 0x4a4a, 0x5a5a, 0x6a6a, 0x7a7a,
    0x8a8a, 0x9a9a, 0xaaaa, 0xbaba, 0xcaca, 0xdada, 0xeaea, 0xfafa
};

constexpr size_t kGreaseCount = sizeof(kGreaseValues) / sizeof(kGreaseValues[0]);

inline bool IsGrease(uint16_t val) {
    return ((val & 0x0f0f) == 0x0a0a) && ((val >> 8) == (val & 0xff));
}

uint16_t GetGrease(size_t index);

// Chrome 131+ BoringSSL Cipher Suites (15 standard suites + 1 GREASE at index 0 = 16 total)
constexpr uint16_t kChromeCipherSuites[] = {
    0x1301, // TLS_AES_128_GCM_SHA256
    0x1302, // TLS_AES_256_GCM_SHA384
    0x1303, // TLS_CHACHA20_POLY1305_SHA256
    0xc02b, // TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256
    0xc02f, // TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
    0xc02c, // TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384
    0xc030, // TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384
    0xcca9, // TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256
    0xcca8, // TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256
    0xc013, // TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA
    0xc014, // TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA
    0x009c, // TLS_RSA_WITH_AES_128_GCM_SHA256
    0x009d, // TLS_RSA_WITH_AES_256_GCM_SHA384
    0x002f, // TLS_RSA_WITH_AES_128_CBC_SHA
    0x0035, // TLS_RSA_WITH_AES_256_CBC_SHA
};

// Supported Groups / Curves (4 standard + 1 GREASE = 5 total)
constexpr uint16_t kChromeSupportedGroups[] = {
    0x11ec, // X25519MLKEM768 (Post-quantum hybrid, 4588)
    0x001d, // X25519 (29)
    0x0017, // secp256r1 (23)
    0x0018, // secp384r1 (24)
};

// TLS Extension IDs
enum TlsExtensionId : uint16_t {
    kExtServerName = 0x0000,
    kExtExtendedMasterSecret = 0x0017,
    kExtRenegotiationInfo = 0xff01,
    kExtSupportedGroups = 0x000a,
    kExtEcPointFormats = 0x000b,
    kExtSessionTicket = 0x0023,
    kExtAlpn = 0x0010,
    kExtStatusRequest = 0x0005,
    kExtSignatureAlgorithms = 0x000d,
    kExtSignedCertificateTimestamp = 0x0012,
    kExtKeyShare = 0x0033,
    kExtPskKeyExchangeModes = 0x002d,
    kExtSupportedVersions = 0x002b,
    kExtCompressCertificate = 0x001b,
    kExtApplicationSettings = 0x4469, // ALPS
    kExtEncryptedClientHello = 0xfe0d, // ECH outer GREASE
};

// Signature Algorithms used by modern Chrome BoringSSL
constexpr uint16_t kChromeSignatureAlgorithms[] = {
    0x0403, // ecdsa_secp256r1_sha256
    0x0804, // rsa_pss_rsae_sha256
    0x0401, // rsa_pkcs1_sha256
    0x0503, // ecdsa_secp384r1_sha384
    0x0805, // rsa_pss_rsae_sha384
    0x0501, // rsa_pkcs1_sha384
    0x0806, // rsa_pss_rsae_sha512
    0x0601, // rsa_pkcs1_sha512
};

struct TlsClientHello {
    uint16_t client_version{0x0303}; // TLS 1.2 legacy version in record/hello
    std::vector<uint16_t> cipher_suites;
    std::vector<uint16_t> supported_groups;
    std::vector<uint8_t> ec_point_formats{0}; // uncompressed (0)
    std::vector<uint16_t> signature_algorithms;
    std::vector<uint16_t> supported_versions;
    std::vector<uint16_t> extension_ids;
    std::string server_name;
    std::vector<std::string> alpn_protocols{"h2", "http/1.1"};
    std::vector<uint8_t> raw_wire_bytes;
};

// HTTP/2 Signatures
constexpr char kH2ConnectionPreface[] = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n";
constexpr size_t kH2ConnectionPrefaceLen = 24;

struct H2Setting {
    uint16_t id;
    uint32_t value;
};

// Chrome 131+ H2 Settings
constexpr H2Setting kChromeH2Settings[] = {
    {0x1, 65536},    // SETTINGS_HEADER_TABLE_SIZE
    {0x2, 0},        // SETTINGS_ENABLE_PUSH
    {0x4, 6291456},  // SETTINGS_INITIAL_WINDOW_SIZE (6 MiB)
    {0x6, 262144},   // SETTINGS_MAX_HEADER_LIST_SIZE (256 KiB)
};

constexpr uint32_t kChromeH2WindowUpdate = 15663105; // 0x00EEFFFF (Stream 0)
constexpr uint32_t kChromeH2PriorityWeight = 256;    // Stream dependency 0, weight 256 (byte 255)

// HTTP/2 Pseudo-Header Order
constexpr const char *kH2PseudoHeaderOrder[] = {
    ":method",
    ":authority",
    ":scheme",
    ":path"
};

// QUIC & HTTP/3 Transport Parameters
struct QuicTransportParams {
    uint64_t initial_max_data{15728640};
    uint64_t initial_max_stream_data_bidi_local{6291456};
    uint64_t initial_max_stream_data_bidi_remote{6291456};
    uint64_t initial_max_stream_data_uni{6291456};
    uint64_t initial_max_streams_bidi{100};
    uint64_t initial_max_streams_uni{100};
    uint64_t max_idle_timeout{30000};
    uint64_t active_connection_id_limit{8};
    uint64_t grease_parameter{0x2a2a};
};

struct H3Setting {
    uint64_t id;
    uint64_t value;
};

// Chrome 131+ H3 Settings
constexpr H3Setting kChromeH3Settings[] = {
    {0x1, 4096},     // SETTINGS_QPACK_MAX_TABLE_CAPACITY
    {0x6, 262144},   // SETTINGS_MAX_FIELD_SECTION_SIZE
    {0x7, 100},      // SETTINGS_QPACK_BLOCKED_STREAMS
    {0x41, 1},       // GREASE setting (RFC 9114 0x1f * N + 0x21)
};

// HTTP/3 Pseudo-Header Order
constexpr const char *kH3PseudoHeaderOrder[] = {
    ":method",
    ":scheme",
    ":authority",
    ":path"
};

// Generator functions
TlsClientHello GenerateChromeTlsClientHello(const std::string &server_name, uint32_t seed = 0);
std::vector<uint8_t> BuildH2ConnectionPrefaceAndSettings();
std::vector<uint8_t> BuildH2WindowUpdate(uint32_t stream_id = 0, uint32_t increment = kChromeH2WindowUpdate);
std::vector<uint8_t> BuildH2PriorityFrame(uint32_t stream_id = 3, uint32_t dependency = 0, uint32_t weight = 256);

// Fingerprint calculators
std::string CalculateJA3(const TlsClientHello &hello);
std::string CalculateJA3Hash(const TlsClientHello &hello);
std::string CalculateJA4(const TlsClientHello &hello, const std::string &sni, const std::string &alpn);

// Signature Verifiers
bool VerifyTlsClientHello(const TlsClientHello &hello, std::string *error);
bool VerifyTlsClientHelloWire(const uint8_t *data, size_t len, std::string *error);
bool VerifyHttp2Settings(const uint8_t *data, size_t len, std::string *error);
bool VerifyHttp2WindowUpdate(const uint8_t *data, size_t len, std::string *error);
bool VerifyQuicParameters(const QuicTransportParams &params, std::string *error);
bool VerifyH2HeaderOrder(const std::vector<std::string> &headers, std::string *error);
bool VerifyH3HeaderOrder(const std::vector<std::string> &headers, std::string *error);

// Utility cryptographic functions (self-contained, zero external deps)
std::string Md5Hex(const std::string &input);
std::string Sha256Hex(const std::string &input);

} // namespace chrome_net

#endif // CHROME_NET_SIGNATURES_H

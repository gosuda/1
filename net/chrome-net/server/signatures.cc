#include "signatures.h"

#include <algorithm>
#include <random>
#include <cstring>

namespace chrome_net {

namespace {

// Standard self-contained MD5 implementation (RFC 1321)
struct Md5Context {
    uint32_t state[4];
    uint32_t count[2];
    uint8_t buffer[64];
};

#define F_MD5(x, y, z) (((x) & (y)) | ((~x) & (z)))
#define G_MD5(x, y, z) (((x) & (z)) | ((y) & (~z)))
#define H_MD5(x, y, z) ((x) ^ (y) ^ (z))
#define I_MD5(x, y, z) ((y) ^ ((x) | (~z)))

#define ROTL_MD5(x, n) (((x) << (n)) | ((x) >> (32 - (n))))

#define FF(a, b, c, d, x, s, ac) { \
    (a) += F_MD5((b), (c), (d)) + (x) + (uint32_t)(ac); \
    (a) = ROTL_MD5((a), (s)); \
    (a) += (b); \
}
#define GG(a, b, c, d, x, s, ac) { \
    (a) += G_MD5((b), (c), (d)) + (x) + (uint32_t)(ac); \
    (a) = ROTL_MD5((a), (s)); \
    (a) += (b); \
}
#define HH(a, b, c, d, x, s, ac) { \
    (a) += H_MD5((b), (c), (d)) + (x) + (uint32_t)(ac); \
    (a) = ROTL_MD5((a), (s)); \
    (a) += (b); \
}
#define II(a, b, c, d, x, s, ac) { \
    (a) += I_MD5((b), (c), (d)) + (x) + (uint32_t)(ac); \
    (a) = ROTL_MD5((a), (s)); \
    (a) += (b); \
}

void Md5Transform(uint32_t state[4], const uint8_t block[64]) {
    uint32_t a = state[0], b = state[1], c = state[2], d = state[3], x[16];
    for (int i = 0, j = 0; i < 16; ++i, j += 4) {
        x[i] = ((uint32_t)block[j]) | (((uint32_t)block[j + 1]) << 8) |
               (((uint32_t)block[j + 2]) << 16) | (((uint32_t)block[j + 3]) << 24);
    }

    // Round 1
    FF(a, b, c, d, x[ 0],  7, 0xd76aa478);
    FF(d, a, b, c, x[ 1], 12, 0xe8c7b756);
    FF(c, d, a, b, x[ 2], 17, 0x242070db);
    FF(b, c, d, a, x[ 3], 22, 0xc1bdceee);
    FF(a, b, c, d, x[ 4],  7, 0xf57c0faf);
    FF(d, a, b, c, x[ 5], 12, 0x4787c62a);
    FF(c, d, a, b, x[ 6], 17, 0xa8304613);
    FF(b, c, d, a, x[ 7], 22, 0xfd469501);
    FF(a, b, c, d, x[ 8],  7, 0x698098d8);
    FF(d, a, b, c, x[ 9], 12, 0x8b44f7af);
    FF(c, d, a, b, x[10], 17, 0xffff5bb1);
    FF(b, c, d, a, x[11], 22, 0x895cd7be);
    FF(a, b, c, d, x[12],  7, 0x6b901122);
    FF(d, a, b, c, x[13], 12, 0xfd987193);
    FF(c, d, a, b, x[14], 17, 0xa679438e);
    FF(b, c, d, a, x[15], 22, 0x49b40821);

    // Round 2
    GG(a, b, c, d, x[ 1],  5, 0xf61e2562);
    GG(d, a, b, c, x[ 6],  9, 0xc040b340);
    GG(c, d, a, b, x[11], 14, 0x265e5a51);
    GG(b, c, d, a, x[ 0], 20, 0xe9b6c7aa);
    GG(a, b, c, d, x[ 5],  5, 0xd62f105d);
    GG(d, a, b, c, x[10],  9, 0x02441453);
    GG(c, d, a, b, x[15], 14, 0xd8a1e681);
    GG(b, c, d, a, x[ 4], 20, 0xe7d3fbc8);
    GG(a, b, c, d, x[ 9],  5, 0x21e1cde6);
    GG(d, a, b, c, x[14],  9, 0xc33707d6);
    GG(c, d, a, b, x[ 3], 14, 0xf4d50d87);
    GG(b, c, d, a, x[ 8], 20, 0x455a14ed);
    GG(a, b, c, d, x[13],  5, 0xa9e3e905);
    GG(d, a, b, c, x[ 2],  9, 0xfcefa3f8);
    GG(c, d, a, b, x[ 7], 14, 0x676f02d9);
    GG(b, c, d, a, x[12], 20, 0x8d2a4c8a);

    // Round 3
    HH(a, b, c, d, x[ 5],  4, 0xfffa3942);
    HH(d, a, b, c, x[ 8], 11, 0x8771f681);
    HH(c, d, a, b, x[11], 16, 0x6d9d6122);
    HH(b, c, d, a, x[14], 23, 0xfde5380c);
    HH(a, b, c, d, x[ 1],  4, 0xa4beea44);
    HH(d, a, b, c, x[ 4], 11, 0x4bdecfa9);
    HH(c, d, a, b, x[ 7], 16, 0xf6bb4b60);
    HH(b, c, d, a, x[10], 23, 0xbebfbc70);
    HH(a, b, c, d, x[13],  4, 0x289b7ec6);
    HH(d, a, b, c, x[ 0], 11, 0xeaa127fa);
    HH(c, d, a, b, x[ 3], 16, 0xd4ef3085);
    HH(b, c, d, a, x[ 6], 23, 0x04881d05);
    HH(a, b, c, d, x[ 9],  4, 0xd9d4d039);
    HH(d, a, b, c, x[12], 11, 0xe6db99e5);
    HH(c, d, a, b, x[15], 16, 0x1fa27cf8);
    HH(b, c, d, a, x[ 2], 23, 0xc4ac5665);

    // Round 4
    II(a, b, c, d, x[ 0],  6, 0xf4292244);
    II(d, a, b, c, x[ 7], 10, 0x432aff97);
    II(c, d, a, b, x[14], 15, 0xab9423a7);
    II(b, c, d, a, x[ 5], 21, 0xfc93a039);
    II(a, b, c, d, x[12],  6, 0x655b59c3);
    II(d, a, b, c, x[ 3], 10, 0x8f0ccc92);
    II(c, d, a, b, x[10], 15, 0xffeff47d);
    II(b, c, d, a, x[ 1], 21, 0x85845dd1);
    II(a, b, c, d, x[ 8],  6, 0x6fa87e4f);
    II(d, a, b, c, x[15], 10, 0xfe2ce6e0);
    II(c, d, a, b, x[ 6], 15, 0xa3014314);
    II(b, c, d, a, x[13], 21, 0x4e0811a1);
    II(a, b, c, d, x[ 4],  6, 0xf7537e82);
    II(d, a, b, c, x[11], 10, 0xbd3af235);
    II(c, d, a, b, x[ 2], 15, 0x2ad7d2bb);
    II(b, c, d, a, x[ 9], 21, 0xeb86d391);

    state[0] += a;
    state[1] += b;
    state[2] += c;
    state[3] += d;
}

void Md5Init(Md5Context *ctx) {
    ctx->count[0] = ctx->count[1] = 0;
    ctx->state[0] = 0x67452301;
    ctx->state[1] = 0xefcdab89;
    ctx->state[2] = 0x98badcfe;
    ctx->state[3] = 0x10325476;
}

void Md5Update(Md5Context *ctx, const uint8_t *input, size_t inputLen) {
    size_t i, index, partLen;
    index = (size_t)((ctx->count[0] >> 3) & 0x3F);
    if ((ctx->count[0] += ((uint32_t)inputLen << 3)) < ((uint32_t)inputLen << 3))
        ctx->count[1]++;
    ctx->count[1] += ((uint32_t)inputLen >> 29);
    partLen = 64 - index;

    if (inputLen >= partLen) {
        memcpy(&ctx->buffer[index], input, partLen);
        Md5Transform(ctx->state, ctx->buffer);
        for (i = partLen; i + 63 < inputLen; i += 64)
            Md5Transform(ctx->state, &input[i]);
        index = 0;
    } else {
        i = 0;
    }
    memcpy(&ctx->buffer[index], &input[i], inputLen - i);
}

void Md5Final(uint8_t digest[16], Md5Context *ctx) {
    static const uint8_t PADDING[64] = { 0x80 };
    uint8_t bits[8];
    for (int i = 0; i < 4; ++i) {
        bits[i] = (uint8_t)((ctx->count[0] >> (i * 8)) & 0xff);
        bits[i + 4] = (uint8_t)((ctx->count[1] >> (i * 8)) & 0xff);
    }
    size_t index = (size_t)((ctx->count[0] >> 3) & 0x3F);
    size_t padLen = (index < 56) ? (56 - index) : (120 - index);
    Md5Update(ctx, PADDING, padLen);
    Md5Update(ctx, bits, 8);
    for (int i = 0; i < 4; ++i) {
        digest[i]      = (uint8_t)((ctx->state[0] >> (i * 8)) & 0xff);
        digest[i + 4]  = (uint8_t)((ctx->state[1] >> (i * 8)) & 0xff);
        digest[i + 8]  = (uint8_t)((ctx->state[2] >> (i * 8)) & 0xff);
        digest[i + 12] = (uint8_t)((ctx->state[3] >> (i * 8)) & 0xff);
    }
}

// Standard self-contained SHA-256 implementation (FIPS 180-4)
struct Sha256Context {
    uint32_t state[8];
    uint64_t count;
    uint8_t buffer[64];
};

#define ROTR_SHA256(x, n) (((x) >> (n)) | ((x) << (32 - (n))))
#define CH_SHA256(x, y, z) (((x) & (y)) ^ (~(x) & (z)))
#define MAJ_SHA256(x, y, z) (((x) & (y)) ^ ((x) & (z)) ^ ((y) & (z)))
#define EP0_SHA256(x) (ROTR_SHA256(x, 2) ^ ROTR_SHA256(x, 13) ^ ROTR_SHA256(x, 22))
#define EP1_SHA256(x) (ROTR_SHA256(x, 6) ^ ROTR_SHA256(x, 11) ^ ROTR_SHA256(x, 25))
#define SIG0_SHA256(x) (ROTR_SHA256(x, 7) ^ ROTR_SHA256(x, 18) ^ ((x) >> 3))
#define SIG1_SHA256(x) (ROTR_SHA256(x, 17) ^ ROTR_SHA256(x, 19) ^ ((x) >> 10))

static const uint32_t kSha256K[64] = {
    0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,
    0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,
    0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,
    0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,
    0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,
    0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,
    0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,
    0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2
};

void Sha256Transform(uint32_t state[8], const uint8_t data[64]) {
    uint32_t a = state[0], b = state[1], c = state[2], d = state[3];
    uint32_t e = state[4], f = state[5], g = state[6], h = state[7];
    uint32_t w[64];

    for (int i = 0; i < 16; ++i) {
        w[i] = ((uint32_t)data[i * 4] << 24) |
               ((uint32_t)data[i * 4 + 1] << 16) |
               ((uint32_t)data[i * 4 + 2] << 8) |
               ((uint32_t)data[i * 4 + 3]);
    }
    for (int i = 16; i < 64; ++i) {
        w[i] = SIG1_SHA256(w[i - 2]) + w[i - 7] + SIG0_SHA256(w[i - 15]) + w[i - 16];
    }

    for (int i = 0; i < 64; ++i) {
        uint32_t t1 = h + EP1_SHA256(e) + CH_SHA256(e, f, g) + kSha256K[i] + w[i];
        uint32_t t2 = EP0_SHA256(a) + MAJ_SHA256(a, b, c);
        h = g;
        g = f;
        f = e;
        e = d + t1;
        d = c;
        c = b;
        b = a;
        a = t1 + t2;
    }

    state[0] += a;
    state[1] += b;
    state[2] += c;
    state[3] += d;
    state[4] += e;
    state[5] += f;
    state[6] += g;
    state[7] += h;
}

void Sha256Init(Sha256Context *ctx) {
    ctx->state[0] = 0x6a09e667;
    ctx->state[1] = 0xbb67ae85;
    ctx->state[2] = 0x3c6ef372;
    ctx->state[3] = 0xa54ff53a;
    ctx->state[4] = 0x510e527f;
    ctx->state[5] = 0x9b05688c;
    ctx->state[6] = 0x1f83d9ab;
    ctx->state[7] = 0x5be0cd19;
    ctx->count = 0;
}

void Sha256Update(Sha256Context *ctx, const uint8_t *data, size_t len) {
    size_t buffer_idx = (size_t)(ctx->count % 64);
    ctx->count += len;
    size_t space = 64 - buffer_idx;

    if (len >= space) {
        memcpy(&ctx->buffer[buffer_idx], data, space);
        Sha256Transform(ctx->state, ctx->buffer);
        size_t offset = space;
        while (offset + 64 <= len) {
            Sha256Transform(ctx->state, &data[offset]);
            offset += 64;
        }
        buffer_idx = 0;
        data += offset;
        len -= offset;
    }
    memcpy(&ctx->buffer[buffer_idx], data, len);
}

void Sha256Final(uint8_t digest[32], Sha256Context *ctx) {
    size_t buffer_idx = (size_t)(ctx->count % 64);
    ctx->buffer[buffer_idx++] = 0x80;
    if (buffer_idx > 56) {
        memset(&ctx->buffer[buffer_idx], 0, 64 - buffer_idx);
        Sha256Transform(ctx->state, ctx->buffer);
        buffer_idx = 0;
    }
    memset(&ctx->buffer[buffer_idx], 0, 56 - buffer_idx);
    uint64_t bits = ctx->count * 8;
    for (int i = 7; i >= 0; --i) {
        ctx->buffer[56 + (7 - i)] = (uint8_t)((bits >> (i * 8)) & 0xff);
    }
    Sha256Transform(ctx->state, ctx->buffer);
    for (int i = 0; i < 8; ++i) {
        digest[i * 4]     = (uint8_t)((ctx->state[i] >> 24) & 0xff);
        digest[i * 4 + 1] = (uint8_t)((ctx->state[i] >> 16) & 0xff);
        digest[i * 4 + 2] = (uint8_t)((ctx->state[i] >> 8) & 0xff);
        digest[i * 4 + 3] = (uint8_t)(ctx->state[i] & 0xff);
    }
}

} // namespace

uint16_t GetGrease(size_t index) {
    return kGreaseValues[index % kGreaseCount];
}

static const char kHexChars[] = "0123456789abcdef";

std::string Md5Hex(const std::string &input) {
    Md5Context ctx;
    Md5Init(&ctx);
    Md5Update(&ctx, reinterpret_cast<const uint8_t*>(input.data()), input.size());
    uint8_t digest[16];
    Md5Final(digest, &ctx);
    std::string out;
    out.reserve(32);
    for (int i = 0; i < 16; ++i) {
        out.push_back(kHexChars[(digest[i] >> 4) & 0x0f]);
        out.push_back(kHexChars[digest[i] & 0x0f]);
    }
    return out;
}

std::string Sha256Hex(const std::string &input) {
    Sha256Context ctx;
    Sha256Init(&ctx);
    Sha256Update(&ctx, reinterpret_cast<const uint8_t*>(input.data()), input.size());
    uint8_t digest[32];
    Sha256Final(digest, &ctx);
    std::string out;
    out.reserve(64);
    for (int i = 0; i < 32; ++i) {
        out.push_back(kHexChars[(digest[i] >> 4) & 0x0f]);
        out.push_back(kHexChars[digest[i] & 0x0f]);
    }
    return out;
}

TlsClientHello GenerateChromeTlsClientHello(const std::string &server_name, uint32_t seed) {
    TlsClientHello hello;
    hello.client_version = 0x0303; // TLS 1.2
    hello.server_name = server_name;

    // 1. Cipher Suites (16 total: 1 GREASE + 15 Chrome suites)
    uint16_t grease_cipher = GetGrease(seed);
    hello.cipher_suites.push_back(grease_cipher);
    for (uint16_t cs : kChromeCipherSuites) {
        hello.cipher_suites.push_back(cs);
    }

    // 2. Supported Groups (5 total: 1 GREASE + 4 Chrome curves)
    uint16_t grease_group = GetGrease(seed + 1);
    hello.supported_groups.push_back(grease_group);
    for (uint16_t group : kChromeSupportedGroups) {
        hello.supported_groups.push_back(group);
    }

    // 3. EC Point Formats
    hello.ec_point_formats = {0}; // uncompressed

    // 4. Signature Algorithms
    for (uint16_t sa : kChromeSignatureAlgorithms) {
        hello.signature_algorithms.push_back(sa);
    }

    // 5. Supported Versions (3 total: 1 GREASE + TLS 1.3 + TLS 1.2)
    uint16_t grease_version = GetGrease(seed + 2);
    hello.supported_versions = {grease_version, 0x0304, 0x0303};

    // 6. Base Extensions
    std::vector<uint16_t> base_exts = {
        kExtServerName,
        kExtExtendedMasterSecret,
        kExtRenegotiationInfo,
        kExtSupportedGroups,
        kExtEcPointFormats,
        kExtSessionTicket,
        kExtAlpn,
        kExtStatusRequest,
        kExtSignatureAlgorithms,
        kExtSignedCertificateTimestamp,
        kExtKeyShare,
        kExtPskKeyExchangeModes,
        kExtSupportedVersions,
        kExtCompressCertificate,
        kExtApplicationSettings,
        kExtEncryptedClientHello,
        GetGrease(seed + 3) // GREASE extension
    };

    // 7. Shuffling / Permutation matching BoringSSL
    // Modern BoringSSL shuffles extension order per ClientHello.
    // If seed == 0, keep standard canonical order; if seed > 0, permute deterministically.
    hello.extension_ids = base_exts;
    if (seed != 0) {
        std::mt19937 g(seed);
        std::shuffle(hello.extension_ids.begin(), hello.extension_ids.end(), g);
    }

    // 8. Construct Wire Bytes
    std::vector<uint8_t> body;

    // Client version: 2 bytes
    body.push_back(0x03);
    body.push_back(0x03);

    // Random: 32 bytes (deterministic for test reproducibility)
    for (int i = 0; i < 32; ++i) {
        body.push_back(static_cast<uint8_t>((seed + i * 7) & 0xff));
    }

    // Legacy session id: 32 bytes
    body.push_back(32);
    for (int i = 0; i < 32; ++i) {
        body.push_back(static_cast<uint8_t>((seed * 3 + i) & 0xff));
    }

    // Cipher suites: 2 bytes length + 2 bytes per suite
    uint16_t cs_len = static_cast<uint16_t>(hello.cipher_suites.size() * 2);
    body.push_back(static_cast<uint8_t>(cs_len >> 8));
    body.push_back(static_cast<uint8_t>(cs_len & 0xff));
    for (uint16_t cs : hello.cipher_suites) {
        body.push_back(static_cast<uint8_t>(cs >> 8));
        body.push_back(static_cast<uint8_t>(cs & 0xff));
    }

    // Compression methods: 1 byte count + 1 byte (0x00)
    body.push_back(1);
    body.push_back(0x00);

    // Extensions buffer
    std::vector<uint8_t> ext_buf;
    for (uint16_t ext_id : hello.extension_ids) {
        std::vector<uint8_t> data;
        if (ext_id == kExtServerName) {
            // SNI extension
            uint16_t name_len = static_cast<uint16_t>(hello.server_name.size());
            uint16_t list_len = name_len + 3;
            data.push_back(static_cast<uint8_t>(list_len >> 8));
            data.push_back(static_cast<uint8_t>(list_len & 0xff));
            data.push_back(0); // host_name type
            data.push_back(static_cast<uint8_t>(name_len >> 8));
            data.push_back(static_cast<uint8_t>(name_len & 0xff));
            data.insert(data.end(), hello.server_name.begin(), hello.server_name.end());
        } else if (ext_id == kExtSupportedGroups) {
            uint16_t len = static_cast<uint16_t>(hello.supported_groups.size() * 2);
            data.push_back(static_cast<uint8_t>(len >> 8));
            data.push_back(static_cast<uint8_t>(len & 0xff));
            for (uint16_t g : hello.supported_groups) {
                data.push_back(static_cast<uint8_t>(g >> 8));
                data.push_back(static_cast<uint8_t>(g & 0xff));
            }
        } else if (ext_id == kExtEcPointFormats) {
            data.push_back(static_cast<uint8_t>(hello.ec_point_formats.size()));
            for (uint8_t f : hello.ec_point_formats) {
                data.push_back(f);
            }
        } else if (ext_id == kExtAlpn) {
            std::vector<uint8_t> alpn_list;
            for (const auto &proto : hello.alpn_protocols) {
                alpn_list.push_back(static_cast<uint8_t>(proto.size()));
                alpn_list.insert(alpn_list.end(), proto.begin(), proto.end());
            }
            uint16_t len = static_cast<uint16_t>(alpn_list.size());
            data.push_back(static_cast<uint8_t>(len >> 8));
            data.push_back(static_cast<uint8_t>(len & 0xff));
            data.insert(data.end(), alpn_list.begin(), alpn_list.end());
        } else if (ext_id == kExtSignatureAlgorithms) {
            uint16_t len = static_cast<uint16_t>(hello.signature_algorithms.size() * 2);
            data.push_back(static_cast<uint8_t>(len >> 8));
            data.push_back(static_cast<uint8_t>(len & 0xff));
            for (uint16_t sa : hello.signature_algorithms) {
                data.push_back(static_cast<uint8_t>(sa >> 8));
                data.push_back(static_cast<uint8_t>(sa & 0xff));
            }
        } else if (ext_id == kExtSupportedVersions) {
            uint8_t len = static_cast<uint8_t>(hello.supported_versions.size() * 2);
            data.push_back(len);
            for (uint16_t v : hello.supported_versions) {
                data.push_back(static_cast<uint8_t>(v >> 8));
                data.push_back(static_cast<uint8_t>(v & 0xff));
            }
        } else if (ext_id == kExtKeyShare) {
            // Key share with GREASE and X25519MLKEM768
            uint16_t total_shares_len = 2 + 1 + 2 + 32; // GREASE (1 byte) + X25519 (32 bytes)
            data.push_back(static_cast<uint8_t>(total_shares_len >> 8));
            data.push_back(static_cast<uint8_t>(total_shares_len & 0xff));
            // GREASE share
            data.push_back(static_cast<uint8_t>(grease_group >> 8));
            data.push_back(static_cast<uint8_t>(grease_group & 0xff));
            data.push_back(0); // key_exchange length 1
            data.push_back(1);
            data.push_back(0);
            // X25519 share (group 0x001d)
            data.push_back(0x00);
            data.push_back(0x1d);
            data.push_back(0x00);
            data.push_back(32);
            for (int k = 0; k < 32; ++k) data.push_back(0x42);
        } else if (ext_id == kExtApplicationSettings) {
            // ALPS carrying h2 settings
            data.push_back(0x02); // "h2"
            data.push_back('h');
            data.push_back('2');
            // Settings payload: HEADER_TABLE_SIZE (65536)
            data.push_back(0x00); data.push_back(0x01);
            data.push_back(0x00); data.push_back(0x01); data.push_back(0x00); data.push_back(0x00);
        } else if (ext_id == kExtEncryptedClientHello) {
            // Outer GREASE ECH (0xfe0d)
            data.push_back(0x00); // outer GREASE
            data.push_back(0x01); // cipher_suite: KDF
            data.push_back(0x20); // config_id
            data.resize(data.size() + 32, 0x00); // payload
        } else if (ext_id == kExtCompressCertificate) {
            // RFC 8879 §3: CertificateCompressionAlgorithms (algorithms length = 2 bytes, Brotli = 0x0002)
            data.push_back(0x02);
            data.push_back(0x00);
            data.push_back(0x02);
        } else {
            // Default placeholder for other extensions (empty or minimal)
        }

        ext_buf.push_back(static_cast<uint8_t>(ext_id >> 8));
        ext_buf.push_back(static_cast<uint8_t>(ext_id & 0xff));
        uint16_t d_len = static_cast<uint16_t>(data.size());
        ext_buf.push_back(static_cast<uint8_t>(d_len >> 8));
        ext_buf.push_back(static_cast<uint8_t>(d_len & 0xff));
        ext_buf.insert(ext_buf.end(), data.begin(), data.end());
    }

    uint16_t ext_total = static_cast<uint16_t>(ext_buf.size());
    body.push_back(static_cast<uint8_t>(ext_total >> 8));
    body.push_back(static_cast<uint8_t>(ext_total & 0xff));
    body.insert(body.end(), ext_buf.begin(), ext_buf.end());

    // Record header (5 bytes) + Handshake header (4 bytes) + body
    uint32_t hs_len = static_cast<uint32_t>(body.size());
    uint16_t rec_len = static_cast<uint16_t>(4 + hs_len);

    hello.raw_wire_bytes.clear();
    // Record Header: Type=Handshake(0x16), Version=0x0301 (TLS 1.0 record), Length
    hello.raw_wire_bytes.push_back(0x16);
    hello.raw_wire_bytes.push_back(0x03);
    hello.raw_wire_bytes.push_back(0x01);
    hello.raw_wire_bytes.push_back(static_cast<uint8_t>(rec_len >> 8));
    hello.raw_wire_bytes.push_back(static_cast<uint8_t>(rec_len & 0xff));

    // Handshake Header: Type=ClientHello(0x01), Length (3 bytes)
    hello.raw_wire_bytes.push_back(0x01);
    hello.raw_wire_bytes.push_back(static_cast<uint8_t>((hs_len >> 16) & 0xff));
    hello.raw_wire_bytes.push_back(static_cast<uint8_t>((hs_len >> 8) & 0xff));
    hello.raw_wire_bytes.push_back(static_cast<uint8_t>(hs_len & 0xff));

    hello.raw_wire_bytes.insert(hello.raw_wire_bytes.end(), body.begin(), body.end());
    return hello;
}

std::vector<uint8_t> BuildH2ConnectionPrefaceAndSettings() {
    std::vector<uint8_t> out(reinterpret_cast<const uint8_t*>(kH2ConnectionPreface),
                             reinterpret_cast<const uint8_t*>(kH2ConnectionPreface) + kH2ConnectionPrefaceLen);

    // 2. SETTINGS Frame:
    // Header: 9 bytes
    // Length: 24 (4 settings * 6 bytes = 24 = 0x000018)
    out.push_back(0x00);
    out.push_back(0x00);
    out.push_back(0x18);
    out.push_back(0x04); // Type: SETTINGS
    out.push_back(0x00); // Flags: none
    out.push_back(0x00); out.push_back(0x00); out.push_back(0x00); out.push_back(0x00); // Stream 0

    // Payload (24 bytes)
    for (const auto &s : kChromeH2Settings) {
        out.push_back(static_cast<uint8_t>(s.id >> 8));
        out.push_back(static_cast<uint8_t>(s.id & 0xff));
        out.push_back(static_cast<uint8_t>((s.value >> 24) & 0xff));
        out.push_back(static_cast<uint8_t>((s.value >> 16) & 0xff));
        out.push_back(static_cast<uint8_t>((s.value >> 8) & 0xff));
        out.push_back(static_cast<uint8_t>(s.value & 0xff));
    }
    return out;
}

std::vector<uint8_t> BuildH2WindowUpdate(uint32_t stream_id, uint32_t increment) {
    std::vector<uint8_t> out;
    // Length: 4 (0x000004)
    out.push_back(0x00);
    out.push_back(0x00);
    out.push_back(0x04);
    out.push_back(0x08); // Type: WINDOW_UPDATE
    out.push_back(0x00); // Flags: none
    // Stream ID (31-bit)
    out.push_back(static_cast<uint8_t>((stream_id >> 24) & 0x7f));
    out.push_back(static_cast<uint8_t>((stream_id >> 16) & 0xff));
    out.push_back(static_cast<uint8_t>((stream_id >> 8) & 0xff));
    out.push_back(static_cast<uint8_t>(stream_id & 0xff));

    // Increment (31-bit)
    out.push_back(static_cast<uint8_t>((increment >> 24) & 0x7f));
    out.push_back(static_cast<uint8_t>((increment >> 16) & 0xff));
    out.push_back(static_cast<uint8_t>((increment >> 8) & 0xff));
    out.push_back(static_cast<uint8_t>(increment & 0xff));
    return out;
}

std::vector<uint8_t> BuildH2PriorityFrame(uint32_t stream_id, uint32_t dependency, uint32_t weight) {
    std::vector<uint8_t> out;
    // Length: 5 (0x000005)
    out.push_back(0x00);
    out.push_back(0x00);
    out.push_back(0x05);
    out.push_back(0x02); // Type: PRIORITY
    out.push_back(0x00); // Flags: none
    // Stream ID
    out.push_back(static_cast<uint8_t>((stream_id >> 24) & 0x7f));
    out.push_back(static_cast<uint8_t>((stream_id >> 16) & 0xff));
    out.push_back(static_cast<uint8_t>((stream_id >> 8) & 0xff));
    out.push_back(static_cast<uint8_t>(stream_id & 0xff));

    // Stream Dependency (with exclusive bit set: bit 31 = 1)
    uint32_t dep_field = 0x80000000 | (dependency & 0x7fffffff);
    out.push_back(static_cast<uint8_t>((dep_field >> 24) & 0xff));
    out.push_back(static_cast<uint8_t>((dep_field >> 16) & 0xff));
    out.push_back(static_cast<uint8_t>((dep_field >> 8) & 0xff));
    out.push_back(static_cast<uint8_t>(dep_field & 0xff));

    // Weight: 1 byte (value - 1)
    uint8_t w = static_cast<uint8_t>((weight > 0 ? weight - 1 : 0) & 0xff);
    out.push_back(w);
    return out;
}

std::string CalculateJA3(const TlsClientHello &hello) {
    std::string out;
    out.append(std::to_string(hello.client_version));
    out.push_back(',');

    bool first = true;
    for (uint16_t cs : hello.cipher_suites) {
        if (IsGrease(cs)) continue;
        if (!first) out.push_back('-');
        out.append(std::to_string(cs));
        first = false;
    }
    out.push_back(',');

    first = true;
    for (uint16_t ext : hello.extension_ids) {
        if (IsGrease(ext) || ext == 0x0015) continue;
        if (!first) out.push_back('-');
        out.append(std::to_string(ext));
        first = false;
    }
    out.push_back(',');

    first = true;
    for (uint16_t g : hello.supported_groups) {
        if (IsGrease(g)) continue;
        if (!first) out.push_back('-');
        out.append(std::to_string(g));
        first = false;
    }
    out.push_back(',');

    first = true;
    for (uint8_t f : hello.ec_point_formats) {
        if (!first) out.push_back('-');
        out.append(std::to_string(static_cast<int>(f)));
        first = false;
    }

    return out;
}

std::string CalculateJA3Hash(const TlsClientHello &hello) {
    return Md5Hex(CalculateJA3(hello));
}

static std::string ToHex4(uint16_t val) {
    std::string s(4, '0');
    s[0] = kHexChars[(val >> 12) & 0x0f];
    s[1] = kHexChars[(val >> 8) & 0x0f];
    s[2] = kHexChars[(val >> 4) & 0x0f];
    s[3] = kHexChars[val & 0x0f];
    return s;
}

static std::string To2Digits(size_t val) {
    std::string s(2, '0');
    s[0] = '0' + static_cast<char>((val / 10) % 10);
    s[1] = '0' + static_cast<char>(val % 10);
    return s;
}

std::string CalculateJA4(const TlsClientHello &hello, const std::string &sni, const std::string &alpn) {
    // JA4 format: [protocol][tls_ver][sni][ciphers_count][exts_count][alpn]_[ciphers_hash]_[exts_hash]
    std::string ja4_a;
    ja4_a.push_back('t'); // TCP
    ja4_a.append("13"); // TLS 1.3
    ja4_a.push_back(sni.empty() ? 'i' : 'd');

    // Count non-GREASE ciphers
    std::vector<uint16_t> filtered_ciphers;
    for (uint16_t cs : hello.cipher_suites) {
        if (!IsGrease(cs)) filtered_ciphers.push_back(cs);
    }
    ja4_a.append(To2Digits(filtered_ciphers.size()));

    // Count non-GREASE extensions
    std::vector<uint16_t> filtered_exts;
    for (uint16_t ext : hello.extension_ids) {
        if (!IsGrease(ext) && ext != 0x0015) filtered_exts.push_back(ext);
    }
    ja4_a.append(To2Digits(filtered_exts.size()));

    // ALPN (first and last char)
    if (alpn.empty()) {
        ja4_a.append("00");
    } else if (alpn.size() == 1) {
        ja4_a.push_back(alpn[0]);
        ja4_a.push_back(alpn[0]);
    } else {
        ja4_a.push_back(alpn.front());
        ja4_a.push_back(alpn.back());
    }

    // Sorted ciphers hex
    std::vector<std::string> cipher_hexes;
    for (uint16_t cs : filtered_ciphers) {
        cipher_hexes.push_back(ToHex4(cs));
    }
    std::sort(cipher_hexes.begin(), cipher_hexes.end());
    std::string ciphers_str;
    for (size_t i = 0; i < cipher_hexes.size(); ++i) {
        if (i > 0) ciphers_str.push_back(',');
        ciphers_str.append(cipher_hexes[i]);
    }
    std::string ciphers_hash = Sha256Hex(ciphers_str).substr(0, 12);

    // Sorted extensions hex + signature algorithms
    std::vector<std::string> ext_hexes;
    for (uint16_t ext : filtered_exts) {
        ext_hexes.push_back(ToHex4(ext));
    }
    std::sort(ext_hexes.begin(), ext_hexes.end());
    std::string exts_str;
    for (size_t i = 0; i < ext_hexes.size(); ++i) {
        if (i > 0) exts_str.push_back(',');
        exts_str.append(ext_hexes[i]);
    }
    if (!hello.signature_algorithms.empty()) {
        exts_str.push_back('_');
        for (size_t i = 0; i < hello.signature_algorithms.size(); ++i) {
            if (i > 0) exts_str.push_back(',');
            exts_str.append(ToHex4(hello.signature_algorithms[i]));
        }
    }
    std::string exts_hash = Sha256Hex(exts_str).substr(0, 12);

    return ja4_a + "_" + ciphers_hash + "_" + exts_hash;
}

bool VerifyTlsClientHello(const TlsClientHello &hello, std::string *error) {
    // 1. Verify cipher suite count and order
    if (hello.cipher_suites.size() != 16) {
        if (error) *error = "Expected 16 cipher suites, got " + std::to_string(hello.cipher_suites.size());
        return false;
    }
    if (!IsGrease(hello.cipher_suites[0])) {
        if (error) *error = "First cipher suite must be RFC 8701 GREASE";
        return false;
    }
    for (size_t i = 0; i < 15; ++i) {
        if (hello.cipher_suites[i + 1] != kChromeCipherSuites[i]) {
            if (error) *error = "Cipher suite mismatch at index " + std::to_string(i + 1);
            return false;
        }
    }

    // 2. Verify supported groups
    if (hello.supported_groups.size() != 5) {
        if (error) *error = "Expected 5 supported groups, got " + std::to_string(hello.supported_groups.size());
        return false;
    }
    if (!IsGrease(hello.supported_groups[0])) {
        if (error) *error = "First supported group must be RFC 8701 GREASE";
        return false;
    }
    if (hello.supported_groups[1] != 0x11ec) {
        if (error) *error = "Second group must be X25519MLKEM768 (0x11ec)";
        return false;
    }
    if (hello.supported_groups[2] != 0x001d || hello.supported_groups[3] != 0x0017 || hello.supported_groups[4] != 0x0018) {
        if (error) *error = "Supported groups standard curve mismatch";
        return false;
    }

    // 3. Verify key extensions presence
    bool has_alps = false;
    bool has_ech = false;
    bool has_grease_ext = false;
    bool has_supported_versions = false;

    for (uint16_t ext : hello.extension_ids) {
        if (ext == kExtApplicationSettings) has_alps = true;
        if (ext == kExtEncryptedClientHello) has_ech = true;
        if (ext == kExtSupportedVersions) has_supported_versions = true;
        if (IsGrease(ext)) has_grease_ext = true;
    }

    if (!has_alps) {
        if (error) *error = "Missing ALPS extension (0x4469)";
        return false;
    }
    if (!has_ech) {
        if (error) *error = "Missing ECH outer GREASE extension (0xfe0d)";
        return false;
    }
    if (!has_supported_versions) {
        if (error) *error = "Missing supported_versions extension (0x002b)";
        return false;
    }
    if (!has_grease_ext) {
        if (error) *error = "Missing RFC 8701 GREASE extension";
        return false;
    }

    return true;
}

bool VerifyTlsClientHelloWire(const uint8_t *data, size_t len, std::string *error) {
    if (len < 5 + 4) {
        if (error) *error = "Wire bytes too short for TLS record and handshake header";
        return false;
    }
    // Record type
    if (data[0] != 0x16) {
        if (error) *error = "Invalid record type: expected Handshake (0x16)";
        return false;
    }
    uint16_t rec_len = (static_cast<uint16_t>(data[3]) << 8) | data[4];
    if (rec_len + 5 > len) {
        if (error) *error = "Truncated TLS record";
        return false;
    }
    // Handshake type
    if (data[5] != 0x01) {
        if (error) *error = "Invalid handshake type: expected ClientHello (0x01)";
        return false;
    }
    uint32_t hs_len = (static_cast<uint32_t>(data[6]) << 16) |
                      (static_cast<uint32_t>(data[7]) << 8) | data[8];
    if (hs_len + 4 > rec_len) {
        if (error) *error = "ClientHello length exceeds record length";
        return false;
    }
    return true;
}

bool VerifyHttp2Settings(const uint8_t *data, size_t len, std::string *error) {
    if (len < 9 + 24) {
        if (error) *error = "Data too short for HTTP/2 SETTINGS frame";
        return false;
    }
    // Check 9-byte frame header
    uint32_t length = (static_cast<uint32_t>(data[0]) << 16) |
                      (static_cast<uint32_t>(data[1]) << 8) | data[2];
    uint8_t type = data[3];
    uint8_t flags = data[4];
    uint32_t stream_id = (static_cast<uint32_t>(data[5] & 0x7f) << 24) |
                         (static_cast<uint32_t>(data[6]) << 16) |
                         (static_cast<uint32_t>(data[7]) << 8) | data[8];

    if (length != 24) {
        if (error) *error = "SETTINGS frame length must be 24, got " + std::to_string(length);
        return false;
    }
    if (type != 0x04) {
        if (error) *error = "Frame type must be SETTINGS (0x04), got " + std::to_string(type);
        return false;
    }
    if (flags != 0x00) {
        if (error) *error = "Initial SETTINGS frame flags must be 0x00";
        return false;
    }
    if (stream_id != 0) {
        if (error) *error = "SETTINGS frame stream ID must be 0";
        return false;
    }

    // Check settings payload (4 pairs)
    const uint8_t *p = data + 9;
    for (int i = 0; i < 4; ++i) {
        uint16_t id = (static_cast<uint16_t>(p[0]) << 8) | p[1];
        uint32_t val = (static_cast<uint32_t>(p[2]) << 24) |
                       (static_cast<uint32_t>(p[3]) << 16) |
                       (static_cast<uint32_t>(p[4]) << 8) | p[5];
        p += 6;

        if (id != kChromeH2Settings[i].id) {
            if (error) *error = "Setting " + std::to_string(i) + " ID mismatch";
            return false;
        }
        if (val != kChromeH2Settings[i].value) {
            if (error) *error = "Setting " + std::to_string(i) + " value mismatch";
            return false;
        }
    }
    return true;
}

bool VerifyHttp2WindowUpdate(const uint8_t *data, size_t len, std::string *error) {
    if (len < 9 + 4) {
        if (error) *error = "Data too short for HTTP/2 WINDOW_UPDATE frame";
        return false;
    }
    uint32_t length = (static_cast<uint32_t>(data[0]) << 16) |
                      (static_cast<uint32_t>(data[1]) << 8) | data[2];
    uint8_t type = data[3];
    uint8_t flags = data[4];
    uint32_t stream_id = (static_cast<uint32_t>(data[5] & 0x7f) << 24) |
                         (static_cast<uint32_t>(data[6]) << 16) |
                         (static_cast<uint32_t>(data[7]) << 8) | data[8];

    if (length != 4) {
        if (error) *error = "WINDOW_UPDATE length must be 4";
        return false;
    }
    if (type != 0x08) {
        if (error) *error = "Frame type must be WINDOW_UPDATE (0x08)";
        return false;
    }
    if (flags != 0x00) {
        if (error) *error = "WINDOW_UPDATE flags must be 0";
        return false;
    }
    if (stream_id != 0) {
        if (error) *error = "WINDOW_UPDATE stream ID must be 0";
        return false;
    }

    uint32_t increment = (static_cast<uint32_t>(data[9] & 0x7f) << 24) |
                         (static_cast<uint32_t>(data[10]) << 16) |
                         (static_cast<uint32_t>(data[11]) << 8) | data[12];
    if (increment != kChromeH2WindowUpdate) {
        if (error) *error = "WINDOW_UPDATE increment mismatch: expected 15663105, got " + std::to_string(increment);
        return false;
    }
    return true;
}

bool VerifyQuicParameters(const QuicTransportParams &params, std::string *error) {
    if (params.initial_max_data != 15728640) {
        if (error) *error = "initial_max_data must be 15728640 (15 MiB)";
        return false;
    }
    if (params.initial_max_stream_data_bidi_local != 6291456) {
        if (error) *error = "initial_max_stream_data_bidi_local must be 6291456 (6 MiB)";
        return false;
    }
    if (params.initial_max_stream_data_bidi_remote != 6291456) {
        if (error) *error = "initial_max_stream_data_bidi_remote must be 6291456 (6 MiB)";
        return false;
    }
    if (params.initial_max_stream_data_uni != 6291456) {
        if (error) *error = "initial_max_stream_data_uni must be 6291456 (6 MiB)";
        return false;
    }
    if (params.initial_max_streams_bidi != 100) {
        if (error) *error = "initial_max_streams_bidi must be 100";
        return false;
    }
    if (params.initial_max_streams_uni != 100) {
        if (error) *error = "initial_max_streams_uni must be 100";
        return false;
    }
    if (params.max_idle_timeout != 30000) {
        if (error) *error = "max_idle_timeout must be 30000";
        return false;
    }
    if (params.active_connection_id_limit != 8) {
        if (error) *error = "active_connection_id_limit must be 8";
        return false;
    }
    if (!IsGrease(static_cast<uint16_t>(params.grease_parameter))) {
        if (error) *error = "QUIC GREASE parameter invalid";
        return false;
    }
    return true;
}

bool VerifyH2HeaderOrder(const std::vector<std::string> &headers, std::string *error) {
    if (headers.size() < 4) {
        if (error) *error = "HTTP/2 request must contain at least 4 pseudo-headers";
        return false;
    }
    for (size_t i = 0; i < 4; ++i) {
        if (headers[i] != kH2PseudoHeaderOrder[i]) {
            if (error) *error = "HTTP/2 pseudo-header order violation at index " + std::to_string(i) +
                                ": expected " + kH2PseudoHeaderOrder[i] + ", got " + headers[i];
            return false;
        }
    }
    return true;
}

bool VerifyH3HeaderOrder(const std::vector<std::string> &headers, std::string *error) {
    if (headers.size() < 4) {
        if (error) *error = "HTTP/3 request must contain at least 4 pseudo-headers";
        return false;
    }
    for (size_t i = 0; i < 4; ++i) {
        if (headers[i] != kH3PseudoHeaderOrder[i]) {
            if (error) *error = "HTTP/3 pseudo-header order violation at index " + std::to_string(i) +
                                ": expected " + kH3PseudoHeaderOrder[i] + ", got " + headers[i];
            return false;
        }
    }
    return true;
}

} // namespace chrome_net

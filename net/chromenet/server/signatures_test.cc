#include "signatures.h"
#include "engine_core.h"
#include "chrome_net.h"

#include <cassert>
#include <cstring>
#include <iostream>
#include <string>
#include <vector>

#define TEST_ASSERT(cond, msg) \
    do { \
        if (!(cond)) { \
            std::cerr << "FAILED: " << msg << " at " << __FILE__ << ":" << __LINE__ << std::endl; \
            return 1; \
        } \
    } while (0)

using namespace chrome_net;

int TestTlsSignatures() {
    std::cout << "[TEST] Running TLS 1.3 ClientHello Signatures Test..." << std::endl;

    // 1. Generate ClientHello
    TlsClientHello hello = GenerateChromeTlsClientHello("example.com", 42);
    std::string err;
    TEST_ASSERT(VerifyTlsClientHello(hello, &err), "VerifyTlsClientHello failed: " + err);

    // 2. Cipher Suites
    TEST_ASSERT(hello.cipher_suites.size() == 16, "Must have exactly 16 cipher suites");
    TEST_ASSERT(IsGrease(hello.cipher_suites[0]), "Cipher suite 0 must be GREASE");
    TEST_ASSERT(hello.cipher_suites[1] == 0x1301, "Cipher suite 1 must be TLS_AES_128_GCM_SHA256 (0x1301)");
    TEST_ASSERT(hello.cipher_suites[2] == 0x1302, "Cipher suite 2 must be TLS_AES_256_GCM_SHA384 (0x1302)");
    TEST_ASSERT(hello.cipher_suites[3] == 0x1303, "Cipher suite 3 must be TLS_CHACHA20_POLY1305_SHA256 (0x1303)");

    // 3. Supported Groups (PQ hybrid X25519MLKEM768 fidelity)
    TEST_ASSERT(hello.supported_groups.size() == 5, "Must have exactly 5 supported groups");
    TEST_ASSERT(IsGrease(hello.supported_groups[0]), "Group 0 must be GREASE");
    TEST_ASSERT(hello.supported_groups[1] == 0x11ec, "Group 1 must be X25519MLKEM768 (0x11ec / 4588)");
    TEST_ASSERT(hello.supported_groups[2] == 0x001d, "Group 2 must be X25519 (0x001d)");
    TEST_ASSERT(hello.supported_groups[3] == 0x0017, "Group 3 must be secp256r1 (0x0017)");
    TEST_ASSERT(hello.supported_groups[4] == 0x0018, "Group 4 must be secp384r1 (0x0018)");

    // 4. Extensions presence
    bool has_alps = false, has_ech = false, has_compress_cert = false;
    for (uint16_t id : hello.extension_ids) {
        if (id == kExtApplicationSettings) has_alps = true;
        if (id == kExtEncryptedClientHello) has_ech = true;
        if (id == kExtCompressCertificate) has_compress_cert = true;
    }
    TEST_ASSERT(has_alps, "Must include ALPS extension (0x4469)");
    TEST_ASSERT(has_ech, "Must include ECH outer GREASE extension (0xfe0d)");
    TEST_ASSERT(has_compress_cert, "Must include compress_certificate extension (0x001b)");

    // 5. Extension order shuffling test (BoringSSL behavior)
    TlsClientHello hello_seed1 = GenerateChromeTlsClientHello("example.com", 1);
    TlsClientHello hello_seed2 = GenerateChromeTlsClientHello("example.com", 9999);
    TEST_ASSERT(hello_seed1.extension_ids != hello_seed2.extension_ids,
                "Different seeds must permute extension order");

    // 6. Wire format verification
    TEST_ASSERT(!hello.raw_wire_bytes.empty(), "Wire bytes must not be empty");
    TEST_ASSERT(VerifyTlsClientHelloWire(hello.raw_wire_bytes.data(), hello.raw_wire_bytes.size(), &err),
                "VerifyTlsClientHelloWire failed: " + err);

    // Verify RFC 8879 compress_certificate wire payload in ClientHello
    // Find extension type 0x001b in wire bytes
    bool found_compress_wire = false;
    const uint8_t *wb = hello.raw_wire_bytes.data();
    size_t wlen = hello.raw_wire_bytes.size();
    for (size_t i = 0; i + 7 <= wlen; ++i) {
        if (wb[i] == 0x00 && wb[i + 1] == 0x1b) { // type 0x001b
            uint16_t elen = (static_cast<uint16_t>(wb[i + 2]) << 8) | wb[i + 3];
            if (elen == 3 && i + 4 + 3 <= wlen) {
                // Payload must be 0x02, 0x00, 0x02 (algorithms length 2, Brotli = 0x0002)
                if (wb[i + 4] == 0x02 && wb[i + 5] == 0x00 && wb[i + 6] == 0x02) {
                    found_compress_wire = true;
                    break;
                }
            }
        }
    }
    TEST_ASSERT(found_compress_wire, "compress_certificate wire payload must be 0x02, 0x00, 0x02 (Brotli) per RFC 8879");

    // 7. JA3 / JA4 Calculators
    std::string ja3 = CalculateJA3(hello);
    std::string ja3_hash = CalculateJA3Hash(hello);
    TEST_ASSERT(!ja3.empty(), "JA3 string must not be empty");
    TEST_ASSERT(ja3_hash.size() == 32, "JA3 hash must be 32 hex chars");
    std::cout << "  JA3 string: " << ja3 << std::endl;
    std::cout << "  JA3 hash:   " << ja3_hash << std::endl;

    std::string ja4 = CalculateJA4(hello, "example.com", "h2");
    TEST_ASSERT(ja4.rfind("t13d15", 0) == 0, "JA4 prefix must be t13d15, got " + ja4);
    std::cout << "  JA4:        " << ja4 << std::endl;

    std::cout << "  [PASS] TLS 1.3 ClientHello Signatures Verified" << std::endl;
    return 0;
}

int TestHttp2Signatures() {
    std::cout << "[TEST] Running HTTP/2 Signatures Test..." << std::endl;

    // 1. Connection Preface + SETTINGS
    std::vector<uint8_t> h2_preface_settings = BuildH2ConnectionPrefaceAndSettings();
    TEST_ASSERT(h2_preface_settings.size() == 24 + 9 + 24, "Preface + SETTINGS frame length mismatch");

    // Check preface string
    std::string preface(reinterpret_cast<const char*>(h2_preface_settings.data()), 24);
    TEST_ASSERT(preface == "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n", "HTTP/2 connection preface mismatch");

    // Check SETTINGS frame
    std::string err;
    TEST_ASSERT(VerifyHttp2Settings(h2_preface_settings.data() + 24, h2_preface_settings.size() - 24, &err),
                "VerifyHttp2Settings failed: " + err);

    // 2. WINDOW_UPDATE (15663105 bytes on stream 0)
    std::vector<uint8_t> win_up = BuildH2WindowUpdate(0, kChromeH2WindowUpdate);
    TEST_ASSERT(win_up.size() == 13, "WINDOW_UPDATE frame size must be 13");
    TEST_ASSERT(VerifyHttp2WindowUpdate(win_up.data(), win_up.size(), &err),
                "VerifyHttp2WindowUpdate failed: " + err);

    // 3. PRIORITY frame (Stream dependency 0, weight 256)
    std::vector<uint8_t> pri = BuildH2PriorityFrame(3, 0, 256);
    TEST_ASSERT(pri.size() == 14, "PRIORITY frame size must be 14");
    // Verify priority wire bytes
    TEST_ASSERT(pri[3] == 0x02, "Frame type must be PRIORITY (0x02)");
    TEST_ASSERT((pri[9] & 0x80) != 0, "Exclusive bit must be set");
    TEST_ASSERT(pri[13] == 255, "Weight byte must be 255 for weight 256");

    // 4. Pseudo-Header order
    std::vector<std::string> valid_h2_headers = {":method", ":authority", ":scheme", ":path"};
    TEST_ASSERT(VerifyH2HeaderOrder(valid_h2_headers, &err), "Valid H2 header order failed");

    std::vector<std::string> invalid_h2_headers = {":method", ":scheme", ":authority", ":path"};
    TEST_ASSERT(!VerifyH2HeaderOrder(invalid_h2_headers, &err), "Invalid H2 header order was incorrectly accepted");

    std::cout << "  [PASS] HTTP/2 Signatures Verified" << std::endl;
    return 0;
}

int TestQuicAndHttp3Signatures() {
    std::cout << "[TEST] Running QUIC & HTTP/3 Signatures Test..." << std::endl;

    QuicTransportParams params;
    std::string err;
    TEST_ASSERT(VerifyQuicParameters(params, &err), "VerifyQuicParameters failed: " + err);

    // Modify a parameter to verify detection
    QuicTransportParams bad_params = params;
    bad_params.initial_max_data = 12345;
    TEST_ASSERT(!VerifyQuicParameters(bad_params, &err), "Invalid QUIC parameters should fail verification");

    // Pseudo-Header order for HTTP/3
    std::vector<std::string> valid_h3_headers = {":method", ":scheme", ":authority", ":path"};
    TEST_ASSERT(VerifyH3HeaderOrder(valid_h3_headers, &err), "Valid H3 header order failed");

    std::vector<std::string> invalid_h3_headers = {":method", ":authority", ":scheme", ":path"};
    TEST_ASSERT(!VerifyH3HeaderOrder(invalid_h3_headers, &err), "Invalid H3 header order was incorrectly accepted");

    std::cout << "  [PASS] QUIC & HTTP/3 Signatures Verified" << std::endl;
    return 0;
}

int TestEngineCoreLifecycle() {
    std::cout << "[TEST] Running Engine Core Lifecycle & C ABI Test..." << std::endl;

    // 1. Create engine
    const char *opts = "{\"force_http3\": true, \"seed\": 123}";
    chrome_engine_t *e = engine_create(opts, strlen(opts));
    TEST_ASSERT(e != nullptr, "engine_create returned null");

    // 2. Perform HTTP request
    const char req[] = "GET https://example.com/test HTTP/1.1\r\nHost: example.com\r\n\r\n";
    char headers[4096];
    size_t headers_len = 0;
    int32_t status = 0;

    chrome_request_t *r = http_request(e, req, sizeof(req) - 1, headers, sizeof(headers), &headers_len, &status);
    TEST_ASSERT(r != nullptr, "http_request returned null");
    TEST_ASSERT(status == 200, "status must be 200");
    TEST_ASSERT(headers_len > 0, "headers_len must be > 0");

    // 3. Read response body in chunks
    char body_buf[16];
    bool eof = false;
    std::string full_body;
    while (!eof) {
        size_t n = http_read_body(r, sizeof(body_buf), body_buf, sizeof(body_buf), &eof);
        full_body.append(body_buf, n);
    }
    TEST_ASSERT(!full_body.empty(), "body must not be empty");
    TEST_ASSERT(full_body.find("https://example.com/test") != std::string::npos, "body must echo requested URL");

    // 4. Destroy request
    int32_t req_st = request_destroy(r);
    TEST_ASSERT(req_st == CHROME_NET_OK, "request_destroy failed");

    // 5. Test WebTransport Adapter
    chrome_wt_session_t *wt = wt_dial(e, "https://example.com/wt");
    TEST_ASSERT(wt != nullptr, "wt_dial returned null");
    const uint8_t dgram_data[] = {0x01, 0x02, 0x03, 0x04};
    TEST_ASSERT(wt->QueueDatagram(dgram_data, sizeof(dgram_data)), "QueueDatagram failed");
    int32_t wt_st = wt_session_destroy(wt);
    TEST_ASSERT(wt_st == CHROME_NET_OK, "wt_session_destroy failed");

    // 6. Test WebSocket Adapter
    chrome_ws_conn_t *ws = ws_dial(e, "wss://example.com/ws");
    TEST_ASSERT(ws != nullptr, "ws_dial returned null");
    const char ws_msg[] = "ping test";
    TEST_ASSERT(ws->SendMessage(WsOpcode::kText, reinterpret_cast<const uint8_t*>(ws_msg), strlen(ws_msg)),
                "ws SendMessage failed");
    int32_t ws_st = ws_conn_destroy(ws);
    TEST_ASSERT(ws_st == CHROME_NET_OK, "ws_conn_destroy failed");

    // Destroy engine
    int32_t eng_st = engine_destroy(e);
    TEST_ASSERT(eng_st == CHROME_NET_OK, "engine_destroy failed");

    std::cout << "  [PASS] Engine Core Lifecycle & C ABI Verified" << std::endl;
    return 0;
}

int TestTeardownChildFirst() {
    std::cout << "[TEST] Running Teardown Order A: Child First -> Parent Second..." << std::endl;
    chrome_engine_t *e = engine_create(nullptr, 0);
    TEST_ASSERT(e != nullptr, "engine_create returned null");

    const char req[] = "GET https://example.com/child_first HTTP/1.1\r\nHost: example.com\r\n\r\n";
    char headers[512];
    size_t headers_len = 0;
    int32_t status = 0;

    chrome_request_t *r = http_request(e, req, sizeof(req) - 1, headers, sizeof(headers), &headers_len, &status);
    TEST_ASSERT(r != nullptr, "http_request returned null");
    chrome_wt_session_t *wt = wt_dial(e, "https://example.com/child_first_wt");
    TEST_ASSERT(wt != nullptr, "wt_dial returned null");
    chrome_ws_conn_t *ws = ws_dial(e, "wss://example.com/child_first_ws");
    TEST_ASSERT(ws != nullptr, "ws_dial returned null");

    // Destroy children first
    TEST_ASSERT(request_destroy(r) == CHROME_NET_OK, "request_destroy failed");
    TEST_ASSERT(wt_session_destroy(wt) == CHROME_NET_OK, "wt_session_destroy failed");
    TEST_ASSERT(ws_conn_destroy(ws) == CHROME_NET_OK, "ws_conn_destroy failed");

    // Then destroy parent - must not crash or double free
    TEST_ASSERT(engine_destroy(e) == CHROME_NET_OK, "engine_destroy after child teardown failed");

    std::cout << "  [PASS] Teardown Child First Verified (No double free)" << std::endl;
    return 0;
}

int TestTeardownParentFirst() {
    std::cout << "[TEST] Running Teardown Order B: Parent First (Cascading Child Teardown)..." << std::endl;
    chrome_engine_t *e = engine_create(nullptr, 0);
    TEST_ASSERT(e != nullptr, "engine_create returned null");

    const char req[] = "GET https://example.com/parent_first HTTP/1.1\r\nHost: example.com\r\n\r\n";
    char headers[512];
    size_t headers_len = 0;
    int32_t status = 0;

    // Create multiple active child handles
    chrome_request_t *r1 = http_request(e, req, sizeof(req) - 1, headers, sizeof(headers), &headers_len, &status);
    chrome_request_t *r2 = http_request(e, req, sizeof(req) - 1, headers, sizeof(headers), &headers_len, &status);
    chrome_wt_session_t *wt1 = wt_dial(e, "https://example.com/pf_wt1");
    chrome_wt_session_t *wt2 = wt_dial(e, "https://example.com/pf_wt2");
    chrome_ws_conn_t *ws1 = ws_dial(e, "wss://example.com/pf_ws1");
    chrome_ws_conn_t *ws2 = ws_dial(e, "wss://example.com/pf_ws2");
    (void)r1; (void)r2; (void)wt1; (void)wt2; (void)ws1; (void)ws2;

    // Destroy parent directly - cascading teardown must safely destroy all children without double free or crash
    TEST_ASSERT(engine_destroy(e) == CHROME_NET_OK, "engine_destroy with multiple active children failed");

    std::cout << "  [PASS] Teardown Parent First (Cascading Cleanup) Verified" << std::endl;
    return 0;
}

int main() {
    std::cout << "========================================" << std::endl;
    std::cout << "Running Chrome-Net Milestone 2 Test Suite" << std::endl;
    std::cout << "========================================" << std::endl;

    if (TestTlsSignatures() != 0) return 1;
    if (TestHttp2Signatures() != 0) return 1;
    if (TestQuicAndHttp3Signatures() != 0) return 1;
    if (TestEngineCoreLifecycle() != 0) return 1;
    if (TestTeardownChildFirst() != 0) return 1;
    if (TestTeardownParentFirst() != 0) return 1;

    std::cout << "========================================" << std::endl;
    std::cout << "ALL MILESTONE 2 TESTS PASSED CLEANLY!" << std::endl;
    std::cout << "========================================" << std::endl;
    return 0;
}

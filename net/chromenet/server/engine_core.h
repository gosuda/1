#ifndef CHROME_NET_ENGINE_CORE_H
#define CHROME_NET_ENGINE_CORE_H

#include "chrome_net.h"
#include "signatures.h"
#include "webtransport_adapter.h"
#include "websocket_adapter.h"

#include <atomic>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <mutex>
#include <string>
#include <unordered_map>
#include <unordered_set>
#include <vector>

namespace chrome_net {

struct EngineOptions {
    std::string user_agent;
    std::string ja3_spec;
    bool force_http3{false};
    bool insecure_skip_verify{false};
    uint32_t timeout_ms{30000};
    uint32_t random_seed{0};
};

EngineOptions ParseEngineOptions(const void *opts, size_t opts_len);

// Default Chrome Headers
std::vector<std::pair<std::string, std::string>> GetDefaultChromeHeaders(const std::string &ua);

} // namespace chrome_net

struct chrome_engine {
    chrome_net::EngineOptions options;
    chrome_net::TlsClientHello client_hello;
    std::atomic<bool> destroyed{false};

    mutable std::mutex mtx;
    std::unordered_set<chrome_request_t*> active_requests;
    std::unordered_set<chrome_wt_session_t*> active_wt_sessions;
    std::unordered_set<chrome_ws_conn_t*> active_ws_conns;
    uint32_t next_id{1};

    explicit chrome_engine(chrome_net::EngineOptions opts);
    ~chrome_engine();

    void RegisterRequest(chrome_request_t *r);
    void UnregisterRequest(chrome_request_t *r);

    void RegisterWtSession(chrome_wt_session_t *s);
    void UnregisterWtSession(chrome_wt_session_t *s);

    void RegisterWsConn(chrome_ws_conn_t *w);
    void UnregisterWsConn(chrome_ws_conn_t *w);
};

struct chrome_request {
    chrome_engine *engine{nullptr};
    std::string method{"GET"};
    std::string url;
    std::vector<std::pair<std::string, std::string>> headers;
    std::vector<uint8_t> request_body;

    int32_t response_status{200};
    std::string response_headers;
    std::vector<uint8_t> response_body;
    size_t body_read_pos{0};

    mutable std::mutex mtx;

    chrome_request(chrome_engine *e, std::string m, std::string u);
    ~chrome_request();

    void SetResponse(int32_t status, const std::string &hdr, std::vector<uint8_t> body);
    size_t ReadBody(uint32_t max_len, void *buf, size_t buf_cap, bool *eof);
};

#endif // CHROME_NET_ENGINE_CORE_H

#ifndef CHROME_NET_WEBSOCKET_ADAPTER_H
#define CHROME_NET_WEBSOCKET_ADAPTER_H

#include "chrome_net.h"
#include <cstddef>
#include <cstdint>
#include <deque>
#include <mutex>
#include <string>
#include <vector>

namespace chrome_net {

enum class WsOpcode : uint8_t {
    kContinuation = 0x0,
    kText = 0x1,
    kBinary = 0x2,
    kClose = 0x8,
    kPing = 0x9,
    kPong = 0xa
};

enum class WsMode {
    kRfc6455_Http1,     // Standard HTTP/1.1 Upgrade
    kRfc8441_Http2ExtendedConnect // Extended CONNECT over HTTP/2
};

enum class WsState {
    kConnecting,
    kOpen,
    kClosing,
    kClosed
};

struct WsFrame {
    bool fin{true};
    WsOpcode opcode{WsOpcode::kText};
    bool masked{true};
    uint32_t mask_key{0};
    std::vector<uint8_t> payload;
};

// Frame serialization & parsing helpers
std::vector<uint8_t> EncodeWsFrame(const WsFrame &frame);
bool DecodeWsFrame(const uint8_t *data, size_t len, WsFrame *frame, size_t *consumed);

} // namespace chrome_net

struct chrome_engine;

struct chrome_ws_conn {
    chrome_engine *engine{nullptr};
    uint32_t conn_id{0};
    std::string url;
    chrome_net::WsMode mode{chrome_net::WsMode::kRfc8441_Http2ExtendedConnect};
    chrome_net::WsState state{chrome_net::WsState::kConnecting};
    uint16_t close_code{1000};
    std::string close_reason;

    mutable std::mutex mtx;
    std::deque<chrome_net::WsFrame> incoming_frames;
    std::deque<chrome_net::WsFrame> outgoing_frames;

    chrome_ws_conn(uint32_t id, std::string target_url, chrome_net::WsMode m);
    ~chrome_ws_conn();

    bool IsActive() const;
    void Close(uint16_t code, const std::string &reason);
    bool SendMessage(chrome_net::WsOpcode opcode, const uint8_t *payload, size_t len);
    size_t ReceiveMessage(uint8_t *out, size_t cap, chrome_net::WsOpcode *out_opcode);
};

#endif // CHROME_NET_WEBSOCKET_ADAPTER_H

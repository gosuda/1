#ifndef CHROME_NET_WEBTRANSPORT_ADAPTER_H
#define CHROME_NET_WEBTRANSPORT_ADAPTER_H

#include "chrome_net.h"
#include <cstddef>
#include <cstdint>
#include <deque>
#include <mutex>
#include <string>
#include <vector>

namespace chrome_net {

enum class WtState {
    kConnecting,
    kOpen,
    kDraining,
    kClosed
};

struct WtDatagram {
    std::vector<uint8_t> payload;
};

struct WtStreamBuffer {
    uint32_t stream_id{0};
    bool is_bidi{true};
    std::vector<uint8_t> rx_buffer;
    std::vector<uint8_t> tx_buffer;
    bool fin_received{false};
    bool fin_sent{false};
};

} // namespace chrome_net

struct chrome_engine;

struct chrome_wt_session {
    chrome_engine *engine{nullptr};
    uint32_t session_id{0};
    std::string url;
    chrome_net::WtState state{chrome_net::WtState::kConnecting};
    uint32_t close_code{0};
    std::string close_reason;

    mutable std::mutex mtx;
    std::deque<chrome_net::WtDatagram> incoming_datagrams;
    std::deque<chrome_net::WtDatagram> outgoing_datagrams;
    std::vector<chrome_net::WtStreamBuffer> streams;

    chrome_wt_session(uint32_t id, std::string target_url);
    ~chrome_wt_session();

    bool IsActive() const;
    void Close(uint32_t code, const std::string &reason);
    bool QueueDatagram(const uint8_t *data, size_t len);
    size_t PopDatagram(uint8_t *out, size_t cap);
};

#endif // CHROME_NET_WEBTRANSPORT_ADAPTER_H

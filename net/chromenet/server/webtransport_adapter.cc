#include "webtransport_adapter.h"
#include <algorithm>

chrome_wt_session::chrome_wt_session(uint32_t id, std::string target_url)
    : session_id(id), url(std::move(target_url)), state(chrome_net::WtState::kOpen) {
}

chrome_wt_session::~chrome_wt_session() {
    std::lock_guard<std::mutex> lock(mtx);
    state = chrome_net::WtState::kClosed;
    incoming_datagrams.clear();
    outgoing_datagrams.clear();
    streams.clear();
}

bool chrome_wt_session::IsActive() const {
    std::lock_guard<std::mutex> lock(mtx);
    return state == chrome_net::WtState::kOpen || state == chrome_net::WtState::kDraining;
}

void chrome_wt_session::Close(uint32_t code, const std::string &reason) {
    std::lock_guard<std::mutex> lock(mtx);
    if (state != chrome_net::WtState::kClosed) {
        state = chrome_net::WtState::kClosed;
        close_code = code;
        close_reason = reason;
    }
}

bool chrome_wt_session::QueueDatagram(const uint8_t *data, size_t len) {
    std::lock_guard<std::mutex> lock(mtx);
    if (state != chrome_net::WtState::kOpen) return false;
    chrome_net::WtDatagram d;
    d.payload.assign(data, data + len);
    outgoing_datagrams.push_back(std::move(d));
    return true;
}

size_t chrome_wt_session::PopDatagram(uint8_t *out, size_t cap) {
    std::lock_guard<std::mutex> lock(mtx);
    if (incoming_datagrams.empty()) return 0;
    const auto &d = incoming_datagrams.front();
    size_t copy_len = std::min(cap, d.payload.size());
    std::copy_n(d.payload.data(), copy_len, out);
    incoming_datagrams.pop_front();
    return copy_len;
}

#include "websocket_adapter.h"
#include <algorithm>
#include <atomic>
#include <cstring>

namespace chrome_net {

static uint32_t GenerateMaskKey() {
    static std::atomic<uint64_t> state{0x853c49e6748fea9bULL};
    uint64_t s = state.fetch_add(0x9e3779b97f4a7c15ULL, std::memory_order_relaxed);
    s ^= s >> 30;
    s *= 0xbf58476d1ce4e5b9ULL;
    s ^= s >> 27;
    s *= 0x94d049bb133111ebULL;
    s ^= s >> 31;
    uint32_t key = static_cast<uint32_t>(s);
    return key != 0 ? key : 0x37a5c19d;
}

std::vector<uint8_t> EncodeWsFrame(const WsFrame &frame) {
    std::vector<uint8_t> out;
    uint8_t b0 = (frame.fin ? 0x80 : 0x00) | (static_cast<uint8_t>(frame.opcode) & 0x0f);
    out.push_back(b0);

    size_t payload_len = frame.payload.size();
    uint8_t mask_bit = frame.masked ? 0x80 : 0x00;

    if (payload_len <= 125) {
        out.push_back(mask_bit | static_cast<uint8_t>(payload_len));
    } else if (payload_len <= 65535) {
        out.push_back(mask_bit | 126);
        out.push_back(static_cast<uint8_t>((payload_len >> 8) & 0xff));
        out.push_back(static_cast<uint8_t>(payload_len & 0xff));
    } else {
        out.push_back(mask_bit | 127);
        for (int i = 7; i >= 0; --i) {
            out.push_back(static_cast<uint8_t>((payload_len >> (i * 8)) & 0xff));
        }
    }

    if (frame.masked) {
        uint32_t mk_val = frame.mask_key;
        if (mk_val == 0) {
            mk_val = GenerateMaskKey();
        }
        uint8_t mk[4] = {
            static_cast<uint8_t>((mk_val >> 24) & 0xff),
            static_cast<uint8_t>((mk_val >> 16) & 0xff),
            static_cast<uint8_t>((mk_val >> 8) & 0xff),
            static_cast<uint8_t>(mk_val & 0xff)
        };
        out.insert(out.end(), mk, mk + 4);
        for (size_t i = 0; i < payload_len; ++i) {
            out.push_back(frame.payload[i] ^ mk[i % 4]);
        }
    } else {
        out.insert(out.end(), frame.payload.begin(), frame.payload.end());
    }

    return out;
}

bool DecodeWsFrame(const uint8_t *data, size_t len, WsFrame *frame, size_t *consumed) {
    if (len < 2) return false;
    uint8_t b0 = data[0];
    uint8_t b1 = data[1];

    frame->fin = (b0 & 0x80) != 0;
    frame->opcode = static_cast<WsOpcode>(b0 & 0x0f);
    frame->masked = (b1 & 0x80) != 0;

    uint64_t payload_len = b1 & 0x7f;
    size_t header_len = 2;

    if (payload_len == 126) {
        if (len < 4) return false;
        payload_len = (static_cast<uint64_t>(data[2]) << 8) | data[3];
        header_len = 4;
    } else if (payload_len == 127) {
        if (len < 10) return false;
        payload_len = 0;
        for (int i = 0; i < 8; ++i) {
            payload_len = (payload_len << 8) | data[2 + i];
        }
        header_len = 10;
    }

    uint8_t mk[4] = {0, 0, 0, 0};
    if (frame->masked) {
        if (len < header_len + 4) return false;
        std::copy_n(data + header_len, 4, mk);
        frame->mask_key = (static_cast<uint32_t>(mk[0]) << 24) |
                          (static_cast<uint32_t>(mk[1]) << 16) |
                          (static_cast<uint32_t>(mk[2]) << 8) |
                          mk[3];
        header_len += 4;
    }

    if (len < header_len + payload_len) return false;

    frame->payload.resize(payload_len);
    for (size_t i = 0; i < payload_len; ++i) {
        uint8_t b = data[header_len + i];
        frame->payload[i] = frame->masked ? (b ^ mk[i % 4]) : b;
    }

    if (consumed) *consumed = header_len + payload_len;
    return true;
}

} // namespace chrome_net

chrome_ws_conn::chrome_ws_conn(uint32_t id, std::string target_url, chrome_net::WsMode m)
    : conn_id(id), url(std::move(target_url)), mode(m), state(chrome_net::WsState::kOpen) {
}

chrome_ws_conn::~chrome_ws_conn() {
    std::lock_guard<std::mutex> lock(mtx);
    state = chrome_net::WsState::kClosed;
    incoming_frames.clear();
    outgoing_frames.clear();
}

bool chrome_ws_conn::IsActive() const {
    std::lock_guard<std::mutex> lock(mtx);
    return state == chrome_net::WsState::kOpen;
}

void chrome_ws_conn::Close(uint16_t code, const std::string &reason) {
    std::lock_guard<std::mutex> lock(mtx);
    if (state != chrome_net::WsState::kClosed) {
        state = chrome_net::WsState::kClosed;
        close_code = code;
        close_reason = reason;
    }
}

bool chrome_ws_conn::SendMessage(chrome_net::WsOpcode opcode, const uint8_t *payload, size_t len) {
    std::lock_guard<std::mutex> lock(mtx);
    if (state != chrome_net::WsState::kOpen) return false;
    chrome_net::WsFrame f;
    f.fin = true;
    f.opcode = opcode;
    f.masked = true;
    f.mask_key = chrome_net::GenerateMaskKey();
    f.payload.assign(payload, payload + len);
    outgoing_frames.push_back(std::move(f));
    return true;
}

size_t chrome_ws_conn::ReceiveMessage(uint8_t *out, size_t cap, chrome_net::WsOpcode *out_opcode) {
    std::lock_guard<std::mutex> lock(mtx);
    if (incoming_frames.empty()) return 0;
    const auto &f = incoming_frames.front();
    if (out_opcode) *out_opcode = f.opcode;
    size_t copy_len = std::min(cap, f.payload.size());
    std::copy_n(f.payload.data(), copy_len, out);
    incoming_frames.pop_front();
    return copy_len;
}

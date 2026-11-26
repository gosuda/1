// Copyright 2026 Go Suda. MIT License.
//
// WebTransport through //net's WebTransportClient (HTTP/3). A WtConn is created on a session thread
// and driven on the network thread. Session threads block on it for events, stream opens and
// writes; streams are named by ids that the engine assigns.

#ifdef UNSAFE_BUFFERS_BUILD
#pragma allow_unsafe_buffers
#endif

#include <string.h>

#include <algorithm>
#include <deque>
#include <map>
#include <memory>
#include <optional>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include "base/functional/bind.h"
#include "base/functional/callback_helpers.h"
#include "base/memory/raw_ptr.h"
#include "base/memory/scoped_refptr.h"
#include "base/no_destructor.h"
#include "base/synchronization/condition_variable.h"
#include "base/synchronization/lock.h"
#include "base/time/time.h"
#include "net/base/net_errors.h"
#include "net/base/network_anonymization_key.h"
#include "net/base/network_handle.h"
#include "net/http/http_request_headers.h"
#include "net/http/http_response_headers.h"
#include "net/quic/web_transport_client.h"
#include "net/third_party/quiche/src/quiche/quic/core/quic_error_codes.h"
#include "net/url_request/url_request_context.h"
#include "third_party/chromenet/chrome_net.h"
#include "third_party/chromenet/internal.h"
#include "url/gurl.h"
#include "url/origin.h"

namespace chromenet {

namespace {

// Event kinds of chrome_wt_event.
constexpr int kEventIncomingBidi = 1;
constexpr int kEventIncomingUni = 2;
constexpr int kEventDatagram = 3;
constexpr int kEventStreamData = 4;
constexpr int kEventClosed = 5;
constexpr int kEventReset = 6;
constexpr int kEventStopSending = 7;
constexpr int kEventError = 8;

// Receive window of a stream: bytes read from the network and not yet consumed by the caller.
constexpr size_t kRecvWindow = 1u << 20;
// A write returns once at most this many bytes of the stream still wait for flow control.
constexpr size_t kSendBacklog = 256u << 10;
constexpr size_t kIoChunk = 64u << 10;
constexpr size_t kMaxQueuedDatagrams = 256;
// OpenStream waits this long for the peer to raise its stream limit.
constexpr base::TimeDelta kOpenStreamTimeout = base::Seconds(30);

struct Event {
  int kind = 0;
  uint64_t stream_id = 0;
  std::string data;
  size_t offset = 0;
  bool fin = false;
  uint32_t code = 0;
};

class WtConn;

// The visitor of one QUIC stream. Owned by the stream; every member runs on the network thread.
class StreamVisitor : public webtransport::StreamVisitor {
 public:
  StreamVisitor(WtConn* conn, uint64_t id, webtransport::Stream* stream);
  ~StreamVisitor() override;

  void OnCanRead() override { PumpRead(); }
  void OnCanWrite() override { PumpWrite(); }
  void OnResetStreamReceived(webtransport::StreamErrorCode error) override;
  void OnStopSendingReceived(webtransport::StreamErrorCode error) override;
  void OnWriteSideInDataRecvdState() override {}

  void PumpRead();
  void PumpWrite();
  void Write(std::string data, bool fin);
  void Consume(size_t n);
  void Abort(int mode, uint32_t code);

 private:
  const raw_ptr<WtConn> conn_;
  const uint64_t id_;
  const raw_ptr<webtransport::Stream> stream_;
  size_t unconsumed_ = 0;
  bool fin_received_ = false;
  std::string pending_;
  size_t pending_offset_ = 0;
  bool fin_pending_ = false;
  bool fin_sent_ = false;
};

class WtConn : public base::RefCountedThreadSafe<WtConn>, public net::WebTransportClientVisitor {
 public:
  WtConn(scoped_refptr<SharedContext> shared, uint64_t id)
      : shared_(std::move(shared)), id_(id), cv_(&lock_) {}
  WtConn(const WtConn&) = delete;
  WtConn& operator=(const WtConn&) = delete;

  uint64_t id() const { return id_; }

  // Network thread.
  void Start(const GURL& url, net::HttpRequestHeaders headers);
  void Shutdown();
  void DoOpenStream(bool bidi, uint64_t* stream_id, int* result);
  void DoWrite(uint64_t stream_id, std::string data, bool fin, int* result);
  void DoAbort(uint64_t stream_id, int mode, uint32_t code, int* result);
  void DoConsume(uint64_t stream_id, uint32_t n);
  void DoSendDatagram(std::string data, int* result);
  void DoMaxDatagram(int64_t* result);
  void DoClose(uint32_t code, std::string reason);

  // Called by stream visitors on the network thread.
  void PushEvent(Event event);
  void SetPending(uint64_t stream_id, size_t pending);
  void ForgetStream(uint64_t stream_id);
  void RegisterStream(uint64_t stream_id, StreamVisitor* visitor) { streams_[stream_id] = visitor; }
  void UnregisterStream(uint64_t stream_id) { streams_.erase(stream_id); }
  uint64_t NewStreamId() { return next_stream_id_++; }

  // Any thread.
  void Fail(int error, std::string detail = std::string());

  // Session threads.
  int WaitHandshake(std::string* head);
  size_t NextEvent(char* buf, size_t cap, int* kind, uint64_t* stream_id, bool* fin, uint32_t* code);
  int OpenStream(bool bidi, uint64_t* stream_id);
  int WriteStream(uint64_t stream_id, const char* data, size_t len, bool fin);

  // net::WebTransportClientVisitor:
  void OnLocalNetworkAccessCheck(const net::IPEndPoint& server_address,
                                 const net::NetLogWithSource& net_log,
                                 net::CompletionOnceCallback callback) override {
    std::move(callback).Run(net::OK);
  }
  void OnBeforeConnect(const net::IPEndPoint& server_address) override {}
  void OnConnected(scoped_refptr<net::HttpResponseHeaders> response_headers) override;
  void OnConnectionFailed(const net::WebTransportError& error) override;
  void OnDraining() override {}
  void OnClosed(const std::optional<net::WebTransportCloseInfo>& close_info) override;
  void OnError(const net::WebTransportError& error) override;
  void OnIncomingBidirectionalStreamAvailable() override { AcceptStreams(true); }
  void OnIncomingUnidirectionalStreamAvailable() override { AcceptStreams(false); }
  void OnDatagramReceived(std::string_view datagram) override;
  void OnCanCreateNewOutgoingBidirectionalStream() override { BumpCanOpen(); }
  void OnCanCreateNewOutgoingUnidirectionalStream() override { BumpCanOpen(); }
  void OnDatagramProcessed(std::optional<quic::DatagramStatus> status) override {}

 private:
  friend class base::RefCountedThreadSafe<WtConn>;
  ~WtConn() override = default;

  void AcceptStreams(bool bidi);
  void BumpCanOpen();
  StreamVisitor* Adopt(webtransport::Stream* stream, uint64_t* id);

  const scoped_refptr<SharedContext> shared_;
  const uint64_t id_;

  // Network thread only.
  std::unique_ptr<net::WebTransportClient> client_;
  std::map<uint64_t, StreamVisitor*> streams_;
  uint64_t next_stream_id_ = 1;

  base::Lock lock_;
  base::ConditionVariable cv_;
  bool handshake_done_ = false;
  std::string head_;
  int handshake_error_ = 0;
  bool closed_ = false;
  std::deque<Event> events_;
  size_t queued_datagrams_ = 0;
  bool has_terminal_ = false;
  Event terminal_;
  uint64_t can_open_generation_ = 0;
  std::map<uint64_t, size_t> pending_;  // send backlog per stream
};

// StreamVisitor ----------------------------------------------------------------------------------

StreamVisitor::StreamVisitor(WtConn* conn, uint64_t id, webtransport::Stream* stream)
    : conn_(conn), id_(id), stream_(stream) {
  conn_->RegisterStream(id_, this);
}

StreamVisitor::~StreamVisitor() {
  conn_->UnregisterStream(id_);
  conn_->ForgetStream(id_);
}

void StreamVisitor::OnResetStreamReceived(webtransport::StreamErrorCode error) {
  Event e;
  e.kind = kEventReset;
  e.stream_id = id_;
  e.code = error;
  conn_->PushEvent(std::move(e));
}

void StreamVisitor::OnStopSendingReceived(webtransport::StreamErrorCode error) {
  Event e;
  e.kind = kEventStopSending;
  e.stream_id = id_;
  e.code = error;
  conn_->PushEvent(std::move(e));
}

void StreamVisitor::PumpRead() {
  while (!fin_received_) {
    size_t readable = stream_->ReadableBytes();
    size_t room = kRecvWindow - std::min(unconsumed_, kRecvWindow);
    if (readable == 0) {
      if (!stream_->PeekNextReadableRegion().fin_next) {
        return;
      }
    } else if (room == 0) {
      return;
    }
    size_t n = std::min({readable, room, kIoChunk});
    std::string buf(n, '\0');
    webtransport::Stream::ReadResult r = stream_->Read(absl::MakeSpan(buf.data(), buf.size()));
    buf.resize(r.bytes_read);
    unconsumed_ += r.bytes_read;
    fin_received_ = r.fin;
    Event e;
    e.kind = kEventStreamData;
    e.stream_id = id_;
    e.data = std::move(buf);
    e.fin = r.fin;
    conn_->PushEvent(std::move(e));
  }
}

void StreamVisitor::Consume(size_t n) {
  unconsumed_ -= std::min(unconsumed_, n);
  PumpRead();
}

void StreamVisitor::Write(std::string data, bool fin) {
  if (pending_offset_ == pending_.size()) {
    pending_ = std::move(data);
    pending_offset_ = 0;
  } else {
    pending_.append(data);
  }
  fin_pending_ |= fin;
  PumpWrite();
}

void StreamVisitor::PumpWrite() {
  while (pending_offset_ < pending_.size() && stream_->CanWrite()) {
    size_t n = std::min(kIoChunk, pending_.size() - pending_offset_);
    if (!stream_->Write(absl::string_view(pending_.data() + pending_offset_, n))) {
      break;
    }
    pending_offset_ += n;
  }
  if (pending_offset_ == pending_.size()) {
    pending_.clear();
    pending_offset_ = 0;
    if (fin_pending_ && !fin_sent_ && stream_->SendFin()) {
      fin_sent_ = true;
    }
  }
  conn_->SetPending(id_, pending_.size() - pending_offset_);
}

void StreamVisitor::Abort(int mode, uint32_t code) {
  if (mode == 1) {
    stream_->ResetWithUserCode(code);
  } else {
    stream_->SendStopSending(code);
  }
}

// WtConn: network thread -------------------------------------------------------------------------

void WtConn::Start(const GURL& url, net::HttpRequestHeaders headers) {
  net::URLRequestContext* context = shared_->context();
  if (!context || !url.SchemeIs("https")) {
    Fail(context ? net::ERR_INVALID_ARGUMENT : net::ERR_FAILED);
    return;
  }
  url::Origin origin = TakeOrigin(url, &headers);
  net::WebTransportParameters params;
  params.enable_web_transport_http3 = true;
  params.additional_headers = headers.GetHeaderVector();
  client_ = net::CreateWebTransportClient(url, origin, this, net::NetworkAnonymizationKey(),
                                          net::handles::kInvalidNetworkHandle, context, params);
  client_->Connect();
}

void WtConn::Shutdown() {
  {
    base::AutoLock hold(lock_);
    closed_ = true;
    if (!handshake_done_) {
      handshake_done_ = true;
      handshake_error_ = net::ERR_ABORTED;
      head_ = "0\n";
    }
    cv_.Broadcast();
  }
  // Close() needs an open session; a client that is still connecting or already failed is just
  // dropped.
  if (client_ && client_->session()) {
    client_->Close(std::nullopt);
  }
  client_.reset();  // destroys the stream visitors
}

webtransport::Session* SessionOf(net::WebTransportClient* client) {
  return client ? client->session() : nullptr;
}

StreamVisitor* WtConn::Adopt(webtransport::Stream* stream, uint64_t* id) {
  *id = NewStreamId();
  auto visitor = std::make_unique<StreamVisitor>(this, *id, stream);
  StreamVisitor* raw = visitor.get();
  stream->SetVisitor(std::move(visitor));
  return raw;
}

void WtConn::AcceptStreams(bool bidi) {
  webtransport::Session* session = SessionOf(client_.get());
  while (session) {
    webtransport::Stream* stream = bidi ? session->AcceptIncomingBidirectionalStream()
                                        : session->AcceptIncomingUnidirectionalStream();
    if (!stream) {
      return;
    }
    uint64_t id = 0;
    StreamVisitor* visitor = Adopt(stream, &id);
    Event e;
    e.kind = bidi ? kEventIncomingBidi : kEventIncomingUni;
    e.stream_id = id;
    PushEvent(std::move(e));
    visitor->PumpRead();
  }
}

void WtConn::DoOpenStream(bool bidi, uint64_t* stream_id, int* result) {
  webtransport::Session* session = SessionOf(client_.get());
  if (!session) {
    *result = net::ERR_CONNECTION_CLOSED;
    return;
  }
  if (!(bidi ? session->CanOpenNextOutgoingBidirectionalStream()
             : session->CanOpenNextOutgoingUnidirectionalStream())) {
    *result = net::ERR_IO_PENDING;  // the caller waits for OnCanCreateNew...
    return;
  }
  webtransport::Stream* stream =
      bidi ? session->OpenOutgoingBidirectionalStream() : session->OpenOutgoingUnidirectionalStream();
  if (!stream) {
    *result = net::ERR_FAILED;
    return;
  }
  StreamVisitor* visitor = Adopt(stream, stream_id);
  *result = net::OK;
  visitor->PumpRead();
}

void WtConn::DoWrite(uint64_t stream_id, std::string data, bool fin, int* result) {
  auto it = streams_.find(stream_id);
  if (it == streams_.end()) {
    *result = data.empty() ? net::OK : net::ERR_CONNECTION_CLOSED;
    return;
  }
  it->second->Write(std::move(data), fin);
  *result = net::OK;
}

void WtConn::DoAbort(uint64_t stream_id, int mode, uint32_t code, int* result) {
  auto it = streams_.find(stream_id);
  if (it != streams_.end()) {
    it->second->Abort(mode, code);
  }
  *result = net::OK;
}

void WtConn::DoConsume(uint64_t stream_id, uint32_t n) {
  auto it = streams_.find(stream_id);
  if (it != streams_.end()) {
    it->second->Consume(n);
  }
}

void WtConn::DoSendDatagram(std::string data, int* result) {
  webtransport::Session* session = SessionOf(client_.get());
  if (!session) {
    *result = net::ERR_CONNECTION_CLOSED;
    return;
  }
  switch (session->SendOrQueueDatagram(data).code) {
    case webtransport::DatagramStatusCode::kSuccess:
    case webtransport::DatagramStatusCode::kBlocked:  // queued
      *result = net::OK;
      break;
    case webtransport::DatagramStatusCode::kTooBig:
      *result = net::ERR_MSG_TOO_BIG;
      break;
    default:
      *result = net::ERR_FAILED;
      break;
  }
}

void WtConn::DoMaxDatagram(int64_t* result) {
  webtransport::Session* session = SessionOf(client_.get());
  *result = session ? static_cast<int64_t>(session->GetMaxDatagramSize())
                    : static_cast<int64_t>(net::ERR_CONNECTION_CLOSED);
}

void WtConn::DoClose(uint32_t code, std::string reason) {
  if (client_ && client_->session()) {
    client_->Close(net::WebTransportCloseInfo(code, reason));
  }
}

// WtConn: visitor callbacks (network thread) -------------------------------------------------------

void WtConn::OnConnected(scoped_refptr<net::HttpResponseHeaders> response_headers) {
  std::string head =
      std::to_string(response_headers ? response_headers->response_code() : 200) + "\n";
  if (response_headers) {
    size_t iter = 0;
    std::string name;
    std::string value;
    while (response_headers->EnumerateHeaderLines(&iter, &name, &value)) {
      head += name + ": " + value + "\n";
    }
  }
  base::AutoLock hold(lock_);
  if (handshake_done_) {
    return;
  }
  handshake_done_ = true;
  head_ = std::move(head);
  cv_.Broadcast();
}

void WtConn::OnConnectionFailed(const net::WebTransportError& error) {
  std::string detail = "quic-error: " + std::string(quic::QuicErrorCodeToString(error.quic_error)) + "\n";
  if (!error.details.empty()) {
    detail += "details: " + error.details + "\n";
  }
  Fail(error.net_error != net::OK ? error.net_error : net::ERR_FAILED, std::move(detail));
}

void WtConn::OnClosed(const std::optional<net::WebTransportCloseInfo>& close_info) {
  base::AutoLock hold(lock_);
  if (has_terminal_) {
    return;
  }
  has_terminal_ = true;
  terminal_.kind = kEventClosed;
  if (close_info) {
    terminal_.code = close_info->code;
    terminal_.data = close_info->reason;
  }
  cv_.Broadcast();
}

void WtConn::OnError(const net::WebTransportError& error) {
  base::AutoLock hold(lock_);
  if (has_terminal_) {
    return;
  }
  has_terminal_ = true;
  terminal_.kind = kEventError;
  terminal_.code = static_cast<uint32_t>(error.net_error != net::OK ? error.net_error : net::ERR_FAILED);
  terminal_.data = "quic-error: " + std::string(quic::QuicErrorCodeToString(error.quic_error));
  if (!error.details.empty()) {
    terminal_.data += "; details: " + error.details;
  }
  cv_.Broadcast();
}

void WtConn::OnDatagramReceived(std::string_view datagram) {
  base::AutoLock hold(lock_);
  if (queued_datagrams_ >= kMaxQueuedDatagrams) {
    return;  // datagrams are unreliable: drop what the caller does not keep up with
  }
  queued_datagrams_++;
  Event e;
  e.kind = kEventDatagram;
  e.data.assign(datagram);
  events_.push_back(std::move(e));
  cv_.Broadcast();
}

void WtConn::BumpCanOpen() {
  base::AutoLock hold(lock_);
  can_open_generation_++;
  cv_.Broadcast();
}

void WtConn::PushEvent(Event event) {
  base::AutoLock hold(lock_);
  events_.push_back(std::move(event));
  cv_.Broadcast();
}

void WtConn::SetPending(uint64_t stream_id, size_t pending) {
  base::AutoLock hold(lock_);
  if (pending == 0) {
    pending_.erase(stream_id);
  } else {
    pending_[stream_id] = pending;
  }
  cv_.Broadcast();
}

void WtConn::ForgetStream(uint64_t stream_id) {
  base::AutoLock hold(lock_);
  pending_.erase(stream_id);
  cv_.Broadcast();
}

void WtConn::Fail(int error, std::string detail) {
  base::AutoLock hold(lock_);
  if (!handshake_done_) {
    handshake_done_ = true;
    handshake_error_ = error;
    head_ = "0\n" + detail;
  } else if (!has_terminal_) {
    has_terminal_ = true;
    terminal_.kind = kEventError;
    terminal_.code = static_cast<uint32_t>(error);
  }
  cv_.Broadcast();
}

// WtConn: session threads --------------------------------------------------------------------------

int WtConn::WaitHandshake(std::string* head) {
  base::AutoLock hold(lock_);
  while (!handshake_done_) {
    cv_.Wait();
  }
  *head = head_;
  return handshake_error_;
}

size_t WtConn::NextEvent(char* buf,
                         size_t cap,
                         int* kind,
                         uint64_t* stream_id,
                         bool* fin,
                         uint32_t* code) {
  base::AutoLock hold(lock_);
  while (events_.empty() && !has_terminal_ && !closed_) {
    cv_.Wait();
  }
  *stream_id = 0;
  *fin = false;
  *code = 0;
  if (events_.empty()) {
    if (has_terminal_) {
      *kind = terminal_.kind;
      *code = terminal_.code;
      size_t n = std::min(cap, terminal_.data.size());
      memcpy(buf, terminal_.data.data(), n);
      return n;
    }
    *kind = kEventClosed;  // closed locally
    return 0;
  }
  Event& front = events_.front();
  *kind = front.kind;
  *stream_id = front.stream_id;
  *code = front.code;
  size_t n = std::min(cap, front.data.size() - front.offset);
  memcpy(buf, front.data.data() + front.offset, n);
  front.offset += n;
  if (front.offset == front.data.size()) {
    *fin = front.fin;
    if (front.kind == kEventDatagram) {
      queued_datagrams_--;
    }
    events_.pop_front();
  }
  return n;
}

int WtConn::OpenStream(bool bidi, uint64_t* stream_id) {
  for (;;) {
    uint64_t generation;
    {
      base::AutoLock hold(lock_);
      if (closed_ || has_terminal_) {
        return net::ERR_CONNECTION_CLOSED;
      }
      generation = can_open_generation_;
    }
    int result = net::OK;
    RunOnNetworkThread(base::BindOnce(&WtConn::DoOpenStream, this, bidi,
                                      base::Unretained(stream_id), base::Unretained(&result)));
    if (result != net::ERR_IO_PENDING) {
      return result;
    }
    base::AutoLock hold(lock_);
    const base::TimeTicks deadline = base::TimeTicks::Now() + kOpenStreamTimeout;
    while (generation == can_open_generation_ && !closed_ && !has_terminal_) {
      base::TimeDelta left = deadline - base::TimeTicks::Now();
      if (left <= base::TimeDelta()) {
        return net::ERR_TIMED_OUT;
      }
      cv_.TimedWait(left);
    }
  }
}

int WtConn::WriteStream(uint64_t stream_id, const char* data, size_t len, bool fin) {
  int result = net::OK;
  RunOnNetworkThread(base::BindOnce(&WtConn::DoWrite, this, stream_id, std::string(data, len), fin,
                                    base::Unretained(&result)));
  if (result != net::OK) {
    return result;
  }
  base::AutoLock hold(lock_);
  for (;;) {
    auto it = pending_.find(stream_id);
    if (it == pending_.end() || it->second <= kSendBacklog || closed_ || has_terminal_) {
      return net::OK;
    }
    cv_.Wait();
  }
}

struct Registry {
  base::Lock lock;
  std::map<uint64_t, scoped_refptr<WtConn>> conns;
  uint64_t next_id = 1;
};

Registry& GetWtRegistry() {
  static base::NoDestructor<Registry> registry;
  return *registry;
}

scoped_refptr<WtConn> FindConn(uint64_t id) {
  Registry& r = GetWtRegistry();
  base::AutoLock hold(r.lock);
  auto it = r.conns.find(id);
  return it == r.conns.end() ? nullptr : it->second;
}

}  // namespace

}  // namespace chromenet

struct chrome_wt {
  scoped_refptr<chromenet::WtConn> conn;
};

extern "C" {

chrome_wt_t* chrome_wt_start(chrome_engine_t* e, const void* head, size_t head_len) {
  chromenet::Registry& registry = chromenet::GetWtRegistry();
  scoped_refptr<chromenet::WtConn> conn;
  {
    base::AutoLock hold(registry.lock);
    uint64_t id = registry.next_id++;
    conn = base::MakeRefCounted<chromenet::WtConn>(e->shared, id);
    registry.conns[id] = conn;
  }
  GURL url;
  net::HttpRequestHeaders headers;
  if (!chromenet::ParseConnectHead(static_cast<const char*>(head), head_len, &url, &headers)) {
    conn->Fail(net::ERR_INVALID_ARGUMENT);
  } else {
    chromenet::NetRunner()->PostTask(
        FROM_HERE, base::BindOnce(&chromenet::WtConn::Start, conn, std::move(url),
                                  std::move(headers)));
  }
  return new chrome_wt{conn};
}

int32_t chrome_wt_wait(chrome_wt_t* w, void* head, size_t head_cap, size_t* head_len, uint64_t* id) {
  std::string h;
  int rv = w->conn->WaitHandshake(&h);
  *id = rv == net::OK ? w->conn->id() : 0;
  if (h.size() > head_cap) {
    h.resize(head_cap);
  }
  memcpy(head, h.data(), h.size());
  *head_len = h.size();
  return rv;
}

void chrome_wt_cancel(chrome_wt_t* w) {
  w->conn->Fail(net::ERR_ABORTED);
  chromenet::NetRunner()->PostTask(FROM_HERE,
                                   base::BindOnce(&chromenet::WtConn::Shutdown, w->conn));
}

int32_t chrome_wt_destroy(chrome_wt_t* w) {
  chromenet::Registry& registry = chromenet::GetWtRegistry();
  {
    base::AutoLock hold(registry.lock);
    registry.conns.erase(w->conn->id());
  }
  chromenet::RunOnNetworkThread(base::BindOnce(&chromenet::WtConn::Shutdown, w->conn));
  delete w;
  return CHROME_NET_OK;
}

size_t chrome_wt_event(uint64_t id,
                       void* buf,
                       size_t buf_cap,
                       int32_t* kind,
                       uint64_t* stream_id,
                       bool* fin,
                       uint32_t* code) {
  scoped_refptr<chromenet::WtConn> conn = chromenet::FindConn(id);
  if (!conn) {
    *kind = 5;
    *stream_id = 0;
    *fin = false;
    *code = 0;
    return 0;
  }
  int k = 0;
  size_t n = conn->NextEvent(static_cast<char*>(buf), buf_cap, &k, stream_id, fin, code);
  *kind = k;
  return n;
}

int32_t chrome_wt_open_stream(uint64_t id, bool bidi, uint64_t* stream_id) {
  scoped_refptr<chromenet::WtConn> conn = chromenet::FindConn(id);
  *stream_id = 0;
  return conn ? conn->OpenStream(bidi, stream_id) : net::ERR_CONNECTION_CLOSED;
}

int32_t chrome_wt_stream_write(uint64_t id,
                               uint64_t stream_id,
                               const void* data,
                               size_t data_len,
                               bool fin) {
  scoped_refptr<chromenet::WtConn> conn = chromenet::FindConn(id);
  return conn ? conn->WriteStream(stream_id, static_cast<const char*>(data), data_len, fin)
              : net::ERR_CONNECTION_CLOSED;
}

int32_t chrome_wt_stream_abort(uint64_t id, uint64_t stream_id, int32_t mode, uint32_t code) {
  scoped_refptr<chromenet::WtConn> conn = chromenet::FindConn(id);
  if (!conn) {
    return net::ERR_CONNECTION_CLOSED;
  }
  int result = net::OK;
  chromenet::RunOnNetworkThread(base::BindOnce(&chromenet::WtConn::DoAbort, conn, stream_id, mode,
                                               code, base::Unretained(&result)));
  return result;
}

int32_t chrome_wt_stream_consume(uint64_t id, uint64_t stream_id, uint32_t n) {
  scoped_refptr<chromenet::WtConn> conn = chromenet::FindConn(id);
  if (!conn) {
    return net::ERR_CONNECTION_CLOSED;
  }
  chromenet::NetRunner()->PostTask(
      FROM_HERE, base::BindOnce(&chromenet::WtConn::DoConsume, conn, stream_id, n));
  return net::OK;
}

int32_t chrome_wt_send_datagram(uint64_t id, const void* data, size_t data_len) {
  scoped_refptr<chromenet::WtConn> conn = chromenet::FindConn(id);
  if (!conn) {
    return net::ERR_CONNECTION_CLOSED;
  }
  int result = net::OK;
  chromenet::RunOnNetworkThread(
      base::BindOnce(&chromenet::WtConn::DoSendDatagram, conn,
                     std::string(static_cast<const char*>(data), data_len),
                     base::Unretained(&result)));
  return result;
}

int64_t chrome_wt_max_datagram(uint64_t id) {
  scoped_refptr<chromenet::WtConn> conn = chromenet::FindConn(id);
  if (!conn) {
    return net::ERR_CONNECTION_CLOSED;
  }
  int64_t result = 0;
  chromenet::RunOnNetworkThread(base::BindOnce(&chromenet::WtConn::DoMaxDatagram, conn,
                                               base::Unretained(&result)));
  return result;
}

int32_t chrome_wt_close(uint64_t id, uint32_t code, const char* reason) {
  scoped_refptr<chromenet::WtConn> conn = chromenet::FindConn(id);
  if (!conn) {
    return net::ERR_CONNECTION_CLOSED;
  }
  chromenet::RunOnNetworkThread(
      base::BindOnce(&chromenet::WtConn::DoClose, conn, code, std::string(reason ? reason : "")));
  return net::OK;
}

}  // extern "C"

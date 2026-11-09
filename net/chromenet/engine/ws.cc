// Copyright 2026 Go Suda. MIT License.
//
// WebSocket through //net's WebSocketStream. A WsConn is created on a session thread, driven on the
// network thread, and read and written by session threads that block on it.

#ifdef UNSAFE_BUFFERS_BUILD
#pragma allow_unsafe_buffers
#endif

#include <string.h>

#include <algorithm>
#include <atomic>
#include <deque>
#include <map>
#include <memory>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include "base/containers/span.h"
#include "base/functional/bind.h"
#include "base/memory/scoped_refptr.h"
#include "base/no_destructor.h"
#include "base/strings/string_split.h"
#include "base/strings/string_util.h"
#include "base/synchronization/condition_variable.h"
#include "base/synchronization/lock.h"
#include "base/synchronization/waitable_event.h"
#include "net/base/isolation_info.h"
#include "net/base/net_errors.h"
#include "net/base/network_handle.h"
#include "net/http/http_request_headers.h"
#include "net/http/http_response_headers.h"
#include "net/http/http_util.h"
#include "net/storage_access_api/status.h"
#include "net/traffic_annotation/network_traffic_annotation.h"
#include "net/url_request/url_request_context.h"
#include "net/websockets/websocket_frame.h"
#include "net/websockets/websocket_handshake_response_info.h"
#include "net/websockets/websocket_stream.h"
#include "third_party/chromenet/chrome_net.h"
#include "third_party/chromenet/internal.h"
#include "url/gurl.h"
#include "url/origin.h"
#include "url/url_constants.h"

namespace chromenet {

namespace {

constexpr net::NetworkTrafficAnnotationTag kWsAnnotation =
    net::DefineNetworkTrafficAnnotation("chromenet_websocket", R"(
      semantics {
        sender: "chromenet"
        description: "A WebSocket connection opened by a Go program through gosuda.org/chromenet."
        trigger: "The Go program dials a WebSocket."
        data: "Whatever the Go program sends."
        destination: OTHER
      }
      policy {
        cookies_allowed: NO
        setting: "Not applicable: the engine is a library."
        policy_exception_justification: "Not applicable: the engine is a library."
      })");

// Frames that arrived and were not read yet may hold at most this many bytes before the network
// stack stops reading from the socket.
constexpr size_t kReadWindow = 4u << 20;

struct QueuedFrame {
  int opcode = 0;
  bool final = false;
  std::string data;
  size_t offset = 0;  // bytes already handed to the reader
};

class WsConn : public base::RefCountedThreadSafe<WsConn> {
 public:
  WsConn(scoped_refptr<SharedContext> shared, uint64_t id)
      : shared_(std::move(shared)), id_(id), cv_(&lock_) {}
  WsConn(const WsConn&) = delete;
  WsConn& operator=(const WsConn&) = delete;

  uint64_t id() const { return id_; }

  // Network thread.
  void Start(const GURL& url, net::HttpRequestHeaders headers);
  void Shutdown();
  void DoWrite(int opcode, bool fin, std::string data);
  void ResumeRead();

  // Any thread: the connection attempt or the connection ends with `error`.
  void Fail(int error, int response_code);

  // Session threads.
  int WaitHandshake(std::string* head);
  size_t Read(char* buf, size_t cap, int* opcode, bool* fin, int* net_error);
  int Write(int opcode, bool fin, const char* data, size_t len);

 private:
  friend class base::RefCountedThreadSafe<WsConn>;
  class Delegate;
  ~WsConn() = default;

  void OnSuccess(std::unique_ptr<net::WebSocketStream> stream,
                 std::unique_ptr<net::WebSocketHandshakeResponseInfo> response);
  void ReadMore();
  void OnReadDone(int result);
  void HandleRead(int result);
  void OnWriteDone(int result);

  const scoped_refptr<SharedContext> shared_;
  const uint64_t id_;

  // Network thread only.
  std::unique_ptr<net::WebSocketStreamRequest> request_;
  std::unique_ptr<net::WebSocketStream> stream_;
  std::vector<std::unique_ptr<net::WebSocketFrame>> read_frames_;
  std::vector<std::unique_ptr<net::WebSocketFrame>> write_frames_;
  std::string write_data_;
  bool reading_ = false;

  base::Lock lock_;
  base::ConditionVariable cv_;
  bool handshake_done_ = false;
  std::string head_;
  int handshake_error_ = 0;
  bool closed_ = false;
  int read_error_ = 0;
  bool read_paused_ = false;
  size_t queued_bytes_ = 0;
  std::deque<QueuedFrame> queue_;
  bool write_done_ = true;
  int write_result_ = 0;

  base::Lock write_mu_;  // one writer at a time
};

class WsConn::Delegate : public net::WebSocketStream::ConnectDelegate {
 public:
  explicit Delegate(scoped_refptr<WsConn> conn) : conn_(std::move(conn)) {}

  void OnCreateRequest(net::URLRequest* url_request) override {}
  int OnURLRequestConnected(net::URLRequest* request,
                            const net::TransportInfo& info,
                            net::CompletionOnceCallback callback) override {
    return net::OK;
  }
  void OnSuccess(std::unique_ptr<net::WebSocketStream> stream,
                 std::unique_ptr<net::WebSocketHandshakeResponseInfo> response) override {
    conn_->OnSuccess(std::move(stream), std::move(response));
  }
  void OnFailure(const std::string& message,
                 int net_error,
                 std::optional<int> response_code) override {
    conn_->Fail(net_error != net::OK ? net_error : net::ERR_FAILED, response_code.value_or(0));
  }
  void OnStartOpeningHandshake(
      std::unique_ptr<net::WebSocketHandshakeRequestInfo> request) override {}
  void OnSSLCertificateError(
      std::unique_ptr<net::WebSocketEventInterface::SSLErrorCallbacks> ssl_error_callbacks,
      int net_error,
      const net::SSLInfo& ssl_info,
      bool fatal) override {
    ssl_error_callbacks->CancelSSLRequest(net_error, &ssl_info);
  }
  int OnAuthRequired(const net::AuthChallengeInfo& auth_info,
                     scoped_refptr<net::HttpResponseHeaders> response_headers,
                     const net::IPEndPoint& remote_endpoint,
                     base::OnceCallback<void(const net::AuthCredentials*)> callback,
                     std::optional<net::AuthCredentials>* credentials) override {
    // The Go caller owns credentials: surface the 401/407 instead of prompting.
    return net::OK;
  }

 private:
  const scoped_refptr<WsConn> conn_;
};

void WsConn::Start(const GURL& url, net::HttpRequestHeaders headers) {
  url::Origin origin = TakeOrigin(url, &headers);
  std::vector<std::string> subprotocols;
  if (std::optional<std::string> protocols = headers.GetHeader("Sec-WebSocket-Protocol")) {
    for (std::string_view p :
         base::SplitStringPiece(*protocols, ",", base::TRIM_WHITESPACE, base::SPLIT_WANT_NONEMPTY)) {
      subprotocols.emplace_back(p);
    }
    headers.RemoveHeader("Sec-WebSocket-Protocol");
  }
  net::URLRequestContext* context = shared_->context();
  if (!context) {
    Fail(net::ERR_FAILED, 0);
    return;
  }
  request_ = net::WebSocketStream::CreateAndConnectStream(
      url, subprotocols, origin, net::StorageAccessApiStatus::kNone,
      net::IsolationInfo::CreateForInternalRequest(origin), headers, context,
      net::NetLogWithSource(), net::WebSocketPriorityHint::kDefault, kWsAnnotation,
      std::make_unique<Delegate>(base::WrapRefCounted(this)), net::handles::kInvalidNetworkHandle);
}

void WsConn::OnSuccess(std::unique_ptr<net::WebSocketStream> stream,
                       std::unique_ptr<net::WebSocketHandshakeResponseInfo> response) {
  std::string head = "101 " + stream->GetSubProtocol() + "\n";
  if (response && response->headers) {
    size_t iter = 0;
    std::string name;
    std::string value;
    while (response->headers->EnumerateHeaderLines(&iter, &name, &value)) {
      head += name + ": " + value + "\n";
    }
  }
  {
    base::AutoLock hold(lock_);
    if (closed_ || handshake_done_) {
      return;
    }
    handshake_done_ = true;
    head_ = std::move(head);
    cv_.Broadcast();
  }
  stream_ = std::move(stream);
  ReadMore();
}

void WsConn::Fail(int error, int response_code) {
  base::AutoLock hold(lock_);
  if (!handshake_done_) {
    handshake_done_ = true;
    handshake_error_ = error;
    head_ = std::to_string(response_code) + "\n";
  } else if (read_error_ == 0) {
    read_error_ = error;
  }
  cv_.Broadcast();
}

void WsConn::Shutdown() {
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
  if (stream_) {
    stream_->Close();
  }
  stream_.reset();
  request_.reset();
}

void WsConn::ReadMore() {
  while (stream_ && !reading_) {
    {
      base::AutoLock hold(lock_);
      if (closed_ || read_error_ != 0) {
        return;
      }
      if (queued_bytes_ >= kReadWindow) {
        read_paused_ = true;
        return;
      }
      read_paused_ = false;
    }
    read_frames_.clear();
    int rv = stream_->ReadFrames(&read_frames_, base::BindOnce(&WsConn::OnReadDone, this));
    if (rv == net::ERR_IO_PENDING) {
      reading_ = true;
      return;
    }
    HandleRead(rv);
  }
}

void WsConn::OnReadDone(int result) {
  reading_ = false;
  HandleRead(result);
  ReadMore();
}

void WsConn::HandleRead(int result) {
  base::AutoLock hold(lock_);
  if (result != net::OK) {
    read_error_ = result;
  } else {
    for (const auto& frame : read_frames_) {
      // No extension that uses reserved bits is negotiated past the stream (permessage-deflate is
      // inflated inside it and arrives with RSV1 cleared), so any set bit is a protocol error.
      if (frame->header.reserved1 || frame->header.reserved2 || frame->header.reserved3) {
        read_error_ = net::ERR_WS_PROTOCOL_ERROR;
        break;
      }
      QueuedFrame q;
      q.opcode = frame->header.opcode;
      q.final = frame->header.final;
      q.data.assign(reinterpret_cast<const char*>(frame->payload.data()), frame->payload.size());
      queued_bytes_ += q.data.size();
      queue_.push_back(std::move(q));
    }
  }
  read_frames_.clear();
  cv_.Broadcast();
}

void WsConn::ResumeRead() {
  if (!reading_) {
    ReadMore();
  }
}

size_t WsConn::Read(char* buf, size_t cap, int* opcode, bool* fin, int* net_error) {
  bool resume = false;
  size_t n = 0;
  {
    base::AutoLock hold(lock_);
    while (queue_.empty() && read_error_ == 0 && !closed_) {
      cv_.Wait();
    }
    if (queue_.empty()) {
      *opcode = 0;
      *fin = false;
      *net_error = read_error_ != 0 ? read_error_ : net::ERR_CONNECTION_CLOSED;
      return 0;
    }
    QueuedFrame& front = queue_.front();
    n = std::min(cap, front.data.size() - front.offset);
    memcpy(buf, front.data.data() + front.offset, n);
    *opcode = front.offset == 0 ? front.opcode : 0;
    front.offset += n;
    const bool last = front.offset == front.data.size();
    *fin = last && front.final;
    *net_error = net::OK;
    queued_bytes_ -= n;
    if (last) {
      queue_.pop_front();
    }
    resume = read_paused_ && queued_bytes_ < kReadWindow;
  }
  if (resume) {
    NetRunner()->PostTask(FROM_HERE, base::BindOnce(&WsConn::ResumeRead, this));
  }
  return n;
}

int WsConn::WaitHandshake(std::string* head) {
  base::AutoLock hold(lock_);
  while (!handshake_done_) {
    cv_.Wait();
  }
  *head = head_;
  return handshake_error_;
}

int WsConn::Write(int opcode, bool fin, const char* data, size_t len) {
  base::AutoLock serialize(write_mu_);
  {
    base::AutoLock hold(lock_);
    if (closed_ || !handshake_done_ || handshake_error_ != 0) {
      return net::ERR_CONNECTION_CLOSED;
    }
    write_done_ = false;
    write_result_ = net::OK;
  }
  NetRunner()->PostTask(FROM_HERE, base::BindOnce(&WsConn::DoWrite, this, opcode, fin,
                                                  std::string(data, len)));
  base::AutoLock hold(lock_);
  while (!write_done_ && !closed_) {
    cv_.Wait();
  }
  return write_done_ ? write_result_ : net::ERR_CONNECTION_CLOSED;
}

void WsConn::DoWrite(int opcode, bool fin, std::string data) {
  if (!stream_) {
    OnWriteDone(net::ERR_CONNECTION_CLOSED);
    return;
  }
  write_data_ = std::move(data);
  auto frame = std::make_unique<net::WebSocketFrame>(opcode);
  frame->header.final = fin;
  frame->header.payload_length = write_data_.size();
  frame->payload = base::as_byte_span(write_data_);
  write_frames_.clear();
  write_frames_.push_back(std::move(frame));
  int rv = stream_->WriteFrames(&write_frames_, base::BindOnce(&WsConn::OnWriteDone, this));
  if (rv != net::ERR_IO_PENDING) {
    OnWriteDone(rv);
  }
}

void WsConn::OnWriteDone(int result) {
  write_frames_.clear();
  write_data_.clear();
  base::AutoLock hold(lock_);
  write_done_ = true;
  write_result_ = result;
  cv_.Broadcast();
}

struct Registry {
  base::Lock lock;
  std::map<uint64_t, scoped_refptr<WsConn>> conns;
  uint64_t next_id = 1;
};

Registry& GetWsRegistry() {
  static base::NoDestructor<Registry> registry;
  return *registry;
}

scoped_refptr<WsConn> FindConn(uint64_t id) {
  Registry& r = GetWsRegistry();
  base::AutoLock hold(r.lock);
  auto it = r.conns.find(id);
  return it == r.conns.end() ? nullptr : it->second;
}

}  // namespace

bool ParseConnectHead(const char* data,
                      size_t len,
                      GURL* url,
                      net::HttpRequestHeaders* headers) {
  std::vector<std::string_view> lines = base::SplitStringPiece(
      std::string_view(data, len), "\n", base::KEEP_WHITESPACE, base::SPLIT_WANT_NONEMPTY);
  if (lines.empty()) {
    return false;
  }
  auto strip_cr = [](std::string_view s) {
    if (!s.empty() && s.back() == '\r') {
      s.remove_suffix(1);
    }
    return s;
  };
  GURL parsed(strip_cr(lines[0]));
  if (!parsed.is_valid() || !(parsed.SchemeIsHTTPOrHTTPS() || parsed.SchemeIsWSOrWSS())) {
    return false;
  }
  *url = std::move(parsed);
  for (size_t i = 1; i < lines.size(); ++i) {
    std::string_view line = strip_cr(lines[i]);
    size_t colon = line.find(':');
    if (colon == std::string_view::npos) {
      return false;
    }
    std::string_view name = line.substr(0, colon);
    std::string_view value = net::HttpUtil::TrimLWS(line.substr(colon + 1));
    if (!net::HttpUtil::IsValidHeaderName(name) || !net::HttpUtil::IsValidHeaderValue(value)) {
      return false;
    }
    headers->SetHeader(name, value);
  }
  return true;
}

url::Origin TakeOrigin(const GURL& url, net::HttpRequestHeaders* headers) {
  if (std::optional<std::string> origin = headers->GetHeader("Origin")) {
    headers->RemoveHeader("Origin");
    url::Origin parsed = url::Origin::Create(GURL(*origin));
    if (!parsed.opaque()) {
      return parsed;
    }
  }
  GURL::Replacements scheme;
  if (url.SchemeIs(url::kWsScheme)) {
    scheme.SetSchemeStr(url::kHttpScheme);
  } else if (url.SchemeIs(url::kWssScheme)) {
    scheme.SetSchemeStr(url::kHttpsScheme);
  }
  return url::Origin::Create(url.ReplaceComponents(scheme));
}

void RunOnNetworkThread(base::OnceClosure task) {
  base::WaitableEvent done;
  NetRunner()->PostTask(
      FROM_HERE, base::BindOnce(
                     [](base::OnceClosure task, base::WaitableEvent* done) {
                       std::move(task).Run();
                       done->Signal();
                     },
                     std::move(task), &done));
  done.Wait();
}

}  // namespace chromenet

struct chrome_ws {
  scoped_refptr<chromenet::WsConn> conn;
};

extern "C" {

chrome_ws_t* chrome_ws_start(chrome_engine_t* e, const void* head, size_t head_len) {
  chromenet::Registry& registry = chromenet::GetWsRegistry();
  scoped_refptr<chromenet::WsConn> conn;
  {
    base::AutoLock hold(registry.lock);
    uint64_t id = registry.next_id++;
    conn = base::MakeRefCounted<chromenet::WsConn>(e->shared, id);
    registry.conns[id] = conn;
  }
  GURL url;
  net::HttpRequestHeaders headers;
  if (!chromenet::ParseConnectHead(static_cast<const char*>(head), head_len, &url, &headers)) {
    conn->Fail(net::ERR_INVALID_ARGUMENT, 0);
  } else {
    // A ws:// URL is the same request as an http:// one for the network stack.
    chromenet::NetRunner()->PostTask(
        FROM_HERE, base::BindOnce(&chromenet::WsConn::Start, conn, std::move(url),
                                  std::move(headers)));
  }
  return new chrome_ws{conn};
}

int32_t chrome_ws_wait(chrome_ws_t* w, void* head, size_t head_cap, size_t* head_len, uint64_t* id) {
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

void chrome_ws_cancel(chrome_ws_t* w) {
  w->conn->Fail(net::ERR_ABORTED, 0);
  chromenet::NetRunner()->PostTask(FROM_HERE,
                                   base::BindOnce(&chromenet::WsConn::Shutdown, w->conn));
}

int32_t chrome_ws_destroy(chrome_ws_t* w) {
  chromenet::Registry& registry = chromenet::GetWsRegistry();
  {
    base::AutoLock hold(registry.lock);
    registry.conns.erase(w->conn->id());
  }
  // Waits for the shutdown, so no network-thread task touches the connection after this returns.
  chromenet::RunOnNetworkThread(base::BindOnce(&chromenet::WsConn::Shutdown, w->conn));
  delete w;
  return CHROME_NET_OK;
}

size_t chrome_ws_read(uint64_t id,
                      void* buf,
                      size_t buf_cap,
                      int32_t* opcode,
                      bool* fin,
                      int32_t* net_error) {
  scoped_refptr<chromenet::WsConn> conn = chromenet::FindConn(id);
  if (!conn) {
    *opcode = 0;
    *fin = false;
    *net_error = net::ERR_CONNECTION_CLOSED;
    return 0;
  }
  int op = 0;
  size_t n = conn->Read(static_cast<char*>(buf), buf_cap, &op, fin, net_error);
  *opcode = op;
  return n;
}

int32_t chrome_ws_write(uint64_t id, int32_t opcode, bool fin, const void* data, size_t data_len) {
  scoped_refptr<chromenet::WsConn> conn = chromenet::FindConn(id);
  if (!conn) {
    return net::ERR_CONNECTION_CLOSED;
  }
  return conn->Write(opcode, fin, static_cast<const char*>(data), data_len);
}

}  // extern "C"

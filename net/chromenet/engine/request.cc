// Copyright 2026 Go Suda. MIT License.

#ifdef UNSAFE_BUFFERS_BUILD
#pragma allow_unsafe_buffers
#endif

#include <string.h>

#include <algorithm>
#include <string_view>
#include <utility>

#include "base/check.h"
#include "base/strings/string_number_conversions.h"
#include "base/strings/string_split.h"
#include "base/strings/strcat.h"
#include "net/base/io_buffer.h"
#include "net/base/load_flags.h"
#include "net/base/net_errors.h"
#include "net/base/network_handle.h"
#include "net/base/upload_data_stream.h"
#include "net/cert/x509_certificate.h"
#include "net/http/http_connection_info.h"
#include "net/http/http_response_headers.h"
#include "net/http/http_response_info.h"
#include "net/http/http_util.h"
#include "net/ssl/ssl_private_key.h"
#include "net/traffic_annotation/network_traffic_annotation.h"
#include "net/url_request/redirect_info.h"
#include "net/url_request/url_request_context.h"
#include "third_party/chromenet/internal.h"

namespace chromenet {

namespace {

constexpr net::NetworkTrafficAnnotationTag kTrafficAnnotation =
    net::DefineNetworkTrafficAnnotation("chromenet_request", R"(
      semantics {
        sender: "chromenet"
        description: "An HTTP request issued by a Go program through gosuda.org/chromenet."
        trigger: "The Go program sends a request."
        data: "Whatever the Go program sends."
        destination: OTHER
      }
      policy {
        cookies_allowed: NO
        setting: "Not applicable: the engine is a library."
        policy_exception_justification: "Not applicable: the engine is a library."
      })");

}  // namespace

// RequestCore -------------------------------------------------------------------------------

RequestCore::RequestCore() : cv_(&lock_) {}
RequestCore::~RequestCore() = default;

void RequestCore::SetHead(std::string head) {
  base::AutoLock hold(lock_);
  if (finished_ || head_ready_) {
    return;
  }
  head_ready_ = true;
  head_ = std::move(head);
  cv_.Broadcast();
}

void RequestCore::Finish(int error) {
  base::AutoLock hold(lock_);
  if (finished_) {
    return;
  }
  finished_ = true;
  error_ = error;
  cv_.Broadcast();
}

void RequestCore::CompleteRead(const char* data, int result) {
  base::AutoLock hold(lock_);
  read_pending_ = false;
  read_result_ = result;
  read_data_.clear();
  if (result > 0) {
    read_data_.assign(data, data + result);
  } else if (!finished_) {
    finished_ = true;
    error_ = result;  // net::OK (0) at the end of the body
  }
  cv_.Broadcast();
}

void RequestCore::CompleteWrite(int result) {
  base::AutoLock hold(lock_);
  write_pending_ = false;
  write_result_ = result;
  cv_.Broadcast();
}

int RequestCore::WaitHead(std::string* head) {
  base::AutoLock hold(lock_);
  while (!head_ready_ && !finished_) {
    cv_.Wait();
  }
  if (head_ready_) {
    *head = head_;
    return net::OK;
  }
  return error_ != net::OK ? error_ : net::ERR_EMPTY_RESPONSE;
}

void RequestCore::BeginRead() {
  base::AutoLock hold(lock_);
  read_data_.clear();
  read_result_ = 0;
  read_pending_ = !finished_;
}

size_t RequestCore::WaitRead(char* buf, size_t cap, bool* eof, int* net_error) {
  base::AutoLock hold(lock_);
  while (read_pending_ && !finished_) {
    cv_.Wait();
  }
  read_pending_ = false;
  if (read_result_ > 0) {
    size_t n = std::min(cap, read_data_.size());
    memcpy(buf, read_data_.data(), n);
    read_data_.clear();
    read_result_ = 0;
    *eof = false;
    *net_error = net::OK;
    return n;
  }
  *eof = true;
  *net_error = read_result_ < 0 ? read_result_ : error_;
  return 0;
}

void RequestCore::BeginWrite() {
  base::AutoLock hold(lock_);
  write_result_ = net::OK;
  write_pending_ = !finished_;
}

int RequestCore::WaitWrite() {
  base::AutoLock hold(lock_);
  while (write_pending_ && !finished_) {
    cv_.Wait();
  }
  if (write_pending_) {
    // The request ended before the network stack consumed the chunk. A clean end (the server
    // answered without reading the whole body) drops the rest of the body.
    write_pending_ = false;
    return error_;
  }
  return write_result_;
}

// ParseRequestHead --------------------------------------------------------------------------

bool ParseRequestHead(const char* data, size_t len, RequestParams* out) {
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
  std::string_view first = strip_cr(lines[0]);
  size_t sp = first.find(' ');
  if (sp == std::string_view::npos) {
    return false;
  }
  std::string_view method = first.substr(0, sp);
  if (!net::HttpUtil::IsToken(method)) {
    return false;
  }
  GURL url(first.substr(sp + 1));
  if (!url.is_valid() || !url.SchemeIsHTTPOrHTTPS()) {
    return false;
  }
  out->method = std::string(method);
  out->url = std::move(url);
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
    out->headers.SetHeader(name, value);
  }
  return true;
}

// PipeUpload --------------------------------------------------------------------------------

// An upload body that the session thread feeds one chunk at a time. A chunk is acknowledged
// (RequestCore::CompleteWrite) once the network stack consumed all of it, so a session never has
// more than one chunk in flight. The stream cannot rewind.
class PipeUpload : public net::UploadDataStream {
 public:
  PipeUpload(int64_t body_len, scoped_refptr<RequestCore> core)
      : net::UploadDataStream(/*is_chunked=*/body_len < 0, /*identifier=*/0),
        size_(body_len < 0 ? 0 : static_cast<uint64_t>(body_len)),
        core_(std::move(core)) {}
  PipeUpload(const PipeUpload&) = delete;
  PipeUpload& operator=(const PipeUpload&) = delete;
  ~PipeUpload() override = default;

  void Append(std::string data, bool eof) {
    chunk_ = std::move(data);
    offset_ = 0;
    if (chunk_.empty()) {
      if (eof) {
        MarkFinal();
        if (pending_buf_) {
          pending_buf_ = nullptr;
          OnReadCompleted(0);
        }
      }
      core_->CompleteWrite(net::OK);
      return;
    }
    eof_ = eof;
    outstanding_ = true;
    if (pending_buf_) {
      int n = Fill(pending_buf_.get(), pending_len_);
      pending_buf_ = nullptr;
      OnReadCompleted(n);
    }
  }

 private:
  int InitInternal(const net::NetLogWithSource& net_log) override {
    if (started_) {
      return net::ERR_UPLOAD_STREAM_REWIND_NOT_SUPPORTED;
    }
    if (!is_chunked()) {
      SetSize(size_);
    }
    return net::OK;
  }

  int ReadInternal(net::IOBuffer* buf, int buf_len) override {
    started_ = true;
    if (outstanding_) {
      return Fill(buf, buf_len);
    }
    if (final_) {
      return 0;
    }
    pending_buf_ = buf;
    pending_len_ = buf_len;
    return net::ERR_IO_PENDING;
  }

  void ResetInternal() override {
    pending_buf_ = nullptr;
    pending_len_ = 0;
  }

  int Fill(net::IOBuffer* buf, int buf_len) {
    size_t n = std::min(chunk_.size() - offset_, static_cast<size_t>(buf_len));
    memcpy(buf->data(), chunk_.data() + offset_, n);
    offset_ += n;
    if (offset_ == chunk_.size()) {
      outstanding_ = false;
      chunk_.clear();
      offset_ = 0;
      if (eof_) {
        MarkFinal();
      }
      core_->CompleteWrite(net::OK);
    }
    return static_cast<int>(n);
  }

  void MarkFinal() {
    if (final_) {
      return;
    }
    final_ = true;
    if (is_chunked()) {
      SetIsFinalChunk();
    }
  }

  const uint64_t size_;
  const scoped_refptr<RequestCore> core_;
  std::string chunk_;
  size_t offset_ = 0;
  bool outstanding_ = false;
  bool eof_ = false;
  bool final_ = false;
  bool started_ = false;
  scoped_refptr<net::IOBuffer> pending_buf_;
  int pending_len_ = 0;
};

// Job ---------------------------------------------------------------------------------------

Job::Job(scoped_refptr<SharedContext> shared, scoped_refptr<RequestCore> core)
    : shared_(std::move(shared)), core_(std::move(core)) {}

Job::~Job() {
  upload_ = nullptr;
  request_.reset();
  core_->Finish(net::ERR_ABORTED);
}

void Job::Start(RequestParams params) {
  net::URLRequestContext* context = shared_->context();
  if (!context) {
    Fail(net::ERR_FAILED);
    return;
  }
  request_ = context->CreateRequest(params.url, net::DEFAULT_PRIORITY, this, kTrafficAnnotation,
                                    net::handles::kInvalidNetworkHandle);
  request_->set_method(params.method);
  request_->SetExtraRequestHeaders(params.headers);
  request_->SetLoadFlags(net::LOAD_DISABLE_CACHE | net::LOAD_DO_NOT_SAVE_COOKIES);
  if (params.body_len != 0) {
    auto upload = std::make_unique<PipeUpload>(params.body_len, core_);
    upload_ = upload.get();
    request_->set_upload(std::move(upload));
  }
  request_->Start();
}

void Job::Read(size_t cap) {
  if (done_ || !request_) {
    core_->CompleteRead(nullptr, 0);
    return;
  }
  if (redirected_) {
    // The redirect stays deferred; the Go client follows it with a new request.
    done_ = true;
    core_->CompleteRead(nullptr, 0);
    return;
  }
  read_buf_ = base::MakeRefCounted<net::IOBufferWithSize>(cap);
  int rv = request_->Read(read_buf_.get(), static_cast<int>(cap));
  if (rv != net::ERR_IO_PENDING) {
    ReadCompleted(rv);
  }
}

void Job::Write(std::string data, bool eof) {
  if (done_ || !upload_) {
    core_->CompleteWrite(upload_ ? net::OK : net::ERR_UNEXPECTED);
    return;
  }
  upload_->Append(std::move(data), eof);
}

void Job::Cancel() {
  if (!done_) {
    done_ = true;
    if (request_) {
      request_->Cancel();
    }
  }
  core_->Finish(net::ERR_ABORTED);
}

void Job::OnReceivedRedirect(net::URLRequest* request,
                             const net::RedirectInfo& redirect_info,
                             bool* defer_redirect) {
  *defer_redirect = true;
  redirected_ = true;
  core_->SetHead(BuildHead(/*redirect=*/true));
}

void Job::OnAuthRequired(net::URLRequest* request, const net::AuthChallengeInfo& auth_info) {
  // The Go caller owns credentials: deliver the 401/407 response instead of prompting.
  request->CancelAuth();
}

void Job::OnCertificateRequested(net::URLRequest* request,
                                 net::SSLCertRequestInfo* cert_request_info) {
  request->ContinueWithCertificate(nullptr, nullptr);
}

void Job::OnSSLCertificateError(net::URLRequest* request,
                                int net_error,
                                const net::SSLInfo& ssl_info,
                                bool fatal) {
  request->CancelWithSSLError(net_error, ssl_info);
}

void Job::OnResponseStarted(net::URLRequest* request, int net_error) {
  if (net_error != net::OK) {
    Fail(net_error);
    return;
  }
  core_->SetHead(BuildHead(/*redirect=*/false));
}

void Job::OnReadCompleted(net::URLRequest* request, int bytes_read) {
  ReadCompleted(bytes_read);
}

void Job::ReadCompleted(int result) {
  core_->CompleteRead(read_buf_ ? read_buf_->data() : nullptr, result);
  read_buf_ = nullptr;
  if (result <= 0) {
    done_ = true;
  }
}

void Job::Fail(int error) {
  done_ = true;
  core_->Finish(error);
}

std::string Job::BuildHead(bool redirect) const {
  const net::HttpResponseHeaders* headers = request_->response_headers();
  const net::HttpResponseInfo& info = request_->response_info();
  std::string proto = info.alpn_negotiated_protocol;
  if (proto.empty() || proto == "unknown") {
    switch (net::HttpConnectionInfoToCoarse(info.connection_info)) {
      case net::HttpConnectionInfoCoarse::kHTTP2:
        proto = "h2";
        break;
      case net::HttpConnectionInfoCoarse::kQUIC:
        proto = "h3";
        break;
      default:
        proto = "http/1.1";
        break;
    }
  }
  std::string head = base::StrCat(
      {base::NumberToString(headers ? headers->response_code() : 0), " ", proto, " ",
       redirect ? "1" : "0", "\n", headers ? headers->GetStatusText() : std::string(), "\n"});
  if (headers) {
    size_t iter = 0;
    std::string name;
    std::string value;
    while (headers->EnumerateHeaderLines(&iter, &name, &value)) {
      base::StrAppend(&head, {name, ": ", value, "\n"});
    }
  }
  return head;
}

}  // namespace chromenet

// Copyright 2026 Go Suda. MIT License.
//
// Internal types of the chromenet engine. Threads:
//   - HQQ session threads call the C API in chrome_net.h and block on RequestCore.
//   - One network thread (IO message pump) owns every //net object: URLRequestContexts, Jobs,
//     URLRequests and upload streams. Session threads reach it only by posting tasks.

#ifndef THIRD_PARTY_CHROMENET_INTERNAL_H_
#define THIRD_PARTY_CHROMENET_INTERNAL_H_

#include <stddef.h>
#include <stdint.h>

#include <memory>
#include <string>
#include <vector>

#include "base/functional/callback.h"
#include "base/memory/ref_counted.h"
#include "base/memory/ref_counted_delete_on_sequence.h"
#include "base/memory/scoped_refptr.h"
#include "base/synchronization/condition_variable.h"
#include "base/synchronization/lock.h"
#include "base/task/single_thread_task_runner.h"
#include "net/http/http_request_headers.h"
#include "net/url_request/url_request.h"
#include "url/gurl.h"
#include "url/origin.h"

namespace net {
class IOBuffer;
class URLRequestContext;
}  // namespace net

namespace chromenet {

// The task runner of the network thread. Valid after chrome_net_init().
scoped_refptr<base::SingleThreadTaskRunner> NetRunner();

struct EngineConfig {
  std::string user_agent;
  std::string accept_language;
  std::string proxy;  // net::ProxyConfig::ProxyRules syntax; empty = direct
  bool quic = true;
  bool http2 = true;
  bool force_quic = false;
  bool insecure_skip_verify = false;
};

// Parses "key=value" lines. Returns false on an unknown key or a malformed value.
bool ParseEngineConfig(const char* data, size_t len, EngineConfig* out);

// One URLRequestContext, shared by every engine handle created with the same options. Created on
// any thread; the context itself is built and destroyed on the network thread.
class SharedContext : public base::RefCountedDeleteOnSequence<SharedContext> {
 public:
  explicit SharedContext(EngineConfig config);
  SharedContext(const SharedContext&) = delete;
  SharedContext& operator=(const SharedContext&) = delete;

  // Network thread only. Builds the context on first use.
  net::URLRequestContext* context();

 private:
  friend class base::RefCountedDeleteOnSequence<SharedContext>;
  friend class base::DeleteHelper<SharedContext>;
  ~SharedContext();

  const EngineConfig config_;
  std::unique_ptr<net::URLRequestContext> context_;
};

// Blocking hand-off between a session thread and the network thread. Every member is guarded by
// lock_.
class RequestCore : public base::RefCountedThreadSafe<RequestCore> {
 public:
  RequestCore();
  RequestCore(const RequestCore&) = delete;
  RequestCore& operator=(const RequestCore&) = delete;

  // Network thread: the response head is available.
  void SetHead(std::string head);
  // Any thread: the request ended with `error` (net::OK at a clean end of the body). The first
  // call wins; it wakes every waiter.
  void Finish(int error);
  // Network thread: the outstanding read completed with `result` bytes from `data` (0 = end of
  // body, < 0 = error).
  void CompleteRead(const char* data, int result);
  // Network thread: the outstanding write was consumed (net::OK) or failed.
  void CompleteWrite(int result);

  // Session thread. Returns net::OK with the head, or the error that ended the request.
  int WaitHead(std::string* head);
  // Session thread. BeginX marks an operation outstanding before it is posted; WaitX blocks until
  // the network thread completed it or the request ended.
  void BeginRead();
  size_t WaitRead(char* buf, size_t cap, bool* eof, int* net_error);
  void BeginWrite();
  int WaitWrite();

 private:
  friend class base::RefCountedThreadSafe<RequestCore>;
  ~RequestCore();

  base::Lock lock_;
  base::ConditionVariable cv_;
  bool head_ready_ = false;
  std::string head_;
  bool finished_ = false;
  int error_ = 0;
  bool read_pending_ = false;
  std::vector<char> read_data_;
  int read_result_ = 0;
  bool write_pending_ = false;
  int write_result_ = 0;
};

struct RequestParams {
  std::string method;
  GURL url;
  net::HttpRequestHeaders headers;
  int64_t body_len = 0;  // -1: chunked, 0: none, > 0: exact length
};

// Parses "METHOD URL\nName: value\n..." into `out`. Returns false on malformed input.
bool ParseRequestHead(const char* data, size_t len, RequestParams* out);

class PipeUpload;

// Owns one URLRequest. Constructed on a session thread; every other member function, and the
// destructor, run on the network thread in the order the session thread posted them.
class Job : public net::URLRequest::Delegate {
 public:
  Job(scoped_refptr<SharedContext> shared, scoped_refptr<RequestCore> core);
  Job(const Job&) = delete;
  Job& operator=(const Job&) = delete;
  ~Job() override;

  void Start(RequestParams params);
  void Read(size_t cap);
  void Write(std::string data, bool eof);
  void Cancel();

  // net::URLRequest::Delegate:
  void OnReceivedRedirect(net::URLRequest* request,
                          const net::RedirectInfo& redirect_info,
                          bool* defer_redirect) override;
  void OnAuthRequired(net::URLRequest* request,
                      const net::AuthChallengeInfo& auth_info) override;
  void OnCertificateRequested(net::URLRequest* request,
                              net::SSLCertRequestInfo* cert_request_info) override;
  void OnSSLCertificateError(net::URLRequest* request,
                             int net_error,
                             const net::SSLInfo& ssl_info,
                             bool fatal) override;
  void OnResponseStarted(net::URLRequest* request, int net_error) override;
  void OnReadCompleted(net::URLRequest* request, int bytes_read) override;

 private:
  std::string BuildHead(bool redirect) const;
  void ReadCompleted(int result);
  void Fail(int error);

  scoped_refptr<SharedContext> shared_;
  scoped_refptr<RequestCore> core_;
  std::unique_ptr<net::URLRequest> request_;
  PipeUpload* upload_ = nullptr;  // owned by request_
  scoped_refptr<net::IOBuffer> read_buf_;
  bool redirected_ = false;
  bool done_ = false;
};

// "URL\nName: value\n..." as sent by the Go client for WebSocket and WebTransport. Returns false on
// a malformed head.
bool ParseConnectHead(const char* data,
                      size_t len,
                      GURL* url,
                      net::HttpRequestHeaders* headers);

// The origin a browser would send for `url`: the Origin header when `headers` has one (removed from
// `headers`), else the http(s) origin of the same host.
url::Origin TakeOrigin(const GURL& url, net::HttpRequestHeaders* headers);

// Runs `task` on the network thread and blocks the calling (session) thread until it returned.
void RunOnNetworkThread(base::OnceClosure task);

}  // namespace chromenet

// One engine handle: the shared URLRequestContext of its options.
struct chrome_engine {
  std::string key;
  scoped_refptr<chromenet::SharedContext> shared;
};

#endif  // THIRD_PARTY_CHROMENET_INTERNAL_H_

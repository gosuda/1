// Copyright 2026 Go Suda. MIT License.

#ifdef UNSAFE_BUFFERS_BUILD
#pragma allow_unsafe_buffers
#endif

#include <signal.h>
#include <string.h>

#include "build/build_config.h"
#if BUILDFLAG(IS_POSIX)
#include <pthread.h>
#endif

#include <map>
#include <string>
#include <string_view>
#include <utility>

#include "base/at_exit.h"
#include "base/command_line.h"
#include "base/feature_list.h"
#include "base/functional/bind.h"
#include "base/functional/callback.h"
#include "base/i18n/icu_util.h"
#include "base/logging.h"
#include "base/no_destructor.h"
#include "base/strings/string_split.h"
#include "base/strings/string_util.h"
#include "base/synchronization/lock.h"
#include "base/synchronization/waitable_event.h"
#include "base/task/thread_pool/thread_pool_instance.h"
#include "base/threading/thread.h"
#include "build/build_config.h"
#include "net/base/net_errors.h"
#include "net/cert/cert_verifier.h"
#include "net/cert/cert_verify_result.h"
#include "net/cert/x509_certificate.h"
#include "net/cookies/cookie_store.h"
#include "net/http/http_network_session.h"
#include "net/proxy_resolution/proxy_config.h"
#include "net/proxy_resolution/proxy_config_service_fixed.h"
#include "net/proxy_resolution/proxy_config_with_annotation.h"
#include "net/quic/quic_context.h"
#include "net/traffic_annotation/network_traffic_annotation.h"
#include "net/url_request/url_request_context.h"
#include "net/url_request/url_request_context_builder.h"
#include "third_party/chromenet/chrome_net.h"
#include "third_party/chromenet/internal.h"

namespace chromenet {

namespace {

constexpr net::NetworkTrafficAnnotationTag kProxyAnnotation =
    net::DefineNetworkTrafficAnnotation("chromenet_proxy_config", R"(
      semantics {
        sender: "chromenet"
        description: "Proxy configuration supplied by the Go program."
        trigger: "Engine creation."
        data: "None."
        destination: OTHER
      }
      policy {
        cookies_allowed: NO
        setting: "Not applicable: the engine is a library."
        policy_exception_justification: "Not applicable: the engine is a library."
      })");

base::Thread* g_net_thread = nullptr;

// The User-Agent of desktop Chrome with this engine's major version, in the reduced form Chrome
// sends (https://www.chromium.org/updates/ua-reduction/).
std::string DefaultUserAgent() {
#if BUILDFLAG(IS_WIN)
  constexpr std::string_view kPlatform = "Windows NT 10.0; Win64; x64";
#elif BUILDFLAG(IS_MAC)
  constexpr std::string_view kPlatform = "Macintosh; Intel Mac OS X 10_15_7";
#else
  constexpr std::string_view kPlatform = "X11; Linux x86_64";
#endif
  return std::string("Mozilla/5.0 (") + std::string(kPlatform) +
         ") AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" CHROMENET_CHROME_MAJOR
         ".0.0.0 Safari/537.36";
}

// Accepts every server certificate. Used only when the Go caller sets InsecureSkipVerify.
class AcceptAllCertVerifier : public net::CertVerifier {
 public:
  int Verify(const RequestParams& params,
             net::CertVerifyResult* verify_result,
             net::CompletionOnceCallback callback,
             std::unique_ptr<Request>* out_req,
             const net::NetLogWithSource& net_log) override {
    verify_result->Reset();
    verify_result->verified_cert = params.certificate();
    return net::OK;
  }
  void Verify2QwacBinding(
      const std::string& binding,
      const std::string& hostname,
      const scoped_refptr<net::X509Certificate>& tls_cert,
      base::OnceCallback<void(const scoped_refptr<net::X509Certificate>&)> callback,
      const net::NetLogWithSource& net_log) override {
    std::move(callback).Run(nullptr);
  }
  void SetConfig(const Config& config) override {}
  void AddObserver(Observer* observer) override {}
  void RemoveObserver(Observer* observer) override {}
};

bool ParseBool(std::string_view v, bool* out) {
  if (v == "1" || v == "true") {
    *out = true;
    return true;
  }
  if (v == "0" || v == "false") {
    *out = false;
    return true;
  }
  return false;
}

// Engines with identical options share one SharedContext, so connection pools, HTTP/2 sessions
// and QUIC sessions are reused across HQQ sessions.
struct Registry {
  base::Lock lock;
  struct Entry {
    scoped_refptr<SharedContext> shared;
    int engines = 0;
  };
  std::map<std::string, Entry> entries;
};

Registry& GetRegistry() {
  static base::NoDestructor<Registry> registry;
  return *registry;
}

}  // namespace

scoped_refptr<base::SingleThreadTaskRunner> NetRunner() {
  return g_net_thread->task_runner();
}

bool ParseEngineConfig(const char* data, size_t len, EngineConfig* out) {
  for (std::string_view line : base::SplitStringPiece(std::string_view(data, len), "\n",
                                                      base::TRIM_WHITESPACE,
                                                      base::SPLIT_WANT_NONEMPTY)) {
    size_t eq = line.find('=');
    if (eq == std::string_view::npos) {
      return false;
    }
    std::string_view key = line.substr(0, eq);
    std::string_view value = line.substr(eq + 1);
    if (key == "user_agent") {
      out->user_agent = std::string(value);
    } else if (key == "accept_language") {
      out->accept_language = std::string(value);
    } else if (key == "proxy") {
      out->proxy = std::string(value);
    } else if (key == "quic") {
      if (!ParseBool(value, &out->quic)) {
        return false;
      }
    } else if (key == "http2") {
      if (!ParseBool(value, &out->http2)) {
        return false;
      }
    } else if (key == "force_quic") {
      if (!ParseBool(value, &out->force_quic)) {
        return false;
      }
    } else if (key == "insecure_skip_verify") {
      if (!ParseBool(value, &out->insecure_skip_verify)) {
        return false;
      }
    } else {
      return false;
    }
  }
  return true;
}

SharedContext::SharedContext(EngineConfig config)
    : base::RefCountedDeleteOnSequence<SharedContext>(NetRunner()), config_(std::move(config)) {}

SharedContext::~SharedContext() = default;

net::URLRequestContext* SharedContext::context() {
  if (context_) {
    return context_.get();
  }
  net::URLRequestContextBuilder builder;
  builder.set_user_agent(config_.user_agent.empty() ? DefaultUserAgent() : config_.user_agent);
  builder.set_accept_language(config_.accept_language.empty() ? "en-US,en;q=0.9"
                                                               : config_.accept_language);
  net::ProxyConfig proxy = net::ProxyConfig::CreateDirect();
  if (!config_.proxy.empty()) {
    proxy.proxy_rules().ParseFromString(config_.proxy);
  }
  builder.set_proxy_config_service(std::make_unique<net::ProxyConfigServiceFixed>(
      net::ProxyConfigWithAnnotation(proxy, kProxyAnnotation)));
  builder.DisableHttpCache();
  builder.SetCookieStore(nullptr);  // the Go caller owns cookies (http.Client.Jar)
  builder.set_enable_brotli(true);
  builder.set_enable_zstd(true);
  net::HttpNetworkSessionParams session_params;
  session_params.enable_http2 = config_.http2;
  session_params.enable_quic = config_.quic;
  builder.set_http_network_session_params(session_params);
  auto quic = std::make_unique<net::QuicContext>();
  quic->params()->force_quic_everywhere = config_.force_quic;
  builder.set_quic_context(std::move(quic));
  if (config_.insecure_skip_verify) {
    builder.SetCertVerifier(std::make_unique<AcceptAllCertVerifier>());
  }
  context_ = builder.Build();
  return context_.get();
}

}  // namespace chromenet

// C API --------------------------------------------------------------------------------------

struct chrome_engine {
  std::string key;
  scoped_refptr<chromenet::SharedContext> shared;
};

struct chrome_request {
  scoped_refptr<chromenet::RequestCore> core;
  chromenet::Job* job;  // deleted on the network thread
};

extern "C" {

int chrome_net_init(int argc, char** argv) {
#if BUILDFLAG(IS_POSIX)
  signal(SIGPIPE, SIG_IGN);
  // hqqrpc_main takes SIGINT and SIGTERM on one thread with sigwait(), and wakes that thread at
  // shutdown with kill(getpid(), SIGTERM). Block both before Chromium starts its threads, which
  // inherit the mask, so the signal never reaches a thread with the default action.
  sigset_t shutdown_signals;
  sigemptyset(&shutdown_signals);
  sigaddset(&shutdown_signals, SIGINT);
  sigaddset(&shutdown_signals, SIGTERM);
  if (pthread_sigmask(SIG_BLOCK, &shutdown_signals, nullptr) != 0) {
    return 1;
  }
#endif
  static base::AtExitManager* at_exit = new base::AtExitManager();
  (void)at_exit;
  base::CommandLine::Init(argc, argv);
  logging::SetMinLogLevel(logging::LOGGING_ERROR);
  if (!base::i18n::InitializeICU()) {
    return 1;
  }
  base::FeatureList::InitInstance(std::string(), std::string());
  base::ThreadPoolInstance::CreateAndStartWithDefaultParams("chromenet");
  chromenet::g_net_thread = new base::Thread("chromenet-network");
  if (!chromenet::g_net_thread->StartWithOptions(
          base::Thread::Options(base::MessagePumpType::IO, 0))) {
    return 1;
  }
  return 0;
}

chrome_engine_t* chrome_engine_create(const void* opts, size_t opts_len) {
  chromenet::EngineConfig config;
  if (!chromenet::ParseEngineConfig(static_cast<const char*>(opts), opts_len, &config)) {
    return nullptr;
  }
  std::string key(static_cast<const char*>(opts), opts_len);
  chromenet::Registry& registry = chromenet::GetRegistry();
  base::AutoLock hold(registry.lock);
  chromenet::Registry::Entry& entry = registry.entries[key];
  if (!entry.shared) {
    entry.shared = base::MakeRefCounted<chromenet::SharedContext>(std::move(config));
  }
  entry.engines++;
  return new chrome_engine{key, entry.shared};
}

int32_t chrome_engine_destroy(chrome_engine_t* e) {
  {
    chromenet::Registry& registry = chromenet::GetRegistry();
    base::AutoLock hold(registry.lock);
    auto it = registry.entries.find(e->key);
    if (it != registry.entries.end() && --it->second.engines == 0) {
      registry.entries.erase(it);
    }
  }
  delete e;
  return CHROME_NET_OK;
}

chrome_request_t* chrome_request_start(chrome_engine_t* e,
                                       const void* head,
                                       size_t head_len,
                                       int64_t body_len) {
  auto core = base::MakeRefCounted<chromenet::RequestCore>();
  auto* job = new chromenet::Job(e->shared, core);
  auto* r = new chrome_request{core, job};
  chromenet::RequestParams params;
  if (body_len < -1 ||
      !chromenet::ParseRequestHead(static_cast<const char*>(head), head_len, &params)) {
    core->Finish(net::ERR_INVALID_ARGUMENT);
    return r;
  }
  params.body_len = body_len;
  chromenet::NetRunner()->PostTask(
      FROM_HERE, base::BindOnce(&chromenet::Job::Start, base::Unretained(job), std::move(params)));
  return r;
}

int32_t chrome_request_write(chrome_request_t* r, const void* data, size_t data_len, bool eof) {
  r->core->BeginWrite();
  chromenet::NetRunner()->PostTask(
      FROM_HERE, base::BindOnce(&chromenet::Job::Write, base::Unretained(r->job),
                                std::string(static_cast<const char*>(data), data_len), eof));
  return r->core->WaitWrite();
}

int32_t chrome_request_wait(chrome_request_t* r, void* head, size_t head_cap, size_t* head_len) {
  std::string h;
  int rv = r->core->WaitHead(&h);
  if (rv != net::OK) {
    *head_len = 0;
    return rv;
  }
  if (h.size() > head_cap) {
    *head_len = 0;
    return net::ERR_RESPONSE_HEADERS_TOO_BIG;
  }
  memcpy(head, h.data(), h.size());
  *head_len = h.size();
  return net::OK;
}

size_t chrome_request_read(chrome_request_t* r,
                           void* buf,
                           size_t buf_cap,
                           bool* eof,
                           int32_t* net_error) {
  if (buf_cap == 0) {
    *eof = false;
    *net_error = net::OK;
    return 0;
  }
  r->core->BeginRead();
  chromenet::NetRunner()->PostTask(
      FROM_HERE, base::BindOnce(&chromenet::Job::Read, base::Unretained(r->job), buf_cap));
  int err = net::OK;
  size_t n = r->core->WaitRead(static_cast<char*>(buf), buf_cap, eof, &err);
  *net_error = err;
  return n;
}

void chrome_request_cancel(chrome_request_t* r) {
  r->core->Finish(net::ERR_ABORTED);
  chromenet::NetRunner()->PostTask(
      FROM_HERE, base::BindOnce(&chromenet::Job::Cancel, base::Unretained(r->job)));
}

const char* chrome_net_error_name(int32_t error) {
  thread_local std::string name;
  name = net::ErrorToShortString(error);
  if (name.starts_with("net::")) {
    name.erase(0, 5);
  }
  return name.c_str();
}

int32_t chrome_request_destroy(chrome_request_t* r) {
  r->core->Finish(net::ERR_ABORTED);
  chromenet::NetRunner()->DeleteSoon(FROM_HERE, r->job);
  delete r;
  return CHROME_NET_OK;
}

}  // extern "C"

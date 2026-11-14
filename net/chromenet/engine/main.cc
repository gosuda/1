// Copyright 2026 Go Suda. MIT License.
//
// chrome_net_server: serves chrome_net.idl over HQQ shared memory. The Go client starts it with
// the listener name as the first argument and --parent-stdin as the lifeline.

#include <stdio.h>

#include "third_party/chromenet/chrome_net.h"

// The generated glue is C: chrome_net_iface has C linkage. MSVC-ABI C++ names variables
// differently from C, so the declaration must be seen as extern "C" here.
extern "C" {
#include "third_party/chromenet/chrome_net_server.h"
}

int main(int argc, char** argv) {
  if (chrome_net_init(argc, argv) != 0) {
    fprintf(stderr, "chrome_net_server: runtime initialization failed\n");
    return 1;
  }
  hqqrpc_server_options o;
  hqqrpc_server_options_init(&o);
  o.default_buffer_size = 64u << 10;
  o.default_buffer_count = 64;
  o.max_message = 16u << 20;
  o.max_sessions = 256;
  // Every new session and every cancellation opens a control link; a burst of concurrent requests
  // opens many at once.
  o.max_control_links = 256;
  return hqqrpc_main(&chrome_net_iface, &o, argc, argv);
}

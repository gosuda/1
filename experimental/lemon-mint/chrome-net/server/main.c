/* C11 Daemon Entrypoint for chrome_net_server */
#include <stdio.h>
#include <stdlib.h>
#include "hqq/hqq_rpc.h"
#include "chrome_net_server.h"

static void *server_on_session_start(void *user, hqqrpc_session *s) {
    (void)user;
    (void)s;
    fprintf(stderr, "[chrome_net_server] Session started\n");
    return NULL;
}

static void server_on_session_end(void *user, void *session_user, hqqrpc_session *s) {
    (void)user;
    (void)session_user;
    (void)s;
    fprintf(stderr, "[chrome_net_server] Session ended\n");
}

int main(int argc, char **argv) {
    hqqrpc_server_options o;

    hqqrpc_server_options_init(&o);
    o.default_buffer_size = 64u << 10;   /* 64 KiB frames */
    o.default_buffer_count = 64;
    o.max_segment_bytes = 256u << 20;    /* 256 MiB max segments */
    o.max_message = 128u << 20;          /* 128 MiB */
    o.scratch_keep = 8u << 20;           /* 8 MiB scratch keep */
    o.on_session_start = server_on_session_start;
    o.on_session_end = server_on_session_end;

    return hqqrpc_main(&chrome_net_iface, &o, argc, argv);
}

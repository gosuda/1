#ifndef CHROME_NET_H
#define CHROME_NET_H

#include <stddef.h>
#include <stdint.h>
#include <stdbool.h>

#ifdef __cplusplus
extern "C" {
#endif

#define CHROME_NET_OK 0
#define CHROME_NET_ERROR -1

typedef struct chrome_engine chrome_engine_t;
typedef struct chrome_request chrome_request_t;
typedef struct chrome_wt_session chrome_wt_session_t;
typedef struct chrome_ws_conn chrome_ws_conn_t;

/* Engine lifecycle */
chrome_engine_t *engine_create(const void *opts, size_t opts_len);
int32_t engine_destroy(chrome_engine_t *e);

/* HTTP/1.1, HTTP/2, HTTP/3 transactions */
chrome_request_t *http_request(chrome_engine_t *e,
                               const void *req, size_t req_len,
                               void *headers, size_t headers_cap,
                               size_t *headers_len, int32_t *status);

size_t http_read_body(chrome_request_t *r, uint32_t max_len,
                      void *buf, size_t buf_cap, bool *eof);

int32_t request_destroy(chrome_request_t *r);

/* WebTransport */
chrome_wt_session_t *wt_dial(chrome_engine_t *e, const char *url);
int32_t wt_session_destroy(chrome_wt_session_t *s);

/* WebSocket */
chrome_ws_conn_t *ws_dial(chrome_engine_t *e, const char *url);
int32_t ws_conn_destroy(chrome_ws_conn_t *w);

#ifdef __cplusplus
}
#endif

#endif /* CHROME_NET_H */

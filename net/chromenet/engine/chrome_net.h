/* C API of the chromenet engine. chrome_net.idl declares the same functions; the generated
 * glue (chrome_net_server.c) calls them from HQQ session threads. */
#ifndef CHROMENET_ENGINE_CHROME_NET_H_
#define CHROMENET_ENGINE_CHROME_NET_H_

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define CHROME_NET_OK 0

typedef struct chrome_engine chrome_engine_t;
typedef struct chrome_request chrome_request_t;

chrome_engine_t *chrome_engine_create(const void *opts, size_t opts_len);
int32_t chrome_engine_destroy(chrome_engine_t *e);

chrome_request_t *chrome_request_start(chrome_engine_t *e, const void *head, size_t head_len,
                                       int64_t body_len);
int32_t chrome_request_write(chrome_request_t *r, const void *data, size_t data_len, bool eof);
int32_t chrome_request_wait(chrome_request_t *r, void *head, size_t head_cap, size_t *head_len);
size_t chrome_request_read(chrome_request_t *r, void *buf, size_t buf_cap, bool *eof,
                           int32_t *net_error);
void chrome_request_cancel(chrome_request_t *r);
int32_t chrome_request_destroy(chrome_request_t *r);
const char *chrome_net_error_name(int32_t error);

/* Initializes the Chromium runtime (ICU, features, thread pool, network thread) once per
 * process. main() calls it before serving. */
int chrome_net_init(int argc, char **argv);

#ifdef __cplusplus
}
#endif

#endif /* CHROMENET_ENGINE_CHROME_NET_H_ */

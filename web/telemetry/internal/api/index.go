package api

import (
	"net/http"

	"github.com/julienschmidt/httprouter"
	"telemetry.gosuda.org/telemetry/internal/types"
)

var _index_html = []byte(
	`<!DOCTYPE html>
<html>
<head>
	<title>GoSuda Telemetry Service</title>
</head>
<body>
	<h1>GoSuda Telemetry Service</h1>
	<hr/>
	<p>Welcome to the GoSuda Telemetry Service!</p>
	<p>Public APIs:</p>
	<ul>
		<li>GET <a href="/healthz">/healthz</a> - Check the health of the service</li>
		<li>GET <a href="/idz">/idz</a> - Generate a new randflake ID</li>
		<li>POST <code>/client/like</code> - Submit a like (JSON: client_id, client_token, url)</li>
		<li>GET <code>/like/count?url=<url></code> - Get like count for a normalized URL (host + pathname)</li>
		<li>POST <code>/client/view</code> - Submit a view (JSON: client_id, client_token, url)</li>
		<li>GET <code>/view/count?url=<url></code> - Get view count for a normalized URL (host + pathname)</li>
		<li>POST <code>/counts/bulk</code> - Bulk lookup counts for multiple URLs (JSON body: { "urls": ["https://...","..."] })</li>
	</ul>
	<p>Notes:</p>
	<ul>
		<li>URLs are normalized to host + pathname before storage and queries.</li>
		<li>CORS: all origins are allowed.</li>
	</ul>
</body>
</html>`,
)

func IndexHandler(is types.InternalServiceProvider) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.Write(_index_html)
	}
}

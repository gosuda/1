package suihttp

import (
	_ "embed"
	"net/http"
)

//go:embed client.js
var clientJS []byte

// ClientHandler serves the shared browser Sui wallet client that prepares
// and submits x402 payments against the prepare handler. Mount it at
// whatever path pages reference as its script src.
func ClientHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(clientJS)
	})
}

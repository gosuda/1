package api

import "net/http"

// CORS returns a middleware that sets CORS headers based on the incoming request.
// If the request contains an Origin header, it is echoed back in
// Access-Control-Allow-Origin. Otherwise it falls back to "*".
//
// The middleware also sets a Vary: Origin header to avoid caching issues
// and responds to OPTIONS (preflight) requests with HTTP 200. For preflight it
// reflects Access-Control-Request-Method and Access-Control-Request-Headers when provided.
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			// Echo the request origin and allow credentials for browsers
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Add("Vary", "Origin")
		} else {
			// No Origin header (e.g., same-origin requests from tools), allow all
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}

		// Default methods/headers for non-preflight responses
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Max-Age", "86400")

		// Handle preflight by reflecting requested method and headers when present.
		if r.Method == http.MethodOptions {
			// Reflect requested method if provided
			if acrm := r.Header.Get("Access-Control-Request-Method"); acrm != "" {
				w.Header().Set("Access-Control-Allow-Methods", acrm)
			} else {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			}

			// Reflect requested headers if provided
			if acrh := r.Header.Get("Access-Control-Request-Headers"); acrh != "" {
				w.Header().Set("Access-Control-Allow-Headers", acrh)
			} else {
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
			}

			w.WriteHeader(http.StatusOK)
			return
		}

		// Forward to next handler
		if next != nil {
			next.ServeHTTP(w, r)
		}
	})
}

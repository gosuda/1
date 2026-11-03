package suihttp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClientHandlerServesClientJS(t *testing.T) {
	recorder := httptest.NewRecorder()
	ClientHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x402/client.js", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "application/javascript; charset=utf-8", recorder.Header().Get("Content-Type"))
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))

	body, err := io.ReadAll(recorder.Body)
	require.NoError(t, err)
	require.NotEmpty(t, body)
	for _, exported := range []string{"getSuiWallets", "onSuiWalletChange", "prepareX402Payment", "x402Fetch"} {
		require.Contains(t, string(body), exported)
	}
	// The browser client talks to the prepare endpoint by default.
	require.Contains(t, string(body), "/x402/prepare")
}

func TestClientHandlerHeadServesHeadersOnly(t *testing.T) {
	recorder := httptest.NewRecorder()
	ClientHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, "/x402/client.js", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "application/javascript; charset=utf-8", recorder.Header().Get("Content-Type"))
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	require.Empty(t, recorder.Body.Bytes())
}

func TestClientHandlerMethodIsGetAndHeadOnly(t *testing.T) {
	recorder := httptest.NewRecorder()
	ClientHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/x402/client.js", nil))
	require.Equal(t, http.StatusMethodNotAllowed, recorder.Code)
	require.Equal(t, "GET, HEAD", recorder.Header().Get("Allow"))
}

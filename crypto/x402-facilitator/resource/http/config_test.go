package x402http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gosuda/x402-facilitator/types"
)

func TestNewValidation(t *testing.T) {
	valid := func() Config {
		return Config{
			Requirements: testRequirements,
			Facilitator:  &stubFacilitator{},
			Resource:     testResource,
		}
	}
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"nil facilitator", func(c *Config) { c.Facilitator = nil }},
		{"blank scheme", func(c *Config) { c.Requirements.Scheme = "  " }},
		{"blank network", func(c *Config) { c.Requirements.Network = "" }},
		{"blank asset", func(c *Config) { c.Requirements.Asset = " " }},
		{"blank amount", func(c *Config) { c.Requirements.Amount = " " }},
		{"blank payTo", func(c *Config) { c.Requirements.PayTo = "\t" }},
		{"nil resource", func(c *Config) { c.Resource = nil }},
		{"blank resource url", func(c *Config) { c.Resource = &types.ResourceInfo{URL: "  "} }},
		{"conflicting extra payment flow", func(c *Config) {
			c.Requirements.Extra = map[string]interface{}{"paymentFlow": "authorization"}
		}},
		{"negative request timeout", func(c *Config) { c.RequestTimeout = -time.Second }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(&cfg)
			gate, err := New(cfg)
			require.Error(t, err)
			require.Nil(t, gate)
		})
	}
}

func TestNewNormalizesExtraPaymentFlow(t *testing.T) {
	cfg := Config{
		Requirements: testRequirements,
		Facilitator:  &stubFacilitator{},
		Resource:     testResource,
	}
	cfg.Requirements.Extra = map[string]interface{}{"assetVersion": "2"}

	gate, err := New(cfg)
	require.NoError(t, err)
	require.Equal(t, map[string]interface{}{"assetVersion": "2"}, cfg.Requirements.Extra,
		"New must not mutate the caller's extra map")

	stub := &stubFacilitator{}
	gate, err = New(Config{
		Requirements: types.PaymentRequirements{
			Scheme:  string(types.Exact),
			Network: "eip155:84532",
			Asset:   "0xasset",
			Amount:  "10000",
			PayTo:   "0xpayto",
			Extra:   map[string]interface{}{"assetVersion": "2"},
		},
		Facilitator: stub,
		Resource:    testResource,
	})
	require.NoError(t, err)
	handler := gate.Wrap(okHandler)
	payload := v2Payload()
	payload.Accepted.Extra["assetVersion"] = "2"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, paidRequest(t, payload))

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, stub.reqs)
	require.Equal(t, PaymentFlowUpfront, stub.reqs.Extra["paymentFlow"],
		"settle must receive the advertised upfront flow")
	require.Equal(t, "2", stub.reqs.Extra["assetVersion"], "existing extra keys survive normalization")
}

func TestNewDefaultsAndNoCfgMutation(t *testing.T) {
	cfg := Config{
		Requirements: testRequirements,
		Facilitator:  &stubFacilitator{},
		Resource:     testResource,
	}
	cfg.Requirements.MaxTimeoutSeconds = 0

	gate, err := New(cfg)
	require.NoError(t, err)
	require.Zero(t, cfg.Requirements.MaxTimeoutSeconds, "New must not mutate cfg")

	downstream := false
	handler := gate.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { downstream = true }))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/resource", nil))

	require.Equal(t, http.StatusPaymentRequired, rec.Code)
	require.False(t, downstream)
	body := decodeChallenge(t, rec)
	require.Len(t, body.Accepts, 1)
	require.Equal(t, 60, body.Accepts[0].MaxTimeoutSeconds, "unset maxTimeoutSeconds defaults to 60")

	payload, err := json.Marshal(body.Accepts[0])
	require.NoError(t, err)
	require.Contains(t, string(payload), `"maxTimeoutSeconds":60`)
}

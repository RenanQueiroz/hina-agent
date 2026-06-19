package llm

import (
	"net/http"
	"time"
)

// noProxyTransport returns an http.Transport that never consults the
// HTTP_PROXY/HTTPS_PROXY/NO_PROXY environment variables (Proxy is nil). This
// carries V1's `trust_env=false` rule forward: a stray proxy env var must not be
// able to intercept model traffic or the bearer API key. It clones
// http.DefaultTransport so connection-pool, dial, and TLS defaults are kept,
// then disables proxy lookup.
func noProxyTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return tr
}

// noProxyHTTPClient builds an *http.Client using the no-proxy transport. A
// timeout of 0 means no client-level deadline (correct for long-lived streaming
// responses, where cancellation is driven by the request context instead).
func noProxyHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: noProxyTransport()}
}

package llm

import (
	"net/http"
	"testing"
)

// TestNoProxyClientIgnoresProxyEnv proves the no-proxy client carries V1's
// trust_env=false rule: even with HTTP(S)_PROXY set, the transport's Proxy hook
// is nil, so no request can ever be routed through a proxy (which would
// otherwise be able to intercept the bearer API key). We assert on Proxy==nil
// rather than dialing a live proxy because Go's env-based proxy resolution
// bypasses loopback hosts by default, which would make a localhost-backend dial
// test pass even without the fix.
func TestNoProxyClientIgnoresProxyEnv(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:9")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:9")
	t.Setenv("NO_PROXY", "")

	c := noProxyHTTPClient(0)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", c.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("transport.Proxy must be nil so HTTP(S)_PROXY is ignored")
	}
}

// TestOpenAICompatUsesNoProxyTransport ensures the wiring actually reaches the
// provider's HTTP client, not just the helper.
func TestOpenAICompatUsesNoProxyTransport(t *testing.T) {
	p := NewOpenAICompatProvider("", "k", "m")
	tr, ok := p.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("compat client transport = %T, want *http.Transport", p.client.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("compat provider must use a no-proxy transport (Proxy must be nil)")
	}
}

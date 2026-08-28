package shim

import (
	"net/http/httptest"
	"net/url"
	"testing"
)

func testProxyServer(t *testing.T, prefix string, upstream *httptest.Server, strategy proxyStrategy) *proxyServer {
	t.Helper()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("failed to parse upstream URL: %v", err)
	}
	return testProxyServerFromRoutes([]proxyRoute{{
		name:      "test",
		prefix:    prefix,
		upstreams: newUpstreamPool([]*url.URL{upstreamURL}),
		client:    upstream.Client(),
		strategy:  strategy,
	}})
}

func testProxyServerFromRoutes(routes []proxyRoute) *proxyServer {
	server := &proxyServer{}
	server.state.Store(&proxyState{
		runtime: &runtimeConfig{ListenAddr: defaultListenAddr},
		table:   &routingTable{routes: routes},
	})
	return server
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func mustMarshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	out, err := marshalJSON(value)
	if err != nil {
		t.Fatalf("failed to marshal JSON: %v", err)
	}
	return out
}

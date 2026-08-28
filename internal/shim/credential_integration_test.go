package shim

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCredentialNotChargedOnClientCancel(t *testing.T) {
	hit := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done() // hang until the canceled client tears the request down
	}))
	defer upstream.Close()

	prov := &recordingProvider{key: "shim-account-key"}
	server := testProxyServerFromRoutes([]proxyRoute{{
		name:        "anyrouter",
		prefix:      anyRouterPrefix,
		upstreams:   newUpstreamPool([]*url.URL{mustURL(t, upstream.URL)}),
		credentials: prov,
		client:      upstream.Client(),
		strategy:    anyRouterStrategy{},
	}})
	proxy := httptest.NewServer(server)
	defer proxy.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/any/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("canceled request should fail on the client side")
	}
	select {
	case <-hit: // the request reached the upstream, so a credential was assigned
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the request; cannot assert charging")
	}

	// By now the client has canceled (Do returned an error) and the request reached
	// the upstream, so assign ran exactly once. On the cancel path ServeHTTP returns
	// before the reportResult charge (upstreamExhausted is false and the context is
	// canceled), so reportResult is never called — reports stays 0.
	assigns, reports := prov.counts()
	if assigns != 1 {
		t.Fatalf("assign calls = %d, want 1 (one credential chosen for the request)", assigns)
	}
	if reports != 0 {
		t.Fatalf("reportResult calls = %d, want 0 (a client cancel must not be charged)", reports)
	}
}

// TestGlobalTargetSkipsCredentialProvider proves that a classifier request
// redirected to the global target never consults the
// route's credentialProvider at all (assign/reportResult both zero) — it carries
// the configured classifier target key, not an account key.
func TestGlobalTargetSkipsCredentialProvider(t *testing.T) {
	var gotAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"<block>no</block>"}]}`))
	}))
	defer target.Close()

	prov := &recordingProvider{key: "sk-account"}
	server := classifierTestServer(t, classifierRuntimeConfig{
		TargetBaseURL: target.URL,
		TargetKey:     "shim-target-key",
	}, target, proxyRoute{
		name:        "anyrouter",
		prefix:      anyRouterPrefix,
		upstreams:   newUpstreamPool([]*url.URL{mustURL(t, "https://anyrouter.invalid")}),
		credentials: prov,
		client:      target.Client(),
		strategy:    anyRouterStrategy{},
	})
	proxy := httptest.NewServer(server)
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/any/v1/messages", bytes.NewReader(classifierBodyWithModel("claude-opus-4-8")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if assigns, reports := prov.counts(); assigns != 0 || reports != 0 {
		t.Fatalf("global target consulted the credential provider: assigns=%d reports=%d, want 0/0", assigns, reports)
	}
	if gotAuth != "Bearer shim-target-key" {
		t.Fatalf("global target Authorization = %q, want Bearer shim-target-key (the classifier target key, not an account)", gotAuth)
	}
}

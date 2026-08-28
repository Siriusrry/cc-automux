package shim

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUnknownPrefixReturnsNotFound(t *testing.T) {
	proxy := httptest.NewServer(&proxyServer{})
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages?beta=true", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

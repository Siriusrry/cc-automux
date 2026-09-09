package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigConditionalReplacement(t *testing.T) {
	manager := testManager(t, nil)
	handler := New(manager)
	get := request(handler, "GET", "/api/v1/config", "Bearer management-key", "")
	tag := get.Header().Get("ETag")
	if tag == "" || get.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("configuration response lacks a validator or cache protection")
	}
	put := func(body, condition string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/api/v1/config", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer management-key")
		req.Header.Set("If-Match", condition)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	cfg := manager.Snapshot().Config()
	body := marshalManagementClientConfig(t, cfg)
	if rec := put(body, tag); rec.Code != http.StatusOK {
		t.Fatalf("unchanged configuration: %d %s", rec.Code, rec.Body.String())
	}
	cfg.Auth.GatewayKey = "new-gateway-key"
	body = marshalManagementClientConfig(t, cfg)
	if rec := put(body, "W/"+tag); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("weak validator: %d", rec.Code)
	}
	// Two tabs save different full configurations from the same read.
	results := make(chan int, 2)
	for _, key := range []string{"first-gateway-key", "second-gateway-key"} {
		next := cfg.Clone()
		next.Auth.GatewayKey = key
		data := marshalManagementClientConfig(t, next)
		go func() { results <- put(data, tag).Code }()
	}
	a, b := <-results, <-results
	if !((a == 200 && b == 412) || (a == 412 && b == 200)) {
		t.Fatalf("racing saves = %d, %d; want one success and one conflict", a, b)
	}
	before := manager.Snapshot()
	if rec := put(body, tag); rec.Code != 412 || !strings.Contains(rec.Body.String(), "configuration_changed") {
		t.Fatalf("stale save: %d %s", rec.Code, rec.Body.String())
	}
	if manager.Snapshot() != before {
		t.Fatal("rejected save published a snapshot")
	}
	// Validators describe content rather than a process-local revision.
	other := testManager(t, nil)
	if other.Snapshot().Revision() != 1 || other.Snapshot().ConfigETag() == before.ConfigETag() {
		t.Fatal("a fresh process must not mistake a different configuration for the old one")
	}
	if rec := put(body, "\"unrelated\", "+before.ConfigETag()); rec.Code != 200 {
		t.Fatalf("matching validator list: %d", rec.Code)
	}
}

package shim

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminServesConfiguredPage(t *testing.T) {
	server := newTestAdminServer(t, baseAdminConfig(), "")

	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("GET /admin content-type = %q, want text/html", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "cc-auto-mode-shim") || !strings.Contains(body, "/admin/assets/admin.js") {
		t.Fatalf("GET /admin body missing expected markers (len %d)", len(body))
	}
}

func TestAdminPathsNotMatchedByProxyRoute(t *testing.T) {
	table := newTestAdminServer(t, baseAdminConfig(), "").currentState().table
	for _, path := range []string{"/admin", "/admin/config", "/admin/status", "/admin/logs", "/admin/logs/tail", "/admin/assets/product-mascot.png", "/admin/assets/shared.css", "/admin/assets/admin.css", "/admin/assets/logs.css", "/admin/assets/admin.js", "/admin/assets/logs.js"} {
		if _, _, ok := table.matchRoute(path); ok {
			t.Fatalf("proxy route unexpectedly matched %q", path)
		}
	}
}

// TestServeHTTPInterceptsAdminBeforeProxy drives the full ServeHTTP entry point
// (not just matchRoute) to prove handleLocalRequest handles /admin* before the
// request can fall through to the AnyRouter/CPA proxy routes or NotFound.
func TestServeHTTPInterceptsAdminBeforeProxy(t *testing.T) {
	server := newTestAdminServer(t, baseAdminConfig(), "")

	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin via ServeHTTP = %d, want 200 (intercepted)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("GET /admin content-type = %q, want text/html", ct)
	}

	// A non-admin, non-proxy path must NOT be intercepted: it falls through to
	// the proxy router and 404s, confirming interception is scoped to /admin*.
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/not-admin", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /not-admin via ServeHTTP = %d, want 404", rec.Code)
	}
}

func TestAdminAssetContentTypes(t *testing.T) {
	server := newTestAdminServer(t, baseAdminConfig(), "")
	cases := []struct {
		path, wantCT string
	}{
		{"/admin/assets/product-mascot.png", "image/png"},
		{"/admin/assets/shared.css", "text/css"},
		{"/admin/assets/admin.css", "text/css"},
		{"/admin/assets/logs.css", "text/css"},
		{"/admin/assets/admin.js", "text/javascript"},
		{"/admin/assets/logs.js", "text/javascript"},
		{"/admin/logs", "text/html"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", tc.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tc.wantCT) {
			t.Fatalf("GET %s content-type = %q, want %q", tc.path, ct, tc.wantCT)
		}
	}
}

func TestAdminAssetRoutesRejectNonGet(t *testing.T) {
	server := newTestAdminServer(t, baseAdminConfig(), "")
	for _, path := range []string{"/admin", "/admin/logs", "/admin/assets/product-mascot.png", "/admin/assets/shared.css", "/admin/assets/admin.css", "/admin/assets/logs.css", "/admin/assets/admin.js", "/admin/assets/logs.js"} {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s status = %d, want 405", path, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Fatalf("POST %s Allow = %q, want %q", path, allow, http.MethodGet)
		}
	}
}

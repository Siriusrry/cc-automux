package webui

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/version"
)

func TestEmbeddedConsole(t *testing.T) {
	h := New()
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/", 302}, {"HEAD", "/ui", 302}, {"GET", "/ui/", 200},
		{"HEAD", "/ui/", 200}, {"POST", "/ui/", 405},
		{"GET", "/ui/assets/../index.html", 404}, {"GET", "/ui/assets/core/", 404},
		{"GET", "/ui/assets/missing.js", 404}, {"GET", "/ui/providers", 404},
	} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(tc.method, tc.path, nil))
		if r.Code != tc.status {
			t.Fatalf("%s %s: %d", tc.method, tc.path, r.Code)
		}
		if tc.status == 302 && r.Header().Get("Location") != "/ui/" {
			t.Fatal("redirect")
		}
		if tc.status == 405 && r.Header().Get("Allow") != "GET, HEAD" {
			t.Fatal("allowed methods")
		}
		if tc.method == "HEAD" && r.Body.Len() != 0 {
			t.Fatal("HEAD returned a body")
		}
	}
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/ui/", nil))
	if !strings.Contains(r.Body.String(), "content=\""+version.Current()+"\"") {
		t.Fatal("version injection")
	}
	if r.Header().Get("Content-Security-Policy") != contentSecurityPolicy || r.Header().Get("Cache-Control") != "no-cache" {
		t.Fatal("console headers")
	}
	refs := regexp.MustCompile("(?:src|href)=\"(assets/[^\"]+)\"").FindAllStringSubmatch(r.Body.String(), -1)
	if len(refs) == 0 {
		t.Fatal("no assets referenced")
	}
	for _, ref := range refs {
		asset := httptest.NewRecorder()
		h.ServeHTTP(asset, httptest.NewRequest("GET", "/ui/"+ref[1], nil))
		if asset.Code != 200 || asset.Body.Len() == 0 || asset.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("unavailable asset: %s (%d)", ref[1], asset.Code)
		}
	}
}

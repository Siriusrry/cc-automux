// Package webui serves the local management console from the application binary.
package webui

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/Siriusrry/cc-automux/internal/version"
)

//go:embed assets
var files embed.FS

const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data: http://127.0.0.1:*; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

type Handler struct{ shell []byte }

func New() *Handler {
	shell, err := files.ReadFile("assets/index.html")
	if err != nil {
		panic(err) // The shell is a required embedded build input.
	}
	return &Handler{shell: bytes.ReplaceAll(shell, []byte("__CC_AUTOMUX_VERSION__"), []byte(version.Current()))}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/" || r.URL.Path == "/ui" {
		http.Redirect(w, r, "/ui/", http.StatusFound)
		return
	}
	data, kind := h.shell, "text/html; charset=utf-8"
	if r.URL.Path != "/ui/" {
		name, ok := strings.CutPrefix(r.URL.Path, "/ui/assets/")
		types := map[string]string{".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8", ".svg": "image/svg+xml"}
		kind = types[path.Ext(name)]
		if !ok || !fs.ValidPath(name) || kind == "" {
			http.NotFound(w, r)
			return
		}
		var err error
		data, err = files.ReadFile("assets/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

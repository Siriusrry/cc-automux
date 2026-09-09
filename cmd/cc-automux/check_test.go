package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/config"
)

func TestCheckConfigurationPreservesBytesAndRejectsPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if _, _, err := config.Initialize(path, "test-management-key"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, err := checkConfiguration(path); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("preflight changed configuration")
	}
	pending := config.PendingPath(path)
	if err := os.WriteFile(pending, []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := checkConfiguration(path); err == nil {
		t.Fatal("accepted unfinished restart")
	}
	if data, err := os.ReadFile(pending); err != nil || string(data) != "incomplete" {
		t.Fatal("preflight changed pending file")
	}
	if err := os.Remove(pending); err != nil {
		t.Fatal(err)
	}
	unknown := bytes.Replace(before, []byte(`"schema_version"`), []byte(`"future_field": true, "schema_version"`), 1)
	if err := os.WriteFile(path, unknown, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := checkConfiguration(path); err == nil {
		t.Fatal("accepted unsupported configuration fields")
	}
}

func TestReadinessAuthenticatesAndChecksIdentity(t *testing.T) {
	version, product, pending, webOK := "v1.0.0", "CC AutoMux", false, true
	var addr string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/management" {
			if r.Header.Get("Authorization") != "" {
				t.Error("key sent to static resources")
			}
			if !webOK {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte("<html>CC AutoMux</html>"))
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-management-key" {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"product": product, "version": version, "listen_addr": addr, "pending": pending})
	}))
	defer server.Close()
	addr = strings.TrimPrefix(server.URL, "http://")
	cfg := config.Default()
	cfg.Auth.ManagementKey = "test-management-key"
	cfg.Service.ListenAddr = addr
	probe := func() error { return probeReady(context.Background(), server.Client(), cfg, "v1.0.0") }
	if err := probe(); err != nil {
		t.Fatal(err)
	}
	version = "v1.0.0-dev"
	if err := probe(); err == nil {
		t.Fatal("accepted wrong version")
	}
	version, product = "v1.0.0", "Other"
	if err := probe(); err == nil {
		t.Fatal("accepted wrong product")
	}
	product, pending = "CC AutoMux", true
	if err := probe(); err == nil {
		t.Fatal("accepted pending restart")
	}
	pending, webOK = false, false
	if err := probe(); err == nil {
		t.Fatal("accepted missing web console")
	}
	webOK = true
	cfg.Auth.ManagementKey = "wrong-key"
	if err := probe(); err == nil {
		t.Fatal("accepted unauthenticated status")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := waitReady(ctx, cfg, "v1.0.0"); err == nil {
		t.Fatal("readiness unexpectedly succeeded")
	}
}

func TestCheckPortRejectsOccupiedAndNonLoopback(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, addr := range []string{listener.Addr().String(), "0.0.0.0:8765", "127.0.0.1:0"} {
		if code := runCheck([]string{"--listen-addr", addr}, &bytes.Buffer{}, &bytes.Buffer{}); code == 0 {
			t.Fatalf("accepted %s", addr)
		}
	}
}

func TestInspectServicePaths(t *testing.T) {
	app := filepath.Join(t.TempDir(), `space 100% $ literal " back\slash`, "cc-automux")
	cfg := filepath.Join(app, "custom.json")
	quote := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `"`, `\"`)
		s = strings.ReplaceAll(s, `%`, `%%`)
		return `"` + s + `"`
	}
	unit := "[Service]\nExecStart=" + quote(strings.ReplaceAll(filepath.Join(app, "bin", "cc-automux"), "$", "$$")) + "\nEnvironment=" + quote("CC_AUTOMUX_CONFIG="+cfg) + "\nEnvironment=" + quote("XDG_STATE_HOME="+filepath.Dir(app)) + "\n"
	path := filepath.Join(t.TempDir(), "test.service")
	if err := os.WriteFile(path, []byte(unit), 0600); err != nil {
		t.Fatal(err)
	}
	gotApp, gotConfig, _, err := inspectService(path)
	if err != nil || gotApp != app || gotConfig != cfg {
		t.Fatalf("inspect unit: %q %q %v", gotApp, gotConfig, err)
	}
	if _, err := unitValue(`"/tmp/%h/bin"`); err == nil {
		t.Fatal("accepted unrecognized specifier")
	}
}

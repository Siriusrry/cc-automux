package app

import (
	"encoding/json"
	"errors"
	"io"
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

func freeListenAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	return addr
}

func writeAppConfig(t *testing.T, path string, cfg config.Config) *config.Store {
	t.Helper()
	store, err := config.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	return store
}

func baseAppConfig(t *testing.T, path string) (*config.Store, config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.Service.ListenAddr = freeListenAddr(t)
	cfg.Auth.ManagementKey = "management-key"
	return writeAppConfig(t, path, cfg), cfg
}

func TestNewBindsLoopbackAndServesOnlyManagementAPI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	_, cfg := baseAppConfig(t, path)
	application, err := New(Options{ConfigPath: path, Restart: func() error { return errors.New("not used") }})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer application.Close()
	if host, _, err := net.SplitHostPort(application.Listener().Addr().String()); err != nil || host != "127.0.0.1" {
		t.Fatalf("listener = %s, err %v", application.Listener().Addr(), err)
	}
	handler := application.server.Handler
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Auth.ManagementKey)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status endpoint = %d %s", rec.Code, rec.Body.String())
	}
	for _, route := range []string{"/any", "/cpa", "/admin", "/v1/messages"} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("retired route %s = %d", route, rec.Code)
		}
	}
}

func TestPendingConfigPromotesOnlyAfterResourcesAreReady(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, active := baseAppConfig(t, path)
	pending := active.Clone()
	pending.Service.ListenAddr = freeListenAddr(t)
	pending.Auth.GatewayKey = "new-gateway"
	if err := store.SavePending(pending); err != nil {
		t.Fatal(err)
	}
	application, err := New(Options{ConfigPath: path, Restart: func() error { return nil }})
	if err != nil {
		t.Fatalf("New(pending) error = %v", err)
	}
	defer application.Close()
	loaded, err := store.Load()
	if err != nil || loaded.Service.ListenAddr != pending.Service.ListenAddr || loaded.Auth.GatewayKey != "new-gateway" {
		t.Fatalf("active after promotion = %#v, %v", loaded, err)
	}
	if exists, _ := store.PendingExists(); exists {
		t.Fatal("pending file remains after promotion")
	}
	if application.Listener().Addr().String() != pending.Service.ListenAddr {
		t.Fatalf("bound listener = %s, want %s", application.Listener().Addr(), pending.Service.ListenAddr)
	}
}

func TestPendingResourceFailureRollsBackToActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, active := baseAppConfig(t, path)
	pending := active.Clone()
	pending.Service.ListenAddr = freeListenAddr(t)
	pending.Service.LogMaxBytes++
	if err := store.SavePending(pending); err != nil {
		t.Fatal(err)
	}
	logOpener := func(maxBytes int64) (io.Writer, io.Writer, func() error, error) {
		if maxBytes == pending.Service.LogMaxBytes {
			return nil, nil, nil, errors.New("log resource failed")
		}
		return io.Discard, io.Discard, func() error { return nil }, nil
	}
	application, err := New(Options{ConfigPath: path, LogOpener: logOpener, Restart: func() error { return nil }})
	if err != nil {
		t.Fatalf("New(rollback) error = %v", err)
	}
	defer application.Close()
	loaded, err := store.Load()
	if err != nil || loaded.Service.ListenAddr != active.Service.ListenAddr || loaded.Service.LogMaxBytes != active.Service.LogMaxBytes {
		t.Fatalf("active after rollback = %#v, %v", loaded, err)
	}
	if exists, _ := store.PendingExists(); exists {
		t.Fatal("pending file remains after rollback")
	}
	if application.Listener().Addr().String() != active.Service.ListenAddr {
		t.Fatalf("fallback listener = %s, want %s", application.Listener().Addr(), active.Service.ListenAddr)
	}
	if status := application.Manager().RestartStatus(); status.State != "failed" || !strings.Contains(status.LastError, "log resource failed") {
		t.Fatalf("rollback status = %#v", status)
	}
}

func TestActiveLogFailureClosesAlreadyBoundListener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	_, cfg := baseAppConfig(t, path)
	logOpener := func(int64) (io.Writer, io.Writer, func() error, error) {
		return nil, nil, nil, errors.New("active log failed")
	}
	if application, err := New(Options{ConfigPath: path, LogOpener: logOpener}); err == nil || application != nil {
		t.Fatalf("New(active log failure) = %#v, %v", application, err)
	}
	listener, err := net.Listen("tcp", cfg.Service.ListenAddr)
	if err != nil {
		t.Fatalf("listener leaked after startup failure: %v", err)
	}
	_ = listener.Close()
}

func TestPendingPortConflictRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, active := baseAppConfig(t, path)
	occupied := freeListenAddr(t)
	blocker, err := net.Listen("tcp", occupied)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	pending := active.Clone()
	pending.Service.ListenAddr = occupied
	if err := store.SavePending(pending); err != nil {
		t.Fatal(err)
	}
	application, err := New(Options{ConfigPath: path, Restart: func() error { return nil }})
	if err != nil {
		t.Fatalf("New(port conflict) error = %v", err)
	}
	defer application.Close()
	loaded, _ := store.Load()
	if loaded.Service.ListenAddr != active.Service.ListenAddr {
		t.Fatalf("port-conflict active config = %s", loaded.Service.ListenAddr)
	}
}

func TestLogPreflightFailureDoesNotWritePendingOrChangeActiveState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, cfg := baseAppConfig(t, path)
	logOpener := func(maxBytes int64) (io.Writer, io.Writer, func() error, error) {
		if maxBytes != cfg.Service.LogMaxBytes {
			return nil, nil, nil, errors.New("candidate log failed")
		}
		return io.Discard, io.Discard, func() error { return nil }, nil
	}
	application, err := New(Options{ConfigPath: path, LogOpener: logOpener, Restart: func() error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	next := cfg.Clone()
	next.Service.LogMaxBytes++
	body, _ := json.Marshal(next)
	request := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer "+cfg.Auth.ManagementKey)
	response := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("log preflight response = %d %s", response.Code, response.Body.String())
	}
	if exists, err := store.PendingExists(); err != nil || exists {
		t.Fatalf("pending after log preflight failure = %v, %v", exists, err)
	}
	loaded, err := store.Load()
	if err != nil || loaded.Service.LogMaxBytes != cfg.Service.LogMaxBytes {
		t.Fatalf("active disk changed: %#v, %v", loaded, err)
	}
	if application.Manager().Snapshot().Config().Service.LogMaxBytes != cfg.Service.LogMaxBytes {
		t.Fatal("active snapshot changed after log preflight failure")
	}
}

func TestSelfExecFailureRebindsOldListenerAndKeepsServing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	_, cfg := baseAppConfig(t, path)
	application, err := New(Options{
		ConfigPath:   path,
		Exec:         func() error { return errors.New("exec denied") },
		RestartDelay: -1,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- application.Serve() }()
	defer func() {
		_ = application.Close()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("Serve did not stop")
		}
	}()

	next := cfg.Clone()
	next.Service.LogMaxBytes++
	body, _ := json.Marshal(next)
	request := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer "+cfg.Auth.ManagementKey)
	response := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("restart response = %d %s", response.Code, response.Body.String())
	}
	for i := 0; i < 200; i++ {
		status := application.Manager().RestartStatus()
		if !status.InProgress {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if status := application.Manager().RestartStatus(); status.InProgress || status.State != "failed" {
		t.Fatalf("restart failure status = %#v", status)
	}
	// The old listener was closed during the failed exec and then rebound. A
	// request through the actual server proves the recovery loop resumed.
	client := &http.Client{Timeout: time.Second}
	statusReq, _ := http.NewRequest(http.MethodGet, "http://"+application.Listener().Addr().String()+"/api/v1/status", nil)
	statusReq.Header.Set("Authorization", "Bearer "+cfg.Auth.ManagementKey)
	statusResp, err := client.Do(statusReq)
	if err != nil {
		t.Fatalf("status after exec failure: %v", err)
	}
	statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status after exec failure = %d", statusResp.StatusCode)
	}
}

func TestCloseDuringRestartDoesNotLeaveServeBlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	_, cfg := baseAppConfig(t, path)
	execStarted := make(chan struct{})
	releaseExec := make(chan struct{})
	application, err := New(Options{
		ConfigPath: path,
		Exec: func() error {
			close(execStarted)
			<-releaseExec
			return errors.New("exec denied")
		},
		RestartDelay: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- application.Serve() }()

	next := cfg.Clone()
	next.Service.LogMaxBytes++
	body, _ := json.Marshal(next)
	request := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer "+cfg.Auth.ManagementKey)
	response := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("restart response = %d", response.Code)
	}
	select {
	case <-execStarted:
	case <-time.After(time.Second):
		t.Fatal("exec did not start")
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	close(releaseExec)
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve() after close = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve remained blocked after Close during restart")
	}
}

func TestCappedWriterConsumesCrossBoundaryWrite(t *testing.T) {
	var destination strings.Builder
	writer := newCappedWriter(4, &destination)
	if n, err := writer.Write([]byte("abcdef")); err != nil || n != 6 {
		t.Fatalf("first Write() = %d, %v", n, err)
	}
	if got := destination.String(); got != "abcd" {
		t.Fatalf("destination = %q, want capped content", got)
	}
	if n, err := writer.Write([]byte("more")); err != nil || n != 4 {
		t.Fatalf("dropped Write() = %d, %v", n, err)
	}
	if destination.String() != "abcd" {
		t.Fatalf("destination grew past cap: %q", destination.String())
	}
}

func TestCappedWriterAccountsForExistingFileSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("old"); err != nil {
		t.Fatal(err)
	}
	writer := newCappedWriter(5, file)
	if n, err := writer.Write([]byte("more")); err != nil || n != 4 {
		t.Fatalf("Write() = %d, %v", n, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "oldmo"; got != want {
		t.Fatalf("log content = %q, want %q", got, want)
	}
}

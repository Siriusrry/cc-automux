package app

import (
	"bytes"
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

func TestNewBindsLoopbackAndServesManagementAndMessagesRoutes(t *testing.T) {
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
	for _, route := range []string{"/any", "/cpa", "/admin", "/v1/messages/count_tokens"} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("retired route %s = %d", route, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`)))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "gateway_not_configured") {
		t.Fatalf("unconfigured Messages route = %d %s", rec.Code, rec.Body.String())
	}
}

func TestAppWiresMessagesHealthDiagnosticsAndRawLogging(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/v1/messages" || r.Header.Get("Authorization") != "Bearer provider-key" {
			t.Errorf("upstream request = path %q auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "raw-app-upstream-error")
	}))
	defer upstream.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Service.ListenAddr = freeListenAddr(t)
	cfg.Auth.ManagementKey = "management-key"
	cfg.Auth.GatewayKey = "gateway-key"
	cfg.Providers = []config.ProviderConfig{{
		ID:      "11111111-1111-4111-8111-111111111111",
		Name:    "provider-a",
		BaseURL: upstream.URL + "/prefix",
		APIKey:  "provider-key",
		Models:  []string{"model-a"},
		Enabled: true,
	}}
	writeAppConfig(t, path, cfg)
	var stdout, stderr bytes.Buffer
	application, err := New(Options{
		ConfigPath: path,
		Stdout:     &stdout,
		Stderr:     &stderr,
		Restart:    func() error { return errors.New("not used") },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	crossDataRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"model-a"}`))
	crossDataRequest.Header.Set("Authorization", "Bearer management-key")
	crossDataResponse := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(crossDataResponse, crossDataRequest)
	if crossDataResponse.Code != http.StatusUnauthorized {
		t.Fatalf("management key authenticated Messages route = %d", crossDataResponse.Code)
	}
	crossManagementRequest := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	crossManagementRequest.Header.Set("Authorization", "Bearer gateway-key")
	crossManagementResponse := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(crossManagementResponse, crossManagementRequest)
	if crossManagementResponse.Code != http.StatusUnauthorized {
		t.Fatalf("gateway key authenticated management route = %d", crossManagementResponse.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=1", strings.NewReader(`{"model":"model-a","metadata":{"user_id":"app-session"}}`))
	request.Header.Set("Authorization", "Bearer gateway-key")
	response := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "bad_gateway") {
		t.Fatalf("Messages response = %d %q", response.Code, response.Body.String())
	}
	if !strings.Contains(stderr.String(), `session_id="app-session"`) ||
		!strings.Contains(stderr.String(), `raw-app-upstream-error`) ||
		!strings.Contains(stderr.String(), `global_health="cooldown"`) ||
		!strings.Contains(stderr.String(), `global_entered_cooldown=true channel_entered_cooldown=false cooldown_until="`) ||
		strings.Contains(stderr.String(), `global_entered_cooldown=true channel_entered_cooldown=false cooldown_until=""`) {
		t.Fatalf("gateway error log = %q", stderr.String())
	}

	healthRequest := httptest.NewRequest(http.MethodGet, "/api/v1/provider-health", nil)
	healthRequest.Header.Set("Authorization", "Bearer management-key")
	healthResponse := httptest.NewRecorder()
	application.server.Handler.ServeHTTP(healthResponse, healthRequest)
	if healthResponse.Code != http.StatusOK ||
		!strings.Contains(healthResponse.Body.String(), `"api_key":"provider-key"`) ||
		!strings.Contains(healthResponse.Body.String(), `"last_session_id":"app-session"`) ||
		!strings.Contains(healthResponse.Body.String(), `"last_error":"raw-app-upstream-error"`) ||
		!strings.Contains(healthResponse.Body.String(), upstream.URL+`/prefix/v1/messages?beta=1`) {
		t.Fatalf("provider health = %d %s", healthResponse.Code, healthResponse.Body.String())
	}
}

func TestAppHotUpdateUsesNewProviderGenerationOnNextRequest(t *testing.T) {
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "provider-a")
	}))
	defer upstreamA.Close()
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "provider-b")
	}))
	defer upstreamB.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Service.ListenAddr = freeListenAddr(t)
	cfg.Auth.ManagementKey = "management-key"
	cfg.Auth.GatewayKey = "gateway-key"
	cfg.Providers = []config.ProviderConfig{{
		ID:      "11111111-1111-4111-8111-111111111111",
		Name:    "provider",
		BaseURL: upstreamA.URL,
		APIKey:  "provider-key-a",
		Models:  []string{"model-a"},
		Enabled: true,
	}}
	writeAppConfig(t, path, cfg)
	application, err := New(Options{ConfigPath: path, Restart: func() error { return errors.New("not used") }})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()

	request := func(session string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"model-a","metadata":{"user_id":"`+session+`"}}`))
		req.Header.Set("Authorization", "Bearer gateway-key")
		response := httptest.NewRecorder()
		application.server.Handler.ServeHTTP(response, req)
		return response
	}
	if first := request("before-update"); first.Code != http.StatusOK || first.Body.String() != "provider-a" {
		t.Fatalf("first response = %d %q", first.Code, first.Body.String())
	}
	next := application.Manager().Snapshot().Config()
	next.Providers[0].BaseURL = upstreamB.URL
	next.Providers[0].APIKey = "provider-key-b"
	if _, err := application.Manager().Apply(next); err != nil {
		t.Fatal(err)
	}
	if second := request("after-update"); second.Code != http.StatusOK || second.Body.String() != "provider-b" {
		t.Fatalf("second response = %d %q", second.Code, second.Body.String())
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer provider-key" {
			t.Errorf("recovered upstream request = path %q auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, "recovered-data-plane")
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	store, cfg := baseAppConfig(t, path)
	cfg.Auth.GatewayKey = "gateway-key"
	cfg.Providers = []config.ProviderConfig{{
		ID:      "11111111-1111-4111-8111-111111111111",
		Name:    "provider-a",
		BaseURL: upstream.URL,
		APIKey:  "provider-key",
		Models:  []string{"model-a"},
		Patches: []string{},
		Enabled: true,
	}}
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
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
	messagesReq, _ := http.NewRequest(http.MethodPost, "http://"+application.Listener().Addr().String()+"/v1/messages", strings.NewReader(`{"model":"model-a"}`))
	messagesReq.Header.Set("Authorization", "Bearer gateway-key")
	messagesResp, err := client.Do(messagesReq)
	if err != nil {
		t.Fatalf("Messages after exec failure: %v", err)
	}
	messagesBody, readErr := io.ReadAll(messagesResp.Body)
	_ = messagesResp.Body.Close()
	if readErr != nil || messagesResp.StatusCode != http.StatusOK || string(messagesBody) != "recovered-data-plane" {
		t.Fatalf("Messages after exec failure = %d %q, %v", messagesResp.StatusCode, messagesBody, readErr)
	}
}

func TestRestartClosesInFlightMessagesWithoutDrain(t *testing.T) {
	upstreamStarted := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(upstreamStarted)
		<-releaseUpstream
	}))
	defer upstream.Close()
	defer close(releaseUpstream)

	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Service.ListenAddr = freeListenAddr(t)
	cfg.Auth.ManagementKey = "management-key"
	cfg.Auth.GatewayKey = "gateway-key"
	cfg.Providers = []config.ProviderConfig{{
		ID:      "11111111-1111-4111-8111-111111111111",
		Name:    "provider-a",
		BaseURL: upstream.URL,
		APIKey:  "provider-key",
		Models:  []string{"model-a"},
		Patches: []string{},
		Enabled: true,
	}}
	writeAppConfig(t, path, cfg)
	execStarted := make(chan struct{})
	application, err := New(Options{
		ConfigPath: path,
		Exec: func() error {
			close(execStarted)
			return errors.New("exec denied")
		},
		RestartDelay: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- application.Serve() }()
	defer func() {
		_ = application.Close()
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Error("Serve did not stop")
		}
	}()
	ready := false
	for i := 0; i < 100; i++ {
		request, _ := http.NewRequest(http.MethodGet, "http://"+cfg.Service.ListenAddr+"/api/v1/status", nil)
		request.Header.Set("Authorization", "Bearer management-key")
		response, requestErr := (&http.Client{Timeout: 100 * time.Millisecond}).Do(request)
		if requestErr == nil && response.StatusCode == http.StatusOK {
			_ = response.Body.Close()
			ready = true
			break
		}
		if response != nil {
			_ = response.Body.Close()
		}
		time.Sleep(time.Millisecond)
	}
	if !ready {
		t.Fatal("application did not become ready")
	}

	dataDone := make(chan error, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, "http://"+cfg.Service.ListenAddr+"/v1/messages", strings.NewReader(`{"model":"model-a"}`))
		request.Header.Set("Authorization", "Bearer gateway-key")
		response, requestErr := (&http.Client{Timeout: 5 * time.Second}).Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		dataDone <- requestErr
	}()
	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Messages request did not reach upstream")
	}

	next := cfg.Clone()
	next.Service.LogMaxBytes++
	body, _ := json.Marshal(next)
	restartRequest := httptest.NewRequest(http.MethodPut, "/api/v1/config", bytes.NewReader(body))
	restartRequest.Header.Set("Authorization", "Bearer management-key")
	restartResponse := httptest.NewRecorder()
	startedAt := time.Now()
	application.server.Handler.ServeHTTP(restartResponse, restartRequest)
	if restartResponse.Code != http.StatusAccepted {
		t.Fatalf("restart response = %d %s", restartResponse.Code, restartResponse.Body.String())
	}
	select {
	case <-execStarted:
		if elapsed := time.Since(startedAt); elapsed >= 2*time.Second {
			t.Fatalf("restart waited for drain: %v", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restart waited instead of starting exec")
	}
	select {
	case <-dataDone:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight client request was not interrupted")
	}
	for i := 0; i < 100 && application.gateway.ActiveRequests() != 0; i++ {
		time.Sleep(time.Millisecond)
	}
	if active := application.gateway.ActiveRequests(); active != 0 {
		t.Fatalf("active Messages requests after restart = %d", active)
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

func TestCappedWriterPreservesOversizedRecordThenRolls(t *testing.T) {
	var destination strings.Builder
	writer := newCappedWriter(4, &destination)
	if n, err := writer.Write([]byte("abcdef")); err != nil || n != 6 {
		t.Fatalf("first Write() = %d, %v", n, err)
	}
	if got := destination.String(); got != "abcdef" {
		t.Fatalf("destination = %q, want complete record", got)
	}
	if n, err := writer.Write([]byte("more")); err != nil || n != 4 {
		t.Fatalf("rolled Write() = %d, %v", n, err)
	}
	if destination.String() != "more" {
		t.Fatalf("destination did not roll over: %q", destination.String())
	}
	if n, err := writer.Write([]byte("next")); err != nil || n != 4 {
		t.Fatalf("second rolled Write() = %d, %v", n, err)
	}
	if destination.String() != "next" {
		t.Fatalf("destination stopped after first rollover: %q", destination.String())
	}
}

func TestCappedWriterAccountsForExistingFileSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("already-too-large"); err != nil {
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
	if got, want := string(data), "more"; got != want {
		t.Fatalf("log content = %q, want %q", got, want)
	}
	if len(data) > 5 {
		t.Fatalf("log exceeded cap after startup rollover: %d", len(data))
	}
	if n, err := writer.Write([]byte("again")); err != nil || n != 5 {
		t.Fatalf("second Write() = %d, %v", n, err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "again"; got != want {
		t.Fatalf("log stopped after startup rollover: %q", got)
	}
}

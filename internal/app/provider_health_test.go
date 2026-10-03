package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/health"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

func TestProviderEnabledAPIsResetHealth(t *testing.T) {
	for _, endpoint := range []string{"provider", "config"} {
		t.Run(endpoint, func(t *testing.T) {
			var upstreamStatus atomic.Int64
			upstreamStatus.Store(http.StatusOK)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(int(upstreamStatus.Load()))
				_, _ = w.Write([]byte(`{"result":"upstream"}`))
			}))
			defer upstream.Close()
			root := t.TempDir()
			path := filepath.Join(root, "config.json")
			cfg := config.Default()
			cfg.Service.ListenAddr = freeListenAddr(t)
			cfg.Auth.ManagementKey, cfg.Auth.GatewayKey = "management-key", "gateway-key"
			cfg.Providers = []config.ProviderConfig{{
				ID: "11111111-1111-4111-8111-111111111111", Name: "target", Enabled: true,
				BaseURL: upstream.URL, APIKey: "provider-key", Models: []string{"model"},
			}}
			writeAppConfig(t, path, cfg)
			application, err := New(Options{ConfigPath: path, LogOpener: discardAppLogOpener,
				HarnessHomeDir: func() (string, error) { return filepath.Join(root, "home"), nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			defer application.Close()
			call := func(method, path, key string, body []byte) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, path, bytes.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+key)
				req.Header.Set("X-Claude-Code-Session-Id", "session")
				rec := httptest.NewRecorder()
				application.server.Handler.ServeHTTP(rec, req)
				return rec
			}
			put := func(enabled bool) {
				t.Helper()
				next := application.manager.Config()
				next.Providers[0].Enabled = enabled
				path := "/api/v1/providers/" + next.Providers[0].ID
				body, err := json.Marshal(next.Providers[0])
				if err != nil {
					t.Fatal(err)
				}
				if endpoint == "config" {
					path, body = "/api/v1/config", marshalAppClientConfig(t, next)
				}
				if rec := call(http.MethodPut, path, cfg.Auth.ManagementKey, body); rec.Code != http.StatusOK {
					t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
				}
			}
			readHealth := func() (health.ProviderSnapshot, string) {
				t.Helper()
				rec := call(http.MethodGet, "/api/v1/provider-health", cfg.Auth.ManagementKey, nil)
				var result struct {
					Providers []struct {
						Static string `json:"static_availability"`
						Global struct {
							State string `json:"state"`
						} `json:"global_health"`
					} `json:"providers"`
				}
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil || len(result.Providers) != 1 {
					t.Fatalf("health GET = %d %s", rec.Code, rec.Body.String())
				}
				snapshot := application.health.Snapshot().Providers[0]
				if result.Providers[0].Global.State != string(snapshot.Global.State) {
					t.Fatal("API did not return current health")
				}
				return snapshot, result.Providers[0].Static
			}
			assertFresh := func(enabled bool) {
				t.Helper()
				got, static := readHealth()
				wantStatic := "disabled_provider"
				if enabled {
					wantStatic = "active"
				}
				if static != wantStatic || got.Global.State != scheduler.GlobalUnknown || got.Global.Diagnostic != (health.Diagnostic{}) {
					t.Fatalf("health was not reset: static=%s, %#v", static, got)
				}
				if len(got.Channels) != 1 || got.Channels[0].State != scheduler.ChannelUnknown || got.Channels[0].Diagnostic != (health.Diagnostic{}) {
					t.Fatalf("channel health was not reset: %#v", got.Channels)
				}
				if application.selector.ActiveAssignmentCount() != 0 {
					t.Fatal("enabled transition retained an old binding")
				}
			}
			gatewayCall := func(want int) {
				t.Helper()
				if rec := call(http.MethodPost, "/v1/messages", cfg.Auth.GatewayKey, []byte(`{"model":"model"}`)); rec.Code != want {
					t.Fatalf("gateway response = %d %s, want %d", rec.Code, rec.Body.String(), want)
				}
			}

			gatewayCall(http.StatusOK)
			oldSnapshot := application.manager.Snapshot()
			old, err := application.selector.Acquire(oldSnapshot,
				scheduler.StickyKey{SessionID: "late", Model: "model", RequestType: traffic.RequestTypeNormal},
				scheduler.NewRequestSelection(scheduler.DefaultAttemptPolicy()))
			if err != nil {
				t.Fatal(err)
			}
			upstreamStatus.Store(http.StatusUnauthorized)
			gatewayCall(http.StatusBadGateway)
			before, _ := readHealth()
			if before.Global.State != scheduler.GlobalCooldown || before.Global.ObservedFailures != 1 {
				t.Fatalf("expected old failure: %#v", before)
			}
			put(true)
			if unchanged, _ := readHealth(); !reflect.DeepEqual(unchanged, before) {
				t.Fatal("no-op update reset health")
			}
			put(false)
			assertFresh(false)
			gatewayCall(http.StatusNotFound)
			put(true)
			assertFresh(true)
			application.selector.Report(old, scheduler.Outcome{Class: scheduler.FailureGlobalImmediate, RawError: "late failure"})
			assertFresh(true)
			upstreamStatus.Store(http.StatusOK)
			gatewayCall(http.StatusOK)
			if got, _ := readHealth(); got.Global.State != scheduler.GlobalHealthy || application.selector.ActiveAssignmentCount() == 0 {
				t.Fatal("re-enabled provider was not routed and bound")
			}
			// No read or data request observes the intermediate disabled snapshot.
			put(false)
			put(true)
			assertFresh(true)
			gatewayCall(http.StatusOK)
		})
	}
}

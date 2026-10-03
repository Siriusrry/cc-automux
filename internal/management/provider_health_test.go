package management

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/health"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

func TestProviderEditsPreserveHealth(t *testing.T) {
	edits := []struct {
		name   string
		mutate func(*config.ProviderConfig)
	}{
		{"name", func(p *config.ProviderConfig) { p.Name = "renamed" }},
		{"models", func(p *config.ProviderConfig) { p.Models = []string{"model", "added"} }},
		{"priority", func(p *config.ProviderConfig) { p.Priority = 42 }},
		{"patches", func(p *config.ProviderConfig) { p.Patches = []string{patch.CLIProxyAPIClassifierSessionID} }},
		{"url", func(p *config.ProviderConfig) { p.BaseURL = "https://changed.example" }},
		{"key", func(p *config.ProviderConfig) { p.APIKey = "changed-key" }},
		{"auth", func(p *config.ProviderConfig) { p.UseXAPIKey = true }},
		{"tls", func(p *config.ProviderConfig) { p.TLS.Mode = config.TLSSkip }},
	}
	for _, endpoint := range []string{"provider", "config"} {
		for _, edit := range edits {
			t.Run(endpoint+"/"+edit.name, func(t *testing.T) {
				m := testManager(t, nil)
				cfg := m.Config()
				cfg.Providers = []config.ProviderConfig{{
					ID: "11111111-1111-4111-8111-111111111111", Name: "provider", Enabled: true,
					BaseURL: "https://provider.example", APIKey: "provider-key", Models: []string{"model", "removed"},
				}}
				if _, err := m.Apply(cfg); err != nil {
					t.Fatal(err)
				}
				store := health.NewDefault()
				selector, err := scheduler.NewSelector(store, scheduler.Options{})
				if err != nil {
					t.Fatal(err)
				}
				selector.Reconcile(m.Snapshot())
				handler := NewWithOptions(m, Options{Health: store, Selector: selector, Sync: func() { selector.Reconcile(m.Snapshot()) }})
				p := m.Snapshot().Providers()[0]
				key := scheduler.HealthKey{ProviderID: p.ID, Generation: p.Generation, Epoch: p.HealthEpoch, Model: "model", RequestType: traffic.RequestTypeNormal}
				global := store.Acquire(key, false)
				normal := store.Acquire(key, false)
				key.RequestType = traffic.RequestTypeClassifier
				classifier := store.Acquire(key, false)
				store.Report(global.Lease, scheduler.Outcome{Class: scheduler.FailureGlobalTransient, RawError: "connection failure"})
				store.Report(normal.Lease, scheduler.Outcome{Class: scheduler.FailureChannelImmediate, RawError: "model failure"})
				store.Report(classifier.Lease, scheduler.Outcome{Class: scheduler.FailureChannelTransient, RawError: "classifier failure"})
				read := func() providerHealthResponse {
					t.Helper()
					rec := request(handler, http.MethodGet, "/api/v1/provider-health", "Bearer management-key", "")
					var result providerHealthListResponse
					if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil || len(result.Providers) != 1 {
						t.Fatalf("health GET: %d %s", rec.Code, rec.Body.String())
					}
					return result.Providers[0]
				}
				before := read()
				if before.GlobalHealth.State != "degraded" || before.GlobalHealth.ObservedFailures != 1 {
					t.Fatal("initial global failure was not recorded")
				}
				next := m.Config()
				edit.mutate(&next.Providers[0])
				path := "/api/v1/providers/" + p.ID
				body, err := json.Marshal(next.Providers[0])
				if err != nil {
					t.Fatal(err)
				}
				if endpoint == "config" {
					path, body = "/api/v1/config", []byte(marshalManagementClientConfig(t, next))
				}
				if rec := request(handler, http.MethodPut, path, "Bearer management-key", string(body)); rec.Code != http.StatusOK {
					t.Fatalf("edit PUT: %d %s", rec.Code, rec.Body.String())
				}
				after := read()
				if !reflect.DeepEqual(after.GlobalHealth, before.GlobalHealth) {
					t.Fatalf("edit reset global health: %#v", after.GlobalHealth)
				}
				for _, old := range before.Channels {
					if old.Model != "model" {
						continue
					}
					found := false
					for _, current := range after.Channels {
						if current.Model == old.Model && current.RequestType == old.RequestType {
							found = true
							if !reflect.DeepEqual(current, old) {
								t.Fatalf("edit reset %s channel: %#v", old.RequestType, current)
							}
						}
					}
					if !found {
						t.Fatalf("edit removed %s channel", old.RequestType)
					}
				}
			})
		}
	}
}

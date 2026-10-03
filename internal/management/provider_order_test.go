package management

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/config"
)

func TestProviderOrderConditionalAtomicPermutationAndExactTiers(t *testing.T) {
	m := testManager(t, nil)
	var providers []config.ProviderConfig
	for i, priority := range []int64{math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64, math.MinInt64} {
		providers = append(providers, config.ProviderConfig{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), Name: fmt.Sprint(i), BaseURL: "https://provider.example", APIKey: "test-key", Models: []string{"m"}, Enabled: i%2 == 0, Priority: priority, TLS: config.TLSConfig{Mode: config.TLSSystem, CAFile: "retained.pem"}})
	}
	if _, err := m.Update(func(c *config.Config) error { c.Providers = providers; return nil }); err != nil {
		t.Fatal(err)
	}
	h := New(m)
	get := request(h, "GET", "/api/v1/provider-health", "Bearer management-key", "")
	var health providerHealthListResponse
	if err := json.Unmarshal(get.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if len(health.Tiers) != 3 || health.Tiers[0].Priority != "9223372036854775807" || health.Tiers[1].Priority != "9223372036854775806" || health.Tiers[2].Priority != "-9223372036854775808" {
		t.Fatalf("tiers: %+v", health.Tiers)
	}
	if get.Header().Get("ETag") != "" {
		t.Fatal("health representation must not use the configuration ETag")
	}
	tag := get.Header().Get("Config-ETag")
	if tag != m.Snapshot().ConfigETag() {
		t.Fatal("validator mismatch")
	}
	put := func(body, tag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/order", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer management-key")
		if tag != "" {
			req.Header.Set("If-Match", tag)
		}
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		return res
	}
	ids := []string{providers[2].ID, providers[0].ID}
	body, _ := json.Marshal(config.ProviderOrderUpdate{ProviderIDs: ids})
	before := m.Snapshot()
	for _, condition := range []string{"", "*", "W/" + tag, `"stale"`, tag + ", " + tag} {
		want := 412
		if condition == "" {
			want = 428
		}
		if r := put(string(body), condition); r.Code != want {
			t.Fatalf("condition %q: %d %s", condition, r.Code, r.Body.String())
		}
	}
	for _, invalid := range []string{`{"provider_ids":null}`, `{"provider_ids":[null]}`, `{"provider_ids":[],"extra":true}`, `{"provider_ids":[1]}`, `{"provider_ids":[],"provider_ids":[]}`, `null`} {
		if r := put(invalid, tag); r.Code != 400 {
			t.Fatalf("shape %s: %d", invalid, r.Code)
		}
	}
	for _, invalid := range [][]string{nil, {}, {ids[0]}, {ids[0], ids[0]}, {ids[0], providers[1].ID}, {"not-uuid"}, {"99999999-9999-4999-8999-999999999999"}} {
		value, _ := json.Marshal(config.ProviderOrderUpdate{ProviderIDs: invalid})
		r := put(string(value), tag)
		if r.Code != 422 && r.Code != 400 {
			t.Fatalf("ids %v: %d", invalid, r.Code)
		}
	}
	if before != m.Snapshot() {
		t.Fatal("rejected orders changed snapshot")
	}
	if r := put(string(body), tag); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	got := m.Config().Providers
	want := append([]config.ProviderConfig(nil), before.Config().Providers...)
	want[0], want[2] = want[2], want[0]
	if !reflect.DeepEqual(got, want) {
		t.Fatal("order changed provider data or other slots")
	}
	for _, p := range before.Providers() {
		for _, q := range m.Snapshot().Providers() {
			if p.ID == q.ID && p.Generation != q.Generation {
				t.Fatal("generation changed")
			}
		}
	}
	after := m.Snapshot()
	if r := put(string(body), after.ConfigETag()); r.Code != 200 || m.Snapshot() != after {
		t.Fatal("idempotent order wrote state")
	}
	if r := put(string(body), tag); r.Code != 412 {
		t.Fatal("stale order accepted")
	}
}

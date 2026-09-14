package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Siriusrry/cc-automux/internal/health"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

type retryTarget struct {
	priority       int64
	disabledHealth bool
	status         int
}
type retryFixture struct {
	snapshot *fakeSnapshot
	store    *health.Store
	selector *scheduler.Scheduler
	handler  *Handler
	events   *eventCollector
	nanos    atomic.Int64
	mu       sync.Mutex
	calls    []string
	problems []string
}

func newRetryFixture(t *testing.T, budget int, targets []retryTarget, reply func(string, http.ResponseWriter, *http.Request)) *retryFixture {
	t.Helper()
	f := &retryFixture{events: &eventCollector{}}
	f.nanos.Store(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		label := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		data, _ := io.ReadAll(r.Body)
		if string(data) != `{"model":"m","stream":false}` {
			f.mu.Lock()
			f.problems = append(f.problems, "replayed request changed")
			f.mu.Unlock()
		}
		f.mu.Lock()
		f.calls = append(f.calls, label)
		f.mu.Unlock()
		if reply != nil {
			reply(label, w, r)
			return
		}
		status := targets[int(label[0]-'A')].status
		w.WriteHeader(status)
		fmt.Fprint(w, label)
	}))
	t.Cleanup(func() {
		upstream.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.problems) > 0 {
			t.Errorf("upstream observations: %v", f.problems)
		}
	})
	f.snapshot = &fakeSnapshot{revision: 1, gatewayKey: "gateway", attemptPolicy: scheduler.AttemptPolicy{MaxAttempts: budget}}
	for i, target := range targets {
		label := string(rune('A' + i))
		p := compileTestProvider(t, fmt.Sprintf("%08d-1111-4111-8111-111111111111", i+1), label, upstream.URL, label, "m", false)
		p.Priority = target.priority
		p.DisableHealth = target.disabledHealth
		f.snapshot.providers = append(f.snapshot.providers, p)
	}
	var err error
	now := func() time.Time { return time.Unix(0, f.nanos.Load()) }
	f.store, err = health.New(scheduler.DefaultPolicy(), health.ClockFunc(now))
	if err != nil {
		t.Fatal(err)
	}
	f.selector, err = scheduler.New(f.store, scheduler.Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	f.selector.Reconcile(f.snapshot)
	f.handler = NewWithOptions(func() scheduler.Snapshot { return f.snapshot }, f.selector, Options{Recorder: f.events})
	t.Cleanup(func() { f.handler.Close() })
	return f
}

func (f *retryFixture) serve(session string) *httptest.ResponseRecorder {
	request := gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m","stream":false}`)
	if session != "" {
		request.Header.Set("X-Claude-Code-Session-Id", session)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, request)
	return w
}

func TestHealthDrivenRetrySequences(t *testing.T) {
	cases := []struct {
		name    string
		budget  int
		targets []retryTarget
		want    string
		status  int
	}{
		{"one without cooldown", 5, []retryTarget{{0, true, 503}}, "AAAAA", 503},
		{"peers without cooldown", 5, []retryTarget{{0, true, 503}, {0, true, 503}}, "ABABA", 503},
		{"one reaches cooldown", 5, []retryTarget{{0, false, 503}}, "AAA", 503},
		{"higher tier retains budget", 2, []retryTarget{{10, false, 503}, {0, false, 200}}, "AA", 503},
		{"cooldown permits lower tier", 5, []retryTarget{{10, false, 503}, {0, false, 200}}, "AAAB", 200},
		{"both higher peers must cool", 7, []retryTarget{{10, false, 503}, {10, false, 503}, {0, false, 200}}, "ABABABC", 200},
		{"disabled health blocks lower tier", 9, []retryTarget{{10, true, 503}, {10, false, 503}, {0, false, 200}}, "ABABABAAA", 503},
		{"immediate error without cooldown", 3, []retryTarget{{10, true, 401}, {0, false, 200}}, "AAA", 502},
		{"one call budget", 1, []retryTarget{{0, true, 503}, {0, true, 200}}, "A", 503},
		{"neutral terminates", 5, []retryTarget{{0, false, 400}, {0, false, 200}}, "A", 400},
	}
	for _, tc := range cases {
		for _, session := range []string{"", "session"} {
			t.Run(tc.name+"/"+session, func(t *testing.T) {
				f := newRetryFixture(t, tc.budget, tc.targets, nil)
				w := f.serve(session)
				f.mu.Lock()
				calls := strings.Join(f.calls, "")
				f.mu.Unlock()
				if calls != tc.want || w.Code != tc.status {
					t.Fatalf("calls=%s status=%d; want %s/%d", calls, w.Code, tc.want, tc.status)
				}
				events := waitGatewayEvents(t, f.events, len(tc.want)*2)
				forwards, results := map[int]Event{}, map[int]Event{}
				trace := events[0].TraceID
				for _, event := range events {
					if event.TraceID != trace || trace == "" {
						t.Fatalf("trace changed: %#v", events)
					}
					dst := results
					if event.Kind == EventForward {
						dst = forwards
					}
					if _, exists := dst[event.Attempt]; exists {
						t.Fatalf("duplicate attempt result: %#v", events)
					}
					dst[event.Attempt] = event
				}
				for i := 1; i <= len(tc.want); i++ {
					wantKind := EventFailover
					if i == len(tc.want) {
						wantKind = EventFailure
						if tc.status == 200 {
							wantKind = EventSuccess
						}
					}
					if forwards[i].ProviderName != string(tc.want[i-1]) || results[i].Kind != wantKind {
						t.Fatalf("attempt %d: %#v", i, events)
					}
				}
			})
		}
	}
}

func seedLowBinding(t *testing.T, f *retryFixture) {
	t.Helper()
	key := scheduler.StickyKey{SessionID: "session", Model: "m", RequestType: traffic.RequestTypeNormal}
	for _, item := range f.snapshot.providers {
		if item.Priority == 0 {
			continue
		}
		lease := f.store.Acquire(scheduler.HealthKey{ProviderID: item.ID, Generation: item.Generation, Model: "m", RequestType: traffic.RequestTypeNormal}, false)
		f.store.Report(lease.Lease, scheduler.Outcome{Class: scheduler.FailureChannelImmediate})
	}
	lease, err := f.selector.Acquire(f.snapshot, key, scheduler.NewRequestSelection(scheduler.DefaultAttemptPolicy()))
	if err != nil || lease.Provider.Name != "B" {
		t.Fatalf("seed binding: %#v %v", lease, err)
	}
	f.selector.Report(lease, scheduler.Outcome{Class: scheduler.FailureNone})
}

func TestRecoveredHighTierKeepsLowBindingUntilSuccess(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status, budget int
		disable        bool
		want           string
	}{
		{"success", 200, 3, false, "A"}, {"failed probe", 503, 1, false, "B"}, {"retry without cooldown", 503, 5, true, "B"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f *retryFixture
			f = newRetryFixture(t, tc.budget, []retryTarget{{10, false, tc.status}, {0, false, 200}}, func(label string, w http.ResponseWriter, r *http.Request) {
				bindings := f.selector.Assignments("")
				if len(bindings) != 1 || bindings[0].ProviderID != f.snapshot.providers[1].ID {
					f.mu.Lock()
					f.problems = append(f.problems, "selection moved binding")
					f.mu.Unlock()
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, label)
			})
			seedLowBinding(t, f)
			if tc.disable {
				next := &fakeSnapshot{revision: 2, gatewayKey: "gateway", attemptPolicy: f.snapshot.attemptPolicy, providers: append([]*provider.CompiledProvider(nil), f.snapshot.providers...)}
				next.providers[0] = next.providers[0].Clone()
				next.providers[0].DisableHealth = true
				f.snapshot = next
				f.selector.Reconcile(next)
			} else {
				f.nanos.Add(int64(time.Minute))
			}
			f.serve("session")
			bindings := f.selector.Assignments("")
			if len(bindings) != 1 || bindings[0].ProviderID != f.snapshot.providers[int(tc.want[0]-'A')].ID {
				t.Fatalf("binding: %#v", bindings)
			}
			f.mu.Lock()
			calls := append([]string(nil), f.calls...)
			f.mu.Unlock()
			expected := []string{"A"}
			if tc.disable {
				expected = []string{"A", "A", "A", "A", "A"}
			}
			if !reflect.DeepEqual(calls, expected) {
				t.Fatalf("calls=%v", calls)
			}
		})
	}
}

func TestSameTargetLateBodyCannotReplaceNewObservation(t *testing.T) {
	release := make(chan struct{})
	var count atomic.Int32
	f := newRetryFixture(t, 2, []retryTarget{{0, true, 503}}, func(_ string, w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		w.WriteHeader(503)
		if n == 1 {
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		fmt.Fprintf(w, "failure-%d", n)
	})
	f.serve("session")
	close(release)
	waitGatewayEvents(t, f.events, 4)
	p := f.snapshot.providers[0]
	state, _ := f.store.ProviderSnapshot(p.ID, p.Generation)
	if len(state.Channels) != 1 || state.Channels[0].LastError != "failure-2" || state.Channels[0].ObservedFailures != 2 {
		t.Fatalf("latest diagnostic: %#v", state)
	}
}

func TestHighestTierProbeAndRecovery(t *testing.T) {
	for _, peer := range []bool{false, true} {
		t.Run(fmt.Sprint("available peer ", peer), func(t *testing.T) {
			targets := []retryTarget{{10, false, 503}, {0, false, 200}}
			if peer {
				targets = append(targets, retryTarget{10, true, 503})
			}
			f := newRetryFixture(t, 5, targets, nil)
			a := f.snapshot.providers[0]
			key := scheduler.HealthKey{ProviderID: a.ID, Generation: a.Generation, Model: "m", RequestType: traffic.RequestTypeNormal}
			first := f.store.Acquire(key, false)
			f.store.Report(first.Lease, scheduler.Outcome{Class: scheduler.FailureChannelImmediate})
			f.nanos.Add(int64(time.Minute))
			probe := f.store.Acquire(key, false)
			if !probe.Available || !probe.Lease.ChannelProbe {
				t.Fatal("missing probe lease")
			}
			defer f.store.Report(probe.Lease, scheduler.Outcome{Class: scheduler.FailureClientCanceled})
			w := f.serve("")
			f.mu.Lock()
			got := strings.Join(f.calls, "")
			f.mu.Unlock()
			want := ""
			if peer {
				want = "CCCCC"
			}
			if got != want || w.Code != 503 {
				t.Fatalf("calls=%s status=%d", got, w.Code)
			}
		})
	}
	t.Run("higher tier recovers during lower call", func(t *testing.T) {
		var f *retryFixture
		f = newRetryFixture(t, 2, []retryTarget{{10, false, 503}, {0, true, 503}}, func(label string, w http.ResponseWriter, _ *http.Request) {
			if label == "B" {
				f.nanos.Add(int64(time.Minute))
			}
			w.WriteHeader(503)
			fmt.Fprint(w, label)
		})
		a := f.snapshot.providers[0]
		lease := f.store.Acquire(scheduler.HealthKey{ProviderID: a.ID, Generation: a.Generation, Model: "m", RequestType: traffic.RequestTypeNormal}, false)
		f.store.Report(lease.Lease, scheduler.Outcome{Class: scheduler.FailureChannelImmediate})
		f.serve("")
		f.mu.Lock()
		got := strings.Join(f.calls, "")
		f.mu.Unlock()
		if got != "BA" {
			t.Fatalf("calls=%s", got)
		}
	})
	t.Run("other channel does not block", func(t *testing.T) {
		f := newRetryFixture(t, 2, []retryTarget{{10, false, 503}, {0, true, 200}}, nil)
		a := f.snapshot.providers[0]
		lease := f.store.Acquire(scheduler.HealthKey{ProviderID: a.ID, Generation: a.Generation, Model: "m", RequestType: traffic.RequestTypeClassifier}, false)
		f.store.Report(lease.Lease, scheduler.Outcome{Class: scheduler.FailureChannelImmediate})
		f.serve("")
		f.mu.Lock()
		got := strings.Join(f.calls, "")
		f.mu.Unlock()
		if got != "AA" {
			t.Fatalf("calls=%s", got)
		}
	})
}

func TestRecoveredTargetIncompleteSuccessDoesNotMigrate(t *testing.T) {
	f := newRetryFixture(t, 5, []retryTarget{{10, false, 200}, {0, false, 200}}, func(_ string, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "20")
		w.WriteHeader(200)
		fmt.Fprint(w, "short")
	})
	seedLowBinding(t, f)
	f.nanos.Add(int64(time.Minute))
	server := httptest.NewServer(f.handler)
	defer server.Close()
	request, _ := http.NewRequest(http.MethodPost, server.URL+MessagesPath, strings.NewReader(`{"model":"m","stream":false}`))
	request.Header.Set("Authorization", "Bearer gateway")
	request.Header.Set("X-Claude-Code-Session-Id", "session")
	response, err := server.Client().Do(request)
	if err == nil {
		_, err = io.ReadAll(response.Body)
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("expected interrupted response")
	}
	waitGatewayEvents(t, f.events, 2)
	bindings := f.selector.Assignments("")
	if len(bindings) != 1 || bindings[0].ProviderID != f.snapshot.providers[1].ID {
		t.Fatalf("incomplete response migrated: %#v", bindings)
	}
	f.mu.Lock()
	got := strings.Join(f.calls, "")
	f.mu.Unlock()
	if got != "A" {
		t.Fatalf("retried after 2xx: %s", got)
	}
}

func TestEnablingHighTargetPreservesExistingLowBindingOnFailure(t *testing.T) {
	f := newRetryFixture(t, 5, []retryTarget{{10, true, 503}, {0, false, 200}}, nil)
	a := f.snapshot.providers[0]
	disabled := &fakeSnapshot{revision: 2, gatewayKey: "gateway", attemptPolicy: f.snapshot.attemptPolicy, providers: append([]*provider.CompiledProvider(nil), f.snapshot.providers...)}
	disabled.providers[0] = a.Clone()
	disabled.providers[0].Enabled = false
	f.snapshot = disabled
	f.selector.Reconcile(disabled)
	key := scheduler.StickyKey{SessionID: "session", Model: "m", RequestType: traffic.RequestTypeNormal}
	lease, err := f.selector.Acquire(f.snapshot, key, scheduler.NewRequestSelection(scheduler.DefaultAttemptPolicy()))
	if err != nil {
		t.Fatal(err)
	}
	f.selector.Report(lease, scheduler.Outcome{Class: scheduler.FailureNone})
	next := &fakeSnapshot{revision: 3, gatewayKey: "gateway", attemptPolicy: f.snapshot.attemptPolicy, providers: append([]*provider.CompiledProvider(nil), f.snapshot.providers...)}
	next.providers[0] = a.Clone()
	next.providers[0].Enabled = true
	f.snapshot = next
	f.selector.Reconcile(next)
	f.serve("session")
	bindings := f.selector.Assignments("")
	if len(bindings) != 1 || bindings[0].ProviderID != next.providers[1].ID {
		t.Fatalf("binding=%#v", bindings)
	}
	f.mu.Lock()
	got := strings.Join(f.calls, "")
	f.mu.Unlock()
	if got != "AAAAA" {
		t.Fatalf("calls=%s", got)
	}
}

func TestOtherModelHighProviderDoesNotBlock(t *testing.T) {
	f := newRetryFixture(t, 3, []retryTarget{{10, true, 503}, {0, true, 200}}, nil)
	next := &fakeSnapshot{revision: 2, gatewayKey: "gateway", attemptPolicy: f.snapshot.attemptPolicy, providers: append([]*provider.CompiledProvider(nil), f.snapshot.providers...)}
	original := next.providers[0]
	next.providers[0] = compileTestProvider(t, original.ID, original.Name, original.BaseURL.String(), original.APIKey, "other", false)
	next.providers[0].Priority = 10
	f.snapshot = next
	f.selector.Reconcile(next)
	response := f.serve("")
	if response.Code != 200 {
		t.Fatalf("status=%d", response.Code)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.calls, "") != "B" {
		t.Fatalf("calls=%v", f.calls)
	}
}

func TestLowBindingSurvivesMixedHighTierFailures(t *testing.T) {
	f := newRetryFixture(t, 9, []retryTarget{{10, false, 503}, {0, false, 200}, {10, false, 503}}, nil)
	seedLowBinding(t, f)
	next := &fakeSnapshot{revision: 2, gatewayKey: "gateway", attemptPolicy: f.snapshot.attemptPolicy, providers: append([]*provider.CompiledProvider(nil), f.snapshot.providers...)}
	next.providers[0] = next.providers[0].Clone()
	next.providers[0].DisableHealth = true
	f.snapshot = next
	f.selector.Reconcile(next)
	f.nanos.Add(int64(time.Minute))
	f.serve("session")
	bindings := f.selector.Assignments("")
	if len(bindings) != 1 || bindings[0].ProviderID != next.providers[1].ID {
		t.Fatalf("bindings=%#v", bindings)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := strings.Join(f.calls, ""); got != "ACAAAAAAA" {
		t.Fatalf("calls=%s", got)
	}
}

func TestConfirmedSuccessMigrationSurvivesClientCancellation(t *testing.T) {
	f := newRetryFixture(t, 3, []retryTarget{{10, false, 200}, {0, false, 200}}, func(_ string, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "event: message_stop\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	seedLowBinding(t, f)
	f.nanos.Add(int64(time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m","stream":false}`).WithContext(ctx)
	request.Header.Set("X-Claude-Code-Session-Id", "session")
	writer := &fixedWriteRecorder{header: make(http.Header), onWrite: cancel}
	f.handler.ServeHTTP(writer, request)
	events := waitGatewayEvents(t, f.events, 3)
	bindings := f.selector.Assignments("")
	if len(bindings) != 1 || bindings[0].ProviderID != f.snapshot.providers[0].ID || events[1].Kind != EventSuccess || events[2].Kind != EventCanceled {
		t.Fatalf("bindings=%#v events=%#v", bindings, events)
	}
}

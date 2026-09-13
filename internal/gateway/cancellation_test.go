package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
)

type cancellationSelector struct {
	*fakeSelector
	cancel context.CancelFunc
	point  string
	calls  int
}

func (s *cancellationSelector) Acquire(snapshot scheduler.Snapshot, key scheduler.StickyKey, excluded map[string]struct{}) (scheduler.AttemptLease, error) {
	s.calls++
	if s.calls == 2 && s.point == "acquire_error" {
		s.cancel()
		return scheduler.AttemptLease{}, errors.New("no candidate")
	}
	lease, err := s.fakeSelector.Acquire(snapshot, key, excluded)
	if s.calls == 2 && s.point == "acquired" {
		s.cancel()
	}
	return lease, err
}
func (s *cancellationSelector) Report(lease scheduler.AttemptLease, outcome scheduler.Outcome) (scheduler.HealthUpdate, uint64) {
	update, id := s.fakeSelector.Report(lease, outcome)
	if s.calls == 1 && s.point == "between" {
		s.cancel()
	}
	return update, id
}

func TestCancellationWhileSelectingNextAttemptIsRecorded(t *testing.T) {
	for _, point := range []string{"between", "acquire_error", "acquired"} {
		t.Run(point, func(t *testing.T) {
			release := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(429)
				w.(http.Flusher).Flush()
				select {
				case <-release:
					_, _ = io.WriteString(w, "original failure")
				case <-r.Context().Done():
				}
			}))
			defer upstream.Close()
			first := compileTestProvider(t, "11111111-1111-4111-8111-111111111111", "one", upstream.URL, "key", "m", false)
			second := compileTestProvider(t, "22222222-2222-4222-8222-222222222222", "two", upstream.URL, "key", "m", false)
			snapshot := &fakeSnapshot{revision: 1, gatewayKey: "gateway", providers: []*provider.CompiledProvider{first, second}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			selector := &cancellationSelector{fakeSelector: &fakeSelector{leases: []scheduler.AttemptLease{leaseFor(first, "m"), leaseFor(second, "m")}}, cancel: cancel, point: point}
			events := &eventCollector{}
			h := NewWithOptions(func() scheduler.Snapshot { return snapshot }, selector, Options{Recorder: events})
			defer h.Close()
			h.ServeHTTP(httptest.NewRecorder(), gatewayRequest(http.MethodPost, MessagesPath, "Bearer gateway", `{"model":"m"}`).WithContext(ctx))
			close(release)
			got := waitGatewayEvents(t, events, 3)
			var canceled, failure Event
			for _, event := range got {
				if event.Kind == EventCanceled {
					canceled = event
				}
				if event.Kind == EventFailure {
					failure = event
				}
			}
			if canceled.Attempt != 2 || canceled.CancelPhase != cancelBeforeUpstream || failure.RawError != "original failure" || failure.NextProviderID != "" {
				t.Fatalf("events=%#v", got)
			}
			_, reports := selector.snapshot()
			if point == "acquired" {
				if canceled.ProviderID != second.ID || len(reports) != 2 || reports[1].Class != scheduler.FailureClientCanceled {
					t.Fatalf("canceled=%#v reports=%#v", canceled, reports)
				}
			} else if canceled.ProviderID != "" || len(reports) != 1 {
				t.Fatalf("canceled=%#v reports=%#v", canceled, reports)
			}
		})
	}
}

package logs

import (
	"context"
	"net/url"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestTraceFilterMatchesHistoryAndLive(t *testing.T) {
	timestamp := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	lines := []string{logLine(timestamp, 1, `"trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","kind":"forward"`), logLine(timestamp, 2, `"trace_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","kind":"forward"`), logLine(timestamp, 3, `"trace_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","kind":"failover"`)}
	parameters, err := ParseParameters(url.Values{"trace_id": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeLines(t, filepath.Join(dir, ActiveFileName), lines...)
	page, err := directoryReader(t, dir).Query(context.Background(), HistoryQuery{Filter: parameters.Filter, Limit: 10})
	if err != nil || !reflect.DeepEqual(itemSeqs(page.Items), []uint64{3, 1}) {
		t.Fatalf("history=%#v err=%v", page, err)
	}
	broker := NewBroker()
	defer broker.Close()
	subscription, err := broker.Subscribe(parameters.Filter)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	for _, line := range lines {
		broker.Publish([]byte(line))
	}
	broker.Flush()
	for _, seq := range []uint64{1, 3} {
		if got := receiveMessage(t, subscription); got.Record.Seq != seq {
			t.Fatalf("live=%#v", got)
		}
	}
	select {
	case extra := <-subscription.Messages():
		t.Fatalf("unrelated trace delivered: %#v", extra)
	default:
	}
}

func TestRetryKindAndTraceFilterMatchHistoryAndLive(t *testing.T) {
	timestamp := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	lines := []string{logLine(timestamp, 1, `"trace_id":"a","kind":"retry","attempt":1,"next_attempt":2`), logLine(timestamp, 2, `"trace_id":"a","kind":"failover"`), logLine(timestamp, 3, `"trace_id":"b","kind":"retry"`), logLine(timestamp, 4, `"trace_id":"a","kind":"retry","attempt":2,"next_attempt":3`)}
	for _, withTrace := range []bool{false, true} {
		query := url.Values{"kind": {"retry"}}
		history := []uint64{4, 3, 1}
		live := []uint64{1, 3, 4}
		if withTrace {
			query.Set("trace_id", "a")
			history = []uint64{4, 1}
			live = []uint64{1, 4}
		}
		parameters, err := ParseParameters(query, ParseOptions{})
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		writeLines(t, filepath.Join(dir, ActiveFileName), lines...)
		page, err := directoryReader(t, dir).Query(context.Background(), HistoryQuery{Filter: parameters.Filter, Limit: 10})
		if err != nil || !reflect.DeepEqual(itemSeqs(page.Items), history) {
			t.Fatalf("history=%#v err=%v", page, err)
		}
		broker := NewBroker()
		subscription, err := broker.Subscribe(parameters.Filter)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range lines {
			broker.Publish([]byte(line))
		}
		broker.Flush()
		for _, seq := range live {
			if got := receiveMessage(t, subscription); got.Record.Seq != seq {
				t.Fatalf("live=%#v", got)
			}
		}
		select {
		case extra := <-subscription.Messages():
			t.Fatalf("unexpected record: %#v", extra)
		default:
		}
		subscription.Close()
		broker.Close()
	}
}

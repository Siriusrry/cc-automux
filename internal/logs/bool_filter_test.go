package logs

import (
	"context"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestBooleanFilterHistoryAndLive(t *testing.T) {
	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	lines := []string{logLine(base, 1, `"stream":false`), logLine(base, 2, `"stream":true`), logLine(base, 3, `"kind":"success"`)}
	dir := t.TempDir()
	writeLines(t, filepath.Join(dir, ActiveFileName), lines...)
	reader := directoryReader(t, dir)
	for _, test := range []struct {
		values []string
		want   []uint64
	}{
		{nil, []uint64{3, 2, 1}}, {[]string{"true"}, []uint64{2}}, {[]string{"false"}, []uint64{1}}, {[]string{"true", "false"}, []uint64{2, 1}},
	} {
		query := url.Values{}
		if test.values != nil {
			query["stream"] = test.values
		}
		params, err := ParseParameters(query, ParseOptions{})
		if err != nil {
			t.Fatal(err)
		}
		page, err := reader.Query(context.Background(), HistoryQuery{Filter: params.Filter, Limit: 10})
		if err != nil || page.SkippedMalformed != 0 || !reflect.DeepEqual(itemSeqs(page.Items), test.want) {
			t.Fatalf("history = %#v, %v", page, err)
		}
		broker := NewBroker()
		subscription, err := broker.Subscribe(params.Filter)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range lines {
			broker.Publish([]byte(line))
		}
		broker.Flush()
		for i := len(test.want) - 1; i >= 0; i-- {
			msg := receiveMessage(t, subscription)
			if msg.Record.Seq != test.want[i] {
				t.Fatalf("live = %#v", msg)
			}
		}
		select {
		case extra := <-subscription.Messages():
			t.Fatalf("unexpected message: %#v", extra)
		default:
		}
		subscription.Close()
		broker.Close()
	}
	for _, value := range []string{"1", "0", "TRUE", "False", "null", ""} {
		if _, err := ParseParameters(url.Values{"stream": {value}}, ParseOptions{}); !IsValidationError(err) {
			t.Fatalf("accepted %q: %v", value, err)
		}
	}
	for _, value := range []string{`"true"`, `1`, `null`, `{}`} {
		if _, err := ParseRecord([]byte(logLine(base, 1, `"stream":`+value))); err == nil {
			t.Fatalf("accepted non-boolean %s", strconv.Quote(value))
		}
	}
}

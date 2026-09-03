package logs

import (
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"
)

func brokerRecord(t *testing.T, seq uint64, level string) Record {
	t.Helper()
	return mustRecord(t, fmt.Sprintf(`{"time":"2026-09-03T12:00:00Z","level":%q,"msg":"gateway","seq":%d,"kind":"failure"}`, level, seq))
}

func TestBrokerFiltersAndKeepsRawRecordBytes(t *testing.T) {
	broker := NewBroker()
	parameters, err := ParseParameters(url.Values{"level": {"ERROR"}}, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	subscription, err := broker.Subscribe(parameters.Filter)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()

	broker.PublishRecord(brokerRecord(t, 1, "INFO"))
	raw := []byte(`{"time":"2026-09-03T12:00:00Z","level":"ERROR","msg":"gateway","seq":5,"kind":"failure","raw_error":"quoted=\"value\"\nline"}`)
	if err := broker.Publish(append(append([]byte(nil), raw...), '\n')); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-subscription.Messages():
		if message.Kind != MessageRecord || message.Record.Seq != 5 || !reflect.DeepEqual(message.Record.Bytes(), raw) {
			t.Fatalf("message = %#v raw=%q", message, message.Record.Bytes())
		}
	case <-time.After(time.Second):
		t.Fatal("matching record was not delivered")
	}
	select {
	case unexpected := <-subscription.Messages():
		t.Fatalf("filter gap produced an extra message: %#v", unexpected)
	default:
	}
}

func TestBrokerDropsWithoutBlockingAndReportsOnlyRealDrops(t *testing.T) {
	broker := NewBroker()
	subscription, err := broker.Subscribe(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	record := brokerRecord(t, 1, "INFO")
	for seq := uint64(1); seq <= SubscriberBuffer; seq++ {
		record.Seq = seq
		broker.PublishRecord(record)
	}
	started := time.Now()
	for seq := uint64(SubscriberBuffer + 1); seq <= SubscriberBuffer+10_000; seq++ {
		record.Seq = seq
		broker.PublishRecord(record)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("blocked subscriber delayed publisher for %v", elapsed)
	}

	// Free one slot. The next publication uses it for the accumulated dropped
	// notice and counts that publication as a new drop because no second slot is
	// available.
	<-subscription.Messages()
	record.Seq = 20_000
	broker.PublishRecord(record)
	for i := 1; i < SubscriberBuffer; i++ {
		message := <-subscription.Messages()
		if message.Kind != MessageRecord {
			t.Fatalf("buffered message %d = %#v", i, message)
		}
	}
	notice := <-subscription.Messages()
	if notice.Kind != MessageDropped || notice.Dropped != 10_000 {
		t.Fatalf("first dropped notice = %#v", notice)
	}

	// With room available, the next publish first reports the one record lost
	// above and then delivers the current record.
	broker.PublishRecord(brokerRecord(t, 20_001, "INFO"))
	notice = <-subscription.Messages()
	if notice.Kind != MessageDropped || notice.Dropped != 1 {
		t.Fatalf("second dropped notice = %#v", notice)
	}
	delivered := <-subscription.Messages()
	if delivered.Kind != MessageRecord || delivered.Record.Seq != 20_001 {
		t.Fatalf("recovered record = %#v", delivered)
	}
}

func TestBrokerSubscriptionLimitMultipleSubscribersAndLifecycle(t *testing.T) {
	broker := NewBroker()
	subscriptions := make([]*Subscription, 0, MaximumStreams)
	for i := 0; i < MaximumStreams; i++ {
		subscription, err := broker.Subscribe(Filter{})
		if err != nil {
			t.Fatal(err)
		}
		subscriptions = append(subscriptions, subscription)
	}
	if broker.SubscriberCount() != MaximumStreams {
		t.Fatalf("subscriber count = %d", broker.SubscriberCount())
	}
	if _, err := broker.Subscribe(Filter{}); !errors.Is(err, ErrStreamUnavailable) {
		t.Fatalf("ninth subscription error = %v", err)
	}
	broker.PublishRecord(brokerRecord(t, 1, "INFO"))
	for i, subscription := range subscriptions {
		message := <-subscription.Messages()
		if message.Kind != MessageRecord || message.Record.Seq != 1 {
			t.Fatalf("subscriber %d message = %#v", i, message)
		}
	}
	subscriptions[0].Close()
	subscriptions[0].Close()
	if broker.SubscriberCount() != MaximumStreams-1 {
		t.Fatalf("count after unsubscribe = %d", broker.SubscriberCount())
	}
	replacement, err := broker.Subscribe(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	broker.Close()
	broker.Close()
	if broker.SubscriberCount() != 0 {
		t.Fatalf("count after close = %d", broker.SubscriberCount())
	}
	for _, subscription := range append(subscriptions[1:], replacement) {
		if _, open := <-subscription.Messages(); open {
			t.Fatal("subscription channel remained open")
		}
	}
	if _, err := broker.Subscribe(Filter{}); !errors.Is(err, ErrStreamUnavailable) {
		t.Fatalf("subscription after close error = %v", err)
	}
}

func TestBrokerRejectsMalformedPublication(t *testing.T) {
	broker := NewBroker()
	if err := broker.Publish([]byte("not-json\n")); err != nil {
		t.Fatalf("publication without subscribers did unnecessary parsing: %v", err)
	}
	subscription, err := broker.Subscribe(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	if err := broker.Publish([]byte("not-json\n")); err == nil {
		t.Fatal("malformed publication was accepted")
	}
}

func TestBrokerConcurrentPublishAndUnsubscribe(t *testing.T) {
	broker := NewBroker()
	subscription, err := broker.Subscribe(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	record := brokerRecord(t, 1, "INFO")
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for j := 0; j < 1_000; j++ {
				broker.PublishRecord(record)
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		<-start
		subscription.Close()
	}()
	close(start)
	wait.Wait()
	broker.Close()
}

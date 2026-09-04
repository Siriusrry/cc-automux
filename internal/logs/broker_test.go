package logs

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func brokerLine(seq uint64, level string) []byte {
	return []byte(fmt.Sprintf(`{"time":"2026-09-03T12:00:00Z","level":%q,"msg":"gateway","seq":%d,"kind":"failure"}`, level, seq))
}

func receiveMessage(t *testing.T, subscription *Subscription) Message {
	t.Helper()
	select {
	case message := <-subscription.Messages():
		return message
	case <-time.After(5 * time.Second):
		t.Fatal("no message was delivered")
	}
	return Message{}
}

func TestBrokerFiltersAndKeepsRawRecordBytes(t *testing.T) {
	broker := NewBroker()
	defer broker.Close()
	parameters, err := ParseParameters(url.Values{"level": {"ERROR"}}, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	subscription, err := broker.Subscribe(parameters.Filter)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()

	broker.Publish(brokerLine(1, "INFO"))
	raw := []byte(`{"time":"2026-09-03T12:00:00Z","level":"ERROR","msg":"gateway","seq":5,"kind":"failure","raw_error":"quoted=\"value\"\nline"}`)
	// Publish copies the line, so the caller's buffer may be reused afterwards.
	buffer := append(append([]byte(nil), raw...), '\n')
	broker.Publish(buffer)
	for i := range buffer {
		buffer[i] = 'x'
	}
	message := receiveMessage(t, subscription)
	if message.Kind != MessageRecord || message.Record.Seq != 5 || !reflect.DeepEqual(message.Record.Bytes(), raw) {
		t.Fatalf("message = %#v raw=%q", message, message.Record.Bytes())
	}
	broker.Flush()
	select {
	case unexpected := <-subscription.Messages():
		t.Fatalf("filter gap produced an extra message: %#v", unexpected)
	default:
	}
}

func TestBrokerPreservesPublicationOrder(t *testing.T) {
	broker := NewBroker()
	defer broker.Close()
	subscription, err := broker.Subscribe(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	for seq := uint64(1); seq <= 200; seq++ {
		broker.Publish(brokerLine(seq, "INFO"))
	}
	broker.Flush()
	for seq := uint64(1); seq <= 200; seq++ {
		message := receiveMessage(t, subscription)
		if message.Kind != MessageRecord || message.Record.Seq != seq {
			t.Fatalf("message for seq %d = %#v", seq, message)
		}
	}
}

func TestBrokerDropsWithoutBlockingAndReportsOnlyRealDrops(t *testing.T) {
	broker := NewBroker()
	defer broker.Close()
	subscription, err := broker.Subscribe(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	for seq := uint64(1); seq <= SubscriberBuffer; seq++ {
		broker.Publish(brokerLine(seq, "INFO"))
	}
	broker.Flush()
	started := time.Now()
	for seq := uint64(SubscriberBuffer + 1); seq <= SubscriberBuffer+10_000; seq++ {
		broker.Publish(brokerLine(seq, "INFO"))
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("blocked subscriber delayed publisher for %v", elapsed)
	}
	broker.Flush()

	// Free one slot. The next publication uses it for the accumulated dropped
	// notice and counts that publication as a new drop because no second slot is
	// available.
	<-subscription.Messages()
	broker.Publish(brokerLine(20_000, "INFO"))
	broker.Flush()
	for i := 1; i < SubscriberBuffer; i++ {
		message := receiveMessage(t, subscription)
		if message.Kind != MessageRecord {
			t.Fatalf("buffered message %d = %#v", i, message)
		}
	}
	notice := receiveMessage(t, subscription)
	if notice.Kind != MessageDropped || notice.Dropped != 10_000 {
		t.Fatalf("first dropped notice = %#v", notice)
	}

	// With room available, the next publish first reports the one record lost
	// above and then delivers the current record.
	broker.Publish(brokerLine(20_001, "INFO"))
	notice = receiveMessage(t, subscription)
	if notice.Kind != MessageDropped || notice.Dropped != 1 {
		t.Fatalf("second dropped notice = %#v", notice)
	}
	delivered := receiveMessage(t, subscription)
	if delivered.Kind != MessageRecord || delivered.Record.Seq != 20_001 {
		t.Fatalf("recovered record = %#v", delivered)
	}
}

// TestBrokerPublishReturnsBeforeDispatch is the property the queue exists for:
// the cost of Publish does not depend on how long the record takes to parse
// and summarize. A record large enough to take a noticeable time to parse is
// still in flight when Publish returns, and only reaches the subscriber later.
func TestBrokerPublishReturnsBeforeDispatch(t *testing.T) {
	broker := NewBroker()
	defer broker.Close()
	subscription, err := broker.Subscribe(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()

	body, err := json.Marshal(strings.Repeat("x", 16<<20))
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(`{"time":"2026-09-03T12:00:00Z","level":"ERROR","msg":"gateway","seq":1,"kind":"failure","raw_error":` + string(body) + "}\n")
	started := time.Now()
	broker.Publish(line)
	publishLatency := time.Since(started)
	if len(subscription.Messages()) != 0 {
		t.Fatal("the record was parsed and delivered before Publish returned")
	}
	broker.Flush()
	dispatchLatency := time.Since(started)
	message := receiveMessage(t, subscription)
	if message.Kind != MessageRecord || len(message.Record.Bytes()) >= len(line) {
		t.Fatalf("published %d bytes of a %d byte line", len(message.Record.Bytes()), len(line))
	}
	if publishLatency > dispatchLatency/2 {
		t.Fatalf("Publish took %v of the %v needed to dispatch the record", publishLatency, dispatchLatency)
	}
}

func TestBrokerQueueOverflowCountsAsDroppedForEverySubscriber(t *testing.T) {
	line := brokerLine(1, "INFO")
	broker := newBroker(len(line) + len(line)/2)
	defer broker.Close()
	gate := make(chan struct{})
	var once sync.Once
	broker.beforeDispatch = func() {
		once.Do(func() { <-gate })
	}
	matching, err := broker.Subscribe(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer matching.Close()
	parameters, err := ParseParameters(url.Values{"level": {"ERROR"}}, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	nonMatching, err := broker.Subscribe(parameters.Filter)
	if err != nil {
		t.Fatal(err)
	}
	defer nonMatching.Close()

	// The first line is dequeued and parked in front of the gate. The second
	// fits in the budget; the third does not and is dropped before it is parsed,
	// so both subscribers are told, including the one it would not have matched.
	// The drop is counted the moment it happens, so the notice rides on the next
	// delivery, which is the parked first record.
	broker.Publish(brokerLine(1, "INFO"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		broker.queueMu.Lock()
		queued := len(broker.queue)
		broker.queueMu.Unlock()
		if queued == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dispatcher did not take the first line")
		}
		time.Sleep(time.Millisecond)
	}
	broker.Publish(brokerLine(2, "INFO"))
	broker.Publish(brokerLine(3, "INFO"))
	close(gate)
	broker.Flush()

	notice := receiveMessage(t, matching)
	if notice.Kind != MessageDropped || notice.Dropped != 1 {
		t.Fatalf("overflow notice = %#v", notice)
	}
	first := receiveMessage(t, matching)
	if first.Kind != MessageRecord || first.Record.Seq != 1 {
		t.Fatalf("first message = %#v", first)
	}
	second := receiveMessage(t, matching)
	if second.Kind != MessageRecord || second.Record.Seq != 2 {
		t.Fatalf("second message = %#v", second)
	}
	select {
	case unexpected := <-nonMatching.Messages():
		t.Fatalf("non-matching subscriber received %#v before any matching record", unexpected)
	default:
	}
	// The drop stays pending until a matching record gives it a slot to ride on.
	broker.Publish(brokerLine(4, "ERROR"))
	notice = receiveMessage(t, nonMatching)
	if notice.Kind != MessageDropped || notice.Dropped != 1 {
		t.Fatalf("non-matching overflow notice = %#v", notice)
	}
	if delivered := receiveMessage(t, nonMatching); delivered.Kind != MessageRecord || delivered.Record.Seq != 4 {
		t.Fatalf("non-matching delivered = %#v", delivered)
	}
}

func TestBrokerBoundsPushedRecordsAndFiltersOnCompleteValues(t *testing.T) {
	broker := NewBroker()
	defer broker.Close()
	subscription, err := broker.Subscribe(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()

	body := strings.Repeat("x", MaximumFieldBytes+321)
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	line := `{"time":"2026-09-03T12:00:00Z","level":"ERROR","msg":"gateway","seq":4,"kind":"failure","raw_error":` + string(encoded) + "}\n"
	broker.Publish([]byte(line))
	message := receiveMessage(t, subscription)
	if message.Kind != MessageRecord {
		t.Fatalf("message = %#v", message)
	}
	// The connection carries the summary, not the complete body: the buffer
	// bounds records rather than bytes, so a burst of oversized upstream errors
	// would otherwise queue far more than the buffer suggests.
	pushed := len(message.Record.Bytes())
	if pushed >= len(line) {
		t.Fatalf("pushed %d bytes for a %d byte line", pushed, len(line))
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(message.Record.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	var limits map[string]int
	if err := json.Unmarshal(fields[truncatedFieldName], &limits); err != nil {
		t.Fatal(err)
	}
	if limits["raw_error"] != len(body) {
		t.Fatalf("marker = %v", limits)
	}
	// The reference resolves the same identity the record was persisted under.
	if reference := decodeStringField(t, fields[referenceFieldName]); reference != EncodeReference(message.Record.Time, 4) {
		t.Fatalf("reference = %q", reference)
	}

	// Filtering uses the complete record, so a value beyond the bound still
	// decides the match and cannot be changed by summarizing.
	filtered, err := ParseParameters(map[string][]string{"kind": {"failure"}}, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	matching, err := broker.Subscribe(filtered.Filter)
	if err != nil {
		t.Fatal(err)
	}
	defer matching.Close()
	broker.Publish([]byte(line))
	if got := receiveMessage(t, matching); got.Kind != MessageRecord || got.Record.Seq != 4 {
		t.Fatalf("filtered message = %#v", got)
	}
}

func decodeStringField(t *testing.T, value json.RawMessage) string {
	t.Helper()
	var decoded string
	if err := json.Unmarshal(value, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
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
	broker.Publish(brokerLine(1, "INFO"))
	for i, subscription := range subscriptions {
		message := receiveMessage(t, subscription)
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
	// Publishing and flushing after Close return immediately.
	broker.Publish(brokerLine(2, "INFO"))
	broker.Flush()
}

func TestBrokerIgnoresMalformedPublication(t *testing.T) {
	broker := NewBroker()
	defer broker.Close()
	subscription, err := broker.Subscribe(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	broker.Publish([]byte("not-json\n"))
	broker.Publish(brokerLine(7, "INFO"))
	broker.Flush()
	message := receiveMessage(t, subscription)
	if message.Kind != MessageRecord || message.Record.Seq != 7 {
		t.Fatalf("message after malformed line = %#v", message)
	}
	select {
	case unexpected := <-subscription.Messages():
		t.Fatalf("malformed line produced a message: %#v", unexpected)
	default:
	}
}

func TestBrokerConcurrentPublishAndUnsubscribe(t *testing.T) {
	broker := NewBroker()
	subscription, err := broker.Subscribe(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	line := brokerLine(1, "INFO")
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for j := 0; j < 1_000; j++ {
				broker.Publish(line)
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
	broker.Flush()
	broker.Close()
}

package logs

import (
	"bytes"
	"errors"
	"sync"
)

const (
	SubscriberBuffer = 256
	MaximumStreams   = 8
	// DispatchQueueBytes bounds the persisted lines waiting to be parsed,
	// summarized and offered to subscribers. Publishing only appends to this
	// queue, so the cost on the writer's path is a copy of the line; everything
	// that scales with parsing happens on the broker's own goroutine. A line
	// that does not fit is counted as dropped for every current subscriber:
	// it has not been parsed yet, so which subscribers it would have matched is
	// unknown, and over-reporting a drop is the safe direction.
	DispatchQueueBytes = 64 << 20
)

var ErrStreamUnavailable = errors.New("log stream unavailable")

type MessageKind uint8

const (
	MessageRecord MessageKind = iota + 1
	MessageDropped
)

type Message struct {
	Kind    MessageKind
	Record  Record
	Dropped uint64
}

type subscriber struct {
	mu      sync.Mutex
	filter  Filter
	channel chan Message
	dropped uint64
}

// deliver offers one already-summarized record. Filtering runs against the
// original record so a bounded field cannot change a match, while only the
// summary is buffered — the buffer bounds records, not bytes, so buffering
// complete oversized bodies would let a burst of upstream failures queue
// hundreds of megabytes against one connection.
func (s *subscriber) deliver(record, summary Record) {
	if s == nil || !s.filter.Match(record) {
		return
	}
	record = summary
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropped > 0 {
		select {
		case s.channel <- Message{Kind: MessageDropped, Dropped: s.dropped}:
			s.dropped = 0
		default:
			s.dropped++
			return
		}
	}
	select {
	case s.channel <- Message{Kind: MessageRecord, Record: record}:
	default:
		s.dropped++
	}
}

// drop counts one line this subscriber never got the chance to see. The count
// is reported by the next successful delivery as an independent message.
func (s *subscriber) drop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.dropped++
	s.mu.Unlock()
}

// Broker fans persisted log lines out to stream subscribers. Publish never
// parses: it copies the line into a bounded FIFO and returns, and a single
// dispatcher goroutine parses, summarizes and offers each line in publication
// order. The write lock that produces the lines is therefore held only for the
// write itself, and no request path waits for a record to be parsed.
type Broker struct {
	mu          sync.RWMutex
	nextID      uint64
	subscribers map[uint64]*subscriber
	closed      bool

	queueMu    sync.Mutex
	queueReady *sync.Cond
	queueDone  *sync.Cond
	queue      [][]byte
	queueBytes int
	queueLimit int
	// enqueued and completed let Flush wait for exactly the lines accepted
	// before it was called, without pausing the dispatcher.
	enqueued  uint64
	completed uint64
	stopped   bool
	// start launches the dispatcher on the first subscription. A broker that
	// never gets a subscriber never queues a line, so it needs no goroutine.
	start sync.Once
	// beforeDispatch is a test seam that runs before each line is parsed. It is
	// nil in production.
	beforeDispatch func()
}

func NewBroker() *Broker {
	return newBroker(DispatchQueueBytes)
}

func newBroker(queueLimit int) *Broker {
	b := &Broker{
		subscribers: make(map[uint64]*subscriber),
		queueLimit:  queueLimit,
	}
	b.queueReady = sync.NewCond(&b.queueMu)
	b.queueDone = sync.NewCond(&b.queueMu)
	return b
}

// Publish accepts one successfully persisted JSON line and returns without
// parsing it. The line is copied, so the caller may reuse its buffer. With no
// subscriber nothing is queued. A line that would exceed the queue budget is
// discarded and counted as dropped for every current subscriber.
func (b *Broker) Publish(line []byte) {
	if b == nil {
		return
	}
	b.mu.RLock()
	closed, subscribers := b.closed, len(b.subscribers)
	b.mu.RUnlock()
	if closed || subscribers == 0 {
		return
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	copied := append([]byte(nil), line...)

	b.queueMu.Lock()
	if b.stopped {
		b.queueMu.Unlock()
		return
	}
	if b.queueBytes+len(copied) > b.queueLimit {
		b.queueMu.Unlock()
		b.dropForAll()
		return
	}
	b.queue = append(b.queue, copied)
	b.queueBytes += len(copied)
	b.enqueued++
	b.queueReady.Signal()
	b.queueMu.Unlock()
}

func (b *Broker) dropForAll() {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, target := range b.subscribers {
		target.drop()
	}
}

// run is the single dispatcher. Taking lines one at a time in FIFO order is
// what keeps subscriber delivery in the same order the records were written.
func (b *Broker) run() {
	for {
		b.queueMu.Lock()
		for len(b.queue) == 0 && !b.stopped {
			b.queueReady.Wait()
		}
		if b.stopped {
			b.queueMu.Unlock()
			return
		}
		line := b.queue[0]
		b.queue[0] = nil
		b.queue = b.queue[1:]
		b.queueBytes -= len(line)
		if len(b.queue) == 0 {
			// Let the drained backing array go instead of keeping the largest
			// burst ever queued alive.
			b.queue = nil
		}
		hook := b.beforeDispatch
		b.queueMu.Unlock()

		if hook != nil {
			hook()
		}
		b.dispatch(line)

		b.queueMu.Lock()
		b.completed++
		b.queueDone.Broadcast()
		b.queueMu.Unlock()
	}
}

func (b *Broker) dispatch(line []byte) {
	// The writer only publishes lines the JSON handler produced, so a parse
	// failure cannot happen for a real record; there is nothing to offer for it.
	record, err := ParseRecord(line)
	if err != nil {
		return
	}
	summary, err := Summarize(record)
	if err != nil {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for _, target := range b.subscribers {
		target.deliver(record, summary)
	}
}

// Flush blocks until every line accepted before the call has been offered to
// the subscribers, or until the broker is closed. The write path never calls
// it; it exists so callers that need the ordering to be observable, such as
// tests, do not have to sleep.
func (b *Broker) Flush() {
	if b == nil {
		return
	}
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	target := b.enqueued
	for b.completed < target && !b.stopped {
		b.queueDone.Wait()
	}
}

type Subscription struct {
	broker *Broker
	id     uint64
	once   sync.Once
	stream <-chan Message
}

func (s *Subscription) Messages() <-chan Message {
	if s == nil {
		return nil
	}
	return s.stream
}

func (s *Subscription) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.broker != nil {
			s.broker.unsubscribe(s.id)
		}
	})
}

func (b *Broker) Subscribe(filter Filter) (*Subscription, error) {
	if b == nil {
		return nil, ErrStreamUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || len(b.subscribers) >= MaximumStreams {
		return nil, ErrStreamUnavailable
	}
	b.nextID++
	target := &subscriber{filter: filter, channel: make(chan Message, SubscriberBuffer)}
	b.subscribers[b.nextID] = target
	b.start.Do(func() { go b.run() })
	return &Subscription{broker: b, id: b.nextID, stream: target.channel}, nil
}

func (b *Broker) unsubscribe(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	target, ok := b.subscribers[id]
	if !ok {
		return
	}
	delete(b.subscribers, id)
	close(target.channel)
}

// Close ends every subscription and stops the dispatcher. Lines still queued
// are discarded: their subscribers are gone, and the file already holds them.
func (b *Broker) Close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	for id, target := range b.subscribers {
		delete(b.subscribers, id)
		close(target.channel)
	}
	b.mu.Unlock()

	b.queueMu.Lock()
	b.stopped = true
	b.queue = nil
	b.queueBytes = 0
	b.queueReady.Broadcast()
	b.queueDone.Broadcast()
	b.queueMu.Unlock()
}

func (b *Broker) SubscriberCount() int {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

package logs

import (
	"bytes"
	"errors"
	"sync"
)

const (
	SubscriberBuffer = 256
	MaximumStreams   = 8
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

type Broker struct {
	mu          sync.RWMutex
	nextID      uint64
	subscribers map[uint64]*subscriber
	closed      bool
}

func NewBroker() *Broker {
	return &Broker{subscribers: make(map[uint64]*subscriber)}
}

// Publish parses one successfully persisted JSON line and offers it to every
// matching subscriber without waiting for subscriber channel capacity.
func (b *Broker) Publish(line []byte) error {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed || len(b.subscribers) == 0 {
		return nil
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	record, err := ParseRecord(line)
	if err != nil {
		return err
	}
	return b.publishParsedLocked(record)
}

func (b *Broker) PublishRecord(record Record) {
	if b == nil {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	_ = b.publishParsedLocked(record)
}

func (b *Broker) publishParsedLocked(record Record) error {
	summary, err := Summarize(record)
	if err != nil {
		return err
	}
	for _, target := range b.subscribers {
		target.deliver(record, summary)
	}
	return nil
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

func (b *Broker) Close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, target := range b.subscribers {
		delete(b.subscribers, id)
		close(target.channel)
	}
}

func (b *Broker) SubscriberCount() int {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

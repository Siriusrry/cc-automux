package patch

import (
	"container/list"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	DefaultAliasStoreTTL      = time.Hour
	DefaultAliasStoreCapacity = 8192
)

var (
	ErrInvalidAliasKey = errors.New("invalid session alias key")
	ErrAliasRandom     = errors.New("could not generate session alias")
)

// AliasKey is the complete identity of a classifier session alias. Generation
// is part of the key so a target reconfiguration can never reuse an alias
// belonging to the previous runtime identity.
type AliasKey struct {
	TargetID          string
	TargetGeneration  string
	OriginalSessionID string
}

// SessionAliasKey is a descriptive alias retained for callers that use the
// longer name.
type SessionAliasKey = AliasKey

type aliasEntry struct {
	key      AliasKey
	alias    string
	lastUsed time.Time
}

// AliasStore is a bounded, process-local, concurrency-safe TTL/LRU map. It
// never persists aliases and does not expose the original key in logs or
// diagnostics. All lookup/create/evict work occurs under one lock, ensuring
// concurrent first use of a key produces exactly one UUID.
type AliasStore struct {
	mu sync.Mutex

	capacity int
	ttl      time.Duration
	now      func() time.Time
	random   io.Reader

	entries map[AliasKey]*list.Element
	lru     *list.List // front = most recently used; back = least recently used
}

type aliasStoreOptions struct {
	capacity int
	ttl      time.Duration
	now      func() time.Time
	random   io.Reader
}

// AliasStoreOption customizes a store for tests or an embedding runtime.
type AliasStoreOption func(*aliasStoreOptions)

func WithAliasStoreCapacity(capacity int) AliasStoreOption {
	return func(options *aliasStoreOptions) {
		if capacity > 0 {
			options.capacity = capacity
		}
	}
}

func WithAliasStoreTTL(ttl time.Duration) AliasStoreOption {
	return func(options *aliasStoreOptions) {
		if ttl > 0 {
			options.ttl = ttl
		}
	}
}

func WithAliasStoreClock(now func() time.Time) AliasStoreOption {
	return func(options *aliasStoreOptions) {
		if now != nil {
			options.now = now
		}
	}
}

func WithAliasStoreRandomReader(reader io.Reader) AliasStoreOption {
	return func(options *aliasStoreOptions) {
		if reader != nil {
			options.random = reader
		}
	}
}

// NewAliasStore accepts options and a small compatibility set of positional
// arguments: an int capacity and/or time.Duration TTL. Invalid or non-positive
// positional values leave documented defaults in place.
func NewAliasStore(arguments ...any) *AliasStore {
	options := aliasStoreOptions{
		capacity: DefaultAliasStoreCapacity,
		ttl:      DefaultAliasStoreTTL,
		now:      time.Now,
		random:   rand.Reader,
	}
	for _, argument := range arguments {
		switch value := argument.(type) {
		case AliasStoreOption:
			if value != nil {
				value(&options)
			}
		case int:
			if value > 0 {
				options.capacity = value
			}
		case time.Duration:
			if value > 0 {
				options.ttl = value
			}
		case func() time.Time:
			if value != nil {
				options.now = value
			}
		case io.Reader:
			if value != nil {
				options.random = value
			}
		}
	}
	return &AliasStore{
		capacity: options.capacity,
		ttl:      options.ttl,
		now:      options.now,
		random:   options.random,
		entries:  make(map[AliasKey]*list.Element, options.capacity),
		lru:      list.New(),
	}
}

// NewSessionAliasStore is an explicit alias for NewAliasStore.
func NewSessionAliasStore(arguments ...any) *AliasStore { return NewAliasStore(arguments...) }

func (s *AliasStore) validateKey(key AliasKey) error {
	if s == nil || strings.TrimSpace(key.TargetID) == "" || strings.TrimSpace(key.TargetGeneration) == "" {
		return ErrInvalidAliasKey
	}
	// Empty original sessions are intentionally allowed: callers receive a
	// request-scoped UUID that is not inserted into this store.
	return nil
}

// GetOrCreate atomically resolves a key. A blank original session ID creates a
// fresh request-scoped UUID and does not enter the global map. Non-blank keys
// are stable until their sliding TTL expires or an LRU eviction removes them.
func (s *AliasStore) GetOrCreate(targetID, generation, originalSessionID string) (string, error) {
	return s.Resolve(AliasKey{
		TargetID:          targetID,
		TargetGeneration:  generation,
		OriginalSessionID: originalSessionID,
	})
}

// Resolve is the key-struct variant of GetOrCreate.
func (s *AliasStore) Resolve(key AliasKey) (string, error) {
	if err := s.validateKey(key); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if key.OriginalSessionID == "" || strings.TrimSpace(key.OriginalSessionID) == "" {
		return s.newUUIDLocked()
	}
	s.purgeExpiredLocked(now)
	if element, ok := s.entries[key]; ok {
		entry := element.Value.(*aliasEntry)
		entry.lastUsed = now
		s.lru.MoveToFront(element)
		return entry.alias, nil
	}
	alias, err := s.newUUIDLocked()
	if err != nil {
		return "", err
	}
	entry := &aliasEntry{
		key:      key,
		alias:    alias,
		lastUsed: now,
	}
	element := s.lru.PushFront(entry)
	s.entries[key] = element
	for len(s.entries) > s.capacity {
		oldest := s.lru.Back()
		if oldest == nil {
			break
		}
		s.removeElementLocked(oldest)
	}
	return alias, nil
}

// Get returns an existing, unexpired alias without creating one. Accessing an
// entry refreshes its sliding TTL and LRU position. The bool is false for
// misses/expired keys.
func (s *AliasStore) Get(key AliasKey) (string, bool) {
	if s == nil || s.validateKey(key) != nil || key.OriginalSessionID == "" || strings.TrimSpace(key.OriginalSessionID) == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeExpiredLocked(now)
	element, ok := s.entries[key]
	if !ok {
		return "", false
	}
	entry := element.Value.(*aliasEntry)
	entry.lastUsed = now
	s.lru.MoveToFront(element)
	return entry.alias, true
}

func (s *AliasStore) Lookup(key AliasKey) (string, bool) { return s.Get(key) }

// ResolveRequestScoped returns a new UUID without touching the global map.
// It is used by the classifier patch when the incoming session is absent.
func (s *AliasStore) ResolveRequestScoped() (string, error) {
	if s == nil {
		return "", ErrInvalidAliasKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newUUIDLocked()
}

// Len purges expired entries and reports the current map size.
func (s *AliasStore) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(s.now())
	return len(s.entries)
}

func (s *AliasStore) Size() int { return s.Len() }

// Capacity and TTL expose immutable configuration for diagnostics/tests.
func (s *AliasStore) Capacity() int {
	if s == nil {
		return 0
	}
	return s.capacity
}

func (s *AliasStore) TTL() time.Duration {
	if s == nil {
		return 0
	}
	return s.ttl
}

// PurgeExpired eagerly removes expired entries and returns the number removed.
func (s *AliasStore) PurgeExpired() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.purgeExpiredLocked(s.now())
}

func (s *AliasStore) purgeExpiredLocked(now time.Time) int {
	removed := 0
	for element := s.lru.Back(); element != nil; {
		previous := element.Prev()
		entry := element.Value.(*aliasEntry)
		if !now.Before(entry.lastUsed.Add(s.ttl)) {
			s.removeElementLocked(element)
			removed++
		}
		element = previous
	}
	return removed
}

func (s *AliasStore) removeElementLocked(element *list.Element) {
	if element == nil {
		return
	}
	entry, ok := element.Value.(*aliasEntry)
	if ok {
		delete(s.entries, entry.key)
	}
	s.lru.Remove(element)
}

func (s *AliasStore) newUUIDLocked() (string, error) {
	var bytes [16]byte
	if _, err := io.ReadFull(s.random, bytes[:]); err != nil {
		return "", fmt.Errorf("%w: %v", ErrAliasRandom, err)
	}
	// RFC 4122 variant 1, version 4.
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	var encoded [36]byte
	hex.Encode(encoded[0:8], bytes[0:4])
	hex.Encode(encoded[9:13], bytes[4:6])
	hex.Encode(encoded[14:18], bytes[6:8])
	hex.Encode(encoded[19:23], bytes[8:10])
	hex.Encode(encoded[24:36], bytes[10:16])
	encoded[8], encoded[13], encoded[18], encoded[23] = '-', '-', '-', '-'
	return string(encoded[:]), nil
}

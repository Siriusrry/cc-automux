package logs

import (
	"sync"
	"time"
)

// Health reports whether the process is still able to persist its own log
// records. A log write failure must not fail the request path — logging is
// observability, not a safety decision — so the process degrades and stays
// serving. Degrading silently is the part that is not acceptable: the
// management interface is the only log viewer, so a broken log has to be
// visible there rather than looking like an idle period.
type Health struct {
	Healthy       bool       `json:"healthy"`
	Failures      uint64     `json:"failures"`
	LastFailureAt *time.Time `json:"last_failure_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

// HealthTracker holds the write-failure state in memory only. It deliberately
// has no way to report through the log itself: a failure reported by writing a
// record would recurse exactly when writes are already failing.
type HealthTracker struct {
	mu          sync.Mutex
	failures    uint64
	lastFailure time.Time
	lastError   string
	now         func() time.Time
}

func NewHealthTracker() *HealthTracker { return &HealthTracker{} }

// RecordFailure counts one failed log write and reports whether this call is the
// transition out of a healthy state. Only that first transition should reach the
// stderr fallback; writing on every failure would flood the fallback channel in
// exactly the disk-full case it exists for.
func (t *HealthTracker) RecordFailure(err error) bool {
	if t == nil || err == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	first := t.failures == 0
	t.failures++
	t.lastFailure = t.clock()
	t.lastError = err.Error()
	return first
}

// RecordSuccess clears a previous degradation once writes work again.
func (t *HealthTracker) RecordSuccess() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failures == 0 {
		return
	}
	t.failures = 0
	t.lastError = ""
	t.lastFailure = time.Time{}
}

func (t *HealthTracker) Snapshot() Health {
	if t == nil {
		return Health{Healthy: true}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	health := Health{Healthy: t.failures == 0, Failures: t.failures, LastError: t.lastError}
	if !t.lastFailure.IsZero() {
		at := t.lastFailure
		health.LastFailureAt = &at
	}
	return health
}

func (t *HealthTracker) clock() time.Time {
	if t.now != nil {
		return t.now().UTC()
	}
	return time.Now().UTC()
}

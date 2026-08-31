package scheduler

import (
	"errors"
	"fmt"
	"time"

	"github.com/Siriusrry/cc-automux/internal/traffic"
)

// AttemptPolicy contains request-scoped retry limits. Runtime snapshots own a
// value so an in-flight request keeps the policy it captured at start.
type AttemptPolicy struct {
	MaxAttempts int
}

const defaultMaxAttempts = 3

func (p AttemptPolicy) Validate() error {
	if p.MaxAttempts <= 0 {
		return errors.New("maximum attempts must be positive")
	}
	return nil
}

// Policy contains the fixed production health, availability, and affinity
// values. Request retry budgets are carried separately by AttemptPolicy.
// Tests may inject another valid value directly into the owning components.
type Policy struct {
	FailureWindow       time.Duration
	FailureThreshold    int
	Cooldowns           [5]time.Duration
	HalfOpenConcurrency int
	StickyTTL           time.Duration
	StickyCapacity      int
	RetryAfterMin       time.Duration
	RetryAfterMax       time.Duration
}

func DefaultPolicy() Policy {
	return Policy{
		FailureWindow:       2 * time.Minute,
		FailureThreshold:    3,
		Cooldowns:           [5]time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute},
		HalfOpenConcurrency: 1,
		StickyTTL:           time.Hour,
		StickyCapacity:      8192,
		RetryAfterMin:       time.Second,
		RetryAfterMax:       15 * time.Minute,
	}
}

// DefaultAttemptPolicy is the canonical default for the request retry budget.
// It is intentionally independent from the health and affinity policy.
func DefaultAttemptPolicy() AttemptPolicy { return AttemptPolicy{MaxAttempts: defaultMaxAttempts} }

// DefaultClassifierAttemptPolicy is the immutable one-attempt classifier
// budget carried by runtime snapshots. Classifier traffic uses it independently
// from the normal request budget.
func DefaultClassifierAttemptPolicy() AttemptPolicy { return AttemptPolicy{MaxAttempts: 1} }

// ResolveAttemptPolicy reads and validates the immutable request budget from a
// runtime snapshot. It never supplies a request-path default.
func ResolveAttemptPolicy(snapshot Snapshot) (AttemptPolicy, error) {
	if snapshot == nil {
		return AttemptPolicy{}, errors.New("snapshot is required")
	}
	policy := snapshot.AttemptPolicy()
	if err := policy.Validate(); err != nil {
		return AttemptPolicy{}, fmt.Errorf("invalid request attempt policy: %w", err)
	}
	return policy, nil
}

// ErrRequestAttemptPolicyUnavailable indicates that a legacy Snapshot does
// not expose a budget for the requested traffic type. In particular, a
// classifier request must never silently consume the ordinary request budget.
var ErrRequestAttemptPolicyUnavailable = errors.New("request attempt policy unavailable")

// ResolveRequestAttemptPolicy resolves the budget for one request type. The
// typed view is preferred when available; the legacy Snapshot.AttemptPolicy
// method remains valid for normal traffic. Classifier traffic without a typed
// budget fails closed so a normal default cannot accidentally enable retry.
func ResolveRequestAttemptPolicy(snapshot Snapshot, requestType traffic.RequestType) (AttemptPolicy, error) {
	if snapshot == nil {
		return AttemptPolicy{}, errors.New("snapshot is required")
	}
	if provider, ok := snapshot.(RequestAttemptPolicyProvider); ok {
		policy := provider.AttemptPolicyFor(requestType)
		if err := policy.Validate(); err != nil {
			return AttemptPolicy{}, fmt.Errorf("invalid request attempt policy: %w", err)
		}
		return policy, nil
	}
	if view, ok := snapshot.(RequestAttemptPolicyView); ok {
		var policy AttemptPolicy
		switch requestType {
		case traffic.RequestTypeNormal:
			policy = view.NormalAttemptPolicy()
		case traffic.RequestTypeClassifier:
			policy = view.ClassifierAttemptPolicy()
		default:
			return AttemptPolicy{}, fmt.Errorf("%w: unsupported request type %q", ErrRequestAttemptPolicyUnavailable, requestType)
		}
		if err := policy.Validate(); err != nil {
			return AttemptPolicy{}, fmt.Errorf("invalid request attempt policy: %w", err)
		}
		return policy, nil
	}
	if requestType != traffic.RequestTypeNormal {
		return AttemptPolicy{}, fmt.Errorf("%w for %q", ErrRequestAttemptPolicyUnavailable, requestType)
	}
	return ResolveAttemptPolicy(snapshot)
}

type requestPolicySnapshot struct {
	Snapshot
	attempts      AttemptPolicy
	typed         RequestAttemptPolicyView
	typedProvider RequestAttemptPolicyProvider
}

func (s requestPolicySnapshot) AttemptPolicy() AttemptPolicy { return s.attempts }

func (s requestPolicySnapshot) AttemptPolicyFor(requestType traffic.RequestType) AttemptPolicy {
	switch requestType {
	case traffic.RequestTypeNormal:
		return s.attempts
	case traffic.RequestTypeClassifier:
		if s.typed != nil {
			return s.typed.ClassifierAttemptPolicy()
		}
		if s.typedProvider != nil {
			return s.typedProvider.AttemptPolicyFor(requestType)
		}
	}
	return AttemptPolicy{}
}

func (s requestPolicySnapshot) NormalAttemptPolicy() AttemptPolicy { return s.attempts }

func (s requestPolicySnapshot) ClassifierAttemptPolicy() AttemptPolicy {
	if s.typed == nil {
		return AttemptPolicy{}
	}
	return s.typed.ClassifierAttemptPolicy()
}

// CaptureAttemptPolicy freezes the snapshot's request budget behind a wrapper
// so every consumer of one request observes the same validated value.
func CaptureAttemptPolicy(snapshot Snapshot) (Snapshot, AttemptPolicy, error) {
	policy, err := ResolveAttemptPolicy(snapshot)
	if err != nil {
		return nil, AttemptPolicy{}, err
	}
	typed, _ := snapshot.(RequestAttemptPolicyView)
	typedProvider, _ := snapshot.(RequestAttemptPolicyProvider)
	return requestPolicySnapshot{Snapshot: snapshot, attempts: policy, typed: typed, typedProvider: typedProvider}, policy, nil
}

func (p Policy) Validate() error {
	if p.FailureWindow <= 0 || p.FailureThreshold <= 0 {
		return errors.New("failure window and threshold must be positive")
	}
	if p.HalfOpenConcurrency != 1 {
		return errors.New("half-open concurrency must be exactly one")
	}
	if p.StickyTTL <= 0 || p.StickyCapacity <= 0 {
		return errors.New("sticky limits must be positive")
	}
	if p.RetryAfterMin <= 0 || p.RetryAfterMax < p.RetryAfterMin {
		return errors.New("retry-after bounds are invalid")
	}
	previous := time.Duration(0)
	for _, value := range p.Cooldowns {
		if value <= 0 || value < previous {
			return errors.New("cooldown schedule must be positive and nondecreasing")
		}
		previous = value
	}
	return nil
}

// Cooldown returns the cooldown for a zero-based backoff level. Levels beyond
// the schedule remain at the final value.
func (p Policy) Cooldown(level int) time.Duration {
	if level < 0 {
		level = 0
	}
	if level >= len(p.Cooldowns) {
		level = len(p.Cooldowns) - 1
	}
	return p.Cooldowns[level]
}

// EffectiveCooldown extends the current backoff with a valid Retry-After
// duration after clamping it to the configured range.
func (p Policy) EffectiveCooldown(level int, retryAfter time.Duration, hasRetryAfter bool) time.Duration {
	base := p.Cooldown(level)
	if !hasRetryAfter {
		return base
	}
	if retryAfter < p.RetryAfterMin {
		retryAfter = p.RetryAfterMin
	}
	if retryAfter > p.RetryAfterMax {
		retryAfter = p.RetryAfterMax
	}
	if retryAfter > base {
		return retryAfter
	}
	return base
}

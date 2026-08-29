package scheduler

import (
	"errors"
	"time"
)

// Policy contains the fixed production scheduling and health timing values.
// Tests may inject another valid value directly into the owning components.
type Policy struct {
	FailureWindow       time.Duration
	FailureThreshold    int
	Cooldowns           [5]time.Duration
	HalfOpenConcurrency int
	MaxAttempts         int
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
		MaxAttempts:         3,
		StickyTTL:           time.Hour,
		StickyCapacity:      8192,
		RetryAfterMin:       time.Second,
		RetryAfterMax:       15 * time.Minute,
	}
}

func (p Policy) Validate() error {
	if p.FailureWindow <= 0 || p.FailureThreshold <= 0 {
		return errors.New("failure window and threshold must be positive")
	}
	if p.HalfOpenConcurrency != 1 {
		return errors.New("half-open concurrency must be exactly one")
	}
	if p.MaxAttempts <= 0 || p.StickyTTL <= 0 || p.StickyCapacity <= 0 {
		return errors.New("attempt and sticky limits must be positive")
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

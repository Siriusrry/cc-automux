package scheduler

import (
	"reflect"
	"testing"
	"time"
)

func TestDefaultPolicy(t *testing.T) {
	got := DefaultPolicy()
	wantCooldowns := [5]time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute}
	if got.FailureWindow != 2*time.Minute || got.FailureThreshold != 3 ||
		got.HalfOpenConcurrency != 1 || got.MaxAttempts != 3 ||
		got.StickyTTL != time.Hour || got.StickyCapacity != 8192 ||
		got.RetryAfterMin != time.Second || got.RetryAfterMax != 15*time.Minute ||
		!reflect.DeepEqual(got.Cooldowns, wantCooldowns) {
		t.Fatalf("DefaultPolicy() = %#v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("DefaultPolicy().Validate() = %v", err)
	}
}

func TestPolicyCooldownAndRetryAfterClamp(t *testing.T) {
	p := DefaultPolicy()
	if got := p.Cooldown(-1); got != time.Minute {
		t.Fatalf("Cooldown(-1) = %v", got)
	}
	if got := p.Cooldown(99); got != 15*time.Minute {
		t.Fatalf("Cooldown(99) = %v", got)
	}
	if got := p.EffectiveCooldown(0, 30*time.Minute, true); got != 15*time.Minute {
		t.Fatalf("large Retry-After cooldown = %v", got)
	}
	if got := p.EffectiveCooldown(2, 0, true); got != 4*time.Minute {
		t.Fatalf("small Retry-After cooldown = %v", got)
	}
	if got := p.EffectiveCooldown(0, 90*time.Second, true); got != 90*time.Second {
		t.Fatalf("extended cooldown = %v", got)
	}
}

func TestOutcomeClassificationAndFailover(t *testing.T) {
	cases := []struct {
		status int
		class  FailureClass
		fail   bool
	}{
		{200, FailureNone, false},
		{400, FailureNeutral, false},
		{401, FailureGlobalImmediate, true},
		{404, FailureChannelImmediate, true},
		{405, FailureGlobalImmediate, true},
		{302, FailureGlobalImmediate, true},
		{408, FailureChannelTransient, true},
		{425, FailureChannelTransient, true},
		{429, FailureChannelTransient, true},
		{503, FailureChannelTransient, true},
	}
	for _, tc := range cases {
		outcome := Outcome{Class: ClassifyHTTPStatus(tc.status), HTTPStatus: tc.status}
		if outcome.Class != tc.class || outcome.ShouldFailover() != tc.fail {
			t.Fatalf("status %d = class %q failover %v", tc.status, outcome.Class, outcome.ShouldFailover())
		}
	}
	started := Outcome{Class: FailureChannelTransient, ResponseStarted: true}
	if started.ShouldFailover() {
		t.Fatal("response-started outcome allowed failover")
	}
}

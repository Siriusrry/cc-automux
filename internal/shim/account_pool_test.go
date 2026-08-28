package shim

import (
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

func newTestAccountPool(n int, clock *time.Time) *accountPool {
	accounts := make([]accountEntry, 0, n)
	for i := 0; i < n; i++ {
		accounts = append(accounts, accountEntry{Label: "acct-" + strconv.Itoa(i), Key: "key-" + strconv.Itoa(i)})
	}
	p := newAccountPool(accounts)
	if clock != nil {
		p.now = func() time.Time { return *clock }
	}
	return p
}

func TestAccountPoolStickyReuse(t *testing.T) {
	p := newTestAccountPool(3, nil)
	key1, idx1, ok := p.assign("session-A")
	if !ok {
		t.Fatal("assign returned ok=false for a configured pool")
	}
	for i := 0; i < 5; i++ {
		key, idx, ok := p.assign("session-A")
		if !ok || idx != idx1 || key != key1 {
			t.Fatalf("sticky reuse broke: got (%q,%d,%v), want (%q,%d,true)", key, idx, ok, key1, idx1)
		}
	}
}

func TestAccountPoolRoundRobinAcrossSessions(t *testing.T) {
	p := newTestAccountPool(3, nil)
	counts := map[int]int{}
	for i := 0; i < 6; i++ {
		_, idx, ok := p.assign("session-" + strconv.Itoa(i))
		if !ok {
			t.Fatalf("assign %d returned ok=false", i)
		}
		counts[idx]++
	}
	if len(counts) != 3 {
		t.Fatalf("new sessions spread over %d accounts, want all 3: %v", len(counts), counts)
	}
	for idx, c := range counts {
		if c != 2 {
			t.Fatalf("account %d served %d sessions, want balanced 2: %v", idx, c, counts)
		}
	}
}

// TestAccountPoolSingleAccountRateLimitSoftDeprioritize proves that when one
// account is persistently rate-limited while its peers serve cleanly, it is
// soft-deprioritized — new sessions avoid it, but a session already pinned to it
// keeps its pin (not dropped), and once it recovers it rejoins the new-session
// rotation automatically (no fixed cooldown).
func TestAccountPoolSingleAccountRateLimitSoftDeprioritize(t *testing.T) {
	clock := time.Unix(1_000, 0)
	p := newTestAccountPool(3, &clock)

	// Pin a session to account 0 — the account about to be rate-limited (A).
	_, idxA, _ := p.assign("old-A")
	if idxA != 0 {
		t.Fatalf("first assign idx = %d, want 0", idxA)
	}

	// A (idx 0) is persistently rate-limited; B (1) and C (2) serve cleanly. All
	// cross the sample floor so the health baseline is well-defined (sample-floor rule).
	for i := 0; i < 8; i++ {
		p.reportResult(0, http.StatusTooManyRequests, false)
		p.reportResult(1, http.StatusOK, false)
		p.reportResult(2, http.StatusOK, false)
	}

	// A is now soft-deprioritized; the healthy peers are not.
	p.mu.Lock()
	if !p.deprioritizedLocked(0, clock) {
		t.Fatalf("account 0 not deprioritized (failRate=%v) despite persistent 429s", p.accounts[0].failRate)
	}
	if p.deprioritizedLocked(1, clock) || p.deprioritizedLocked(2, clock) {
		t.Fatal("healthy accounts 1/2 wrongly deprioritized")
	}
	p.mu.Unlock()

	// New sessions avoid A: a run of fresh sessions lands only on 1 and 2.
	for i := 0; i < 6; i++ {
		_, idx, ok := p.assign("new-" + strconv.Itoa(i))
		if !ok {
			t.Fatalf("assign new-%d ok=false", i)
		}
		if idx == 0 {
			t.Fatalf("new session new-%d landed on deprioritized account 0", i)
		}
	}

	// Sticky outranks deprioritize: old-A is still served by account 0 and its pin
	// is NOT dropped.
	if _, idx, ok := p.assign("old-A"); !ok || idx != 0 {
		t.Fatalf("sticky old-A assign = (%d,%v), want (0,true) — pin must survive deprioritize", idx, ok)
	}
	p.mu.Lock()
	if _, ok := p.sessions["old-A"]; !ok {
		t.Fatal("sticky session old-A was dropped; deprioritize must never drop a pin")
	}
	p.mu.Unlock()

	// A recovers: feed it successes so its EWMA decays below the δ gap, then a fresh
	// session can land on it again (auto un-deprioritize, no cooldown wait).
	for i := 0; i < 20; i++ {
		p.reportResult(0, http.StatusOK, false)
	}
	p.mu.Lock()
	stillDeprio := p.deprioritizedLocked(0, clock)
	p.mu.Unlock()
	if stillDeprio {
		t.Fatalf("account 0 still deprioritized after recovery (failRate must decay below δ)")
	}
	landedOnA := false
	for i := 0; i < 9; i++ {
		if _, idx, _ := p.assign("recover-" + strconv.Itoa(i)); idx == 0 {
			landedOnA = true
			break
		}
	}
	if !landedOnA {
		t.Fatal("account 0 never received a new session after recovery; did not rejoin rotation")
	}
}

func TestAccountPoolAllCoolingDownFallback(t *testing.T) {
	clock := time.Unix(5_000, 0)
	p := newTestAccountPool(3, &clock)

	// Hard-evict every account via a fatal 401, advancing the clock between strikes
	// so their cooldowns end in account order (0 soonest, 2 latest).
	for idx := 0; idx < 3; idx++ {
		p.reportResult(idx, http.StatusUnauthorized, false)
		clock = clock.Add(time.Second)
	}

	// Every account is still cooling down, but a request must still go out with a
	// shim-owned key: assign falls back to the soonest-to-recover account (0).
	key, idx, ok := p.assign("session-X")
	if !ok {
		t.Fatal("assign returned ok=false while all accounts cooling down; want fallback ok=true")
	}
	if idx != 0 {
		t.Fatalf("fallback idx = %d, want 0 (soonest to recover)", idx)
	}
	if key != "key-0" {
		t.Fatalf("fallback key = %q, want key-0", key)
	}
}

// TestAccountPoolTransportErrorFeedsEWMANoEvict asserts the rewritten semantics: a
// long run of transport errors (a suspect-failure signal) feeds the EWMA failure
// rate and counts samples, but never hard-evicts (no cooldown) nor drops the pinned
// session — the account stays usable. (Old behavior evicted at the threshold.)
func TestAccountPoolTransportErrorFeedsEWMANoEvict(t *testing.T) {
	clock := time.Unix(4_000, 0)
	p := newTestAccountPool(2, &clock)
	if _, idx, _ := p.assign("s"); idx != 0 {
		t.Fatalf("idx = %d, want 0", idx)
	}

	const runs = 10
	for i := 0; i < runs; i++ {
		p.reportResult(0, 0, true)
	}

	p.mu.Lock()
	failRate := p.accounts[0].failRate
	samples := p.accounts[0].samples
	cooled := !p.accounts[0].cooldownUntil.IsZero()
	p.mu.Unlock()
	if cooled {
		t.Fatal("transport errors cooled account 0 down; 429/5xx/transport must not hard-evict")
	}
	if samples != runs {
		t.Fatalf("samples = %d, want %d (each transport error is one sample)", samples, runs)
	}
	if failRate <= 0 {
		t.Fatalf("failRate = %v, want > 0 after transport failures", failRate)
	}
	// The session is still pinned to account 0 (not dropped, not reassigned).
	if _, idx, _ := p.assign("s"); idx != 0 {
		t.Fatalf("idx = %d, want 0 (pin survives transport errors)", idx)
	}
}

// TestAccountPoolFatalAuthHardEvictAndRejoin proves a fatal 401/403 is
// the only hard eviction — it drops the pinned session and sets a cooldown, the
// account is skipped while cooling, and it rejoins the rotation once the cooldown
// elapses. A 429 (platform jitter), by contrast, never hard-evicts or drops a pin.
func TestAccountPoolFatalAuthHardEvictAndRejoin(t *testing.T) {
	clock := time.Unix(2_000, 0)
	p := newTestAccountPool(2, &clock)

	if _, idx, _ := p.assign("s"); idx != 0 {
		t.Fatalf("idx = %d, want 0", idx)
	}
	p.reportResult(0, http.StatusForbidden, false)

	// The fatal auth rejection dropped the pinned session and set a cooldown.
	p.mu.Lock()
	_, stillPinned := p.sessions["s"]
	cooled := !p.accounts[0].cooldownUntil.IsZero()
	p.mu.Unlock()
	if stillPinned {
		t.Fatal("403 did not drop the pinned session")
	}
	if !cooled {
		t.Fatal("403 did not set cooldownUntil")
	}

	// While cooling down, the reassigned session avoids account 0.
	if _, idx, _ := p.assign("s"); idx != 1 {
		t.Fatalf("after 403 idx = %d, want 1 (evicted account skipped)", idx)
	}

	// After the cooldown elapses, account 0 rejoins and can take a new session.
	clock = clock.Add(accountCooldown + time.Second)
	rejoined := false
	for i := 0; i < 4; i++ {
		if _, idx, _ := p.assign("rejoin-" + strconv.Itoa(i)); idx == 0 {
			rejoined = true
			break
		}
	}
	if !rejoined {
		t.Fatal("account 0 never rejoined rotation after cooldown elapsed")
	}

	// Contrast: a long run of 429 never hard-evicts and never drops the pin.
	p2 := newTestAccountPool(2, &clock)
	_, _, _ = p2.assign("t")
	for i := 0; i < 10; i++ {
		p2.reportResult(0, http.StatusTooManyRequests, false)
	}
	p2.mu.Lock()
	cooled2 := !p2.accounts[0].cooldownUntil.IsZero()
	_, pinned2 := p2.sessions["t"]
	p2.mu.Unlock()
	if cooled2 {
		t.Fatal("repeated 429 cooled account 0 down; 429 must never hard-evict")
	}
	if !pinned2 {
		t.Fatal("repeated 429 dropped the pinned session; 429 must never drop a pin")
	}
}

func TestAccountPoolNoAccountsFallback(t *testing.T) {
	p := newAccountPool(nil)
	if _, idx, ok := p.assign("session-A"); ok || idx != -1 {
		t.Fatalf("assign on empty pool = (%d,%v), want (-1,false)", idx, ok)
	}

	// Accounts with only blank keys are skipped, so the pool assigns nothing.
	blank := newAccountPool([]accountEntry{{Label: "x", Key: "   "}})
	if _, idx, ok := blank.assign("session-B"); ok || idx != -1 {
		t.Fatalf("assign on blank-key-only pool = (%d,%v), want (-1,false)", idx, ok)
	}
}

func TestAccountPoolSessionTTLAndCap(t *testing.T) {
	clock := time.Unix(3_000, 0)
	p := newTestAccountPool(1, &clock)

	_, _, _ = p.assign("old")
	if len(p.sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(p.sessions))
	}
	// Advance past the TTL: the idle pin is pruned on the next assign.
	clock = clock.Add(sessionAssignmentTTL + time.Second)
	_, _, _ = p.assign("new")
	if _, ok := p.sessions["old"]; ok {
		t.Fatal("expired session pin was not pruned")
	}
	if len(p.sessions) != 1 {
		t.Fatalf("session count after prune = %d, want 1 (only the new pin)", len(p.sessions))
	}

	// Cap enforcement: with a tiny cap, a new session evicts the LRU pin.
	p.sessionCap = 2
	p.sessionTTL = 0 // disable TTL pruning so only the cap is exercised
	p.sessions = map[string]*sessionAssignment{}
	clock = clock.Add(time.Hour)
	_, _, _ = p.assign("a")
	clock = clock.Add(time.Second)
	_, _, _ = p.assign("b")
	clock = clock.Add(time.Second)
	_, _, _ = p.assign("c") // exceeds cap → evicts LRU ("a")
	if len(p.sessions) != 2 {
		t.Fatalf("session count = %d, want 2 (cap enforced)", len(p.sessions))
	}
	if _, ok := p.sessions["a"]; ok {
		t.Fatal("LRU session pin should have been evicted at cap")
	}
}

func TestAccountPoolConcurrentAccess(t *testing.T) {
	const accounts = 4
	// Dedicate the last account to a 401 hammer: a fatal auth rejection is the only
	// hard eviction now, so this forces the eviction + session-drop path to run
	// concurrently with churning assigns/reports. The churning goroutines never
	// report on it, so no concurrent success clears its cooldown.
	const hammeredIdx = accounts - 1
	p := newTestAccountPool(accounts, nil)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				session := "s-" + strconv.Itoa((g*200+i)%50)
				key, idx, ok := p.assign(session)
				if !ok || idx < 0 || key == "" {
					t.Errorf("assign returned (%q,%d,%v)", key, idx, ok)
					return
				}
				if g%2 == 0 {
					// Churn the non-hammered accounts with jitter that only feeds the
					// EWMA (a 429 then a success), exercising reportResult and the
					// reassignment path concurrently without hard-evicting them.
					if idx != hammeredIdx {
						p.reportResult(idx, http.StatusTooManyRequests, false)
						p.reportResult(idx, http.StatusOK, false)
					}
					continue
				}
				// Hammer the dedicated account with a fatal 401 that hard-evicts and
				// drops its sessions, forcing eviction + reassignment to run
				// concurrently with the churning goroutines (race-checked).
				p.reportResult(hammeredIdx, http.StatusUnauthorized, false)
			}
		}(g)
	}
	wg.Wait()

	// The dedicated account saw only fatal 401s (no clearing success), so it must
	// have entered cooldown at least once — proving the hard-eviction +
	// reassignment path actually ran under concurrency.
	if p.accounts[hammeredIdx].cooldownUntil.IsZero() {
		t.Fatalf("hammered account %d never entered cooldown; eviction path not exercised", hammeredIdx)
	}
}

// TestAccountPoolPlatformJitterNoDeprioritize proves that when AnyRouter jitters
// platform-wide (every account sees the same intermittent 429s), the
// failure rates rise together and stay within δ of each other, so NO account is
// deprioritized, no session is dropped, and an old session still resolves to its
// original account.
func TestAccountPoolPlatformJitterNoDeprioritize(t *testing.T) {
	clock := time.Unix(1_000, 0)
	p := newTestAccountPool(3, &clock)

	// Pin one session to each account so we can prove the pins survive.
	pinned := map[string]int{}
	for i := 0; i < 3; i++ {
		s := "sticky-" + strconv.Itoa(i)
		_, idx, _ := p.assign(s)
		pinned[s] = idx
	}

	// Platform-wide jitter: every account sees the SAME alternating 429/200 pattern,
	// well past the sample floor.
	for round := 0; round < 10; round++ {
		for idx := 0; idx < 3; idx++ {
			if round%2 == 0 {
				p.reportResult(idx, http.StatusTooManyRequests, false)
			} else {
				p.reportResult(idx, http.StatusOK, false)
			}
		}
	}

	// (a) Nobody is deprioritized: all rates rose together, every gap stays below δ.
	p.mu.Lock()
	for i := 0; i < 3; i++ {
		if p.accounts[i].samples < p.minSamples {
			t.Fatalf("account %d samples = %d, want >= %d (eligible to deprioritize)", i, p.accounts[i].samples, p.minSamples)
		}
		if p.deprioritizedLocked(i, clock) {
			t.Fatalf("account %d deprioritized under platform-wide jitter (failRate=%v); want none", i, p.accounts[i].failRate)
		}
	}
	gotSessions := len(p.sessions)
	p.mu.Unlock()

	// (b) No sticky session was dropped.
	if gotSessions != len(pinned) {
		t.Fatalf("sessions after jitter = %d, want %d (none dropped)", gotSessions, len(pinned))
	}

	// (c) Each old session still resolves to its original account.
	for s, want := range pinned {
		if _, idx, ok := p.assign(s); !ok || idx != want {
			t.Fatalf("sticky session %s assign = (%d,%v), want (%d,true)", s, idx, ok, want)
		}
	}
}

// TestAccountPoolCooldownAccountExcludedFromBaseline proves a just-revoked key
// cannot skew the deprioritize baseline. A 401/403 hard eviction sets a
// cooldown but does NOT reset the account's EWMA, so a key that was healthy until it
// was pulled keeps failRate≈0 the whole time it cools. If that frozen ≈0 anchored
// the baseline, the live peers riding ordinary account-independent platform jitter
// (failRate well above 0) would be measured against ≈0 and wrongly soft-deprioritized
// while it cools. minFailRateLocked must therefore exclude the cooling account, so
// the baseline is taken among the live peers themselves and none is deprioritized.
func TestAccountPoolCooldownAccountExcludedFromBaseline(t *testing.T) {
	clock := time.Unix(1_000, 0)
	p := newTestAccountPool(3, &clock)

	// Account A (idx 0): clean successes to cross the sample floor at failRate≈0, then
	// a fatal 401 revokes it → cooldown. The 401 freezes failRate at ≈0 (it neither
	// decays nor counts a sample), modelling a key that was healthy until it was pulled.
	for i := 0; i < 6; i++ {
		p.reportResult(0, http.StatusOK, false)
	}
	p.reportResult(0, http.StatusUnauthorized, false)

	// Accounts B (1) and C (2): the SAME account-independent platform jitter (a 2:1
	// 429:200 mix), so both carry an identical, substantial failRate well past the
	// sample floor. Identical sequences ⇒ bit-identical failRate ⇒ a zero gap between
	// them.
	for round := 0; round < 6; round++ {
		for _, st := range []int{http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusOK} {
			p.reportResult(1, st, false)
			p.reportResult(2, st, false)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Setup sanity: A is actually cooling down with its failRate still ≈0, and B/C are
	// jittered hard enough that including A would incorrectly deprioritize them.
	if cu := p.accounts[0].cooldownUntil; cu.IsZero() || !clock.Before(cu) {
		t.Fatalf("account 0 not in cooldown (cooldownUntil=%v); setup invalid", cu)
	}
	if p.accounts[0].failRate > 1e-9 {
		t.Fatalf("account 0 failRate = %v, want ≈0 (frozen by the 401, not decayed)", p.accounts[0].failRate)
	}
	if p.accounts[1].failRate <= p.deprioritizeDelta {
		t.Fatalf("setup too weak: B failRate %v must exceed δ %v so A's ≈0 baseline would deprioritize it", p.accounts[1].failRate, p.deprioritizeDelta)
	}

	// The baseline excludes the cooling account 0: it is taken among B/C, so it equals
	// the live peers' rate (well above 0), NOT A's frozen ≈0.
	lowest, ok := p.minFailRateLocked(clock)
	if !ok {
		t.Fatal("minFailRateLocked found no eligible account; B/C should qualify")
	}
	if diff := lowest - p.accounts[1].failRate; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("baseline = %v, want B/C failRate %v — account 0's frozen ≈0 leaked into the baseline", lowest, p.accounts[1].failRate)
	}

	// B and C are NOT deprioritized: measured against each other (the live baseline)
	// their gap is ~0, well under δ.
	if p.deprioritizedLocked(1, clock) {
		t.Fatalf("account 1 wrongly deprioritized (failRate=%v, baseline=%v) — cooled peer leaked into baseline", p.accounts[1].failRate, lowest)
	}
	if p.deprioritizedLocked(2, clock) {
		t.Fatalf("account 2 wrongly deprioritized (failRate=%v, baseline=%v) — cooled peer leaked into baseline", p.accounts[2].failRate, lowest)
	}
}

// TestAccountPoolEWMAMath proves the EWMA failure rate exactly tracks
// failRate = α·f + (1-α)·failRate (α=0.3) for a fixed result sequence, within a
// tight tolerance. f=1 for a suspect failure (429), f=0 for a success.
func TestAccountPoolEWMAMath(t *testing.T) {
	p := newTestAccountPool(1, nil)
	// Sequence of suspect failures (true) / successes (false) applied to account 0.
	steps := []bool{true, true, false, true, false} // 429,429,200,429,200
	for _, fail := range steps {
		if fail {
			p.reportResult(0, http.StatusTooManyRequests, false)
		} else {
			p.reportResult(0, http.StatusOK, false)
		}
	}
	// Hand-computed with α=0.3 starting from failRate=0:
	//   0.3 → 0.51 → 0.357 → 0.5499 → 0.38493
	const want = 0.38493
	p.mu.Lock()
	got := p.accounts[0].failRate
	gotSamples := p.accounts[0].samples
	p.mu.Unlock()
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("failRate = %.17g, want %.17g (Δ=%g)", got, want, diff)
	}
	if gotSamples != len(steps) {
		t.Fatalf("samples = %d, want %d (success and failure both count)", gotSamples, len(steps))
	}
}

// TestAnyRouterAccountRotationMultiSessionEndToEnd drives normal (non-classifier)
// /any traffic through the full ServeHTTP path with a 3-account pool and several
// sessions, asserting all three rotation invariants end-to-end: (1) new sessions
// round-robin across every account, (2) the same session sticks to one account,
// and (3) every request has the client's x-api-key stripped and Authorization
// replaced by the shim-owned account key. (TestAnyRouterAccountAuthOverrideEndToEnd
// covers only a single account + single request.)
func TestAccountPoolSnapshotReportsRelativeHealth(t *testing.T) {
	clock := time.Unix(1_000, 0)
	p := newTestAccountPool(3, &clock)

	// Pin one session to account 0 — the one about to be rate-limited.
	if _, idx, _ := p.assign("sticky-0"); idx != 0 {
		t.Fatalf("first assign idx = %d, want 0", idx)
	}

	// Account 0 keeps getting 429 (platform-style suspect failures); 1 and 2 serve
	// cleanly. All cross the sample floor so the health baseline is defined (sample-floor rule).
	for i := 0; i < 8; i++ {
		p.reportResult(0, http.StatusTooManyRequests, false)
		p.reportResult(1, http.StatusOK, false)
		p.reportResult(2, http.StatusOK, false)
	}

	snap := p.snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot len = %d, want 3", len(snap))
	}
	a0 := snap[0]
	if a0.Label != "acct-0" {
		t.Fatalf("snapshot[0].Label = %q, want acct-0", a0.Label)
	}
	if !a0.Deprioritized {
		t.Fatalf("snapshot[0].Deprioritized = false, want true (failRate=%v)", a0.FailRate)
	}
	if a0.Cooldown {
		t.Fatal("snapshot[0].Cooldown = true; under relative-health a 429 must NOT hard-evict (no absolute cooling)")
	}
	if a0.FailRate <= 0 {
		t.Fatalf("snapshot[0].FailRate = %v, want > 0 after persistent 429s", a0.FailRate)
	}
	if a0.Samples != 8 {
		t.Fatalf("snapshot[0].Samples = %d, want 8", a0.Samples)
	}
	if a0.Sessions != 1 {
		t.Fatalf("snapshot[0].Sessions = %d, want 1 (sticky-0 keeps its pin under soft-deprioritize)", a0.Sessions)
	}
	for _, i := range []int{1, 2} {
		if snap[i].Deprioritized {
			t.Fatalf("snapshot[%d].Deprioritized = true, want false (healthy peer)", i)
		}
		if snap[i].Cooldown {
			t.Fatalf("snapshot[%d].Cooldown = true, want false", i)
		}
		if snap[i].Sessions != 0 {
			t.Fatalf("snapshot[%d].Sessions = %d, want 0", i, snap[i].Sessions)
		}
	}

	// A 401 hard-evicts account 1: cooldown=true is the ONLY state that sets it,
	// distinct from account 0's soft-deprioritization.
	p.reportResult(1, http.StatusUnauthorized, false)
	snap = p.snapshot()
	if !snap[1].Cooldown {
		t.Fatal("snapshot[1].Cooldown = false after 401, want true (hard eviction)")
	}
	if snap[0].Cooldown {
		t.Fatal("snapshot[0].Cooldown = true, want false (429 never hard-evicts)")
	}
}

// TestStaticKeyProviderAssignAndReportNoop covers the degenerate single-key
// credentialProvider (credential abstraction): assign always returns the configured key at idx 0
// regardless of session, an empty key is returned verbatim (so applyAccountAuth
// stays a no-op and the client's own auth is forwarded), and reportResult is a
// no-op that never evicts the lone key.

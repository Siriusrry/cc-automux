package shim

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// accountFailureThreshold gates the hard-eviction check in reportResult. Only a
	// fatal auth rejection (401/403) reaches it now — it sets an account's failure
	// count to the threshold to evict on the first strike. Platform jitter
	// (transport error, 429, 5xx) no longer counts toward eviction; it feeds the
	// per-account EWMA failure rate instead (relative health + soft-deprioritize).
	accountFailureThreshold = 3
	// accountCooldown is how long a hard-evicted (401/403) account is skipped before
	// it rejoins the rotation, clearing its eviction state (failure count + cooldown)
	// but KEEPING its EWMA failRate/samples — the rejoin resumes the moving
	// average rather than re-anchoring on a fresh failRate=0 (see isHealthyLocked).
	accountCooldown = 60 * time.Second
	// sessionAssignmentTTL prunes idle session→account pins so the map cannot
	// grow without bound as sessions come and go.
	sessionAssignmentTTL = time.Hour
	// sessionAssignmentCap bounds the session map; the least-recently-used pin
	// is evicted when a new session would exceed it.
	sessionAssignmentCap = 8192
	// accountEWMAAlpha is the smoothing factor (α) for each account's exponentially
	// weighted moving average failure rate: failRate = α·f + (1-α)·failRate, where
	// f=1 for a suspect failure (429/5xx/transport) and f=0 for a success. Higher α
	// reacts faster to recent results; lower α remembers longer.
	accountEWMAAlpha = 0.3
	// accountDeprioritizeDelta is how far (δ) an account's failure rate must sit
	// above the healthiest peer's before new sessions avoid it. Platform-wide
	// jitter lifts every failRate together, keeping the gap below δ so no account is
	// deprioritized; a single rate-limited account rises above its peers and crosses
	// it.
	accountDeprioritizeDelta = 0.3
	// accountMinSamples is the per-account observation floor (M): an account
	// with fewer samples is neutral — it neither sets the health baseline nor is
	// deprioritized — so a fresh account's failRate=0 cannot peg the baseline and
	// wrongly deprioritize a mature account riding normal jitter.
	accountMinSamples = 5
)

// credentialProvider supplies the shim-owned upstream key for a request and
// accountPool rotates a set of shim-owned AnyRouter account keys across
// sessions. Assignment is session-sticky (one session pins to one account to
// maximize upstream cache hits) and cross-session balanced (new sessions round
// robin over healthy, non-deprioritized accounts). Failure handling resists
// AnyRouter's frequent platform jitter: only a fatal auth rejection (401/403)
// hard-evicts (cooldown + drop the account's sessions); 429/5xx/transport blips
// merely feed a per-account EWMA failure rate, and an account whose rate sits
// more than δ above the healthiest peer is soft-deprioritized — new sessions
// avoid it while its sticky sessions keep their pin, until its rate decays back
// (no fixed cooldown). It is safe for concurrent use.
type accountPool struct {
	mu       sync.Mutex
	accounts []accountState
	sessions map[string]*sessionAssignment
	cursor   int

	now               func() time.Time
	failureThreshold  int
	cooldown          time.Duration
	sessionTTL        time.Duration
	sessionCap        int
	ewmaAlpha         float64
	deprioritizeDelta float64
	minSamples        int
}

type accountState struct {
	label         string
	key           string
	failures      int
	failRate      float64
	samples       int
	cooldownUntil time.Time
}

type sessionAssignment struct {
	idx     int
	lastUse time.Time
}

// newAccountPool builds a pool from the normalized config accounts, skipping
// entries with an empty key (an empty pool falls back to forwarding the
// client's own auth).
func newAccountPool(accounts []accountEntry) *accountPool {
	states := make([]accountState, 0, len(accounts))
	for _, a := range accounts {
		if strings.TrimSpace(a.Key) == "" {
			continue
		}
		states = append(states, accountState{label: a.Label, key: a.Key})
	}
	return &accountPool{
		accounts:          states,
		sessions:          make(map[string]*sessionAssignment),
		now:               time.Now,
		failureThreshold:  accountFailureThreshold,
		cooldown:          accountCooldown,
		sessionTTL:        sessionAssignmentTTL,
		sessionCap:        sessionAssignmentCap,
		ewmaAlpha:         accountEWMAAlpha,
		deprioritizeDelta: accountDeprioritizeDelta,
		minSamples:        accountMinSamples,
	}
}

// assign returns the account key and index to use for a request. An existing
// healthy session pin is reused (sticky); otherwise the next healthy account by
// round-robin cursor is chosen and recorded. When every account is in cooldown,
// the one whose cooldown ends soonest is used so the request still goes out with
// a shim-owned key rather than the client's. ok is false only when no account is
// configured.
func (p *accountPool) assign(sessionID string) (string, int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.accounts) == 0 {
		return "", -1, false
	}
	now := p.now()
	p.pruneSessionsLocked(now)

	if sessionID != "" {
		if a, ok := p.sessions[sessionID]; ok {
			if p.isHealthyLocked(a.idx, now) {
				a.lastUse = now
				return p.accounts[a.idx].key, a.idx, true
			}
			// The pinned account is unhealthy; drop the pin and reassign. This drop is
			// triggered ONLY by a 401/403 hard eviction: 429/5xx/transport
			// no longer set a cooldown, so isHealthyLocked stays true for a merely
			// soft-deprioritized account and its sticky sessions keep their pin
			// (upstream cache locality preserved). Deprioritization only steers NEW
			// sessions away (nextHealthyLocked); it never drops an existing pin.
			delete(p.sessions, sessionID)
		}
	}

	idx, ok := p.nextHealthyLocked(now)
	if !ok {
		idx = p.soonestRecoveryLocked()
	}
	if sessionID != "" {
		p.ensureCapacityLocked()
		p.sessions[sessionID] = &sessionAssignment{idx: idx, lastUse: now}
	}
	return p.accounts[idx].key, idx, true
}

// reportResult records the outcome of a request served by account idx, resisting
// AnyRouter platform jitter. A fatal auth rejection (401/403) is
// the ONLY hard eviction: it cools the account down and drops its sessions so they
// reassign. A 429/5xx/transport result is treated as a suspect failure — most such
// blips are account-independent platform jitter — so it only feeds the EWMA failure
// rate (f=1) and is never hard-evicted nor drops sessions; relative health
// (deprioritizedLocked) decides whether THIS account is failing markedly more than
// its peers. A normal response is a health signal (f=0) that decays the rate and
// lifts any prior 401/403 cooldown.
func (p *accountPool) reportResult(idx, status int, transportErr bool) {
	if idx < 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx >= len(p.accounts) {
		return
	}
	acct := &p.accounts[idx]
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// A rejected key (revoked/banned) will not recover by retrying; evict on the
		// first strike via the threshold check below.
		acct.failures = p.failureThreshold
	case transportErr || status == http.StatusTooManyRequests || status >= 500:
		// Suspect failure (likely platform jitter): update the EWMA only — no
		// eviction, no dropped sessions.
		acct.failRate = p.ewmaAlpha + (1-p.ewmaAlpha)*acct.failRate
		acct.samples++
		return
	default:
		// A normal response proves the account/auth works: decay the rate toward 0
		// and clear any prior 401/403 cooldown so a recovered key rejoins at once.
		acct.failRate = (1 - p.ewmaAlpha) * acct.failRate
		acct.samples++
		acct.failures = 0
		acct.cooldownUntil = time.Time{}
		return
	}
	if acct.failures >= p.failureThreshold {
		acct.cooldownUntil = p.now().Add(p.cooldown)
		p.dropSessionsForLocked(idx)
	}
}

// isHealthyLocked reports whether account idx may serve traffic now. An account
// whose 401/403 cooldown has elapsed rejoins here by clearing its failure count and
// cooldown — but it KEEPS its EWMA failRate/samples: the rejoin resumes the
// moving average rather than zeroing it, so a key is judged on its continuing health
// and a recovered account cannot re-enter with a fresh failRate=0 that would peg the
// deprioritize baseline.
//
// Intentional: there is no permanent ban. A persistently-bad key (e.g. a revoked
// one returning 401/403) is evicted, sits out the cooldown, rejoins here, gets
// picked, fails again, and is re-evicted — a steady-state cooldown-then-rejoin
// loop, not a bug. Cooldown bounds how often such a key is retried; if it ever
// recovers it rejoins automatically without operator action.
func (p *accountPool) isHealthyLocked(idx int, now time.Time) bool {
	acct := &p.accounts[idx]
	if acct.cooldownUntil.IsZero() {
		return true
	}
	if now.Before(acct.cooldownUntil) {
		return false
	}
	acct.failures = 0
	acct.cooldownUntil = time.Time{}
	return true
}

// minFailRateLocked returns the lowest EWMA failure rate among accounts eligible
// to set the health baseline, and whether any such account exists. An account is
// eligible only when it has reached the sample floor AND is not currently in a
// 401/403 cooldown. Two exclusions, both guarding the baseline against a spurious
// near-zero anchor:
//   - below the floor: a fresh account's failRate=0 must not peg the baseline
//     and wrongly deprioritize a mature account riding normal jitter.
//   - in cooldown: a just-revoked key keeps its pre-eviction failRate (often ≈0,
//     for a key that was healthy until it was pulled) the whole time it cools, so
//     counting it would anchor the baseline near zero and deprioritize the live
//     peers carrying ordinary platform jitter.
//
// The cooldown test is pure (no side effect): unlike isHealthyLocked it does NOT
// reset an elapsed cooldown — an account whose cooldown has already passed is
// treated as eligible and left for isHealthyLocked to formally rejoin.
func (p *accountPool) minFailRateLocked(now time.Time) (float64, bool) {
	lowest := 0.0
	found := false
	for i := range p.accounts {
		if p.accounts[i].samples < p.minSamples {
			continue
		}
		if cu := p.accounts[i].cooldownUntil; !cu.IsZero() && now.Before(cu) {
			continue
		}
		if !found || p.accounts[i].failRate < lowest {
			lowest = p.accounts[i].failRate
			found = true
		}
	}
	return lowest, found
}

// deprioritizedLocked reports whether account idx is currently soft-deprioritized:
// it has reached the sample floor AND its failure rate exceeds the healthiest
// eligible peer's by more than δ. The baseline (minFailRateLocked) is taken only
// over accounts that are past the floor AND not in a 401/403 cooldown; when no
// such account exists, ok is false and nobody is deprioritized. The `ok &&` guard
// is defensive, not load-bearing under today's only caller: nextHealthyLocked calls
// this only after isHealthyLocked confirms idx is not in cooldown, and an idx past
// the floor then qualifies in minFailRateLocked itself, so found (ok) is always true
// on that path. The guard would matter only if a future caller invoked this on an
// in-cooldown idx (which could leave found=false). An account below the floor is
// never deprioritized. The
// healthiest qualifying account has a zero gap, so at least one eligible account is
// always non-deprioritized (platform-wide jitter, which lifts every rate together,
// leaves the gaps below δ and so deprioritizes nobody).
//
// A soft-deprioritized account is steered away from, not starved: deprioritization
// only stops NEW sessions from landing on it (nextHealthyLocked) and never drops an
// existing pin. How such an account recovers depends on whether it still carries
// sticky sessions:
//   - WITH sticky sessions: that traffic keeps flowing, and each success decays its
//     EWMA failRate (reportResult f=0) until the gap closes and new sessions return.
//   - WITH no sticky sessions: it serves no traffic, so its own failRate is frozen.
//     It stays parked until either its peers' rates rise and lift the baseline toward
//     it (gap closes), or every healthy account becomes deprioritized and
//     nextHealthyLocked's fallback loop routes new sessions to it (thawing its EWMA).
//
// This conservative steady state is deliberate, not starvation: the pool can never
// starve, because the healthiest qualifying account always has a zero gap and so is
// never deprioritized.
func (p *accountPool) deprioritizedLocked(idx int, now time.Time) bool {
	if p.accounts[idx].samples < p.minSamples {
		return false
	}
	lowest, ok := p.minFailRateLocked(now)
	return ok && p.accounts[idx].failRate-lowest > p.deprioritizeDelta
}

// accountStatus is one rotation account in a GET /admin/status snapshot. FailRate
// is the EWMA
// failure rate, Deprioritized is the soft-steer-away flag (rate exceeds the
// healthiest eligible peer by more than δ), Cooldown is the 401/403 hard-eviction
// state — NOT the retired absolute "cooling" eviction that 429/5xx/transport used
// to trigger. Sessions is how many sticky sessions are currently pinned to the
// account. Field names are part of the /admin/status contract.
type accountStatus struct {
	Label         string  `json:"label"`
	FailRate      float64 `json:"fail_rate"`
	Samples       int     `json:"samples"`
	Sessions      int     `json:"sessions"`
	Deprioritized bool    `json:"deprioritized"`
	Cooldown      bool    `json:"cooldown"`
}

// snapshot returns a consistent per-account relative-health view under the pool lock. It
// is a pure read: like minFailRateLocked it treats an account whose cooldown has
// elapsed as no longer cooling without formally rejoining it (no mutation, unlike
// isHealthyLocked). A nil pool (a route with no rotation, e.g. CPA) snapshots as
// nil so the caller can render an empty account list.
func (p *accountPool) snapshot() []accountStatus {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	sessionCounts := make([]int, len(p.accounts))
	for _, a := range p.sessions {
		if a.idx >= 0 && a.idx < len(sessionCounts) {
			sessionCounts[a.idx]++
		}
	}
	out := make([]accountStatus, len(p.accounts))
	for i := range p.accounts {
		acct := &p.accounts[i]
		out[i] = accountStatus{
			Label:         acct.label,
			FailRate:      acct.failRate,
			Samples:       acct.samples,
			Sessions:      sessionCounts[i],
			Deprioritized: p.deprioritizedLocked(i, now),
			Cooldown:      !acct.cooldownUntil.IsZero() && now.Before(acct.cooldownUntil),
		}
	}
	return out
}

// nextHealthyLocked returns the account a new session should use and advances the
// cursor past it for cross-session round-robin balancing. It first round-robins
// over healthy, non-deprioritized accounts so new sessions steer away from an
// account failing markedly more than its peers; only if every healthy account is
// deprioritized does it degrade to plain round-robin over all healthy accounts
// (rather than starve new sessions). Returns false only when every account is in a
// 401/403 cooldown.
func (p *accountPool) nextHealthyLocked(now time.Time) (int, bool) {
	n := len(p.accounts)
	for i := 0; i < n; i++ {
		idx := (p.cursor + i) % n
		if p.isHealthyLocked(idx, now) && !p.deprioritizedLocked(idx, now) {
			p.cursor = (idx + 1) % n
			return idx, true
		}
	}
	for i := 0; i < n; i++ {
		idx := (p.cursor + i) % n
		if p.isHealthyLocked(idx, now) {
			p.cursor = (idx + 1) % n
			return idx, true
		}
	}
	return -1, false
}

// soonestRecoveryLocked returns the account whose cooldown ends earliest, used
// only when every account is currently cooling down.
func (p *accountPool) soonestRecoveryLocked() int {
	best := 0
	for i := 1; i < len(p.accounts); i++ {
		if p.accounts[i].cooldownUntil.Before(p.accounts[best].cooldownUntil) {
			best = i
		}
	}
	return best
}

func (p *accountPool) dropSessionsForLocked(idx int) {
	for sid, a := range p.sessions {
		if a.idx == idx {
			delete(p.sessions, sid)
		}
	}
}

func (p *accountPool) pruneSessionsLocked(now time.Time) {
	if p.sessionTTL <= 0 {
		return
	}
	for sid, a := range p.sessions {
		if a.lastUse.Add(p.sessionTTL).Before(now) {
			delete(p.sessions, sid)
		}
	}
}

// ensureCapacityLocked evicts the least-recently-used session pin when the map
// is at capacity, bounding its growth.
func (p *accountPool) ensureCapacityLocked() {
	if p.sessionCap <= 0 || len(p.sessions) < p.sessionCap {
		return
	}
	oldestKey := ""
	var oldest time.Time
	for sid, a := range p.sessions {
		if oldestKey == "" || a.lastUse.Before(oldest) {
			oldestKey = sid
			oldest = a.lastUse
		}
	}
	if oldestKey != "" {
		delete(p.sessions, oldestKey)
	}
}

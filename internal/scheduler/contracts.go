package scheduler

import (
	"time"

	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

type ProviderGeneration = provider.ProviderGeneration

type StickyKey struct {
	SessionID   string
	Model       string
	RequestType traffic.RequestType
}

type StaticAvailability string

const (
	StaticActive           StaticAvailability = "active"
	StaticDisabledProvider StaticAvailability = "disabled_provider"
	StaticNoModels         StaticAvailability = "no_models"
)

type GlobalHealthState string

const (
	GlobalUnknown  GlobalHealthState = "unknown"
	GlobalHealthy  GlobalHealthState = "healthy"
	GlobalDegraded GlobalHealthState = "degraded"
	GlobalCooldown GlobalHealthState = "cooldown"
	GlobalHalfOpen GlobalHealthState = "half_open"
	GlobalDisabled GlobalHealthState = "disabled"
)

type ChannelHealthState string

const (
	ChannelUnknown  ChannelHealthState = "unknown"
	ChannelHealthy  ChannelHealthState = "healthy"
	ChannelDegraded ChannelHealthState = "degraded"
	ChannelCooldown ChannelHealthState = "cooldown"
	ChannelHalfOpen ChannelHealthState = "half_open"
	ChannelDisabled ChannelHealthState = "disabled"
)

// FailureClass is the health and failover meaning of one completed attempt.
type FailureClass string

const (
	FailureNone             FailureClass = "none"
	FailureNeutral          FailureClass = "neutral"
	FailureGlobalImmediate  FailureClass = "global_immediate"
	FailureGlobalTransient  FailureClass = "global_transient"
	FailureChannelImmediate FailureClass = "channel_immediate"
	FailureChannelTransient FailureClass = "channel_transient"
	FailureChannelStream    FailureClass = "channel_stream"
	FailureClientCanceled   FailureClass = "client_canceled"
	FailureDownstream       FailureClass = "downstream"
)

type Outcome struct {
	Class           FailureClass
	HTTPStatus      int
	RetryAfter      time.Duration
	HasRetryAfter   bool
	UpstreamURL     string
	RawError        string
	ErrorPending    bool
	SessionID       string
	ResponseStarted bool
	ClientCanceled  bool
}

func ClassifyHTTPStatus(status int) FailureClass {
	switch {
	case status >= 200 && status < 300:
		return FailureNone
	case status == 401 || status == 403 || status == 405 || (status >= 300 && status < 400):
		return FailureGlobalImmediate
	case status == 402 || status == 404:
		return FailureChannelImmediate
	case status == 408 || status == 425 || status == 429 || status >= 500:
		return FailureChannelTransient
	default:
		return FailureNeutral
	}
}

func (o Outcome) ShouldFailover() bool {
	if o.ResponseStarted || o.ClientCanceled {
		return false
	}
	switch o.Class {
	case FailureGlobalImmediate, FailureGlobalTransient, FailureChannelImmediate, FailureChannelTransient:
		return true
	default:
		return false
	}
}

func (o Outcome) IsHealthFailure() bool {
	switch o.Class {
	case FailureGlobalImmediate, FailureGlobalTransient, FailureChannelImmediate, FailureChannelTransient, FailureChannelStream:
		return true
	default:
		return false
	}
}

type HealthKey struct {
	ProviderID  string
	Generation  ProviderGeneration
	Model       string
	RequestType traffic.RequestType
}

type HealthLease struct {
	Key          HealthKey
	GlobalProbe  bool
	ChannelProbe bool
	Disabled     bool
	// Token is an opaque single-use value issued and consumed by the health controller.
	Token uint64
}

type HealthDecision struct {
	Available    bool
	Lease        HealthLease
	GlobalState  GlobalHealthState
	ChannelState ChannelHealthState
	RetryAt      *time.Time
}

type HealthUpdate struct {
	GlobalState            GlobalHealthState
	ChannelState           ChannelHealthState
	GlobalEnteredCooldown  bool
	ChannelEnteredCooldown bool
	CooldownUntil          *time.Time
}

type SelectionReason string

const (
	SelectionRegular     SelectionReason = "regular"
	SelectionStickyRetry SelectionReason = "sticky_retry"
)

type AttemptLease struct {
	SnapshotRevision uint64
	Provider         *provider.CompiledProvider
	Model            string
	RequestType      traffic.RequestType
	Generation       ProviderGeneration
	FromSticky       bool
	SelectionReason  SelectionReason
	HalfOpenProbe    bool
	HealthLease      HealthLease

	stickyKey StickyKey
	// stickyMigration carries the assignment observed at the start of this
	// request. A successful replacement can atomically move affinity only if
	// that source assignment is
	// still current; unrelated concurrent requests cannot overwrite it.
	stickyMigration        bool
	stickySourceProviderID string
	stickySourceGeneration ProviderGeneration
	stickySourceVersion    uint64
	cursorKey              roundRobinKey
	cursorVersion          uint64
	advanceCursorOnSuccess bool
}

type Assignment struct {
	Key        StickyKey
	ProviderID string
	Generation ProviderGeneration
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// Snapshot is the immutable request-time view required by the selector.
type Snapshot interface {
	Revision() uint64
	GatewayKey() string
	AttemptPolicy() AttemptPolicy
	Candidates(model string) []*provider.CompiledProvider
	Providers() []*provider.CompiledProvider
}

// HealthController is the scheduler-facing health state boundary.
type HealthController interface {
	Reconcile(providers []*provider.CompiledProvider)
	Acquire(key HealthKey, disableHealth bool) HealthDecision
	Report(lease HealthLease, outcome Outcome) (HealthUpdate, uint64)
	EarliestRetry(keys []HealthKey) (time.Time, bool)
}

// Selector is the gateway-facing scheduling boundary.
type Selector interface {
	Acquire(snapshot Snapshot, key StickyKey, request *RequestSelection) (AttemptLease, error)
	Report(lease AttemptLease, outcome Outcome) (HealthUpdate, uint64)
	Reconcile(snapshot Snapshot)
	Assignments(providerID string) []Assignment
	ActiveAssignmentCount() int
}

// RequestSelection belongs to one client request and captures its immutable
// budget. Round membership only controls order, never eligibility or call count.
// The gateway owns StartAttempt; the selector owns selection and affinity state.
type RequestSelection struct {
	policy             AttemptPolicy
	attemptsUsed       int
	initialized        bool
	ordered            []*provider.CompiledProvider
	visited            map[string]bool
	source             affinitySource
	stickyPhase        stickyRetryPhase
	stickyAttemptsUsed int
}

// The continuous allowance is considered once and cannot be reactivated by
// later round-robin visits to the same provider.
type stickyRetryPhase uint8

const (
	stickyRetryPending stickyRetryPhase = iota
	stickyRetryActive
	stickyRetryEnded
)

// affinitySource is the identity/version observed by this request. Unlike a
// cooldown tombstone it owns no lifetime timestamps and is never refreshed from
// another request's replacement binding.
type affinitySource struct {
	ProviderID string
	Generation ProviderGeneration
	Version    uint64
}

func NewRequestSelection(policy AttemptPolicy) *RequestSelection {
	return &RequestSelection{policy: policy}
}

func (r *RequestSelection) AttemptsUsed() int { return r.attemptsUsed }

// StartAttempt is called immediately before issuing the upstream HTTP call.
func (r *RequestSelection) StartAttempt() int {
	r.attemptsUsed++
	if r.stickyPhase == stickyRetryActive {
		r.stickyAttemptsUsed++
		if r.stickyAttemptsUsed >= r.policy.StickyNoCooldownAttempts {
			r.stickyPhase = stickyRetryEnded
		}
	}
	return r.attemptsUsed
}

func (r *RequestSelection) HasBudget() bool {
	return r != nil && r.attemptsUsed < r.policy.MaxAttempts
}

// ErrorUpdater fills a current attempt's diagnostic without reporting health twice.
type ErrorUpdater interface {
	UpdateError(lease AttemptLease, observation uint64, raw string, incomplete, truncated bool)
}

// HealthErrorUpdater is the health-store counterpart of ErrorUpdater.
type HealthErrorUpdater interface {
	UpdateError(lease HealthLease, observation uint64, raw string, incomplete, truncated bool)
}

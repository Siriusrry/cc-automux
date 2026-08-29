package scheduler

import (
	"time"

	"github.com/Siriusrry/cc-automux/internal/provider"
)

type ProviderGeneration = provider.ProviderGeneration

type TrafficClass string

const (
	TrafficClassNormal     TrafficClass = "normal"
	TrafficClassClassifier TrafficClass = "classifier"
)

type StickyKey struct {
	SessionID    string
	Model        string
	TrafficClass TrafficClass
}

type StaticAvailability string

const (
	StaticActive           StaticAvailability = "active"
	StaticDisabledProvider StaticAvailability = "disabled_provider"
	StaticNoModels         StaticAvailability = "no_models"
	StaticPatchUnavailable StaticAvailability = "patch_unavailable"
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
	case status == 404:
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
	ProviderID   string
	Generation   ProviderGeneration
	Model        string
	TrafficClass TrafficClass
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

type AttemptLease struct {
	SnapshotRevision uint64
	Provider         *provider.CompiledProvider
	Model            string
	TrafficClass     TrafficClass
	Generation       ProviderGeneration
	FromSticky       bool
	HalfOpenProbe    bool
	HealthLease      HealthLease

	stickyKey              StickyKey
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
	Report(lease HealthLease, outcome Outcome) HealthUpdate
	EarliestRetry(keys []HealthKey) (time.Time, bool)
}

// Selector is the gateway-facing scheduling boundary.
type Selector interface {
	Acquire(snapshot Snapshot, key StickyKey, excluded map[string]struct{}) (AttemptLease, error)
	Report(lease AttemptLease, outcome Outcome) HealthUpdate
	Reconcile(snapshot Snapshot)
	Assignments(providerID string) []Assignment
	ActiveAssignmentCount() int
}

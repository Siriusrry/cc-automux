package health

import (
	"sort"
	"time"

	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

// Diagnostic contains the state-machine and latest-observation fields shared
// by provider-global and model-channel health records.
type Diagnostic struct {
	BackoffLevel        int
	ConsecutiveFailures int
	ObservedFailures    uint64
	CooldownUntil       *time.Time
	LastSuccessAt       *time.Time
	LastFailureAt       *time.Time
	ProbeInFlight       bool
	LastUpstreamURL     string
	LastError           string
	LastSessionID       string
}

type GlobalSnapshot struct {
	State scheduler.GlobalHealthState
	Diagnostic
}

type ChannelSnapshot struct {
	Model       string
	RequestType traffic.RequestType
	State       scheduler.ChannelHealthState
	Diagnostic
}

type ProviderSnapshot struct {
	ProviderID    string
	Generation    scheduler.ProviderGeneration
	DisableHealth bool
	Global        GlobalSnapshot
	Channels      []ChannelSnapshot
}

type Snapshot struct {
	GeneratedAt time.Time
	Providers   []ProviderSnapshot
}

type StateCounts struct {
	Unknown  int `json:"unknown"`
	Healthy  int `json:"healthy"`
	Degraded int `json:"degraded"`
	Cooldown int `json:"cooldown"`
	HalfOpen int `json:"half_open"`
	Disabled int `json:"disabled"`
}

type Aggregate struct {
	Global                      StateCounts
	Channels                    StateCounts
	HealthDisabledProviderCount int
}

// Snapshot returns a detached, deterministic view of all current generations.
func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := Snapshot{GeneratedAt: s.clock.Now(), Providers: make([]ProviderSnapshot, 0, len(s.active))}
	ids := make([]string, 0, len(s.active))
	for id := range s.active {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		result.Providers = append(result.Providers, snapshotScope(s.active[id]))
	}
	return result
}

// ProviderSnapshot returns the current generation for one provider.
func (s *Store) ProviderSnapshot(providerID string, generation scheduler.ProviderGeneration) (ProviderSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	scope := s.active[providerID]
	if scope == nil || scope.key.generation != generation {
		return ProviderSnapshot{}, false
	}
	return snapshotScope(scope), true
}

// Aggregate returns fixed-field counts suitable for a status endpoint.
func (s *Store) Aggregate() Aggregate {
	snapshot := s.Snapshot()
	var result Aggregate
	for _, item := range snapshot.Providers {
		if item.DisableHealth {
			result.HealthDisabledProviderCount++
		}
		addGlobalCount(&result.Global, item.Global.State)
		for _, channel := range item.Channels {
			addChannelCount(&result.Channels, channel.State)
		}
	}
	return result
}

func snapshotScope(scope *providerScope) ProviderSnapshot {
	result := ProviderSnapshot{
		ProviderID:    scope.key.providerID,
		Generation:    scope.key.generation,
		DisableHealth: scope.disableHealth,
		Global: GlobalSnapshot{
			State:      globalState(scope.global.state),
			Diagnostic: snapshotDiagnostic(&scope.global),
		},
	}
	modelOrder := make(map[string]int, len(scope.models))
	for i, model := range scope.models {
		modelOrder[model] = i
	}
	for key, entry := range scope.channels {
		if _, configured := scope.modelSet[key.model]; !configured {
			continue
		}
		result.Channels = append(result.Channels, ChannelSnapshot{
			Model:       key.model,
			RequestType: key.requestType,
			State:       channelState(entry.state),
			Diagnostic:  snapshotDiagnostic(entry),
		})
	}
	sort.Slice(result.Channels, func(i, j int) bool {
		left, right := result.Channels[i], result.Channels[j]
		if modelOrder[left.Model] != modelOrder[right.Model] {
			return modelOrder[left.Model] < modelOrder[right.Model]
		}
		return left.RequestType < right.RequestType
	})
	return result
}

func snapshotDiagnostic(entry *stateEntry) Diagnostic {
	return Diagnostic{
		BackoffLevel:        entry.backoffLevel,
		ConsecutiveFailures: entry.consecutiveFailures,
		ObservedFailures:    entry.observedFailures,
		CooldownUntil:       timePointer(entry.cooldownUntil),
		LastSuccessAt:       timePointer(entry.lastSuccessAt),
		LastFailureAt:       timePointer(entry.lastFailureAt),
		ProbeInFlight:       entry.probeToken != 0,
		LastUpstreamURL:     entry.lastUpstreamURL,
		LastError:           entry.lastError,
		LastSessionID:       entry.lastSessionID,
	}
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	copy := value
	return &copy
}

func addGlobalCount(counts *StateCounts, state scheduler.GlobalHealthState) {
	addCount(counts, string(state))
}

func addChannelCount(counts *StateCounts, state scheduler.ChannelHealthState) {
	addCount(counts, string(state))
}

func addCount(counts *StateCounts, state string) {
	switch state {
	case "unknown":
		counts.Unknown++
	case "healthy":
		counts.Healthy++
	case "degraded":
		counts.Degraded++
	case "cooldown":
		counts.Cooldown++
	case "half_open":
		counts.HalfOpen++
	case "disabled":
		counts.Disabled++
	}
}

func globalState(state machineState) scheduler.GlobalHealthState {
	switch state {
	case stateHealthy:
		return scheduler.GlobalHealthy
	case stateDegraded:
		return scheduler.GlobalDegraded
	case stateCooldown:
		return scheduler.GlobalCooldown
	case stateHalfOpen:
		return scheduler.GlobalHalfOpen
	case stateDisabled:
		return scheduler.GlobalDisabled
	default:
		return scheduler.GlobalUnknown
	}
}

func channelState(state machineState) scheduler.ChannelHealthState {
	switch state {
	case stateHealthy:
		return scheduler.ChannelHealthy
	case stateDegraded:
		return scheduler.ChannelDegraded
	case stateCooldown:
		return scheduler.ChannelCooldown
	case stateHalfOpen:
		return scheduler.ChannelHalfOpen
	case stateDisabled:
		return scheduler.ChannelDisabled
	default:
		return scheduler.ChannelUnknown
	}
}

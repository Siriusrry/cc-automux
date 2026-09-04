package gateway

import (
	"time"

	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

type EventKind string

const (
	EventForward  EventKind = "forward"
	EventFailover EventKind = "failover"
	EventSuccess  EventKind = "success"
	EventFailure  EventKind = "failure"
)

// Event contains the complete diagnostic values observed by the data plane.
// Recorders decide how those values are rendered or persisted.
//
// An event carries no timestamp of its own. The log record's timestamp is
// assigned where the record is serialized, so that it cannot disagree with the
// order records are written in.
type Event struct {
	Kind                   EventKind
	ProviderID             string
	ProviderName           string
	SessionID              string
	Model                  string
	RequestType            traffic.RequestType
	Attempt                int
	UpstreamURL            string
	HTTPStatus             int
	RawError               string
	PatchID                string
	PatchStage             string
	NextProviderID         string
	NextProviderName       string
	NextAttempt            int
	NextUpstreamURL        string
	GlobalHealth           scheduler.GlobalHealthState
	ChannelHealth          scheduler.ChannelHealthState
	GlobalEnteredCooldown  bool
	ChannelEnteredCooldown bool
	CooldownUntil          *time.Time
}

type EventRecorder interface {
	RecordGatewayEvent(Event)
}

type EventRecorderFunc func(Event)

func (f EventRecorderFunc) RecordGatewayEvent(event Event) {
	if f != nil {
		f(event)
	}
}

type discardRecorder struct{}

func (discardRecorder) RecordGatewayEvent(Event) {}

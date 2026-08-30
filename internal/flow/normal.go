package flow

import (
	"context"
	"errors"

	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

// NormalPlanner constructs the normal request plan. It intentionally
// performs no provider selection or HTTP work.
type NormalPlanner struct{}

func NewNormalPlanner() *NormalPlanner { return &NormalPlanner{} }

func (p *NormalPlanner) RequestType() traffic.RequestType { return traffic.RequestTypeNormal }

func (p *NormalPlanner) Build(_ context.Context, snapshot SnapshotView, ingress traffic.IngressRequest) (ExecutionPlan, error) {
	if snapshot == nil {
		return ExecutionPlan{}, errors.New("snapshot is required")
	}
	if ingress.RequestType != traffic.RequestTypeNormal {
		return ExecutionPlan{}, errors.New("normal planner received a non-normal request")
	}
	if ingress.CapturedBody == nil {
		return ExecutionPlan{}, errors.New("captured body is required")
	}
	policy := snapshot.NormalAttemptPolicy()
	if err := policy.Validate(); err != nil {
		return ExecutionPlan{}, err
	}
	prepared, err := traffic.NewPreparedRequest(traffic.RequestPlan{
		OriginalModel:     ingress.OriginalModel,
		EffectiveModel:    ingress.OriginalModel,
		OriginalSessionID: ingress.OriginalSessionID,
		RequestType:       ingress.RequestType,
	}, ingress.CapturedBody, ingress.CapturedIndex)
	if err != nil {
		return ExecutionPlan{}, err
	}
	return ExecutionPlan{
		PreparedRequest: prepared,
		TargetMode:      TargetModeProviderPool,
		AttemptPolicy:   policy,
	}, nil
}

var _ Planner = (*NormalPlanner)(nil)
var _ scheduler.AttemptPolicy

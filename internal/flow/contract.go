package flow

import (
	"context"
	"fmt"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

// TargetMode selects the kind of upstream target described by an execution
// plan. The normal flow produces provider-pool plans; the fixed-target value
// is reserved for the Auto Mode flow.
type TargetMode string

const (
	TargetModeProviderPool TargetMode = "provider_pool"
	TargetModeFixedTarget  TargetMode = "fixed_target"
)

// AutoModeSnapshot is deliberately a small value boundary. The normal planner
// does not inspect it; classifier flows can use the same contract
// without making flow depend on the runtime implementation package.
type AutoModeSnapshot struct {
	Mode            string
	ClassifierModel string
	FixedTarget     *provider.CompiledTarget
}

// SnapshotView is the read-only part of a runtime snapshot needed while
// constructing an execution plan.
type SnapshotView interface {
	Revision() uint64
	NormalAttemptPolicy() scheduler.AttemptPolicy
	ClassifierAttemptPolicy() scheduler.AttemptPolicy
	AutoMode() AutoModeSnapshot
}

// Planner performs type-level preparation only. It must not select a
// provider, execute patches, send HTTP, or report health.
type Planner interface {
	RequestType() traffic.RequestType
	Build(context.Context, SnapshotView, traffic.IngressRequest) (ExecutionPlan, error)
}

// ExecutionPlan is immutable after construction and is the only description
// interpreted by the gateway executor.
type ExecutionPlan struct {
	PreparedRequest traffic.PreparedRequest
	TargetMode      TargetMode
	AttemptPolicy   scheduler.AttemptPolicy
	FixedTarget     *provider.CompiledTarget
}

// Validate checks the structural invariants shared by all plans.
func (p ExecutionPlan) Validate() error {
	if err := p.AttemptPolicy.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidExecutionPlan, err)
	}
	if p.TargetMode != TargetModeProviderPool && p.TargetMode != TargetModeFixedTarget {
		return ErrInvalidExecutionPlan
	}
	if _, err := traffic.NewPreparedRequest(
		p.PreparedRequest.Plan,
		p.PreparedRequest.BaseBody,
		p.PreparedRequest.BaseIndex,
	); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidExecutionPlan, err)
	}
	if p.TargetMode == TargetModeFixedTarget && p.FixedTarget == nil {
		return ErrInvalidExecutionPlan
	}
	if p.TargetMode == TargetModeProviderPool && p.FixedTarget != nil {
		return ErrInvalidExecutionPlan
	}
	return nil
}

// Keep bodyfile imported in this contract's API documentation and make it
// explicit that a PreparedRequest owns a sealed Body. The alias is unused at
// runtime but prevents accidental future replacement with []byte.
var _ bodyfile.Body

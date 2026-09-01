// Package automode contains the request-type-specific Claude Code Auto Mode
// detector and planner.  It deliberately does not select providers or perform
// network work; those operations remain in flow/gateway.
package automode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/flow"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

var (
	// ErrAutoModeNotConfigured is returned after a request has been reliably
	// identified as a classifier while Auto Mode is disabled.
	ErrAutoModeNotConfigured = errors.New("auto mode is not configured")
	ErrInvalidClassifierPlan = errors.New("invalid classifier plan")
	ErrClassifierModelEmpty  = errors.New("classifier model is empty")
)

// ClassifierPlanner prepares the one immutable classifier request body and
// chooses the pool/fixed target branch described by the runtime snapshot.
// It never calls Scheduler, applies patches, or sends HTTP.
type ClassifierPlanner struct{}

func NewClassifierPlanner() *ClassifierPlanner { return &ClassifierPlanner{} }

func (p *ClassifierPlanner) RequestType() traffic.RequestType {
	return traffic.RequestTypeClassifier
}

func (p *ClassifierPlanner) Build(ctx context.Context, snapshot flow.SnapshotView, ingress traffic.IngressRequest) (flow.ExecutionPlan, error) {
	if err := plannerContextErr(ctx); err != nil {
		return flow.ExecutionPlan{}, err
	}
	if snapshot == nil {
		return flow.ExecutionPlan{}, errors.New("snapshot is required")
	}
	if ingress.RequestType != traffic.RequestTypeClassifier {
		return flow.ExecutionPlan{}, fmt.Errorf("%w: request type is %q", ErrInvalidClassifierPlan, ingress.RequestType)
	}
	if ingress.CapturedBody == nil {
		return flow.ExecutionPlan{}, fmt.Errorf("%w: captured body is required", ErrInvalidClassifierPlan)
	}
	auto := snapshot.AutoMode()
	switch auto.Mode {
	case "", "disabled":
		return flow.ExecutionPlan{}, ErrAutoModeNotConfigured
	case "provider_pool", "fixed_provider":
		// Continue below.
	default:
		return flow.ExecutionPlan{}, fmt.Errorf("%w: unsupported mode %q", ErrInvalidClassifierPlan, auto.Mode)
	}
	if auto.ClassifierModel == "" {
		return flow.ExecutionPlan{}, ErrClassifierModelEmpty
	}
	policy := snapshot.ClassifierAttemptPolicy()
	if err := policy.Validate(); err != nil {
		return flow.ExecutionPlan{}, fmt.Errorf("%w: invalid classifier attempt policy: %v", ErrInvalidClassifierPlan, err)
	}
	if policy.MaxAttempts != 1 {
		return flow.ExecutionPlan{}, fmt.Errorf("%w: classifier attempt policy must allow exactly one attempt", ErrInvalidClassifierPlan)
	}

	modelField, ok := ingress.CapturedIndex.Lookup("/model")
	if !ok || modelField.Type != bodyfile.JSONString {
		return flow.ExecutionPlan{}, fmt.Errorf("%w: top-level model field is unavailable", ErrInvalidClassifierPlan)
	}
	replacement, err := json.Marshal(auto.ClassifierModel)
	if err != nil {
		return flow.ExecutionPlan{}, fmt.Errorf("%w: encode classifier model: %v", ErrInvalidClassifierPlan, err)
	}
	edit := bodyfile.Edit{Start: modelField.ValueRange.Start, End: modelField.ValueRange.End, Replacement: replacement}
	// Reuse the ingress scan contract.  The helper writes and scans the
	// derived body in one pass, preserving all unknown/non-target bytes and
	// binding the new index to the sealed body.
	base, index, err := bodyfile.ApplyEditsAndScanWithIndexContext(ctx, ingress.CapturedBody, ingress.CapturedIndex, []bodyfile.Edit{edit}, ingress.CapturedIndex.Spec())
	if err != nil {
		return flow.ExecutionPlan{}, fmt.Errorf("%w: model override: %w", ErrInvalidClassifierPlan, err)
	}
	if err := plannerContextErr(ctx); err != nil {
		_ = base.Close()
		return flow.ExecutionPlan{}, err
	}
	planFacts, err := traffic.NewRequestPlan(ingress.OriginalModel, auto.ClassifierModel, ingress.OriginalSessionID, traffic.RequestTypeClassifier)
	if err != nil {
		_ = base.Close()
		return flow.ExecutionPlan{}, fmt.Errorf("%w: %w", ErrInvalidClassifierPlan, err)
	}
	prepared, err := traffic.NewPreparedRequest(planFacts, base, index)
	if err != nil {
		_ = base.Close()
		return flow.ExecutionPlan{}, fmt.Errorf("%w: %w", ErrInvalidClassifierPlan, err)
	}
	result := flow.ExecutionPlan{
		PreparedRequest: prepared,
		AttemptPolicy:   policy,
		TargetMode:      flow.TargetModeProviderPool,
	}
	if auto.Mode == "fixed_provider" {
		if auto.FixedTarget == nil {
			_ = base.Close()
			return flow.ExecutionPlan{}, fmt.Errorf("%w: fixed target is unavailable", ErrInvalidClassifierPlan)
		}
		result.TargetMode = flow.TargetModeFixedTarget
		result.FixedTarget = auto.FixedTarget
	}
	if err := plannerContextErr(ctx); err != nil {
		_ = base.Close()
		return flow.ExecutionPlan{}, err
	}
	return result, nil
}

func plannerContextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

var _ flow.Planner = (*ClassifierPlanner)(nil)

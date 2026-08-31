package automode

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/flow"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

type plannerSnapshot struct {
	auto       flow.AutoModeSnapshot
	classifier scheduler.AttemptPolicy
}

func (plannerSnapshot) Revision() uint64 { return 1 }
func (plannerSnapshot) NormalAttemptPolicy() scheduler.AttemptPolicy {
	return scheduler.DefaultAttemptPolicy()
}
func (s plannerSnapshot) ClassifierAttemptPolicy() scheduler.AttemptPolicy { return s.classifier }
func (s plannerSnapshot) AutoMode() flow.AutoModeSnapshot                  { return s.auto }

func TestClassifierPlannerOverridesOnlyModelAndBuildsPoolPlan(t *testing.T) {
	input := "{\n  \"future\" : [1, 2], \"model\" : \"client-model\", \"system\" : [], \"tail\" : true\n}"
	ingress, closeIngress := plannerIngress(t, input, "session-a")
	defer closeIngress()

	plan, err := NewClassifierPlanner().Build(context.Background(), plannerSnapshot{
		auto:       flow.AutoModeSnapshot{Mode: "provider_pool", ClassifierModel: "classifier/model"},
		classifier: scheduler.AttemptPolicy{MaxAttempts: 1},
	}, ingress)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.PreparedRequest.BaseBody.Close()
	if err := plan.Validate(); err != nil {
		t.Fatalf("plan validation: %v", err)
	}
	if plan.TargetMode != flow.TargetModeProviderPool || plan.FixedTarget != nil || plan.AttemptPolicy.MaxAttempts != 1 {
		t.Fatalf("pool plan = %#v", plan)
	}
	facts := plan.PreparedRequest.Plan
	if facts.OriginalModel != "client-model" || facts.EffectiveModel != "classifier/model" || facts.OriginalSessionID != "session-a" || facts.RequestType != traffic.RequestTypeClassifier {
		t.Fatalf("request plan = %#v", facts)
	}
	want := strings.Replace(input, `"client-model"`, `"classifier/model"`, 1)
	if got := plannerBodyText(t, plan.PreparedRequest.BaseBody); got != want {
		t.Fatalf("prepared body = %q, want %q", got, want)
	}
	if plan.PreparedRequest.BaseIndex.ModelValue() != "classifier/model" {
		t.Fatalf("prepared index model = %q", plan.PreparedRequest.BaseIndex.ModelValue())
	}
}

func TestClassifierPlannerBuildsFixedPlanFromSnapshotTarget(t *testing.T) {
	ingress, closeIngress := plannerIngress(t, `{"model":"client-model","system":[]}`, "session-fixed")
	defer closeIngress()
	target := &provider.CompiledFixedTarget{ID: provider.FixedTargetID, Protocol: "openai_responses"}
	plan, err := NewClassifierPlanner().Build(context.Background(), plannerSnapshot{
		auto: flow.AutoModeSnapshot{
			Mode:            "fixed_provider",
			ClassifierModel: "fixed-classifier",
			FixedTarget:     target,
		},
		classifier: scheduler.AttemptPolicy{MaxAttempts: 1},
	}, ingress)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.PreparedRequest.BaseBody.Close()
	if plan.TargetMode != flow.TargetModeFixedTarget || plan.FixedTarget != target || plan.AttemptPolicy.MaxAttempts != 1 {
		t.Fatalf("fixed plan = %#v", plan)
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("plan validation: %v", err)
	}
}

func TestClassifierPlannerFailsClosedForDisabledInvalidOrCanceledPlans(t *testing.T) {
	ingress, closeIngress := plannerIngress(t, `{"model":"client-model","system":[]}`, "")
	defer closeIngress()
	planner := NewClassifierPlanner()

	if _, err := planner.Build(context.Background(), plannerSnapshot{
		auto:       flow.AutoModeSnapshot{Mode: "disabled"},
		classifier: scheduler.AttemptPolicy{MaxAttempts: 1},
	}, ingress); !errors.Is(err, ErrAutoModeNotConfigured) {
		t.Fatalf("disabled error = %v", err)
	}
	if _, err := planner.Build(context.Background(), plannerSnapshot{
		auto:       flow.AutoModeSnapshot{Mode: "provider_pool", ClassifierModel: "classifier"},
		classifier: scheduler.AttemptPolicy{MaxAttempts: 2},
	}, ingress); !errors.Is(err, ErrInvalidClassifierPlan) {
		t.Fatalf("invalid budget error = %v", err)
	}
	if _, err := planner.Build(context.Background(), plannerSnapshot{
		auto:       flow.AutoModeSnapshot{Mode: "fixed_provider", ClassifierModel: "classifier"},
		classifier: scheduler.AttemptPolicy{MaxAttempts: 1},
	}, ingress); !errors.Is(err, ErrInvalidClassifierPlan) {
		t.Fatalf("missing fixed target error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := planner.Build(canceled, plannerSnapshot{
		auto:       flow.AutoModeSnapshot{Mode: "provider_pool", ClassifierModel: "classifier"},
		classifier: scheduler.AttemptPolicy{MaxAttempts: 1},
	}, ingress); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
}

func plannerIngress(t *testing.T, input, session string) (traffic.IngressRequest, func()) {
	t.Helper()
	spec, err := bodyfile.RequestScanSpec("/system")
	if err != nil {
		t.Fatal(err)
	}
	body, index, err := bodyfile.CaptureAndScan(strings.NewReader(input), spec, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	detection := traffic.NewDetectionRequest(body, index, index.ModelValue(), session, traffic.EmptyHeaders{})
	ingress, err := traffic.NewIngressRequest(detection, traffic.RequestTypeClassifier)
	if err != nil {
		_ = body.Close()
		t.Fatal(err)
	}
	return ingress, func() { _ = body.Close() }
}

func plannerBodyText(t *testing.T, body bodyfile.Body) string {
	t.Helper()
	reader, err := body.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read body: %v / %v", readErr, closeErr)
	}
	return string(data)
}

package flow

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/scheduler"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

type flowTestSnapshot struct {
	revision   uint64
	normal     scheduler.AttemptPolicy
	classifier scheduler.AttemptPolicy
	auto       AutoModeSnapshot

	revisionCalls   atomic.Int64
	normalCalls     atomic.Int64
	classifierCalls atomic.Int64
	autoCalls       atomic.Int64
}

func (s *flowTestSnapshot) Revision() uint64 {
	s.revisionCalls.Add(1)
	return s.revision
}

func (s *flowTestSnapshot) NormalAttemptPolicy() scheduler.AttemptPolicy {
	s.normalCalls.Add(1)
	return s.normal
}

func (s *flowTestSnapshot) ClassifierAttemptPolicy() scheduler.AttemptPolicy {
	s.classifierCalls.Add(1)
	return s.classifier
}

func (s *flowTestSnapshot) AutoMode() AutoModeSnapshot {
	s.autoCalls.Add(1)
	return s.auto
}

type flowTestPlanner struct {
	typ   traffic.RequestType
	build func(context.Context, SnapshotView, traffic.IngressRequest) (ExecutionPlan, error)
}

func (p *flowTestPlanner) RequestType() traffic.RequestType { return p.typ }

func (p *flowTestPlanner) Build(ctx context.Context, snapshot SnapshotView, ingress traffic.IngressRequest) (ExecutionPlan, error) {
	return p.build(ctx, snapshot, ingress)
}

type panicTypePlanner struct{}

func (*panicTypePlanner) RequestType() traffic.RequestType { panic("request type panic") }
func (*panicTypePlanner) Build(context.Context, SnapshotView, traffic.IngressRequest) (ExecutionPlan, error) {
	return ExecutionPlan{}, nil
}

func TestRegistryRejectsMalformedAndDuplicatePlanners(t *testing.T) {
	validBuild := func(context.Context, SnapshotView, traffic.IngressRequest) (ExecutionPlan, error) {
		return ExecutionPlan{}, nil
	}
	var typedNil *flowTestPlanner
	tests := []struct {
		name     string
		planners []Planner
		want     error
	}{
		{name: "nil", planners: []Planner{nil}, want: ErrDuplicatePlanner},
		{name: "typed nil", planners: []Planner{typedNil}, want: ErrDuplicatePlanner},
		{name: "empty type", planners: []Planner{&flowTestPlanner{build: validBuild}}, want: ErrUnknownRequestType},
		{name: "whitespace type", planners: []Planner{&flowTestPlanner{typ: "bad type", build: validBuild}}, want: ErrUnknownRequestType},
		{name: "wildcard type", planners: []Planner{&flowTestPlanner{typ: "*", build: validBuild}}, want: ErrUnknownRequestType},
		{name: "type panic", planners: []Planner{&panicTypePlanner{}}, want: ErrUnknownRequestType},
		{
			name: "duplicate",
			planners: []Planner{
				&flowTestPlanner{typ: traffic.RequestTypeNormal, build: validBuild},
				&flowTestPlanner{typ: traffic.RequestTypeNormal, build: validBuild},
			},
			want: ErrDuplicatePlanner,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewRegistry(test.planners...); !errors.Is(err, test.want) {
				t.Fatalf("NewRegistry error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestRegistryLookupAndListAreDeterministic(t *testing.T) {
	build := func(context.Context, SnapshotView, traffic.IngressRequest) (ExecutionPlan, error) {
		return ExecutionPlan{}, nil
	}
	z := &flowTestPlanner{typ: "z_future", build: build}
	normal := &flowTestPlanner{typ: traffic.RequestTypeNormal, build: build}
	a := &flowTestPlanner{typ: "a_future", build: build}
	registry, err := NewRegistryFromSlice([]Planner{z, normal, a})
	if err != nil {
		t.Fatal(err)
	}
	want := []traffic.RequestType{"a_future", traffic.RequestTypeNormal, "z_future"}
	if got := registry.List(); !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %#v, want %#v", got, want)
	}
	listed := registry.List()
	listed[0] = "mutated"
	if got := registry.List(); !reflect.DeepEqual(got, want) {
		t.Fatalf("List shares caller mutation: %#v", got)
	}
	if got, ok := registry.Lookup("z_future"); !ok || got != z {
		t.Fatalf("Lookup = %#v, %v", got, ok)
	}
	var nilRegistry *Registry
	if _, ok := nilRegistry.Lookup(traffic.RequestTypeNormal); ok || nilRegistry.List() == nil {
		t.Fatal("nil Registry lookup/list contract failed")
	}
}

func TestNormalPlannerBuildsImmutableProviderPoolPlan(t *testing.T) {
	ingress := flowTestIngress(t, traffic.RequestTypeNormal)
	snapshot := &flowTestSnapshot{
		revision:   41,
		normal:     scheduler.AttemptPolicy{MaxAttempts: 3},
		classifier: scheduler.AttemptPolicy{MaxAttempts: 1},
	}
	plan, err := NewNormalPlanner().Build(context.Background(), snapshot, ingress)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	if plan.TargetMode != TargetModeProviderPool || plan.FixedTarget != nil {
		t.Fatalf("target = %q, %#v", plan.TargetMode, plan.FixedTarget)
	}
	if plan.AttemptPolicy != snapshot.normal {
		t.Fatalf("attempt policy = %#v", plan.AttemptPolicy)
	}
	if plan.PreparedRequest.BaseBody != ingress.CapturedBody {
		t.Fatal("normal planner copied or replaced CapturedBody")
	}
	if err := plan.PreparedRequest.BaseIndex.ValidateBody(ingress.CapturedBody); err != nil {
		t.Fatalf("BaseIndex binding: %v", err)
	}
	wantPlan := traffic.RequestPlan{
		OriginalModel:     "original-model",
		EffectiveModel:    "original-model",
		OriginalSessionID: "session-id",
		RequestType:       traffic.RequestTypeNormal,
	}
	if plan.PreparedRequest.Plan != wantPlan {
		t.Fatalf("RequestPlan = %#v, want %#v", plan.PreparedRequest.Plan, wantPlan)
	}
	if snapshot.normalCalls.Load() != 1 || snapshot.classifierCalls.Load() != 0 || snapshot.autoCalls.Load() != 0 || snapshot.revisionCalls.Load() != 0 {
		t.Fatalf("snapshot calls = revision:%d normal:%d classifier:%d auto:%d",
			snapshot.revisionCalls.Load(), snapshot.normalCalls.Load(), snapshot.classifierCalls.Load(), snapshot.autoCalls.Load())
	}
}

func TestNormalPlannerRejectsInvalidInputs(t *testing.T) {
	planner := NewNormalPlanner()
	valid := flowTestIngress(t, traffic.RequestTypeNormal)
	validSnapshot := &flowTestSnapshot{normal: scheduler.AttemptPolicy{MaxAttempts: 3}}

	if _, err := planner.Build(context.Background(), nil, valid); err == nil {
		t.Fatal("nil snapshot succeeded")
	}
	wrongType := valid
	wrongType.RequestType = traffic.RequestTypeClassifier
	if _, err := planner.Build(context.Background(), validSnapshot, wrongType); err == nil {
		t.Fatal("non-normal ingress succeeded")
	}
	missingBody := valid
	missingBody.CapturedBody = nil
	if _, err := planner.Build(context.Background(), validSnapshot, missingBody); err == nil {
		t.Fatal("nil captured body succeeded")
	}
	if _, err := planner.Build(context.Background(), &flowTestSnapshot{}, valid); err == nil {
		t.Fatal("invalid normal attempt policy succeeded")
	}
	missingModel := valid
	missingModel.OriginalModel = ""
	if _, err := planner.Build(context.Background(), validSnapshot, missingModel); err == nil {
		t.Fatal("empty original model succeeded")
	}
	other := flowTestIngress(t, traffic.RequestTypeNormal)
	mismatchedIndex := valid
	mismatchedIndex.CapturedIndex = other.CapturedIndex
	if _, err := planner.Build(context.Background(), validSnapshot, mismatchedIndex); !errors.Is(err, bodyfile.ErrIndexBodyMismatch) {
		t.Fatalf("mismatched index error = %v", err)
	}
}

func TestExecutionPlanValidation(t *testing.T) {
	prepared := flowTestPrepared(t, traffic.RequestTypeNormal)
	validPool := ExecutionPlan{
		PreparedRequest: prepared,
		TargetMode:      TargetModeProviderPool,
		AttemptPolicy:   scheduler.AttemptPolicy{MaxAttempts: 3},
	}
	if err := validPool.Validate(); err != nil {
		t.Fatal(err)
	}
	preparedClassifier := flowTestPrepared(t, traffic.RequestTypeClassifier)
	validFixed := validPool
	validFixed.PreparedRequest = preparedClassifier
	validFixed.TargetMode = TargetModeFixedTarget
	validFixed.FixedTarget = &provider.CompiledFixedTarget{ID: "fixed", Protocol: "openai_responses"}
	validFixed.AttemptPolicy = scheduler.AttemptPolicy{MaxAttempts: 1}
	if err := validFixed.Validate(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		plan ExecutionPlan
	}{
		{name: "invalid policy", plan: func() ExecutionPlan { p := validPool; p.AttemptPolicy = scheduler.AttemptPolicy{}; return p }()},
		{name: "unknown target mode", plan: func() ExecutionPlan { p := validPool; p.TargetMode = "unknown"; return p }()},
		{name: "nil body", plan: func() ExecutionPlan { p := validPool; p.PreparedRequest.BaseBody = nil; return p }()},
		{name: "zero index", plan: func() ExecutionPlan { p := validPool; p.PreparedRequest.BaseIndex = bodyfile.JSONIndex{}; return p }()},
		{name: "empty effective model", plan: func() ExecutionPlan { p := validPool; p.PreparedRequest.Plan.EffectiveModel = ""; return p }()},
		{name: "fixed target missing", plan: func() ExecutionPlan { p := validPool; p.TargetMode = TargetModeFixedTarget; return p }()},
		{name: "pool has fixed target", plan: func() ExecutionPlan {
			p := validPool
			p.FixedTarget = &provider.CompiledFixedTarget{ID: "unexpected", Protocol: "openai_responses"}
			return p
		}()},
		{name: "fixed target with normal request", plan: func() ExecutionPlan {
			p := validPool
			p.TargetMode = TargetModeFixedTarget
			p.FixedTarget = &provider.CompiledFixedTarget{ID: "fixed", Protocol: "openai_responses"}
			return p
		}()},
		{name: "classifier wrong budget", plan: func() ExecutionPlan {
			p := validFixed
			p.AttemptPolicy = scheduler.AttemptPolicy{MaxAttempts: 3}
			return p
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.plan.Validate(); !errors.Is(err, ErrInvalidExecutionPlan) {
				t.Fatalf("Validate = %v, want ErrInvalidExecutionPlan", err)
			}
		})
	}
}

func TestDispatcherSupportsFutureTypeWithoutBusinessSwitch(t *testing.T) {
	futureType := traffic.RequestType("future_type")
	ingress := flowTestIngress(t, futureType)
	prepared := flowTestPrepared(t, futureType)
	snapshot := &flowTestSnapshot{
		normal:     scheduler.AttemptPolicy{MaxAttempts: 3},
		classifier: scheduler.AttemptPolicy{MaxAttempts: 1},
	}
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "expected")
	var calls atomic.Int64
	planner := &flowTestPlanner{
		typ: futureType,
		build: func(gotContext context.Context, gotSnapshot SnapshotView, gotIngress traffic.IngressRequest) (ExecutionPlan, error) {
			calls.Add(1)
			if gotContext.Value(contextKey{}) != "expected" || gotSnapshot != snapshot || gotIngress.RequestType != futureType {
				return ExecutionPlan{}, errors.New("dispatcher changed planner inputs")
			}
			return ExecutionPlan{
				PreparedRequest: prepared,
				TargetMode:      TargetModeProviderPool,
				AttemptPolicy:   scheduler.AttemptPolicy{MaxAttempts: 1},
			}, nil
		},
	}
	registry, err := NewRegistry(NewNormalPlanner(), planner)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(registry)
	plan, err := dispatcher.Dispatch(ctx, snapshot, ingress)
	if err != nil {
		t.Fatal(err)
	}
	if plan.PreparedRequest.Plan.RequestType != futureType || calls.Load() != 1 {
		t.Fatalf("future plan = %#v, calls = %d", plan, calls.Load())
	}
	if _, err := registry.Dispatch(ctx, snapshot, ingress); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("Registry.Dispatch calls = %d", calls.Load())
	}

	const goroutines = 32
	var group sync.WaitGroup
	errorsSeen := make(chan error, goroutines)
	for index := 0; index < goroutines; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := dispatcher.Dispatch(ctx, snapshot, ingress)
			if err != nil {
				errorsSeen <- err
			}
		}()
	}
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Error(err)
	}
}

func TestDispatcherFailsClosedForMissingBuildErrorAndInvalidPlan(t *testing.T) {
	ingress := flowTestIngress(t, traffic.RequestTypeNormal)
	snapshot := &flowTestSnapshot{normal: scheduler.AttemptPolicy{MaxAttempts: 3}}
	var nilDispatcher *Dispatcher
	if _, err := nilDispatcher.Dispatch(context.Background(), snapshot, ingress); !errors.Is(err, ErrMissingPlanner) {
		t.Fatalf("nil dispatcher error = %v", err)
	}
	if _, err := NewDispatcher(nil).Dispatch(context.Background(), snapshot, ingress); !errors.Is(err, ErrMissingPlanner) {
		t.Fatalf("nil registry error = %v", err)
	}
	empty, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Dispatch(context.Background(), snapshot, ingress); !errors.Is(err, ErrMissingPlanner) {
		t.Fatalf("missing planner error = %v", err)
	}

	buildFailure := errors.New("planner build failed")
	failing, err := NewRegistry(&flowTestPlanner{
		typ: traffic.RequestTypeNormal,
		build: func(context.Context, SnapshotView, traffic.IngressRequest) (ExecutionPlan, error) {
			return ExecutionPlan{}, buildFailure
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failing.Dispatch(context.Background(), snapshot, ingress); !errors.Is(err, buildFailure) {
		t.Fatalf("build failure = %v", err)
	}

	invalid, err := NewRegistry(&flowTestPlanner{
		typ: traffic.RequestTypeNormal,
		build: func(context.Context, SnapshotView, traffic.IngressRequest) (ExecutionPlan, error) {
			return ExecutionPlan{TargetMode: TargetModeProviderPool, AttemptPolicy: scheduler.AttemptPolicy{MaxAttempts: 1}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invalid.Dispatch(context.Background(), snapshot, ingress); !errors.Is(err, ErrInvalidExecutionPlan) {
		t.Fatalf("invalid plan error = %v", err)
	}
}

func flowTestIngress(t *testing.T, requestType traffic.RequestType) traffic.IngressRequest {
	t.Helper()
	body, index := flowTestBody(t)
	return traffic.IngressRequest{
		CapturedBody:      body,
		CapturedIndex:     index,
		OriginalModel:     "original-model",
		OriginalSessionID: "session-id",
		RequestType:       requestType,
	}
}

func flowTestPrepared(t *testing.T, requestType traffic.RequestType) traffic.PreparedRequest {
	t.Helper()
	body, index := flowTestBody(t)
	plan, err := traffic.NewRequestPlan("original-model", "effective-model", "session-id", requestType)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := traffic.NewPreparedRequest(plan, body, index)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func flowTestBody(t *testing.T) (bodyfile.Body, bodyfile.JSONIndex) {
	t.Helper()
	body, err := bodyfile.Capture(bytes.NewReader([]byte(`{"model":"original-model","messages":[]}`)), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = body.Close() })
	index, err := bodyfile.Index(body)
	if err != nil {
		t.Fatal(err)
	}
	return body, index
}

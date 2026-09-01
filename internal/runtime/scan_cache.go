package runtime

import (
	"fmt"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/patch"
	"github.com/Siriusrry/cc-automux/internal/provider"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

// ScanRequirements is the immutable detector portion of every request scan
// contract. The application builds it from the same detector registry used by
// Gateway, and Runtime merges it with reachable patch requirements whenever a
// new Snapshot is compiled.
type ScanRequirements struct {
	detectorPaths []string
	rawMarkers    []string
}

// NewScanRequirements validates and snapshots detector-declared paths and raw
// markers for reuse across runtime revisions.
func NewScanRequirements(detectorPaths, rawMarkers []string) (ScanRequirements, error) {
	paths := append([]string(nil), detectorPaths...)
	markers := append([]string(nil), rawMarkers...)
	if _, err := bodyfile.RequestScanSpecWithRawMarkers(paths, markers...); err != nil {
		return ScanRequirements{}, err
	}
	return ScanRequirements{detectorPaths: paths, rawMarkers: markers}, nil
}

func compileRequestScan(catalog *provider.Catalog, auto CompiledAutoMode, requirements ScanRequirements) (*bodyfile.CompiledScanSpec, error) {
	paths := append([]string(nil), requirements.detectorPaths...)
	providers := catalog.Providers()
	for _, item := range providers {
		if item == nil || !item.Enabled || len(item.Models) == 0 {
			continue
		}
		var err error
		paths, err = appendPlanRequestPaths(paths, item.PatchPlan, traffic.RequestTypeNormal)
		if err != nil {
			return nil, fmt.Errorf("compile provider %q normal request scan: %w", item.ID, err)
		}
		if auto.Mode != config.AutoModeProviderPool || !item.SupportsModel(auto.ClassifierModel) {
			continue
		}
		paths, err = appendPlanRequestPaths(paths, item.PatchPlan, traffic.RequestTypeClassifier)
		if err != nil {
			return nil, fmt.Errorf("compile provider %q classifier request scan: %w", item.ID, err)
		}
	}
	if auto.Mode == config.AutoModeFixedProvider && auto.FixedTarget != nil {
		var err error
		paths, err = appendPlanRequestPaths(paths, auto.FixedTarget.PatchPlan, traffic.RequestTypeClassifier)
		if err != nil {
			return nil, fmt.Errorf("compile fixed target classifier request scan: %w", err)
		}
	}
	request, err := bodyfile.RequestScanSpecWithRawMarkers(paths, requirements.rawMarkers...)
	if err != nil {
		return nil, fmt.Errorf("compile request scan: %w", err)
	}
	compiled, err := bodyfile.CompileScanSpec(request)
	if err != nil {
		return nil, fmt.Errorf("compile request scanner: %w", err)
	}
	return compiled, nil
}

func appendPlanRequestPaths(paths []string, plan patch.Plan, requestType traffic.RequestType) ([]string, error) {
	required, err := plan.RequiredPaths(patch.StageRequest, requestType)
	if err != nil {
		return nil, err
	}
	return appendUniqueScanPaths(paths, required...), nil
}

func appendUniqueScanPaths(paths []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(paths)+len(additions))
	for _, path := range paths {
		seen[path] = struct{}{}
	}
	for _, path := range additions {
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths
}

// RequestScanSpec returns the immutable request scan contract compiled with
// this Snapshot.
func (s *Snapshot) RequestScanSpec() *bodyfile.CompiledScanSpec {
	if s == nil {
		return nil
	}
	return s.requestScan
}

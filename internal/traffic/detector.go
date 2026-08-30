package traffic

import (
	"fmt"
	"reflect"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
)

// Detector is a read-only classifier.  It must not mutate the RequestView or
// perform provider selection, body edits, network calls, or health reporting.
type Detector interface {
	Type() RequestType
	Detect(RequestView) (matched bool, err error)
}

// ScanPathDetector is implemented by detectors that inspect JSON fields.
// Gateway includes RequiredPaths in the single incremental ingress scan.
type ScanPathDetector interface {
	Detector
	RequiredPaths() []string
}

// DetectorRegistry is the gateway-facing classification boundary.
type DetectorRegistry interface {
	Classify(RequestView) (RequestType, error)
}

// Registry is an immutable ordered detector registry.  Normal is a fallback,
// not a detector entry; an empty registry therefore classifies every request
// as normal in production.
type Registry struct {
	detectors       []Detector
	scanPaths       []string
	scanPathsByType map[RequestType][]string
}

// NewRegistry validates and snapshots detectors.  Detector order is retained
// for deterministic evaluation and diagnostics; classification never silently
// picks the first match when two detectors match.
func NewRegistry(detectors ...Detector) (*Registry, error) {
	entries := append([]Detector(nil), detectors...)
	seen := make(map[RequestType]struct{}, len(entries))
	seenPaths := make(map[string]struct{})
	var scanPaths []string
	scanPathsByType := make(map[RequestType][]string, len(entries))
	for index, detector := range entries {
		if isNilDetector(detector) {
			return nil, fmt.Errorf("%w at index %d", ErrNilDetector, index)
		}
		requestType, typeErr := detectorTypeSafely(detector)
		if typeErr != nil {
			return nil, fmt.Errorf("detector %d: %w", index, typeErr)
		}
		if err := validateRequestType(requestType, false); err != nil {
			return nil, fmt.Errorf("detector %d: %w", index, err)
		}
		if requestType == RequestType("*") {
			return nil, fmt.Errorf("detector %d: %w: wildcard is not a request type", index, ErrInvalidRequestType)
		}
		if _, exists := seen[requestType]; exists {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateDetector, requestType)
		}
		seen[requestType] = struct{}{}
		paths, pathErr := detectorPathsSafely(detector)
		if pathErr != nil {
			return nil, fmt.Errorf("detector %d: %w", index, pathErr)
		}
		if _, pathErr := bodyfile.NewScanSpec(paths...); pathErr != nil {
			return nil, fmt.Errorf("detector %d: invalid required paths: %w", index, pathErr)
		}
		for _, path := range paths {
			scanPathsByType[requestType] = append(scanPathsByType[requestType], path)
			if _, duplicate := seenPaths[path]; duplicate {
				continue
			}
			seenPaths[path] = struct{}{}
			scanPaths = append(scanPaths, path)
		}
	}
	return &Registry{detectors: entries, scanPaths: scanPaths, scanPathsByType: scanPathsByType}, nil
}

// NewDetectorRegistry is an alias for NewRegistry.
func NewDetectorRegistry(detectors ...Detector) (*Registry, error) {
	return NewRegistry(detectors...)
}

// NewRegistryFromSlice is the slice-shaped constructor for callers that keep
// detector definitions in a collection before composing the immutable
// registry.
func NewRegistryFromSlice(detectors []Detector) (*Registry, error) {
	return NewRegistry(detectors...)
}

// NewDetectorRegistryFromSlice is a descriptive alias for
// NewRegistryFromSlice.
func NewDetectorRegistryFromSlice(detectors []Detector) (*Registry, error) {
	return NewRegistryFromSlice(detectors)
}

// DefaultRegistry installs no specialised detector, so every request falls
// back to normal.
func DefaultRegistry() *Registry {
	registry, err := NewRegistry()
	if err != nil {
		panic(err)
	}
	return registry
}

// ProductionRegistry is an explicit alias for the production detector set.
func ProductionRegistry() *Registry { return DefaultRegistry() }

// MustRegistry constructs a registry and panics on malformed test setup.
func MustRegistry(detectors ...Detector) *Registry {
	registry, err := NewRegistry(detectors...)
	if err != nil {
		panic(err)
	}
	return registry
}

// Classify evaluates all specialised detectors.  No match returns normal; one
// match returns that detector type; multiple matches or a detector error fail
// closed with a stable errors.Is sentinel.
func (r *Registry) Classify(view RequestView) (RequestType, error) {
	if r == nil {
		return "", ErrNilDetectorRegistry
	}
	view.Headers = normalizeHeaders(view.Headers)
	matched := make([]RequestType, 0, 1)
	for _, detector := range r.detectors {
		ok, err := detectSafely(detector, view)
		if err != nil {
			requestType, typeErr := detectorTypeSafely(detector)
			if typeErr != nil {
				requestType = "unknown"
			}
			return "", fmt.Errorf("%w (%s): %w", ErrDetectionFailed, requestType, err)
		}
		if ok {
			requestType, typeErr := detectorTypeSafely(detector)
			if typeErr != nil {
				return "", fmt.Errorf("%w: %w", ErrDetectionFailed, typeErr)
			}
			matched = append(matched, requestType)
		}
	}
	switch len(matched) {
	case 0:
		return RequestTypeNormal, nil
	case 1:
		return matched[0], nil
	default:
		return "", fmt.Errorf("%w: %v", ErrAmbiguousRequestType, matched)
	}
}

func detectSafely(detector Detector, view RequestView) (matched bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("detector panic: %v", recovered)
			matched = false
		}
	}()
	return detector.Detect(view)
}

func detectorTypeSafely(detector Detector) (requestType RequestType, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("detector type panic: %v", recovered)
		}
	}()
	requestType = detector.Type()
	return requestType, nil
}

func detectorPathsSafely(detector Detector) (paths []string, err error) {
	requirements, ok := detector.(interface{ RequiredPaths() []string })
	if !ok {
		return []string{}, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			paths = nil
			err = fmt.Errorf("required paths panic: %v", recovered)
		}
	}()
	paths = requirements.RequiredPaths()
	if paths == nil {
		return []string{}, nil
	}
	return append([]string(nil), paths...), nil
}

// ClassifyDetection is a convenience method for the canonical ingress fact
// type.  It returns an IngressRequest only after successful classification.
func (r *Registry) ClassifyDetection(request DetectionRequest) (IngressRequest, error) {
	if err := request.Validate(); err != nil {
		return IngressRequest{}, err
	}
	requestType, err := r.Classify(request.View())
	if err != nil {
		return IngressRequest{}, err
	}
	return NewIngressRequest(request, requestType)
}

// Detectors returns a defensive copy of the registered detector interfaces.
// The registry itself remains immutable; callers should treat detector values
// as read-only according to the Detector contract.
func (r *Registry) Detectors() []Detector {
	if r == nil || len(r.detectors) == 0 {
		return []Detector{}
	}
	return append([]Detector(nil), r.detectors...)
}

// RequiredPaths returns the stable union declared by specialised detectors.
func (r *Registry) RequiredPaths() []string {
	if r == nil || len(r.scanPaths) == 0 {
		return []string{}
	}
	return append([]string(nil), r.scanPaths...)
}

// RequiredPathsForType returns the paths frozen for the detector which can
// produce requestType. Registry construction has already called Type and
// RequiredPaths through panic-safe boundaries, so request handling never
// invokes mutable detector metadata methods again.
func (r *Registry) RequiredPathsForType(requestType RequestType) []string {
	if r == nil {
		return []string{}
	}
	paths := r.scanPathsByType[requestType]
	if len(paths) == 0 {
		return []string{}
	}
	return append([]string(nil), paths...)
}

// Types returns registered specialised types in deterministic registration
// order.  It is useful for discovery/testing and never includes normal.
func (r *Registry) Types() []RequestType {
	if r == nil {
		return []RequestType{}
	}
	result := make([]RequestType, 0, len(r.detectors))
	for _, detector := range r.detectors {
		result = append(result, detector.Type())
	}
	return result
}

// Len reports the number of specialised detectors.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.detectors)
}

func isNilDetector(detector Detector) bool {
	if detector == nil {
		return true
	}
	// An interface containing a nil pointer is a common test fixture.  Avoid
	// invoking methods on it and classify it as malformed at construction.
	value := reflect.ValueOf(detector)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Compile-time assertion documents that Registry is the production boundary.
var _ DetectorRegistry = (*Registry)(nil)

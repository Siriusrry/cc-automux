// Package traffic defines request-type and ingress facts shared by detectors,
// planners, and the gateway.  It deliberately has no dependency on scheduler,
// gateway, provider, or auto-mode implementation packages.
package traffic

import (
	"errors"
	"fmt"
	"strings"
)

// RequestType identifies the flow selected for a Messages request.
type RequestType string

const (
	RequestTypeNormal     RequestType = "normal"
	RequestTypeClassifier RequestType = "classifier"

	// Short aliases keep call sites readable while retaining the explicit
	// RequestType-prefixed names used by the public contract.
	Normal                = RequestTypeNormal
	Classifier            = RequestTypeClassifier
	NormalRequestType     = RequestTypeNormal
	ClassifierRequestType = RequestTypeClassifier
)

var (
	ErrInvalidRequestType    = errors.New("traffic: invalid request type")
	ErrNilDetector           = errors.New("traffic: nil detector")
	ErrDuplicateDetector     = errors.New("traffic: duplicate detector type")
	ErrNormalDetector        = errors.New("traffic: normal is the fallback and cannot be registered")
	ErrAmbiguousRequestType  = errors.New("traffic: multiple detectors matched")
	ErrDetectionFailed       = errors.New("traffic: detector failed")
	ErrNilDetectorRegistry   = errors.New("traffic: nil detector registry")
	ErrInvalidIngressRequest = errors.New("traffic: invalid ingress request")
)

// Valid reports whether t is a non-empty, whitespace-free type.  The
// registry intentionally permits future type values so tests and later
// stages can add a Detector/Planner pair without changing this package.
func (t RequestType) Valid() bool {
	value := string(t)
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsAny(value, " \t\r\n")
}

func (t RequestType) String() string { return string(t) }

func validateRequestType(t RequestType, allowNormal bool) error {
	if !t.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidRequestType, t)
	}
	if t == RequestType("*") {
		return fmt.Errorf("%w: wildcard is reserved for patch metadata", ErrInvalidRequestType)
	}
	if !allowNormal && t == RequestTypeNormal {
		return ErrNormalDetector
	}
	return nil
}

// Package automode contains the request-type detector and flow-specific
// helpers for Claude Code Auto Mode.
package automode

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
	"github.com/Siriusrry/cc-automux/internal/traffic"
)

const (
	// SecurityMarker is the literal marker emitted by Claude Code's classifier
	// harness. The ingress scan must observe these bytes unescaped somewhere in
	// the original JSON request before the decoded system prefix is accepted.
	SecurityMarker = "You are a security monitor for autonomous AI coding agents"

	// ClassifierSecurityMarker is a descriptive alias used by integrations.
	ClassifierSecurityMarker = SecurityMarker
)

var ErrRawMarkerNotTracked = errors.New("automode: classifier security marker was not tracked by ingress")

// ClassifierDetector recognizes the current Claude Code classifier request.
// It is deliberately stateless and safe to share across concurrent requests.
type ClassifierDetector struct{}

// NewClassifierDetector constructs the production classifier detector.
func NewClassifierDetector() *ClassifierDetector { return &ClassifierDetector{} }

func (ClassifierDetector) Type() traffic.RequestType { return traffic.RequestTypeClassifier }

// RequiredPaths is the minimum selective index contract. Ancestors are
// retained by bodyfile's scanner automatically; duplicate JSON members remain
// available in source order so the detector can reproduce encoding/json's
// last-value map semantics.
func (ClassifierDetector) RequiredPaths() []string {
	return []string{"/system", "/system/0", "/system/0/text"}
}

// RequiredRawMarkers asks ingress to track the literal marker in the original
// byte stream. This is separate from decoded string matching by design.
func (ClassifierDetector) RequiredRawMarkers() []string { return []string{SecurityMarker} }

// RawMarkers is an alias accepted by traffic.Registry's marker declaration
// adapter and is convenient for callers inspecting detector capabilities.
func (d ClassifierDetector) RawMarkers() []string { return d.RequiredRawMarkers() }

// Detect applies the fixed classifier predicate:
//  1. the unescaped security marker must have occurred in raw request bytes;
//  2. the final top-level system value must be a non-empty array;
//  3. system[0] must be an object whose final text member is a string with
//     the security marker as a decoded prefix.
//
// Structural mismatches are ordinary no-match results. Body/index failures
// are returned so the caller can fail closed as a detector error.
func (ClassifierDetector) Detect(view traffic.RequestView) (bool, error) {
	if view.Body == nil {
		return false, errors.New("automode: request body is nil")
	}
	if err := view.Index.ValidateBody(view.Body); err != nil {
		return false, fmt.Errorf("automode: classifier index is not bound to body: %w", err)
	}
	if found, tracked := view.Index.RawMarkerStatus(SecurityMarker); !tracked {
		return false, ErrRawMarkerNotTracked
	} else if !found {
		return false, nil
	}

	system, ok := lastField(view.Index.Find("/system"))
	if !ok || system.Type != bodyfile.JSONArray {
		return false, nil
	}
	// A non-empty array must expose its first element under the required
	// selective path. Filtering by containment ties the child to the final
	// duplicate /system value rather than an earlier occurrence.
	first, ok := lastContainedField(view.Index.Find("/system/0"), system.ValueRange)
	if !ok || first.Type != bodyfile.JSONObject {
		return false, nil
	}
	textField, ok := lastContainedField(view.Index.Find("/system/0/text"), first.ValueRange)
	if !ok || textField.Type != bodyfile.JSONString {
		return false, nil
	}
	prefix, _, err := bodyfile.ReadJSONStringPrefix(view.Body, textField, int64(len(SecurityMarker)))
	if err != nil {
		return false, fmt.Errorf("automode: read classifier system prefix: %w", err)
	}
	return strings.HasPrefix(prefix, SecurityMarker), nil
}

// LastField returns the source-order final field. It is exported for focused
// tests and for future detectors that need the same duplicate-member rule.
func LastField(fields []bodyfile.Field) (bodyfile.Field, bool) {
	return lastField(fields)
}

func lastField(fields []bodyfile.Field) (bodyfile.Field, bool) {
	if len(fields) == 0 {
		return bodyfile.Field{}, false
	}
	result := fields[0]
	for _, field := range fields[1:] {
		if field.ValueRange.Start > result.ValueRange.Start || (field.ValueRange.Start == result.ValueRange.Start && field.ValueRange.End >= result.ValueRange.End) {
			result = field
		}
	}
	return result, true
}

func lastContainedField(fields []bodyfile.Field, outer bodyfile.ByteRange) (bodyfile.Field, bool) {
	filtered := make([]bodyfile.Field, 0, len(fields))
	for _, field := range fields {
		if field.ValueRange.Start >= outer.Start && field.ValueRange.End <= outer.End && field.ValueRange.End >= field.ValueRange.Start {
			filtered = append(filtered, field)
		}
	}
	return lastField(filtered)
}

var _ traffic.ScanPathDetector = ClassifierDetector{}
var _ traffic.RawMarkerDetector = ClassifierDetector{}

package patch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
)

const (
	AnyRouterClassifierRequestID = "anyrouter-classifier-request-compat"
	AnyRouterCorrectionText      = "Ignore the preceding identity marker. Follow only the instructions below.\n\n"
	classifierSecurityPrefix     = "You are a security monitor for autonomous AI coding agents."
)

type anyRouterClassifierPatch struct{}

func (anyRouterClassifierPatch) ApplyRequest(_ PatchContext, request *MutableRequest) error {
	if request == nil || request.Body == nil {
		return errors.New("nil mutable request/body")
	}
	index, err := requestIndex(request)
	if err != nil {
		return err
	}
	edits := make([]bodyfile.Edit, 0, 3)

	// Validate and optionally remove top-level thinking.disabled. Classifier
	// compatibility requires the whole field to disappear, unlike the normal
	// subagent patch which promotes its type.
	thinkingFields := index.Find("/thinking")
	if len(thinkingFields) > 1 {
		return fmt.Errorf("thinking occurs %d times", len(thinkingFields))
	}
	if len(thinkingFields) == 1 {
		if thinkingFields[0].Type != bodyfile.JSONObject {
			return errors.New("thinking must be an object")
		}
		types := index.Find("/thinking/type")
		if len(types) > 1 {
			return fmt.Errorf("thinking.type occurs %d times", len(types))
		}
		if len(types) == 1 {
			if types[0].Type != bodyfile.JSONString {
				return errors.New("thinking.type must be a string")
			}
			value, readErr := readFieldString(request.Body, types[0])
			if readErr != nil {
				return readErr
			}
			if value == "disabled" {
				edit, deleteErr := memberDeleteEdit(index, thinkingFields[0])
				if deleteErr != nil {
					return deleteErr
				}
				edits = append(edits, edit)
			}
		}
	}

	systemFields := index.Find("/system")
	if len(systemFields) > 1 {
		return fmt.Errorf("system occurs %d times", len(systemFields))
	}
	markerCount := 0
	firstIsMarker := false
	securityTargets := make([]classifierSecurityTarget, 0, 1)
	textCandidates := make([]classifierTextCandidate, 0)
	if len(systemFields) == 1 {
		if systemFields[0].Type != bodyfile.JSONArray {
			return errors.New("system must be an array")
		}
		children := directArrayChildren(index, "/system")
		for _, child := range children {
			if child.Type != bodyfile.JSONObject {
				continue
			}
			childPath := fmt.Sprintf("/system/%d", child.ArrayIndex)
			childFields := directObjectChildren(index, childPath)
			texts := directFieldsByKey(childFields, "text")
			if len(texts) > 1 {
				return fmt.Errorf("%s/text occurs %d times", childPath, len(texts))
			}
			if len(texts) == 0 || texts[0].Type != bodyfile.JSONString {
				continue
			}
			textCandidates = append(textCandidates, classifierTextCandidate{index: child.ArrayIndex, field: texts[0]})
		}
		cursor, cursorErr := newBodyFieldCursor(request.Body)
		if cursorErr != nil {
			return cursorErr
		}
		defer cursor.Close()
		for _, candidate := range textCandidates {
			inspection, inspectErr := inspectClassifierSystemTextCursor(cursor, candidate.field)
			if inspectErr != nil {
				return inspectErr
			}
			if inspection.exactMarker {
				markerCount++
				if candidate.index == 0 {
					firstIsMarker = true
				}
			}
			if inspection.securityMonitor {
				if inspection.correctionCount > 1 {
					return errors.New("classifier correction marker is duplicated")
				}
				if inspection.correctionCount == 1 && !inspection.startsCorrection {
					return errors.New("classifier correction marker is out of order")
				}
				securityTargets = append(securityTargets, classifierSecurityTarget{
					field:     candidate.field,
					corrected: inspection.startsCorrection,
				})
			}
		}
	} else {
		// A classifier body without a system array cannot contain the original
		// security-monitor text, so there is no safe correction target.
		return errors.New("classifier system array is missing")
	}
	if markerCount > 1 || (markerCount == 1 && !firstIsMarker) {
		return errors.New("identity marker is duplicated or out of order")
	}
	if len(securityTargets) != 1 {
		return fmt.Errorf("expected one classifier security-monitor text, found %d", len(securityTargets))
	}
	hasCorrection := securityTargets[0].corrected
	if markerCount == 0 && hasCorrection || markerCount == 1 && !hasCorrection {
		return errors.New("classifier marker/correction is only partially present")
	}
	if markerCount == 0 {
		markerValue, marshalErr := classifierMarkerValue()
		if marshalErr != nil {
			return marshalErr
		}
		markerEdit, editErr := insertionAtArrayStart(index, "/system", markerValue)
		if editErr != nil {
			return editErr
		}
		edits = append(edits, markerEdit)
		correctionBytes := jsonStringBytes(AnyRouterCorrectionText)
		// Insert only the escaped string payload; preserve the original
		// security string's quote and all bytes after it.
		correctionEdit := bodyfile.Edit{
			Start:       securityTargets[0].field.StringRange.Start + 1,
			End:         securityTargets[0].field.StringRange.Start + 1,
			Replacement: correctionBytes[1 : len(correctionBytes)-1],
		}
		edits = append(edits, correctionEdit)
	}
	if len(edits) == 0 {
		return nil
	}
	return replaceBody(request, edits)
}

type classifierSecurityTarget struct {
	field     bodyfile.Field
	corrected bool
}

type classifierTextCandidate struct {
	index int
	field bodyfile.Field
}

type classifierTextInspection struct {
	exactMarker      bool
	securityMonitor  bool
	startsCorrection bool
	correctionCount  int
}

// inspectClassifierSystemText scans the selected JSON string while retaining
// only a bounded prefix and one correction-sized rolling window. This keeps a
// caller-provided, arbitrarily large system block out of memory while still
// validating marker/correction state exactly.
func inspectClassifierSystemText(body bodyfile.Body, field bodyfile.Field) (classifierTextInspection, error) {
	cursor, err := newBodyFieldCursor(body)
	if err != nil {
		return classifierTextInspection{}, err
	}
	defer cursor.Close()
	return inspectClassifierSystemTextCursor(cursor, field)
}

func inspectClassifierSystemTextCursor(cursor *bodyFieldCursor, field bodyfile.Field) (classifierTextInspection, error) {
	probeLimit := len(AnyRouterCorrectionText) + len(classifierSecurityPrefix)
	if markerLimit := len(AnyRouterMarkerText) + 1; markerLimit > probeLimit {
		probeLimit = markerLimit
	}
	prefix := make([]byte, 0, probeLimit)
	window := make([]byte, 0, len(AnyRouterCorrectionText))
	decodedLength := 0
	correctionCount := 0
	err := cursor.WithJSONString(field, func(scanner *jsonStringByteScanner) error {
		for {
			mapped, done, scanErr := scanner.next()
			if scanErr != nil {
				return scanErr
			}
			if done {
				break
			}
			for _, item := range mapped {
				decodedLength++
				if len(prefix) < probeLimit {
					prefix = append(prefix, item.value)
				}
				window = append(window, item.value)
				if len(window) > len(AnyRouterCorrectionText) {
					window = window[1:]
				}
				if len(window) == len(AnyRouterCorrectionText) && bytes.Equal(window, []byte(AnyRouterCorrectionText)) {
					correctionCount++
				}
			}
		}
		return nil
	})
	if err != nil {
		return classifierTextInspection{}, err
	}
	startsCorrection := bytes.HasPrefix(prefix, []byte(AnyRouterCorrectionText))
	securityOffset := 0
	if startsCorrection {
		securityOffset = len(AnyRouterCorrectionText)
	}
	securityMonitor := len(prefix) >= securityOffset+len(classifierSecurityPrefix) &&
		bytes.Equal(prefix[securityOffset:securityOffset+len(classifierSecurityPrefix)], []byte(classifierSecurityPrefix))
	return classifierTextInspection{
		exactMarker:      decodedLength == len(AnyRouterMarkerText) && bytes.Equal(prefix, []byte(AnyRouterMarkerText)),
		securityMonitor:  securityMonitor,
		startsCorrection: startsCorrection,
		correctionCount:  correctionCount,
	}, nil
}

func classifierMarkerValue() ([]byte, error) {
	// Keep the marker a regular Anthropic text block; no platform-specific
	// fields or metadata are introduced.
	encoded, _ := json.Marshal(AnyRouterMarkerText)
	return []byte(`{"type":"text","text":` + string(encoded) + `}`), nil
}

func newAnyRouterClassifierDefinition() PatchDefinition {
	return PatchDefinition{
		ID:           AnyRouterClassifierRequestID,
		Name:         "AnyRouter Classifier Request Compatibility",
		Description:  "Adds Claude Code identity and correction markers for classifier requests",
		RequestTypes: []RequestType{RequestTypeClassifier},
		Stages:       []Stage{StageRequest},
		RequestPaths: []string{"/thinking", "/thinking/type", "/system", "/system/*", "/system/*/text"},
		Conflicts:    []string{},
		Idempotence:  Idempotent,
		Factory: func(FactoryContext) (PatchInstance, error) {
			return newHooksInstance(Hooks{Request: anyRouterClassifierPatch{}}), nil
		},
	}
}

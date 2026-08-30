package patch

import (
	"errors"
	"fmt"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
)

const (
	AnyRouterSubagentThinkingID = "anyrouter-subagent-thinking"
	AnyRouterMarkerText         = "You are Claude Code, Anthropic's official CLI for Claude."
)

type anyRouterSubagentPatch struct{}

func (anyRouterSubagentPatch) ApplyRequest(_ PatchContext, request *MutableRequest) error {
	if request == nil || request.Body == nil {
		return errors.New("nil mutable request/body")
	}
	index, err := requestIndex(request)
	if err != nil {
		return err
	}
	thinking, present, err := onlyField(index, "/thinking")
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if thinking.Type != bodyfile.JSONObject {
		return errors.New("thinking must be an object")
	}
	types := index.Find("/thinking/type")
	if len(types) > 1 {
		return fmt.Errorf("thinking.type occurs %d times", len(types))
	}
	if len(types) == 0 {
		return nil
	}
	if types[0].Type != bodyfile.JSONString {
		return errors.New("thinking.type must be a string")
	}
	value, err := readFieldString(request.Body, types[0])
	if err != nil {
		return err
	}
	if value != "disabled" {
		return nil
	}
	return replaceBody(request, []bodyfile.Edit{{
		Start:       types[0].StringRange.Start,
		End:         types[0].StringRange.End,
		Replacement: jsonStringBytes("adaptive"),
	}})
}

func replaceBody(request *MutableRequest, edits []bodyfile.Edit) error {
	body, err := applyEdits(request.Body, edits)
	if err != nil {
		return err
	}
	request.Body = body
	return nil
}

func newAnyRouterSubagentDefinition() PatchDefinition {
	return PatchDefinition{
		ID:           AnyRouterSubagentThinkingID,
		Name:         "AnyRouter Subagent Thinking Compatibility",
		Description:  "Promotes disabled thinking on normal requests sent to compatible AnyRouter targets",
		RequestTypes: []RequestType{RequestTypeNormal},
		Stages:       []Stage{StageRequest},
		Conflicts:    []string{},
		Idempotence:  Idempotent,
		Factory: func(FactoryContext) (PatchInstance, error) {
			return newHooksInstance(Hooks{Request: anyRouterSubagentPatch{}}), nil
		},
	}
}

package flow

import "errors"

var (
	ErrInvalidExecutionPlan = errors.New("invalid execution plan")
	ErrUnknownRequestType   = errors.New("unknown request type")
	ErrDuplicatePlanner     = errors.New("duplicate planner")
	ErrMissingPlanner       = errors.New("request flow unavailable")
)

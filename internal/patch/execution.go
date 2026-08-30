package patch

import (
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
)

// HookError identifies the exact patch and stage that failed while retaining
// the underlying error for errors.Is/errors.As. Gateway diagnostics can expose
// PatchID and Stage without parsing an error string or observing request data.
type HookError struct {
	PatchID string
	Stage   Stage
	Err     error
}

func (e *HookError) Error() string {
	if e == nil {
		return "patch hook failed"
	}
	if e.Err == nil {
		return fmt.Sprintf("patch %q %s failed", e.PatchID, e.Stage)
	}
	return fmt.Sprintf("patch %q %s: %v", e.PatchID, e.Stage, e.Err)
}

func (e *HookError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Execution owns the independent instances for one upstream attempt. It is
// safe for a caller to close it exactly once; hooks themselves are serialized
// by the gateway's request/response lifecycle and are guarded against repeat
// invocation here as an additional per_execution invariant.
type Execution struct {
	mu sync.Mutex

	context PatchContext
	entries []executionEntry

	requestStarted  bool
	responseStarted bool
	closed          bool
}

type executionEntry struct {
	definition PatchDefinition
	instance   PatchInstance
	request    RequestPatch
	response   ResponsePatch
}

func newExecution(context PatchContext, entries []executionEntry) *Execution {
	return &Execution{
		context: context,
		entries: entries,
	}
}

// Context returns the immutable context captured at instance creation.
func (e *Execution) Context() PatchContext {
	if e == nil {
		return PatchContext{}
	}
	return e.context
}

// ApplyRequest executes applicable request hooks in configured order. The
// method marks the request phase before invoking user code, so a failed hook
// cannot be retried on the same execution and accidentally produce a partially
// transformed request.
func (e *Execution) ApplyRequest(context PatchContext, request *MutableRequest) error {
	if e == nil {
		return errors.New("nil patch execution")
	}
	if request == nil {
		return errors.New("nil mutable request")
	}
	if err := e.validateCallContext(context); err != nil {
		return err
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errors.New("patch execution is closed")
	}
	if e.requestStarted {
		e.mu.Unlock()
		return errors.New("request hooks already applied")
	}
	e.requestStarted = true
	entries := append([]executionEntry(nil), e.entries...)
	e.mu.Unlock()

	initialBody := request.Body
	for i := range entries {
		if entries[i].request == nil {
			continue
		}
		previousBody := request.Body
		hookErr := applyRequestSafely(entries[i].request, e.context, request)
		closeErr := closeIntermediateBody(initialBody, previousBody, request.Body)
		if hookErr != nil || closeErr != nil {
			return &HookError{
				PatchID: entries[i].definition.ID,
				Stage:   StageRequest,
				Err:     errors.Join(hookErr, wrapBodyCloseError(closeErr)),
			}
		}
	}
	return nil
}

// ApplyResponse executes applicable response hooks in reverse configured
// order. It is independent from request completion: a gateway may call it
// only for the terminal response, and any error is terminal (never retried).
func (e *Execution) ApplyResponse(context PatchContext, response *MutableResponse) error {
	if e == nil {
		return errors.New("nil patch execution")
	}
	if response == nil {
		return errors.New("nil mutable response")
	}
	if err := e.validateCallContext(context); err != nil {
		return err
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errors.New("patch execution is closed")
	}
	if e.responseStarted {
		e.mu.Unlock()
		return errors.New("response hooks already applied")
	}
	e.responseStarted = true
	entries := append([]executionEntry(nil), e.entries...)
	e.mu.Unlock()

	initialBody := response.Body
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].response == nil {
			continue
		}
		previousBody := response.Body
		hookErr := applyResponseSafely(entries[i].response, e.context, response)
		closeErr := closeIntermediateBody(initialBody, previousBody, response.Body)
		if hookErr != nil || closeErr != nil {
			return &HookError{
				PatchID: entries[i].definition.ID,
				Stage:   StageResponse,
				Err:     errors.Join(hookErr, wrapBodyCloseError(closeErr)),
			}
		}
	}
	return nil
}

func closeIntermediateBody(initial, previous, current bodyfile.Body) error {
	if previous == nil || sameBodyHandle(previous, initial) || sameBodyHandle(previous, current) {
		return nil
	}
	return previous.Close()
}

func wrapBodyCloseError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close intermediate body: %w", err)
}

func sameBodyHandle(left, right bodyfile.Body) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftValue, rightValue := reflect.ValueOf(left), reflect.ValueOf(right)
	if leftValue.Type() != rightValue.Type() {
		return false
	}
	if leftValue.Type().Comparable() {
		return leftValue.Interface() == rightValue.Interface()
	}
	if leftValue.Kind() == reflect.Pointer && rightValue.Kind() == reflect.Pointer {
		return leftValue.Pointer() == rightValue.Pointer()
	}
	return false
}

// ApplyRequestOnly and ApplyResponseOnly are convenience aliases for code
// that uses the context captured by NewInstance.
func (e *Execution) ApplyRequestOnly(request *MutableRequest) error {
	if e == nil {
		return errors.New("nil patch execution")
	}
	return e.ApplyRequest(e.context, request)
}

func (e *Execution) ApplyResponseOnly(response *MutableResponse) error {
	if e == nil {
		return errors.New("nil patch execution")
	}
	return e.ApplyResponse(e.context, response)
}

// Close releases request-scoped instances. It is idempotent and returns the
// first close error while still attempting every instance.
func (e *Execution) Close() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	entries := append([]executionEntry(nil), e.entries...)
	e.mu.Unlock()
	var first error
	for _, entry := range entries {
		if err := closeInstanceError(entry.instance); err != nil && first == nil {
			first = fmt.Errorf("patch %q close: %w", entry.definition.ID, err)
		}
	}
	return first
}

func (e *Execution) validateCallContext(context PatchContext) error {
	// A zero context is treated as "use the captured context" for convenient
	// direct callers; a non-zero context must match exactly so a request cannot
	// accidentally apply a plan compiled for another target/generation.
	if context == (PatchContext{}) {
		return nil
	}
	if context != e.context {
		return errors.New("patch context does not match execution")
	}
	return nil
}

func closeEntries(entries []executionEntry) {
	for _, entry := range entries {
		closeInstance(entry.instance)
	}
}

func closeInstance(instance PatchInstance) {
	_ = closeInstanceError(instance)
}

func closeInstanceError(instance PatchInstance) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("close patch instance panic: %v", recovered)
		}
	}()
	if closer, ok := instance.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

func instanceHooks(instance PatchInstance) (request RequestPatch, response ResponsePatch, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			request, response = nil, nil
			err = fmt.Errorf("inspect patch instance panic: %v", recovered)
		}
	}()
	request = instance.RequestPatch()
	response = instance.ResponsePatch()
	if isNilInterfaceValue(request) {
		request = nil
	}
	if isNilInterfaceValue(response) {
		response = nil
	}
	return request, response, nil
}

func invokeFactory(factory InstanceFactory, context FactoryContext) (instance PatchInstance, err error) {
	if factory == nil {
		return nil, ErrFactoryUnavailable
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			instance = nil
			err = fmt.Errorf("factory panic: %v", recovered)
		}
	}()
	instance, err = factory(context)
	if err != nil {
		return nil, err
	}
	if isNilPatchInstance(instance) {
		return nil, ErrFactoryUnavailable
	}
	return instance, nil
}

func isNilPatchInstance(instance PatchInstance) bool {
	return isNilInterfaceValue(instance)
}

func isNilInterfaceValue(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

func applyRequestSafely(hook RequestPatch, context PatchContext, request *MutableRequest) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("request hook panic: %v", recovered)
		}
	}()
	return hook.ApplyRequest(context, request)
}

func applyResponseSafely(hook ResponsePatch, context PatchContext, response *MutableResponse) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("response hook panic: %v", recovered)
		}
	}()
	return hook.ApplyResponse(context, response)
}

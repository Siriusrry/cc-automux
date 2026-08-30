package traffic

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
)

// HeaderView is the read-only header surface available to detectors.  It
// deliberately contains no credential, URL, TLS, or request mutation methods.
type HeaderView interface {
	Get(string) (string, bool)
	Values(string) []string
}

// MutableHeaderSet is the narrow read/write header surface used by later
// patch stages.  Defining it here lets patch and protocol packages share the
// contract without importing net/http.
type MutableHeaderSet interface {
	Get(string) (string, bool)
	Values(string) []string
	Set(string, string)
	Delete(string)
}

// HTTPHeaders adapts net/http headers to the read-only HeaderView contract.
// Values are copied on return so callers cannot mutate the source request.
type HTTPHeaders struct{ headers http.Header }

func NewHeaderView(headers http.Header) HeaderView { return NewHTTPHeaders(headers) }

func NewHTTPHeaders(headers http.Header) HTTPHeaders {
	return HTTPHeaders{headers: sanitizeHTTPHeaders(headers)}
}

func (h HTTPHeaders) Get(name string) (string, bool) {
	if isCredentialHeader(name) || h.headers == nil {
		return "", false
	}
	for key, values := range h.headers {
		if strings.EqualFold(key, name) {
			if len(values) == 0 {
				return "", true
			}
			return values[0], true
		}
	}
	return "", false
}

func (h HTTPHeaders) Values(name string) []string {
	if isCredentialHeader(name) || h.headers == nil {
		return []string{}
	}
	var result []string
	for key, values := range h.headers {
		if strings.EqualFold(key, name) {
			result = append(result, values...)
		}
	}
	if result == nil {
		return []string{}
	}
	return append([]string(nil), result...)
}

// HeaderMap is a convenient case-insensitive, immutable header snapshot for
// tests and non-HTTP callers.
type HeaderMap map[string][]string

func NewHeaderMap(values map[string][]string) HeaderMap {
	result := make(HeaderMap, len(values))
	for key, entries := range values {
		result[key] = append([]string(nil), entries...)
	}
	return result
}

func (h HeaderMap) Get(name string) (string, bool) {
	if isCredentialHeader(name) {
		return "", false
	}
	for key, values := range h {
		if strings.EqualFold(key, name) {
			if len(values) == 0 {
				return "", true
			}
			return values[0], true
		}
	}
	return "", false
}

func (h HeaderMap) Values(name string) []string {
	if isCredentialHeader(name) {
		return []string{}
	}
	for key, values := range h {
		if strings.EqualFold(key, name) {
			return append([]string(nil), values...)
		}
	}
	return []string{}
}

// EmptyHeaders is the zero-cost empty read-only header view.
type EmptyHeaders struct{}

func (EmptyHeaders) Get(string) (string, bool) { return "", false }
func (EmptyHeaders) Values(string) []string    { return []string{} }

// DetectionRequest is the immutable, pre-classification request fact set.
// RequestType is intentionally absent: detectors must not observe a mutable
// or partially selected type.
type DetectionRequest struct {
	CapturedBody      bodyfile.Body
	CapturedIndex     bodyfile.JSONIndex
	OriginalModel     string
	OriginalSessionID string
	Headers           HeaderView
}

// RequestView is the read-only view passed to each Detector.
type RequestView struct {
	Body              bodyfile.Body
	Index             bodyfile.JSONIndex
	OriginalModel     string
	OriginalSessionID string
	Headers           HeaderView
}

// View returns a defensive value view of d.  Body and Index are immutable
// handles; no request bytes are copied.
func (d DetectionRequest) View() RequestView {
	return RequestView{
		Body:              d.CapturedBody,
		Index:             d.CapturedIndex,
		OriginalModel:     d.OriginalModel,
		OriginalSessionID: d.OriginalSessionID,
		Headers:           normalizeHeaders(d.Headers),
	}
}

// NewDetectionRequest constructs the canonical pre-classification fact set.
func NewDetectionRequest(body bodyfile.Body, index bodyfile.JSONIndex, model, session string, headers HeaderView) DetectionRequest {
	return DetectionRequest{
		CapturedBody:      body,
		CapturedIndex:     index,
		OriginalModel:     model,
		OriginalSessionID: session,
		Headers:           normalizeHeaders(headers),
	}
}

// NewDetectionRequestFromView adapts a RequestView back to its ingress form.
func NewDetectionRequestFromView(view RequestView) DetectionRequest {
	return DetectionRequest{
		CapturedBody:      view.Body,
		CapturedIndex:     view.Index,
		OriginalModel:     view.OriginalModel,
		OriginalSessionID: view.OriginalSessionID,
		Headers:           normalizeHeaders(view.Headers),
	}
}

// IngressRequest is the immutable fact set after request type classification.
type IngressRequest struct {
	CapturedBody      bodyfile.Body
	CapturedIndex     bodyfile.JSONIndex
	OriginalModel     string
	OriginalSessionID string
	RequestType       RequestType
}

// NewIngressRequest adds a validated request type to a DetectionRequest.
func NewIngressRequest(d DetectionRequest, requestType RequestType) (IngressRequest, error) {
	if err := validateRequestType(requestType, true); err != nil {
		return IngressRequest{}, err
	}
	if d.CapturedBody == nil {
		return IngressRequest{}, fmt.Errorf("%w: captured body is nil", ErrInvalidIngressRequest)
	}
	if d.OriginalModel == "" {
		return IngressRequest{}, fmt.Errorf("%w: original model is empty", ErrInvalidIngressRequest)
	}
	return IngressRequest{
		CapturedBody:      d.CapturedBody,
		CapturedIndex:     d.CapturedIndex,
		OriginalModel:     d.OriginalModel,
		OriginalSessionID: d.OriginalSessionID,
		RequestType:       requestType,
	}, nil
}

// MustIngressRequest is a test/helper constructor that panics on invalid
// facts; production callers should use NewIngressRequest.
func MustIngressRequest(d DetectionRequest, requestType RequestType) IngressRequest {
	result, err := NewIngressRequest(d, requestType)
	if err != nil {
		panic(err)
	}
	return result
}

// Ingress returns a typed request or an error.  It is a method-form alias for
// NewIngressRequest and is convenient in detector tests.
func (d DetectionRequest) Ingress(requestType RequestType) (IngressRequest, error) {
	return NewIngressRequest(d, requestType)
}

// RequestPlan contains facts fixed by a Planner and reused for every target
// attempt.  It has no mutable body or scheduler state.
type RequestPlan struct {
	OriginalModel     string
	EffectiveModel    string
	OriginalSessionID string
	RequestType       RequestType
}

// NewRequestPlan validates and constructs a plan's public facts.
func NewRequestPlan(originalModel, effectiveModel, session string, requestType RequestType) (RequestPlan, error) {
	if originalModel == "" || effectiveModel == "" {
		return RequestPlan{}, fmt.Errorf("%w: model must not be empty", ErrInvalidIngressRequest)
	}
	if err := validateRequestType(requestType, true); err != nil {
		return RequestPlan{}, err
	}
	return RequestPlan{OriginalModel: originalModel, EffectiveModel: effectiveModel, OriginalSessionID: session, RequestType: requestType}, nil
}

// PreparedRequest is the one immutable request preparation shared by all
// provider attempts.
type PreparedRequest struct {
	Plan      RequestPlan
	BaseBody  bodyfile.Body
	BaseIndex bodyfile.JSONIndex
}

// NewPreparedRequest validates and constructs a prepared request.  It does
// not parse, classify, or copy body bytes.
func NewPreparedRequest(plan RequestPlan, body bodyfile.Body, index bodyfile.JSONIndex) (PreparedRequest, error) {
	if body == nil {
		return PreparedRequest{}, fmt.Errorf("%w: base body is nil", ErrInvalidIngressRequest)
	}
	if plan.OriginalModel == "" || plan.EffectiveModel == "" {
		return PreparedRequest{}, fmt.Errorf("%w: plan model is empty", ErrInvalidIngressRequest)
	}
	if err := validateRequestType(plan.RequestType, true); err != nil {
		return PreparedRequest{}, err
	}
	if err := index.ValidateBody(body); err != nil {
		return PreparedRequest{}, err
	}
	return PreparedRequest{Plan: plan, BaseBody: body, BaseIndex: index}, nil
}

// MustPreparedRequest is intended for tests and immutable fixture setup.
func MustPreparedRequest(plan RequestPlan, body bodyfile.Body, index bodyfile.JSONIndex) PreparedRequest {
	result, err := NewPreparedRequest(plan, body, index)
	if err != nil {
		panic(err)
	}
	return result
}

// Prepare is a method-form alias for NewPreparedRequest.
func (p RequestPlan) Prepare(body bodyfile.Body, index bodyfile.JSONIndex) (PreparedRequest, error) {
	return NewPreparedRequest(p, body, index)
}

func normalizeHeaders(headers HeaderView) HeaderView {
	if headers == nil {
		return EmptyHeaders{}
	}
	if _, alreadySanitized := headers.(sanitizedHeaders); alreadySanitized {
		return headers
	}
	return sanitizedHeaders{source: headers}
}

// sanitizedHeaders enforces the credential boundary even when a caller
// supplies a custom HeaderView implementation rather than NewHTTPHeaders.
// Detector code can inspect ordinary client metadata, but never gateway,
// management, or provider credential headers.
type sanitizedHeaders struct{ source HeaderView }

func (h sanitizedHeaders) Get(name string) (string, bool) {
	if isCredentialHeader(name) {
		return "", false
	}
	return h.source.Get(name)
}

func (h sanitizedHeaders) Values(name string) []string {
	if isCredentialHeader(name) {
		return []string{}
	}
	values := h.source.Values(name)
	if values == nil {
		return []string{}
	}
	return append([]string(nil), values...)
}

func sanitizeHTTPHeaders(headers http.Header) http.Header {
	result := make(http.Header)
	for key, values := range headers {
		if isCredentialHeader(key) {
			continue
		}
		result[key] = append([]string(nil), values...)
	}
	return result
}

func isCredentialHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "authorization", "proxy-authorization", "x-api-key", "api-key", "x-management-key", "x-gateway-key", "x-provider-key":
		return true
	default:
		return false
	}
}

// Ensure malformed zero values fail closed when callers explicitly validate.
func (d DetectionRequest) Validate() error {
	if d.CapturedBody == nil {
		return errors.New("traffic: captured body is nil")
	}
	if d.OriginalModel == "" {
		return errors.New("traffic: original model is empty")
	}
	return nil
}

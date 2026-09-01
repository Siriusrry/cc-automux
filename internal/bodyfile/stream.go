package bodyfile

// The scanner in this file is deliberately incremental and non-recursive.
// JSON nesting is represented by an explicit heap stack, so a peer cannot
// exhaust a goroutine stack by sending a deeply nested request. CaptureAndScan
// feeds the scanner from the same chunks that it writes to the sealed body;
// production callers therefore never need a second pass just to build an
// index.

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Siriusrry/cc-automux/internal/modelname"
)

// ScanSpec selects the JSON pointer paths retained by a scanner. Paths are
// RFC 6901 pointers; a `*` segment matches one object key or array index.
// Ancestors of selected paths are retained because byte ranges for a parent
// container are needed to perform safe edits. The scanner always validates the
// complete top-level JSON value, even when Paths is empty.
//
// RequireObject is currently enforced for every scan. SelectAll is only for
// the legacy Index/IndexObject helpers; selective production scans leave it
// false (including an empty Paths list).
type ScanSpec struct {
	Paths []string
	// RawMarkers lists byte sequences to find while the scanner consumes the
	// source stream.  Matches are recorded without decoding JSON strings; this
	// is useful for ingress predicates that require an exact, unescaped marker
	// in the original request bytes.
	RawMarkers    []string
	RequireModel  bool
	RequireObject bool
	SelectAll     bool
}

// CompiledScanSpec is an immutable scan contract with its selector trie,
// marker automata, and key-size bound prepared once. It is safe to share across
// concurrent scanners; each scanner receives only its own mutable parse and
// marker-match state.
type CompiledScanSpec struct {
	spec        ScanSpec
	trie        *selectorTrie
	keyRawLimit int
	rawMatchers []rawMarkerMatcher
}

// NewScanSpec validates and copies a selective path set.
func NewScanSpec(paths ...string) (ScanSpec, error) {
	spec := ScanSpec{Paths: append([]string(nil), paths...), RequireObject: true}
	if err := spec.Validate(); err != nil {
		return ScanSpec{}, err
	}
	return spec, nil
}

// Validate checks JSON-pointer syntax and rejects duplicate selectors.
func (s ScanSpec) Validate() error {
	seen := make(map[string]struct{}, len(s.Paths))
	for _, path := range s.Paths {
		if path == "" || path[0] != '/' {
			return fmt.Errorf("bodyfile: scan path %q is not an absolute JSON pointer", path)
		}
		if _, duplicate := seen[path]; duplicate {
			return fmt.Errorf("bodyfile: duplicate scan path %q", path)
		}
		seen[path] = struct{}{}
		if _, err := splitPointer(path); err != nil {
			return err
		}
	}
	markers := make(map[string]struct{}, len(s.RawMarkers))
	for _, marker := range s.RawMarkers {
		if marker == "" {
			return errors.New("bodyfile: raw marker must not be empty")
		}
		if _, duplicate := markers[marker]; duplicate {
			return fmt.Errorf("bodyfile: duplicate raw marker %q", marker)
		}
		markers[marker] = struct{}{}
	}
	return nil
}

// RequestScanSpec describes a strict Messages request scan. Model is always
// retained and decoded; remaining paths are the union required by request
// hooks and detectors.
func RequestScanSpec(paths ...string) (ScanSpec, error) {
	spec, err := NewScanSpec(paths...)
	if err != nil {
		return ScanSpec{}, err
	}
	spec.RequireModel = true
	return spec, nil
}

// RequestScanSpecWithRawMarkers constructs a request scan contract with the
// supplied selective paths and raw byte markers.  Marker matching is done in
// the same incremental pass as JSON validation and body capture.
func RequestScanSpecWithRawMarkers(paths []string, markers ...string) (ScanSpec, error) {
	spec, err := RequestScanSpec(paths...)
	if err != nil {
		return ScanSpec{}, err
	}
	spec.RawMarkers = append([]string(nil), markers...)
	if err := spec.Validate(); err != nil {
		return ScanSpec{}, err
	}
	return spec, nil
}

// RequestScanSpecWithMarkers is a concise alias for
// RequestScanSpecWithRawMarkers.
func RequestScanSpecWithMarkers(paths []string, markers ...string) (ScanSpec, error) {
	return RequestScanSpecWithRawMarkers(paths, markers...)
}

// WithRawMarkers returns a validated copy of a scan contract with raw marker
// matching enabled.  It never mutates the receiver's slices.
func (s ScanSpec) WithRawMarkers(markers ...string) (ScanSpec, error) {
	result := s
	result.RawMarkers = append([]string(nil), markers...)
	if err := result.Validate(); err != nil {
		return ScanSpec{}, err
	}
	return result, nil
}

// ResponseScanSpec describes a selective response scan. It validates one
// complete top-level object, but does not require or interpret model.
func ResponseScanSpec(paths ...string) (ScanSpec, error) {
	return NewScanSpec(paths...)
}

// ScanResult is not bound to a body until the body builder has sealed. This
// prevents a partially captured file from escaping as a valid index.
type ScanResult struct {
	Spec       ScanSpec
	Model      string
	ModelRange ByteRange
	Fields     []Field
	Containers []ContainerInfo
	ModelSeen  bool
	size       int64
	complete   bool
	digest     [sha256.Size]byte
	hasDigest  bool
	binding    *scanBinding
	source     Body
	rawMarkers map[string]bool
}

// Bind attaches an immutable body to a completed scan.
func (r ScanResult) Bind(body Body) (JSONIndex, error) {
	if body == nil {
		return JSONIndex{}, ErrNilBody
	}
	if !r.complete || r.size < 0 || body.Size() != r.size {
		return JSONIndex{}, ErrIndexBodyMismatch
	}
	if r.source != nil && !sameBody(r.source, body) {
		return JSONIndex{}, ErrIndexBodyMismatch
	}
	if r.binding != nil {
		bound, ok := body.(interface{ scanBinding() *scanBinding })
		if !ok || bound.scanBinding() != r.binding {
			return JSONIndex{}, ErrIndexBodyMismatch
		}
	} else if r.source == nil && r.hasDigest {
		actual, err := digestBody(body)
		if err != nil {
			return JSONIndex{}, errors.Join(ErrIndexBodyMismatch, err)
		}
		if actual != r.digest {
			return JSONIndex{}, ErrIndexBodyMismatch
		}
	}
	return r.bindScanned(body)
}

// bindScanned attaches a result to the body that was already consumed by the
// same scan. Capture/edit paths use this internal form so binding never causes
// a second body read. Public Bind retains a defensive identity/digest check for
// callers that finish a scanner independently.
func (r ScanResult) bindScanned(body Body) (JSONIndex, error) {
	if body == nil || !r.complete || r.size < 0 || body.Size() != r.size {
		return JSONIndex{}, ErrIndexBodyMismatch
	}
	return makeJSONIndex(body, r.Spec, r.Model, r.ModelRange, r.ModelSeen,
		append([]Field(nil), r.Fields...), append([]ContainerInfo(nil), r.Containers...), cloneRawMarkerMatches(r.rawMarkers)), nil
}

func digestBody(body Body) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	reader, err := openBodyReader(body)
	if err != nil {
		return result, err
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, reader)
	closeErr := reader.Close()
	if copyErr != nil || closeErr != nil {
		return result, errors.Join(copyErr, closeErr)
	}
	copy(result[:], hasher.Sum(nil))
	return result, nil
}

func makeJSONIndex(body Body, spec ScanSpec, model string, modelRange ByteRange, modelSeen bool, fields []Field, containers []ContainerInfo, rawMarkers map[string]bool) JSONIndex {
	byPath := make(map[string][]int, len(fields))
	children := make(map[string][]int)
	for index, field := range fields {
		byPath[field.Path] = append(byPath[field.Path], index)
		children[parentJSONPointer(field.Path)] = append(children[parentJSONPointer(field.Path)], index)
	}
	byContainer := make(map[string][]ContainerInfo, len(containers))
	for _, container := range containers {
		byContainer[container.Path] = append(byContainer[container.Path], container)
	}
	return JSONIndex{Model: model, ModelRange: modelRange, body: body, bodySize: body.Size(), fields: fields, byPath: byPath, containers: byContainer, children: children, spec: spec, valid: true, modelSeen: modelSeen, rawMarkers: cloneRawMarkerMatches(rawMarkers)}
}

// JSONScanner incrementally validates and indexes one top-level object.
// Write accepts arbitrary chunk boundaries. Finish performs EOF checks and is
// idempotent.
type JSONScanner struct {
	spec        ScanSpec
	trie        *selectorTrie
	digest      hash.Hash
	binding     *scanBinding
	keyRawLimit int
	rawMatchers []rawMarkerMatcher

	offset int64
	stack  []scanFrame
	token  *scanToken

	started  bool
	rootDone bool
	finished bool
	err      error

	modelSeen  bool
	model      string
	modelRange ByteRange
	fields     []Field
	containers []ContainerInfo
}

// rawMarkerMatcher tracks one literal byte sequence with a compact KMP
// automaton. It retains only the marker-sized prefix state, never the body.
type rawMarkerMatcher struct {
	pattern []byte
	failure []int
	matched int
	found   bool
}

func newRawMarkerMatcher(pattern string) rawMarkerMatcher {
	matcher := rawMarkerMatcher{pattern: []byte(pattern)}
	matcher.failure = make([]int, len(matcher.pattern))
	for i, prefix := 1, 0; i < len(matcher.pattern); i++ {
		for prefix > 0 && matcher.pattern[i] != matcher.pattern[prefix] {
			prefix = matcher.failure[prefix-1]
		}
		if matcher.pattern[i] == matcher.pattern[prefix] {
			prefix++
		}
		matcher.failure[i] = prefix
	}
	return matcher
}

func (m *rawMarkerMatcher) feed(value byte) {
	if m == nil || m.found || len(m.pattern) == 0 {
		return
	}
	for m.matched > 0 && value != m.pattern[m.matched] {
		m.matched = m.failure[m.matched-1]
	}
	if value == m.pattern[m.matched] {
		m.matched++
	}
	if m.matched == len(m.pattern) {
		m.found = true
		m.matched = m.failure[m.matched-1]
	}
}

func cloneRawMarkerMatches(values map[string]bool) map[string]bool {
	if len(values) == 0 {
		return map[string]bool{}
	}
	result := make(map[string]bool, len(values))
	for marker, found := range values {
		result[marker] = found
	}
	return result
}

// Scanner is a concise alias.
type Scanner = JSONScanner

// CompileScanSpec validates and compiles an immutable scanner contract.
func CompileScanSpec(spec ScanSpec) (*CompiledScanSpec, error) {
	// Object-only is the stable bodyfile contract. Treat a zero value as the
	// default rather than exposing a scalar mode callers could misuse.
	spec.RequireObject = true
	spec.Paths = append([]string(nil), spec.Paths...)
	spec.RawMarkers = append([]string(nil), spec.RawMarkers...)
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	rawMatchers := make([]rawMarkerMatcher, len(spec.RawMarkers))
	for index, marker := range spec.RawMarkers {
		rawMatchers[index] = newRawMarkerMatcher(marker)
	}
	return &CompiledScanSpec{
		spec:        spec,
		trie:        newSelectorTrie(spec.Paths, spec.SelectAll),
		keyRawLimit: selectorKeyRawLimit(spec),
		rawMatchers: rawMatchers,
	}, nil
}

// NewJSONScanner constructs an incremental scanner.
func NewJSONScanner(spec ScanSpec) (*JSONScanner, error) {
	compiled, err := CompileScanSpec(spec)
	if err != nil {
		return nil, err
	}
	return compiled.newScanner(), nil
}

func (s *CompiledScanSpec) newScanner() *JSONScanner {
	if s == nil {
		return nil
	}
	return &JSONScanner{
		spec:        s.spec,
		trie:        s.trie,
		digest:      sha256.New(),
		keyRawLimit: s.keyRawLimit,
		rawMatchers: append([]rawMarkerMatcher(nil), s.rawMatchers...),
	}
}

// NewScanner is an ergonomic alias.
func NewScanner(spec ScanSpec) (*JSONScanner, error) { return NewJSONScanner(spec) }

// Scan is an alias for Write.
func (s *JSONScanner) Scan(p []byte) (int, error) { return s.Write(p) }

// Write consumes p without retaining the chunk. A scalar delimiter is
// returned as unconsumed and immediately reprocessed by the enclosing frame.
func (s *JSONScanner) Write(p []byte) (int, error) {
	if s == nil {
		return 0, errors.New("bodyfile: nil JSON scanner")
	}
	if s.finished {
		if s.err != nil {
			return 0, s.err
		}
		return 0, errors.New("bodyfile: JSON scanner is finished")
	}
	if s.err != nil {
		return 0, s.err
	}
	if len(p) > 0 && s.digest != nil {
		_, _ = s.digest.Write(p)
	}
	// Matchers consume the original bytes exactly as supplied by the caller.
	// Running them before token handling preserves matches split across Write
	// calls and deliberately does not decode JSON escapes.
	for index := range s.rawMatchers {
		matcher := &s.rawMatchers[index]
		for _, value := range p {
			matcher.feed(value)
		}
	}
	for index := 0; index < len(p); {
		consumed, err := s.step(p[index])
		if err != nil {
			s.err = normalizeScanError(err, s.offset)
			return index, s.err
		}
		if consumed {
			index++
		}
	}
	return len(p), nil
}

// Finish signals EOF and checks token/container closure, model requirements,
// and the one-object top-level contract.
func (s *JSONScanner) Finish() (ScanResult, error) {
	if s == nil {
		return ScanResult{}, errors.New("bodyfile: nil JSON scanner")
	}
	if s.finished {
		if s.err != nil {
			return s.fail(s.err)
		}
		return s.result(), nil
	}
	s.finished = true
	if s.err != nil {
		return ScanResult{}, s.err
	}
	if s.token != nil {
		switch s.token.kind {
		case tokenLiteral:
			if !s.token.literalComplete {
				return s.fail(ErrInvalidJSON)
			}
			s.finishToken()
		case tokenNumber:
			if !numberTerminal(s.token.numberState) {
				return s.fail(fmt.Errorf("%w: incomplete number", ErrInvalidJSON))
			}
			s.finishToken()
		default:
			return s.fail(fmt.Errorf("%w: unterminated string", ErrInvalidJSON))
		}
		if s.err != nil {
			return s.fail(s.err)
		}
	}
	if !s.started || !s.rootDone || len(s.stack) != 0 {
		return s.fail(ErrInvalidJSON)
	}
	if s.spec.RequireModel && !s.modelSeen {
		return s.fail(ErrModelMissing)
	}
	return s.result(), nil
}

// Close is an io.Closer-style spelling that discards the result.
func (s *JSONScanner) Close() error {
	_, err := s.Finish()
	return err
}

func (s *JSONScanner) result() ScanResult {
	rawMarkers := make(map[string]bool, len(s.rawMatchers))
	for index, matcher := range s.rawMatchers {
		if index < len(s.spec.RawMarkers) {
			rawMarkers[s.spec.RawMarkers[index]] = matcher.found
		}
	}
	result := ScanResult{
		Spec: func() ScanSpec {
			spec := s.spec
			spec.Paths = append([]string(nil), s.spec.Paths...)
			spec.RawMarkers = append([]string(nil), s.spec.RawMarkers...)
			return spec
		}(),
		Model:      s.model,
		ModelRange: s.modelRange,
		Fields:     append([]Field(nil), s.fields...),
		Containers: append([]ContainerInfo(nil), s.containers...),
		ModelSeen:  s.modelSeen,
		size:       s.offset,
		complete:   true,
	}
	if s.digest != nil {
		copy(result.digest[:], s.digest.Sum(nil))
		result.hasDigest = true
	}
	result.binding = s.binding
	result.rawMarkers = rawMarkers
	return result
}

func (s *JSONScanner) fail(err error) (ScanResult, error) {
	s.err = normalizeScanError(err, s.offset)
	return ScanResult{}, s.err
}

// step consumes one byte. false means the byte is a scalar delimiter and
// must be passed again to the parent frame.
func (s *JSONScanner) step(value byte) (bool, error) {
	if s.token != nil {
		return s.consumeToken(value)
	}
	if s.rootDone {
		if isJSONWhitespace(value) {
			s.offset++
			return true, nil
		}
		return true, ErrTrailingJSON
	}
	if !s.started {
		if isJSONWhitespace(value) {
			s.offset++
			return true, nil
		}
		if value != '{' {
			return true, fmt.Errorf("%w: top-level JSON value must be an object", ErrInvalidJSON)
		}
		s.started = true
		rootStart := s.offset
		s.offset++
		s.pushContainer(value, valueContext{start: rootStart, memberStart: rootStart, arrayIndex: -1, depth: -1, states: []*selectorNode{s.trie.root}})
		return true, nil
	}
	if len(s.stack) == 0 {
		return true, ErrInvalidJSON
	}
	frame := &s.stack[len(s.stack)-1]
	switch frame.state {
	case objectKeyOrEnd:
		if isJSONWhitespace(value) {
			s.offset++
			return true, nil
		}
		if value == '}' {
			if frame.afterComma {
				return true, errors.New("trailing object comma")
			}
			s.offset++
			return true, s.closeContainer()
		}
		if value != '"' {
			return true, errors.New("object key must be a string")
		}
		s.noteNextSibling(frame, s.offset)
		s.offset++
		frame.state = objectColon
		frame.pending = valueContext{memberStart: s.offset - 1, arrayIndex: -1, depth: frame.depth + 1}
		s.token = newKeyToken(s.offset-1, s.keyRawLimitFor(frame))
		return true, nil
	case objectColon:
		if isJSONWhitespace(value) {
			s.offset++
			return true, nil
		}
		if value != ':' {
			return true, errors.New("object member must contain ':'")
		}
		s.offset++
		frame.state = objectValue
		return true, nil
	case objectValue:
		if isJSONWhitespace(value) {
			s.offset++
			return true, nil
		}
		ctx := frame.pending
		ctx.start = s.offset
		return s.startValue(value, ctx)
	case objectCommaOrEnd:
		if isJSONWhitespace(value) {
			s.offset++
			return true, nil
		}
		switch value {
		case ',':
			s.offset++
			frame.state = objectKeyOrEnd
			frame.afterComma = true
			return true, nil
		case '}':
			s.offset++
			return true, s.closeContainer()
		default:
			return true, errors.New("expected ',' or '}'")
		}
	case arrayValueOrEnd:
		if isJSONWhitespace(value) {
			s.offset++
			return true, nil
		}
		if value == ']' {
			s.offset++
			return true, s.closeContainer()
		}
		s.noteNextSibling(frame, s.offset)
		ctx := s.makeArrayContext(frame, frame.nextIndex, s.offset)
		return s.startValue(value, ctx)
	case arrayValueRequired:
		if isJSONWhitespace(value) {
			s.offset++
			return true, nil
		}
		if value == ']' {
			return true, errors.New("trailing array comma")
		}
		s.noteNextSibling(frame, s.offset)
		ctx := s.makeArrayContext(frame, frame.nextIndex, s.offset)
		return s.startValue(value, ctx)
	case arrayCommaOrEnd:
		if isJSONWhitespace(value) {
			s.offset++
			return true, nil
		}
		switch value {
		case ',':
			s.offset++
			frame.state = arrayValueRequired
			return true, nil
		case ']':
			s.offset++
			return true, s.closeContainer()
		default:
			return true, errors.New("expected ',' or ']'")
		}
	default:
		return true, ErrInvalidJSON
	}
}

type frameState uint8

const (
	objectKeyOrEnd frameState = iota
	objectColon
	objectValue
	objectCommaOrEnd
	arrayValueOrEnd
	arrayValueRequired
	arrayCommaOrEnd
)

type scanFrame struct {
	kind       byte
	state      frameState
	afterComma bool
	start      int64
	ctx        valueContext
	path       *scanPath
	states     []*selectorNode
	depth      int
	selected   bool
	record     bool

	childCount      int
	firstChildStart int64
	lastChildEnd    int64
	nextIndex       int
	pending         valueContext
	deletePending   int
}

type valueContext struct {
	start       int64
	memberStart int64
	path        *scanPath
	states      []*selectorNode
	selected    bool
	record      bool
	key         string
	keyRange    ByteRange
	arrayIndex  int
	depth       int
	isModel     bool
}

type scanTokenKind uint8

const (
	tokenKey scanTokenKind = iota
	tokenString
	tokenLiteral
	tokenNumber
)

type scanToken struct {
	kind  scanTokenKind
	start int64
	ctx   valueContext

	escaped       bool
	unicodeDigits int
	unicodeValue  uint16
	raw           []byte // object keys and the required model only
	rawLimit      int
	rawOverflow   bool

	modelDecodedBytes  int
	modelHighSurrogate uint16
	modelUTF8Pending   [utf8.UTFMax]byte
	modelUTF8Length    int

	literal         string
	literalPos      int
	literalComplete bool
	numberState     numberState
}

func newKeyToken(start int64, rawLimit int) *scanToken {
	token := &scanToken{kind: tokenKey, start: start, rawLimit: rawLimit}
	token.appendRaw('"')
	return token
}

func (t *scanToken) appendRaw(value byte) {
	if t == nil {
		return
	}
	if t.rawLimit == 0 || len(t.raw) < t.rawLimit {
		t.raw = append(t.raw, value)
		return
	}
	// Keep validating a long key, but stop retaining its spelling once it
	// cannot match any bounded selector. Wildcard selectors opt into an
	// unbounded token because their key is itself a selected field.
	t.rawOverflow = true
}

func (s *JSONScanner) startValue(first byte, ctx valueContext) (bool, error) {
	parent := &s.stack[len(s.stack)-1]
	if parent.kind == '{' {
		parent.state = objectCommaOrEnd
	} else {
		parent.state = arrayCommaOrEnd
	}
	switch first {
	case '"':
		s.offset++
		var raw []byte
		rawLimit := 0
		if ctx.isModel {
			raw = append(raw, '"')
			// A valid JSON spelling of at most MaxBytes decoded UTF-8 bytes
			// needs at most six source bytes per decoded byte, plus quotes.
			rawLimit = modelname.MaxBytes*6 + 2
		}
		s.token = &scanToken{kind: tokenString, start: ctx.start, ctx: ctx, raw: raw, rawLimit: rawLimit}
		return true, nil
	case '{', '[':
		s.offset++
		s.pushContainer(first, ctx)
		return true, nil
	case 't', 'f', 'n':
		s.offset++
		literal := map[byte]string{'t': "true", 'f': "false", 'n': "null"}[first]
		s.token = &scanToken{kind: tokenLiteral, start: ctx.start, ctx: ctx, literal: literal, literalPos: 1, literalComplete: len(literal) == 1}
		return true, nil
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		s.offset++
		state, ok := initialNumberState(first)
		if !ok {
			return true, errors.New("invalid number")
		}
		s.token = &scanToken{kind: tokenNumber, start: ctx.start, ctx: ctx, numberState: state}
		return true, nil
	default:
		return true, fmt.Errorf("unexpected JSON value byte %q", first)
	}
}

func (s *JSONScanner) consumeToken(value byte) (bool, error) {
	switch s.token.kind {
	case tokenKey, tokenString:
		return s.consumeStringToken(value)
	case tokenLiteral:
		return s.consumeLiteralToken(value)
	case tokenNumber:
		return s.consumeNumberToken(value)
	default:
		return true, ErrInvalidJSON
	}
}

func (s *JSONScanner) consumeStringToken(value byte) (bool, error) {
	token := s.token
	if token.unicodeDigits > 0 {
		if !isHexDigit(value) {
			return true, errors.New("invalid unicode escape digit")
		}
		digit := hexDigitValue(value)
		token.unicodeValue = token.unicodeValue<<4 | uint16(digit)
		token.unicodeDigits--
		if token.unicodeDigits == 0 {
			if err := consumeModelUnicodeUnit(token, token.unicodeValue); err != nil {
				return true, err
			}
		}
		if token.kind == tokenKey || token.ctx.isModel {
			token.appendRaw(value)
		}
		s.offset++
		return true, nil
	}
	if token.escaped {
		token.escaped = false
		switch value {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			if err := flushModelHighSurrogate(token); err != nil {
				return true, err
			}
			if err := addModelDecodedBytes(token, 1); err != nil {
				return true, err
			}
		case 'u':
			token.unicodeDigits = 4
			token.unicodeValue = 0
		default:
			return true, errors.New("invalid string escape")
		}
		if token.kind == tokenKey || token.ctx.isModel {
			token.appendRaw(value)
		}
		s.offset++
		return true, nil
	}
	if value == '"' {
		if err := finishModelDecodedString(token); err != nil {
			return true, err
		}
		if token.kind == tokenKey || token.ctx.isModel {
			token.appendRaw(value)
		}
		s.offset++
		s.finishToken()
		if s.err != nil {
			return true, normalizeScanError(s.err, s.offset)
		}
		return true, nil
	}
	if value == '\\' {
		if err := finishModelRawRune(token); err != nil {
			return true, err
		}
		token.escaped = true
		if token.kind == tokenKey || token.ctx.isModel {
			token.appendRaw(value)
		}
		s.offset++
		return true, nil
	}
	if value < 0x20 {
		return true, errors.New("unescaped control character in string")
	}
	if err := consumeModelRawUTF8Byte(token, value); err != nil {
		return true, err
	}
	if token.kind == tokenKey || token.ctx.isModel {
		token.appendRaw(value)
	}
	s.offset++
	return true, nil
}

func hexDigitValue(value byte) byte {
	switch {
	case value >= '0' && value <= '9':
		return value - '0'
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10
	default:
		return value - 'A' + 10
	}
}

func addModelDecodedBytes(token *scanToken, count int) error {
	if token == nil || !token.ctx.isModel {
		return nil
	}
	if count < 0 || token.modelDecodedBytes > modelname.MaxBytes-count {
		return ErrModelTooLong
	}
	token.modelDecodedBytes += count
	return nil
}

func consumeModelRawUTF8Byte(token *scanToken, value byte) error {
	if token == nil || !token.ctx.isModel {
		return nil
	}
	if token.modelHighSurrogate != 0 {
		if err := flushModelHighSurrogate(token); err != nil {
			return err
		}
	}
	if token.modelUTF8Length >= len(token.modelUTF8Pending) {
		return errors.New("invalid UTF-8 in model")
	}
	token.modelUTF8Pending[token.modelUTF8Length] = value
	token.modelUTF8Length++
	pending := token.modelUTF8Pending[:token.modelUTF8Length]
	if !utf8.FullRune(pending) {
		return nil
	}
	runeValue, size := utf8.DecodeRune(pending)
	if runeValue == utf8.RuneError && size == 1 || size != len(pending) {
		return errors.New("invalid UTF-8 in model")
	}
	token.modelUTF8Length = 0
	return addModelDecodedBytes(token, size)
}

func finishModelRawRune(token *scanToken) error {
	if token != nil && token.ctx.isModel && token.modelUTF8Length != 0 {
		return errors.New("invalid UTF-8 in model")
	}
	return nil
}

func consumeModelUnicodeUnit(token *scanToken, unit uint16) error {
	if token == nil || !token.ctx.isModel {
		return nil
	}
	if token.modelHighSurrogate != 0 {
		if unit >= 0xdc00 && unit <= 0xdfff {
			runeValue := utf16.DecodeRune(rune(token.modelHighSurrogate), rune(unit))
			token.modelHighSurrogate = 0
			return addModelDecodedBytes(token, utf8.RuneLen(runeValue))
		}
		if err := flushModelHighSurrogate(token); err != nil {
			return err
		}
	}
	switch {
	case unit >= 0xd800 && unit <= 0xdbff:
		token.modelHighSurrogate = unit
		return nil
	case unit >= 0xdc00 && unit <= 0xdfff:
		return addModelDecodedBytes(token, utf8.RuneLen(utf8.RuneError))
	default:
		return addModelDecodedBytes(token, utf8.RuneLen(rune(unit)))
	}
}

func flushModelHighSurrogate(token *scanToken) error {
	if token == nil || !token.ctx.isModel || token.modelHighSurrogate == 0 {
		return nil
	}
	token.modelHighSurrogate = 0
	return addModelDecodedBytes(token, utf8.RuneLen(utf8.RuneError))
}

func finishModelDecodedString(token *scanToken) error {
	if err := finishModelRawRune(token); err != nil {
		return err
	}
	return flushModelHighSurrogate(token)
}

func (s *JSONScanner) consumeLiteralToken(value byte) (bool, error) {
	token := s.token
	if token.literalComplete {
		if isJSONDelimiter(value) {
			s.finishToken()
			return false, nil
		}
		return true, errors.New("invalid literal boundary")
	}
	if token.literalPos >= len(token.literal) || value != token.literal[token.literalPos] {
		return true, errors.New("invalid literal")
	}
	token.literalPos++
	token.literalComplete = token.literalPos == len(token.literal)
	s.offset++
	return true, nil
}

func (s *JSONScanner) consumeNumberToken(value byte) (bool, error) {
	token := s.token
	next, ok, terminal := advanceNumber(token.numberState, value)
	if terminal {
		if numberTerminal(token.numberState) && isJSONDelimiter(value) {
			s.finishToken()
			return false, nil
		}
		return true, errors.New("invalid number")
	}
	if !ok {
		return true, errors.New("invalid number")
	}
	token.numberState = next
	s.offset++
	return true, nil
}

func (s *JSONScanner) finishToken() {
	token := s.token
	s.token = nil
	if token.kind == tokenKey {
		frame := &s.stack[len(s.stack)-1]
		if token.rawOverflow {
			frame.pending = s.makeObjectContext(frame, "", ByteRange{}, token.start, true)
			frame.afterComma = false
			return
		}
		var key string
		if err := json.Unmarshal(token.raw, &key); err != nil {
			s.err = err
			return
		}
		frame.pending = s.makeObjectContext(frame, key, ByteRange{Start: token.start, End: s.offset}, token.start, false)
		frame.afterComma = false
		return
	}
	var typ JSONValueType
	var stringRange ByteRange
	var stringValue string
	switch token.kind {
	case tokenString:
		typ = JSONString
		stringRange = ByteRange{Start: token.start, End: s.offset}
		if token.ctx.isModel {
			if err := json.Unmarshal(token.raw, &stringValue); err != nil {
				s.err = err
				return
			}
			if len(stringValue) > modelname.MaxBytes {
				s.err = ErrModelTooLong
				return
			}
		}
	case tokenLiteral:
		if token.literal == "null" {
			typ = JSONNull
		} else {
			typ = JSONBool
		}
	case tokenNumber:
		typ = JSONNumber
	default:
		s.err = ErrInvalidJSON
		return
	}
	s.completeValue(token.ctx, typ, stringRange, stringValue, s.offset)
}

func (s *JSONScanner) makeObjectContext(frame *scanFrame, key string, keyRange ByteRange, memberStart int64, keyOverflow bool) valueContext {
	states := transitionStates(frame.states, key)
	isModel := s.spec.RequireModel && len(s.stack) == 1 && key == "model"
	retain := !keyOverflow && (s.trie.selectAll || len(states) > 0 || isModel)
	selected := !keyOverflow && (s.trie.selectAll || hasTerminal(states) || isModel)
	if keyOverflow {
		states = nil
	}
	var path *scanPath
	if retain {
		path = appendPath(frame.path, key)
	} else {
		// The key was needed transiently to traverse the selector trie, but an
		// unrelated container may remain open for an arbitrarily long time. Do
		// not retain the decoded key or its range in that frame.
		key = ""
		keyRange = ByteRange{}
	}
	return valueContext{memberStart: memberStart, path: path, states: states, selected: selected, record: retain, key: key, keyRange: keyRange, arrayIndex: -1, depth: frame.depth + 1, isModel: isModel}
}

func (s *JSONScanner) makeArrayContext(frame *scanFrame, index int, start int64) valueContext {
	segment := strconv.Itoa(index)
	states := transitionStates(frame.states, segment)
	retain := s.trie.selectAll || len(states) > 0
	selected := s.trie.selectAll || hasTerminal(states)
	var path *scanPath
	if retain {
		path = appendPath(frame.path, segment)
	}
	return valueContext{start: start, memberStart: start, path: path, states: states, selected: selected, record: retain, arrayIndex: index, depth: frame.depth + 1}
}

func (s *JSONScanner) keyRawLimitFor(frame *scanFrame) int {
	if s == nil || s.trie == nil || s.trie.selectAll || frame == nil {
		return 0
	}
	// A wildcard below this object selects every key, including an arbitrarily
	// long one. Keep that spelling so the selected field can be addressed.
	for _, state := range frame.states {
		if state != nil && state.wildcard != nil {
			return 0
		}
	}
	return s.keyRawLimit
}

func (s *JSONScanner) pushContainer(kind byte, ctx valueContext) {
	frame := scanFrame{kind: kind, start: ctx.start, ctx: ctx, path: ctx.path, states: ctx.states, depth: ctx.depth, selected: ctx.selected, record: ctx.record, deletePending: -1}
	if kind == '{' {
		frame.state = objectKeyOrEnd
	} else {
		frame.state = arrayValueOrEnd
	}
	s.stack = append(s.stack, frame)
}

// noteNextSibling completes the comma-safe delete range of the immediately
// preceding selected child. It is invoked at the next sibling's first syntax
// byte, even when that sibling itself is not selected.
func (s *JSONScanner) noteNextSibling(parent *scanFrame, nextStart int64) {
	if parent == nil || parent.deletePending < 0 || parent.deletePending >= len(s.fields) {
		return
	}
	field := &s.fields[parent.deletePending]
	start := field.MemberRange.Start
	if field.ArrayIndex >= 0 {
		start = field.ValueRange.Start
	}
	field.DeleteRange = ByteRange{Start: start, End: nextStart}
	parent.deletePending = -1
}

func (s *JSONScanner) closeContainer() error {
	if len(s.stack) == 0 {
		return ErrInvalidJSON
	}
	frame := s.stack[len(s.stack)-1]
	end := s.offset
	typ := JSONObject
	if frame.kind == '[' {
		typ = JSONArray
	}
	// Root is always included because edit helpers need its closing offset;
	// selective child containers are included only when on a selected path.
	if len(s.stack) == 1 || frame.record {
		s.containers = append(s.containers, ContainerInfo{Path: pathString(frame.path), Type: typ, ValueRange: ByteRange{Start: frame.start, End: end}, ChildCount: frame.childCount, FirstChildStart: frame.firstChildStart, LastChildEnd: frame.lastChildEnd, CloseOffset: end - 1})
	}
	s.stack = s.stack[:len(s.stack)-1]
	if len(s.stack) == 0 {
		s.rootDone = true
		return nil
	}
	s.completeValue(frame.ctx, typ, ByteRange{}, "", end)
	return s.err
}

func (s *JSONScanner) completeValue(ctx valueContext, typ JSONValueType, stringRange ByteRange, stringValue string, end int64) {
	if ctx.isModel {
		if s.modelSeen {
			s.err = ErrModelRepeated
			return
		}
		s.modelSeen = true
		if typ != JSONString {
			s.err = ErrModelNotString
			return
		}
		if stringValue == "" {
			s.err = ErrModelEmpty
			return
		}
		s.model = stringValue
		s.modelRange = stringRange
	}
	var parent *scanFrame
	var previousEnd int64
	var hadPrevious bool
	if len(s.stack) > 0 {
		parent = &s.stack[len(s.stack)-1]
		previousEnd = parent.lastChildEnd
		hadPrevious = parent.childCount > 0
		parent.childCount++
		start := ctx.memberStart
		if parent.kind == '[' {
			start = ctx.start
			parent.nextIndex++
		}
		if parent.childCount == 1 {
			parent.firstChildStart = start
		}
		parent.lastChildEnd = end
	}
	// A selector may name a descendant without naming its container. Keep the
	// container boundary (needed for safe insertion/deletion), but do not add a
	// scalar that can never satisfy the descendant selector.
	if !ctx.selected && !(ctx.record && (typ == JSONObject || typ == JSONArray)) && !s.trie.selectAll {
		return
	}
	deleteStart := ctx.memberStart
	if ctx.arrayIndex >= 0 {
		deleteStart = ctx.start
	}
	if hadPrevious {
		deleteStart = previousEnd
	}
	s.fields = append(s.fields, Field{Path: pathString(ctx.path), Key: ctx.key, KeyRange: ctx.keyRange, MemberRange: ByteRange{Start: ctx.memberStart, End: end}, DeleteRange: ByteRange{Start: deleteStart, End: end}, ValueRange: ByteRange{Start: ctx.start, End: end}, StringRange: stringRange, Type: typ, ArrayIndex: ctx.arrayIndex, Depth: ctx.depth})
	if parent != nil {
		parent.deletePending = len(s.fields) - 1
	}
}

type scanPath struct {
	parent  *scanPath
	segment string
}

func appendPath(parent *scanPath, segment string) *scanPath {
	return &scanPath{parent: parent, segment: segment}
}

func pathString(path *scanPath) string {
	if path == nil {
		return ""
	}
	count := 0
	for item := path; item != nil; item = item.parent {
		count++
	}
	segments := make([]string, count)
	for item, index := path, count-1; item != nil; item, index = item.parent, index-1 {
		segments[index] = item.segment
	}
	var builder strings.Builder
	for _, segment := range segments {
		builder.WriteByte('/')
		builder.WriteString(escapePointer(segment))
	}
	return builder.String()
}

// selectorTrie is traversed once as the parser descends. No path lookup
// scans the complete field list.
type selectorTrie struct {
	root      *selectorNode
	selectAll bool
}

type selectorNode struct {
	children map[string]*selectorNode
	wildcard *selectorNode
	terminal bool
}

func newSelectorTrie(paths []string, selectAll bool) *selectorTrie {
	trie := &selectorTrie{root: &selectorNode{children: make(map[string]*selectorNode)}, selectAll: selectAll}
	for _, path := range paths {
		segments, _ := splitPointer(path)
		node := trie.root
		for _, segment := range segments {
			if segment == "*" {
				if node.wildcard == nil {
					node.wildcard = &selectorNode{children: make(map[string]*selectorNode)}
				}
				node = node.wildcard
				continue
			}
			if node.children[segment] == nil {
				node.children[segment] = &selectorNode{children: make(map[string]*selectorNode)}
			}
			node = node.children[segment]
		}
		node.terminal = true
	}
	return trie
}

func selectorKeyRawLimit(spec ScanSpec) int {
	maxDecoded := len("model")
	for _, path := range spec.Paths {
		segments, err := splitPointer(path)
		if err != nil {
			continue
		}
		for _, segment := range segments {
			if len(segment) > maxDecoded {
				maxDecoded = len(segment)
			}
		}
	}
	// A UTF-8 byte can be represented by at most one six-byte \uXXXX escape
	// (surrogate pairs are still covered by this conservative factor). Include
	// the surrounding quote and keep a useful minimum for short selectors.
	const escapeFactor = 8
	const minimum = 64
	maxInt := int(^uint(0) >> 1)
	if maxDecoded > (maxInt-2)/escapeFactor {
		return maxInt
	}
	limit := maxDecoded*escapeFactor + 2
	if limit < minimum {
		return minimum
	}
	return limit
}

func transitionStates(states []*selectorNode, segment string) []*selectorNode {
	if len(states) == 0 {
		return nil
	}
	result := make([]*selectorNode, 0, len(states)*2)
	seen := make(map[*selectorNode]struct{}, len(states)*2)
	for _, state := range states {
		if state == nil {
			continue
		}
		if child := state.children[segment]; child != nil {
			if _, ok := seen[child]; !ok {
				seen[child] = struct{}{}
				result = append(result, child)
			}
		}
		if child := state.wildcard; child != nil {
			if _, ok := seen[child]; !ok {
				seen[child] = struct{}{}
				result = append(result, child)
			}
		}
	}
	return result
}

func hasTerminal(states []*selectorNode) bool {
	for _, state := range states {
		if state != nil && state.terminal {
			return true
		}
	}
	return false
}

// splitPointer decodes ~0/~1 while retaining `*` as a wildcard segment.
func splitPointer(path string) ([]string, error) {
	if path == "" || path[0] != '/' {
		return nil, fmt.Errorf("bodyfile: invalid JSON pointer %q", path)
	}
	parts := strings.Split(path[1:], "/")
	for i, part := range parts {
		var builder strings.Builder
		for j := 0; j < len(part); j++ {
			if part[j] != '~' {
				builder.WriteByte(part[j])
				continue
			}
			if j+1 >= len(part) || (part[j+1] != '0' && part[j+1] != '1') {
				return nil, fmt.Errorf("bodyfile: invalid JSON pointer escape in %q", path)
			}
			if part[j+1] == '0' {
				builder.WriteByte('~')
			} else {
				builder.WriteByte('/')
			}
			j++
		}
		parts[i] = builder.String()
	}
	return parts, nil
}

func isJSONWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

func isJSONDelimiter(value byte) bool {
	return isJSONWhitespace(value) || value == ',' || value == ']' || value == '}'
}

func normalizeScanError(err error, offset int64) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrModelMissing) || errors.Is(err, ErrModelRepeated) || errors.Is(err, ErrModelNotString) || errors.Is(err, ErrModelEmpty) || errors.Is(err, ErrModelTooLong) || errors.Is(err, ErrTrailingJSON) {
		return err
	}
	if errors.Is(err, ErrInvalidJSON) {
		return err
	}
	return fmt.Errorf("%w at byte %d: %v", ErrInvalidJSON, offset, err)
}

// number lexer ------------------------------------------------------------

type numberState uint8

const (
	numberSign numberState = iota
	numberZero
	numberInteger
	numberDot
	numberFraction
	numberExponent
	numberExponentSign
	numberExponentDigits
)

func initialNumberState(first byte) (numberState, bool) {
	switch first {
	case '-':
		return numberSign, true
	case '0':
		return numberZero, true
	case '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return numberInteger, true
	default:
		return 0, false
	}
}

func numberTerminal(state numberState) bool {
	return state == numberZero || state == numberInteger || state == numberFraction || state == numberExponentDigits
}

func advanceNumber(state numberState, value byte) (numberState, bool, bool) {
	switch state {
	case numberSign:
		if value == '0' {
			return numberZero, true, false
		}
		if value >= '1' && value <= '9' {
			return numberInteger, true, false
		}
	case numberZero:
		if value == '.' {
			return numberDot, true, false
		}
		if value == 'e' || value == 'E' {
			return numberExponent, true, false
		}
		if isJSONDelimiter(value) {
			return state, true, true
		}
	case numberInteger:
		if value >= '0' && value <= '9' {
			return state, true, false
		}
		if value == '.' {
			return numberDot, true, false
		}
		if value == 'e' || value == 'E' {
			return numberExponent, true, false
		}
		if isJSONDelimiter(value) {
			return state, true, true
		}
	case numberDot:
		if value >= '0' && value <= '9' {
			return numberFraction, true, false
		}
	case numberFraction:
		if value >= '0' && value <= '9' {
			return state, true, false
		}
		if value == 'e' || value == 'E' {
			return numberExponent, true, false
		}
		if isJSONDelimiter(value) {
			return state, true, true
		}
	case numberExponent:
		if value == '+' || value == '-' {
			return numberExponentSign, true, false
		}
		if value >= '0' && value <= '9' {
			return numberExponentDigits, true, false
		}
	case numberExponentSign:
		if value >= '0' && value <= '9' {
			return numberExponentDigits, true, false
		}
	case numberExponentDigits:
		if value >= '0' && value <= '9' {
			return state, true, false
		}
		if isJSONDelimiter(value) {
			return state, true, true
		}
	}
	return state, false, false
}

// ScanBody is the explicit second-pass helper for already sealed bodies. It
// is useful to response hooks that intentionally buffer a terminal response;
// CaptureAndScan should be used for ingress whenever possible.
func ScanBody(body Body, spec ScanSpec) (result JSONIndex, returnErr error) {
	if body == nil {
		return JSONIndex{}, ErrNilBody
	}
	scanner, err := NewJSONScanner(spec)
	if err != nil {
		return JSONIndex{}, err
	}
	if binder, ok := body.(interface{ scanBinding() *scanBinding }); ok {
		scanner.binding = binder.scanBinding()
	}
	reader, err := openBodyReader(body)
	if err != nil {
		return JSONIndex{}, err
	}
	closed := false
	defer func() {
		if !closed {
			if closeErr := reader.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, closeErr)
			}
		}
	}()
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			if _, scanErr := scanner.Write(buffer[:n]); scanErr != nil {
				return JSONIndex{}, scanErr
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return JSONIndex{}, wrapLocalIO(LocalIORead, readErr)
		}
		if n == 0 {
			return JSONIndex{}, io.ErrNoProgress
		}
	}
	scanResult, err := scanner.Finish()
	if err != nil {
		return JSONIndex{}, err
	}
	closeErr := reader.Close()
	closed = true
	if closeErr != nil {
		return JSONIndex{}, closeErr
	}
	scanResult.source = body
	result, returnErr = scanResult.Bind(body)
	return result, returnErr
}

// IndexSelective validates and indexes only the requested paths.
func IndexSelective(body Body, spec ScanSpec) (JSONIndex, error) { return ScanBody(body, spec) }

// CaptureAndScan writes and scans source in lockstep. It never materialises
// the complete body and does not rescan the sealed file.
func CaptureAndScan(source io.Reader, spec ScanSpec, directory ...string) (body Body, index JSONIndex, returnErr error) {
	dir := ""
	if len(directory) > 0 {
		dir = directory[0]
	}
	return captureAndScanWithBuilder(source, spec, func() (Builder, error) { return NewBuilder(dir) })
}

// CaptureAndScanCompiled captures and scans with a precompiled immutable
// contract, avoiding per-request path/marker copying, validation, selector
// trie construction, and marker automaton construction.
func CaptureAndScanCompiled(source io.Reader, spec *CompiledScanSpec, directory ...string) (body Body, index JSONIndex, returnErr error) {
	dir := ""
	if len(directory) > 0 {
		dir = directory[0]
	}
	return captureAndScanWithCompiledBuilder(source, spec, func() (Builder, error) { return NewBuilder(dir) })
}

func captureAndScanWithBuilder(source io.Reader, spec ScanSpec, create func() (Builder, error)) (body Body, index JSONIndex, returnErr error) {
	compiled, err := CompileScanSpec(spec)
	if err != nil {
		return nil, JSONIndex{}, err
	}
	return captureAndScanWithCompiledBuilder(source, compiled, create)
}

func captureAndScanWithCompiledBuilder(source io.Reader, spec *CompiledScanSpec, create func() (Builder, error)) (body Body, index JSONIndex, returnErr error) {
	if source == nil {
		return nil, JSONIndex{}, fmt.Errorf("capture body: %w", ErrNilBody)
	}
	if spec == nil {
		return nil, JSONIndex{}, errors.New("bodyfile: nil compiled scan spec")
	}
	if create == nil {
		return nil, JSONIndex{}, errors.New("bodyfile: nil builder factory")
	}
	scanner := spec.newScanner()
	builder, err := create()
	if err != nil {
		return nil, JSONIndex{}, err
	}
	if binder, ok := builder.(interface{ scanBinding() *scanBinding }); ok {
		scanner.binding = binder.scanBinding()
	}
	sealed := false
	defer func() {
		if !sealed {
			if abortErr := builder.Abort(); abortErr != nil {
				returnErr = errors.Join(returnErr, abortErr)
			}
		}
	}()
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := source.Read(buffer)
		var scanErr error
		if n > 0 {
			_, scanErr = scanner.Write(buffer[:n])
			if scanErr != nil {
				if readErr != nil && !errors.Is(readErr, io.EOF) {
					return nil, JSONIndex{}, errors.Join(scanErr, &ReadError{Err: readErr})
				}
				return nil, JSONIndex{}, scanErr
			}
			written, writeErr := builder.Write(buffer[:n])
			if writeErr != nil {
				return nil, JSONIndex{}, &WriteError{Err: wrapLocalIO(LocalIOWrite, writeErr)}
			}
			if written != n {
				return nil, JSONIndex{}, &WriteError{Err: wrapLocalIO(LocalIOWrite, io.ErrShortWrite)}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) && scanErr == nil {
				break
			}
			if errors.Is(readErr, io.EOF) {
				return nil, JSONIndex{}, scanErr
			}
			return nil, JSONIndex{}, errors.Join(scanErr, &ReadError{Err: readErr})
		}
		if scanErr != nil {
			return nil, JSONIndex{}, scanErr
		}
		if n == 0 {
			return nil, JSONIndex{}, &ReadError{Err: io.ErrNoProgress}
		}
	}
	result, err := scanner.Finish()
	if err != nil {
		return nil, JSONIndex{}, err
	}
	body, err = builder.Seal()
	if err != nil {
		return nil, JSONIndex{}, fmt.Errorf("seal body: %w", err)
	}
	sealed = true
	result.source = body
	index, err = result.Bind(body)
	if err != nil {
		_ = body.Close()
		return nil, JSONIndex{}, err
	}
	return body, index, nil
}

func CaptureWithIndex(source io.Reader, spec ScanSpec, directory ...string) (Body, JSONIndex, error) {
	return CaptureAndScan(source, spec, directory...)
}

func CaptureIndexed(source io.Reader, spec ScanSpec, directory ...string) (Body, JSONIndex, error) {
	return CaptureAndScan(source, spec, directory...)
}

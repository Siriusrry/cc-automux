package bodyfile

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
)

// ByteRange is a half-open byte range [Start, End) in a Body.
type ByteRange struct {
	Start int64
	End   int64
}

// Len returns the number of bytes covered by the range.
func (r ByteRange) Len() int64 {
	if r.End < r.Start {
		return 0
	}
	return r.End - r.Start
}

// Contains reports whether offset lies within the half-open range.
func (r ByteRange) Contains(offset int64) bool { return offset >= r.Start && offset < r.End }

// JSONValueType identifies the syntax kind of an indexed value.
type JSONValueType string

const (
	JSONNull   JSONValueType = "null"
	JSONBool   JSONValueType = "boolean"
	JSONNumber JSONValueType = "number"
	JSONString JSONValueType = "string"
	JSONObject JSONValueType = "object"
	JSONArray  JSONValueType = "array"
)

// Field describes one object member or array value discovered by Index.  The
// index stores ranges and small scalar metadata only; it never stores a copy
// of the value bytes.
type Field struct {
	// Path is an RFC 6901-style JSON pointer.  Object keys use ~0/~1 escaping.
	Path string
	// Key is set for object members and empty for array elements/top-level value.
	Key string
	// KeyRange includes the JSON string delimiters for an object key.
	KeyRange ByteRange
	// MemberRange covers key through value (including neither surrounding
	// member comma nor object whitespace), and is useful for deletion edits.
	MemberRange ByteRange
	// ValueRange covers the complete JSON value, including string quotes.
	ValueRange ByteRange
	// StringRange is set for JSON strings and equals ValueRange for a string
	// value.  It is zero for all other value types.
	StringRange ByteRange
	Type        JSONValueType
	// ArrayIndex is the zero-based array index, or -1 for object members.
	ArrayIndex int
	Depth      int
}

// JSONIndex is a read-only structural index bound to one Body.  For an index
// built by Index, Model is the decoded unique top-level "model" member and
// ModelRange points at its JSON string. IndexObject leaves both fields empty.
//
// The exported scalar fields are snapshots.  The internal slices and body
// identity are never returned directly, so a caller cannot mutate the index
// or accidentally bind it to another Body.
type JSONIndex struct {
	Model      string
	ModelRange ByteRange

	body      Body
	bodySize  int64
	fields    []Field
	valid     bool
	modelSeen bool
}

var (
	ErrInvalidJSON       = errors.New("bodyfile: invalid JSON")
	ErrTrailingJSON      = errors.New("bodyfile: trailing JSON content")
	ErrModelMissing      = errors.New("bodyfile: model is required")
	ErrModelRepeated     = errors.New("bodyfile: model must not be repeated")
	ErrModelNotString    = errors.New("bodyfile: model must be a string")
	ErrModelEmpty        = errors.New("bodyfile: model must not be empty")
	ErrIndexBodyMismatch = errors.New("bodyfile: JSON index belongs to another body")
)

// Index performs one streaming Messages-request JSON scan. It requires a
// single top-level object with one non-empty string model.
func Index(body Body) (JSONIndex, error) { return buildIndex(body, true) }

// BuildIndex is the descriptive alias for Index.
func BuildIndex(body Body) (JSONIndex, error) { return Index(body) }

// IndexObject validates and indexes one general top-level JSON object without
// applying the Messages-request model rule. It is used for response bodies,
// where model can be absent, repeated, empty, or non-string without changing
// the JSON object's structural validity.
func IndexObject(body Body) (JSONIndex, error) { return buildIndex(body, false) }

func buildIndex(body Body, requireModel bool) (result JSONIndex, returnErr error) {
	if body == nil {
		return JSONIndex{}, ErrNilBody
	}
	reader, err := openBodyReader(body)
	if err != nil {
		return JSONIndex{}, err
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			result = JSONIndex{}
			returnErr = errors.Join(returnErr, closeErr)
		}
	}()

	parser := &jsonIndexer{
		reader:       bufio.NewReaderSize(reader, 32*1024),
		requireModel: requireModel,
	}
	if err := parser.scan(); err != nil {
		return JSONIndex{}, err
	}
	return JSONIndex{
		Model:      parser.model,
		ModelRange: parser.modelRange,
		body:       body,
		bodySize:   body.Size(),
		fields:     parser.fields,
		valid:      true,
		modelSeen:  parser.modelSeen,
	}, nil
}

// ParseIndex is an alias retained for callers that prefer parser terminology.
func ParseIndex(body Body) (JSONIndex, error) { return BuildIndex(body) }

// ValidateBody confirms that this index was built from body.  It is useful at
// patch boundaries where an index must not be reused after a body edit.
func (i JSONIndex) ValidateBody(body Body) error {
	if !i.valid || i.body == nil {
		return ErrIndexBodyMismatch
	}
	if body == nil || !sameBody(i.body, body) || body.Size() != i.bodySize {
		return ErrIndexBodyMismatch
	}
	return nil
}

// Fields returns a defensive copy of all discovered field descriptors.
func (i JSONIndex) Fields() []Field {
	if len(i.fields) == 0 {
		return []Field{}
	}
	return append([]Field(nil), i.fields...)
}

// Lookup returns the first field at path.  Paths use JSON pointer syntax;
// Lookup("/model") is therefore the common top-level model query.
func (i JSONIndex) Lookup(path string) (Field, bool) {
	for _, field := range i.fields {
		if field.Path == path {
			return field, true
		}
	}
	return Field{}, false
}

// Find returns every field at path (normally one, except repeated keys).
func (i JSONIndex) Find(path string) []Field {
	result := make([]Field, 0, 1)
	for _, field := range i.fields {
		if field.Path == path {
			result = append(result, field)
		}
	}
	return result
}

// FieldAt and Range are convenience aliases used by patch implementations.
func (i JSONIndex) FieldAt(path string) (Field, bool) { return i.Lookup(path) }

func (i JSONIndex) Range(path string) (ByteRange, bool) {
	field, ok := i.Lookup(path)
	if !ok {
		return ByteRange{}, false
	}
	return field.ValueRange, true
}

// ModelValue returns the decoded top-level model scalar.
func (i JSONIndex) ModelValue() string { return i.Model }

// OriginalModel is a descriptive alias for ModelValue.
func (i JSONIndex) OriginalModel() string { return i.Model }

// ModelBytesRange returns the exact byte range of the model JSON string.
func (i JSONIndex) ModelBytesRange() ByteRange { return i.ModelRange }

// Size returns the body size captured by this index.
func (i JSONIndex) Size() int64 { return i.bodySize }

// Body returns the immutable body this index was built from.  The returned
// handle exposes read-only operations only; it is provided for patch helpers
// that need to inspect delimiters while constructing an edit.  Callers must
// not close it independently of the request owner.
func (i JSONIndex) Body() Body { return i.body }

func sameBody(left, right Body) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	if l, ok := left.(interface{ bodyIdentity() *fileBody }); ok {
		if r, ok := right.(interface{ bodyIdentity() *fileBody }); ok {
			return l.bodyIdentity() == r.bodyIdentity()
		}
	}
	// Most test bodies are pointer-backed.  Avoid comparing a potentially
	// non-comparable dynamic value (for example a map-backed fake body).
	lv, rv := reflect.ValueOf(left), reflect.ValueOf(right)
	if lv.IsValid() && rv.IsValid() && lv.Type() == rv.Type() && lv.Type().Comparable() {
		return lv.Interface() == rv.Interface()
	}
	if lv.IsValid() && rv.IsValid() && lv.Kind() == reflect.Ptr && rv.Kind() == reflect.Ptr {
		return lv.Pointer() == rv.Pointer()
	}
	return false
}

type jsonIndexer struct {
	reader       *bufio.Reader
	offset       int64
	peekErr      error
	requireModel bool
	model        string
	modelRange   ByteRange
	modelSeen    bool
	fields       []Field
}

func (p *jsonIndexer) scan() error {
	if err := p.skipWhitespace(); err != nil {
		return p.invalid(err)
	}
	first, err := p.readByte()
	if err != nil {
		return p.invalid(err)
	}
	if first != '{' {
		return p.invalid(errors.New("top-level JSON value must be an object"))
	}
	if err := p.parseObject("", 0, true); err != nil {
		return err
	}
	if err := p.skipWhitespace(); err != nil {
		return p.invalid(err)
	}
	if _, err := p.reader.Peek(1); err == nil {
		return fmt.Errorf("%w at byte %d", ErrTrailingJSON, p.offset)
	} else if !errors.Is(err, io.EOF) {
		return p.invalid(err)
	}
	if p.requireModel && !p.modelSeen {
		return ErrModelMissing
	}
	return nil
}

func (p *jsonIndexer) parseObject(path string, depth int, topLevel bool) error {
	// The opening brace has already been consumed by the caller for the
	// top-level object; nested callers consume it in parseValue.
	if err := p.skipWhitespace(); err != nil {
		return p.invalid(err)
	}
	if next := p.peekByte(); next == '}' {
		_, _ = p.readByte()
		if topLevel && p.requireModel && !p.modelSeen {
			return ErrModelMissing
		}
		return nil
	}
	for {
		keyStart := p.offset
		key, keyRange, err := p.readString(true)
		if err != nil {
			return p.invalid(err)
		}
		if err := p.skipWhitespace(); err != nil {
			return p.invalid(err)
		}
		colon, err := p.readByte()
		if err != nil || colon != ':' {
			if err == nil {
				err = errors.New("object member must contain ':'")
			}
			return p.invalid(err)
		}
		if err := p.skipWhitespace(); err != nil {
			return p.invalid(err)
		}
		memberPath := path + "/" + escapePointer(key)
		value, err := p.parseValue(memberPath, depth+1)
		if err != nil {
			return err
		}
		member := value.field
		member.Path = memberPath
		member.Key = key
		member.KeyRange = keyRange
		member.MemberRange = ByteRange{Start: keyStart, End: value.end}
		member.Depth = depth
		p.fields = append(p.fields, member)

		if topLevel && p.requireModel && key == "model" {
			if p.modelSeen {
				return ErrModelRepeated
			}
			p.modelSeen = true
			if value.field.Type != JSONString {
				return ErrModelNotString
			}
			if value.stringValue == "" {
				return ErrModelEmpty
			}
			p.model = value.stringValue
			p.modelRange = value.field.StringRange
		}

		if err := p.skipWhitespace(); err != nil {
			return p.invalid(err)
		}
		next, err := p.readByte()
		if err != nil {
			return p.invalid(err)
		}
		switch next {
		case '}':
			if topLevel && p.requireModel && !p.modelSeen {
				return ErrModelMissing
			}
			return nil
		case ',':
			if err := p.skipWhitespace(); err != nil {
				return p.invalid(err)
			}
			// The next iteration will reject a trailing comma when it cannot
			// read a string key.
		default:
			return p.invalid(fmt.Errorf("expected ',' or '}', got %q", next))
		}
	}
}

type parsedValue struct {
	field       Field
	end         int64
	stringValue string
}

func (p *jsonIndexer) parseValue(path string, depth int) (parsedValue, error) {
	if err := p.skipWhitespace(); err != nil {
		return parsedValue{}, p.invalid(err)
	}
	start := p.offset
	next := p.peekByte()
	result := parsedValue{field: Field{Path: path, ValueRange: ByteRange{Start: start}, ArrayIndex: -1, Depth: depth}}
	switch next {
	case '"':
		// Only the top-level model is needed as a decoded scalar during the
		// foundational scan.  Other strings are validated byte-by-byte without
		// retaining potentially very large values in memory; patches can use
		// their recorded ranges to read a selected value later.
		capture := p.requireModel && path == "/model"
		value, stringRange, err := p.readString(capture)
		if err != nil {
			return parsedValue{}, p.invalid(err)
		}
		result.field.Type = JSONString
		result.field.StringRange = stringRange
		result.field.ValueRange.End = p.offset
		result.end = p.offset
		result.stringValue = value
		return result, nil
	case '{':
		_, _ = p.readByte()
		if err := p.parseObject(path, depth, false); err != nil {
			return parsedValue{}, err
		}
		result.field.Type = JSONObject
		result.field.ValueRange.End = p.offset
		result.end = p.offset
		return result, nil
	case '[':
		_, _ = p.readByte()
		if err := p.parseArray(path, depth); err != nil {
			return parsedValue{}, err
		}
		result.field.Type = JSONArray
		result.field.ValueRange.End = p.offset
		result.end = p.offset
		return result, nil
	case 't':
		if err := p.consumeLiteral("true"); err != nil {
			return parsedValue{}, p.invalid(err)
		}
		result.field.Type = JSONBool
	case 'f':
		if err := p.consumeLiteral("false"); err != nil {
			return parsedValue{}, p.invalid(err)
		}
		result.field.Type = JSONBool
	case 'n':
		if err := p.consumeLiteral("null"); err != nil {
			return parsedValue{}, p.invalid(err)
		}
		result.field.Type = JSONNull
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		if err := p.consumeNumber(); err != nil {
			return parsedValue{}, p.invalid(err)
		}
		result.field.Type = JSONNumber
	default:
		return parsedValue{}, p.invalid(fmt.Errorf("unexpected JSON value byte %q", next))
	}
	result.field.ValueRange.End = p.offset
	result.end = p.offset
	return result, nil
}

func (p *jsonIndexer) parseArray(path string, depth int) error {
	if err := p.skipWhitespace(); err != nil {
		return p.invalid(err)
	}
	if p.peekByte() == ']' {
		_, _ = p.readByte()
		return nil
	}
	index := 0
	for {
		memberPath := path + "/" + strconv.Itoa(index)
		value, err := p.parseValue(memberPath, depth+1)
		if err != nil {
			return err
		}
		field := value.field
		field.Path = memberPath
		field.ArrayIndex = index
		// Depth denotes the containing object/array level.  Object members
		// parsed by parseObject use that object's depth; array elements use the
		// array container's depth so a nested object's direct children are one
		// level deeper, matching the same invariant.
		field.Depth = depth
		p.fields = append(p.fields, field)
		index++
		if err := p.skipWhitespace(); err != nil {
			return p.invalid(err)
		}
		next, err := p.readByte()
		if err != nil {
			return p.invalid(err)
		}
		switch next {
		case ']':
			return nil
		case ',':
			if err := p.skipWhitespace(); err != nil {
				return p.invalid(err)
			}
		default:
			return p.invalid(fmt.Errorf("expected ',' or ']', got %q", next))
		}
	}
}

func (p *jsonIndexer) readString(capture bool) (string, ByteRange, error) {
	start := p.offset
	quote, err := p.readByte()
	if err != nil {
		return "", ByteRange{}, err
	}
	if quote != '"' {
		return "", ByteRange{}, fmt.Errorf("expected JSON string, got %q", quote)
	}
	var raw strings.Builder
	if capture {
		raw.Grow(32)
		raw.WriteByte('"')
	}
	for {
		value, err := p.readByte()
		if err != nil {
			return "", ByteRange{}, err
		}
		if capture {
			raw.WriteByte(value)
		}
		switch value {
		case '"':
			if !capture {
				return "", ByteRange{Start: start, End: p.offset}, nil
			}
			var decoded string
			err := json.Unmarshal([]byte(raw.String()), &decoded)
			if err != nil {
				return "", ByteRange{}, err
			}
			return decoded, ByteRange{Start: start, End: p.offset}, nil
		case '\\':
			escaped, err := p.readByte()
			if err != nil {
				return "", ByteRange{}, err
			}
			if capture {
				raw.WriteByte(escaped)
			}
			switch escaped {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				for j := 0; j < 4; j++ {
					digit, err := p.readByte()
					if err != nil {
						return "", ByteRange{}, err
					}
					if capture {
						raw.WriteByte(digit)
					}
					if !isHexDigit(digit) {
						return "", ByteRange{}, fmt.Errorf("invalid unicode escape digit %q", digit)
					}
				}
			default:
				return "", ByteRange{}, fmt.Errorf("invalid string escape %q", escaped)
			}
		default:
			if value < 0x20 {
				return "", ByteRange{}, errors.New("unescaped control character in string")
			}
		}
	}
}

func (p *jsonIndexer) consumeLiteral(literal string) error {
	for i := 0; i < len(literal); i++ {
		value, err := p.readByte()
		if err != nil {
			return err
		}
		if value != literal[i] {
			return fmt.Errorf("invalid literal %q", literal)
		}
	}
	return nil
}

func (p *jsonIndexer) consumeNumber() error {
	if p.peekByte() == '-' {
		_, _ = p.readByte()
	}
	first := p.peekByte()
	switch {
	case first == '0':
		_, _ = p.readByte()
		if next := p.peekByte(); next >= '0' && next <= '9' {
			return errors.New("leading zero in JSON number")
		}
	case first >= '1' && first <= '9':
		p.consumeDigits()
	default:
		return errors.New("invalid JSON number")
	}
	if p.peekByte() == '.' {
		_, _ = p.readByte()
		if next := p.peekByte(); next < '0' || next > '9' {
			return errors.New("fraction has no digits")
		}
		p.consumeDigits()
	}
	if next := p.peekByte(); next == 'e' || next == 'E' {
		_, _ = p.readByte()
		if sign := p.peekByte(); sign == '+' || sign == '-' {
			_, _ = p.readByte()
		}
		if next := p.peekByte(); next < '0' || next > '9' {
			return errors.New("exponent has no digits")
		}
		p.consumeDigits()
	}
	return nil
}

func (p *jsonIndexer) consumeDigits() {
	for {
		next := p.peekByte()
		if next < '0' || next > '9' {
			return
		}
		_, _ = p.readByte()
	}
}

func (p *jsonIndexer) skipWhitespace() error {
	for {
		next, err := p.reader.Peek(1)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch next[0] {
		case ' ', '\t', '\r', '\n':
			_, _ = p.readByte()
		default:
			return nil
		}
	}
}

func (p *jsonIndexer) peekByte() byte {
	bytes, err := p.reader.Peek(1)
	if err != nil {
		if !errors.Is(err, io.EOF) && p.peekErr == nil {
			p.peekErr = err
		}
		return 0
	}
	if len(bytes) == 0 {
		return 0
	}
	return bytes[0]
}

func (p *jsonIndexer) readByte() (byte, error) {
	value, err := p.reader.ReadByte()
	if err != nil {
		return 0, err
	}
	p.offset++
	return value, nil
}

func (p *jsonIndexer) invalid(err error) error {
	if p.peekErr != nil {
		return p.peekErr
	}
	if errors.Is(err, ErrLocalIO) {
		return err
	}
	if err == nil {
		err = ErrInvalidJSON
	}
	if errors.Is(err, ErrModelMissing) || errors.Is(err, ErrModelRepeated) || errors.Is(err, ErrModelNotString) || errors.Is(err, ErrModelEmpty) || errors.Is(err, ErrTrailingJSON) {
		return err
	}
	return fmt.Errorf("%w at byte %d: %v", ErrInvalidJSON, p.offset, err)
}

func escapePointer(value string) string {
	value = strings.ReplaceAll(value, "~", "~0")
	return strings.ReplaceAll(value, "/", "~1")
}

func isHexDigit(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'f') || (value >= 'A' && value <= 'F')
}

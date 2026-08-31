package bodyfile

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/Siriusrry/cc-automux/internal/modelname"
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

// Field describes one selected object member or array value. The index stores
// byte ranges and small scalar metadata only; it never stores value bytes.
type Field struct {
	// Path is an RFC 6901-style JSON pointer. Object keys use ~0/~1 escaping.
	Path string
	// Key is set for object members; an empty string may be a legal object key.
	// ArrayIndex, not Key, distinguishes array elements from object members.
	Key string
	// KeyRange includes the JSON string delimiters for an object key.
	KeyRange ByteRange
	// MemberRange covers key through value, excluding commas and surrounding
	// whitespace. For array values it equals ValueRange.
	MemberRange ByteRange
	// DeleteRange is the comma-safe range for removing this member/element. It
	// is derived while scanning from neighboring syntax boundaries; unrelated
	// siblings do not need their own Field records.
	DeleteRange ByteRange
	// ValueRange covers the complete JSON value, including string quotes.
	ValueRange ByteRange
	// StringRange is ValueRange for strings and zero for all other values.
	StringRange ByteRange
	Type        JSONValueType
	// ArrayIndex is the zero-based array index, or -1 for object members.
	ArrayIndex int
	// Depth is the containing object's/array's depth; top-level members are 0.
	Depth int
}

// ContainerInfo retains aggregate boundaries for a selected object/array. It
// intentionally does not retain unrelated child Field records.
type ContainerInfo struct {
	Path            string
	Type            JSONValueType
	ValueRange      ByteRange
	ChildCount      int
	FirstChildStart int64
	LastChildEnd    int64
	CloseOffset     int64
}

// JSONIndex is a read-only selective index bound to one immutable Body. Its
// lookup maps are built during Bind, so Find/Lookup/DirectChildren never scan
// the complete field directory.
type JSONIndex struct {
	Model      string
	ModelRange ByteRange

	body       Body
	bodySize   int64
	fields     []Field
	byPath     map[string][]int
	containers map[string][]ContainerInfo
	children   map[string][]int
	spec       ScanSpec
	valid      bool
	modelSeen  bool
	rawMarkers map[string]bool
}

var (
	ErrInvalidJSON       = errors.New("bodyfile: invalid JSON")
	ErrTrailingJSON      = errors.New("bodyfile: trailing JSON content")
	ErrModelMissing      = errors.New("bodyfile: model is required")
	ErrModelRepeated     = errors.New("bodyfile: model must not be repeated")
	ErrModelNotString    = errors.New("bodyfile: model must be a string")
	ErrModelEmpty        = errors.New("bodyfile: model must not be empty")
	ErrModelTooLong      = fmt.Errorf("bodyfile: model must not exceed %d decoded UTF-8 bytes", modelname.MaxBytes)
	ErrIndexBodyMismatch = errors.New("bodyfile: JSON index belongs to another body")
)

// Index is the compatibility all-fields request helper. Production capture
// paths should call CaptureAndScan with an explicit selective RequestScanSpec.
func Index(body Body) (JSONIndex, error) {
	return ScanBody(body, ScanSpec{RequireModel: true, RequireObject: true, SelectAll: true})
}

// BuildIndex is the descriptive alias for Index.
func BuildIndex(body Body) (JSONIndex, error) { return Index(body) }

// IndexObject is the compatibility all-fields response helper. Production
// response paths should call CaptureAndScan only when the selected Provider
// actually has a response hook, with that Plan's ResponseScanSpec.
func IndexObject(body Body) (JSONIndex, error) {
	return ScanBody(body, ScanSpec{RequireObject: true, SelectAll: true})
}

// ParseIndex is an alias retained for callers that prefer parser terminology.
func ParseIndex(body Body) (JSONIndex, error) { return BuildIndex(body) }

// ValidateBody confirms that this index was built from body.
func (i JSONIndex) ValidateBody(body Body) error {
	if !i.valid || i.body == nil {
		return ErrIndexBodyMismatch
	}
	if body == nil || !sameBody(i.body, body) || body.Size() != i.bodySize {
		return ErrIndexBodyMismatch
	}
	return nil
}

// Fields returns a defensive copy of selected field descriptors.
func (i JSONIndex) Fields() []Field {
	if len(i.fields) == 0 {
		return []Field{}
	}
	return append([]Field(nil), i.fields...)
}

// Lookup returns the first selected field at path in O(1) map time.
func (i JSONIndex) Lookup(path string) (Field, bool) {
	indices := i.byPath[path]
	if len(indices) == 0 || indices[0] < 0 || indices[0] >= len(i.fields) {
		return Field{}, false
	}
	return i.fields[indices[0]], true
}

// Find returns every selected field at path without rescanning the index.
func (i JSONIndex) Find(path string) []Field {
	indices := i.byPath[path]
	result := make([]Field, 0, len(indices))
	for _, index := range indices {
		if index >= 0 && index < len(i.fields) {
			result = append(result, i.fields[index])
		}
	}
	return result
}

// FieldAt and Range are convenience aliases.
func (i JSONIndex) FieldAt(path string) (Field, bool) { return i.Lookup(path) }

func (i JSONIndex) Range(path string) (ByteRange, bool) {
	field, ok := i.Lookup(path)
	if !ok {
		return ByteRange{}, false
	}
	return field.ValueRange, true
}

// Containers returns aggregate metadata for all selected occurrences at path.
func (i JSONIndex) Containers(path string) []ContainerInfo {
	items := i.containers[path]
	if len(items) == 0 {
		return []ContainerInfo{}
	}
	return append([]ContainerInfo(nil), items...)
}

// ContainerAt returns the first selected container occurrence at path.
func (i JSONIndex) ContainerAt(path string) (ContainerInfo, bool) {
	items := i.containers[path]
	if len(items) == 0 {
		return ContainerInfo{}, false
	}
	return items[0], true
}

// DirectChildren returns selected direct children using the prebuilt map.
func (i JSONIndex) DirectChildren(path string) []Field {
	indices := i.children[path]
	result := make([]Field, 0, len(indices))
	for _, index := range indices {
		if index >= 0 && index < len(i.fields) {
			result = append(result, i.fields[index])
		}
	}
	return result
}

// Spec returns the scan contract which produced this index.
func (i JSONIndex) Spec() ScanSpec {
	result := i.spec
	result.Paths = append([]string(nil), i.spec.Paths...)
	result.RawMarkers = append([]string(nil), i.spec.RawMarkers...)
	return result
}

// RawMarkerStatus reports whether marker was found in the original bytes and
// whether this index's ingress scan tracked that marker at all. The second
// result lets detectors distinguish an untracked marker from a tracked
// negative match without rescanning the body.
func (i JSONIndex) RawMarkerStatus(marker string) (found, tracked bool) {
	if i.rawMarkers == nil {
		return false, false
	}
	found, tracked = i.rawMarkers[marker]
	if !tracked {
		// A configured marker with a false result is still present in the map;
		// check the scan contract for callers that construct an empty map.
		for _, configured := range i.spec.RawMarkers {
			if configured == marker {
				return false, true
			}
		}
	}
	return found, tracked
}

// RawMarkerFound reports whether the scanner observed marker in the original
// request bytes. It returns false when marker was not part of the scan
// contract, allowing callers to fail closed instead of performing a rescan.
func (i JSONIndex) RawMarkerFound(marker string) bool {
	found, _ := i.RawMarkerStatus(marker)
	return found
}

// HasRawMarker is a descriptive alias for RawMarkerFound.
func (i JSONIndex) HasRawMarker(marker string) bool { return i.RawMarkerFound(marker) }

// RawMarkers returns a defensive copy of all raw marker scan results keyed by
// their configured byte sequence.
func (i JSONIndex) RawMarkers() map[string]bool {
	result := make(map[string]bool, len(i.rawMarkers))
	for marker, found := range i.rawMarkers {
		result[marker] = found
	}
	return result
}

// ModelValue returns the decoded top-level model scalar.
func (i JSONIndex) ModelValue() string { return i.Model }

// OriginalModel is a descriptive alias for ModelValue.
func (i JSONIndex) OriginalModel() string { return i.Model }

// ModelBytesRange returns the exact byte range of the model JSON string.
func (i JSONIndex) ModelBytesRange() ByteRange { return i.ModelRange }

// Size returns the body size captured by this index.
func (i JSONIndex) Size() int64 { return i.bodySize }

// Body returns the immutable body this index is bound to. The request owner,
// not an index consumer, owns its lifetime.
func (i JSONIndex) Body() Body { return i.body }

func parentJSONPointer(path string) string {
	if path == "" {
		return ""
	}
	if index := strings.LastIndexByte(path, '/'); index > 0 {
		return path[:index]
	}
	return ""
}

func sameBody(left, right Body) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	if l, ok := left.(interface{ bodyIdentity() *fileBody }); ok {
		if r, ok := right.(interface{ bodyIdentity() *fileBody }); ok {
			return l.bodyIdentity() == r.bodyIdentity()
		}
	}
	lv, rv := reflect.ValueOf(left), reflect.ValueOf(right)
	if lv.IsValid() && rv.IsValid() && lv.Type() == rv.Type() && lv.Type().Comparable() {
		return lv.Interface() == rv.Interface()
	}
	if lv.IsValid() && rv.IsValid() && lv.Kind() == reflect.Ptr && rv.Kind() == reflect.Ptr {
		return lv.Pointer() == rv.Pointer()
	}
	return false
}

func escapePointer(value string) string {
	value = strings.ReplaceAll(value, "~", "~0")
	return strings.ReplaceAll(value, "/", "~1")
}

func isHexDigit(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'f') || (value >= 'A' && value <= 'F')
}

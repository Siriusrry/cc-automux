package patch

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
)

var ErrIndexUnavailable = errors.New("patch: bound JSON index is unavailable")

func findFields(index bodyfile.JSONIndex, path string) []bodyfile.Field {
	return index.Find(path)
}

func onlyField(index bodyfile.JSONIndex, path string) (bodyfile.Field, bool, error) {
	fields := index.Find(path)
	if len(fields) == 0 {
		return bodyfile.Field{}, false, nil
	}
	if len(fields) != 1 {
		return bodyfile.Field{}, false, fmt.Errorf("field %s occurs %d times", path, len(fields))
	}
	return fields[0], true, nil
}

func requestIndex(request *MutableRequest) (bodyfile.JSONIndex, error) {
	if request == nil || request.Body == nil {
		return bodyfile.JSONIndex{}, errors.New("nil mutable request/body")
	}
	if err := request.index.ValidateBody(request.Body); err == nil {
		return request.index, nil
	}
	if request.strictIndex {
		return bodyfile.JSONIndex{}, ErrIndexUnavailable
	}
	spec := request.index.Spec()
	var index bodyfile.JSONIndex
	var err error
	if spec.RequireModel || spec.SelectAll || spec.Paths != nil {
		index, err = bodyfile.IndexSelective(request.Body, spec)
	} else {
		// Explicit compatibility for focused callers that construct a bare
		// MutableRequest literal. NewMutableRequest marks production requests
		// strict, so this branch can never reopen a live Gateway body.
		index, err = bodyfile.Index(request.Body)
	}
	if err != nil {
		return bodyfile.JSONIndex{}, err
	}
	request.index = index
	return index, nil
}

func responseIndex(response *MutableResponse) (bodyfile.JSONIndex, error) {
	if response == nil || response.Body == nil {
		return bodyfile.JSONIndex{}, errors.New("nil mutable response/body")
	}
	if err := response.index.ValidateBody(response.Body); err == nil {
		return response.index, nil
	}
	// Response field selection has one source of truth: the selected Plan's
	// ResponsePaths, materialized by Gateway before this hook runs. A missing
	// binding is a programming/contract error, never a reason to rescan with a
	// second hard-coded path list.
	return bodyfile.JSONIndex{}, ErrIndexUnavailable
}

func readFieldString(body bodyfile.Body, field bodyfile.Field) (string, error) {
	if field.Type != bodyfile.JSONString || field.StringRange.End < field.StringRange.Start+2 {
		return "", errors.New("field is not a JSON string")
	}
	reader, err := body.OpenReader()
	if err != nil {
		return "", err
	}
	defer reader.Close()
	if _, err := io.CopyN(io.Discard, reader, field.StringRange.Start); err != nil {
		return "", err
	}
	raw := make([]byte, field.StringRange.End-field.StringRange.Start)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return "", err
	}
	var value string
	err = json.Unmarshal(raw, &value)
	if err != nil {
		return "", fmt.Errorf("decode JSON string: %w", err)
	}
	return value, nil
}

// bodyFieldCursor walks one immutable Body in ascending field-offset order.
// Hooks that inspect several selected strings use one cursor instead of
// opening a reader and seeking from byte zero for every content item.
type bodyFieldCursor struct {
	reader io.ReadCloser
	offset int64
}

func newBodyFieldCursor(body bodyfile.Body) (*bodyFieldCursor, error) {
	if body == nil {
		return nil, errors.New("nil body")
	}
	reader, err := body.OpenReader()
	if err != nil {
		return nil, err
	}
	return &bodyFieldCursor{reader: reader}, nil
}

func (c *bodyFieldCursor) Close() error {
	if c == nil || c.reader == nil {
		return nil
	}
	return c.reader.Close()
}

func (c *bodyFieldCursor) moveTo(target int64) error {
	if c == nil || c.reader == nil || target < c.offset {
		return errors.New("selected fields are not in ascending byte order")
	}
	if target == c.offset {
		return nil
	}
	n, err := io.CopyN(io.Discard, c.reader, target-c.offset)
	c.offset += n
	if err != nil {
		return err
	}
	return nil
}

func (c *bodyFieldCursor) ReadString(field bodyfile.Field) (string, error) {
	if field.Type != bodyfile.JSONString || field.StringRange.End < field.StringRange.Start+2 {
		return "", errors.New("field is not a JSON string")
	}
	start := field.StringRange.Start
	end := field.StringRange.End
	if err := c.moveTo(start); err != nil {
		return "", err
	}
	raw := make([]byte, end-start)
	if _, err := io.ReadFull(c.reader, raw); err != nil {
		return "", err
	}
	c.offset = end
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("decode JSON string: %w", err)
	}
	return value, nil
}

// WithJSONString exposes one selected string to a bounded decoder while
// preserving the cursor position for the next field. The callback must not
// retain the scanner after it returns.
func (c *bodyFieldCursor) WithJSONString(field bodyfile.Field, callback func(*jsonStringByteScanner) error) error {
	if c == nil || callback == nil {
		return errors.New("nil body cursor/callback")
	}
	if field.Type != bodyfile.JSONString || field.StringRange.End < field.StringRange.Start+2 {
		return errors.New("field is not a JSON string")
	}
	contentStart := field.StringRange.Start + 1
	contentEnd := field.StringRange.End - 1
	if err := c.moveTo(contentStart); err != nil {
		return err
	}
	limited := &io.LimitedReader{R: c.reader, N: contentEnd - contentStart}
	scanner := &jsonStringByteScanner{
		reader: bufio.NewReaderSize(limited, 32*1024),
		rawPos: contentStart,
		rawEnd: contentEnd,
	}
	err := callback(scanner)
	if err == nil && limited.N > 0 {
		_, err = io.CopyN(io.Discard, limited, limited.N)
	}
	if err != nil {
		return err
	}
	c.offset = contentEnd
	if err := c.moveTo(field.StringRange.End); err != nil {
		return err
	}
	return nil
}

func readFieldRaw(body bodyfile.Body, field bodyfile.Field) ([]byte, error) {
	if field.ValueRange.End < field.ValueRange.Start {
		return nil, errors.New("invalid field range")
	}
	reader, err := body.OpenReader()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	if _, err := io.CopyN(io.Discard, reader, field.ValueRange.Start); err != nil {
		return nil, err
	}
	raw := make([]byte, field.ValueRange.End-field.ValueRange.Start)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func directObjectChildren(index bodyfile.JSONIndex, parentPath string) []bodyfile.Field {
	fields := index.DirectChildren(parentPath)
	result := make([]bodyfile.Field, 0, len(fields))
	for _, field := range fields {
		if field.ArrayIndex >= 0 {
			continue
		}
		result = append(result, field)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].MemberRange.Start < result[j].MemberRange.Start })
	return result
}

func directFieldsByKey(fields []bodyfile.Field, key string) []bodyfile.Field {
	result := make([]bodyfile.Field, 0, 1)
	for _, field := range fields {
		if field.Key == key {
			result = append(result, field)
		}
	}
	return result
}

func directArrayChildren(index bodyfile.JSONIndex, parentPath string) []bodyfile.Field {
	fields := index.DirectChildren(parentPath)
	result := make([]bodyfile.Field, 0, len(fields))
	for _, field := range fields {
		if field.ArrayIndex < 0 {
			continue
		}
		result = append(result, field)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].ArrayIndex < result[j].ArrayIndex })
	return result
}

func memberDeleteEdit(index bodyfile.JSONIndex, field bodyfile.Field) (bodyfile.Edit, error) {
	// An empty object key is valid JSON; ArrayIndex is the unambiguous
	// object-member/array-element discriminator.
	if field.ArrayIndex >= 0 || field.DeleteRange.End <= field.DeleteRange.Start {
		return bodyfile.Edit{}, errors.New("field has no comma-safe object delete range")
	}
	return bodyfile.Edit{Start: field.DeleteRange.Start, End: field.DeleteRange.End}, nil
}

func arrayDeleteEdit(index bodyfile.JSONIndex, field bodyfile.Field) (bodyfile.Edit, error) {
	if field.ArrayIndex < 0 || field.DeleteRange.End <= field.DeleteRange.Start {
		return bodyfile.Edit{}, errors.New("field has no comma-safe array delete range")
	}
	return bodyfile.Edit{Start: field.DeleteRange.Start, End: field.DeleteRange.End}, nil
}

func insertionAtObjectStart(index bodyfile.JSONIndex, parentPath, key string, value []byte) (bodyfile.Edit, error) {
	member := append(jsonStringBytes(key), ':')
	member = append(member, value...)
	container, ok := index.ContainerAt(parentPath)
	if !ok || container.Type != bodyfile.JSONObject {
		return bodyfile.Edit{}, fmt.Errorf("object %s not found", parentPath)
	}
	if container.ChildCount > 0 {
		member = append(member, ',')
		return bodyfile.Edit{Start: container.FirstChildStart, End: container.FirstChildStart, Replacement: member}, nil
	}
	return bodyfile.Edit{Start: container.CloseOffset, End: container.CloseOffset, Replacement: member}, nil
}

func insertionAtArrayStart(index bodyfile.JSONIndex, arrayPath string, value []byte) (bodyfile.Edit, error) {
	array, ok := index.Lookup(arrayPath)
	if !ok || array.Type != bodyfile.JSONArray || array.ValueRange.End <= array.ValueRange.Start {
		return bodyfile.Edit{}, fmt.Errorf("array %s not found", arrayPath)
	}
	container, exists := index.ContainerAt(arrayPath)
	if !exists || container.Type != bodyfile.JSONArray {
		return bodyfile.Edit{}, fmt.Errorf("array %s not found", arrayPath)
	}
	if container.ChildCount > 0 {
		value = append(append([]byte(nil), value...), ',')
	}
	return bodyfile.Edit{Start: array.ValueRange.Start + 1, End: array.ValueRange.Start + 1, Replacement: value}, nil
}

func rootCloseOffset(index bodyfile.JSONIndex) (int64, error) {
	container, ok := index.ContainerAt("")
	if !ok || container.Type != bodyfile.JSONObject || container.CloseOffset <= 0 {
		return 0, errors.New("root JSON object is unavailable")
	}
	return container.CloseOffset, nil
}

func applyEdits(body bodyfile.Body, edits []bodyfile.Edit) (bodyfile.Body, error) {
	if len(edits) == 0 {
		return body, nil
	}
	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].Start != edits[j].Start {
			return edits[i].Start < edits[j].Start
		}
		return edits[i].End < edits[j].End
	})
	for i := 1; i < len(edits); i++ {
		if edits[i].Start < edits[i-1].End || (edits[i].Start == edits[i-1].Start && edits[i].End == edits[i-1].End) {
			return nil, fmt.Errorf("overlapping patch edits")
		}
	}
	return bodyfile.ApplyEdits(body, edits)
}

func jsonStringBytes(value string) []byte {
	encoded, _ := json.Marshal(value)
	// encoding/json escapes HTML-significant bytes for embedding in HTML. The
	// result remains valid JSON when those three escapes are rendered literally,
	// and preserving them improves byte-oriented compatibility diagnostics.
	encoded = bytes.ReplaceAll(encoded, []byte(`\u003c`), []byte("<"))
	encoded = bytes.ReplaceAll(encoded, []byte(`\u003e`), []byte(">"))
	encoded = bytes.ReplaceAll(encoded, []byte(`\u0026`), []byte("&"))
	return encoded
}

func encodeObjectMember(key string, value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	member := append(jsonStringBytes(key), ':')
	return append(member, encoded...), nil
}

// fieldRawStringPrefix reads a bounded prefix without materialising the whole
// selected string. It is useful for marker/correction checks.
func fieldRawStringPrefix(body bodyfile.Body, field bodyfile.Field, max int64) (string, bool, error) {
	if field.Type != bodyfile.JSONString || field.StringRange.End <= field.StringRange.Start+1 {
		return "", false, errors.New("field is not string")
	}
	length := field.StringRange.End - field.StringRange.Start - 2
	if length > max {
		length = max
	}
	reader, err := body.OpenReader()
	if err != nil {
		return "", false, err
	}
	defer reader.Close()
	if _, err := io.CopyN(io.Discard, reader, field.StringRange.Start+1); err != nil {
		return "", false, err
	}
	raw := make([]byte, length)
	if _, err := io.ReadFull(reader, raw); err != nil && !errors.Is(err, io.EOF) {
		return "", false, err
	}
	// Prefix bytes can end in an escape; decode only when complete. Callers
	// needing exact full strings use readFieldString.
	var value string
	err = json.Unmarshal(append(append([]byte{'"'}, raw...), '"'), &value)
	if err != nil {
		return string(raw), false, nil
	}
	return value, length == field.StringRange.End-field.StringRange.Start-2, nil
}

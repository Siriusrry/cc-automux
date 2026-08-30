package patch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
)

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
	index, err := bodyfile.Index(request.Body)
	if err != nil {
		return bodyfile.JSONIndex{}, err
	}
	request.index = index
	return index, nil
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
	fields := index.Fields()
	result := make([]bodyfile.Field, 0)
	parentDepth := -1
	if parentPath == "" {
		parentDepth = -1
	} else {
		for _, field := range fields {
			if field.Path == parentPath {
				// JSONIndex records a member's depth as the depth of its
				// containing object. A nested object's direct members are one
				// level below the parent member's recorded depth.
				parentDepth = field.Depth + 1
				break
			}
		}
	}
	for _, field := range fields {
		if field.Key == "" {
			continue
		}
		if parentPath == "" {
			if field.Depth == 0 {
				result = append(result, field)
			}
			continue
		}
		if field.Depth != parentDepth {
			continue
		}
		if fieldParentPath(field.Path) == parentPath {
			result = append(result, field)
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].MemberRange.Start < result[j].MemberRange.Start })
	return result
}

func directArrayChildren(index bodyfile.JSONIndex, parentPath string) []bodyfile.Field {
	fields := index.Fields()
	result := make([]bodyfile.Field, 0)
	for _, field := range fields {
		if field.ArrayIndex < 0 || fieldParentPath(field.Path) != parentPath {
			continue
		}
		result = append(result, field)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].ArrayIndex < result[j].ArrayIndex })
	return result
}

func fieldParentPath(path string) string {
	if path == "" {
		return ""
	}
	index := strings.LastIndexByte(path, '/')
	if index <= 0 {
		return ""
	}
	return path[:index]
}

func memberDeleteEdit(index bodyfile.JSONIndex, field bodyfile.Field) (bodyfile.Edit, error) {
	parent := fieldParentPath(field.Path)
	children := directObjectChildren(index, parent)
	position := -1
	for i := range children {
		if children[i].MemberRange == field.MemberRange {
			position = i
			break
		}
	}
	if position < 0 {
		return bodyfile.Edit{}, errors.New("field is not a direct object member")
	}
	if position+1 < len(children) {
		return bodyfile.Edit{Start: field.MemberRange.Start, End: children[position+1].MemberRange.Start}, nil
	}
	if position > 0 {
		return bodyfile.Edit{Start: children[position-1].MemberRange.End, End: field.MemberRange.End}, nil
	}
	return bodyfile.Edit{Start: field.MemberRange.Start, End: field.MemberRange.End}, nil
}

func arrayDeleteEdit(index bodyfile.JSONIndex, field bodyfile.Field) (bodyfile.Edit, error) {
	parent := fieldParentPath(field.Path)
	children := directArrayChildren(index, parent)
	position := -1
	for i := range children {
		if children[i].ValueRange == field.ValueRange {
			position = i
			break
		}
	}
	if position < 0 {
		return bodyfile.Edit{}, errors.New("field is not a direct array element")
	}
	if position+1 < len(children) {
		return bodyfile.Edit{Start: field.ValueRange.Start, End: children[position+1].ValueRange.Start}, nil
	}
	if position > 0 {
		return bodyfile.Edit{Start: children[position-1].ValueRange.End, End: field.ValueRange.End}, nil
	}
	return bodyfile.Edit{Start: field.ValueRange.Start, End: field.ValueRange.End}, nil
}

func insertionAtObjectStart(index bodyfile.JSONIndex, parentPath, key string, value []byte) (bodyfile.Edit, error) {
	children := directObjectChildren(index, parentPath)
	member := append(jsonStringBytes(key), ':')
	member = append(member, value...)
	if len(children) > 0 {
		member = append(member, ',')
		return bodyfile.Edit{Start: children[0].MemberRange.Start, End: children[0].MemberRange.Start, Replacement: member}, nil
	}
	var closeAt int64
	if parentPath == "" {
		var err error
		closeAt, err = rootCloseOffset(index)
		if err != nil {
			return bodyfile.Edit{}, err
		}
	} else {
		parent, ok := index.Lookup(parentPath)
		if !ok || parent.Type != bodyfile.JSONObject || parent.ValueRange.End <= parent.ValueRange.Start {
			return bodyfile.Edit{}, fmt.Errorf("object %s not found", parentPath)
		}
		closeAt = parent.ValueRange.End - 1
	}
	return bodyfile.Edit{Start: closeAt, End: closeAt, Replacement: member}, nil
}

func insertionAtArrayStart(index bodyfile.JSONIndex, arrayPath string, value []byte) (bodyfile.Edit, error) {
	array, ok := index.Lookup(arrayPath)
	if !ok || array.Type != bodyfile.JSONArray || array.ValueRange.End <= array.ValueRange.Start {
		return bodyfile.Edit{}, fmt.Errorf("array %s not found", arrayPath)
	}
	children := directArrayChildren(index, arrayPath)
	if len(children) > 0 {
		value = append(append([]byte(nil), value...), ',')
	}
	return bodyfile.Edit{Start: array.ValueRange.Start + 1, End: array.ValueRange.Start + 1, Replacement: value}, nil
}

func rootCloseOffset(index bodyfile.JSONIndex) (int64, error) {
	body := indexBody(index)
	if body == nil {
		return 0, errors.New("index does not expose body")
	}
	reader, err := body.OpenReader()
	if err != nil {
		return 0, err
	}
	defer reader.Close()
	var offset, lastNonSpace int64
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(buffer)
		for _, value := range buffer[:n] {
			if value != ' ' && value != '\t' && value != '\r' && value != '\n' {
				lastNonSpace = offset
			}
			offset++
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return 0, readErr
		}
	}
	if lastNonSpace <= 0 {
		return 0, errors.New("empty JSON body")
	}
	return lastNonSpace, nil
}

// JSONIndex intentionally keeps its body private.  This helper uses the
// optional interface implemented by the current bodyfile index; a fallback
// scan is provided for foreign indexes in tests.
func indexBody(index bodyfile.JSONIndex) bodyfile.Body {
	if provider, ok := any(index).(interface{ Body() bodyfile.Body }); ok {
		return provider.Body()
	}
	return nil
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

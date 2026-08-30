package patch

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sort"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
)

const GPTClassifierResponseReassemblyID = "gpt-classifier-response-reassembly"

type gptClassifierResponsePatch struct {
	stopSequences []string
	requestDone   bool
}

func (p *gptClassifierResponsePatch) ApplyRequest(_ PatchContext, request *MutableRequest) error {
	if p == nil || request == nil || request.Body == nil {
		return errors.New("nil mutable request/body")
	}
	if p.requestDone {
		return errors.New("request hook already applied")
	}
	p.requestDone = true
	if request.Headers == nil {
		request.Headers = NewHTTPHeaderSet(nil)
	}
	request.Headers.Delete("Accept-Encoding")
	index, err := requestIndex(request)
	if err != nil {
		return err
	}
	stopFields := index.Find("/stop_sequences")
	if len(stopFields) > 1 {
		return fmt.Errorf("stop_sequences occurs %d times", len(stopFields))
	}
	if len(stopFields) == 0 {
		p.stopSequences = nil
		return nil
	}
	if stopFields[0].Type != bodyfile.JSONArray {
		return errors.New("stop_sequences must be an array")
	}
	children := directArrayChildren(index, "/stop_sequences")
	p.stopSequences = make([]string, 0, len(children))
	for _, child := range children {
		if child.Type != bodyfile.JSONString {
			return errors.New("stop_sequences entries must be strings")
		}
		value, readErr := readFieldString(request.Body, child)
		if readErr != nil {
			return readErr
		}
		if value == "" {
			return errors.New("stop_sequences entries must not be empty")
		}
		p.stopSequences = append(p.stopSequences, value)
	}
	return nil
}

func (p *gptClassifierResponsePatch) ApplyResponse(_ PatchContext, response *MutableResponse) error {
	if p == nil || response == nil || response.Body == nil {
		return errors.New("nil mutable response/body")
	}
	if response.Status < 200 || response.Status >= 300 {
		// Upstream errors are passed through by the gateway and must not be
		// interpreted as classifier messages.
		return nil
	}
	index, err := responseIndex(response,
		"/type", "/content", "/content/*", "/content/*/type", "/content/*/text",
		"/stop_reason", "/stop_sequence",
	)
	if err != nil {
		return err
	}
	typeFields := index.Find("/type")
	if len(typeFields) != 1 || typeFields[0].Type != bodyfile.JSONString {
		return errors.New("response type is missing or ambiguous")
	}
	typeValue, err := readFieldString(response.Body, typeFields[0])
	if err != nil {
		return err
	}
	if typeValue != "message" {
		return errors.New("response type is not Anthropic message")
	}
	contentFields := index.Find("/content")
	if len(contentFields) != 1 || contentFields[0].Type != bodyfile.JSONArray {
		return errors.New("response content is missing or ambiguous")
	}
	if err := validateResponseStopFields(index); err != nil {
		return err
	}
	blocks := directArrayChildren(index, "/content")
	if len(blocks) == 0 {
		return errors.New("response content is empty")
	}
	removed := make([]bool, len(blocks))
	match := stopMatch{}
	for i, block := range blocks {
		if block.Type != bodyfile.JSONObject {
			return errors.New("response content block must be an object")
		}
		path := fmt.Sprintf("/content/%d/type", block.ArrayIndex)
		types := index.Find(path)
		if len(types) != 1 || types[0].Type != bodyfile.JSONString {
			return fmt.Errorf("%s is missing or ambiguous", path)
		}
		kind, readErr := readFieldString(response.Body, types[0])
		if readErr != nil {
			return readErr
		}
		if kind == "thinking" || kind == "redacted_thinking" {
			removed[i] = true
			continue
		}
		if kind != "text" || match.found {
			continue
		}
		textPath := fmt.Sprintf("/content/%d/text", block.ArrayIndex)
		texts := index.Find(textPath)
		if len(texts) != 1 || texts[0].Type != bodyfile.JSONString {
			return fmt.Errorf("%s is missing or ambiguous", textPath)
		}
		start, sequence, found, scanErr := findStopInString(response.Body, texts[0], p.stopSequences)
		if scanErr != nil {
			return scanErr
		}
		if found {
			match = stopMatch{found: true, rawStart: start, sequence: sequence, textField: texts[0], blockIndex: i}
			for j := i + 1; j < len(blocks); j++ {
				removed[j] = true
			}
		}
	}

	// If every block would be removed, the result cannot be a usable message.
	remaining := 0
	for _, isRemoved := range removed {
		if !isRemoved {
			remaining++
		}
	}
	if remaining == 0 {
		return errors.New("response has no content after thinking removal")
	}
	edits := make([]bodyfile.Edit, 0, len(blocks)+3)
	edits = append(edits, arrayRemovalEdits(index, blocks, removed)...)
	if match.found {
		edits = append(edits, bodyfile.Edit{
			Start: match.rawStart,
			End:   match.textField.StringRange.End - 1,
		})
		if err := appendStopFieldEdits(index, response.Body, &edits, match.sequence); err != nil {
			return err
		}
	}
	if len(edits) == 0 {
		return nil
	}
	if err := sortAndValidateEdits(edits); err != nil {
		return err
	}
	body, nextIndex, err := bodyfile.ApplyEditsAndScan(response.Body, edits, index.Spec())
	if err != nil {
		return err
	}
	if err := response.SetBody(body, nextIndex); err != nil {
		_ = body.Close()
		return err
	}
	if response.Headers == nil {
		response.Headers = NewHTTPHeaderSet(nil)
	}
	response.Headers.Delete("Content-Encoding")
	response.Headers.Set("Content-Length", fmt.Sprintf("%d", body.Size()))
	return nil
}

type stopMatch struct {
	found      bool
	rawStart   int64
	sequence   string
	textField  bodyfile.Field
	blockIndex int
}

func arrayRemovalEdits(index bodyfile.JSONIndex, blocks []bodyfile.Field, removed []bool) []bodyfile.Edit {
	if len(blocks) == 0 {
		return nil
	}
	all := true
	for _, value := range removed {
		if !value {
			all = false
			break
		}
	}
	if all {
		return []bodyfile.Edit{{Start: blocks[0].ValueRange.Start, End: blocks[len(blocks)-1].ValueRange.End}}
	}
	result := make([]bodyfile.Edit, 0)
	for i := 0; i < len(blocks); {
		if !removed[i] {
			i++
			continue
		}
		start := i
		for i < len(blocks) && removed[i] {
			i++
		}
		end := i - 1
		if i < len(blocks) {
			result = append(result, bodyfile.Edit{Start: blocks[start].ValueRange.Start, End: blocks[i].ValueRange.Start})
		} else {
			result = append(result, bodyfile.Edit{Start: blocks[start-1].ValueRange.End, End: blocks[end].ValueRange.End})
		}
	}
	return result
}

func appendStopFieldEdits(index bodyfile.JSONIndex, body bodyfile.Body, edits *[]bodyfile.Edit, sequence string) error {
	_ = body // retained in the signature for callers that already opened a body
	reasonFields := index.Find("/stop_reason")
	if len(reasonFields) > 1 {
		return errors.New("stop_reason occurs more than once")
	}
	sequenceFields := index.Find("/stop_sequence")
	if len(sequenceFields) > 1 {
		return errors.New("stop_sequence occurs more than once")
	}
	if len(reasonFields) == 1 {
		if !isValidStopField(reasonFields[0]) {
			return errors.New("stop_reason must be a string or null")
		}
		*edits = append(*edits, bodyfile.Edit{Start: reasonFields[0].ValueRange.Start, End: reasonFields[0].ValueRange.End, Replacement: jsonStringBytes("stop_sequence")})
	}
	if len(sequenceFields) == 1 {
		if !isValidStopField(sequenceFields[0]) {
			return errors.New("stop_sequence must be a string or null")
		}
		*edits = append(*edits, bodyfile.Edit{Start: sequenceFields[0].ValueRange.Start, End: sequenceFields[0].ValueRange.End, Replacement: jsonStringBytes(sequence)})
	}
	missingReason := len(reasonFields) == 0
	missingSequence := len(sequenceFields) == 0
	if missingReason || missingSequence {
		members := make([][]byte, 0, 2)
		if missingReason {
			member, err := encodeObjectMember("stop_reason", "stop_sequence")
			if err != nil {
				return err
			}
			members = append(members, member)
		}
		if missingSequence {
			member, err := encodeObjectMember("stop_sequence", sequence)
			if err != nil {
				return err
			}
			members = append(members, member)
		}
		replacement := joinObjectMembers(members)
		children := directObjectChildren(index, "")
		var offset int64
		if len(children) > 0 {
			offset = children[0].MemberRange.Start
			replacement = append(replacement, ',')
		} else {
			closeOffset, err := rootCloseOffset(index)
			if err != nil {
				return err
			}
			offset = closeOffset
		}
		*edits = append(*edits, bodyfile.Edit{Start: offset, End: offset, Replacement: replacement})
	}
	return nil
}

func validateResponseStopFields(index bodyfile.JSONIndex) error {
	for _, candidate := range []struct {
		name   string
		fields []bodyfile.Field
	}{
		{name: "stop_reason", fields: index.Find("/stop_reason")},
		{name: "stop_sequence", fields: index.Find("/stop_sequence")},
	} {
		if len(candidate.fields) > 1 {
			return fmt.Errorf("%s occurs more than once", candidate.name)
		}
		if len(candidate.fields) == 1 && !isValidStopField(candidate.fields[0]) {
			return fmt.Errorf("%s must be a string or null", candidate.name)
		}
	}
	return nil
}

func isValidStopField(field bodyfile.Field) bool {
	return field.Type == bodyfile.JSONString || field.Type == bodyfile.JSONNull
}

func joinObjectMembers(members [][]byte) []byte {
	if len(members) == 0 {
		return nil
	}
	var result []byte
	for i, member := range members {
		if i > 0 {
			result = append(result, ',')
		}
		result = append(result, member...)
	}
	return result
}

func sortAndValidateEdits(edits []bodyfile.Edit) error {
	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].Start != edits[j].Start {
			return edits[i].Start < edits[j].Start
		}
		return edits[i].End < edits[j].End
	})
	for i := 1; i < len(edits); i++ {
		if edits[i].Start < edits[i-1].End || (edits[i].Start == edits[i-1].Start && edits[i].End == edits[i-1].End) {
			return errors.New("overlapping response patch edits")
		}
	}
	return nil
}

// findStopInString scans a JSON string directly from the immutable body. It
// keeps only a bounded suffix (the longest configured stop sequence), so a
// multi-megabyte response text never becomes a []byte/string in memory.
func findStopInString(body bodyfile.Body, field bodyfile.Field, sequences []string) (int64, string, bool, error) {
	if len(sequences) == 0 {
		return 0, "", false, nil
	}
	maxLen := 0
	matchers := make([]stopSequenceMatcher, len(sequences))
	for i, sequence := range sequences {
		matchers[i] = newStopSequenceMatcher([]byte(sequence))
		if len(matchers[i].pattern) > maxLen {
			maxLen = len(matchers[i].pattern)
		}
	}
	reader, err := body.OpenReader()
	if err != nil {
		return 0, "", false, err
	}
	defer reader.Close()
	if _, err := io.CopyN(io.Discard, reader, field.StringRange.Start+1); err != nil {
		return 0, "", false, err
	}
	scanner := &jsonStringByteScanner{reader: bufio.NewReaderSize(reader, 32*1024), rawPos: field.StringRange.Start + 1, rawEnd: field.StringRange.End - 1}
	window := make([]mappedByte, 0, maxLen)
	decodedPosition := int64(-1)
	bestStart := int64(-1)
	bestSequence := -1
	bestRawStart := int64(0)
	for {
		mapped, done, scanErr := scanner.next()
		if scanErr != nil {
			return 0, "", false, scanErr
		}
		if done {
			if bestSequence < 0 {
				return 0, "", false, nil
			}
			return bestRawStart, sequences[bestSequence], true, nil
		}
		for _, item := range mapped {
			decodedPosition++
			window = append(window, item)
			if len(window) > maxLen {
				window = window[len(window)-maxLen:]
			}
			for i := range matchers {
				if !matchers[i].feed(item.value) {
					continue
				}
				start := decodedPosition - int64(len(matchers[i].pattern)) + 1
				rawStart := window[len(window)-len(matchers[i].pattern)].rawStart
				if bestSequence < 0 || start < bestStart || start == bestStart && i < bestSequence {
					bestStart = start
					bestSequence = i
					bestRawStart = rawStart
				}
			}
		}
	}
}

type stopSequenceMatcher struct {
	pattern []byte
	failure []int
	matched int
}

func newStopSequenceMatcher(pattern []byte) stopSequenceMatcher {
	matcher := stopSequenceMatcher{
		pattern: append([]byte(nil), pattern...),
		failure: make([]int, len(pattern)),
	}
	for i, prefix := 1, 0; i < len(pattern); i++ {
		for prefix > 0 && pattern[i] != pattern[prefix] {
			prefix = matcher.failure[prefix-1]
		}
		if pattern[i] == pattern[prefix] {
			prefix++
		}
		matcher.failure[i] = prefix
	}
	return matcher
}

func (m *stopSequenceMatcher) feed(value byte) bool {
	for m.matched > 0 && value != m.pattern[m.matched] {
		m.matched = m.failure[m.matched-1]
	}
	if value == m.pattern[m.matched] {
		m.matched++
	}
	if m.matched != len(m.pattern) {
		return false
	}
	m.matched = m.failure[m.matched-1]
	return true
}

type mappedByte struct {
	value            byte
	rawStart, rawEnd int64
}

type jsonStringByteScanner struct {
	reader         *bufio.Reader
	rawPos, rawEnd int64
	pending        []mappedByte
}

func (s *jsonStringByteScanner) next() ([]mappedByte, bool, error) {
	if len(s.pending) > 0 {
		value := s.pending
		s.pending = nil
		return value, false, nil
	}
	if s.rawPos >= s.rawEnd {
		return nil, true, nil
	}
	start := s.rawPos
	value, err := s.reader.ReadByte()
	if err != nil {
		return nil, false, err
	}
	s.rawPos++
	if value == '\\' {
		escape, readErr := s.reader.ReadByte()
		if readErr != nil {
			return nil, false, readErr
		}
		s.rawPos++
		switch escape {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			decoded := decodeSimpleEscape(escape)
			return []mappedByte{{value: decoded, rawStart: start, rawEnd: s.rawPos}}, false, nil
		case 'u':
			first, readErr := s.readHexRune()
			if readErr != nil {
				return nil, false, readErr
			}
			rawEnd := s.rawPos
			runeValue := rune(first)
			if first >= 0xd800 && first <= 0xdbff {
				// A surrogate pair must be represented by a second \u escape.
				peek, peekErr := s.reader.Peek(2)
				if peekErr != nil || len(peek) < 2 || peek[0] != '\\' || peek[1] != 'u' {
					return nil, false, errors.New("unpaired high surrogate in JSON string")
				}
				_, _ = s.reader.Discard(2)
				s.rawPos += 2
				second, secondErr := s.readHexRune()
				if secondErr != nil || second < 0xdc00 || second > 0xdfff {
					return nil, false, errors.New("invalid JSON surrogate pair")
				}
				rawEnd = s.rawPos
				runeValue = utf16.DecodeRune(rune(first), rune(second))
			} else if first >= 0xdc00 && first <= 0xdfff {
				return nil, false, errors.New("unpaired low surrogate in JSON string")
			}
			var buffer [utf8.UTFMax]byte
			n := utf8.EncodeRune(buffer[:], runeValue)
			if n <= 0 {
				return nil, false, errors.New("invalid unicode escape")
			}
			mapped := make([]mappedByte, n)
			for i := 0; i < n; i++ {
				mapped[i] = mappedByte{value: buffer[i], rawStart: start, rawEnd: rawEnd}
			}
			return mapped, false, nil
		default:
			return nil, false, errors.New("invalid JSON string escape")
		}
	}
	if value == '"' {
		return nil, true, nil
	}
	if value < 0x20 {
		return nil, false, errors.New("control byte in JSON string")
	}
	return []mappedByte{{value: value, rawStart: start, rawEnd: s.rawPos}}, false, nil
}

func (s *jsonStringByteScanner) readHexRune() (uint16, error) {
	if s.rawPos+4 > s.rawEnd {
		return 0, errors.New("short unicode escape")
	}
	var value uint16
	for i := 0; i < 4; i++ {
		byteValue, err := s.reader.ReadByte()
		if err != nil {
			return 0, err
		}
		s.rawPos++
		digit, ok := hexDigit(byteValue)
		if !ok {
			return 0, errors.New("invalid unicode escape digit")
		}
		value = value<<4 | uint16(digit)
	}
	return value, nil
}

func decodeSimpleEscape(value byte) byte {
	switch value {
	case 'b':
		return '\b'
	case 'f':
		return '\f'
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	default:
		return value
	}
}

func hexDigit(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func newGPTClassifierResponseDefinition() PatchDefinition {
	return PatchDefinition{
		ID:           GPTClassifierResponseReassemblyID,
		Name:         "GPT Classifier Response Reassembly",
		Description:  "Reassembles successful classifier responses into the expected Anthropic message shape",
		RequestTypes: []RequestType{RequestTypeClassifier},
		Stages:       []Stage{StageRequest, StageResponse},
		RequestPaths: []string{"/stop_sequences", "/stop_sequences/*"},
		ResponsePaths: []string{
			"/type", "/content", "/content/*", "/content/*/type", "/content/*/text",
			"/stop_reason", "/stop_sequence",
		},
		Conflicts:   []string{},
		Idempotence: PerExecution,
		Factory: func(FactoryContext) (PatchInstance, error) {
			instance := &gptClassifierResponsePatch{}
			return newHooksInstance(Hooks{Request: instance, Response: instance}), nil
		},
	}
}

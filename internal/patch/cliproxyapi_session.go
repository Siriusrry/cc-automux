package patch

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Siriusrry/cc-automux/internal/bodyfile"
)

const CLIProxyAPIClassifierSessionID = "cliproxyapi-classifier-session-isolation"

type cliproxyapiSessionPatch struct {
	store *AliasStore
	alias string
}

func (p *cliproxyapiSessionPatch) ApplyRequest(context PatchContext, request *MutableRequest) error {
	if p == nil || request == nil || request.Body == nil {
		return errors.New("nil mutable request/body")
	}
	store := p.store
	if store == nil {
		return errors.New("classifier alias store is unavailable")
	}
	original := context.OriginalSessionID
	validOriginal := validSessionID(original)
	var alias string
	var err error
	if validOriginal {
		alias, err = store.GetOrCreate(context.TargetID, context.Generation, original)
	} else {
		// Missing/invalid ingress sessions are deliberately request-scoped and
		// never inserted into the shared Alias Store.
		alias, err = store.ResolveRequestScoped()
	}
	if err != nil {
		return err
	}
	p.alias = alias
	if request.Headers == nil {
		request.Headers = NewHTTPHeaderSet(nil)
	}
	request.Headers.Delete("X-Claude-Code-Session-Id")
	request.Headers.Set("X-Claude-Code-Session-Id", alias)

	index, err := requestIndex(request)
	if err != nil {
		return err
	}
	edits := make([]bodyfile.Edit, 0, 1)
	metadata, metadataPresent, err := onlyField(index, "/metadata")
	if err != nil {
		return err
	}
	if metadataPresent {
		if metadata.Type != bodyfile.JSONObject {
			return errors.New("metadata must be an object")
		}
		users := index.Find("/metadata/user_id")
		if len(users) > 1 {
			return fmt.Errorf("metadata.user_id occurs %d times", len(users))
		}
		if len(users) == 1 {
			if users[0].Type != bodyfile.JSONString {
				return errors.New("metadata.user_id must be a JSON string")
			}
			edit, rewriteErr := innerSessionIDEdit(request.Body, users[0], alias)
			if rewriteErr != nil {
				return rewriteErr
			}
			edits = append(edits, edit)
		} else {
			// The value above is replaced below with the escaped inner object;
			// constructing it directly avoids an intermediate object AST.
			inner := `{"session_id":` + string(jsonStringBytes(alias)) + `}`
			insert, insertErr := insertionAtObjectStart(index, "/metadata", "user_id", jsonStringBytes(inner))
			if insertErr != nil {
				return insertErr
			}
			edits = append(edits, insert)
		}
	} else {
		inner := `{"session_id":` + string(jsonStringBytes(alias)) + `}`
		member := []byte(`{"user_id":` + string(jsonStringBytes(inner)) + `}`)
		insert, insertErr := insertionAtObjectStart(index, "", "metadata", member)
		if insertErr != nil {
			return insertErr
		}
		edits = append(edits, insert)
	}
	return replaceBody(request, edits)
}

func validSessionID(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, runeValue := range value {
		if unicode.IsSpace(runeValue) || unicode.IsControl(runeValue) {
			return false
		}
	}
	return true
}

// innerSessionIDEdit incrementally decodes metadata.user_id and parses the
// decoded JSON object without materialising either string or an AST.  The
// parser keeps source offsets for decoded syntax bytes, so the resulting edit
// changes only session_id string content or inserts one object member.  Every
// other outer-string byte, including its original escape spelling, is copied
// unchanged by bodyfile.ApplyEdits.
func innerSessionIDEdit(body bodyfile.Body, field bodyfile.Field, alias string) (edit bodyfile.Edit, returnErr error) {
	if body == nil || field.Type != bodyfile.JSONString || field.StringRange.End < field.StringRange.Start+2 {
		return bodyfile.Edit{}, errors.New("metadata.user_id must be a JSON string")
	}
	reader, err := body.OpenReader()
	if err != nil {
		return bodyfile.Edit{}, err
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, localBodyError(bodyfile.LocalIOClose, closeErr))
		}
	}()

	payloadStart := field.StringRange.Start + 1
	payloadEnd := field.StringRange.End - 1
	if _, err := io.CopyN(io.Discard, reader, payloadStart); err != nil {
		return bodyfile.Edit{}, localBodyError(bodyfile.LocalIORead, err)
	}
	decoder := &outerJSONStringDecoder{
		reader: bufio.NewReaderSize(io.LimitReader(reader, payloadEnd-payloadStart), 32*1024),
		offset: payloadStart,
		end:    payloadEnd,
	}
	location, err := (&innerJSONParser{source: decoder}).scanSessionObject()
	if err != nil {
		return bodyfile.Edit{}, err
	}
	if location.present {
		return bodyfile.Edit{
			Start:       location.contentStart,
			End:         location.contentEnd,
			Replacement: encodeOuterStringContent([]byte(alias)),
		}, nil
	}
	member := []byte(`"session_id":` + string(jsonStringBytes(alias)))
	if location.hasMembers {
		member = append([]byte{','}, member...)
	}
	return bodyfile.Edit{
		Start:       location.closeAt,
		End:         location.closeAt,
		Replacement: encodeOuterStringContent(member),
	}, nil
}

func localBodyError(operation bodyfile.LocalIOOperation, err error) error {
	if err == nil || bodyfile.IsLocalIOError(err) {
		return err
	}
	return &bodyfile.LocalIOError{Operation: operation, Err: err}
}

// encodeOuterStringContent quotes bytes as one JSON string, then removes the
// delimiters so the result can be inserted inside an existing outer string.
// Callers pass only small generated session fragments, never user input.
func encodeOuterStringContent(value []byte) []byte {
	encoded, _ := json.Marshal(string(value))
	return encoded[1 : len(encoded)-1]
}

type mappedJSONByte struct {
	value            byte
	rawStart, rawEnd int64
}

// outerJSONStringDecoder streams the raw payload of the outer JSON string and
// emits its decoded UTF-8 bytes together with absolute source ranges.
type outerJSONStringDecoder struct {
	reader      *bufio.Reader
	offset, end int64
	pending     [utf8.UTFMax]byte
	pendingPos  int
	pendingLen  int
	pendingFrom int64
	pendingTo   int64
}

func (d *outerJSONStringDecoder) next() (mappedJSONByte, error) {
	if d.pendingPos < d.pendingLen {
		result := mappedJSONByte{value: d.pending[d.pendingPos], rawStart: d.pendingFrom, rawEnd: d.pendingTo}
		d.pendingPos++
		return result, nil
	}
	first, err := d.readRaw()
	if err != nil {
		return mappedJSONByte{}, err
	}
	if first.value == '\\' {
		return d.decodeEscape(first)
	}
	if first.value < utf8.RuneSelf {
		return first, nil
	}
	return d.decodeRawRune(first)
}

func (d *outerJSONStringDecoder) readRaw() (mappedJSONByte, error) {
	if d.offset >= d.end {
		return mappedJSONByte{}, io.EOF
	}
	value, err := d.reader.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return mappedJSONByte{}, localBodyError(bodyfile.LocalIORead, err)
	}
	start := d.offset
	d.offset++
	return mappedJSONByte{value: value, rawStart: start, rawEnd: d.offset}, nil
}

func (d *outerJSONStringDecoder) peekRaw(count int) ([]byte, error) {
	if count < 0 || int64(count) > d.end-d.offset {
		return nil, io.EOF
	}
	value, err := d.reader.Peek(count)
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, localBodyError(bodyfile.LocalIORead, err)
	}
	return value, nil
}

func (d *outerJSONStringDecoder) decodeEscape(first mappedJSONByte) (mappedJSONByte, error) {
	escaped, err := d.readRaw()
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return mappedJSONByte{}, errors.New("invalid metadata.user_id outer string escape")
		}
		return mappedJSONByte{}, err
	}
	var value rune
	switch escaped.value {
	case '"', '\\', '/':
		value = rune(escaped.value)
	case 'b':
		value = '\b'
	case 'f':
		value = '\f'
	case 'n':
		value = '\n'
	case 'r':
		value = '\r'
	case 't':
		value = '\t'
	case 'u':
		code, end, unicodeErr := d.readOuterHex4()
		if unicodeErr != nil {
			return mappedJSONByte{}, unicodeErr
		}
		escaped.rawEnd = end
		value = rune(code)
		if 0xD800 <= code && code <= 0xDBFF {
			if following, peekErr := d.peekRaw(6); peekErr == nil && following[0] == '\\' && following[1] == 'u' {
				low, ok := decodeHex4(following[2:])
				if ok && 0xDC00 <= low && low <= 0xDFFF {
					for range 6 {
						consumed, consumeErr := d.readRaw()
						if consumeErr != nil {
							return mappedJSONByte{}, consumeErr
						}
						escaped.rawEnd = consumed.rawEnd
					}
					value = utf16.DecodeRune(rune(code), rune(low))
				}
			} else if peekErr != nil && !errors.Is(peekErr, io.EOF) && !errors.Is(peekErr, io.ErrUnexpectedEOF) {
				return mappedJSONByte{}, peekErr
			}
		}
		if 0xD800 <= value && value <= 0xDFFF {
			value = utf8.RuneError
		}
	default:
		return mappedJSONByte{}, errors.New("invalid metadata.user_id outer string escape")
	}
	return d.queueRune(value, first.rawStart, escaped.rawEnd), nil
}

func (d *outerJSONStringDecoder) readOuterHex4() (uint16, int64, error) {
	var digits [4]byte
	var end int64
	for index := range digits {
		value, err := d.readRaw()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return 0, 0, errors.New("short metadata.user_id outer unicode escape")
			}
			return 0, 0, err
		}
		digits[index] = value.value
		end = value.rawEnd
	}
	decoded, ok := decodeHex4(digits[:])
	if !ok {
		return 0, 0, errors.New("invalid metadata.user_id outer unicode escape")
	}
	return decoded, end, nil
}

func (d *outerJSONStringDecoder) decodeRawRune(first mappedJSONByte) (mappedJSONByte, error) {
	available := d.end - d.offset
	if available > utf8.UTFMax-1 {
		available = utf8.UTFMax - 1
	}
	var encoded [utf8.UTFMax]byte
	encoded[0] = first.value
	if available > 0 {
		following, err := d.peekRaw(int(available))
		if err != nil {
			return mappedJSONByte{}, err
		}
		copy(encoded[1:], following)
	}
	value, width := utf8.DecodeRune(encoded[:1+available])
	end := first.rawEnd
	for index := 1; index < width; index++ {
		consumed, err := d.readRaw()
		if err != nil {
			return mappedJSONByte{}, err
		}
		end = consumed.rawEnd
	}
	return d.queueRune(value, first.rawStart, end), nil
}

func (d *outerJSONStringDecoder) queueRune(value rune, start, end int64) mappedJSONByte {
	d.pendingLen = utf8.EncodeRune(d.pending[:], value)
	d.pendingPos = 1
	d.pendingFrom = start
	d.pendingTo = end
	return mappedJSONByte{value: d.pending[0], rawStart: start, rawEnd: end}
}

func decodeHex4(value []byte) (uint16, bool) {
	if len(value) != 4 {
		return 0, false
	}
	var result uint16
	for _, digit := range value {
		result <<= 4
		switch {
		case '0' <= digit && digit <= '9':
			result += uint16(digit - '0')
		case 'a' <= digit && digit <= 'f':
			result += uint16(digit-'a') + 10
		case 'A' <= digit && digit <= 'F':
			result += uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return result, true
}

type innerSessionLocation struct {
	present                  bool
	hasMembers               bool
	contentStart, contentEnd int64
	closeAt                  int64
}

type innerJSONParser struct {
	source *outerJSONStringDecoder
	buffer []mappedJSONByte
}

func (p *innerJSONParser) scanSessionObject() (innerSessionLocation, error) {
	if err := p.skipSpace(); err != nil {
		return innerSessionLocation{}, err
	}
	location, err := p.parseObject(true)
	if err != nil {
		return innerSessionLocation{}, err
	}
	if err := p.skipSpace(); err != nil {
		return innerSessionLocation{}, err
	}
	if _, err := p.peek(1); err == nil {
		return innerSessionLocation{}, errors.New("trailing metadata.user_id JSON")
	} else if !errors.Is(err, io.EOF) {
		return innerSessionLocation{}, err
	}
	return location, nil
}

func (p *innerJSONParser) parseObject(sessionRoot bool) (innerSessionLocation, error) {
	open, err := p.next()
	if err != nil || open.value != '{' {
		return innerSessionLocation{}, errors.New("metadata.user_id must contain a JSON object")
	}
	var location innerSessionLocation
	if err := p.skipSpace(); err != nil {
		return innerSessionLocation{}, err
	}
	if next, peekErr := p.peek(1); peekErr == nil && next.value == '}' {
		closeToken, _ := p.next()
		location.closeAt = closeToken.rawStart
		return location, nil
	} else if peekErr != nil {
		if errors.Is(peekErr, io.EOF) {
			return innerSessionLocation{}, errors.New("unterminated metadata.user_id object")
		}
		return innerSessionLocation{}, peekErr
	}

	for {
		key, stringErr := p.parseString(innerStringKey)
		if stringErr != nil {
			return innerSessionLocation{}, stringErr
		}
		location.hasMembers = true
		if err := p.skipSpace(); err != nil {
			return innerSessionLocation{}, err
		}
		colon, colonErr := p.next()
		if colonErr != nil || colon.value != ':' {
			return innerSessionLocation{}, errors.New("metadata.user_id object member missing colon")
		}
		if err := p.skipSpace(); err != nil {
			return innerSessionLocation{}, err
		}
		if sessionRoot && key.matchesSessionKey {
			if location.present {
				return innerSessionLocation{}, errors.New("metadata.user_id.session_id occurs more than once")
			}
			next, peekErr := p.peek(1)
			if peekErr != nil || next.value != '"' {
				return innerSessionLocation{}, errors.New("metadata.user_id.session_id must be a string")
			}
			value, valueErr := p.parseString(innerStringSession)
			if valueErr != nil {
				return innerSessionLocation{}, valueErr
			}
			if !value.validSession {
				return innerSessionLocation{}, errors.New("metadata.user_id.session_id is invalid")
			}
			location.present = true
			location.contentStart = value.contentStart
			location.contentEnd = value.contentEnd
		} else if err := p.parseValue(); err != nil {
			return innerSessionLocation{}, err
		}
		if err := p.skipSpace(); err != nil {
			return innerSessionLocation{}, err
		}
		separator, separatorErr := p.next()
		if separatorErr != nil {
			return innerSessionLocation{}, errors.New("unterminated metadata.user_id object")
		}
		switch separator.value {
		case '}':
			location.closeAt = separator.rawStart
			return location, nil
		case ',':
			if err := p.skipSpace(); err != nil {
				return innerSessionLocation{}, err
			}
			if next, peekErr := p.peek(1); peekErr != nil || next.value == '}' {
				return innerSessionLocation{}, errors.New("invalid metadata.user_id object separator")
			}
		default:
			return innerSessionLocation{}, errors.New("invalid metadata.user_id object separator")
		}
	}
}

func (p *innerJSONParser) parseArray() error {
	open, err := p.next()
	if err != nil || open.value != '[' {
		return errors.New("invalid metadata.user_id array")
	}
	if err := p.skipSpace(); err != nil {
		return err
	}
	if next, peekErr := p.peek(1); peekErr == nil && next.value == ']' {
		_, _ = p.next()
		return nil
	} else if peekErr != nil {
		return errors.New("unterminated metadata.user_id array")
	}
	for {
		if err := p.parseValue(); err != nil {
			return err
		}
		if err := p.skipSpace(); err != nil {
			return err
		}
		separator, err := p.next()
		if err != nil {
			return errors.New("unterminated metadata.user_id array")
		}
		switch separator.value {
		case ']':
			return nil
		case ',':
			if err := p.skipSpace(); err != nil {
				return err
			}
			if next, peekErr := p.peek(1); peekErr != nil || next.value == ']' {
				return errors.New("invalid metadata.user_id array separator")
			}
		default:
			return errors.New("invalid metadata.user_id array separator")
		}
	}
}

func (p *innerJSONParser) parseValue() error {
	next, err := p.peek(1)
	if err != nil {
		return errors.New("missing metadata.user_id JSON value")
	}
	switch next.value {
	case '"':
		_, err = p.parseString(innerStringIgnore)
		return err
	case '{':
		_, err = p.parseObject(false)
		return err
	case '[':
		return p.parseArray()
	case 't':
		return p.consumeLiteral("true")
	case 'f':
		return p.consumeLiteral("false")
	case 'n':
		return p.consumeLiteral("null")
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return p.consumeNumber()
	default:
		return errors.New("invalid metadata.user_id JSON value")
	}
}

type innerStringPurpose uint8

const (
	innerStringIgnore innerStringPurpose = iota
	innerStringKey
	innerStringSession
)

type innerStringResult struct {
	matchesSessionKey        bool
	validSession             bool
	contentStart, contentEnd int64
}

func (p *innerJSONParser) parseString(purpose innerStringPurpose) (innerStringResult, error) {
	open, err := p.next()
	if err != nil || open.value != '"' {
		return innerStringResult{}, errors.New("metadata.user_id JSON string required")
	}
	result := innerStringResult{
		matchesSessionKey: purpose == innerStringKey,
		validSession:      purpose == innerStringSession,
		contentStart:      open.rawEnd,
	}
	keyOffset := 0
	sessionHasRune := false
	observe := func(value rune) {
		if purpose == innerStringKey {
			if keyOffset >= len("session_id") || value != rune("session_id"[keyOffset]) {
				result.matchesSessionKey = false
			}
			keyOffset++
		}
		if purpose == innerStringSession {
			sessionHasRune = true
			if unicode.IsSpace(value) || unicode.IsControl(value) {
				result.validSession = false
			}
		}
	}

	for {
		value, err := p.next()
		if err != nil {
			return innerStringResult{}, errors.New("unterminated metadata.user_id JSON string")
		}
		switch value.value {
		case '"':
			result.contentEnd = value.rawStart
			if purpose == innerStringKey && keyOffset != len("session_id") {
				result.matchesSessionKey = false
			}
			if purpose == innerStringSession && !sessionHasRune {
				result.validSession = false
			}
			return result, nil
		case '\\':
			escaped, escapeErr := p.next()
			if escapeErr != nil {
				return innerStringResult{}, errors.New("unterminated metadata.user_id JSON escape")
			}
			switch escaped.value {
			case '"', '\\', '/':
				observe(rune(escaped.value))
			case 'b':
				observe('\b')
			case 'f':
				observe('\f')
			case 'n':
				observe('\n')
			case 'r':
				observe('\r')
			case 't':
				observe('\t')
			case 'u':
				runeValue, unicodeErr := p.consumeUnicodeEscape()
				if unicodeErr != nil {
					return innerStringResult{}, unicodeErr
				}
				observe(runeValue)
			default:
				return innerStringResult{}, errors.New("invalid metadata.user_id JSON escape")
			}
		default:
			if value.value < 0x20 {
				return innerStringResult{}, errors.New("control byte in metadata.user_id JSON string")
			}
			if value.value < utf8.RuneSelf {
				observe(rune(value.value))
				continue
			}
			runeValue, runeErr := p.consumeRawRune(value)
			if runeErr != nil {
				return innerStringResult{}, runeErr
			}
			observe(runeValue)
		}
	}
}

func (p *innerJSONParser) consumeUnicodeEscape() (rune, error) {
	var digits [4]byte
	for index := range digits {
		value, err := p.next()
		if err != nil {
			return 0, errors.New("short metadata.user_id unicode escape")
		}
		digits[index] = value.value
	}
	code, ok := decodeHex4(digits[:])
	if !ok {
		return 0, errors.New("invalid metadata.user_id unicode escape")
	}
	if 0xD800 <= code && code <= 0xDBFF {
		following, err := p.peekBytes(6)
		if err == nil && following[0] == '\\' && following[1] == 'u' {
			low, validLow := decodeHex4(following[2:])
			if validLow && 0xDC00 <= low && low <= 0xDFFF {
				for range 6 {
					_, _ = p.next()
				}
				return utf16.DecodeRune(rune(code), rune(low)), nil
			}
		} else if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		return utf8.RuneError, nil
	}
	if 0xDC00 <= code && code <= 0xDFFF {
		return utf8.RuneError, nil
	}
	return rune(code), nil
}

func (p *innerJSONParser) consumeRawRune(first mappedJSONByte) (rune, error) {
	width := 0
	switch {
	case 0xC2 <= first.value && first.value <= 0xDF:
		width = 2
	case 0xE0 <= first.value && first.value <= 0xEF:
		width = 3
	case 0xF0 <= first.value && first.value <= 0xF4:
		width = 4
	default:
		return 0, errors.New("invalid UTF-8 in metadata.user_id JSON string")
	}
	var encoded [utf8.UTFMax]byte
	encoded[0] = first.value
	for index := 1; index < width; index++ {
		value, err := p.next()
		if err != nil {
			return 0, errors.New("short UTF-8 in metadata.user_id JSON string")
		}
		encoded[index] = value.value
	}
	value, decodedWidth := utf8.DecodeRune(encoded[:width])
	if decodedWidth != width {
		return 0, errors.New("invalid UTF-8 in metadata.user_id JSON string")
	}
	return value, nil
}

func (p *innerJSONParser) consumeLiteral(literal string) error {
	for index := range literal {
		value, err := p.next()
		if err != nil || value.value != literal[index] {
			return errors.New("invalid metadata.user_id JSON literal")
		}
	}
	if next, err := p.peek(1); err == nil && !isValueDelimiter(next.value) {
		return errors.New("invalid metadata.user_id JSON literal")
	}
	return nil
}

func (p *innerJSONParser) consumeNumber() error {
	if next, _ := p.peek(1); next.value == '-' {
		_, _ = p.next()
	}
	first, err := p.peek(1)
	if err != nil {
		return errors.New("invalid metadata.user_id JSON number")
	}
	switch {
	case first.value == '0':
		_, _ = p.next()
		if next, peekErr := p.peek(1); peekErr == nil && isDecimalDigit(next.value) {
			return errors.New("invalid metadata.user_id JSON number")
		}
	case '1' <= first.value && first.value <= '9':
		p.consumeDigits()
	default:
		return errors.New("invalid metadata.user_id JSON number")
	}
	if next, peekErr := p.peek(1); peekErr == nil && next.value == '.' {
		_, _ = p.next()
		if next, digitErr := p.peek(1); digitErr != nil || !isDecimalDigit(next.value) {
			return errors.New("invalid metadata.user_id JSON number")
		}
		p.consumeDigits()
	}
	if next, peekErr := p.peek(1); peekErr == nil && (next.value == 'e' || next.value == 'E') {
		_, _ = p.next()
		if sign, signErr := p.peek(1); signErr == nil && (sign.value == '+' || sign.value == '-') {
			_, _ = p.next()
		}
		if next, digitErr := p.peek(1); digitErr != nil || !isDecimalDigit(next.value) {
			return errors.New("invalid metadata.user_id JSON number")
		}
		p.consumeDigits()
	}
	if next, err := p.peek(1); err == nil && !isValueDelimiter(next.value) {
		return errors.New("invalid metadata.user_id JSON number")
	}
	return nil
}

func (p *innerJSONParser) consumeDigits() {
	for {
		next, err := p.peek(1)
		if err != nil || !isDecimalDigit(next.value) {
			return
		}
		_, _ = p.next()
	}
}

func isDecimalDigit(value byte) bool { return '0' <= value && value <= '9' }

func isValueDelimiter(value byte) bool {
	switch value {
	case ' ', '\t', '\r', '\n', ',', '}', ']':
		return true
	default:
		return false
	}
}

func (p *innerJSONParser) skipSpace() error {
	for {
		next, err := p.peek(1)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch next.value {
		case ' ', '\t', '\r', '\n':
			_, _ = p.next()
		default:
			return nil
		}
	}
}

func (p *innerJSONParser) peek(count int) (mappedJSONByte, error) {
	if count <= 0 {
		return mappedJSONByte{}, errors.New("invalid metadata.user_id parser lookahead")
	}
	for len(p.buffer) < count {
		value, err := p.source.next()
		if err != nil {
			return mappedJSONByte{}, err
		}
		p.buffer = append(p.buffer, value)
	}
	return p.buffer[count-1], nil
}

func (p *innerJSONParser) peekBytes(count int) ([]byte, error) {
	if _, err := p.peek(count); err != nil {
		return nil, err
	}
	result := make([]byte, count)
	for index := range result {
		result[index] = p.buffer[index].value
	}
	return result, nil
}

func (p *innerJSONParser) next() (mappedJSONByte, error) {
	if len(p.buffer) == 0 {
		return p.source.next()
	}
	value := p.buffer[0]
	p.buffer = p.buffer[1:]
	return value, nil
}

func newCLIProxyAPIClassifierDefinition() PatchDefinition {
	return PatchDefinition{
		ID:           CLIProxyAPIClassifierSessionID,
		Name:         "CLIProxyAPI Classifier Session Isolation",
		Description:  "Isolates classifier sessions with a stable per-target UUID",
		RequestTypes: []RequestType{RequestTypeClassifier},
		Stages:       []Stage{StageRequest},
		Conflicts:    []string{},
		Idempotence:  PerExecution,
		Factory: func(context FactoryContext) (PatchInstance, error) {
			return newHooksInstance(Hooks{Request: &cliproxyapiSessionPatch{store: context.Services.AliasStore}}), nil
		},
	}
}

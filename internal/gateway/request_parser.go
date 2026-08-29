package gateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type messageFields struct {
	model string
}

type requestParseError struct {
	code    string
	message string
	err     error
}

func (e *requestParseError) Error() string {
	if e.err == nil {
		return e.message
	}
	return e.message + ": " + e.err.Error()
}

func (e *requestParseError) Unwrap() error { return e.err }

func parseMessagesRequest(source io.Reader) (messageFields, error) {
	parser := &jsonStream{reader: bufio.NewReaderSize(source, 32*1024)}
	fields, err := parser.parseTopObject()
	if err != nil {
		var requestErr *requestParseError
		if errors.As(err, &requestErr) {
			return messageFields{}, err
		}
		return messageFields{}, &requestParseError{code: "invalid_json", message: "request body is not valid JSON", err: err}
	}
	if err := parser.skipWhitespace(); err != nil {
		return messageFields{}, &requestParseError{code: "invalid_json", message: "request body is not valid JSON", err: err}
	}
	if _, err := parser.reader.Peek(1); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("unexpected trailing JSON content")
		}
		return messageFields{}, &requestParseError{code: "invalid_json", message: "request body contains trailing JSON", err: err}
	}
	return fields, nil
}

type jsonStream struct {
	reader *bufio.Reader
}

func (p *jsonStream) parseTopObject() (messageFields, error) {
	var fields messageFields
	if err := p.skipWhitespace(); err != nil {
		return fields, err
	}
	first, err := p.readByte()
	if err != nil {
		return fields, err
	}
	if first != '{' {
		return fields, errors.New("request body must be a JSON object")
	}
	modelSeen := false
	if err := p.skipWhitespace(); err != nil {
		return fields, err
	}
	if p.peekByte() == '}' {
		_, _ = p.readByte()
		return fields, &requestParseError{code: "invalid_model", message: "model is required"}
	}
	for {
		key, captured, err := p.readJSONString(128)
		if err != nil {
			return fields, err
		}
		if err := p.expectColon(); err != nil {
			return fields, err
		}
		switch {
		case captured && key == "model":
			if modelSeen {
				return fields, &requestParseError{code: "invalid_model", message: "model must not be repeated"}
			}
			modelSeen = true
			if err := p.skipWhitespace(); err != nil {
				return fields, err
			}
			if p.peekByte() != '"' {
				return fields, &requestParseError{code: "invalid_model", message: "model must be a string"}
			}
			model, _, err := p.readJSONString(-1)
			if err != nil {
				return fields, err
			}
			if model == "" {
				return fields, &requestParseError{code: "invalid_model", message: "model must not be empty"}
			}
			fields.model = model
		default:
			if err := p.parseValue(); err != nil {
				return fields, err
			}
		}
		if err := p.skipWhitespace(); err != nil {
			return fields, err
		}
		next, err := p.readByte()
		if err != nil {
			return fields, err
		}
		switch next {
		case '}':
			if !modelSeen {
				return fields, &requestParseError{code: "invalid_model", message: "model is required"}
			}
			return fields, nil
		case ',':
			if err := p.skipWhitespace(); err != nil {
				return fields, err
			}
		default:
			return fields, fmt.Errorf("expected comma or object end, got %q", next)
		}
	}
}

func (p *jsonStream) parseValue() error {
	if err := p.skipWhitespace(); err != nil {
		return err
	}
	switch p.peekByte() {
	case '"':
		_, _, err := p.readJSONString(0)
		return err
	case '{':
		return p.parseObject()
	case '[':
		return p.parseArray()
	case 't':
		return p.consumeLiteral("true")
	case 'f':
		return p.consumeLiteral("false")
	case 'n':
		return p.consumeLiteral("null")
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return p.parseNumber()
	default:
		return fmt.Errorf("unexpected JSON value byte %q", p.peekByte())
	}
}

func (p *jsonStream) parseObject() error {
	if _, err := p.readByte(); err != nil {
		return err
	}
	if err := p.skipWhitespace(); err != nil {
		return err
	}
	if p.peekByte() == '}' {
		_, _ = p.readByte()
		return nil
	}
	for {
		if _, _, err := p.readJSONString(0); err != nil {
			return err
		}
		if err := p.expectColon(); err != nil {
			return err
		}
		if err := p.parseValue(); err != nil {
			return err
		}
		if err := p.skipWhitespace(); err != nil {
			return err
		}
		next, err := p.readByte()
		if err != nil {
			return err
		}
		if next == '}' {
			return nil
		}
		if next != ',' {
			return fmt.Errorf("expected comma or object end, got %q", next)
		}
		if err := p.skipWhitespace(); err != nil {
			return err
		}
	}
}

func (p *jsonStream) parseArray() error {
	if _, err := p.readByte(); err != nil {
		return err
	}
	if err := p.skipWhitespace(); err != nil {
		return err
	}
	if p.peekByte() == ']' {
		_, _ = p.readByte()
		return nil
	}
	for {
		if err := p.parseValue(); err != nil {
			return err
		}
		if err := p.skipWhitespace(); err != nil {
			return err
		}
		next, err := p.readByte()
		if err != nil {
			return err
		}
		if next == ']' {
			return nil
		}
		if next != ',' {
			return fmt.Errorf("expected comma or array end, got %q", next)
		}
		if err := p.skipWhitespace(); err != nil {
			return err
		}
	}
}

func (p *jsonStream) readJSONString(captureLimit int) (string, bool, error) {
	first, err := p.readByte()
	if err != nil {
		return "", false, err
	}
	if first != '"' {
		return "", false, fmt.Errorf("expected JSON string, got %q", first)
	}
	capturing := captureLimit != 0
	raw := make([]byte, 0, 64)
	if capturing {
		raw = append(raw, '"')
	}
	appendRaw := func(value byte) {
		if !capturing {
			return
		}
		if captureLimit >= 0 && len(raw)+1 > captureLimit {
			capturing = false
			raw = nil
			return
		}
		raw = append(raw, value)
	}
	for {
		value, err := p.readByte()
		if err != nil {
			return "", false, err
		}
		appendRaw(value)
		switch value {
		case '"':
			if !capturing {
				return "", false, nil
			}
			var decoded string
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return "", false, err
			}
			return decoded, true, nil
		case '\\':
			escaped, err := p.readByte()
			if err != nil {
				return "", false, err
			}
			appendRaw(escaped)
			switch escaped {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				for i := 0; i < 4; i++ {
					hex, err := p.readByte()
					if err != nil {
						return "", false, err
					}
					appendRaw(hex)
					if !isHex(hex) {
						return "", false, fmt.Errorf("invalid unicode escape digit %q", hex)
					}
				}
			default:
				return "", false, fmt.Errorf("invalid string escape %q", escaped)
			}
		default:
			if value < 0x20 {
				return "", false, errors.New("unescaped control character in string")
			}
		}
	}
}

func (p *jsonStream) parseNumber() error {
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
		if digit := p.peekByte(); digit < '0' || digit > '9' {
			return errors.New("exponent has no digits")
		}
		p.consumeDigits()
	}
	return nil
}

func (p *jsonStream) consumeDigits() {
	for {
		value := p.peekByte()
		if value < '0' || value > '9' {
			return
		}
		_, _ = p.readByte()
	}
}

func (p *jsonStream) consumeLiteral(literal string) error {
	for i := range literal {
		value, err := p.readByte()
		if err != nil {
			return err
		}
		if value != literal[i] {
			return fmt.Errorf("invalid JSON literal %q", literal)
		}
	}
	return nil
}

func (p *jsonStream) expectColon() error {
	if err := p.skipWhitespace(); err != nil {
		return err
	}
	value, err := p.readByte()
	if err != nil {
		return err
	}
	if value != ':' {
		return fmt.Errorf("expected colon, got %q", value)
	}
	return nil
}

func (p *jsonStream) skipWhitespace() error {
	for {
		switch p.peekByte() {
		case ' ', '\t', '\r', '\n':
			_, _ = p.readByte()
		default:
			return nil
		}
	}
}

func (p *jsonStream) peekByte() byte {
	value, err := p.reader.Peek(1)
	if err != nil {
		return 0
	}
	return value[0]
}

func (p *jsonStream) readByte() (byte, error) {
	return p.reader.ReadByte()
}

func isHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

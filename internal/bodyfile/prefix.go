package bodyfile

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

var (
	ErrInvalidJSONStringPrefix = errors.New("bodyfile: invalid JSON string range")
	ErrNegativePrefixLimit     = errors.New("bodyfile: negative JSON string prefix limit")
)

// ReadJSONStringPrefix decodes one indexed JSON string while retaining at
// most max decoded UTF-8 bytes. It consumes the complete string to validate
// escapes and determine whether the returned prefix is complete, but never
// allocates storage proportional to the string's total length.
//
// The bool result is true when the entire decoded string fits within max;
// false means the returned string is a bounded prefix. A field must come from
// an index bound to body; callers are responsible for validating that binding.
func ReadJSONStringPrefix(body Body, field Field, max int64) (prefix string, complete bool, returnErr error) {
	if body == nil {
		return "", false, ErrNilBody
	}
	if max < 0 {
		return "", false, ErrNegativePrefixLimit
	}
	if field.Type != JSONString || field.StringRange.Start < 0 || field.StringRange.End < field.StringRange.Start+2 || field.StringRange.End > body.Size() {
		return "", false, ErrInvalidJSONStringPrefix
	}
	reader, err := openBodyReader(body)
	if err != nil {
		return "", false, err
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, closeErr)
		}
	}()
	if field.StringRange.Start > 0 {
		if _, err := io.CopyN(io.Discard, reader, field.StringRange.Start); err != nil {
			return "", false, err
		}
	}
	opening := []byte{0}
	if _, err := io.ReadFull(reader, opening); err != nil {
		return "", false, err
	}
	if opening[0] != '"' {
		return "", false, ErrInvalidJSONStringPrefix
	}
	contentLength := field.StringRange.End - field.StringRange.Start - 2
	limited := &io.LimitedReader{R: reader, N: contentLength}
	scanner := bufio.NewReaderSize(limited, 32*1024)
	var result []byte
	if max > 0 {
		capHint := max
		if capHint > contentLength {
			capHint = contentLength
		}
		// Avoid a potentially large upfront allocation when a caller asks for
		// a generous bound; append still remains bounded by max.
		if capHint > 32*1024 {
			capHint = 32 * 1024
		}
		result = make([]byte, 0, int(capHint))
	}
	var decodedBytes int64
	for {
		value, readErr := scanner.ReadByte()
		if readErr != nil {
			if errors.Is(readErr, io.EOF) && limited.N == 0 {
				break
			}
			return "", false, readErr
		}
		var runeValue rune
		switch value {
		case '\\':
			escape, err := scanner.ReadByte()
			if err != nil {
				return "", false, err
			}
			switch escape {
			case '"', '\\', '/':
				runeValue = rune(escape)
			case 'b':
				runeValue = '\b'
			case 'f':
				runeValue = '\f'
			case 'n':
				runeValue = '\n'
			case 'r':
				runeValue = '\r'
			case 't':
				runeValue = '\t'
			case 'u':
				unit, err := readHexUnit(scanner)
				if err != nil {
					return "", false, err
				}
				switch {
				case unit >= 0xd800 && unit <= 0xdbff:
					// A high surrogate must be followed immediately by a low
					// surrogate encoded as another unicode escape.
					backslash, err := scanner.ReadByte()
					if err != nil {
						return "", false, errors.New("bodyfile: unpaired high surrogate")
					}
					u, err := scanner.ReadByte()
					if err != nil || backslash != '\\' || u != 'u' {
						return "", false, errors.New("bodyfile: unpaired high surrogate")
					}
					low, err := readHexUnit(scanner)
					if err != nil || low < 0xdc00 || low > 0xdfff {
						return "", false, errors.New("bodyfile: invalid JSON surrogate pair")
					}
					runeValue = utf16Rune(unit, low)
				case unit >= 0xdc00 && unit <= 0xdfff:
					return "", false, errors.New("bodyfile: unpaired low surrogate")
				default:
					runeValue = rune(unit)
				}
			default:
				return "", false, fmt.Errorf("bodyfile: invalid JSON string escape %q", escape)
			}
		default:
			if value < 0x20 {
				return "", false, ErrInvalidJSONStringPrefix
			}
			if value < utf8.RuneSelf {
				runeValue = rune(value)
			} else {
				decoded, err := readUTF8Rune(scanner, value)
				if err != nil {
					return "", false, err
				}
				runeValue = decoded
			}
		}
		runeSize := utf8.RuneLen(runeValue)
		if runeSize < 0 {
			return "", false, errors.New("bodyfile: invalid decoded JSON rune")
		}
		if decodedBytes > int64(^uint64(0)>>1)-int64(runeSize) {
			return "", false, errors.New("bodyfile: decoded JSON string is too large")
		}
		decodedBytes += int64(runeSize)
		if decodedBytes <= max {
			var encoded [utf8.UTFMax]byte
			n := utf8.EncodeRune(encoded[:], runeValue)
			result = append(result, encoded[:n]...)
		}
	}
	closing := []byte{0}
	if _, err := io.ReadFull(reader, closing); err != nil {
		return "", false, err
	}
	if closing[0] != '"' {
		return "", false, ErrInvalidJSONStringPrefix
	}
	return string(result), decodedBytes <= max, nil
}

// ReadStringPrefix is a concise alias for ReadJSONStringPrefix.
func ReadStringPrefix(body Body, field Field, max int64) (string, bool, error) {
	return ReadJSONStringPrefix(body, field, max)
}

// ReadFieldStringPrefix is a descriptive alias retained for callers that
// name the indexed value explicitly.
func ReadFieldStringPrefix(body Body, field Field, max int64) (string, bool, error) {
	return ReadJSONStringPrefix(body, field, max)
}

func readHexUnit(reader *bufio.Reader) (uint16, error) {
	var value uint16
	for index := 0; index < 4; index++ {
		byteValue, err := reader.ReadByte()
		if err != nil {
			return 0, err
		}
		digit, ok := hexDigitValueOK(byteValue)
		if !ok {
			return 0, errors.New("bodyfile: invalid unicode escape digit")
		}
		value = value<<4 | uint16(digit)
	}
	return value, nil
}

func hexDigitValueOK(value byte) (byte, bool) {
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

func utf16Rune(high, low uint16) rune {
	return rune((uint32(high)-0xd800)<<10 + (uint32(low) - 0xdc00) + 0x10000)
}

func readUTF8Rune(reader *bufio.Reader, first byte) (rune, error) {
	width := 0
	switch {
	case first&0xe0 == 0xc0:
		width = 2
	case first&0xf0 == 0xe0:
		width = 3
	case first&0xf8 == 0xf0:
		width = 4
	default:
		return 0, errors.New("bodyfile: invalid UTF-8 in JSON string")
	}
	var encoded [utf8.UTFMax]byte
	encoded[0] = first
	for index := 1; index < width; index++ {
		value, err := reader.ReadByte()
		if err != nil {
			return 0, err
		}
		encoded[index] = value
	}
	runeValue, size := utf8.DecodeRune(encoded[:width])
	if runeValue == utf8.RuneError && size == 1 || size != width {
		return 0, errors.New("bodyfile: invalid UTF-8 in JSON string")
	}
	return runeValue, nil
}

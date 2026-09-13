package gateway

import "unicode/utf8"

type boundedText struct {
	data      []byte
	limit     int
	truncated bool
}

func (b *boundedText) add(p []byte) {
	if b.limit <= 0 {
		b.limit = DefaultErrorTextLimit
	}
	remaining := b.limit - len(b.data)
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	b.data = append(b.data, p...)
}
func (b *boundedText) text() string {
	data := b.data
	if b.truncated {
		for i := len(data) - 1; i >= 0 && i >= len(data)-utf8.UTFMax; i-- {
			if utf8.RuneStart(data[i]) {
				if !utf8.FullRune(data[i:]) {
					data = data[:i]
				}
				break
			}
		}
	}
	return string(data)
}
func (b *boundedText) reset() { b.data = b.data[:0]; b.truncated = false }

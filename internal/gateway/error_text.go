package gateway

import "github.com/Siriusrry/cc-automux/internal/textlimit"

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
	if !b.truncated {
		return string(b.data)
	}
	value, _ := textlimit.Prefix(string(b.data), b.limit)
	return value
}

func (b *boundedText) reset() { b.data = b.data[:0]; b.truncated = false }

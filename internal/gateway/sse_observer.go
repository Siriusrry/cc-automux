package gateway

import (
	"encoding/json"
	"mime"
	"net/http"
	"strings"

	"github.com/Siriusrry/cc-automux/internal/scheduler"
)

type responseVerdict struct {
	reason     string
	class      scheduler.FailureClass
	raw        string
	incomplete string
}

// sseObserver retains only event names and bounded error data. Ordinary data
// lines are discarded as soon as their field name is identified.
type sseObserver struct {
	event     string
	prefix    []byte
	value     []byte
	mode      byte
	skipSpace bool
	cr        bool
	lineBytes int
	data      boundedText
	dataLines int
	result    *responseVerdict
}

func observeSSE(response *http.Response) bool {
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	encoding := strings.TrimSpace(response.Header.Get("Content-Encoding"))
	return response.StatusCode >= 200 && response.StatusCode < 300 && err == nil && media == "text/event-stream" && (encoding == "" || strings.EqualFold(encoding, "identity"))
}

func (s *sseObserver) feed(p []byte) {
	for _, b := range p {
		if s.result != nil {
			return
		}
		if b == '\n' && s.cr {
			s.cr = false
			continue
		}
		s.cr = false
		if b == '\r' || b == '\n' {
			s.endLine()
			s.cr = b == '\r'
			continue
		}
		s.lineBytes++
		if s.mode == 0 {
			if b == ':' {
				switch string(s.prefix) {
				case "event":
					s.mode = 'e'
				case "data":
					s.mode = 'd'
				default:
					s.mode = 'x'
				}
				s.skipSpace = true
				if s.mode == 'd' && s.event == "error" {
					if s.dataLines > 0 {
						s.data.add([]byte{'\n'})
					}
					s.dataLines++
				}
				continue
			}
			if len(s.prefix) < 6 {
				s.prefix = append(s.prefix, b)
			} else {
				s.mode = 'x'
			}
			continue
		}
		if s.skipSpace {
			s.skipSpace = false
			if b == ' ' {
				continue
			}
		}
		switch s.mode {
		case 'e':
			if len(s.value) < 32 {
				s.value = append(s.value, b)
			}
		case 'd':
			if s.event == "error" {
				s.data.add([]byte{b})
			}
		}
	}
}
func (s *sseObserver) endLine() {
	if s.lineBytes == 0 {
		switch s.event {
		case "message_stop":
			s.result = &responseVerdict{reason: "completed", class: scheduler.FailureNone}
		case "error":
			raw := s.data.text()
			class := scheduler.FailureChannelTransient
			if !s.data.truncated {
				class = sseErrorClass(raw)
			}
			s.result = &responseVerdict{reason: "stream_error_event", class: class, raw: raw}
			if s.data.truncated {
				s.result.incomplete = "truncated"
			}
		}
		s.event = ""
		s.data.reset()
		s.dataLines = 0
	} else if s.mode == 'e' {
		s.event = string(s.value)
	} else if s.mode == 0 && string(s.prefix) == "event" {
		s.event = ""
	}
	s.prefix = s.prefix[:0]
	s.value = s.value[:0]
	s.mode = 0
	s.lineBytes = 0
	s.skipSpace = false
}
func sseErrorClass(raw string) scheduler.FailureClass {
	var event struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(raw), &event) != nil {
		return scheduler.FailureChannelTransient
	}
	switch event.Error.Type {
	case "authentication_error", "permission_error":
		return scheduler.FailureGlobalImmediate
	case "not_found_error":
		return scheduler.FailureChannelImmediate
	case "invalid_request_error", "request_too_large", "billing_error", "conflict_error":
		return scheduler.FailureNeutral
	default:
		return scheduler.FailureChannelTransient
	}
}

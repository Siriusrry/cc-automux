package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Siriusrry/cc-automux/internal/scheduler"
)

const (
	DefaultResponseHeaderTimeout = 200 * time.Second
	DefaultResponseIdleTimeout   = 240 * time.Second
	DefaultErrorBodyTimeout      = 120 * time.Second
	DefaultErrorTextLimit        = 64 * 1024
)

// UpstreamLimits are fixed production limits, injectable for local tests.
type UpstreamLimits struct {
	ResponseHeader time.Duration
	ResponseIdle   time.Duration
	ErrorBody      time.Duration
	ErrorTextBytes int
}

func (v UpstreamLimits) defaults() UpstreamLimits {
	if v.ResponseHeader <= 0 {
		v.ResponseHeader = DefaultResponseHeaderTimeout
	}
	if v.ResponseIdle <= 0 {
		v.ResponseIdle = DefaultResponseIdleTimeout
	}
	if v.ErrorBody <= 0 {
		v.ErrorBody = DefaultErrorBodyTimeout
	}
	if v.ErrorTextBytes <= 0 {
		v.ErrorTextBytes = DefaultErrorTextLimit
	}
	return v
}

type attemptStop struct{ reason, message string }

func (e *attemptStop) Error() string { return e.message }

type upstreamAttempt struct {
	fixed        bool
	background   bool
	result       *responseVerdict
	observer     *sseObserver
	streaming    bool
	status       int
	mu           sync.Mutex
	ctx          context.Context
	cancel       context.CancelCauseFunc
	stopClient   func() bool
	stopShutdown func() bool
	timer        *time.Timer
	epoch        uint64
	headersAt    time.Time
	bytes        int64
	stopped      *attemptStop
	finished     bool
	detached     bool
	limits       UpstreamLimits
}

func (h *Handler) newUpstreamAttempt(client context.Context, stream bool) *upstreamAttempt {
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(client))
	a := &upstreamAttempt{ctx: ctx, cancel: cancel, limits: h.limits}
	a.stopClient = context.AfterFunc(client, func() { a.stop("client_canceled", "", true) })
	a.stopShutdown = context.AfterFunc(h.shutdown, func() { a.stop("shutdown", "gateway is shutting down", false) })
	if stream {
		a.mu.Lock()
		a.armLocked(a.limits.ResponseHeader, "response_header_timeout")
		a.mu.Unlock()
	}
	return a
}

func (a *upstreamAttempt) armLocked(wait time.Duration, reason string) {
	a.stopTimerLocked()
	epoch := a.epoch
	a.timer = time.AfterFunc(wait, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.epoch != epoch || a.finished || a.stopped != nil {
			return
		}
		a.stopLocked(reason, fmt.Sprintf("upstream %s after %.3g seconds; received %d bytes", reason, wait.Seconds(), a.bytes))
	})
}
func (a *upstreamAttempt) stopTimerLocked() {
	a.epoch++
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
}
func (a *upstreamAttempt) stopLocked(reason, message string) {
	if a.stopped != nil || a.finished {
		return
	}
	class := scheduler.FailureChannelImmediate
	if reason == "client_canceled" {
		class = scheduler.FailureClientCanceled
	}
	if reason == "shutdown" {
		class = scheduler.FailureNeutral
	}
	verdictReason := reason
	if reason == "shutdown" {
		verdictReason = "local_error"
	}
	a.claimLocked(responseVerdict{reason: verdictReason, class: class, raw: message})
	a.stopped = &attemptStop{reason: reason, message: message}
	a.stopTimerLocked()
	a.cancel(a.stopped)
}
func (a *upstreamAttempt) stop(reason, message string, client bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if client && a.detached {
		return
	}
	a.stopLocked(reason, message)
}
func (a *upstreamAttempt) receiveHeaders(response *http.Response, err error) (*http.Response, error) {
	a.mu.Lock()
	a.stopTimerLocked()
	stopped := a.stopped
	if stopped == nil && err != nil {
		a.claimLocked(responseVerdict{reason: "transport_error", class: scheduler.FailureGlobalTransient, raw: err.Error()})
	}
	if stopped == nil && err == nil && response != nil {
		a.headersAt = time.Now()
		class := scheduler.ClassifyHTTPStatus(response.StatusCode)
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			if a.fixed || (scheduler.Outcome{Class: class}).ShouldFailover() {
				a.claimLocked(responseVerdict{reason: "http_error", class: class})
				a.detached = true
			}
		}
		if response.Body != nil {
			if !a.detached {
				a.armLocked(a.limits.ResponseIdle, "response_idle_timeout")
			}
			response.Body = &attemptBodyReader{ReadCloser: response.Body, attempt: a}
		}
	}
	a.mu.Unlock()
	if stopped != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, stopped
	}
	return response, err
}
func (a *upstreamAttempt) reason() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped != nil {
		return a.stopped.reason
	}
	return ""
}
func (a *upstreamAttempt) close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.finished = true
	a.stopTimerLocked()
	a.mu.Unlock()
	a.stopClient()
	a.stopShutdown()
	a.cancel(nil)
}
func timeoutResponse(err error) (string, string, bool) {
	var stop *attemptStop
	if !errors.As(err, &stop) {
		return "", "", false
	}
	switch stop.reason {
	case "response_header_timeout":
		return "upstream_response_timeout", stop.message, true
	case "response_idle_timeout":
		return "upstream_idle_timeout", stop.message, true
	}
	return "", "", false
}

// attemptBodyReader resolves timeout/data races before exposing bytes to callers.
type attemptBodyReader struct {
	io.ReadCloser
	attempt  *upstreamAttempt
	once     sync.Once
	closeErr error
}

func (b *attemptBodyReader) Read(p []byte) (int, error) {
	a := b.attempt
	a.mu.Lock()
	stopped := a.stopped
	a.mu.Unlock()
	if stopped != nil {
		return 0, stopped
	}
	n, err := b.ReadCloser.Read(p)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped != nil {
		return 0, a.stopped
	}
	if n > 0 {
		if a.observer != nil {
			a.observer.feed(p[:n])
			if a.observer.result != nil {
				a.claimLocked(*a.observer.result)
			}
		}
		a.bytes += int64(n)
		if !a.background {
			a.armLocked(a.limits.ResponseIdle, "response_idle_timeout")
		}
	}
	if err != nil {
		a.stopTimerLocked()
		if !a.streaming && !errors.Is(err, io.EOF) {
			a.claimLocked(responseVerdict{reason: "stream_interrupted", class: scheduler.FailureChannelStream, raw: err.Error()})
		}
		if a.streaming {
			verdict := responseVerdict{reason: "stream_interrupted", class: scheduler.FailureChannelStream, raw: err.Error()}
			if errors.Is(err, io.EOF) {
				verdict = responseVerdict{reason: "completed", class: scheduler.FailureNone}
				if a.observer != nil {
					verdict = responseVerdict{reason: "stream_truncated", class: scheduler.FailureChannelStream, raw: "stream ended before message_stop"}
				} else if a.status < 200 || a.status >= 300 {
					verdict = responseVerdict{reason: "http_error", class: scheduler.ClassifyHTTPStatus(a.status)}
				}
			}
			a.claimLocked(verdict)
		}
	}
	return n, err
}
func (b *attemptBodyReader) Close() error {
	b.once.Do(func() {
		b.attempt.mu.Lock()
		b.attempt.stopTimerLocked()
		b.attempt.mu.Unlock()
		b.closeErr = b.ReadCloser.Close()
	})
	return b.closeErr
}

func (a *upstreamAttempt) claimLocked(v responseVerdict) {
	if a.result == nil {
		a.result = &v
	}
}
func (a *upstreamAttempt) claim(v responseVerdict) responseVerdict {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.claimLocked(v)
	return *a.result
}
func (a *upstreamAttempt) verdict() responseVerdict {
	if a == nil {
		return responseVerdict{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.result == nil {
		return responseVerdict{}
	}
	return *a.result
}
func (a *upstreamAttempt) observe(response *http.Response, sse bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.streaming = true
	a.status = response.StatusCode
	if sse && observeSSE(response) {
		a.observer = &sseObserver{data: boundedText{limit: a.limits.ErrorTextBytes}}
	}
}

func (a *upstreamAttempt) readBackground() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.detached = true
	a.background = true
	remaining := time.Until(a.headersAt.Add(a.limits.ErrorBody))
	a.armLocked(remaining, "error_body_timeout")
}
func (a *upstreamAttempt) readFinal(client context.Context) {
	a.mu.Lock()
	a.detached = false
	a.armLocked(time.Until(a.headersAt.Add(a.limits.ResponseIdle)), "response_idle_timeout")
	a.mu.Unlock()
	if client.Err() != nil {
		a.stop("client_canceled", "", true)
	}
}

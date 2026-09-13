package gateway

import "io"

func cancelPhase(started bool, status int) cancellationPhase {
	if status != 0 {
		return cancelReceivingResponse
	}
	if started {
		return cancelAwaitingResponse
	}
	return cancelBeforeUpstream
}

// clientWriteError distinguishes a failed downstream write from a read or cleanup error.
type clientWriteError struct{ error }

type downstreamWriter struct {
	io.Writer
	err error
}

func (w *downstreamWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = clientWriteError{err}
	}
	return n, err
}

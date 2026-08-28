package app

import (
	"errors"
	"io"
	"log"
	"os"
	"sync"
)

// LogOpener abstracts startup log-resource acquisition so restart transactions
// can preflight it and tests can inject failures without touching real files.
type LogOpener func(maxBytes int64) (stdout, stderr io.Writer, close func() error, err error)

func logOpenerFor(stdout, stderr io.Writer) LogOpener {
	return func(maxBytes int64) (io.Writer, io.Writer, func() error, error) {
		if maxBytes <= 0 {
			return nil, nil, nil, errors.New("log_max_bytes must be positive")
		}
		return newCappedWriter(maxBytes, stdout), newCappedWriter(maxBytes, stderr), func() error { return nil }, nil
	}
}

// cappedWriter is deliberately small: it preserves complete writes until the
// configured byte ceiling and then drops subsequent log bytes. It never logs
// configuration values or request bodies.
type cappedWriter struct {
	mu        sync.Mutex
	maxBytes  int64
	bytesSeen int64
	delegate  io.Writer
}

func newCappedWriter(maxBytes int64, delegate io.Writer) io.Writer {
	writer := &cappedWriter{maxBytes: maxBytes, delegate: delegate}
	// LaunchAgent opens the log files and passes them as stdout/stderr. Account
	// for their existing size so the configured ceiling remains effective across
	// self-reexecs instead of resetting on every process image.
	if file, ok := delegate.(interface{ Stat() (os.FileInfo, error) }); ok {
		if info, err := file.Stat(); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			writer.bytesSeen = info.Size()
		}
	}
	return writer
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	originalLen := len(p)
	if w.maxBytes <= w.bytesSeen {
		return originalLen, nil
	}
	remaining := w.maxBytes - w.bytesSeen
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	if w.delegate == nil {
		w.bytesSeen += int64(len(p))
		// This is an intentional dropping sink after the configured ceiling;
		// report the original write as consumed so log.Logger does not emit a
		// secondary short-write error when a record crosses the boundary.
		return originalLen, nil
	}
	n, err := w.delegate.Write(p)
	w.bytesSeen += int64(n)
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	return originalLen, nil
}

type logger struct {
	info  *log.Logger
	error *log.Logger
	close func() error
}

func openLogger(opener LogOpener, maxBytes int64) (*logger, error) {
	stdout, stderr, closeFunc, err := opener(maxBytes)
	if err != nil {
		return nil, err
	}
	if closeFunc == nil {
		closeFunc = func() error { return nil }
	}
	if stdout == nil || stderr == nil {
		_ = closeFunc()
		return nil, errors.New("log opener returned a nil writer")
	}
	return &logger{
		info:  log.New(stdout, "cc-automux: ", log.LstdFlags),
		error: log.New(stderr, "cc-automux error: ", log.LstdFlags),
		close: closeFunc,
	}, nil
}

func (l *logger) Close() error {
	if l == nil || l.close == nil {
		return nil
	}
	return l.close()
}

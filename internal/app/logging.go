package app

import (
	"errors"
	"fmt"
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
		stdoutWriter, err := newCappedWriterChecked(maxBytes, stdout)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("initialize stdout log: %w", err)
		}
		stderrWriter, err := newCappedWriterChecked(maxBytes, stderr)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("initialize stderr log: %w", err)
		}
		return stdoutWriter, stderrWriter, func() error { return nil }, nil
	}
}

// cappedWriter keeps each regular-file log stream at or below maxBytes. When a
// write would cross the limit, the target is rolled over in place and the new
// write continues in the fresh window. This is a size limit, not a permanent
// logging stop. Opaque writers that cannot be reset still get the same logical
// window accounting, but their already-emitted bytes cannot be removed by an
// io.Writer-only interface.
type cappedWriter struct {
	mu        sync.Mutex
	maxBytes  int64
	bytesSeen int64
	delegate  io.Writer
	initErr   error
}

// fileLogTarget is the subset of *os.File used to normalize and roll over a
// LaunchAgent stdout/stderr file without taking ownership of its descriptor.
type fileLogTarget interface {
	io.Writer
	Stat() (os.FileInfo, error)
	Truncate(size int64) error
	Seek(offset int64, whence int) (int64, error)
}

type resettableLogTarget interface {
	Reset()
}

func newCappedWriter(maxBytes int64, delegate io.Writer) io.Writer {
	writer, err := newCappedWriterChecked(maxBytes, delegate)
	if err != nil {
		// Keep the historical one-result helper useful in package tests while
		// allowing the production opener to report initialization failures. The
		// first write will surface the same error instead of silently losing logs.
		return &cappedWriter{maxBytes: maxBytes, delegate: delegate, initErr: err}
	}
	return writer
}

func newCappedWriterChecked(maxBytes int64, delegate io.Writer) (*cappedWriter, error) {
	writer := &cappedWriter{maxBytes: maxBytes, delegate: delegate}
	if maxBytes <= 0 {
		return nil, errors.New("log_max_bytes must be positive")
	}
	if err := writer.initialize(); err != nil {
		return nil, err
	}
	return writer, nil
}

func (w *cappedWriter) initialize() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	file, ok := w.delegate.(fileLogTarget)
	if !ok {
		return nil
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if info.Size() > w.maxBytes {
		if err := resetFile(file); err != nil {
			return fmt.Errorf("reset oversized log: %w", err)
		}
		w.bytesSeen = 0
		return nil
	}
	w.bytesSeen = info.Size()
	return nil
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.initErr != nil {
		return 0, w.initErr
	}
	originalLen := len(p)
	if originalLen == 0 {
		return 0, nil
	}
	if file, ok := w.delegate.(fileLogTarget); ok {
		// The descriptor can outlive a previous process image and can also be
		// externally truncated. Refresh the accounting before deciding whether
		// this write starts a new window.
		if info, err := file.Stat(); err != nil {
			return 0, err
		} else if info.Mode().IsRegular() {
			w.bytesSeen = info.Size()
		}
	}

	if int64(len(p)) > w.maxBytes {
		// A single record cannot fit in the configured window. Retain its
		// bounded prefix and report the original write as consumed, matching
		// normal bounded-log behavior while still allowing later records.
		p = p[:int(w.maxBytes)]
	}
	if w.bytesSeen > w.maxBytes || int64(len(p)) > w.maxBytes-w.bytesSeen {
		if err := w.rolloverLocked(); err != nil {
			return 0, err
		}
	}
	if w.delegate == nil {
		w.bytesSeen += int64(len(p))
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

func (w *cappedWriter) rolloverLocked() error {
	if file, ok := w.delegate.(fileLogTarget); ok {
		if info, err := file.Stat(); err != nil {
			return err
		} else if info.Mode().IsRegular() {
			if err := resetFile(file); err != nil {
				return fmt.Errorf("roll over log: %w", err)
			}
		}
	} else if resettable, ok := w.delegate.(resettableLogTarget); ok {
		resettable.Reset()
	}
	w.bytesSeen = 0
	return nil
}

func resetFile(file fileLogTarget) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	_, err := file.Seek(0, io.SeekStart)
	return err
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

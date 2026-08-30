package bodyfile

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestBuilderSealBodyLifecycleAndConcurrentReaders(t *testing.T) {
	directory := t.TempDir()
	builder, err := NewBuilder(directory)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte("body-data-"), 4096)
	if n, err := builder.Write(want); err != nil || n != len(want) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	body, err := builder.Seal()
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if got := body.Size(); got != int64(len(want)) {
		t.Fatalf("Size = %d, want %d", got, len(want))
	}
	if _, err := builder.Write([]byte("late")); !errors.Is(err, ErrBuilderClosed) {
		t.Fatalf("write after seal error = %v", err)
	}
	if _, err := builder.Seal(); !errors.Is(err, ErrBuilderClosed) {
		t.Fatalf("second Seal error = %v", err)
	}
	if err := builder.Abort(); err != nil {
		t.Fatalf("Abort after Seal = %v", err)
	}

	const readers = 8
	var group sync.WaitGroup
	errorsSeen := make(chan error, readers)
	for i := 0; i < readers; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			reader, err := body.OpenReader()
			if err != nil {
				errorsSeen <- err
				return
			}
			defer reader.Close()
			got, err := io.ReadAll(reader)
			if err != nil {
				errorsSeen <- err
				return
			}
			if !bytes.Equal(got, want) {
				errorsSeen <- errors.New("reader bytes differ")
			}
		}()
	}
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Error(err)
	}

	if runtime.GOOS != "windows" {
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("Unix body file was not immediately unlinked: %v", entries)
		}
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if _, err := body.OpenReader(); !errors.Is(err, ErrBodyClosed) {
		t.Fatalf("OpenReader after close error = %v", err)
	}
}

func TestBuilderAbortAndCaptureFailureCleanup(t *testing.T) {
	directory := t.TempDir()
	builder, err := NewBuilder(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := builder.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	if err := builder.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := builder.Abort(); err != nil {
		t.Fatalf("second Abort = %v", err)
	}
	if _, err := builder.Seal(); !errors.Is(err, ErrBuilderClosed) {
		t.Fatalf("Seal after abort error = %v", err)
	}

	_, err = Capture(&failingReader{}, directory)
	if err == nil || !strings.Contains(err.Error(), "read body") {
		t.Fatalf("Capture error = %v", err)
	}
	if !IsReadError(err) || !errors.Is(err, ErrRead) {
		t.Fatalf("Capture read error classification = %v", err)
	}
	if errors.Is(err, ErrLocalIO) {
		t.Fatalf("source reader error was classified as local I/O: %v", err)
	}
	entries, readErr := os.ReadDir(directory)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary body survived failure: %v", entries)
	}
}

func TestLocalIOErrorsAreClassifiedByOperation(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		_, err := NewBuilder(filepath.Join(t.TempDir(), "missing", "directory"))
		assertLocalIO(t, err, LocalIOCreate)
		if errors.Is(err, ErrSourceRead) {
			t.Fatalf("create error classified as source read: %v", err)
		}
	})

	t.Run("write", func(t *testing.T) {
		file := closedTemporaryFile(t)
		builder := &fileBuilder{file: file}
		_, err := builder.Write([]byte("x"))
		assertLocalIO(t, err, LocalIOWrite)

		file = closedTemporaryFile(t)
		err = copyIntoBuilder(&fileBuilder{file: file}, strings.NewReader("x"))
		assertLocalIO(t, err, LocalIOWrite)
		if !errors.Is(err, ErrWrite) {
			t.Fatalf("copy write error does not match ErrWrite: %v", err)
		}
	})

	t.Run("open", func(t *testing.T) {
		cause := errors.New("open failed")
		_, err := IndexObject(&faultBody{openErr: cause})
		assertLocalIO(t, err, LocalIOOpen)
		if !errors.Is(err, cause) {
			t.Fatalf("open cause not preserved: %v", err)
		}
	})

	t.Run("read", func(t *testing.T) {
		cause := errors.New("read failed")
		_, err := IndexObject(&faultBody{
			size:   2,
			reader: &faultReadCloser{readErr: cause},
		})
		assertLocalIO(t, err, LocalIORead)
		if !errors.Is(err, cause) {
			t.Fatalf("read cause not preserved: %v", err)
		}

		file := closedTemporaryFile(t)
		body := &fileBody{file: file, size: 1}
		reader, openErr := body.OpenReader()
		if openErr != nil {
			t.Fatal(openErr)
		}
		_, err = reader.Read(make([]byte, 1))
		assertLocalIO(t, err, LocalIORead)
	})

	t.Run("close", func(t *testing.T) {
		cause := errors.New("reader close failed")
		_, err := IndexObject(&faultBody{
			size:   2,
			reader: &faultReadCloser{Reader: strings.NewReader("{}"), closeErr: cause},
		})
		assertLocalIO(t, err, LocalIOClose)
		if !errors.Is(err, cause) {
			t.Fatalf("close cause not preserved: %v", err)
		}

		file := closedTemporaryFile(t)
		err = (&fileBody{file: file}).Close()
		assertLocalIO(t, err, LocalIOClose)
	})

	t.Run("abort", func(t *testing.T) {
		file := closedTemporaryFile(t)
		err := (&fileBuilder{file: file}).Abort()
		assertLocalIO(t, err, LocalIOAbort)
	})
}

func TestCaptureAbortsWhenSourcePanics(t *testing.T) {
	directory := t.TempDir()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Capture did not propagate source panic")
			}
		}()
		_, _ = Capture(panicReader{}, directory)
	}()
	if entries, err := os.ReadDir(directory); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && len(entries) != 0 {
		t.Fatalf("body file survived panic: %v", entries)
	}
}

func TestCaptureHasNoSizeThreshold(t *testing.T) {
	// Use a generated stream rather than retaining the complete input.  This is
	// large enough to cross common in-memory spool thresholds while remaining a
	// focused package test.
	const size = 3*1024*1024 + 17
	source := io.LimitReader(&repeatingReader{pattern: []byte("abc123")}, size)
	body, err := Capture(source, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if body.Size() != size {
		t.Fatalf("Size = %d, want %d", body.Size(), size)
	}
	reader, err := body.OpenReader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if n, err := io.Copy(io.Discard, reader); err != nil || n != size {
		t.Fatalf("copy = %d, %v", n, err)
	}
}

func TestBodyFileDoesNotExposeTemporaryPath(t *testing.T) {
	directory := t.TempDir()
	body, err := Capture(strings.NewReader("{}"), directory)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	// Public Body deliberately has no Name method.  Also guard against a path
	// accidentally appearing in its formatted representation.
	if _, ok := body.(interface{ Name() string }); ok {
		t.Fatal("Body unexpectedly exposes Name")
	}
	if strings.Contains(strings.TrimSpace(directory), filepath.Base(directory)+string(os.PathSeparator)) {
		t.Fatal("invalid test directory")
	}
}

type failingReader struct{ emitted bool }

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.emitted {
		r.emitted = true
		return copy(p, "partial"), nil
	}
	return 0, errors.New("source failed")
}

type repeatingReader struct {
	pattern []byte
	offset  int
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("reader panic") }

func (r *repeatingReader) Read(p []byte) (int, error) {
	for index := range p {
		p[index] = r.pattern[r.offset%len(r.pattern)]
		r.offset++
	}
	return len(p), nil
}

type faultBody struct {
	size    int64
	reader  io.ReadCloser
	openErr error
}

func (b *faultBody) OpenReader() (io.ReadCloser, error) { return b.reader, b.openErr }
func (b *faultBody) Size() int64                        { return b.size }
func (b *faultBody) Close() error                       { return nil }

type faultReadCloser struct {
	io.Reader
	readErr  error
	closeErr error
}

func (r *faultReadCloser) Read(p []byte) (int, error) {
	if r.readErr != nil {
		return 0, r.readErr
	}
	return r.Reader.Read(p)
}

func (r *faultReadCloser) Close() error { return r.closeErr }

func closedTemporaryFile(t *testing.T) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "closed-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return file
}

func assertLocalIO(t *testing.T, err error, operation LocalIOOperation) {
	t.Helper()
	if err == nil || !errors.Is(err, ErrLocalIO) || !IsLocalIOError(err) {
		t.Fatalf("error = %v, want local I/O", err)
	}
	if errors.Is(err, ErrSourceRead) {
		t.Fatalf("local error classified as source read: %v", err)
	}
	var localErr *LocalIOError
	if !errors.As(err, &localErr) {
		t.Fatalf("error has no LocalIOError: %v", err)
	}
	if localErr.Operation != operation {
		t.Fatalf("operation = %q, want %q", localErr.Operation, operation)
	}
}

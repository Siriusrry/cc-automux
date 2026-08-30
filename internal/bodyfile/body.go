// Package bodyfile provides request-lifetime, immutable bodies backed by a
// temporary file.  Bodies deliberately expose only read operations; all
// writes happen through a Builder and become visible after Seal succeeds.
package bodyfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// Body is an immutable, request-scoped byte stream.
//
// OpenReader returns a new reader positioned at offset zero.  Callers may
// open and consume more than one reader concurrently.  Close is idempotent;
// it releases the backing temporary file and does not expose its path.
type Body interface {
	OpenReader() (io.ReadCloser, error)
	Size() int64
	Close() error
}

// Builder is the only mutable body surface.  A builder must either be sealed
// successfully or aborted by its owner.
type Builder interface {
	Write([]byte) (int, error)
	Seal() (Body, error)
	Abort() error
}

var (
	ErrNilBody       = errors.New("bodyfile: nil body")
	ErrBuilderClosed = errors.New("bodyfile: builder is closed")
	ErrBodyClosed    = errors.New("bodyfile: body is closed")
	ErrSourceRead    = errors.New("bodyfile: source read failed")
	// ErrRead is the retained name for ErrSourceRead.
	ErrRead    = ErrSourceRead
	ErrWrite   = errors.New("bodyfile: body write failed")
	ErrLocalIO = errors.New("bodyfile: local I/O failed")
)

// LocalIOOperation identifies the request-body file operation that failed.
type LocalIOOperation string

const (
	LocalIOCreate LocalIOOperation = "create"
	LocalIOWrite  LocalIOOperation = "write"
	LocalIOOpen   LocalIOOperation = "open"
	LocalIORead   LocalIOOperation = "read"
	LocalIOClose  LocalIOOperation = "close"
	LocalIOAbort  LocalIOOperation = "abort"
)

// LocalIOError preserves an underlying filesystem error while providing one
// stable errors.Is classification for all request-lifetime body I/O.
type LocalIOError struct {
	Operation LocalIOOperation
	Err       error
}

func (e *LocalIOError) Error() string {
	if e == nil {
		return ErrLocalIO.Error()
	}
	if e.Err == nil {
		return fmt.Sprintf("bodyfile: local %s failed", e.Operation)
	}
	return fmt.Sprintf("bodyfile: local %s failed: %v", e.Operation, e.Err)
}

func (e *LocalIOError) Unwrap() error {
	if e == nil || e.Err == nil {
		return ErrLocalIO
	}
	return errors.Join(ErrLocalIO, e.Err)
}

func IsLocalIOError(err error) bool { return errors.Is(err, ErrLocalIO) }

func wrapLocalIO(operation LocalIOOperation, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrLocalIO) {
		return err
	}
	return &LocalIOError{Operation: operation, Err: err}
}

// ReadError and WriteError preserve the underlying I/O cause while allowing
// gateway callers to distinguish malformed client reads from local spool
// creation/seal failures without inspecting error strings.
type ReadError struct{ Err error }

func (e *ReadError) Error() string {
	if e == nil || e.Err == nil {
		return "read body: " + ErrRead.Error()
	}
	return "read body: " + e.Err.Error()
}
func (e *ReadError) Unwrap() error {
	if e == nil {
		return ErrSourceRead
	}
	return errors.Join(ErrSourceRead, e.Err)
}

type WriteError struct{ Err error }

func (e *WriteError) Error() string {
	if e == nil || e.Err == nil {
		return "write body: " + ErrWrite.Error()
	}
	return "write body: " + e.Err.Error()
}
func (e *WriteError) Unwrap() error {
	if e == nil {
		return ErrWrite
	}
	return errors.Join(ErrWrite, e.Err)
}

func IsReadError(err error) bool  { return errors.Is(err, ErrSourceRead) }
func IsWriteError(err error) bool { return errors.Is(err, ErrWrite) }

// fileBody is intentionally kept private.  In particular, no path is
// retained in a public value and no operation exposes the underlying writer.
type fileBody struct {
	file    *os.File
	size    int64
	binding *scanBinding

	mu     sync.RWMutex
	closed bool
}

type fileBuilder struct {
	file    *os.File
	size    int64
	binding *scanBinding

	mu      sync.Mutex
	sealed  bool
	aborted bool
}

// NewBuilder creates a temporary body in directory.  With no directory (or
// an empty directory), the platform temporary directory is used.  The
// variadic form keeps the API convenient for both production callers and
// tests that want to pin a directory.
func NewBuilder(directory ...string) (Builder, error) {
	dir := ""
	if len(directory) > 0 {
		dir = directory[0]
	}
	file, err := createBodyFile(dir)
	if err != nil {
		return nil, wrapLocalIO(LocalIOCreate, err)
	}
	return &fileBuilder{file: file, binding: &scanBinding{}}, nil
}

// NewFileBuilder is an explicit-name alias for NewBuilder.
func NewFileBuilder(directory string) (Builder, error) { return NewBuilder(directory) }

// Capture copies source into a sealed Body using a bounded transfer buffer.
// It never imposes a size limit and never materialises the complete input in
// memory.  On any read/write/seal failure the temporary file is aborted.
func Capture(source io.Reader, directory ...string) (body Body, err error) {
	if source == nil {
		return nil, fmt.Errorf("capture body: %w", ErrNilBody)
	}
	builder, err := NewBuilder(directory...)
	if err != nil {
		return nil, err
	}
	// Abort on every non-success path, including a panic from an adversarial
	// reader or filesystem implementation.  Unix files are already unlinked;
	// this defer is still required on Windows to release delete-on-close.
	sealed := false
	defer func() {
		if !sealed {
			if abortErr := builder.Abort(); abortErr != nil {
				err = errors.Join(err, abortErr)
			}
		}
	}()
	if err := copyIntoBuilder(builder, source); err != nil {
		return nil, err
	}
	sealedBody, sealErr := builder.Seal()
	if sealErr != nil {
		return nil, fmt.Errorf("seal body: %w", sealErr)
	}
	sealed = true
	return sealedBody, nil
}

// NewBodyFromReader is a descriptive alias for Capture.
func NewBodyFromReader(source io.Reader, directory ...string) (Body, error) {
	return Capture(source, directory...)
}

func copyIntoBuilder(builder Builder, source io.Reader) error {
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			written := 0
			for written < n {
				count, writeErr := builder.Write(buffer[written:n])
				if count > 0 {
					written += count
				}
				if writeErr != nil {
					return &WriteError{Err: wrapLocalIO(LocalIOWrite, writeErr)}
				}
				if count == 0 {
					return &WriteError{Err: wrapLocalIO(LocalIOWrite, io.ErrShortWrite)}
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return &ReadError{Err: readErr}
		}
		if n == 0 {
			return &ReadError{Err: io.ErrNoProgress}
		}
	}
}

func (b *fileBuilder) Write(p []byte) (int, error) {
	if b == nil {
		return 0, ErrBuilderClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.file == nil || b.sealed || b.aborted {
		return 0, ErrBuilderClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	n, err := b.file.Write(p)
	if n > 0 {
		b.size += int64(n)
	}
	return n, wrapLocalIO(LocalIOWrite, err)
}

func (b *fileBuilder) Seal() (Body, error) {
	if b == nil {
		return nil, ErrBuilderClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.file == nil || b.sealed || b.aborted {
		return nil, ErrBuilderClosed
	}
	// Writes are synchronous on os.File.  Sync is intentionally not required:
	// the body is consumed by this process and a durable disk commit is outside
	// the request-lifetime contract.
	file := b.file
	size := b.size
	b.file = nil
	b.sealed = true
	return &fileBody{file: file, size: size, binding: b.binding}, nil
}

func (b *fileBuilder) Abort() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.file == nil {
		b.aborted = true
		return nil
	}
	file := b.file
	b.file = nil
	b.aborted = true
	return wrapLocalIO(LocalIOAbort, file.Close())
}

func (b *fileBody) OpenReader() (io.ReadCloser, error) {
	if b == nil {
		return nil, wrapLocalIO(LocalIOOpen, ErrBodyClosed)
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.file == nil || b.closed {
		return nil, wrapLocalIO(LocalIOOpen, ErrBodyClosed)
	}
	return &bodyReader{reader: io.NewSectionReader(b.file, 0, b.size)}, nil
}

// Reader is a compatibility alias used by older replay callers.  New code
// should use OpenReader as required by the body contract.
func (b *fileBody) Reader() (io.ReadCloser, error) { return b.OpenReader() }

func (b *fileBody) Size() int64 {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.size
}

func (b *fileBody) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.file == nil || b.closed {
		b.closed = true
		return nil
	}
	b.closed = true
	file := b.file
	b.file = nil
	return wrapLocalIO(LocalIOClose, file.Close())
}

type bodyReader struct{ reader io.Reader }

func (r *bodyReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, wrapLocalIO(LocalIORead, err)
	}
	return n, err
}
func (r *bodyReader) Close() error { return nil }

type classifiedReader struct{ reader io.ReadCloser }

func openBodyReader(body Body) (io.ReadCloser, error) {
	reader, err := body.OpenReader()
	if err != nil {
		return nil, wrapLocalIO(LocalIOOpen, err)
	}
	return &classifiedReader{reader: reader}, nil
}

func (r *classifiedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, wrapLocalIO(LocalIORead, err)
	}
	return n, err
}

func (r *classifiedReader) Close() error {
	if r == nil || r.reader == nil {
		return nil
	}
	return wrapLocalIO(LocalIOClose, r.reader.Close())
}

// bodyIdentity is used by JSONIndex to make accidental cross-body reuse
// detectable without exposing the backing file or path publicly.
func (b *fileBody) bodyIdentity() *fileBody { return b }

// scanBinding is an in-process identity token shared by a Builder and the
// Body produced by its Seal. It lets the incremental scanner bind its result
// without reopening the sealed file merely to compare bytes.
type scanBinding struct{}

func (b *fileBuilder) scanBinding() *scanBinding {
	if b == nil {
		return nil
	}
	return b.binding
}

func (b *fileBody) scanBinding() *scanBinding {
	if b == nil {
		return nil
	}
	return b.binding
}

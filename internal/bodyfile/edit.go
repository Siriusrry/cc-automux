package bodyfile

import (
	"errors"
	"fmt"
	"io"
	"sort"
)

// Edit replaces the half-open byte range [Start, End) with Replacement.
// Replacement is nil/empty for a deletion or insertion-only edit.
type Edit struct {
	Start       int64
	End         int64
	Replacement []byte
}

// ByteRangeEdit and RangeEdit are descriptive aliases for Edit.
type ByteRangeEdit = Edit
type RangeEdit = Edit

var (
	ErrInvalidEdit = errors.New("bodyfile: invalid byte-range edit")
	ErrEditOverlap = errors.New("bodyfile: overlapping byte-range edits")
	// ErrIndexRequired prevents a no-op scan helper from silently reopening a
	// body. Callers that already have an index should use
	// ApplyEditsAndScanWithIndex for the no-edit case.
	ErrIndexRequired = errors.New("bodyfile: existing JSON index is required for an empty edit set")
)

// ApplyEdits streams body through ordered, non-overlapping edits and returns a
// newly sealed Body.  An empty edit list is a true no-op and returns body
// itself, preserving the immutable BaseBody for unmodified attempts.
func ApplyEdits(body Body, edits []Edit, directory ...string) (Body, error) {
	dir := ""
	if len(directory) > 0 {
		dir = directory[0]
	}
	return ApplyEditsIn(body, edits, dir)
}

// ApplyByteRangeEdits is an alias for ApplyEdits.
func ApplyByteRangeEdits(body Body, edits []ByteRangeEdit, directory ...string) (Body, error) {
	return ApplyEdits(body, edits, directory...)
}

// EditBody is an alias for ApplyEdits.
func EditBody(body Body, edits []Edit, directory ...string) (Body, error) {
	return ApplyEdits(body, edits, directory...)
}

// ApplyEditsIn is ApplyEdits with an explicit temporary directory.  It is
// useful for tests and for callers that already have a request-local spool
// directory.  The directory is never retained by the returned Body.
func ApplyEditsIn(body Body, edits []Edit, directory string) (result Body, returnErr error) {
	if body == nil {
		return nil, ErrNilBody
	}
	if err := validateEdits(body.Size(), edits); err != nil {
		return nil, err
	}
	if len(edits) == 0 {
		return body, nil
	}
	reader, err := openBodyReader(body)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			if result != nil && returnErr == nil {
				cleanupErr := result.Close()
				result = nil
				returnErr = errors.Join(closeErr, cleanupErr)
				return
			}
			returnErr = errors.Join(returnErr, closeErr)
		}
	}()
	builder, err := NewBuilder(directory)
	if err != nil {
		return nil, err
	}
	abort := true
	defer func() {
		if abort {
			if abortErr := builder.Abort(); abortErr != nil {
				returnErr = errors.Join(returnErr, abortErr)
			}
		}
	}()

	var cursor int64
	for _, edit := range edits {
		if err := copyBodyRange(builder, reader, edit.Start-cursor); err != nil {
			return nil, fmt.Errorf("copy body before edit: %w", err)
		}
		cursor = edit.Start
		if len(edit.Replacement) > 0 {
			if err := writeAll(builder, edit.Replacement); err != nil {
				return nil, fmt.Errorf("write edit replacement: %w", err)
			}
		}
		if edit.End > cursor {
			if err := discardBodyRange(reader, edit.End-cursor); err != nil {
				return nil, fmt.Errorf("skip edited body range: %w", err)
			}
			cursor = edit.End
		}
	}
	if err := copyBodyRange(builder, reader, body.Size()-cursor); err != nil {
		return nil, fmt.Errorf("copy body after edits: %w", err)
	}
	result, err = builder.Seal()
	if err != nil {
		return nil, fmt.Errorf("seal edited body: %w", err)
	}
	abort = false
	return result, nil
}

// ApplyEditsAndScan streams body through edits while writing and selectively
// scanning the derived body in lockstep. It never seals and then rescans the
// derived file. Callers use the returned index for the next hook in a patch
// chain.
func ApplyEditsAndScan(body Body, edits []Edit, spec ScanSpec, directory ...string) (result Body, index JSONIndex, returnErr error) {
	if body == nil {
		return nil, JSONIndex{}, ErrNilBody
	}
	if err := validateEdits(body.Size(), edits); err != nil {
		return nil, JSONIndex{}, err
	}
	if len(edits) == 0 {
		return body, JSONIndex{}, ErrIndexRequired
	}
	dir := ""
	if len(directory) > 0 {
		dir = directory[0]
	}
	scanner, err := NewJSONScanner(spec)
	if err != nil {
		return nil, JSONIndex{}, err
	}
	reader, err := openBodyReader(body)
	if err != nil {
		return nil, JSONIndex{}, err
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			if result != nil && returnErr == nil {
				cleanupErr := result.Close()
				result = nil
				index = JSONIndex{}
				returnErr = errors.Join(closeErr, cleanupErr)
				return
			}
			returnErr = errors.Join(returnErr, closeErr)
		}
	}()
	builder, err := NewBuilder(dir)
	if err != nil {
		return nil, JSONIndex{}, err
	}
	if binder, ok := builder.(interface{ scanBinding() *scanBinding }); ok {
		scanner.binding = binder.scanBinding()
	}
	sink := &scanningBuilder{Builder: builder, scanner: scanner}
	abort := true
	defer func() {
		if abort {
			if abortErr := builder.Abort(); abortErr != nil {
				returnErr = errors.Join(returnErr, abortErr)
			}
		}
	}()

	var cursor int64
	for _, edit := range edits {
		if err := copyBodyRange(sink, reader, edit.Start-cursor); err != nil {
			return nil, JSONIndex{}, fmt.Errorf("copy body before edit: %w", err)
		}
		cursor = edit.Start
		if len(edit.Replacement) > 0 {
			if err := writeAll(sink, edit.Replacement); err != nil {
				return nil, JSONIndex{}, fmt.Errorf("write edit replacement: %w", err)
			}
		}
		if edit.End > cursor {
			if err := discardBodyRange(reader, edit.End-cursor); err != nil {
				return nil, JSONIndex{}, fmt.Errorf("skip edited body range: %w", err)
			}
			cursor = edit.End
		}
	}
	if err := copyBodyRange(sink, reader, body.Size()-cursor); err != nil {
		return nil, JSONIndex{}, fmt.Errorf("copy body after edits: %w", err)
	}
	scanResult, err := scanner.Finish()
	if err != nil {
		return nil, JSONIndex{}, err
	}
	result, err = builder.Seal()
	if err != nil {
		return nil, JSONIndex{}, fmt.Errorf("seal edited body: %w", err)
	}
	abort = false
	scanResult.source = result
	index, err = scanResult.Bind(result)
	if err != nil {
		cleanupErr := result.Close()
		result = nil
		return nil, JSONIndex{}, errors.Join(err, cleanupErr)
	}
	return result, index, nil
}

// ApplyEditsAndScanWithIndex is the indexed form used when a hook has already
// inspected the current body. A no-op returns the existing body/index without
// opening a reader; non-empty edits delegate to the lockstep writer/scanner.
func ApplyEditsAndScanWithIndex(body Body, index JSONIndex, edits []Edit, spec ScanSpec, directory ...string) (Body, JSONIndex, error) {
	if body == nil {
		return nil, JSONIndex{}, ErrNilBody
	}
	if err := index.ValidateBody(body); err != nil {
		return nil, JSONIndex{}, err
	}
	if len(edits) == 0 {
		projected, err := index.Project(spec)
		if err != nil {
			return nil, JSONIndex{}, err
		}
		return body, projected, nil
	}
	return ApplyEditsAndScan(body, edits, spec, directory...)
}

type scanningBuilder struct {
	Builder
	scanner *JSONScanner
}

func (b *scanningBuilder) Write(data []byte) (int, error) {
	if b == nil || b.Builder == nil || b.scanner == nil {
		return 0, ErrBuilderClosed
	}
	n, err := b.Builder.Write(data)
	if n > 0 {
		if _, scanErr := b.scanner.Write(data[:n]); scanErr != nil {
			return n, &scanWriteError{Err: scanErr, writeErr: err}
		}
	}
	return n, err
}

func validateEdits(size int64, edits []Edit) error {
	if size < 0 {
		return fmt.Errorf("%w: negative body size", ErrInvalidEdit)
	}
	var previous Edit
	for index, edit := range edits {
		if edit.Start < 0 || edit.End < edit.Start || edit.End > size {
			return fmt.Errorf("%w at index %d: range [%d,%d) outside body size %d", ErrInvalidEdit, index, edit.Start, edit.End, size)
		}
		if index > 0 {
			if edit.Start < previous.End || (edit.Start == previous.Start && edit.End == previous.End) {
				return fmt.Errorf("%w at index %d", ErrEditOverlap, index)
			}
			// Same-position insertion edits are ambiguous even though their
			// ranges are empty; require the caller to combine them explicitly.
			if edit.Start == previous.Start && edit.End == edit.Start {
				return fmt.Errorf("%w at index %d: same-position insertions", ErrEditOverlap, index)
			}
		}
		previous = edit
	}
	return nil
}

func writeAll(builder Builder, data []byte) error {
	for len(data) > 0 {
		n, err := builder.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			var scanErr *scanWriteError
			if errors.As(err, &scanErr) {
				if scanErr.writeErr != nil {
					return errors.Join(scanErr.Err, scanErr.writeErr)
				}
				return scanErr.Err
			}
			return &WriteError{Err: wrapLocalIO(LocalIOWrite, err)}
		}
		if n == 0 {
			return &WriteError{Err: wrapLocalIO(LocalIOWrite, io.ErrShortWrite)}
		}
	}
	return nil
}

// scanWriteError distinguishes a semantic JSON scanner failure from a real
// Builder I/O failure. ApplyEditsAndScan uses it so malformed derived JSON is
// reported as ErrInvalidJSON/patch_failed rather than replay_unavailable.
type scanWriteError struct {
	Err      error
	writeErr error
}

func (e *scanWriteError) Error() string {
	if e == nil || e.Err == nil {
		return "JSON scan failed"
	}
	return e.Err.Error()
}

func (e *scanWriteError) Unwrap() error {
	if e == nil {
		return nil
	}
	return errors.Join(e.Err, e.writeErr)
}

func copyBodyRange(builder Builder, reader io.Reader, count int64) error {
	if count < 0 {
		return ErrInvalidEdit
	}
	if count == 0 {
		return nil
	}
	buffer := make([]byte, 32*1024)
	remaining := count
	for remaining > 0 {
		want := int64(len(buffer))
		if want > remaining {
			want = remaining
		}
		n, err := io.ReadFull(reader, buffer[:want])
		if n > 0 {
			if writeErr := writeAll(builder, buffer[:n]); writeErr != nil {
				return writeErr
			}
			remaining -= int64(n)
		}
		if err != nil {
			return wrapLocalIO(LocalIORead, err)
		}
	}
	return nil
}

func discardBodyRange(reader io.Reader, count int64) error {
	if count < 0 {
		return ErrInvalidEdit
	}
	if count == 0 {
		return nil
	}
	_, err := io.CopyN(io.Discard, reader, count)
	if err != nil {
		return wrapLocalIO(LocalIORead, err)
	}
	return nil
}

// SortedEdits returns a defensive sorted copy.  ApplyEdits intentionally
// requires callers to provide sorted edits so accidental semantic reordering
// is caught; this helper is available when sorting is explicitly desired.
func SortedEdits(edits []Edit) []Edit {
	result := append([]Edit(nil), edits...)
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Start != result[j].Start {
			return result[i].Start < result[j].Start
		}
		return result[i].End < result[j].End
	})
	return result
}

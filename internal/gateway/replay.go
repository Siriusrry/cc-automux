package gateway

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
)

type replayBody struct {
	file   *os.File
	size   int64
	closed bool
}

func spoolRequestBody(body io.Reader, directory string) (*replayBody, error) {
	file, err := createReplayFile(directory)
	if err != nil {
		return nil, fmt.Errorf("create request replay file: %w", err)
	}
	replay := &replayBody{file: file}
	if err := replay.copyFrom(body); err != nil {
		_ = replay.Close()
		return nil, err
	}
	return replay, nil
}

func (r *replayBody) copyFrom(source io.Reader) error {
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			written := 0
			for written < n {
				count, writeErr := r.file.Write(buffer[written:n])
				if count > 0 {
					written += count
					r.size += int64(count)
				}
				if writeErr != nil {
					return fmt.Errorf("write request replay file: %w", writeErr)
				}
				if count == 0 {
					return fmt.Errorf("write request replay file: %w", io.ErrShortWrite)
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return &requestBodyReadError{err: readErr}
		}
		if n == 0 {
			return fmt.Errorf("read request body: %w", io.ErrNoProgress)
		}
	}
}

type requestBodyReadError struct {
	err error
}

func (e *requestBodyReadError) Error() string { return "read request body: " + e.err.Error() }
func (e *requestBodyReadError) Unwrap() error { return e.err }

func isRequestBodyReadError(err error) bool {
	var readErr *requestBodyReadError
	return errors.As(err, &readErr)
}

func (r *replayBody) Reader() (io.ReadCloser, error) {
	if r == nil || r.file == nil || r.closed {
		return nil, errors.New("request replay file is closed")
	}
	section := io.NewSectionReader(r.file, 0, r.size)
	return &replayReader{reader: section}, nil
}

func (r *replayBody) Size() int64 {
	if r == nil {
		return 0
	}
	return r.size
}

func (r *replayBody) Close() error {
	if r == nil || r.file == nil || r.closed {
		return nil
	}
	r.closed = true
	return r.file.Close()
}

type replayReader struct {
	reader io.Reader
}

func (r *replayReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, &replayReadError{err: err}
	}
	return n, err
}

func (r *replayReader) Close() error { return nil }

type replayReadError struct {
	err error
}

func (e *replayReadError) Error() string { return "read request replay file: " + e.err.Error() }
func (e *replayReadError) Unwrap() error { return e.err }

func requestBodyReadFailed(err error) bool {
	var replayErr *replayReadError
	return errors.As(err, &replayErr)
}

func closeRequestBody(request *http.Request) {
	if request != nil && request.Body != nil {
		_ = request.Body.Close()
	}
}

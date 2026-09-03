package logs

import (
	"bytes"
	"context"
	"errors"
	"io"
)

const reverseReadChunk = int64(64 * 1024)

type HistoryQuery struct {
	Filter Filter
	Limit  int
	Cursor *Cursor
}

func (p Parameters) HistoryQuery() HistoryQuery {
	return HistoryQuery{Filter: p.Filter, Limit: p.Limit, Cursor: p.Cursor}
}

type Page struct {
	Items            []Record `json:"items"`
	HasMore          bool     `json:"has_more"`
	NextCursor       string   `json:"next_cursor,omitempty"`
	SkippedMalformed int      `json:"skipped_malformed"`
}

type Reader struct {
	source SnapshotSource
}

func NewReader(source SnapshotSource) (*Reader, error) {
	if source == nil {
		return nil, errors.New("log snapshot source must not be nil")
	}
	return &Reader{source: source}, nil
}

func (r *Reader) Query(ctx context.Context, query HistoryQuery) (page Page, resultErr error) {
	page.Items = []Record{}
	if r == nil || r.source == nil {
		return page, errors.New("log reader is not initialized")
	}
	if query.Limit <= 0 || query.Limit > MaximumLimit {
		return page, errors.New("log query limit is invalid")
	}
	snapshot, err := r.source.OpenSnapshot()
	if err != nil {
		return page, err
	}
	defer func() {
		if closeErr := snapshot.Close(); resultErr == nil && closeErr != nil {
			resultErr = closeErr
		}
	}()

	streams := make([]*recordStream, 0, 2)
	for _, item := range []struct {
		file ReadFile
		size int64
	}{{snapshot.Active, snapshot.ActiveSize}, {snapshot.Archive, snapshot.ArchiveSize}} {
		if item.file == nil {
			continue
		}
		stream, err := newRecordStream(item.file, item.size)
		if err != nil {
			return page, err
		}
		if err := stream.advance(ctx, &page.SkippedMalformed); err != nil {
			return page, err
		}
		streams = append(streams, stream)
	}

	for {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		selected := newestStream(streams)
		if selected == nil {
			break
		}
		record := selected.current
		if query.Cursor != nil && comparePosition(record.Time, record.Seq, query.Cursor.Time, query.Cursor.Seq) >= 0 {
			if err := selected.advance(ctx, &page.SkippedMalformed); err != nil {
				return page, err
			}
			continue
		}
		if !query.Filter.Match(record) {
			if err := selected.advance(ctx, &page.SkippedMalformed); err != nil {
				return page, err
			}
			continue
		}
		if len(page.Items) == query.Limit {
			page.HasMore = true
			last := page.Items[len(page.Items)-1]
			page.NextCursor = EncodeCursor(last.Time, last.Seq)
			break
		}
		summary, err := Summarize(record)
		if err != nil {
			// The line parsed but could not be summarized, so it cannot be
			// represented within the response bound. Account for it the same way
			// as an unparseable line instead of failing the whole request.
			page.SkippedMalformed++
			if err := selected.advance(ctx, &page.SkippedMalformed); err != nil {
				return page, err
			}
			continue
		}
		page.Items = append(page.Items, summary)
		if err := selected.advance(ctx, &page.SkippedMalformed); err != nil {
			return page, err
		}
	}
	return page, nil
}

// ErrRecordNotFound reports that no persisted record carries the reference. The
// record was rotated away, which is an ordinary outcome rather than a failure.
var ErrRecordNotFound = errors.New("log record not found")

// Record returns the complete persisted record named by a reference. Both
// generations are scanned in full: event records carry the time the event
// occurred rather than the time it was written, so file order cannot be relied
// on to stop early without risking an intermittent miss.
func (r *Reader) Record(ctx context.Context, reference Cursor) (record Record, resultErr error) {
	if r == nil || r.source == nil {
		return Record{}, errors.New("log reader is not initialized")
	}
	snapshot, err := r.source.OpenSnapshot()
	if err != nil {
		return Record{}, err
	}
	defer func() {
		if closeErr := snapshot.Close(); resultErr == nil && closeErr != nil {
			resultErr = closeErr
		}
	}()

	for _, item := range []struct {
		file ReadFile
		size int64
	}{{snapshot.Active, snapshot.ActiveSize}, {snapshot.Archive, snapshot.ArchiveSize}} {
		if item.file == nil {
			continue
		}
		stream, err := newRecordStream(item.file, item.size)
		if err != nil {
			return Record{}, err
		}
		skipped := 0
		for {
			if err := ctx.Err(); err != nil {
				return Record{}, err
			}
			if err := stream.advance(ctx, &skipped); err != nil {
				return Record{}, err
			}
			if !stream.has {
				break
			}
			if comparePosition(stream.current.Time, stream.current.Seq, reference.Time, reference.Seq) == 0 {
				return stream.current, nil
			}
		}
	}
	return Record{}, ErrRecordNotFound
}

type recordStream struct {
	lines   *reverseLineReader
	current Record
	has     bool
}

func newRecordStream(file ReadFile, size int64) (*recordStream, error) {
	lines, err := newReverseLineReader(file, size)
	if err != nil {
		return nil, err
	}
	return &recordStream{lines: lines}, nil
}

func (s *recordStream) advance(ctx context.Context, skipped *int) error {
	s.has = false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, ok, err := s.lines.Next()
		if err != nil || !ok {
			return err
		}
		record, err := ParseRecord(line)
		if err != nil {
			(*skipped)++
			continue
		}
		s.current = record
		s.has = true
		return nil
	}
}

func newestStream(streams []*recordStream) *recordStream {
	var newest *recordStream
	for _, stream := range streams {
		if stream == nil || !stream.has {
			continue
		}
		if newest == nil || comparePosition(stream.current.Time, stream.current.Seq, newest.current.Time, newest.current.Seq) > 0 {
			newest = stream
		}
	}
	return newest
}

// reverseLineReader walks a byte range backwards one line at a time while
// retaining the block it last read. Successive lines that share a block are
// served from memory, so scanning a range costs one read of that range rather
// than one block read per line.
type reverseLineReader struct {
	file ReadFile
	// end is the exclusive upper bound of the bytes still to be scanned; the
	// next line reported ends here.
	end int64
	// buf holds the file bytes at [start, start+len(buf)) and always covers
	// end, so the unscanned window is buf[:end-start].
	start        int64
	buf          []byte
	pendingEmpty bool
}

func newReverseLineReader(file ReadFile, size int64) (*reverseLineReader, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("log path is not a regular file")
	}
	if size < 0 || size > info.Size() {
		return nil, errors.New("log snapshot size is invalid")
	}
	reader := &reverseLineReader{file: file}
	end := size
	if end > 0 {
		last := []byte{0}
		if err := readAtFull(file, last, end-1); err != nil {
			return nil, err
		}
		if last[0] == '\n' {
			end--
			reader.pendingEmpty = end == 0
		}
	}
	reader.end = end
	reader.start = end
	return reader, nil
}

func (r *reverseLineReader) Next() ([]byte, bool, error) {
	if r.pendingEmpty {
		r.pendingEmpty = false
		return []byte{}, true, nil
	}
	for {
		if r.end == 0 {
			return nil, false, nil
		}
		window := r.buf[:r.end-r.start]
		if index := bytes.LastIndexByte(window, '\n'); index >= 0 {
			line := append([]byte(nil), window[index+1:]...)
			r.end = r.start + int64(index)
			return line, true, nil
		}
		if r.start == 0 {
			line := append([]byte(nil), window...)
			r.end = 0
			return line, true, nil
		}
		if err := r.extend(); err != nil {
			return nil, false, err
		}
	}
}

// extend drops the already-scanned tail and prepends more file bytes. The read
// size doubles while a single line keeps growing, so assembling one oversized
// record stays linear in its length instead of quadratic in its block count.
func (r *reverseLineReader) extend() error {
	retained := r.end - r.start
	r.buf = r.buf[:retained]
	want := reverseReadChunk
	if retained > want {
		want = retained
	}
	if want > r.start {
		want = r.start
	}
	chunk := make([]byte, want+retained)
	if err := readAtFull(r.file, chunk[:want], r.start-want); err != nil {
		return err
	}
	copy(chunk[want:], r.buf)
	r.buf = chunk
	r.start -= want
	return nil
}

func readAtFull(file io.ReaderAt, destination []byte, offset int64) error {
	n, err := file.ReadAt(destination, offset)
	if n == len(destination) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return err
}

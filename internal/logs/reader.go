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
		page.Items = append(page.Items, record)
		if err := selected.advance(ctx, &page.SkippedMalformed); err != nil {
			return page, err
		}
	}
	return page, nil
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

type reverseLineReader struct {
	file         ReadFile
	end          int64
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
	reader := &reverseLineReader{file: file, end: size}
	if reader.end > 0 {
		last := []byte{0}
		if err := readAtFull(file, last, reader.end-1); err != nil {
			return nil, err
		}
		if last[0] == '\n' {
			reader.end--
			reader.pendingEmpty = reader.end == 0
		}
	}
	return reader, nil
}

func (r *reverseLineReader) Next() ([]byte, bool, error) {
	if r.pendingEmpty {
		r.pendingEmpty = false
		return []byte{}, true, nil
	}
	if r.end == 0 {
		return nil, false, nil
	}
	searchEnd := r.end
	parts := make([][]byte, 0, 1)
	total := 0
	for searchEnd > 0 {
		start := searchEnd - reverseReadChunk
		if start < 0 {
			start = 0
		}
		chunk := make([]byte, searchEnd-start)
		if err := readAtFull(r.file, chunk, start); err != nil {
			return nil, false, err
		}
		if index := bytes.LastIndexByte(chunk, '\n'); index >= 0 {
			part := append([]byte(nil), chunk[index+1:]...)
			parts = append(parts, part)
			total += len(part)
			r.end = start + int64(index)
			return joinReverseParts(parts, total), true, nil
		}
		part := append([]byte(nil), chunk...)
		parts = append(parts, part)
		total += len(part)
		searchEnd = start
	}
	r.end = 0
	return joinReverseParts(parts, total), true, nil
}

func joinReverseParts(parts [][]byte, total int) []byte {
	line := make([]byte, 0, total)
	for i := len(parts) - 1; i >= 0; i-- {
		line = append(line, parts[i]...)
	}
	return line
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

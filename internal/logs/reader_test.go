package logs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func logLine(timestamp time.Time, seq uint64, fields string) string {
	if fields != "" {
		fields = "," + fields
	}
	return fmt.Sprintf(`{"time":%q,"level":"INFO","msg":"gateway","seq":%d%s}`, timestamp.Format(time.RFC3339Nano), seq, fields)
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func directoryReader(t *testing.T, dir string) *Reader {
	t.Helper()
	source, err := NewDirectorySource(dir)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(source)
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

func itemSeqs(items []Record) []uint64 {
	result := make([]uint64, len(items))
	for i := range items {
		result[i] = items[i].Seq
	}
	return result
}

func TestReaderPagesAcrossActiveAndArchive(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	writeLines(t, filepath.Join(dir, ArchiveFileName),
		logLine(base.Add(time.Second), 1, `"kind":"success"`),
		logLine(base.Add(2*time.Second), 2, `"kind":"success"`),
		logLine(base.Add(3*time.Second), 3, `"kind":"success"`),
	)
	// Equal timestamps intentionally exercise the seq tiebreak.
	writeLines(t, filepath.Join(dir, ActiveFileName),
		logLine(base.Add(4*time.Second), 4, `"kind":"success"`),
		logLine(base.Add(5*time.Second), 5, `"kind":"success"`),
		logLine(base.Add(5*time.Second), 6, `"kind":"success"`),
	)
	reader := directoryReader(t, dir)

	first, err := reader.Query(context.Background(), HistoryQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := itemSeqs(first.Items); !reflect.DeepEqual(got, []uint64{6, 5}) || !first.HasMore || first.NextCursor == "" || first.SkippedMalformed != 0 {
		t.Fatalf("first page = seq %v, %#v", got, first)
	}
	cursor, err := DecodeCursor(first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	second, err := reader.Query(context.Background(), HistoryQuery{Limit: 2, Cursor: &cursor})
	if err != nil {
		t.Fatal(err)
	}
	if got := itemSeqs(second.Items); !reflect.DeepEqual(got, []uint64{4, 3}) || !second.HasMore {
		t.Fatalf("second page = seq %v, %#v", got, second)
	}
	cursor, _ = DecodeCursor(second.NextCursor)
	third, err := reader.Query(context.Background(), HistoryQuery{Limit: 2, Cursor: &cursor})
	if err != nil {
		t.Fatal(err)
	}
	if got := itemSeqs(third.Items); !reflect.DeepEqual(got, []uint64{2, 1}) || third.HasMore || third.NextCursor != "" {
		t.Fatalf("third page = seq %v, %#v", got, third)
	}
}

func TestReaderFilteringMalformedLinesAndLargeRecords(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	largeError, _ := json.Marshal(strings.Repeat("x", int(reverseReadChunk)+100))
	content := strings.Join([]string{
		logLine(base, 1, `"kind":"success","provider_id":"a","http_status":200`),
		"not-json",
		logLine(base.Add(time.Second), 2, `"kind":"failure","provider_id":"a","http_status":503,"raw_error":`+string(largeError)),
		logLine(base.Add(2*time.Second), 3, `"kind":"failure","provider_id":"b","http_status":503`),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, ActiveFileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	parameters, err := ParseParameters(map[string][]string{
		"kind":        {"failure"},
		"provider_id": {"a"},
		"http_status": {"503"},
	}, ParseOptions{History: true})
	if err != nil {
		t.Fatal(err)
	}
	page, err := directoryReader(t, dir).Query(context.Background(), parameters.HistoryQuery())
	if err != nil {
		t.Fatal(err)
	}
	if got := itemSeqs(page.Items); !reflect.DeepEqual(got, []uint64{2}) || page.SkippedMalformed != 1 {
		t.Fatalf("filtered page = seq %v, %#v", got, page)
	}
	if !strings.Contains(string(page.Items[0].Bytes()), strings.Repeat("x", 100)) {
		t.Fatal("large record was truncated")
	}
}

func TestReaderTimeEndpointsAndStaleCursor(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	writeLines(t, filepath.Join(dir, ActiveFileName),
		logLine(base, 1, `"kind":"success"`),
		logLine(base.Add(time.Second), 2, `"kind":"success"`),
		logLine(base.Add(2*time.Second), 3, `"kind":"success"`),
	)
	parameters, err := ParseParameters(map[string][]string{
		"since": {base.Add(time.Second).Format(time.RFC3339Nano)},
		"until": {base.Add(2 * time.Second).Format(time.RFC3339Nano)},
	}, ParseOptions{History: true})
	if err != nil {
		t.Fatal(err)
	}
	page, err := directoryReader(t, dir).Query(context.Background(), parameters.HistoryQuery())
	if err != nil || !reflect.DeepEqual(itemSeqs(page.Items), []uint64{3, 2}) {
		t.Fatalf("inclusive time page = %v, %v", itemSeqs(page.Items), err)
	}

	newerMissing := Cursor{Time: base.Add(10 * time.Second), Seq: 999}
	page, err = directoryReader(t, dir).Query(context.Background(), HistoryQuery{Limit: 10, Cursor: &newerMissing})
	if err != nil || !reflect.DeepEqual(itemSeqs(page.Items), []uint64{3, 2, 1}) {
		t.Fatalf("newer stale cursor = %v, %v", itemSeqs(page.Items), err)
	}
	olderMissing := Cursor{Time: base.Add(-time.Second), Seq: 0}
	page, err = directoryReader(t, dir).Query(context.Background(), HistoryQuery{Limit: 10, Cursor: &olderMissing})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("older stale cursor = %#v, %v", page, err)
	}
}

func TestReaderMergesGenerationsByTimeAndSequence(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	writeLines(t, filepath.Join(dir, ArchiveFileName),
		logLine(base.Add(time.Second), 1, `"kind":"success"`),
		logLine(base.Add(4*time.Second), 4, `"kind":"success"`),
	)
	writeLines(t, filepath.Join(dir, ActiveFileName),
		logLine(base.Add(2*time.Second), 2, `"kind":"success"`),
		logLine(base.Add(3*time.Second), 3, `"kind":"success"`),
	)
	page, err := directoryReader(t, dir).Query(context.Background(), HistoryQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := itemSeqs(page.Items); !reflect.DeepEqual(got, []uint64{4, 3, 2, 1}) {
		t.Fatalf("merged order = %v", got)
	}
}

func TestReaderMissingFilesAndReadFailures(t *testing.T) {
	page, err := directoryReader(t, t.TempDir()).Query(context.Background(), HistoryQuery{Limit: DefaultLimit})
	if err != nil || page.Items == nil || len(page.Items) != 0 || page.HasMore || page.SkippedMalformed != 0 {
		t.Fatalf("missing files page = %#v, %v", page, err)
	}

	failingReader, err := NewReader(SnapshotSourceFunc(func() (Snapshot, error) {
		return Snapshot{}, errors.New("open failed")
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failingReader.Query(context.Background(), HistoryQuery{Limit: 1}); err == nil {
		t.Fatal("snapshot open failure was ignored")
	}

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ActiveFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := directoryReader(t, dir).Query(context.Background(), HistoryQuery{Limit: 1}); err == nil {
		t.Fatal("non-regular active path was accepted")
	}
}

// countingFile reports how much the reader pulled from disk so the retained
// block stays observable. Re-reading a block per line would make both counters
// grow with the number of records instead of the scanned byte range.
type countingFile struct {
	ReadFile
	reads int
	bytes int64
}

func (f *countingFile) ReadAt(p []byte, offset int64) (int, error) {
	n, err := f.ReadFile.ReadAt(p, offset)
	f.reads++
	f.bytes += int64(n)
	return n, err
}

func TestReaderServesLinesSharingABlockFromMemory(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	const records = 400
	lines := make([]string, 0, records)
	for i := 0; i < records; i++ {
		lines = append(lines, logLine(base.Add(time.Duration(i)*time.Second), uint64(i+1), `"kind":"success"`))
	}
	path := filepath.Join(dir, ActiveFileName)
	writeLines(t, path, lines...)
	size := fileSize(t, path)
	if size >= reverseReadChunk {
		t.Fatalf("fixture must fit one block: %d bytes", size)
	}

	counting := openCountingFile(t, path)
	page, err := readerForFile(t, counting, size).Query(context.Background(), HistoryQuery{Limit: records})
	if err != nil || len(page.Items) != records {
		t.Fatalf("page = %d items, %v", len(page.Items), err)
	}
	// One terminator probe plus one block read covers the whole fixture.
	if counting.reads > 2 {
		t.Fatalf("reading %d records issued %d reads", records, counting.reads)
	}
	if counting.bytes > size+1 {
		t.Fatalf("read %d bytes for a %d byte range", counting.bytes, size)
	}
}

func TestReaderAssemblesOversizedRecordWithoutRereading(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	oversized, err := json.Marshal(strings.Repeat("y", int(reverseReadChunk)*8))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ActiveFileName)
	writeLines(t, path,
		logLine(base, 1, `"kind":"success"`),
		logLine(base.Add(time.Second), 2, `"kind":"failure","raw_error":`+string(oversized)),
		logLine(base.Add(2*time.Second), 3, `"kind":"success"`),
	)
	size := fileSize(t, path)

	counting := openCountingFile(t, path)
	reader := readerForFile(t, counting, size)
	page, err := reader.Query(context.Background(), HistoryQuery{Limit: 10})
	if err != nil || !reflect.DeepEqual(itemSeqs(page.Items), []uint64{3, 2, 1}) {
		t.Fatalf("page = %v, %v", itemSeqs(page.Items), err)
	}
	if !strings.Contains(string(page.Items[1].Bytes()), strings.Repeat("y", int(reverseReadChunk)*8)) {
		t.Fatal("oversized record was truncated")
	}
	// The doubling growth keeps total reads logarithmic in the record length,
	// and every byte of the file is pulled at most twice.
	if counting.reads > 8 {
		t.Fatalf("assembling one oversized record issued %d reads", counting.reads)
	}
	if counting.bytes > 2*size {
		t.Fatalf("read %d bytes for a %d byte file", counting.bytes, size)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func openCountingFile(t *testing.T, path string) *countingFile {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return &countingFile{ReadFile: file}
}

func readerForFile(t *testing.T, file ReadFile, size int64) *Reader {
	t.Helper()
	reader, err := NewReader(SnapshotSourceFunc(func() (Snapshot, error) {
		return Snapshot{Active: nopCloseFile{file}, ActiveSize: size}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

// nopCloseFile keeps the counting handle usable after Query closes its snapshot.
type nopCloseFile struct{ ReadFile }

func (nopCloseFile) Close() error { return nil }

func TestReaderSnapshotSurvivesConcurrentNamespaceRotation(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	activePath := filepath.Join(dir, ActiveFileName)
	archivePath := filepath.Join(dir, ArchiveFileName)
	writeLines(t, archivePath, logLine(base, 1, `"kind":"success"`), logLine(base.Add(time.Second), 2, `"kind":"success"`))
	writeLines(t, activePath, logLine(base.Add(2*time.Second), 3, `"kind":"success"`), logLine(base.Add(3*time.Second), 4, `"kind":"success"`))

	source := SnapshotSourceFunc(func() (Snapshot, error) {
		directorySource, err := NewDirectorySource(dir)
		if err != nil {
			return Snapshot{}, err
		}
		snapshot, err := directorySource.OpenSnapshot()
		if err != nil {
			return Snapshot{}, err
		}
		if err := os.Rename(activePath, activePath+".bound"); err != nil {
			_ = snapshot.Close()
			return Snapshot{}, err
		}
		if err := os.Rename(archivePath, archivePath+".bound"); err != nil {
			_ = snapshot.Close()
			return Snapshot{}, err
		}
		writeLines(t, archivePath, logLine(base.Add(2*time.Second), 3, `"kind":"success"`), logLine(base.Add(3*time.Second), 4, `"kind":"success"`))
		writeLines(t, activePath, logLine(base.Add(4*time.Second), 5, `"kind":"success"`))
		return snapshot, nil
	})
	reader, err := NewReader(source)
	if err != nil {
		t.Fatal(err)
	}
	page, err := reader.Query(context.Background(), HistoryQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := itemSeqs(page.Items); !reflect.DeepEqual(got, []uint64{4, 3, 2, 1}) {
		t.Fatalf("bound snapshot records = %v", got)
	}
}

func TestReaderSnapshotExcludesWritesAfterOpen(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	activePath := filepath.Join(dir, ActiveFileName)
	writeLines(t, activePath, logLine(base, 1, `"kind":"success"`))
	source := SnapshotSourceFunc(func() (Snapshot, error) {
		file, err := os.Open(activePath)
		if err != nil {
			return Snapshot{}, err
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return Snapshot{}, err
		}
		writer, err := os.OpenFile(activePath, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			_ = file.Close()
			return Snapshot{}, err
		}
		_, writeErr := writer.WriteString(logLine(base.Add(time.Second), 2, `"kind":"success"`) + "\n")
		closeErr := writer.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			_ = file.Close()
			return Snapshot{}, err
		}
		return Snapshot{Active: file, ActiveSize: info.Size()}, nil
	})
	reader, err := NewReader(source)
	if err != nil {
		t.Fatal(err)
	}
	page, err := reader.Query(context.Background(), HistoryQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := itemSeqs(page.Items); !reflect.DeepEqual(got, []uint64{1}) {
		t.Fatalf("snapshot included a later write: %v", got)
	}
}

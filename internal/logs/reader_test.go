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

func TestReaderSnapshotSurvivesConcurrentNamespaceRotation(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	activePath := filepath.Join(dir, ActiveFileName)
	archivePath := filepath.Join(dir, ArchiveFileName)
	writeLines(t, archivePath, logLine(base, 1, `"kind":"success"`), logLine(base.Add(time.Second), 2, `"kind":"success"`))
	writeLines(t, activePath, logLine(base.Add(2*time.Second), 3, `"kind":"success"`), logLine(base.Add(3*time.Second), 4, `"kind":"success"`))

	source := SnapshotSourceFunc(func() (Snapshot, error) {
		active, err := os.Open(activePath)
		if err != nil {
			return Snapshot{}, err
		}
		archive, err := os.Open(archivePath)
		if err != nil {
			_ = active.Close()
			return Snapshot{}, err
		}
		activeInfo, err := active.Stat()
		if err != nil {
			_ = active.Close()
			_ = archive.Close()
			return Snapshot{}, err
		}
		archiveInfo, err := archive.Stat()
		if err != nil {
			_ = active.Close()
			_ = archive.Close()
			return Snapshot{}, err
		}
		if err := os.Rename(activePath, activePath+".bound"); err != nil {
			_ = active.Close()
			_ = archive.Close()
			return Snapshot{}, err
		}
		if err := os.Rename(archivePath, archivePath+".bound"); err != nil {
			_ = active.Close()
			_ = archive.Close()
			return Snapshot{}, err
		}
		writeLines(t, archivePath, logLine(base.Add(2*time.Second), 3, `"kind":"success"`), logLine(base.Add(3*time.Second), 4, `"kind":"success"`))
		writeLines(t, activePath, logLine(base.Add(4*time.Second), 5, `"kind":"success"`))
		return Snapshot{Active: active, ActiveSize: activeInfo.Size(), Archive: archive, ArchiveSize: archiveInfo.Size()}, nil
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

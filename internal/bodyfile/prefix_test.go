package bodyfile

import (
	"strings"
	"testing"
)

func TestRawMarkerScanTracksLiteralBytesAcrossChunks(t *testing.T) {
	marker := "security-monitor"
	spec, err := RequestScanSpecWithRawMarkers([]string{"/system"}, marker)
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := NewJSONScanner(spec)
	if err != nil {
		t.Fatal(err)
	}
	input := []byte(`{"model":"m","note":"security-monitor","system":[]}`)
	for _, chunk := range [][]byte{input[:17], input[17:29], input[29:]} {
		if _, err := scanner.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	_, err = scanner.Finish()
	if err != nil {
		t.Fatal(err)
	}
	body, index, err := CaptureAndScan(strings.NewReader(string(input)), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if found, tracked := index.RawMarkerStatus(marker); !tracked || !found || !index.RawMarkerFound(marker) {
		t.Fatalf("raw marker status = found:%v tracked:%v", found, tracked)
	}
	copyMap := index.RawMarkers()
	copyMap[marker] = false
	if !index.RawMarkerFound(marker) {
		t.Fatal("RawMarkers leaked mutable state")
	}
}

func TestRawMarkerScanDoesNotDecodeEscapes(t *testing.T) {
	marker := "security-monitor"
	spec, err := RequestScanSpecWithRawMarkers(nil, marker)
	if err != nil {
		t.Fatal(err)
	}
	body, index, err := CaptureAndScan(strings.NewReader(`{"model":"m","text":"security-\u006donitor"}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if found, tracked := index.RawMarkerStatus(marker); !tracked || found {
		t.Fatalf("escaped marker status = found:%v tracked:%v", found, tracked)
	}
}

func TestReadJSONStringPrefixDecodesBoundedEscapes(t *testing.T) {
	input := `{"model":"m","text":"hello \u4e16\u754c"}`
	spec, err := RequestScanSpec("/text")
	if err != nil {
		t.Fatal(err)
	}
	body, index, err := CaptureAndScan(strings.NewReader(input), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	field, ok := index.Lookup("/text")
	if !ok {
		t.Fatal("text field missing")
	}
	if got, complete, err := ReadJSONStringPrefix(body, field, 6); err != nil || got != "hello " || complete {
		t.Fatalf("prefix(6) = %q complete:%v err:%v", got, complete, err)
	}
	if got, complete, err := ReadJSONStringPrefix(body, field, 7); err != nil || got != "hello " || complete {
		t.Fatalf("prefix(7) = %q complete:%v err:%v", got, complete, err)
	}
	if got, complete, err := ReadJSONStringPrefix(body, field, 32); err != nil || got != "hello 世界" || !complete {
		t.Fatalf("prefix(full) = %q complete:%v err:%v", got, complete, err)
	}
}

func TestReadJSONStringPrefixHandlesSurrogatePairAndLargeString(t *testing.T) {
	input := `{"model":"m","text":"\ud83d\ude00` + strings.Repeat("x", 2<<20) + `"}`
	spec, err := RequestScanSpec("/text")
	if err != nil {
		t.Fatal(err)
	}
	body, index, err := CaptureAndScan(strings.NewReader(input), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	field, _ := index.Lookup("/text")
	got, complete, err := ReadJSONStringPrefix(body, field, 4)
	if err != nil || got != "😀" || complete {
		t.Fatalf("large prefix = %q complete:%v err:%v", got, complete, err)
	}
}

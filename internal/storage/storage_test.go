package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWriteJSONAtomicAndReadJSONRoundTrip(t *testing.T) {
	root := t.TempDir()
	payload := map[string]string{"status": "ok"}

	if err := WriteJSONAtomic(root, "state.json", payload); err != nil {
		t.Fatalf("WriteJSONAtomic() error = %v", err)
	}

	var got map[string]string
	if err := ReadJSON(filepath.Join(root, "state.json"), &got); err != nil {
		t.Fatalf("ReadJSON() error = %v", err)
	}
	if got["status"] != "ok" {
		t.Fatalf("ReadJSON() got %q, want ok", got["status"])
	}
}

func TestWriteBodyAtomicPreservesBytesAndRenamesFile(t *testing.T) {
	root := t.TempDir()
	data := []byte{0, 1, 2, 255, 10}

	n, err := WriteBodyAtomic(root, "payload.bin", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("WriteBodyAtomic() error = %v", err)
	}
	if n != int64(len(data)) {
		t.Fatalf("WriteBodyAtomic() wrote %d bytes, want %d", n, len(data))
	}

	b, err := os.ReadFile(filepath.Join(root, "payload.bin"))
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if !bytes.Equal(b, data) {
		t.Fatalf("payload mismatch: got %v, want %v", b, data)
	}
}

func TestWriteBodyAtomicReplacesExistingFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "payload.bin")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	if _, err := WriteBodyAtomic(root, "payload.bin", bytes.NewReader([]byte("new"))); err != nil {
		t.Fatalf("WriteBodyAtomic() error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if string(got) != "new" {
		t.Fatalf("payload = %q, want %q", got, "new")
	}
}

func TestWriteBodyAtomicCleansUpAfterReaderFailure(t *testing.T) {
	root := t.TempDir()
	reader := errorReader{}

	if _, err := WriteBodyAtomic(root, "payload.bin", reader); !errors.Is(err, errReader) {
		t.Fatalf("WriteBodyAtomic() error = %v, want %v", err, errReader)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("os.ReadDir() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files remain after failed write: %v", entries)
	}
}

func TestWriteBodyAtomicCreatesNestedDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "nested", "queue")

	if _, err := WriteBodyAtomic(dir, "payload.bin", bytes.NewReader([]byte("nested"))); err != nil {
		t.Fatalf("WriteBodyAtomic() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "payload.bin"))
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if string(got) != "nested" {
		t.Fatalf("payload = %q, want %q", got, "nested")
	}
}

func TestWriteBodyAtomicWritesDistinctFilesConcurrently(t *testing.T) {
	root := t.TempDir()
	const writes = 20
	var wg sync.WaitGroup
	errs := make(chan error, writes)
	for i := 0; i < writes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := filepath.Join(root, "payload-"+string(rune('a'+i))+".bin")
			_, err := WriteBodyAtomic(filepath.Dir(name), filepath.Base(name), bytes.NewReader([]byte{byte(i)}))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent WriteBodyAtomic() error = %v", err)
		}
	}
	for i := 0; i < writes; i++ {
		name := filepath.Join(root, "payload-"+string(rune('a'+i))+".bin")
		got, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("os.ReadFile(%q) error = %v", name, err)
		}
		if !bytes.Equal(got, []byte{byte(i)}) {
			t.Fatalf("payload %q = %v, want %v", name, got, []byte{byte(i)})
		}
	}
}

var errReader = errors.New("reader failed")

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errReader
}

func TestSafeNameRejectsIllegalCharacters(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{name: "valid", in: "client-01", want: true},
		{name: "dot", in: "service.v1", want: true},
		{name: "space", in: "bad name", want: false},
		{name: "slash", in: "bad/name", want: false},
		{name: "double-dot", in: "bad..name", want: false},
		{name: "current-directory", in: ".", want: false},
		{name: "parent-directory", in: "..", want: false},
		{name: "empty", in: "", want: false},
	}
	for _, tc := range cases {
		if got := SafeName(tc.in); got != tc.want {
			t.Fatalf("SafeName(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNewIDProducesHexStringLength(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatalf("NewID() error = %v", err)
	}
	if len(id) != 32 {
		t.Fatalf("NewID() length = %d, want 32", len(id))
	}
	if _, err := json.Marshal(id); err != nil {
		t.Fatalf("json.Marshal(%q) error = %v", id, err)
	}
}

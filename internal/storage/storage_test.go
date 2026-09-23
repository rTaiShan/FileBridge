package storage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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

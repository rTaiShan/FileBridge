package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filebridge/internal/storage"
)

func TestPersistentIDPersistsAndReusesValue(t *testing.T) {
	state := t.TempDir()
	id1, err := persistentID(state)
	if err != nil {
		t.Fatalf("persistentID() first call error = %v", err)
	}
	if !storage.SafeName(id1) {
		t.Fatalf("persistentID() returned unsafe id %q", id1)
	}

	content, err := os.ReadFile(filepath.Join(state, "client-id"))
	if err != nil {
		t.Fatalf("read persisted client ID: %v", err)
	}
	if !strings.Contains(string(content), id1) {
		t.Fatalf("persisted file %q does not contain %q", string(content), id1)
	}

	id2, err := persistentID(state)
	if err != nil {
		t.Fatalf("persistentID() second call error = %v", err)
	}
	if id2 != id1 {
		t.Fatalf("persistentID() second call = %q, want %q", id2, id1)
	}
}

func TestPersistentIDRegeneratesInvalidStoredValue(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(state, 0o750); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(state, "client-id"), []byte("bad/id\n"), 0o600); err != nil {
		t.Fatalf("write invalid id: %v", err)
	}

	id, err := persistentID(state)
	if err != nil {
		t.Fatalf("persistentID() error = %v", err)
	}
	if !storage.SafeName(id) {
		t.Fatalf("persistentID() returned unsafe value %q", id)
	}
}

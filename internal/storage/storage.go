package storage

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func RequestDir(root, clientID string) string {
	return filepath.Join(root, "server", "inbox", clientID)
}
func ResponseDir(root, clientID string) string {
	return filepath.Join(root, "clients", clientID, "inbox")
}
func RegistryPath(root string) string  { return filepath.Join(root, "registry", "services.json") }
func HeartbeatPath(root string) string { return filepath.Join(root, "server", "heartbeat.json") }

func RequestMeta(id string) string   { return id + ".request.json" }
func RequestBody(id string) string   { return id + ".request.body" }
func RequestCancel(id string) string { return id + ".request.cancelled" }
func ResponseMeta(id string) string  { return id + ".response.json" }
func ResponseBody(id string) string  { return id + ".response.body" }

// WriteJSONAtomic makes the final name visible only after complete JSON has
// been written. Metadata is deliberately the publish marker for each message.
func WriteJSONAtomic(dir, name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return writeAtomic(dir, name, strings.NewReader(string(b)))
}

func WriteBodyAtomic(dir, name string, r io.Reader) (int64, error) {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	n, copyErr := io.Copy(tmp, r)
	if copyErr == nil {
		copyErr = tmp.Sync()
	}
	closeErr := tmp.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(tmpName)
		return n, copyErr
	}
	destination := filepath.Join(dir, name)
	var renameErr error
	for range 10 {
		if renameErr = os.Rename(tmpName, destination); renameErr == nil {
			return n, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = os.Remove(tmpName)
	return n, renameErr
}

func writeAtomic(dir, name string, r io.Reader) error {
	_, err := WriteBodyAtomic(dir, name, r)
	return err
}

func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func NewID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate request ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func SafeName(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return !strings.Contains(s, "..")
}

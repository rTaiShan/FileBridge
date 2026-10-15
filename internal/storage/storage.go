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

// ServerRequestDir is the v2, server-scoped request queue. A request is
// published to exactly one server, so another server can never claim it.
func ServerRequestDir(root, serverID, clientID string) string {
	return filepath.Join(root, "servers", serverID, "inbox", clientID)
}

// ServerResponseDir is the v2, server-scoped response queue. Keeping
// responses under the selected server also prevents response-file collisions.
func ServerResponseDir(root, serverID, clientID string) string {
	return filepath.Join(root, "clients", clientID, "responses", serverID)
}

func ServerRegistryPath(root, serverID string) string {
	return filepath.Join(root, "servers", serverID, "registry.json")
}

func ServerHeartbeatPath(root, serverID string) string {
	return filepath.Join(root, "servers", serverID, "heartbeat.json")
}

// The default-server helpers keep internal callers that do not need multiple
// servers concise. New transport code should use the server-scoped helpers.
func RequestDir(root, clientID string) string  { return ServerRequestDir(root, "default", clientID) }
func ResponseDir(root, clientID string) string { return ServerResponseDir(root, "default", clientID) }
func RegistryPath(root string) string          { return ServerRegistryPath(root, "default") }
func HeartbeatPath(root string) string         { return ServerHeartbeatPath(root, "default") }

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
		if renameErr = replaceFile(tmpName, destination); renameErr == nil {
			return n, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = os.Remove(tmpName)
	return n, renameErr
}

func replaceFile(source, destination string) error {
	// os.Rename uses the platform replacement primitive. On Windows this
	// preserves the destination while retrying transient sharing violations.
	return os.Rename(source, destination)
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
	if s == "" || s == "." || s == ".." || len(s) > 100 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return !strings.Contains(s, "..")
}

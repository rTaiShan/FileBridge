package bridge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filebridge/internal/config"
	"filebridge/internal/protocol"
	"filebridge/internal/storage"
)

func TestServerRejectsInvalidRequestMetadata(t *testing.T) {
	root := t.TempDir()
	server := NewServer(root, config.Server{Services: map[string]config.Service{"reports": {Target: "http://127.0.0.1:8000"}}})

	clientID := "alice"
	requestID := "req-invalid"
	requestDir := filepath.Join(root, "server", "inbox", clientID)
	if err := os.MkdirAll(requestDir, 0o750); err != nil {
		t.Fatalf("mkdir request dir: %v", err)
	}
	bodyPath := filepath.Join(requestDir, storage.RequestBody(requestID))
	if err := os.WriteFile(bodyPath, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write body: %v", err)
	}
	meta := protocol.Request{
		ID:        "different-id",
		ClientID:  clientID,
		Service:   "reports",
		Method:    http.MethodGet,
		Path:      "/status",
		Body:      protocol.Body{File: storage.RequestBody(requestID), Size: 5},
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := storage.WriteJSONAtomic(requestDir, storage.RequestMeta(requestID), meta); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	responseDir := storage.ResponseDir(root, clientID)
	if err := os.MkdirAll(responseDir, 0o750); err != nil {
		t.Fatalf("mkdir response dir: %v", err)
	}

	server.handle(context.Background(), clientID, requestID, requestDir)

	var resp protocol.Response
	if err := storage.ReadJSON(filepath.Join(responseDir, storage.ResponseMeta(requestID)), &resp); err != nil {
		t.Fatalf("read bridge response: %v", err)
	}
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.Status, http.StatusBadRequest)
	}
	if resp.Error == nil || resp.Error.Code != "invalid_request" {
		t.Fatalf("error = %+v, want invalid_request", resp.Error)
	}
}

func TestServerRejectsExpiredRequestBeforeForwarding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be called when deadline is already expired")
	}))
	defer upstream.Close()

	root := t.TempDir()
	server := NewServer(root, config.Server{Services: map[string]config.Service{"reports": {Target: upstream.URL}}})

	clientID := "alice"
	requestID := "req-expired"
	requestDir := filepath.Join(root, "server", "inbox", clientID)
	if err := os.MkdirAll(requestDir, 0o750); err != nil {
		t.Fatalf("mkdir request dir: %v", err)
	}
	bodyPath := filepath.Join(requestDir, storage.RequestBody(requestID))
	if err := os.WriteFile(bodyPath, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write body: %v", err)
	}
	meta := protocol.Request{
		ID:        requestID,
		ClientID:  clientID,
		Service:   "reports",
		Method:    http.MethodGet,
		Path:      "/status",
		Query:     "mode=full",
		Body:      protocol.Body{File: storage.RequestBody(requestID), Size: 5},
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Deadline:  time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano),
	}
	if err := storage.WriteJSONAtomic(requestDir, storage.RequestMeta(requestID), meta); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	responseDir := storage.ResponseDir(root, clientID)
	if err := os.MkdirAll(responseDir, 0o750); err != nil {
		t.Fatalf("mkdir response dir: %v", err)
	}

	server.handle(context.Background(), clientID, requestID, requestDir)

	var resp protocol.Response
	if err := storage.ReadJSON(filepath.Join(responseDir, storage.ResponseMeta(requestID)), &resp); err != nil {
		t.Fatalf("read bridge response: %v", err)
	}
	if resp.Status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d", resp.Status, http.StatusGatewayTimeout)
	}
	if resp.Error == nil || resp.Error.Code != "deadline_exceeded" {
		t.Fatalf("error = %+v, want deadline_exceeded", resp.Error)
	}
}

func TestServerReturnsBridgeErrorForUnknownService(t *testing.T) {
	root := t.TempDir()
	server := NewServer(root, config.Server{Services: map[string]config.Service{"reports": {Target: "http://127.0.0.1:8000"}}})

	clientID := "alice"
	requestID := "req-missing"
	requestDir := filepath.Join(root, "server", "inbox", clientID)
	if err := os.MkdirAll(requestDir, 0o750); err != nil {
		t.Fatalf("mkdir request dir: %v", err)
	}
	bodyPath := filepath.Join(requestDir, storage.RequestBody(requestID))
	if err := os.WriteFile(bodyPath, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write body: %v", err)
	}
	meta := protocol.Request{
		ID:        requestID,
		ClientID:  clientID,
		Service:   "missing-service",
		Method:    http.MethodGet,
		Path:      "/status",
		Body:      protocol.Body{File: storage.RequestBody(requestID), Size: 5},
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := storage.WriteJSONAtomic(requestDir, storage.RequestMeta(requestID), meta); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	responseDir := storage.ResponseDir(root, clientID)
	if err := os.MkdirAll(responseDir, 0o750); err != nil {
		t.Fatalf("mkdir response dir: %v", err)
	}

	server.handle(context.Background(), clientID, requestID, requestDir)

	var resp protocol.Response
	if err := storage.ReadJSON(filepath.Join(responseDir, storage.ResponseMeta(requestID)), &resp); err != nil {
		t.Fatalf("read bridge response: %v", err)
	}
	if resp.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.Status, http.StatusNotFound)
	}
	if resp.Error == nil || resp.Error.Code != "service_not_found" {
		t.Fatalf("error = %+v, want service_not_found", resp.Error)
	}
}

func TestClientWaitForResponseTimesOut(t *testing.T) {
	root := t.TempDir()
	client, err := NewClient(root, "alice")
	if err != nil {
		t.Fatalf("NewClient() error: %v", err)
	}
	client.PollInterval = 5 * time.Millisecond

	_, _, err = client.waitForResponse(context.Background(), "no-response-yet", time.Now().Add(50*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForResponse error = %v, want DeadlineExceeded", err)
	}
}

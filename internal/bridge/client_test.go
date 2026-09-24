package bridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filebridge/internal/protocol"
	"filebridge/internal/storage"
)

func TestBridgeStatusAndHealthReflectHeartbeat(t *testing.T) {
	root := t.TempDir()
	client, err := NewClient(root, "alice-laptop")
	if err != nil {
		t.Fatalf("NewClient() error: %v", err)
	}
	client.registry = protocol.Registry{Services: map[string]protocol.RegistryService{"reports": {Description: "Reporting API"}}}

	if err := storage.WriteJSONAtomic(filepath.Dir(storage.HeartbeatPath(root)), filepath.Base(storage.HeartbeatPath(root)), protocol.Heartbeat{UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), Version: protocol.Version}); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	statusReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000/_bridge/status", nil)
	statusRes := httptest.NewRecorder()
	client.handleBridge(statusRes, statusReq)
	if statusRes.Code != http.StatusOK {
		t.Fatalf("/_bridge/status code = %d, want %d", statusRes.Code, http.StatusOK)
	}
	var status map[string]any
	if err := json.Unmarshal(statusRes.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if got := status["server_alive"]; got != true {
		t.Fatalf("status[server_alive] = %v, want true", got)
	}
	if got := status["service_count"]; got != float64(1) {
		t.Fatalf("status[service_count] = %v, want 1", got)
	}

	healthRes := httptest.NewRecorder()
	client.handleBridge(healthRes, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000/_bridge/health", nil))
	if healthRes.Code != http.StatusOK {
		t.Fatalf("/_bridge/health code = %d, want %d", healthRes.Code, http.StatusOK)
	}

	stale := protocol.Heartbeat{UpdatedAt: time.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339Nano), Version: protocol.Version}
	if err := storage.WriteJSONAtomic(filepath.Dir(storage.HeartbeatPath(root)), filepath.Base(storage.HeartbeatPath(root)), stale); err != nil {
		t.Fatalf("write stale heartbeat: %v", err)
	}
	client.HeartbeatMaxAge = time.Second

	staleHealth := httptest.NewRecorder()
	client.handleBridge(staleHealth, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000/_bridge/health", nil))
	if staleHealth.Code != http.StatusBadGateway {
		t.Fatalf("/_bridge/health with stale heartbeat code = %d, want %d", staleHealth.Code, http.StatusBadGateway)
	}
}

func TestProxyRejectsUnknownServiceAndUnsafePath(t *testing.T) {
	root := t.TempDir()
	client, err := NewClient(root, "alice-laptop")
	if err != nil {
		t.Fatalf("NewClient() error: %v", err)
	}
	client.registry = protocol.Registry{Services: map[string]protocol.RegistryService{"reports": {Description: "Reporting API"}}}
	if err := storage.WriteJSONAtomic(filepath.Dir(storage.RegistryPath(root)), filepath.Base(storage.RegistryPath(root)), client.registry); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	if err := storage.WriteJSONAtomic(filepath.Dir(storage.HeartbeatPath(root)), filepath.Base(storage.HeartbeatPath(root)), protocol.Heartbeat{UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), Version: protocol.Version}); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	missingSvcRes := httptest.NewRecorder()
	client.handleProxy(missingSvcRes, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000/does-not-exist/status", nil))
	if missingSvcRes.Code != http.StatusNotFound {
		t.Fatalf("unknown service code = %d, want %d", missingSvcRes.Code, http.StatusNotFound)
	}

	unsafeReqRes := httptest.NewRecorder()
	client.handleProxy(unsafeReqRes, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000/bad%2Fservice", nil))
	if unsafeReqRes.Code != http.StatusNotFound {
		t.Fatalf("unsafe path code = %d, want %d", unsafeReqRes.Code, http.StatusNotFound)
	}
}

func TestSanitizeHeadersStripsHopByHopValues(t *testing.T) {
	headers := http.Header{
		"Connection":          {"keep-alive, Upgrade"},
		"Upgrade":             {"websocket"},
		"X-Test":              {"preserved"},
		"Proxy-Authorization": {"token"},
	}

	filtered := sanitizeHeaders(headers)
	if got := filtered.Values("Connection"); len(got) != 0 {
		t.Fatalf("Connection header remained: %v", got)
	}
	if got := filtered.Get("Upgrade"); got != "" {
		t.Fatalf("Upgrade header remained: %q", got)
	}
	if got := filtered.Get("X-Test"); got != "preserved" {
		t.Fatalf("X-Test header = %q, want preserved", got)
	}
	if got := filtered.Get("Proxy-Authorization"); got != "" {
		t.Fatalf("Proxy-Authorization header remained: %q", got)
	}
}

func TestSplitServicePathRejectsUnsafeServiceNames(t *testing.T) {
	if _, _, err := splitServicePath("/reports/status"); err != nil {
		t.Fatalf("valid service path returned error: %v", err)
	}
	if _, _, err := splitServicePath("/bad%2Fservice"); err == nil || !strings.Contains(err.Error(), "invalid service name") {
		t.Fatalf("unsafe service path error = %v, want invalid service name", err)
	}
}

func TestMarkCancelledWritesRequestMarker(t *testing.T) {
	client, err := NewClient(t.TempDir(), "alice")
	if err != nil {
		t.Fatalf("NewClient() error: %v", err)
	}
	if err := client.markCancelled("request-1"); err != nil {
		t.Fatalf("markCancelled() error: %v", err)
	}
	path := filepath.Join(storage.RequestDir(client.Root, client.ID), storage.RequestCancel("request-1"))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cancellation marker missing: %v", err)
	}
}

func TestCleanupStaleRemovesOldRequestAndResponseFiles(t *testing.T) {
	client, err := NewClient(t.TempDir(), "alice")
	if err != nil {
		t.Fatalf("NewClient() error: %v", err)
	}
	client.Timeout = 10 * time.Millisecond
	requestID := "old-request"
	requestDir := storage.RequestDir(client.Root, client.ID)
	responseDir := storage.ResponseDir(client.Root, client.ID)
	for _, path := range []string{
		filepath.Join(requestDir, storage.RequestMeta(requestID)),
		filepath.Join(requestDir, storage.RequestBody(requestID)),
		filepath.Join(requestDir, storage.RequestCancel(requestID)),
		filepath.Join(responseDir, storage.ResponseMeta(requestID)),
		filepath.Join(responseDir, storage.ResponseBody(requestID)),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	old := time.Now().Add(-time.Second)
	if err := os.Chtimes(filepath.Join(requestDir, storage.RequestMeta(requestID)), old, old); err != nil {
		t.Fatalf("age request metadata: %v", err)
	}
	if err := os.Chtimes(filepath.Join(responseDir, storage.ResponseMeta(requestID)), old, old); err != nil {
		t.Fatalf("age response metadata: %v", err)
	}

	client.cleanupStale()
	for _, path := range []string{
		filepath.Join(requestDir, storage.RequestMeta(requestID)),
		filepath.Join(requestDir, storage.RequestBody(requestID)),
		filepath.Join(requestDir, storage.RequestCancel(requestID)),
		filepath.Join(responseDir, storage.ResponseMeta(requestID)),
		filepath.Join(responseDir, storage.ResponseBody(requestID)),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale file %s still exists, stat error = %v", path, err)
		}
	}

	for _, path := range []string{
		filepath.Join(requestDir, storage.RequestBody("orphan-request")),
		filepath.Join(responseDir, storage.ResponseBody("orphan-response")),
	} {
		if err := os.WriteFile(path, []byte("orphan"), 0o600); err != nil {
			t.Fatalf("write orphan %s: %v", path, err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("age orphan %s: %v", path, err)
		}
	}
	client.cleanupStale()
	for _, path := range []string{
		filepath.Join(requestDir, storage.RequestBody("orphan-request")),
		filepath.Join(responseDir, storage.ResponseBody("orphan-response")),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("orphan file %s still exists, stat error = %v", path, err)
		}
	}
}

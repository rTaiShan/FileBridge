package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"filebridge/internal/protocol"
	"filebridge/internal/storage"
)

type Client struct {
	Root            string
	ID              string
	Timeout         time.Duration
	PollInterval    time.Duration
	HeartbeatMaxAge time.Duration

	mu       sync.RWMutex
	registry protocol.Registry
}

func NewClient(root, id string) (*Client, error) {
	if !storage.SafeName(id) {
		return nil, fmt.Errorf("invalid client ID")
	}
	return &Client{Root: root, ID: id, Timeout: 15 * time.Second, PollInterval: 500 * time.Millisecond, HeartbeatMaxAge: 3 * time.Second, registry: protocol.Registry{Services: map[string]protocol.RegistryService{}}}, nil
}

// RunRefresher keeps registry data fresh even when no request is in progress.
func (c *Client) RunRefresher(ctx context.Context) {
	if c.PollInterval <= 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	c.RefreshRegistry()
	c.cleanupStale()
	t := time.NewTicker(c.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.RefreshRegistry()
			c.cleanupStale()
		}
	}
}

func (c *Client) RefreshRegistry() error {
	var registry protocol.Registry
	if err := storage.ReadJSON(storage.RegistryPath(c.Root), &registry); err != nil {
		return err
	}
	if registry.Services == nil {
		registry.Services = map[string]protocol.RegistryService{}
	}
	c.mu.Lock()
	c.registry = registry
	c.mu.Unlock()
	return nil
}

func (c *Client) registrySnapshot() protocol.Registry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.registry
}

func (c *Client) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_bridge/") {
			c.handleBridge(w, r)
			return
		}
		c.handleProxy(w, r)
	})
}

func (c *Client) handleBridge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/_bridge/version":
		writeJSON(w, http.StatusOK, map[string]string{"version": protocol.Version})
	case "/_bridge/services":
		writeJSON(w, http.StatusOK, c.registrySnapshot())
	case "/_bridge/status":
		status := map[string]any{"client_id": c.ID, "server_alive": c.serverAlive(), "service_count": len(c.registrySnapshot().Services)}
		writeJSON(w, http.StatusOK, status)
	case "/_bridge/health":
		if !c.serverAlive() {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "server heartbeat is stale or unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	default:
		http.NotFound(w, r)
	}
}

func (c *Client) handleProxy(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		http.Error(w, "service name is required", http.StatusNotFound)
		return
	}
	if err := c.RefreshRegistry(); err != nil && len(c.registrySnapshot().Services) == 0 {
		http.Error(w, "service registry unavailable", http.StatusBadGateway)
		return
	}
	service, remotePath, err := splitServicePath(r.URL.EscapedPath())
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if _, ok := c.registrySnapshot().Services[service]; !ok {
		http.Error(w, "service not found", http.StatusNotFound)
		return
	}
	if !c.serverAlive() {
		http.Error(w, "server bridge is unavailable", http.StatusBadGateway)
		return
	}

	id, err := storage.NewID()
	if err != nil {
		http.Error(w, "could not create bridge request", http.StatusInternalServerError)
		return
	}
	dir := storage.RequestDir(c.Root, c.ID)
	n, err := storage.WriteBodyAtomic(dir, storage.RequestBody(id), r.Body)
	if err != nil {
		http.Error(w, "could not publish request body", http.StatusBadGateway)
		return
	}
	deadline := time.Now().Add(c.Timeout).UTC()
	req := protocol.Request{ID: id, ClientID: c.ID, Service: service, Method: r.Method, Path: remotePath, Query: r.URL.RawQuery, Headers: sanitizeHeaders(r.Header), Body: protocol.Body{File: storage.RequestBody(id), Size: n}, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Deadline: deadline.Format(time.RFC3339Nano)}
	if err := storage.WriteJSONAtomic(dir, storage.RequestMeta(id), req); err != nil {
		_ = os.Remove(filepath.Join(dir, storage.RequestBody(id)))
		http.Error(w, "could not publish request", http.StatusBadGateway)
		return
	}

	resp, body, err := c.waitForResponse(r.Context(), id, deadline)
	if err != nil {
		if err := c.markCancelled(id); err != nil {
			log.Printf("FileBridge: mark request %s cancelled: %v", id, err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "bridge request timed out", http.StatusGatewayTimeout)
		} else {
			http.Error(w, "bridge response unavailable", http.StatusBadGateway)
		}
		return
	}
	c.copyResponse(w, resp, body)
	if err := body.Close(); err != nil {
		c.cleanup(id)
		return
	}
	c.cleanup(id)
}

func (c *Client) waitForResponse(requestContext context.Context, id string, deadline time.Time) (protocol.Response, io.ReadCloser, error) {
	poll := c.PollInterval
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	timeout := time.NewTimer(time.Until(deadline))
	defer timeout.Stop()
	path := filepath.Join(storage.ResponseDir(c.Root, c.ID), storage.ResponseMeta(id))
	for {
		var response protocol.Response
		if err := storage.ReadJSON(path, &response); err == nil {
			if response.ID != id || response.ReplyTo != id {
				return protocol.Response{}, nil, fmt.Errorf("invalid response metadata")
			}
			if response.Error != nil {
				return response, io.NopCloser(strings.NewReader("")), nil
			}
			if response.Body.File != storage.ResponseBody(id) {
				return protocol.Response{}, nil, fmt.Errorf("invalid response body metadata")
			}
			body, err := os.Open(filepath.Join(storage.ResponseDir(c.Root, c.ID), response.Body.File))
			if err == nil {
				return response, body, nil
			}
		}
		select {
		case <-requestContext.Done():
			return protocol.Response{}, nil, requestContext.Err()
		case <-timeout.C:
			return protocol.Response{}, nil, context.DeadlineExceeded
		case <-t.C:
		}
	}
}

func (c *Client) copyResponse(w http.ResponseWriter, response protocol.Response, body io.Reader) {
	if response.Error != nil {
		writeJSON(w, response.Status, map[string]any{"error": response.Error.Code, "message": response.Error.Message})
		return
	}
	for key, values := range sanitizeHeaders(response.Headers) {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.Status)
	_, _ = io.Copy(w, body)
}

func (c *Client) cleanup(id string) {
	for _, path := range []string{
		filepath.Join(storage.RequestDir(c.Root, c.ID), storage.RequestMeta(id)), filepath.Join(storage.RequestDir(c.Root, c.ID), storage.RequestBody(id)),
		filepath.Join(storage.RequestDir(c.Root, c.ID), storage.RequestCancel(id)), filepath.Join(storage.ResponseDir(c.Root, c.ID), storage.ResponseMeta(id)), filepath.Join(storage.ResponseDir(c.Root, c.ID), storage.ResponseBody(id)),
	} {
		_ = os.Remove(path)
	}
}

func (c *Client) markCancelled(id string) error {
	dir := storage.RequestDir(c.Root, c.ID)
	_, err := storage.WriteBodyAtomic(dir, storage.RequestCancel(id), strings.NewReader("cancelled\n"))
	return err
}

func (c *Client) cleanupStale() {
	maxAge := 2 * c.Timeout
	if maxAge <= 0 {
		maxAge = time.Minute
	}
	cutoff := time.Now().Add(-maxAge)
	for _, item := range []struct {
		dir      string
		metadata string
		body     string
		cancel   string
	}{
		{storage.RequestDir(c.Root, c.ID), ".request.json", ".request.body", ".request.cancelled"},
		{storage.ResponseDir(c.Root, c.ID), ".response.json", ".response.body", ""},
	} {
		entries, err := os.ReadDir(item.dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), item.metadata) {
				continue
			}
			info, err := entry.Info()
			if err != nil || info.ModTime().After(cutoff) {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), item.metadata)
			if !storage.SafeName(id) {
				continue
			}
			_ = os.Remove(filepath.Join(item.dir, entry.Name()))
			if item.body != "" {
				_ = os.Remove(filepath.Join(item.dir, id+item.body))
			}
			if item.cancel != "" {
				_ = os.Remove(filepath.Join(item.dir, id+item.cancel))
			}
		}
	}
	c.cleanupOrphanFiles(storage.RequestDir(c.Root, c.ID), ".request.body", storage.RequestMeta)
	c.cleanupOrphanFiles(storage.RequestDir(c.Root, c.ID), ".request.cancelled", storage.RequestMeta)
	c.cleanupOrphanFiles(storage.ResponseDir(c.Root, c.ID), ".response.body", storage.ResponseMeta)
}

func (c *Client) cleanupOrphanFiles(dir, suffix string, metadata func(string) string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-2 * c.Timeout)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), suffix)
		if !storage.SafeName(id) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, metadata(id))); err == nil {
			continue
		}
		info, err := entry.Info()
		if err == nil && !info.ModTime().After(cutoff) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

func (c *Client) serverAlive() bool {
	var heartbeat protocol.Heartbeat
	if storage.ReadJSON(storage.HeartbeatPath(c.Root), &heartbeat) != nil {
		return false
	}
	updated, err := time.Parse(time.RFC3339Nano, heartbeat.UpdatedAt)
	if err != nil {
		return false
	}
	maxAge := c.HeartbeatMaxAge
	if maxAge <= 0 {
		maxAge = 3 * c.PollInterval
	}
	return time.Since(updated) <= maxAge
}

func splitServicePath(escapedPath string) (string, string, error) {
	trimmed := strings.TrimPrefix(escapedPath, "/")
	parts := strings.SplitN(trimmed, "/", 2)
	if parts[0] == "" {
		return "", "", fmt.Errorf("service name is required")
	}
	service, err := url.PathUnescape(parts[0])
	if err != nil || !storage.SafeName(service) {
		return "", "", fmt.Errorf("invalid service name")
	}
	remote := "/"
	if len(parts) == 2 {
		remote += parts[1]
	}
	return service, remote, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

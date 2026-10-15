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
	"sort"
	"strings"
	"sync"
	"time"

	"filebridge/internal/protocol"
	"filebridge/internal/storage"
	"filebridge/internal/webui"
)

const explicitProxyPrefix = "/_bridge/proxy/"
const activityLimit = 50

type Client struct {
	Root            string
	ID              string
	Timeout         time.Duration
	PollInterval    time.Duration
	HeartbeatMaxAge time.Duration

	mu             sync.RWMutex
	registry       protocol.Registry
	allowedServers map[string]struct{}
	lastRegistryAt string
	activityMu     sync.RWMutex
	activity       []requestActivity
}

type requestActivity struct {
	ID            string `json:"id"`
	StartedAt     string `json:"started_at"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	ServerID      string `json:"server_id"`
	Service       string `json:"service"`
	Status        int    `json:"status,omitempty"`
	DurationMS    int64  `json:"duration_ms"`
	RequestBytes  int64  `json:"request_bytes"`
	ResponseBytes int64  `json:"response_bytes"`
	Outcome       string `json:"outcome"`
}

func NewClient(root, id string) (*Client, error) {
	if !storage.SafeName(id) {
		return nil, fmt.Errorf("invalid client ID")
	}
	return &Client{
		Root: root, ID: id, Timeout: 15 * time.Second, PollInterval: 5 * time.Second, HeartbeatMaxAge: 25 * time.Second,
		registry: protocol.Registry{Version: protocol.RegistryVersion, Servers: map[string]protocol.ServerRegistry{}},
	}, nil
}

// SetAllowedServers restricts discovery to explicit, trusted server IDs. An
// empty list deliberately means discovery is unrestricted for backwards-simple
// deployments; production deployments should provide this allowlist.
func (c *Client) SetAllowedServers(ids []string) error {
	allowed := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if !storage.SafeName(id) {
			return fmt.Errorf("invalid allowed server ID %q", id)
		}
		allowed[id] = struct{}{}
	}
	c.mu.Lock()
	c.allowedServers = allowed
	c.mu.Unlock()
	return nil
}

func (c *Client) serverAllowed(id string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.allowedServers) == 0 || hasServer(c.allowedServers, id)
}

func hasServer(servers map[string]struct{}, id string) bool {
	_, ok := servers[id]
	return ok
}

// RunRefresher keeps registry data fresh even when no request is in progress.
func (c *Client) RunRefresher(ctx context.Context) {
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
	_ = c.RefreshRegistry()
	c.cleanupStale()
	t := time.NewTicker(c.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = c.RefreshRegistry()
			c.cleanupStale()
		}
	}
}

// RefreshRegistry reads one independently-published registry per server. It
// never writes or merges a shared registry, avoiding multi-writer races.
func (c *Client) RefreshRegistry() error {
	entries, err := os.ReadDir(filepath.Join(c.Root, "servers"))
	if err != nil {
		return err
	}
	servers := make(map[string]protocol.ServerRegistry)
	var firstErr error
	for _, entry := range entries {
		id := entry.Name()
		if !entry.IsDir() || !storage.SafeName(id) || !c.serverAllowed(id) {
			continue
		}
		var advertised protocol.ServerRegistry
		if err := storage.ReadJSON(storage.ServerRegistryPath(c.Root, id), &advertised); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := validAdvertisement(id, advertised); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		servers[id] = advertised
	}
	if len(servers) == 0 && firstErr != nil {
		return fmt.Errorf("no valid server registry: %w", firstErr)
	}
	if len(servers) == 0 {
		return fmt.Errorf("no server registries found")
	}
	c.mu.Lock()
	c.registry = protocol.Registry{Version: protocol.RegistryVersion, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), Servers: servers}
	c.lastRegistryAt = c.registry.UpdatedAt
	c.mu.Unlock()
	return nil
}

func validAdvertisement(directoryID string, registry protocol.ServerRegistry) error {
	if registry.Version != protocol.RegistryVersion || registry.ServerID != directoryID || !storage.SafeName(registry.ServerID) || !storage.SafeName(registry.InstanceID) || len(registry.Services) == 0 {
		return fmt.Errorf("invalid server registry for %q", directoryID)
	}
	if _, err := time.Parse(time.RFC3339Nano, registry.UpdatedAt); err != nil {
		return fmt.Errorf("invalid server registry timestamp for %q", directoryID)
	}
	for name := range registry.Services {
		if !storage.SafeName(name) {
			return fmt.Errorf("invalid service name in registry for %q", directoryID)
		}
	}
	return nil
}

func (c *Client) registrySnapshot() protocol.Registry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.registry
}

func (c *Client) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/_bridge/ui/", http.StatusTemporaryRedirect)
			return
		}
		if r.URL.Path == "/_bridge/ui" {
			http.Redirect(w, r, "/_bridge/ui/", http.StatusTemporaryRedirect)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/_bridge/ui/") {
			webui.Handler(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, explicitProxyPrefix) {
			c.handleExplicitProxy(w, r)
			return
		}
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
	registry := c.registrySnapshot()
	switch r.URL.Path {
	case "/_bridge/version":
		writeJSON(w, http.StatusOK, map[string]string{"version": protocol.Version})
	case "/_bridge/services":
		writeJSON(w, http.StatusOK, registry)
	case "/_bridge/activity":
		writeJSON(w, http.StatusOK, c.activitySnapshot())
	case "/_bridge/status":
		live, services := c.registryHealth(registry)
		writeJSON(w, http.StatusOK, map[string]any{"client_id": c.ID, "version": protocol.Version, "checked_at": time.Now().UTC().Format(time.RFC3339Nano), "registry_refreshed_at": c.lastRegistryTimestamp(), "server_alive": live > 0, "server_count": len(registry.Servers), "live_server_count": live, "service_count": services, "servers": c.serverStatuses(registry)})
	case "/_bridge/health":
		live, _ := c.registryHealth(registry)
		if live == 0 {
			writeJSON(w, http.StatusBadGateway, map[string]any{"status": "degraded", "checked_at": time.Now().UTC().Format(time.RFC3339Nano), "server_count": len(registry.Servers), "live_server_count": live, "error": "no server heartbeat is fresh"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "checked_at": time.Now().UTC().Format(time.RFC3339Nano), "server_count": len(registry.Servers), "live_server_count": live})
	default:
		http.NotFound(w, r)
	}
}

func (c *Client) lastRegistryTimestamp() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastRegistryAt
}

func (c *Client) activitySnapshot() []requestActivity {
	c.activityMu.RLock()
	defer c.activityMu.RUnlock()
	activity := make([]requestActivity, len(c.activity))
	copy(activity, c.activity)
	return activity
}

func (c *Client) recordActivity(activity requestActivity) {
	c.activityMu.Lock()
	defer c.activityMu.Unlock()
	activity.DurationMS = time.Since(parseActivityTime(activity.StartedAt)).Milliseconds()
	c.activity = append([]requestActivity{activity}, c.activity...)
	if len(c.activity) > activityLimit {
		c.activity = c.activity[:activityLimit]
	}
}

func parseActivityTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Now()
	}
	return parsed
}

// serverStatus is a read-only dashboard summary. It never exposes upstream
// target addresses, which remain private to the server process.
type serverStatus struct {
	ServerID   string `json:"server_id"`
	InstanceID string `json:"instance_id"`
	UpdatedAt  string `json:"updated_at"`
	Alive      bool   `json:"alive"`
}

func (c *Client) serverStatuses(registry protocol.Registry) []serverStatus {
	servers := make([]serverStatus, 0, len(registry.Servers))
	for _, server := range registry.Servers {
		servers = append(servers, serverStatus{ServerID: server.ServerID, InstanceID: server.InstanceID, UpdatedAt: server.UpdatedAt, Alive: c.serverAlive(server.ServerID, server.InstanceID)})
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].ServerID < servers[j].ServerID })
	return servers
}

func (c *Client) registryHealth(registry protocol.Registry) (live, services int) {
	for _, server := range registry.Servers {
		services += len(server.Services)
		if c.serverAlive(server.ServerID, server.InstanceID) {
			live++
		}
	}
	return live, services
}

func (c *Client) handleExplicitProxy(w http.ResponseWriter, r *http.Request) {
	serverID, service, remotePath, err := splitServerServicePath(r.URL.EscapedPath())
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	c.proxy(w, r, serverID, service, remotePath, true)
}

func (c *Client) handleProxy(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		http.Error(w, "service name is required", http.StatusNotFound)
		return
	}
	service, remotePath, err := splitServicePath(r.URL.EscapedPath())
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	c.proxy(w, r, "", service, remotePath, false)
}

func (c *Client) proxy(w http.ResponseWriter, r *http.Request, serverID, service, remotePath string, explicit bool) {
	if err := c.RefreshRegistry(); err != nil && len(c.registrySnapshot().Servers) == 0 {
		http.Error(w, "service registry unavailable", http.StatusBadGateway)
		return
	}
	registry := c.registrySnapshot()
	server, err := resolveServer(registry, serverID, service, explicit)
	if err != nil {
		status := http.StatusNotFound
		if errors.Is(err, errAmbiguousService) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	if !c.serverAlive(server.ServerID, server.InstanceID) {
		http.Error(w, "selected server bridge is unavailable", http.StatusBadGateway)
		return
	}

	id, err := storage.NewID()
	if err != nil {
		http.Error(w, "could not create bridge request", http.StatusInternalServerError)
		return
	}
	dir := storage.ServerRequestDir(c.Root, server.ServerID, c.ID)
	n, err := storage.WriteBodyAtomic(dir, storage.RequestBody(id), r.Body)
	if err != nil {
		http.Error(w, "could not publish request body", http.StatusBadGateway)
		return
	}
	deadline := time.Now().Add(c.Timeout).UTC()
	req := protocol.Request{ID: id, ClientID: c.ID, ServerID: server.ServerID, ServerInstanceID: server.InstanceID, Service: service, Method: r.Method, Path: remotePath, Query: r.URL.RawQuery, Headers: sanitizeHeaders(r.Header), Body: protocol.Body{File: storage.RequestBody(id), Size: n}, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Deadline: deadline.Format(time.RFC3339Nano)}
	if err := storage.WriteJSONAtomic(dir, storage.RequestMeta(id), req); err != nil {
		_ = os.Remove(filepath.Join(dir, storage.RequestBody(id)))
		http.Error(w, "could not publish request", http.StatusBadGateway)
		return
	}
	activity := requestActivity{ID: id, StartedAt: req.CreatedAt, Method: r.Method, Path: remotePath, ServerID: server.ServerID, Service: service, RequestBytes: n, Outcome: "pending"}
	defer func() {
		if activity.Outcome == "pending" {
			activity.Outcome = "failed"
		}
		c.recordActivity(activity)
	}()

	resp, body, err := c.waitForServerResponse(r.Context(), server.ServerID, server.InstanceID, id, deadline)
	if err != nil {
		if err := c.markCancelled(server.ServerID, id); err != nil {
			log.Printf("FileBridge: mark request %s/%s cancelled: %v", server.ServerID, id, err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			activity.Status = http.StatusGatewayTimeout
			http.Error(w, "bridge request timed out", http.StatusGatewayTimeout)
		} else {
			activity.Status = http.StatusBadGateway
			http.Error(w, "bridge response unavailable", http.StatusBadGateway)
		}
		return
	}
	activity.Status = resp.Status
	activity.ResponseBytes = resp.Body.Size
	activity.Outcome = "completed"
	c.copyResponse(w, resp, body)
	if err := body.Close(); err != nil {
		c.cleanup(server.ServerID, id)
		return
	}
	c.cleanup(server.ServerID, id)
}

var errAmbiguousService = errors.New("service is advertised by multiple servers; use /_bridge/proxy/<server-id>/<service>/...")

func resolveServer(registry protocol.Registry, requestedID, service string, explicit bool) (protocol.ServerRegistry, error) {
	if explicit {
		server, ok := registry.Servers[requestedID]
		if !ok || !hasService(server, service) {
			return protocol.ServerRegistry{}, fmt.Errorf("service not found")
		}
		return server, nil
	}
	var matched protocol.ServerRegistry
	for _, server := range registry.Servers {
		if !hasService(server, service) {
			continue
		}
		if matched.ServerID != "" {
			return protocol.ServerRegistry{}, errAmbiguousService
		}
		matched = server
	}
	if matched.ServerID == "" {
		return protocol.ServerRegistry{}, fmt.Errorf("service not found")
	}
	return matched, nil
}

func hasService(server protocol.ServerRegistry, service string) bool {
	_, ok := server.Services[service]
	return ok
}

func (c *Client) waitForServerResponse(requestContext context.Context, serverID, instanceID, id string, deadline time.Time) (protocol.Response, io.ReadCloser, error) {
	poll := c.PollInterval
	if poll <= 0 {
		poll = 5 * time.Second
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	timeout := time.NewTimer(time.Until(deadline))
	defer timeout.Stop()
	dir := storage.ServerResponseDir(c.Root, serverID, c.ID)
	path := filepath.Join(dir, storage.ResponseMeta(id))
	for {
		var response protocol.Response
		if err := storage.ReadJSON(path, &response); err == nil {
			if response.ID != id || response.ReplyTo != id || response.ServerID != serverID || response.ServerInstanceID != instanceID {
				return protocol.Response{}, nil, fmt.Errorf("invalid response metadata")
			}
			if response.Error != nil {
				return response, io.NopCloser(strings.NewReader("")), nil
			}
			if response.Body.File != storage.ResponseBody(id) {
				return protocol.Response{}, nil, fmt.Errorf("invalid response body metadata")
			}
			body, err := os.Open(filepath.Join(dir, response.Body.File))
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

func (c *Client) cleanup(serverID, id string) {
	for _, path := range []string{
		filepath.Join(storage.ServerRequestDir(c.Root, serverID, c.ID), storage.RequestMeta(id)), filepath.Join(storage.ServerRequestDir(c.Root, serverID, c.ID), storage.RequestBody(id)),
		filepath.Join(storage.ServerRequestDir(c.Root, serverID, c.ID), storage.RequestCancel(id)), filepath.Join(storage.ServerResponseDir(c.Root, serverID, c.ID), storage.ResponseMeta(id)), filepath.Join(storage.ServerResponseDir(c.Root, serverID, c.ID), storage.ResponseBody(id)),
	} {
		_ = os.Remove(path)
	}
}

func (c *Client) markCancelled(serverAndID ...string) error {
	serverID, id := "default", ""
	if len(serverAndID) == 1 {
		id = serverAndID[0]
	} else if len(serverAndID) == 2 {
		serverID, id = serverAndID[0], serverAndID[1]
	} else {
		return fmt.Errorf("expected request ID or server ID and request ID")
	}
	dir := storage.ServerRequestDir(c.Root, serverID, c.ID)
	_, err := storage.WriteBodyAtomic(dir, storage.RequestCancel(id), strings.NewReader("cancelled\n"))
	return err
}

func (c *Client) cleanupStale() {
	entries, err := os.ReadDir(filepath.Join(c.Root, "servers"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !storage.SafeName(entry.Name()) {
			continue
		}
		serverID := entry.Name()
		c.cleanupStaleDir(storage.ServerRequestDir(c.Root, serverID, c.ID), ".request.json", ".request.body", ".request.cancelled")
		c.cleanupStaleDir(storage.ServerResponseDir(c.Root, serverID, c.ID), ".response.json", ".response.body", "")
	}
}

func (c *Client) cleanupStaleDir(dir, metadata, body, cancel string) {
	maxAge := 2 * c.Timeout
	if maxAge <= 0 {
		maxAge = time.Minute
	}
	cutoff := time.Now().Add(-maxAge)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), metadata) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), metadata)
		if !storage.SafeName(id) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
		if body != "" {
			_ = os.Remove(filepath.Join(dir, id+body))
		}
		if cancel != "" {
			_ = os.Remove(filepath.Join(dir, id+cancel))
		}
	}
	c.cleanupOrphanFiles(dir, body, func(id string) string { return id + metadata })
	if cancel != "" {
		c.cleanupOrphanFiles(dir, cancel, func(id string) string { return id + metadata })
	}
}

func (c *Client) cleanupOrphanFiles(dir, suffix string, metadata func(string) string) {
	if suffix == "" {
		return
	}
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

func (c *Client) serverAlive(target ...string) bool {
	serverID, instanceID := "default", ""
	if len(target) == 2 {
		serverID, instanceID = target[0], target[1]
	} else if len(target) != 0 {
		return false
	}
	if instanceID == "" {
		if server, ok := c.registrySnapshot().Servers[serverID]; ok {
			instanceID = server.InstanceID
		}
	}
	var heartbeat protocol.Heartbeat
	if storage.ReadJSON(storage.ServerHeartbeatPath(c.Root, serverID), &heartbeat) != nil || heartbeat.ServerID != serverID || heartbeat.InstanceID != instanceID || heartbeat.Version != protocol.Version {
		return false
	}
	updated, err := time.Parse(time.RFC3339Nano, heartbeat.UpdatedAt)
	if err != nil {
		return false
	}
	maxAge := c.HeartbeatMaxAge
	if maxAge <= 0 {
		maxAge = 5 * c.PollInterval
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

func splitServerServicePath(escapedPath string) (string, string, string, error) {
	trimmed := strings.TrimPrefix(escapedPath, explicitProxyPrefix)
	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("server and service name are required")
	}
	serverID, serverErr := url.PathUnescape(parts[0])
	service, serviceErr := url.PathUnescape(parts[1])
	if serverErr != nil || serviceErr != nil || !storage.SafeName(serverID) || !storage.SafeName(service) {
		return "", "", "", fmt.Errorf("invalid server or service name")
	}
	remote := "/"
	if len(parts) == 3 {
		remote += parts[2]
	}
	return serverID, service, remote, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// waitForResponse is retained for default-server internal callers. New code
// uses waitForServerResponse with the selected server identity.
func (c *Client) waitForResponse(requestContext context.Context, id string, deadline time.Time) (protocol.Response, io.ReadCloser, error) {
	return c.waitForServerResponse(requestContext, "default", "", id, deadline)
}

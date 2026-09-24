package bridge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"filebridge/internal/config"
	"filebridge/internal/protocol"
	"filebridge/internal/storage"
)

type Server struct {
	Root         string
	Config       config.Server
	PollInterval time.Duration
	HTTPClient   *http.Client

	mu       sync.Mutex
	inFlight map[string]struct{}
	logMu    sync.Mutex
	lastLog  map[string]time.Time
}

func NewServer(root string, cfg config.Server) *Server {
	return &Server{Root: root, Config: cfg, PollInterval: 500 * time.Millisecond, HTTPClient: &http.Client{Timeout: 30 * time.Second}, inFlight: map[string]struct{}{}, lastLog: map[string]time.Time{}}
}

func (s *Server) Run(ctx context.Context) error {
	if s.PollInterval <= 0 {
		s.PollInterval = 500 * time.Millisecond
	}
	registryPublished := false
	ticker := time.NewTicker(s.PollInterval)
	defer ticker.Stop()
	for {
		if !registryPublished {
			if err := s.PublishRegistry(); err != nil {
				s.logIssue("registry", "publish registry", err)
			} else {
				registryPublished = true
			}
		}
		if err := s.publishHeartbeat(); err != nil {
			s.logIssue("heartbeat", "publish heartbeat", err)
		}
		if err := s.ScanOnce(ctx); err != nil {
			s.logIssue("scan", "scan inbox", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Server) PublishRegistry() error {
	services := make(map[string]protocol.RegistryService, len(s.Config.Services))
	for name, service := range s.Config.Services {
		services[name] = protocol.RegistryService{Description: service.Description}
	}
	return storage.WriteJSONAtomic(filepath.Dir(storage.RegistryPath(s.Root)), filepath.Base(storage.RegistryPath(s.Root)), protocol.Registry{Version: 1, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), Services: services})
}

func (s *Server) publishHeartbeat() error {
	p := storage.HeartbeatPath(s.Root)
	return storage.WriteJSONAtomic(filepath.Dir(p), filepath.Base(p), protocol.Heartbeat{UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), Version: protocol.Version})
}

func (s *Server) ScanOnce(ctx context.Context) error {
	base := filepath.Join(s.Root, "server", "inbox")
	if err := os.MkdirAll(base, 0750); err != nil {
		return err
	}
	clients, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	for _, client := range clients {
		if !client.IsDir() || !storage.SafeName(client.Name()) {
			continue
		}
		dir := filepath.Join(base, client.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".request.json") || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".request.json")
			if !storage.SafeName(id) {
				continue
			}
			key := client.Name() + "/" + id
			if !s.claim(key) {
				continue
			}
			go func(clientID, requestID, requestDir, claimKey string) {
				defer s.release(claimKey)
				defer func() {
					if recovered := recover(); recovered != nil {
						log.Printf("FileBridge: request %s/%s panicked: %v", clientID, requestID, recovered)
					}
				}()
				s.handle(ctx, clientID, requestID, requestDir)
			}(client.Name(), id, dir, key)
		}
	}
	return nil
}

func (s *Server) claim(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.inFlight[key]; ok {
		return false
	}
	s.inFlight[key] = struct{}{}
	return true
}
func (s *Server) release(key string) { s.mu.Lock(); delete(s.inFlight, key); s.mu.Unlock() }

func (s *Server) logIssue(key, operation string, err error) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.lastLog == nil {
		s.lastLog = map[string]time.Time{}
	}
	if last := s.lastLog[key]; !last.IsZero() && time.Since(last) < 5*time.Second {
		return
	}
	s.lastLog[key] = time.Now()
	log.Printf("FileBridge: %s: %v", operation, err)
}

func (s *Server) handle(parent context.Context, clientID, id, requestDir string) {
	responseDir := storage.ResponseDir(s.Root, clientID)
	defer s.cleanupRequest(requestDir, id)
	if _, err := os.Stat(filepath.Join(responseDir, storage.ResponseMeta(id))); err == nil {
		return
	}
	if s.requestCancelled(requestDir, id) {
		return
	}
	var req protocol.Request
	if err := storage.ReadJSON(filepath.Join(requestDir, storage.RequestMeta(id)), &req); err != nil {
		s.writeBridgeError(responseDir, id, "invalid_request", "invalid request metadata", http.StatusBadRequest)
		return
	}
	if req.ID != id || req.ClientID != clientID || !storage.SafeName(req.ClientID) || req.Method == "" || !strings.HasPrefix(req.Path, "/") || req.Body.File != storage.RequestBody(id) {
		s.writeBridgeError(responseDir, id, "invalid_request", "invalid request metadata", http.StatusBadRequest)
		return
	}
	bodyPath := filepath.Join(requestDir, req.Body.File)
	info, err := os.Stat(bodyPath)
	if os.IsNotExist(err) {
		s.writeBridgeError(responseDir, id, "invalid_request", "request body is missing", http.StatusBadRequest)
		return
	}
	if err != nil {
		return
	}
	if info.Size() != req.Body.Size {
		s.writeBridgeError(responseDir, id, "invalid_request", "request body size does not match metadata", http.StatusBadRequest)
		return
	}
	if s.requestCancelled(requestDir, id) {
		return
	}
	service, ok := s.Config.Services[req.Service]
	if !ok {
		s.writeBridgeError(responseDir, id, "service_not_found", "service is not registered", http.StatusNotFound)
		return
	}
	if req.Deadline != "" {
		if deadline, parseErr := time.Parse(time.RFC3339Nano, req.Deadline); parseErr == nil && time.Now().After(deadline) {
			s.writeBridgeError(responseDir, id, "deadline_exceeded", "request expired before forwarding", http.StatusGatewayTimeout)
			return
		}
	}
	ctx := parent
	if req.Deadline != "" {
		if deadline, err := time.Parse(time.RFC3339Nano, req.Deadline); err == nil {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(parent, deadline)
			defer cancel()
		}
	}
	if err := s.forward(ctx, service.Target, req, bodyPath, responseDir); err != nil {
		code, status := "upstream_unavailable", http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			code, status = "deadline_exceeded", http.StatusGatewayTimeout
		}
		s.writeBridgeError(responseDir, id, code, err.Error(), status)
	}
}

func (s *Server) requestCancelled(requestDir, id string) bool {
	_, err := os.Stat(filepath.Join(requestDir, storage.RequestCancel(id)))
	return err == nil
}

func (s *Server) cleanupRequest(requestDir, id string) {
	for _, name := range []string{storage.RequestMeta(id), storage.RequestBody(id), storage.RequestCancel(id)} {
		_ = os.Remove(filepath.Join(requestDir, name))
	}
}

func (s *Server) forward(ctx context.Context, target string, req protocol.Request, bodyPath, responseDir string) error {
	base, err := url.Parse(target)
	if err != nil {
		return err
	}
	remote, err := url.ParseRequestURI(req.Path)
	if err != nil {
		return fmt.Errorf("invalid remote path: %w", err)
	}
	base.Path = joinURLPath(base.Path, remote.Path)
	base.RawPath = joinURLPath(base.EscapedPath(), remote.EscapedPath())
	base.RawQuery = req.Query
	body, err := os.Open(bodyPath)
	if err != nil {
		return err
	}
	defer body.Close()
	h := sanitizeHeaders(req.Headers)
	hreq, err := http.NewRequestWithContext(ctx, req.Method, base.String(), body)
	if err != nil {
		return err
	}
	hreq.Header = h
	hreq.ContentLength = req.Body.Size
	if req.Body.Size == 0 {
		hreq.Body = http.NoBody
	}
	resp, err := s.HTTPClient.Do(hreq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	n, err := storage.WriteBodyAtomic(responseDir, storage.ResponseBody(req.ID), resp.Body)
	if err != nil {
		return err
	}
	return storage.WriteJSONAtomic(responseDir, storage.ResponseMeta(req.ID), protocol.Response{ID: req.ID, ReplyTo: req.ID, Status: resp.StatusCode, Headers: sanitizeHeaders(resp.Header), Body: protocol.Body{File: storage.ResponseBody(req.ID), Size: n}, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
}

func (s *Server) writeBridgeError(dir, id, code, message string, status int) {
	_ = storage.WriteJSONAtomic(dir, storage.ResponseMeta(id), protocol.Response{ID: id, ReplyTo: id, Status: status, Body: protocol.Body{File: "", Size: 0}, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Error: &protocol.BridgeError{Code: code, Message: message}})
}

func joinURLPath(a, b string) string {
	if a == "" || a == "/" {
		return b
	}
	return strings.TrimRight(a, "/") + "/" + strings.TrimLeft(b, "/")
}

func sanitizeHeaders(in http.Header) http.Header {
	out := in.Clone()
	for _, key := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		out.Del(key)
	}
	for _, value := range in.Values("Connection") {
		for _, key := range strings.Split(value, ",") {
			out.Del(strings.TrimSpace(key))
		}
	}
	return out
}

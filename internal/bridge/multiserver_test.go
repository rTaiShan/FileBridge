package bridge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filebridge/internal/config"
	"filebridge/internal/protocol"
	"filebridge/internal/storage"
)

func TestMultiServerRoutesAreIsolatedAndLegacyRouteIsUnambiguous(t *testing.T) {
	upstream := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, name+":"+r.URL.Path)
		}))
	}
	northUpstream, southUpstream := upstream("north"), upstream("south")
	defer northUpstream.Close()
	defer southUpstream.Close()

	root := t.TempDir()
	north, err := NewServerWithID(root, "north", config.Server{Services: map[string]config.Service{"reports": {Target: northUpstream.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	south, err := NewServerWithID(root, "south", config.Server{Services: map[string]config.Service{"reports": {Target: southUpstream.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	north.PollInterval, south.PollInterval = 5*time.Millisecond, 5*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	var servers sync.WaitGroup
	servers.Add(2)
	go func() { defer servers.Done(); _ = north.Run(ctx) }()
	go func() { defer servers.Done(); _ = south.Run(ctx) }()
	defer func() { cancel(); servers.Wait() }()

	client, err := NewClient(root, "multi-client")
	if err != nil {
		t.Fatal(err)
	}
	client.PollInterval, client.Timeout, client.HeartbeatMaxAge = 5*time.Millisecond, time.Second, time.Second
	waitFor(t, time.Second, func() bool {
		if client.RefreshRegistry() != nil {
			return false
		}
		registry := client.registrySnapshot()
		return len(registry.Servers) == 2 && client.serverAlive("north", north.InstanceID) && client.serverAlive("south", south.InstanceID)
	})

	for _, test := range []struct {
		path string
		want string
	}{
		{"/_bridge/proxy/north/reports/check", "north:/check"},
		{"/_bridge/proxy/south/reports/check", "south:/check"},
	} {
		res := httptest.NewRecorder()
		client.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000"+test.path, nil))
		if res.Code != http.StatusOK || res.Body.String() != test.want {
			t.Fatalf("%s: status/body = %d/%q, want 200/%q", test.path, res.Code, res.Body.String(), test.want)
		}
	}

	ambiguous := httptest.NewRecorder()
	client.Handler().ServeHTTP(ambiguous, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000/reports/check", nil))
	if ambiguous.Code != http.StatusConflict {
		t.Fatalf("legacy duplicate route status = %d, want %d", ambiguous.Code, http.StatusConflict)
	}

	if err := client.SetAllowedServers([]string{"north"}); err != nil {
		t.Fatal(err)
	}
	if err := client.RefreshRegistry(); err != nil {
		t.Fatal(err)
	}
	if got := len(client.registrySnapshot().Servers); got != 1 {
		t.Fatalf("allowed-server registry count = %d, want 1", got)
	}
	blocked := httptest.NewRecorder()
	client.Handler().ServeHTTP(blocked, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000/_bridge/proxy/south/reports/check", nil))
	if blocked.Code != http.StatusNotFound {
		t.Fatalf("untrusted server route status = %d, want %d", blocked.Code, http.StatusNotFound)
	}
}

func waitFor(t *testing.T, timeout time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func TestServerRejectsRequestForPriorInstance(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer upstream.Close()

	root := t.TempDir()
	server, err := NewServerWithID(root, "north", config.Server{Services: map[string]config.Service{"reports": {Target: upstream.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	clientID, requestID := "instance-client", "prior-instance"
	dir := storage.ServerRequestDir(root, server.ID, clientID)
	if _, err := storage.WriteBodyAtomic(dir, storage.RequestBody(requestID), strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	req := protocol.Request{ID: requestID, ClientID: clientID, ServerID: server.ID, ServerInstanceID: "old-instance", Service: "reports", Method: http.MethodGet, Path: "/", Body: protocol.Body{File: storage.RequestBody(requestID)}}
	if err := storage.WriteJSONAtomic(dir, storage.RequestMeta(requestID), req); err != nil {
		t.Fatal(err)
	}
	server.handle(context.Background(), clientID, requestID, dir)
	var response protocol.Response
	if err := storage.ReadJSON(filepath.Join(storage.ServerResponseDir(root, server.ID, clientID), storage.ResponseMeta(requestID)), &response); err != nil {
		t.Fatal(err)
	}
	if called || response.Status != http.StatusServiceUnavailable || response.Error == nil || response.Error.Code != "server_instance_changed" || response.ServerInstanceID != "old-instance" {
		t.Fatalf("called=%v response=%+v, want rejected prior-instance request", called, response)
	}
}

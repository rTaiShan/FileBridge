package bridge

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"filebridge/internal/config"
)

func TestEndToEndHTTPTransport(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/failure" {
			w.Header().Set("X-Remote", "yes")
			http.Error(w, "teapot", http.StatusTeapot)
			return
		}
		w.Header().Set("X-Remote", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	}))
	defer upstream.Close()

	root := t.TempDir()
	server := NewServer(root, config.Server{Services: map[string]config.Service{"reports": {Target: upstream.URL}}})
	server.PollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Run(ctx) }()
	client, err := NewClient(root, "test-client")
	if err != nil {
		t.Fatal(err)
	}
	client.PollInterval, client.Timeout, client.HeartbeatMaxAge = 10*time.Millisecond, 2*time.Second, time.Second
	deadline := time.Now().Add(time.Second)
	for client.RefreshRegistry() != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	for !client.serverAlive() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !client.serverAlive() {
		t.Fatal("server heartbeat not published")
	}

	cases := []struct {
		method, path string
		body         []byte
		want         int
	}{
		{http.MethodGet, "/reports/status?mode=full", nil, http.StatusCreated},
		{http.MethodPost, "/reports/generate", []byte(`{"ok":true}`), http.StatusCreated},
		{http.MethodPost, "/reports/upload", []byte{0, 1, 2, 255}, http.StatusCreated},
		{http.MethodGet, "/reports/failure", nil, http.StatusTeapot},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, "http://127.0.0.1:9000"+tc.path, bytes.NewReader(tc.body))
		r.Header.Set("X-Test", "preserved")
		w := httptest.NewRecorder()
		client.Handler().ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s: status %d, want %d; %q", tc.path, w.Code, tc.want, w.Body.String())
		}
		if w.Header().Get("X-Remote") != "yes" {
			t.Fatalf("%s: remote header missing", tc.path)
		}
		if tc.want != http.StatusTeapot && !bytes.Equal(w.Body.Bytes(), tc.body) {
			t.Fatalf("%s: body changed", tc.path)
		}
	}
}

func TestEndToEndTimeoutCancelsSlowUpstream(t *testing.T) {
	started := make(chan struct{})
	var startedOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-time.After(150 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()

	root := t.TempDir()
	server := NewServer(root, config.Server{Services: map[string]config.Service{"reports": {Target: upstream.URL}}})
	server.PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Run(ctx) }()

	client, err := NewClient(root, "timeout-client")
	if err != nil {
		t.Fatal(err)
	}
	client.PollInterval, client.Timeout, client.HeartbeatMaxAge = 5*time.Millisecond, 40*time.Millisecond, time.Second
	deadline := time.Now().Add(time.Second)
	for client.RefreshRegistry() != nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for !client.serverAlive() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !client.serverAlive() {
		t.Fatal("server heartbeat not published")
	}

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9000/reports/slow", nil)
	res := httptest.NewRecorder()
	client.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusGatewayTimeout)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("slow upstream was not called")
	}
}

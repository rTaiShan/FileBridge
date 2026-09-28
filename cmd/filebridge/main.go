package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"filebridge/internal/bridge"
	"filebridge/internal/config"
	"filebridge/internal/storage"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "server":
		runServer(os.Args[2:])
	case "client":
		runClient(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func runServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	share, configPath := fs.String("share", "", "shared FileBridge directory"), fs.String("config", "", "server services configuration")
	serverID := fs.String("server-id", "", "stable unique server ID (required)")
	poll := fs.Duration("poll", 500*time.Millisecond, "shared-folder polling interval")
	fs.Parse(args)
	if *share == "" || *configPath == "" || *serverID == "" {
		fs.Usage()
		os.Exit(2)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	server, err := bridge.NewServerWithID(*share, *serverID, cfg)
	if err != nil {
		log.Fatal(err)
	}
	server.PollInterval = *poll
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	log.Printf("FileBridge server %s: %d service(s), share %s", *serverID, len(cfg.Services), *share)
	if err := server.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

func runClient(args []string) {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	share, id := fs.String("share", "", "shared FileBridge directory"), fs.String("client-id", "", "stable client ID (generated if omitted)")
	listen := fs.String("listen", "127.0.0.1:9000", "local HTTP listener")
	timeout, poll := fs.Duration("timeout", 15*time.Second, "request timeout"), fs.Duration("poll", 500*time.Millisecond, "shared-folder polling interval")
	state := fs.String("state", defaultStatePath(), "client state directory")
	trustedServers := fs.String("servers", "", "comma-separated trusted server IDs (empty discovers all)")
	fs.Parse(args)
	if *share == "" {
		fs.Usage()
		os.Exit(2)
	}
	var err error
	if *id == "" {
		*id, err = persistentID(*state)
		if err != nil {
			log.Fatal(err)
		}
	}
	client, err := bridge.NewClient(*share, *id)
	if err != nil {
		log.Fatal(err)
	}
	if *trustedServers != "" {
		ids := make([]string, 0)
		for _, id := range strings.Split(*trustedServers, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			log.Fatal("--servers must contain at least one server ID")
		}
		if err := client.SetAllowedServers(ids); err != nil {
			log.Fatal(err)
		}
	}
	client.Timeout, client.PollInterval = *timeout, *poll
	client.HeartbeatMaxAge = 3 * *poll
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go client.RunRefresher(ctx)
	httpServer := &http.Server{Addr: *listen, Handler: client.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()
	log.Printf("FileBridge client %s listening on http://%s", *id, *listen)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func persistentID(state string) (string, error) {
	p := filepath.Join(state, "client-id")
	if b, err := os.ReadFile(p); err == nil {
		if existing := strings.TrimSpace(string(b)); storage.SafeName(existing) {
			return existing, nil
		}
	}
	id, err := storage.NewID()
	if err != nil {
		return "", err
	}
	if _, err := storage.WriteBodyAtomic(state, "client-id", strings.NewReader(id+"\n")); err != nil {
		return "", err
	}
	return id, nil
}

func defaultStatePath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return "."
	}
	return filepath.Join(d, "FileBridge")
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: filebridge <server|client> [options]")
}

package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"hubfly-scale/internal/api"
	"hubfly-scale/internal/bandwidth"
	"hubfly-scale/internal/docker"
	"hubfly-scale/internal/externalresources"
	"hubfly-scale/internal/externalstatus"
	"hubfly-scale/internal/scaler"
	"hubfly-scale/internal/store"
	"hubfly-scale/internal/traffic"
	"hubfly-scale/internal/vscale"
)

var Version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(Version)
		return
	}

	logger := log.New(os.Stdout, "hubfly-scale ", log.LstdFlags|log.Lmicroseconds)

	loadDotEnv(envPaths(), logger)

	dbPath := getenv("HF_SCALE_DB", "./data/hubfly-scale.db")
	addr := getenv("HF_SCALE_ADDR", ":10006")

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		logger.Fatalf("ensure db directory: %v", err)
	}

	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		logger.Fatalf("init store: %v", err)
	}
	defer st.Close()

	baseURL := getenv("HF_EXTERNAL_BASE_URL", "https://hubfly.space")
	statusKey := os.Getenv("API_KEY_EXTERNAL")
	statusClient := externalstatus.NewClient(joinURL(baseURL, "/api/containers/status"), statusKey, logger)
	resourcesClient := externalresources.NewClient(joinURL(baseURL, "/api/containers/resources"), statusKey)

	dockerClient := docker.NewCLIClient()
	watcher := traffic.NewWatcher(logger)
	manager := scaler.NewManager(st, dockerClient, watcher, logger, statusClient)
	if err := manager.LoadAndStart(context.Background()); err != nil {
		logger.Fatalf("load containers: %v", err)
	}
	vscaleManager := vscale.NewManager(st, dockerClient, logger, resourcesClient)
	if err := vscaleManager.LoadAndStart(context.Background()); err != nil {
		logger.Fatalf("load vertical containers: %v", err)
	}

	limiter := bandwidth.NewLimiter(dockerClient, logger)
	netLimiter := bandwidth.NewNetworkLimiter(dockerClient, logger)
	if err := applyStoredBandwidth(context.Background(), st, limiter, netLimiter, logger); err != nil {
		logger.Printf("apply stored bandwidth: %v", err)
	}
	server := api.NewServer(st, manager, limiter, netLimiter, vscaleManager, logger)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           server.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Printf("api listening on %s", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("listen server: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	manager.StopAll()
	vscaleManager.StopAll()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Printf("shutdown server: %v", err)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envPaths() []string {
	if raw := strings.TrimSpace(os.Getenv("HF_SCALE_ENV_PATHS")); raw != "" {
		parts := strings.Split(raw, ",")
		paths := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				paths = append(paths, p)
			}
		}
		if len(paths) > 0 {
			return paths
		}
	}
	if p := strings.TrimSpace(os.Getenv("HF_SCALE_ENV_PATH")); p != "" {
		return []string{p}
	}
	return []string{
		".env",
		"/hubfly-tool-manager/tools/hubfly-scale/.env",
	}
}

func loadDotEnv(paths []string, logger *log.Logger) {
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if logger != nil {
			logger.Printf("loaded env file path=%s", path)
		}
		parseDotEnv(string(data))
		return
	}
}

func parseDotEnv(content string) {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if key == "" {
			continue
		}
		val = trimQuotes(val)
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}

func trimQuotes(val string) string {
	if len(val) < 2 {
		return val
	}
	if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
		return val[1 : len(val)-1]
	}
	return val
}

func joinURL(base, path string) string {
	base = strings.TrimSpace(base)
	path = strings.TrimSpace(path)
	if base == "" {
		return path
	}
	if strings.HasSuffix(base, "/") {
		base = strings.TrimRight(base, "/")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

func applyStoredBandwidth(ctx context.Context, st *store.SQLiteStore, limiter *bandwidth.Limiter, netLimiter *bandwidth.NetworkLimiter, logger *log.Logger) error {
	if st == nil || limiter == nil || netLimiter == nil {
		return nil
	}
	limits, err := st.ListBandwidthLimits(ctx)
	if err != nil {
		return err
	}
	for _, limit := range limits {
		req := bandwidth.UpdateRequest{
			EgressMbps:  limit.EgressMbps,
			IngressMbps: limit.IngressMbps,
		}
		if err := limiter.Apply(ctx, limit.Name, req); err != nil {
			logger.Printf("container=%s bandwidth reapply failed: %v", limit.Name, err)
		} else {
			logger.Printf("container=%s bandwidth re-applied egress=%.2f ingress=%.2f", limit.Name, limit.EgressMbps, limit.IngressMbps)
		}
	}

	netLimits, err := st.ListNetworkBandwidthLimits(ctx)
	if err != nil {
		return err
	}
	for _, limit := range netLimits {
		req := bandwidth.UpdateRequest{
			EgressMbps:  limit.EgressMbps,
			IngressMbps: limit.IngressMbps,
		}
		if err := netLimiter.Apply(ctx, limit.Name, req); err != nil {
			logger.Printf("network=%s bandwidth reapply failed: %v", limit.Name, err)
		} else {
			logger.Printf("network=%s bandwidth re-applied egress=%.2f ingress=%.2f", limit.Name, limit.EgressMbps, limit.IngressMbps)
		}
	}
	return nil
}

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
	statusClient := externalstatus.NewClient(joinURL(baseURL, "/api/containers/status"), statusKey)
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

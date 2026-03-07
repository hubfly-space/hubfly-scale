package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hubfly-scale/internal/api"
	"hubfly-scale/internal/docker"
	"hubfly-scale/internal/scaler"
	"hubfly-scale/internal/store"
	"hubfly-scale/internal/traffic"
)

func main() {
	logger := log.New(os.Stdout, "hubfly-scale ", log.LstdFlags|log.Lmicroseconds)

	dbPath := getenv("HF_SCALE_DB", "./hubfly-scale.db")
	addr := getenv("HF_SCALE_ADDR", ":8080")

	st, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		logger.Fatalf("init store: %v", err)
	}
	defer st.Close()

	dockerClient := docker.NewCLIClient()
	watcher := traffic.NewWatcher(logger)
	manager := scaler.NewManager(st, dockerClient, watcher, logger)
	if err := manager.LoadAndStart(context.Background()); err != nil {
		logger.Fatalf("load containers: %v", err)
	}

	server := api.NewServer(st, manager, logger)
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

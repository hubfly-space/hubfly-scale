package scaler

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"hubfly-scale/internal/docker"
	"hubfly-scale/internal/externalstatus"
	"hubfly-scale/internal/model"
	"hubfly-scale/internal/store"
	"hubfly-scale/internal/traffic"
)

type Manager struct {
	store   *store.SQLiteStore
	docker  docker.Client
	watcher *traffic.Watcher
	logger  *log.Logger
	status  externalstatus.Reporter

	mu          sync.Mutex
	controllers map[string]context.CancelFunc
}

func NewManager(st *store.SQLiteStore, dc docker.Client, watcher *traffic.Watcher, logger *log.Logger, status externalstatus.Reporter) *Manager {
	return &Manager{
		store:       st,
		docker:      dc,
		watcher:     watcher,
		logger:      logger,
		status:      status,
		controllers: make(map[string]context.CancelFunc),
	}
}

func (m *Manager) LoadAndStart(ctx context.Context) error {
	containers, err := m.store.ListContainers(ctx)
	if err != nil {
		return fmt.Errorf("load containers: %w", err)
	}
	for _, c := range containers {
		if err := m.StartOrRestart(ctx, c.Config); err != nil {
			m.logger.Printf("container=%s start failed: %v", c.Config.Name, err)
		}
	}
	return nil
}

func (m *Manager) StartOrRestart(ctx context.Context, cfg model.ContainerConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("container name is required")
	}
	m.logger.Printf("container=%s register requested", cfg.Name)
	if cfg.BusyWindow <= 0 {
		cfg.BusyWindow = 2 * time.Second
	}
	if cfg.SleepAfter <= 0 {
		cfg.SleepAfter = 60 * time.Second
	}
	if cfg.InspectInterval <= 0 {
		cfg.InspectInterval = 5 * time.Second
	}
	if cfg.MinCPU < 0 || cfg.MaxCPU < 0 {
		return fmt.Errorf("min_cpu/max_cpu must be >= 0")
	}
	if cfg.MinCPU > 0 && cfg.MaxCPU > 0 && cfg.MaxCPU < cfg.MinCPU {
		return fmt.Errorf("max_cpu must be >= min_cpu")
	}

	if err := m.store.UpsertContainer(ctx, cfg); err != nil {
		m.logger.Printf("container=%s register upsert failed: %v", cfg.Name, err)
		return err
	}
	m.logger.Printf("container=%s register upserted", cfg.Name)

	m.mu.Lock()
	if cancel, ok := m.controllers[cfg.Name]; ok {
		m.logger.Printf("container=%s register replacing existing controller", cfg.Name)
		cancel()
		delete(m.controllers, cfg.Name)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	m.controllers[cfg.Name] = cancel
	m.mu.Unlock()

	ctrl := newController(cfg, m.store, m.docker, m.watcher, m.logger, m.status, m.Unregister)
	go ctrl.run(runCtx)
	m.logger.Printf("container=%s register controller started", cfg.Name)
	return nil
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, cancel := range m.controllers {
		cancel()
	}
	m.controllers = map[string]context.CancelFunc{}
}

func (m *Manager) ListTracked(ctx context.Context) ([]model.TrackedContainer, error) {
	containers, err := m.store.ListContainers(ctx)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	tracked := make(map[string]struct{}, len(m.controllers))
	for name := range m.controllers {
		tracked[name] = struct{}{}
	}
	m.mu.Unlock()

	out := make([]model.TrackedContainer, 0, len(tracked))
	for _, c := range containers {
		if _, ok := tracked[c.Config.Name]; !ok {
			continue
		}
		out = append(out, model.TrackedContainer{
			Name:      c.Config.Name,
			CurrentIP: c.Runtime.CurrentIP,
			Status:    c.Runtime.Status,
			Paused:    c.Runtime.Paused,
		})
	}
	return out, nil
}

func (m *Manager) Unregister(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("container name is required")
	}

	m.logger.Printf("container=%s unregister requested", name)

	if m.docker != nil {
		if err := m.docker.Unpause(ctx, name); err != nil {
			m.logger.Printf("container=%s unregister unpause failed: %v", name, err)
		} else {
			m.logger.Printf("container=%s unregister unpause succeeded", name)
		}
	}

	m.mu.Lock()
	if cancel, ok := m.controllers[name]; ok {
		cancel()
		delete(m.controllers, name)
	}
	m.mu.Unlock()

	m.store.DropRuntime(name)

	if err := m.store.DeleteContainer(ctx, name); err != nil {
		m.logger.Printf("container=%s unregister delete failed: %v", name, err)
		return err
	}

	m.logger.Printf("container=%s unregister completed", name)
	return nil
}

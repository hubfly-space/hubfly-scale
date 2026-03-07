package scaler

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"hubfly-scale/internal/docker"
	"hubfly-scale/internal/model"
	"hubfly-scale/internal/store"
	"hubfly-scale/internal/traffic"
)

type Manager struct {
	store   *store.SQLiteStore
	docker  docker.Client
	watcher *traffic.Watcher
	logger  *log.Logger

	mu          sync.Mutex
	controllers map[string]context.CancelFunc
}

func NewManager(st *store.SQLiteStore, dc docker.Client, watcher *traffic.Watcher, logger *log.Logger) *Manager {
	return &Manager{
		store:       st,
		docker:      dc,
		watcher:     watcher,
		logger:      logger,
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
	if cfg.BusyWindow <= 0 {
		cfg.BusyWindow = 2 * time.Second
	}
	if cfg.SleepAfter <= 0 {
		cfg.SleepAfter = 60 * time.Second
	}
	if cfg.InspectInterval <= 0 {
		cfg.InspectInterval = 5 * time.Second
	}

	if err := m.store.UpsertContainer(ctx, cfg); err != nil {
		return err
	}

	m.mu.Lock()
	if cancel, ok := m.controllers[cfg.Name]; ok {
		cancel()
		delete(m.controllers, cfg.Name)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	m.controllers[cfg.Name] = cancel
	m.mu.Unlock()

	ctrl := newController(cfg, m.store, m.docker, m.watcher, m.logger)
	go ctrl.run(runCtx)
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

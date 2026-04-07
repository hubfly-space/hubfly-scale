package vscale

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"hubfly-scale/internal/docker"
	"hubfly-scale/internal/model"
	"hubfly-scale/internal/store"
)

type Manager struct {
	store  *store.SQLiteStore
	docker docker.Client
	logger *log.Logger

	mu          sync.Mutex
	controllers map[string]context.CancelFunc
}

func NewManager(st *store.SQLiteStore, dc docker.Client, logger *log.Logger) *Manager {
	return &Manager{
		store:       st,
		docker:      dc,
		logger:      logger,
		controllers: make(map[string]context.CancelFunc),
	}
}

func (m *Manager) LoadAndStart(ctx context.Context) error {
	containers, err := m.store.ListVertical(ctx)
	if err != nil {
		return fmt.Errorf("load vertical: %w", err)
	}
	for _, c := range containers {
		if err := m.StartOrRestart(ctx, c.Config); err != nil {
			m.logger.Printf("vscale container=%s start failed: %v", c.Config.Name, err)
		}
	}
	return nil
}

func (m *Manager) StartOrRestart(ctx context.Context, cfg model.VerticalScaleConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("container name is required")
	}
	if cfg.MinCPU <= 0 || cfg.MaxCPU <= 0 {
		return fmt.Errorf("min_cpu and max_cpu are required")
	}
	if cfg.MaxCPU < cfg.MinCPU {
		return fmt.Errorf("max_cpu must be >= min_cpu")
	}

	if err := m.store.UpsertVertical(ctx, cfg); err != nil {
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

	ctrl := newController(cfg, m.store, m.docker, m.logger)
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

func (m *Manager) Unregister(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("container name is required")
	}

	m.mu.Lock()
	if cancel, ok := m.controllers[name]; ok {
		cancel()
		delete(m.controllers, name)
	}
	m.mu.Unlock()

	return m.store.DeleteVertical(ctx, name)
}

func (m *Manager) Touch(ctx context.Context, name string) error {
	info, err := m.store.GetVertical(ctx, name)
	if err != nil {
		return err
	}
	if info.Runtime.CurrentCPU <= 0 {
		info.Runtime.CurrentCPU = info.Config.MinCPU
	}
	info.Runtime.UpdatedAt = time.Now().UTC()
	return m.store.UpdateVerticalRuntime(ctx, info.Runtime)
}

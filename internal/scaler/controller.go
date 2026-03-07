package scaler

import (
	"context"
	"log"
	"sync"
	"time"

	"hubfly-scale/internal/docker"
	"hubfly-scale/internal/model"
	"hubfly-scale/internal/store"
	"hubfly-scale/internal/traffic"
)

type controller struct {
	cfg     model.ContainerConfig
	store   *store.SQLiteStore
	docker  docker.Client
	watcher *traffic.Watcher
	logger  *log.Logger

	mu      sync.Mutex
	runtime model.ContainerRuntime
}

func newController(cfg model.ContainerConfig, st *store.SQLiteStore, dc docker.Client, w *traffic.Watcher, logger *log.Logger) *controller {
	now := time.Now().UTC()
	return &controller{
		cfg:     cfg,
		store:   st,
		docker:  dc,
		watcher: w,
		logger:  logger,
		runtime: model.ContainerRuntime{
			Name:              cfg.Name,
			Status:            model.StatusIdle,
			LastStateChangeAt: now,
			UpdatedAt:         now,
		},
	}
}

func (c *controller) loadPersistedRuntime(ctx context.Context) {
	info, err := c.store.GetContainer(ctx, c.cfg.Name)
	if err != nil {
		return
	}
	c.runtime = info.Runtime
}

func (c *controller) run(ctx context.Context) {
	c.loadPersistedRuntime(ctx)

	inspectTicker := time.NewTicker(c.cfg.InspectInterval)
	if c.cfg.InspectInterval <= 0 {
		inspectTicker = time.NewTicker(5 * time.Second)
	}
	persistTicker := time.NewTicker(2 * time.Second)
	defer inspectTicker.Stop()
	defer persistTicker.Stop()

	var (
		watchCancel context.CancelFunc
		packetCh    <-chan struct{}
		errCh       <-chan error
	)

	reconcile := func() {
		now := time.Now().UTC()

		ip, err := c.docker.InspectIP(ctx, c.cfg.Name)
		if err != nil {
			c.setStatus(now, model.StatusError)
			c.logger.Printf("container=%s inspect ip failed: %v", c.cfg.Name, err)
			return
		}
		paused, err := c.docker.InspectPaused(ctx, c.cfg.Name)
		if err != nil {
			c.setStatus(now, model.StatusError)
			c.logger.Printf("container=%s inspect paused failed: %v", c.cfg.Name, err)
			return
		}

		if ip != "" && ip != c.runtime.CurrentIP {
			if watchCancel != nil {
				watchCancel()
			}
			watchCtx, cancel := context.WithCancel(ctx)
			watchCancel = cancel

			pch, ech, err := c.watcher.Start(watchCtx, ip)
			if err != nil {
				c.setStatus(now, model.StatusError)
				c.logger.Printf("container=%s start watcher failed: %v", c.cfg.Name, err)
			} else {
				packetCh = pch
				errCh = ech
			}
			c.runtime.CurrentIP = ip
			c.logger.Printf("container=%s watcher bound ip=%s", c.cfg.Name, ip)
		}

		c.runtime.Paused = paused
		newStatus := decideStatus(now, c.runtime.LastTrafficAt, paused, c.cfg.BusyWindow)
		c.setStatus(now, newStatus)

		if shouldPause(now, c.runtime.LastTrafficAt, c.runtime.LastStateChangeAt, c.cfg.SleepAfter, paused) {
			if err := c.docker.Pause(ctx, c.cfg.Name); err != nil {
				c.setStatus(now, model.StatusError)
				c.logger.Printf("container=%s pause failed: %v", c.cfg.Name, err)
			} else {
				c.runtime.Paused = true
				c.setStatus(now, model.StatusSleeping)
				c.logger.Printf("container=%s paused after inactivity", c.cfg.Name)
			}
		}
	}

	reconcile()
	for {
		select {
		case <-ctx.Done():
			if watchCancel != nil {
				watchCancel()
			}
			_ = c.store.UpdateRuntime(context.Background(), c.snapshot())
			return
		case <-inspectTicker.C:
			reconcile()
		case <-persistTicker.C:
			_ = c.store.UpdateRuntime(ctx, c.snapshot())
		case <-packetCh:
			now := time.Now().UTC()
			c.runtime.LastTrafficAt = &now
			if c.runtime.Paused {
				if err := c.docker.Unpause(ctx, c.cfg.Name); err != nil {
					c.setStatus(now, model.StatusError)
					c.logger.Printf("container=%s unpause failed: %v", c.cfg.Name, err)
				} else {
					c.runtime.Paused = false
					c.logger.Printf("container=%s unpaused on traffic", c.cfg.Name)
				}
			}
			c.setStatus(now, model.StatusBusy)
		case err := <-errCh:
			if err != nil {
				c.logger.Printf("container=%s watcher error: %v", c.cfg.Name, err)
				c.setStatus(time.Now().UTC(), model.StatusError)
			}
		}
	}
}

func (c *controller) setStatus(now time.Time, status string) {
	if c.runtime.Status != status {
		c.runtime.Status = status
		c.runtime.LastStateChangeAt = now
	}
}

func (c *controller) snapshot() model.ContainerRuntime {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := c.runtime
	cp.UpdatedAt = time.Now().UTC()
	return cp
}

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

const maxInspectFailures = 3

type controller struct {
	cfg     model.ContainerConfig
	store   *store.SQLiteStore
	docker  docker.Client
	watcher *traffic.Watcher
	logger  *log.Logger
	drop    func(context.Context, string) error

	mu      sync.Mutex
	runtime model.ContainerRuntime

	inspectFailures int

	cpuWindow    *cpuWindow
	cpuPrevStats docker.ContainerStats
	cpuHasPrev   bool
}

func newController(cfg model.ContainerConfig, st *store.SQLiteStore, dc docker.Client, w *traffic.Watcher, logger *log.Logger, drop func(context.Context, string) error) *controller {
	now := time.Now().UTC()
	return &controller{
		cfg:     cfg,
		store:   st,
		docker:  dc,
		watcher: w,
		logger:  logger,
		drop:    drop,
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

func (c *controller) cpuScalingEnabled() bool {
	return c.cfg.MinCPU > 0 && c.cfg.MaxCPU > 0 && c.cfg.MaxCPU >= c.cfg.MinCPU
}

func (c *controller) initCPUState() {
	if !c.cpuScalingEnabled() {
		return
	}
	if c.runtime.CurrentCPU <= 0 {
		c.runtime.CurrentCPU = c.cfg.MinCPU
	}
	if c.runtime.CurrentCPU < c.cfg.MinCPU {
		c.runtime.CurrentCPU = c.cfg.MinCPU
	}
	if c.runtime.CurrentCPU > c.cfg.MaxCPU {
		c.runtime.CurrentCPU = c.cfg.MaxCPU
	}
	if c.cpuWindow == nil {
		c.cpuWindow = newCPUWindow(cpuWindowSize)
	}
}

func (c *controller) run(ctx context.Context) {
	c.loadPersistedRuntime(ctx)
	c.initCPUState()

	inspectTicker := time.NewTicker(c.cfg.InspectInterval)
	if c.cfg.InspectInterval <= 0 {
		inspectTicker = time.NewTicker(5 * time.Second)
	}
	persistTicker := time.NewTicker(2 * time.Second)
	var cpuTicker *time.Ticker
	if c.cpuScalingEnabled() {
		cpuTicker = time.NewTicker(cpuPollInterval)
	}
	defer inspectTicker.Stop()
	defer persistTicker.Stop()
	if cpuTicker != nil {
		defer cpuTicker.Stop()
	}

	var (
		watchCancel context.CancelFunc
		packetCh    <-chan struct{}
		errCh       <-chan error
	)

	reconcile := func() bool {
		now := time.Now().UTC()

		ip, err := c.docker.InspectIP(ctx, c.cfg.Name)
		if err != nil {
			if c.handleInspectFailure(now, "inspect ip", err) {
				if watchCancel != nil {
					watchCancel()
				}
				return true
			}
			return false
		}
		paused, err := c.docker.InspectPaused(ctx, c.cfg.Name)
		if err != nil {
			if c.handleInspectFailure(now, "inspect paused", err) {
				if watchCancel != nil {
					watchCancel()
				}
				return true
			}
			return false
		}
		c.inspectFailures = 0

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
		return false
	}

	if reconcile() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			if watchCancel != nil {
				watchCancel()
			}
			_ = c.store.UpdateRuntime(context.Background(), c.snapshot())
			return
		case <-inspectTicker.C:
			if reconcile() {
				return
			}
		case <-persistTicker.C:
			_ = c.store.UpdateRuntime(ctx, c.snapshot())
		case <-func() <-chan time.Time {
			if cpuTicker != nil {
				return cpuTicker.C
			}
			return nil
		}():
			c.handleCPUScale(ctx)
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

func (c *controller) handleInspectFailure(now time.Time, op string, err error) bool {
	c.inspectFailures++
	c.setStatus(now, model.StatusError)
	c.logger.Printf("container=%s %s failed: %v", c.cfg.Name, op, err)
	if c.inspectFailures < maxInspectFailures {
		return false
	}

	c.logger.Printf("container=%s dropping after %d consecutive inspect failures", c.cfg.Name, c.inspectFailures)
	if c.drop != nil {
		if err := c.drop(context.Background(), c.cfg.Name); err != nil {
			c.logger.Printf("container=%s drop failed: %v", c.cfg.Name, err)
		}
	}
	return true
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

func (c *controller) handleCPUScale(ctx context.Context) {
	if !c.cpuScalingEnabled() {
		return
	}
	now := time.Now().UTC()
	stats, err := c.docker.Stats(ctx, c.cfg.Name)
	if err != nil {
		c.logger.Printf("container=%s stats failed: %v", c.cfg.Name, err)
		return
	}

	usagePct, ok := cpuUsagePercent(c.cpuPrevStats, stats, c.runtime.CurrentCPU, c.cpuHasPrev)
	c.cpuPrevStats = stats
	c.cpuHasPrev = true
	if !ok {
		return
	}

	if c.runtime.CurrentCPU <= 0 {
		c.runtime.CurrentCPU = c.cfg.MinCPU
	}

	if c.runtime.CPUCooldownUntil != nil && now.Before(*c.runtime.CPUCooldownUntil) {
		c.cpuWindow.Reset()
		return
	}

	c.cpuWindow.Add(usagePct)
	if !c.cpuWindow.Full() {
		return
	}

	if c.cpuWindow.CountAbove(cpuScaleUpThreshold) >= cpuRequiredHits {
		c.scaleCPU(ctx, now, cpuStepCores)
		return
	}
	if c.cpuWindow.CountBelow(cpuScaleDownThreshold) >= cpuRequiredHits {
		c.scaleCPU(ctx, now, -cpuStepCores)
		return
	}
}

func (c *controller) scaleCPU(ctx context.Context, now time.Time, delta float64) {
	target := c.runtime.CurrentCPU + delta
	if delta > 0 && target > c.cfg.MaxCPU {
		target = c.cfg.MaxCPU
	}
	if delta < 0 && target < c.cfg.MinCPU {
		target = c.cfg.MinCPU
	}
	if floatEquals(target, c.runtime.CurrentCPU) {
		c.cpuWindow.Reset()
		return
	}

	nano := int64(target * 1e9)
	if nano < 1 {
		nano = 1
	}
	if err := c.docker.UpdateCPU(ctx, c.cfg.Name, nano); err != nil {
		c.logger.Printf("container=%s cpu update failed: %v", c.cfg.Name, err)
		return
	}

	c.runtime.CurrentCPU = target
	cooldownUntil := now.Add(cpuCooldown)
	c.runtime.CPUCooldownUntil = &cooldownUntil
	c.cpuWindow.Reset()
	c.logger.Printf("container=%s cpu scaled to %.2f cores", c.cfg.Name, target)
}

func floatEquals(a, b float64) bool {
	const eps = 0.000001
	if a > b {
		return a-b < eps
	}
	return b-a < eps
}

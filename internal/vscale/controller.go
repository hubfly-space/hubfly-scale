package vscale

import (
	"context"
	"log"
	"time"

	"hubfly-scale/internal/docker"
	"hubfly-scale/internal/model"
	"hubfly-scale/internal/store"
)

type controller struct {
	cfg    model.VerticalScaleConfig
	store  *store.SQLiteStore
	docker docker.Client
	logger *log.Logger

	runtime     model.VerticalScaleRuntime
	cpuWindow   *cpuWindow
	prevStats   docker.ContainerStats
	hasPrevStat bool
}

func newController(cfg model.VerticalScaleConfig, st *store.SQLiteStore, dc docker.Client, logger *log.Logger) *controller {
	now := time.Now().UTC()
	return &controller{
		cfg:    cfg,
		store:  st,
		docker: dc,
		logger: logger,
		runtime: model.VerticalScaleRuntime{
			Name:       cfg.Name,
			CurrentCPU: cfg.MinCPU,
			UpdatedAt:  now,
		},
		cpuWindow: newCPUWindow(cpuWindowSize),
	}
}

func (c *controller) loadPersistedRuntime(ctx context.Context) {
	info, err := c.store.GetVertical(ctx, c.cfg.Name)
	if err != nil {
		return
	}
	c.runtime = info.Runtime
}

func (c *controller) initCPUState() {
	if c.runtime.CurrentCPU <= 0 {
		c.runtime.CurrentCPU = c.cfg.MinCPU
	}
	if c.runtime.CurrentCPU < c.cfg.MinCPU {
		c.runtime.CurrentCPU = c.cfg.MinCPU
	}
	if c.runtime.CurrentCPU > c.cfg.MaxCPU {
		c.runtime.CurrentCPU = c.cfg.MaxCPU
	}
}

func (c *controller) run(ctx context.Context) {
	c.loadPersistedRuntime(ctx)
	c.initCPUState()

	pollTicker := time.NewTicker(cpuPollInterval)
	persistTicker := time.NewTicker(10 * time.Second)
	defer pollTicker.Stop()
	defer persistTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = c.store.UpdateVerticalRuntime(context.Background(), c.snapshot())
			return
		case <-persistTicker.C:
			_ = c.store.UpdateVerticalRuntime(ctx, c.snapshot())
		case <-pollTicker.C:
			c.handleCPUScale(ctx)
		}
	}
}

func (c *controller) handleCPUScale(ctx context.Context) {
	now := time.Now().UTC()
	stats, err := c.docker.Stats(ctx, c.cfg.Name)
	if err != nil {
		c.logger.Printf("vscale container=%s stats failed: %v", c.cfg.Name, err)
		return
	}

	usagePct, ok := cpuUsagePercent(c.prevStats, stats, c.runtime.CurrentCPU, c.hasPrevStat)
	c.prevStats = stats
	c.hasPrevStat = true
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
		c.logger.Printf("vscale container=%s cpu update failed: %v", c.cfg.Name, err)
		return
	}

	c.runtime.CurrentCPU = target
	cooldownUntil := now.Add(cpuCooldown)
	c.runtime.CPUCooldownUntil = &cooldownUntil
	c.cpuWindow.Reset()
	c.logger.Printf("vscale container=%s cpu scaled to %.2f cores", c.cfg.Name, target)
}

func (c *controller) snapshot() model.VerticalScaleRuntime {
	rt := c.runtime
	rt.UpdatedAt = time.Now().UTC()
	return rt
}

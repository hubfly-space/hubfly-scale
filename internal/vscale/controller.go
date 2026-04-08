package vscale

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"

	"hubfly-scale/internal/docker"
	"hubfly-scale/internal/externalresources"
	"hubfly-scale/internal/model"
	"hubfly-scale/internal/store"
)

type controller struct {
	cfg    model.VerticalScaleConfig
	store  *store.SQLiteStore
	docker docker.Client
	logger *log.Logger
	res    externalresources.Reporter
	drop   func(context.Context, string) error

	runtime       model.VerticalScaleRuntime
	cpuWindow     *cpuWindow
	memWindow     *memWindow
	memLongWindow *memWindow
	prevStats     docker.ContainerStats
	hasPrevStat   bool
	statsFailures int
	dockerID      string
}

const maxStatsFailures = 3

func newController(cfg model.VerticalScaleConfig, st *store.SQLiteStore, dc docker.Client, logger *log.Logger, res externalresources.Reporter, drop func(context.Context, string) error) *controller {
	now := time.Now().UTC()
	return &controller{
		cfg:    cfg,
		store:  st,
		docker: dc,
		logger: logger,
		res:    res,
		drop:   drop,
		runtime: model.VerticalScaleRuntime{
			Name:         cfg.Name,
			CurrentCPU:   cfg.MinCPU,
			CurrentMemMB: cfg.MinMemMB,
			UpdatedAt:    now,
		},
		cpuWindow:     newCPUWindow(cpuWindowSize),
		memWindow:     newMemWindow(memUpWindowSize),
		memLongWindow: newMemWindow(memDownWindowSize),
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
	if c.runtime.CurrentMemMB <= 0 {
		c.runtime.CurrentMemMB = c.cfg.MinMemMB
	}
	if c.runtime.CurrentMemMB < c.cfg.MinMemMB {
		c.runtime.CurrentMemMB = c.cfg.MinMemMB
	}
	if c.runtime.CurrentMemMB > c.cfg.MaxMemMB && c.cfg.MaxMemMB > 0 {
		c.runtime.CurrentMemMB = c.cfg.MaxMemMB
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
			if c.handleScale(ctx) {
				return
			}
		}
	}
}

func (c *controller) handleScale(ctx context.Context) bool {
	now := time.Now().UTC()
	stats, err := c.docker.Stats(ctx, c.cfg.Name)
	if err != nil {
		c.logger.Printf("vscale container=%s stats failed: %v", c.cfg.Name, err)
		return c.handleStatsFailure(now, err)
	}
	c.statsFailures = 0

	cpuUsagePct, cpuOK := cpuUsagePercent(c.prevStats, stats, c.runtime.CurrentCPU, c.hasPrevStat)
	memWorkingSetMB := workingSetMB(stats)
	memUsagePct := memoryUsagePercent(memWorkingSetMB, c.runtime.CurrentMemMB)

	oomDetected := stats.OOMKills > c.prevStats.OOMKills || stats.Failcnt > c.prevStats.Failcnt

	c.prevStats = stats
	c.hasPrevStat = true

	if c.runtime.CurrentCPU <= 0 {
		c.runtime.CurrentCPU = c.cfg.MinCPU
	}

	if c.runtime.CPUCooldownUntil != nil && now.Before(*c.runtime.CPUCooldownUntil) {
		c.cpuWindow.Reset()
	} else if cpuOK {
		c.cpuWindow.Add(cpuUsagePct)
		if c.cpuWindow.Full() {
			if c.cpuWindow.CountAbove(cpuScaleUpThreshold) >= cpuRequiredHits {
				c.scaleCPU(ctx, now, cpuStepCores)
				return false
			}
			if c.cpuWindow.CountBelow(cpuScaleDownThreshold) >= cpuRequiredHits {
				c.scaleCPU(ctx, now, -cpuStepCores)
				return false
			}
		}
	}

	c.handleMemoryScale(ctx, now, memWorkingSetMB, memUsagePct, oomDetected)
	return false
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

	if err := c.updateResources(ctx, &target, nil); err != nil {
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

func (c *controller) handleStatsFailure(now time.Time, err error) bool {
	c.statsFailures++
	if c.statsFailures < maxStatsFailures {
		return false
	}
	c.logger.Printf("vscale container=%s dropping after %d consecutive stats failures", c.cfg.Name, c.statsFailures)
	if c.drop != nil {
		if derr := c.drop(context.Background(), c.cfg.Name); derr != nil {
			c.logger.Printf("vscale container=%s drop failed: %v", c.cfg.Name, derr)
		}
	}
	return true
}

func (c *controller) handleMemoryScale(ctx context.Context, now time.Time, workingSetMB int64, usagePct float64, oomDetected bool) {
	if c.cfg.MinMemMB <= 0 || c.cfg.MaxMemMB <= 0 || c.cfg.MaxMemMB < c.cfg.MinMemMB {
		return
	}

	if oomDetected {
		target := int64(float64(c.runtime.CurrentMemMB) * memOOMMultiplier)
		c.scaleMemory(ctx, now, target, memUpCooldown)
		return
	}

	if c.runtime.MemCooldownUntil != nil && now.Before(*c.runtime.MemCooldownUntil) {
		c.memWindow.Reset()
		return
	}

	if usagePct > 0 {
		c.memWindow.Add(usagePct)
		if c.memWindow.Full() && c.memWindow.CountAbove(memUpThreshold) >= memUpRequiredHits {
			target := c.runtime.CurrentMemMB + memUpStepMB
			c.scaleMemory(ctx, now, target, memUpCooldown)
			return
		}
	}

	c.memLongWindow.Add(float64(workingSetMB))
	if c.memLongWindow.Full() {
		p95 := c.memLongWindow.P95()
		if p95 > 0 && p95 <= float64(c.runtime.CurrentMemMB)*memDownThreshold/100 {
			target := c.runtime.CurrentMemMB - memDownStepMB
			minAllowed := int64(math.Ceil(p95 * memHeadroomMultiplier))
			if minAllowed < c.cfg.MinMemMB {
				minAllowed = c.cfg.MinMemMB
			}
			if target < minAllowed {
				target = minAllowed
			}
			c.scaleMemory(ctx, now, target, memDownCooldown)
		}
	}
}

func (c *controller) scaleMemory(ctx context.Context, now time.Time, target int64, cooldown time.Duration) {
	if target > c.cfg.MaxMemMB {
		target = c.cfg.MaxMemMB
	}
	if target < c.cfg.MinMemMB {
		target = c.cfg.MinMemMB
	}
	if target == c.runtime.CurrentMemMB {
		return
	}
	if err := c.updateResources(ctx, nil, &target); err != nil {
		c.logger.Printf("vscale container=%s memory update failed: %v", c.cfg.Name, err)
		return
	}
	c.runtime.CurrentMemMB = target
	cooldownUntil := now.Add(cooldown)
	c.runtime.MemCooldownUntil = &cooldownUntil
	c.memWindow.Reset()
	c.memLongWindow.Reset()
	c.logger.Printf("vscale container=%s memory scaled to %d MB", c.cfg.Name, target)
}

func workingSetMB(stats docker.ContainerStats) int64 {
	usage := stats.MemoryUsage
	cache := stats.MemoryCache
	if usage > cache {
		usage = usage - cache
	}
	return int64(usage / (1024 * 1024))
}

func memoryUsagePercent(workingSetMB int64, currentMemMB int64) float64 {
	if currentMemMB <= 0 {
		return 0
	}
	return (float64(workingSetMB) / float64(currentMemMB)) * 100
}

func (c *controller) updateResources(ctx context.Context, cpu *float64, memMB *int64) error {
	if c.res == nil {
		return fmt.Errorf("external resource reporter not configured")
	}
	if c.dockerID == "" {
		id, err := c.docker.InspectID(ctx, c.cfg.Name)
		if err != nil {
			return err
		}
		c.dockerID = id
	}
	if c.dockerID == "" {
		return fmt.Errorf("empty docker container id")
	}
	return c.res.Update(ctx, c.dockerID, cpu, memMB)
}

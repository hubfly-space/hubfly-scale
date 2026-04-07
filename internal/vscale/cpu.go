package vscale

import (
	"time"

	"hubfly-scale/internal/docker"
)

const (
	cpuPollInterval       = 5 * time.Second
	cpuScaleUpThreshold   = 80.0
	cpuScaleDownThreshold = 40.0
	cpuWindowSize         = 12
	cpuRequiredHits       = 10
	cpuStepCores          = 0.2
	cpuCooldown           = 5 * time.Minute
)

type cpuWindow struct {
	samples []float64
	size    int
	idx     int
	count   int
}

func newCPUWindow(size int) *cpuWindow {
	return &cpuWindow{
		samples: make([]float64, size),
		size:    size,
	}
}

func (w *cpuWindow) Add(v float64) {
	w.samples[w.idx] = v
	w.idx = (w.idx + 1) % w.size
	if w.count < w.size {
		w.count++
	}
}

func (w *cpuWindow) Full() bool {
	return w.count >= w.size
}

func (w *cpuWindow) Reset() {
	w.idx = 0
	w.count = 0
}

func (w *cpuWindow) CountAbove(threshold float64) int {
	n := w.count
	if n == 0 {
		return 0
	}
	count := 0
	for i := 0; i < n; i++ {
		if w.samples[i] >= threshold {
			count++
		}
	}
	return count
}

func (w *cpuWindow) CountBelow(threshold float64) int {
	n := w.count
	if n == 0 {
		return 0
	}
	count := 0
	for i := 0; i < n; i++ {
		if w.samples[i] <= threshold {
			count++
		}
	}
	return count
}

func cpuUsagePercent(prev docker.ContainerStats, curr docker.ContainerStats, currentCPU float64, hasPrev bool) (float64, bool) {
	if !hasPrev {
		return 0, false
	}
	if currentCPU <= 0 {
		return 0, false
	}
	if curr.TotalUsage < prev.TotalUsage || curr.SystemUsage < prev.SystemUsage {
		return 0, false
	}
	cpuDelta := curr.TotalUsage - prev.TotalUsage
	systemDelta := curr.SystemUsage - prev.SystemUsage
	if systemDelta == 0 {
		return 0, false
	}
	online := curr.OnlineCPUs
	if online == 0 {
		online = prev.OnlineCPUs
	}
	if online == 0 {
		return 0, false
	}
	coresUsed := (float64(cpuDelta) / float64(systemDelta)) * float64(online)
	if coresUsed < 0 {
		coresUsed = 0
	}
	return (coresUsed / currentCPU) * 100, true
}

func floatEquals(a, b float64) bool {
	const eps = 0.000001
	if a > b {
		return a-b < eps
	}
	return b-a < eps
}

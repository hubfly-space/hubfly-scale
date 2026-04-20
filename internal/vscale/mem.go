package vscale

import (
	"math"
	"sort"
	"time"
)

const (
	memUpThreshold        = 85.0
	memDownThreshold      = 50.0
	memUpWindowSize       = 4
	memUpRequiredHits     = 4
	memDownWindowSize     = 360 // 30m at 5s polls
	memUpStepMB           = 128
	memDownStepMB         = 128
	memUpCooldown         = 3 * time.Minute
	memDownCooldown       = 30 * time.Minute
	memHeadroomMultiplier = 1.3
	memOOMMultiplier      = 2.0
)

type memWindow struct {
	samples []float64
	size    int
	idx     int
	count   int
}

func newMemWindow(size int) *memWindow {
	return &memWindow{
		samples: make([]float64, size),
		size:    size,
	}
}

func (w *memWindow) Add(v float64) {
	w.samples[w.idx] = v
	w.idx = (w.idx + 1) % w.size
	if w.count < w.size {
		w.count++
	}
}

func (w *memWindow) Full() bool {
	return w.count >= w.size
}

func (w *memWindow) Reset() {
	w.idx = 0
	w.count = 0
}

func (w *memWindow) CountAbove(threshold float64) int {
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

func (w *memWindow) P95() float64 {
	if w.count == 0 {
		return 0
	}
	cp := make([]float64, w.count)
	copy(cp, w.samples[:w.count])
	sort.Float64s(cp)
	idx := int(math.Ceil(float64(w.count)*0.95)) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

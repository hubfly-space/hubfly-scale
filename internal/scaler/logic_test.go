package scaler

import (
	"testing"
	"time"

	"hubfly-scale/internal/model"
)

func TestDecideStatus(t *testing.T) {
	now := time.Now()
	last := now.Add(-1 * time.Second)

	if got := decideStatus(now, &last, false, 2*time.Second); got != model.StatusBusy {
		t.Fatalf("expected busy, got %s", got)
	}
	if got := decideStatus(now, &last, false, 100*time.Millisecond); got != model.StatusIdle {
		t.Fatalf("expected idle, got %s", got)
	}
	if got := decideStatus(now, &last, true, 2*time.Second); got != model.StatusSleeping {
		t.Fatalf("expected sleeping, got %s", got)
	}
}

func TestShouldPause(t *testing.T) {
	now := time.Now()
	ref := now.Add(-5 * time.Minute)
	last := now.Add(-20 * time.Second)

	if !shouldPause(now, &last, ref, 10*time.Second, false) {
		t.Fatal("expected pause when idle exceeds threshold")
	}
	if shouldPause(now, &last, ref, 30*time.Second, false) {
		t.Fatal("did not expect pause before threshold")
	}
	if shouldPause(now, nil, ref, 10*time.Minute, false) {
		t.Fatal("did not expect pause when no traffic and reference is recent")
	}
	if shouldPause(now, &last, ref, 10*time.Second, true) {
		t.Fatal("did not expect pause when already paused")
	}
}

package scaler

import (
	"time"

	"hubfly-scale/internal/model"
)

func decideStatus(now time.Time, lastTrafficAt *time.Time, paused bool, busyWindow time.Duration) string {
	if paused {
		return model.StatusSleeping
	}
	if lastTrafficAt != nil && now.Sub(*lastTrafficAt) <= busyWindow {
		return model.StatusBusy
	}
	return model.StatusIdle
}

func shouldPause(now time.Time, lastTrafficAt *time.Time, reference time.Time, sleepAfter time.Duration, paused bool) bool {
	if paused {
		return false
	}
	if sleepAfter <= 0 {
		return false
	}
	idleSince := reference
	if lastTrafficAt != nil {
		idleSince = *lastTrafficAt
	}
	return now.Sub(idleSince) >= sleepAfter
}

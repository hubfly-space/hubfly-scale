package model

import "time"

const (
	StatusBusy     = "busy"
	StatusIdle     = "idle"
	StatusSleeping = "sleeping"
	StatusError    = "error"
)

type ContainerConfig struct {
	Name            string        `json:"name"`
	SleepAfter      time.Duration `json:"-"`
	BusyWindow      time.Duration `json:"-"`
	InspectInterval time.Duration `json:"-"`
}

type ContainerConfigAPI struct {
	Name                string `json:"name"`
	SleepAfterSeconds   int64  `json:"sleep_after_seconds"`
	BusyWindowSeconds   int64  `json:"busy_window_seconds"`
	InspectIntervalSecs int64  `json:"inspect_interval_seconds"`
}

func (c ContainerConfigAPI) ToInternal() ContainerConfig {
	return ContainerConfig{
		Name:            c.Name,
		SleepAfter:      time.Duration(c.SleepAfterSeconds) * time.Second,
		BusyWindow:      time.Duration(c.BusyWindowSeconds) * time.Second,
		InspectInterval: time.Duration(c.InspectIntervalSecs) * time.Second,
	}
}

func (c ContainerConfig) ToAPI() ContainerConfigAPI {
	return ContainerConfigAPI{
		Name:                c.Name,
		SleepAfterSeconds:   int64(c.SleepAfter / time.Second),
		BusyWindowSeconds:   int64(c.BusyWindow / time.Second),
		InspectIntervalSecs: int64(c.InspectInterval / time.Second),
	}
}

type ContainerRuntime struct {
	Name              string     `json:"name"`
	CurrentIP         string     `json:"current_ip"`
	Status            string     `json:"status"`
	LastTrafficAt     *time.Time `json:"last_traffic_at,omitempty"`
	LastStateChangeAt time.Time  `json:"last_state_change_at"`
	Paused            bool       `json:"paused"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type ContainerInfo struct {
	Config  ContainerConfig  `json:"config"`
	Runtime ContainerRuntime `json:"runtime"`
}

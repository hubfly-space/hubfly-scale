package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"hubfly-scale/internal/model"
)

type SQLiteStore struct {
	db *sql.DB

	runtimeQueue     chan model.ContainerRuntime
	runtimeStop      chan struct{}
	runtimeDone      chan struct{}
	runtimeStartOnce sync.Once
	runtimeStopOnce  sync.Once

	droppedMu       sync.RWMutex
	droppedRuntimes map[string]struct{}
}

func NewSQLiteStore(dsn string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	if _, err := db.Exec(`
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA temp_store = MEMORY;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set sqlite pragmas: %w", err)
	}

	s := &SQLiteStore{
		db:              db,
		droppedRuntimes: make(map[string]struct{}),
	}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	s.startRuntimeBatcher()
	return s, nil
}

func (s *SQLiteStore) Close() error {
	s.stopRuntimeBatcher()
	return s.db.Close()
}

func (s *SQLiteStore) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS containers (
  name TEXT PRIMARY KEY,
  sleep_after_seconds INTEGER NOT NULL,
  busy_window_seconds INTEGER NOT NULL,
  inspect_interval_seconds INTEGER NOT NULL,
  min_cpu REAL NOT NULL DEFAULT 0,
  max_cpu REAL NOT NULL DEFAULT 0,
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS container_runtime (
  name TEXT PRIMARY KEY,
  current_ip TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  last_traffic_at DATETIME,
  last_state_change_at DATETIME NOT NULL,
  paused INTEGER NOT NULL DEFAULT 0,
  current_cpu REAL NOT NULL DEFAULT 0,
  cpu_cooldown_until DATETIME,
  updated_at DATETIME NOT NULL,
  FOREIGN KEY(name) REFERENCES containers(name) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS vertical_containers (
  name TEXT PRIMARY KEY,
  min_cpu REAL NOT NULL DEFAULT 0,
  max_cpu REAL NOT NULL DEFAULT 0,
  min_mem_mb INTEGER NOT NULL DEFAULT 0,
  max_mem_mb INTEGER NOT NULL DEFAULT 0,
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS vertical_runtime (
  name TEXT PRIMARY KEY,
  current_cpu REAL NOT NULL DEFAULT 0,
  cpu_cooldown_until DATETIME,
  current_mem_mb INTEGER NOT NULL DEFAULT 0,
  mem_cooldown_until DATETIME,
  updated_at DATETIME NOT NULL,
  FOREIGN KEY(name) REFERENCES vertical_containers(name) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS bandwidth_limits (
  name TEXT PRIMARY KEY,
  egress_mbps REAL NOT NULL DEFAULT 0,
  ingress_mbps REAL NOT NULL DEFAULT 0,
  updated_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS network_bandwidth_limits (
  name TEXT PRIMARY KEY,
  egress_mbps REAL NOT NULL DEFAULT 0,
  ingress_mbps REAL NOT NULL DEFAULT 0,
  updated_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_runtime_status ON container_runtime(status);
CREATE INDEX IF NOT EXISTS idx_runtime_last_traffic ON container_runtime(last_traffic_at);
`)
	if err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	if err := s.ensureColumn(ctx, "containers", "min_cpu", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "containers", "max_cpu", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "container_runtime", "current_cpu", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "container_runtime", "cpu_cooldown_until", "DATETIME"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "vertical_containers", "min_mem_mb", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "vertical_containers", "max_mem_mb", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "vertical_runtime", "current_mem_mb", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "vertical_runtime", "mem_cooldown_until", "DATETIME"); err != nil {
		return err
	}
	return nil
}

func (s *SQLiteStore) ensureColumn(ctx context.Context, table, column, def string) error {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("inspect table %s: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid        int
			name       string
			colType    string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultVal, &pk); err != nil {
			return fmt.Errorf("scan table info %s: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate table info %s: %w", table, err)
	}

	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, def)); err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *SQLiteStore) UpsertContainer(ctx context.Context, cfg model.ContainerConfig) error {
	s.allowRuntime(cfg.Name)

	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO containers (name, sleep_after_seconds, busy_window_seconds, inspect_interval_seconds, min_cpu, max_cpu, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
  sleep_after_seconds=excluded.sleep_after_seconds,
  busy_window_seconds=excluded.busy_window_seconds,
  inspect_interval_seconds=excluded.inspect_interval_seconds,
  min_cpu=excluded.min_cpu,
  max_cpu=excluded.max_cpu,
  updated_at=excluded.updated_at
`, cfg.Name, int64(cfg.SleepAfter/time.Second), int64(cfg.BusyWindow/time.Second), int64(cfg.InspectInterval/time.Second), cfg.MinCPU, cfg.MaxCPU, now, now)
	if err != nil {
		return fmt.Errorf("upsert container: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
INSERT INTO container_runtime (name, status, last_state_change_at, updated_at, current_cpu)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(name) DO NOTHING
`, cfg.Name, model.StatusIdle, now, now, cfg.MinCPU)
	if err != nil {
		return fmt.Errorf("ensure runtime row: %w", err)
	}

	return nil
}

func (s *SQLiteStore) ListContainers(ctx context.Context) ([]model.ContainerInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT
  c.name, c.sleep_after_seconds, c.busy_window_seconds, c.inspect_interval_seconds, c.min_cpu, c.max_cpu,
  r.current_ip, r.status, r.last_traffic_at, r.last_state_change_at, r.paused, r.current_cpu, r.cpu_cooldown_until, r.updated_at
FROM containers c
JOIN container_runtime r ON c.name = r.name
ORDER BY c.name ASC
`)
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	defer rows.Close()

	var out []model.ContainerInfo
	for rows.Next() {
		var (
			name                   string
			sleepAfterSeconds      int64
			busyWindowSeconds      int64
			inspectIntervalSeconds int64
			minCPU                 float64
			maxCPU                 float64
			ip                     string
			status                 string
			lastTrafficNullable    sql.NullTime
			lastStateChange        time.Time
			pausedInt              int
			currentCPU             float64
			cooldownNullable       sql.NullTime
			updatedAt              time.Time
		)
		if err := rows.Scan(
			&name,
			&sleepAfterSeconds,
			&busyWindowSeconds,
			&inspectIntervalSeconds,
			&minCPU,
			&maxCPU,
			&ip,
			&status,
			&lastTrafficNullable,
			&lastStateChange,
			&pausedInt,
			&currentCPU,
			&cooldownNullable,
			&updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan container row: %w", err)
		}

		var lastTraffic *time.Time
		if lastTrafficNullable.Valid {
			t := lastTrafficNullable.Time
			lastTraffic = &t
		}

		out = append(out, model.ContainerInfo{
			Config: model.ContainerConfig{
				Name:            name,
				SleepAfter:      time.Duration(sleepAfterSeconds) * time.Second,
				BusyWindow:      time.Duration(busyWindowSeconds) * time.Second,
				InspectInterval: time.Duration(inspectIntervalSeconds) * time.Second,
				MinCPU:          minCPU,
				MaxCPU:          maxCPU,
			},
			Runtime: model.ContainerRuntime{
				Name:              name,
				CurrentIP:         ip,
				Status:            status,
				LastTrafficAt:     lastTraffic,
				LastStateChangeAt: lastStateChange,
				Paused:            pausedInt == 1,
				CurrentCPU:        currentCPU,
				UpdatedAt:         updatedAt,
			},
		})
		if cooldownNullable.Valid {
			t := cooldownNullable.Time
			out[len(out)-1].Runtime.CPUCooldownUntil = &t
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate container rows: %w", err)
	}

	return out, nil
}

func (s *SQLiteStore) GetContainer(ctx context.Context, name string) (model.ContainerInfo, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT
  c.name, c.sleep_after_seconds, c.busy_window_seconds, c.inspect_interval_seconds, c.min_cpu, c.max_cpu,
  r.current_ip, r.status, r.last_traffic_at, r.last_state_change_at, r.paused, r.current_cpu, r.cpu_cooldown_until, r.updated_at
FROM containers c
JOIN container_runtime r ON c.name = r.name
WHERE c.name = ?
`, name)

	var (
		res                    model.ContainerInfo
		sleepAfterSeconds      int64
		busyWindowSeconds      int64
		inspectIntervalSeconds int64
		minCPU                 float64
		maxCPU                 float64
		lastTrafficNullable    sql.NullTime
		pausedInt              int
		currentCPU             float64
		cooldownNullable       sql.NullTime
	)
	if err := row.Scan(
		&res.Config.Name,
		&sleepAfterSeconds,
		&busyWindowSeconds,
		&inspectIntervalSeconds,
		&minCPU,
		&maxCPU,
		&res.Runtime.CurrentIP,
		&res.Runtime.Status,
		&lastTrafficNullable,
		&res.Runtime.LastStateChangeAt,
		&pausedInt,
		&currentCPU,
		&cooldownNullable,
		&res.Runtime.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.ContainerInfo{}, ErrNotFound
		}
		return model.ContainerInfo{}, fmt.Errorf("get container: %w", err)
	}
	res.Config.SleepAfter = time.Duration(sleepAfterSeconds) * time.Second
	res.Config.BusyWindow = time.Duration(busyWindowSeconds) * time.Second
	res.Config.InspectInterval = time.Duration(inspectIntervalSeconds) * time.Second
	res.Config.MinCPU = minCPU
	res.Config.MaxCPU = maxCPU
	res.Runtime.Name = res.Config.Name
	res.Runtime.Paused = pausedInt == 1
	res.Runtime.CurrentCPU = currentCPU
	if lastTrafficNullable.Valid {
		t := lastTrafficNullable.Time
		res.Runtime.LastTrafficAt = &t
	}
	if cooldownNullable.Valid {
		t := cooldownNullable.Time
		res.Runtime.CPUCooldownUntil = &t
	}

	return res, nil
}

func (s *SQLiteStore) UpdateRuntime(ctx context.Context, rt model.ContainerRuntime) error {
	if rt.Name == "" || s.runtimeDropped(rt.Name) {
		return nil
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
UPDATE container_runtime
SET current_ip=?, status=?, last_traffic_at=?, last_state_change_at=?, paused=?, current_cpu=?, cpu_cooldown_until=?, updated_at=?
WHERE name=?
`, rt.CurrentIP, rt.Status, rt.LastTrafficAt, rt.LastStateChangeAt, boolToInt(rt.Paused), rt.CurrentCPU, rt.CPUCooldownUntil, now, rt.Name)
	if err != nil {
		return fmt.Errorf("update runtime: %w", err)
	}
	return nil
}

func (s *SQLiteStore) QueueRuntimeUpdate(rt model.ContainerRuntime) error {
	s.startRuntimeBatcher()
	if rt.Name == "" || s.runtimeDropped(rt.Name) {
		return nil
	}
	select {
	case s.runtimeQueue <- rt:
		return nil
	default:
		return s.UpdateRuntime(context.Background(), rt)
	}
}

func (s *SQLiteStore) DropRuntime(name string) {
	if name == "" {
		return
	}
	s.dropRuntime(name)
}

func (s *SQLiteStore) startRuntimeBatcher() {
	s.runtimeStartOnce.Do(func() {
		s.runtimeQueue = make(chan model.ContainerRuntime, 1024)
		s.runtimeStop = make(chan struct{})
		s.runtimeDone = make(chan struct{})
		go s.runtimeBatchLoop()
	})
}

func (s *SQLiteStore) stopRuntimeBatcher() {
	if s.runtimeStop == nil {
		return
	}
	s.runtimeStopOnce.Do(func() {
		close(s.runtimeStop)
		<-s.runtimeDone
	})
}

func (s *SQLiteStore) runtimeBatchLoop() {
	const (
		flushInterval = 1 * time.Second
		maxBatchSize  = 100
	)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	pending := make(map[string]model.ContainerRuntime)

	flush := func() {
		if len(pending) == 0 {
			return
		}
		ctx := context.Background()
		now := time.Now().UTC()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			pending = make(map[string]model.ContainerRuntime)
			return
		}
		stmt, err := tx.PrepareContext(ctx, `
UPDATE container_runtime
SET current_ip=?, status=?, last_traffic_at=?, last_state_change_at=?, paused=?, current_cpu=?, cpu_cooldown_until=?, updated_at=?
WHERE name=?
`)
		if err != nil {
			_ = tx.Rollback()
			pending = make(map[string]model.ContainerRuntime)
			return
		}
		for _, rt := range pending {
			_, _ = stmt.ExecContext(ctx,
				rt.CurrentIP,
				rt.Status,
				rt.LastTrafficAt,
				rt.LastStateChangeAt,
				boolToInt(rt.Paused),
				rt.CurrentCPU,
				rt.CPUCooldownUntil,
				now,
				rt.Name,
			)
		}
		_ = stmt.Close()
		_ = tx.Commit()
		pending = make(map[string]model.ContainerRuntime)
	}

		for {
			select {
			case rt := <-s.runtimeQueue:
				if rt.Name == "" || s.runtimeDropped(rt.Name) {
					continue
				}
				pending[rt.Name] = rt
				if len(pending) >= maxBatchSize {
					flush()
			}
		case <-ticker.C:
			flush()
		case <-s.runtimeStop:
			flush()
			close(s.runtimeDone)
			return
		}
	}
}

func (s *SQLiteStore) runtimeDropped(name string) bool {
	s.droppedMu.RLock()
	defer s.droppedMu.RUnlock()
	_, ok := s.droppedRuntimes[name]
	return ok
}

func (s *SQLiteStore) dropRuntime(name string) {
	s.droppedMu.Lock()
	defer s.droppedMu.Unlock()
	s.droppedRuntimes[name] = struct{}{}
}

func (s *SQLiteStore) allowRuntime(name string) {
	if name == "" {
		return
	}
	s.droppedMu.Lock()
	defer s.droppedMu.Unlock()
	delete(s.droppedRuntimes, name)
}

func (s *SQLiteStore) UpsertVertical(ctx context.Context, cfg model.VerticalScaleConfig) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO vertical_containers (name, min_cpu, max_cpu, min_mem_mb, max_mem_mb, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
  min_cpu=excluded.min_cpu,
  max_cpu=excluded.max_cpu,
  min_mem_mb=excluded.min_mem_mb,
  max_mem_mb=excluded.max_mem_mb,
  updated_at=excluded.updated_at
`, cfg.Name, cfg.MinCPU, cfg.MaxCPU, cfg.MinMemMB, cfg.MaxMemMB, now, now)
	if err != nil {
		return fmt.Errorf("upsert vertical: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
INSERT INTO vertical_runtime (name, current_cpu, current_mem_mb, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(name) DO NOTHING
`, cfg.Name, cfg.MinCPU, cfg.MinMemMB, now)
	if err != nil {
		return fmt.Errorf("ensure vertical runtime: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListVertical(ctx context.Context) ([]model.VerticalScaleInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT
  c.name, c.min_cpu, c.max_cpu, c.min_mem_mb, c.max_mem_mb,
  r.current_cpu, r.cpu_cooldown_until, r.current_mem_mb, r.mem_cooldown_until, r.updated_at
FROM vertical_containers c
JOIN vertical_runtime r ON c.name = r.name
ORDER BY c.name ASC
`)
	if err != nil {
		return nil, fmt.Errorf("list vertical: %w", err)
	}
	defer rows.Close()

	var out []model.VerticalScaleInfo
	for rows.Next() {
		var (
			name                string
			minCPU              float64
			maxCPU              float64
			minMemMB            int64
			maxMemMB            int64
			currentCPU          float64
			cooldownNullable    sql.NullTime
			currentMemMB        int64
			memCooldownNullable sql.NullTime
			updatedAt           time.Time
		)
		if err := rows.Scan(&name, &minCPU, &maxCPU, &minMemMB, &maxMemMB, &currentCPU, &cooldownNullable, &currentMemMB, &memCooldownNullable, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan vertical row: %w", err)
		}
		info := model.VerticalScaleInfo{
			Config: model.VerticalScaleConfig{
				Name:     name,
				MinCPU:   minCPU,
				MaxCPU:   maxCPU,
				MinMemMB: minMemMB,
				MaxMemMB: maxMemMB,
			},
			Runtime: model.VerticalScaleRuntime{
				Name:         name,
				CurrentCPU:   currentCPU,
				CurrentMemMB: currentMemMB,
				UpdatedAt:    updatedAt,
			},
		}
		if cooldownNullable.Valid {
			t := cooldownNullable.Time
			info.Runtime.CPUCooldownUntil = &t
		}
		if memCooldownNullable.Valid {
			t := memCooldownNullable.Time
			info.Runtime.MemCooldownUntil = &t
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vertical rows: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) GetVertical(ctx context.Context, name string) (model.VerticalScaleInfo, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT
  c.name, c.min_cpu, c.max_cpu, c.min_mem_mb, c.max_mem_mb,
  r.current_cpu, r.cpu_cooldown_until, r.current_mem_mb, r.mem_cooldown_until, r.updated_at
FROM vertical_containers c
JOIN vertical_runtime r ON c.name = r.name
WHERE c.name = ?
`, name)

	var (
		res                 model.VerticalScaleInfo
		minCPU              float64
		maxCPU              float64
		minMemMB            int64
		maxMemMB            int64
		currentCPU          float64
		cooldownNullable    sql.NullTime
		currentMemMB        int64
		memCooldownNullable sql.NullTime
	)
	if err := row.Scan(&res.Config.Name, &minCPU, &maxCPU, &minMemMB, &maxMemMB, &currentCPU, &cooldownNullable, &currentMemMB, &memCooldownNullable, &res.Runtime.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.VerticalScaleInfo{}, ErrNotFound
		}
		return model.VerticalScaleInfo{}, fmt.Errorf("get vertical: %w", err)
	}
	res.Config.MinCPU = minCPU
	res.Config.MaxCPU = maxCPU
	res.Config.MinMemMB = minMemMB
	res.Config.MaxMemMB = maxMemMB
	res.Runtime.Name = res.Config.Name
	res.Runtime.CurrentCPU = currentCPU
	res.Runtime.CurrentMemMB = currentMemMB
	if cooldownNullable.Valid {
		t := cooldownNullable.Time
		res.Runtime.CPUCooldownUntil = &t
	}
	if memCooldownNullable.Valid {
		t := memCooldownNullable.Time
		res.Runtime.MemCooldownUntil = &t
	}
	return res, nil
}

func (s *SQLiteStore) UpdateVerticalRuntime(ctx context.Context, rt model.VerticalScaleRuntime) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
UPDATE vertical_runtime
SET current_cpu=?, cpu_cooldown_until=?, current_mem_mb=?, mem_cooldown_until=?, updated_at=?
WHERE name=?
`, rt.CurrentCPU, rt.CPUCooldownUntil, rt.CurrentMemMB, rt.MemCooldownUntil, now, rt.Name)
	if err != nil {
		return fmt.Errorf("update vertical runtime: %w", err)
	}
	return nil
}

func (s *SQLiteStore) DeleteVertical(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM vertical_containers WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete vertical: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete vertical: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) DeleteContainer(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM containers WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete container: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete container: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) UpsertBandwidthLimit(ctx context.Context, name string, egress, ingress float64) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO bandwidth_limits (name, egress_mbps, ingress_mbps, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
  egress_mbps=excluded.egress_mbps,
  ingress_mbps=excluded.ingress_mbps,
  updated_at=excluded.updated_at
`, name, egress, ingress, now)
	if err != nil {
		return fmt.Errorf("upsert bandwidth limit: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListBandwidthLimits(ctx context.Context) ([]model.BandwidthLimit, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT name, egress_mbps, ingress_mbps, updated_at
FROM bandwidth_limits
ORDER BY name
`)
	if err != nil {
		return nil, fmt.Errorf("list bandwidth limits: %w", err)
	}
	defer rows.Close()

	var limits []model.BandwidthLimit
	for rows.Next() {
		var limit model.BandwidthLimit
		if err := rows.Scan(&limit.Name, &limit.EgressMbps, &limit.IngressMbps, &limit.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan bandwidth limits: %w", err)
		}
		limits = append(limits, limit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate bandwidth limits: %w", err)
	}
	return limits, nil
}

func (s *SQLiteStore) UpsertNetworkBandwidthLimit(ctx context.Context, name string, egress, ingress float64) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO network_bandwidth_limits (name, egress_mbps, ingress_mbps, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
  egress_mbps=excluded.egress_mbps,
  ingress_mbps=excluded.ingress_mbps,
  updated_at=excluded.updated_at
`, name, egress, ingress, now)
	if err != nil {
		return fmt.Errorf("upsert network bandwidth limit: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListNetworkBandwidthLimits(ctx context.Context) ([]model.NetworkBandwidthLimit, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT name, egress_mbps, ingress_mbps, updated_at
FROM network_bandwidth_limits
ORDER BY name
`)
	if err != nil {
		return nil, fmt.Errorf("list network bandwidth limits: %w", err)
	}
	defer rows.Close()

	var limits []model.NetworkBandwidthLimit
	for rows.Next() {
		var limit model.NetworkBandwidthLimit
		if err := rows.Scan(&limit.Name, &limit.EgressMbps, &limit.IngressMbps, &limit.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan network bandwidth limits: %w", err)
		}
		limits = append(limits, limit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate network bandwidth limits: %w", err)
	}
	return limits, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

var ErrNotFound = errors.New("not found")

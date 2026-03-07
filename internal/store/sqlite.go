package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"hubfly-scale/internal/model"
)

type SQLiteStore struct {
	db *sql.DB
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

	s := &SQLiteStore{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS containers (
  name TEXT PRIMARY KEY,
  sleep_after_seconds INTEGER NOT NULL,
  busy_window_seconds INTEGER NOT NULL,
  inspect_interval_seconds INTEGER NOT NULL,
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
  updated_at DATETIME NOT NULL,
  FOREIGN KEY(name) REFERENCES containers(name) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_runtime_status ON container_runtime(status);
CREATE INDEX IF NOT EXISTS idx_runtime_last_traffic ON container_runtime(last_traffic_at);
`)
	if err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	return nil
}

func (s *SQLiteStore) UpsertContainer(ctx context.Context, cfg model.ContainerConfig) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO containers (name, sleep_after_seconds, busy_window_seconds, inspect_interval_seconds, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
  sleep_after_seconds=excluded.sleep_after_seconds,
  busy_window_seconds=excluded.busy_window_seconds,
  inspect_interval_seconds=excluded.inspect_interval_seconds,
  updated_at=excluded.updated_at
`, cfg.Name, int64(cfg.SleepAfter/time.Second), int64(cfg.BusyWindow/time.Second), int64(cfg.InspectInterval/time.Second), now, now)
	if err != nil {
		return fmt.Errorf("upsert container: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
INSERT INTO container_runtime (name, status, last_state_change_at, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(name) DO NOTHING
`, cfg.Name, model.StatusIdle, now, now)
	if err != nil {
		return fmt.Errorf("ensure runtime row: %w", err)
	}

	return nil
}

func (s *SQLiteStore) ListContainers(ctx context.Context) ([]model.ContainerInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT
  c.name, c.sleep_after_seconds, c.busy_window_seconds, c.inspect_interval_seconds,
  r.current_ip, r.status, r.last_traffic_at, r.last_state_change_at, r.paused, r.updated_at
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
			ip                     string
			status                 string
			lastTrafficNullable    sql.NullTime
			lastStateChange        time.Time
			pausedInt              int
			updatedAt              time.Time
		)
		if err := rows.Scan(
			&name,
			&sleepAfterSeconds,
			&busyWindowSeconds,
			&inspectIntervalSeconds,
			&ip,
			&status,
			&lastTrafficNullable,
			&lastStateChange,
			&pausedInt,
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
			},
			Runtime: model.ContainerRuntime{
				Name:              name,
				CurrentIP:         ip,
				Status:            status,
				LastTrafficAt:     lastTraffic,
				LastStateChangeAt: lastStateChange,
				Paused:            pausedInt == 1,
				UpdatedAt:         updatedAt,
			},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate container rows: %w", err)
	}

	return out, nil
}

func (s *SQLiteStore) GetContainer(ctx context.Context, name string) (model.ContainerInfo, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT
  c.name, c.sleep_after_seconds, c.busy_window_seconds, c.inspect_interval_seconds,
  r.current_ip, r.status, r.last_traffic_at, r.last_state_change_at, r.paused, r.updated_at
FROM containers c
JOIN container_runtime r ON c.name = r.name
WHERE c.name = ?
`, name)

	var (
		res                    model.ContainerInfo
		sleepAfterSeconds      int64
		busyWindowSeconds      int64
		inspectIntervalSeconds int64
		lastTrafficNullable    sql.NullTime
		pausedInt              int
	)
	if err := row.Scan(
		&res.Config.Name,
		&sleepAfterSeconds,
		&busyWindowSeconds,
		&inspectIntervalSeconds,
		&res.Runtime.CurrentIP,
		&res.Runtime.Status,
		&lastTrafficNullable,
		&res.Runtime.LastStateChangeAt,
		&pausedInt,
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
	res.Runtime.Name = res.Config.Name
	res.Runtime.Paused = pausedInt == 1
	if lastTrafficNullable.Valid {
		t := lastTrafficNullable.Time
		res.Runtime.LastTrafficAt = &t
	}

	return res, nil
}

func (s *SQLiteStore) UpdateRuntime(ctx context.Context, rt model.ContainerRuntime) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
UPDATE container_runtime
SET current_ip=?, status=?, last_traffic_at=?, last_state_change_at=?, paused=?, updated_at=?
WHERE name=?
`, rt.CurrentIP, rt.Status, rt.LastTrafficAt, rt.LastStateChangeAt, boolToInt(rt.Paused), now, rt.Name)
	if err != nil {
		return fmt.Errorf("update runtime: %w", err)
	}
	return nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

var ErrNotFound = errors.New("not found")

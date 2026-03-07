# Progress

## 2026-03-07

### Completed
- Bootstrapped Go service structure with clear modules for API, scaler manager, docker client, traffic watcher, and SQLite store.
- Implemented container registration API (`POST /v1/containers`) with persisted config and controller startup/restart.
- Implemented status endpoints (`GET /v1/containers`, `GET /v1/containers/{name}`) and health endpoint (`GET /healthz`).
- Added SQLite schema and performance pragmas (WAL, NORMAL sync, memory temp store, busy timeout).
- Implemented controller loop for each container:
  - Tracks latest container IP via periodic docker inspect.
  - Starts/restarts `tcpdump` watcher bound to current container IP.
  - Marks runtime state (`busy`, `idle`, `sleeping`, `error`).
  - Pauses idle containers using Docker pause after configured inactivity.
  - Unpauses on detected traffic.
- Added initial unit tests for state-transition logic.
- Added graceful shutdown and startup loading of existing registered containers.

### Notes
- Current design intentionally isolates per-container controllers to simplify extension toward multi-container scaling strategies later.

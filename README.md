# hubfly-scale

`hubfly-scale` is a lightweight Go service that watches container traffic and automatically puts idle containers to sleep with `docker pause`, then wakes them with `docker unpause` when traffic appears again.

Current scope is **single-container runtime control** (scale-to-zero behavior via pause/unpause), designed with modular internals so future multi-replica scaling can be added.

## How It Works

For each registered container:

1. The service stores config + runtime state in SQLite.
2. It inspects Docker periodically to keep the latest container IP.
3. It starts `tcpdump` with a host filter for that IP.
4. Incoming traffic marks the container as `busy`.
5. If no traffic is seen for `sleep_after_seconds`, the container is paused.
6. On traffic detection while paused, the container is unpaused.

Runtime states:
- `busy`
- `idle`
- `sleeping`
- `error`

## Architecture

- API: `internal/api`
- Scaler manager/controller: `internal/scaler`
- Traffic watcher (`tcpdump`): `internal/traffic`
- Docker integration (`docker` CLI): `internal/docker`
- SQLite storage: `internal/store`

## Requirements

- Go `1.25+`
- Docker installed and running
- `tcpdump` installed
- Permissions to:
  - run `docker` commands
  - run `tcpdump` on host interfaces (often root or `CAP_NET_RAW/CAP_NET_ADMIN`)

## Run

```bash
go run ./cmd/hubfly-scale
```

Optional env vars:

- `HF_SCALE_ADDR` (default `:10006`)
- `HF_SCALE_DB` (default `./data/hubfly-scale.db`)

Example:

```bash
HF_SCALE_ADDR=":10006" HF_SCALE_DB="./data/hubfly-scale.db" go run ./cmd/hubfly-scale
```

Print binary version:

```bash
hubfly-scale version
```

Output is only the version string (for releases, this is the git tag).

## Release Artifact

GitHub Actions workflow: `.github/workflows/release-linux.yml`

On tag push matching `v*`, it:
- builds Linux binary `hubfly-scale`
- injects version from the tag into the binary
- creates `hubfly-scale.zip` with the binary at zip root
- uploads `hubfly-scale.zip` as the GitHub release asset

## API

### Health

```bash
curl -s http://localhost:10006/healthz
```

### Register/Update a container scaler config

```bash
curl -s -X POST http://localhost:10006/v1/containers \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "my-app",
    "sleep_after_seconds": 30,
    "busy_window_seconds": 2,
    "inspect_interval_seconds": 5
  }'
```

Fields:
- `name`: Docker container name (required)
- `sleep_after_seconds`: inactivity before pause (default `60`)
- `busy_window_seconds`: traffic freshness window to mark `busy` (default `2`)
- `inspect_interval_seconds`: interval to refresh IP/paused state (default `5`)
- Vertical CPU scaling is configured separately (see below).

### Vertical CPU Scaling (Fixed Policy)

Register a container for vertical CPU scaling (separate from sleep registration):

```bash
curl -s -X POST http://localhost:10006/v1/vertical/containers \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "my-app",
    "min_cpu": 0.5,
    "max_cpu": 2.0
  }'
```

List vertical scaling configs:

```bash
curl -s http://localhost:10006/v1/vertical/containers | jq
```

Remove a vertical scaling config:

```bash
curl -s -X DELETE http://localhost:10006/v1/vertical/containers/my-app
```

Policy:
- poll CPU stats every 5s
- scale up by `+0.2` cores when usage is ≥80% for 10 of the last 12 samples (≈60s)
- scale down by `-0.2` cores when usage is ≤40% for 10 of the last 12 samples (≈60s)
- enforce a 5-minute cooldown after any CPU change
- never go below `min_cpu` or above `max_cpu`

### List all managed containers

```bash
curl -s http://localhost:10006/v1/containers | jq
```

### Get one managed container

```bash
curl -s http://localhost:10006/v1/containers/my-app | jq
```

### Reload a container controller

```bash
curl -s -X POST http://localhost:10006/v1/containers/my-app/reload
```

### Unregister a container

```bash
curl -s -X DELETE http://localhost:10006/v1/containers/my-app
```

### Update container bandwidth limits (egress/ingress)

```bash
curl -s -X POST http://localhost:10006/v1/containers/my-app/bandwidth \
  -H 'Content-Type: application/json' \
  -d '{
    "egress_mbps": 10,
    "ingress_mbps": 5
  }'
```

Notes:
- Requires host privileges to run `tc`, `ip`, and `nsenter`.
- Egress and ingress are optional; at least one must be > 0.
- Ingress shaping uses a per-container IFB device (`ifb<ifindex>`).
- This endpoint does not require prior container registration.

### Update network bandwidth limits (shared across containers)

```bash
curl -s -X POST http://localhost:10006/v1/networks/my-project/bandwidth \
  -H 'Content-Type: application/json' \
  -d '{
    "egress_mbps": 50,
    "ingress_mbps": 50
  }'
```

Notes:
- Applies a shared cap on the network bridge for all containers in that network.
- Uses HTB + fq_codel (or sfq fallback) for fairness.
- This cap applies to total bridge egress (shared up/down pool).

## Quick End-to-End Test with Curl

Use a container with an open port (example: nginx on `8081`).

1. Start test container:

```bash
docker run -d --name my-app -p 8081:80 nginx:alpine
```

2. Start `hubfly-scale` (in another terminal):

```bash
go run ./cmd/hubfly-scale
```

3. Register container with short sleep timeout:

```bash
curl -s -X POST http://localhost:10006/v1/containers \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "my-app",
    "sleep_after_seconds": 10,
    "busy_window_seconds": 2,
    "inspect_interval_seconds": 3
  }' | jq
```

4. Generate traffic:

```bash
curl -s http://localhost:8081 > /dev/null
curl -s http://localhost:8081 > /dev/null
```

5. Check runtime state:

```bash
curl -s http://localhost:10006/v1/containers/my-app | jq
```

6. Wait >10s without traffic, then verify paused:

```bash
docker inspect -f '{{.State.Paused}}' my-app
curl -s http://localhost:10006/v1/containers/my-app | jq
```

7. Send traffic again and verify it wakes:

```bash
curl -s http://localhost:8081 > /dev/null
sleep 1
docker inspect -f '{{.State.Paused}}' my-app
curl -s http://localhost:10006/v1/containers/my-app | jq
```

## Notes and Limitations

- This version focuses on a **single container runtime unit** (no replica orchestration yet).
- `tcpdump` behavior depends on host/network setup and required privileges.
- If Docker inspections fail 3 consecutive times for a container, it is automatically unregistered.
- For production use, add API authentication and integration tests (tracked in `todo.md`).

## Development

Run tests:

```bash
go test ./...
```

Build:

```bash
go build ./...
```

#!/usr/bin/env bash
set -euo pipefail

# Usage:
#   SERVER=http://localhost:10006 NAME=vscale-test ./scripts/test-vscale.sh

SERVER="${SERVER:-http://localhost:10006}"
NAME="${NAME:-vscale-test}"
MIN_CPU="${MIN_CPU:-0.5}"
MAX_CPU="${MAX_CPU:-2.0}"
MIN_MEM_MB="${MIN_MEM_MB:-256}"
MAX_MEM_MB="${MAX_MEM_MB:-2048}"
IMAGE="${IMAGE:-python:3.12-alpine}"

mem_stress_mb() {
  # Target ~90% of min memory to reliably trip the 85% threshold.
  local target
  target=$((MIN_MEM_MB * 90 / 100))
  if [ "$target" -lt 64 ]; then
    target=64
  fi
  echo "$target"
}

echo "==> Starting test container: $NAME"
docker rm -f "$NAME" >/dev/null 2>&1 || true

# Use Python image so we can generate CPU/memory load without apk.
docker run -d --name "$NAME" \
  --memory "${MIN_MEM_MB}m" \
  --cpus "${MIN_CPU}" \
  "$IMAGE" sh -c "sleep infinity"

sleep 2
if ! docker ps --format '{{.Names}}' | grep -qx "$NAME"; then
  echo "Container failed to start. Check docker logs $NAME"
  exit 1
fi

echo "==> Registering vertical scaling"
curl -s -X POST "$SERVER/v1/vertical/containers" \
  -H 'Content-Type: application/json' \
  -d "{
    \"name\": \"$NAME\",
    \"min_cpu\": $MIN_CPU,
    \"max_cpu\": $MAX_CPU,
    \"min_mem_mb\": $MIN_MEM_MB,
    \"max_mem_mb\": $MAX_MEM_MB
  }" | sed 's/.*/&/g'

echo
echo "==> Baseline runtime"
curl -s "$SERVER/v1/vertical/containers/$NAME" | sed 's/.*/&/g'

echo
echo "==> Start CPU + memory stress (should trigger scale up in ~60s)"
MEM_STRESS_MB="$(mem_stress_mb)"
docker exec -d "$NAME" sh -c "python - <<'PY' >/dev/null 2>&1
import multiprocessing as mp
import time
def burn():
    while True:
        pass
procs = [mp.Process(target=burn) for _ in range(2)]
for p in procs:
    p.start()
time.sleep(180)
PY"
docker exec -d "$NAME" sh -c "MEM_MB=${MEM_STRESS_MB} python - <<'PY' >/dev/null 2>&1
import os, time
mb = int(os.environ.get('MEM_MB', '128'))
buf = [b'x' * 1024 * 1024 for _ in range(mb)]
time.sleep(180)
PY"

echo "==> Watching for scale-up (polling runtime + docker limits)"
for i in $(seq 1 24); do
  echo "--- $(date -Is)"
  curl -s "$SERVER/v1/vertical/containers/$NAME" | sed 's/.*/&/g'
  docker inspect -f 'cpu={{.HostConfig.NanoCpus}} mem={{.HostConfig.Memory}}' "$NAME" || true
  sleep 5
done

echo
echo "==> Stop stress"
docker exec "$NAME" pkill -f python >/dev/null 2>&1 || true

echo
echo "==> Scale-down policy is slow by design (p95 <= 50% for ~30 minutes)."
echo "    Keep the container idle and watch the runtime over time:"
echo "    curl -s $SERVER/v1/vertical/containers/$NAME"

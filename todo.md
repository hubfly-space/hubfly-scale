# TODO

## High Priority
- Add authenticated API access (token or mTLS) before production usage.
- Add explicit manual wake/sleep API actions.
- Improve traffic path for paused containers with a deterministic wake trigger strategy if host networking behavior varies.
- Add integration tests (docker + tcpdump) for end-to-end pause/unpause lifecycle.

## Medium Priority
- Introduce batched/transactional runtime updates to reduce write amplification at high packet rates.
- Replace shell-based docker/tcpdump calls with SDK-backed implementations where beneficial.
- Add structured logging fields and log level controls.
- Add runtime metrics endpoint (Prometheus) for CPU, event rates, pause/unpause counts, and watcher errors.
- Add config validation and per-container advanced options (network interface, BPF filter overrides).

## Future (Scale-out)
- Add a scheduler layer for N replicas instead of 1 paused/running container.
- Add policy engine for scale-up/down rules across multiple containers.
- Add distributed state backend option for multi-node deployments.

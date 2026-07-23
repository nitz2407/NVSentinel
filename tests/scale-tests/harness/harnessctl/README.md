# harnessctl

The NVSentinel scale-test harness controller: **one Go binary** that replaces the
fragile bash orchestration with `client-go` (typed API access, informer-based
waits, dynamic CR handling) and emits structured JSON/JUnit results for
unattended, per-release runs (requirements goal G7).

The same binary runs in two roles:

- **Operator CLI** — `preflight`, `scale-nodes`, `teardown-nodes`, `janitor-check`,
  `sim-reboot`, `phase0`.
- **In-cluster Job image** — `inject`, `reconcile` (launched as Jobs by `phase0`).

Helm installs stay as thin shell wrappers (`../phase0/10|20|30-install-*.sh`);
rewriting `helm install` in Go buys no robustness.

> **Scope:** these commands cover **§9 / Phase 0 only**. Phase 1
> (`MB-*`, §10) and Phase 2 (`SYS-*`, §11) subcommands are not implemented yet —
> see the status table in `../README.md`.

## Build

```bash
go mod tidy
CGO_ENABLED=0 go build -o harnessctl .
docker build -t "$HARNESS_IMAGE" .
docker push "$HARNESS_IMAGE"
```

## Commands

| Command | Phase | What it does |
|---------|-------|--------------|
| `preflight` | P0.1 | verify cluster reachability + node inventory |
| `bringup [-dir ...]` | P0.1 | run the helm install scripts (monitoring + KWOK + NVSentinel) |
| `scale-nodes [-count N]` | P0.2 | create GPU-shaped KWOK nodes, informer-wait Ready, record ceiling (+ apiserver p99) |
| `teardown-nodes` | — | delete all `type=kwok` nodes |
| `inject [...]` | P0.3 | (in Job) attribute events to KWOK nodes, stamp correlation id, write ledger |
| `reconcile [...]` | P0.3 | (in Job) account every injected id vs the datastore, emit report |
| `janitor-check` | P0.4 | create RebootNode + GPUReset CRs, cycle bootID, verify completion |
| `sim-reboot -node N` | — | simulate a reboot (NotReady → Ready + fresh bootID) |
| `phase0 [--only ...] [--install-dir ...]` | all | run the full acceptance suite; structured results in `results/` |

## Run

```bash
# source the shared config so env-based defaults apply
set -a; source ../config/harness.env; set +a
export HARNESS_IMAGE=myregistry/nvsentinel-harness/harnessctl:v1
export HARNESS_MONGO_URI='mongodb://root:PASS@mongodb-store-headless.nvsentinel.svc:27017/?replicaSet=rs0&authSource=admin'

./harnessctl preflight
./harnessctl bringup                # helm install monitoring + KWOK + NVSentinel
./harnessctl scale-nodes --count 20000
./harnessctl phase0                 # nodes + inject/reconcile + janitor
./harnessctl phase0 --only inject
./harnessctl phase0 --install-dir ../phase0   # also run the helm install scripts first
```

## Configuration

All defaults come from environment variables (same names as
`../config/harness.env`); per-command flags override them. Key ones:
`KWOK_NODE_COUNT`, `KWOK_NODE_PREFIX`, `HARNESS_IMAGE`, `HARNESS_MONGO_URI`,
`P03_EVENT_COUNT`, `P03_EVENT_RATE`, `MAX_APISERVER_P99_SECONDS`,
`NODE_READY_TIMEOUT`, `HARNESS_RESULTS_DIR`.

## Notes / assumptions

- Prometheus is queried through the **API server service proxy** (no
  port-forward). Override `PROM_SERVICE` / `PROM_PORT` / `MONITORING_NAMESPACE`.
- `scale-nodes` sets node capacity/allocatable via the status subresource; KWOK
  maintains the Ready condition and lease.
- CRs are created via the **dynamic client** (`janitor.dgxc.nvidia.com/v1alpha1`),
  so no dependency on the janitor Go module.
- **PostgreSQL is not yet supported** by `reconcile` (tracked Phase-0 gap).

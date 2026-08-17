# harnessctl

The NVSentinel scale-test harness controller: **one Go binary** that replaces the
fragile bash orchestration with `client-go` (typed API access, informer-based
waits, dynamic CR handling) and emits structured JSON/JUnit results for
unattended, per-release runs (requirements goal G7).

The same tooling runs in two roles:

- **Operator CLI (`harnessctl`)** — `stack bringup/cleanup/report`, `nodes scale/ceiling`,
  `janitor check`, `pool create/…` on your laptop.
- **In-cluster injector (`harness-inject`)** — slim binary in a multi-arch image
  (`--injector-image`, default `ghcr.io/nvidia/nvsentinel/harness-inject`);
  inject/reconcile primitives only. `pool create` deploys it; the host CLI execs
  into those pods via client-go remotecommand.

Helm installs for `stack bringup` use the **Helm Go SDK** with **embedded** values
under `assets/` (`go:embed`). Optional chart-specific overlays are merged on top
(Helm overlay semantics) — e.g. Kind smoke:
`stack bringup --nvsentinel-values ../nvsentinel/values-harness-kind.yaml --monitoring-values ../monitoring/values-kind.yaml`.
Manifest apply, logs, rollout restart, and metrics use **client-go** (no
`helm`/`kubectl` on `PATH`). The binary is self-contained for bringup when no
overlays are passed.

> **Scope:** these commands cover **§9 / Phase 0 only**. Phase 1
> (`MB-*`, §10) and Phase 2 (`SYS-*`, §11) subcommands are not implemented yet —
> see the status table in `../README.md`.

## Build

```bash
# Operator CLI (full binary)
go mod tidy
CGO_ENABLED=0 go build -o harnessctl .

# Slim in-cluster injector (inject + reconcile only)
CGO_ENABLED=0 go build -tags injector -o harness-inject .

# Injector image (run from NVSentinel repo root)
docker buildx build -f tests/scale-tests/harness/harnessctl/Dockerfile \
  --platform linux/amd64,linux/arm64 -t "$HARNESS_INJECT_IMAGE" --push .
```

## Commands

AWS-CLI-style noun-verb: `harnessctl <group> <command> [--flags]`. Run
`harnessctl`, `harnessctl <group>`, or `harnessctl <group> <command> -h` for help.

| Command | Phase | What it does |
|---------|-------|--------------|
| `stack bringup [--nvsentinel-values … --monitoring-values … --nvs-chart-version … --kwok-version … --cert-manager-version … --metrics-server-version … --kps-chart-version …]` | P0.1 | install only what is missing (or version-mismatched); embedded values + optional per-chart overlays; non-interactive |
| `stack cleanup [--pool]` | — | delete `type=kwok` nodes + orphaned janitor CRs (+ optional harness-owned pool only; never `platform-connectors`) |
| `stack report [--title … --window …]` | — | collect latency/throughput/resource/CR/mongo metrics → `report.md`/`report.json` |
| `nodes scale --count N` | P0.2 | create GPU-shaped KWOK nodes, informer-wait Ready, record ceiling (+ apiserver p99) |
| `nodes ceiling [--start --step --max]` | P0.2 | ramp node count until degradation and attribute it |
| `events inject [--pattern --fatal-event --mechanism …]` | P0.3 | fire every resident injector, attribute events to KWOK nodes, stamp correlation id |
| `events reconcile --run-id ID [--direct]` | P0.3 | account every injected id vs the datastore, emit report |
| `events coldstart [--count --remediation-ratio …]` | — | seed a MongoDB haystack, cold-start a consumer, measure initial scan time |
| `janitor check` | P0.4 | create RebootNode + GPUReset CRs, cycle bootID, verify completion |
| `pool create \| teardown \| startup-burst \| connection-sweep` | P0.5 | stage harness pool (`nvs-harness-*`) + injectors; teardown/cleanup never touch live platform connectors |

Legacy single-token names (`bringup`, `scale-nodes`, `inject`, …) still work as
hidden aliases during the transition.

## Run

```bash
CGO_ENABLED=0 go build -o harnessctl .
BIN=./harnessctl

# All inputs are flags — no env file. (config/harness.env is read only by the
# install *scripts* that `stack bringup` wraps.)
$BIN stack bringup --nvs-chart-version v1.16.0
$BIN nodes scale   --count 20000 --provider-id-scheme kwok --results-dir ./results
$BIN pool create   --per-node-pod-limit 10 --results-dir ./results
$BIN janitor check --results-dir ./results
RID=$($BIN events inject --fatal-fraction 0.08 --results-dir ./results | tail -1)
$BIN events reconcile --run-id "$RID" --results-dir ./results
$BIN stack report --title "scale 20k" --window 1h --results-dir ./results
```

## Configuration

**Flags only — `harnessctl` reads no config/env file.** Every input is a
`--kebab-case` flag; `-h` on any command lists them. Flags are **scoped per
command** — each subcommand registers only the flags it actually reads (via small
composable binders in `config.go`), so e.g. `janitor check -h` shows just
`--results-dir`/`--action-timeout`/`--job-complete-delay`, and the version-aware
targets appear only on `stack bringup`. The rough grouping:

| Group | Flags | Used by |
|-------|-------|---------|
| namespaces | `--nvs-namespace`, `--monitoring-namespace`, `--kwok-namespace`, `--janitor-namespace`, `--cert-manager-namespace` | whichever commands touch that namespace |
| results | `--results-dir` | every command that writes artifacts (all except `stack bringup`/`stack cleanup`) |
| prometheus | `--monitoring-namespace`, `--prom-service`, `--prom-port` | `nodes scale`/`nodes ceiling`, `stack report`, `pool` sweeps |
| mongo | `--mongo-service`/`--mongo-replica-set`/`--mongo-port`/`--mongo-tls-secret`/`--mongo-root-secret` | `events inject`/`reconcile`/`coldstart` |
| node guardrails | `--max-apiserver-p99`, `--node-ready-timeout`, `--max-cluster-cpu-pct`, `--max-cluster-mem-pct` | `nodes scale`/`nodes ceiling` |
| node shape | `--node-prefix`, `--gpu-count`, `--node-cpu`, `--node-memory`, `--node-max-pods`, `--node-batch`, `--provider-id-scheme` | `nodes scale`/`nodes ceiling` |
| version targets | `--nvs-chart`, `--nvs-chart-version`, `--kwok-version`, `--cert-manager-version`, `--metrics-server-version`, `--kps-chart-version` | `stack bringup` only |
| injector | `--injector-image` | `pool create` + `events inject`/`reconcile` — slim multi-arch `ghcr.io/nvidia/nvsentinel/harness-inject` (inject/reconcile primitives only) |

`stack bringup` is fully declarative and non-interactive: a component that is
already present at the target version is skipped; anything missing (or, for
NVSentinel/KWOK/cert-manager/metrics-server, running a different image tag than
the `--*-version` target) is installed/upgraded via the Go installers.
Leaving a version flag empty means "accept whatever is installed" for detection;
when an install is required, baked-in defaults matching `config/harness.env` are
used (`v1.16.0` / `v0.6.1` / `v1.16.2` / `v0.7.2` / KPS `65.5.0`).
kube-prometheus-stack is presence-only for detection (chart version, not image tag).

Two internal-only exceptions still read an env var (never part of the flag surface,
no `harness.env` entry): a few poll-interval tuning knobs (`P03_DRAIN_*`,
`HARNESS_MONITOR_*`, `NODE_READY_STALL_SECONDS`) and `MONGO_URI`, which the
distributed orchestrator sets inside injector pods so mTLS credentials stay off the
command line.

## Notes / assumptions

- Prometheus is queried through the **API server service proxy** (no
  port-forward). Override `--prom-service` / `--prom-port` / `--monitoring-namespace`.
- `scale-nodes` sets node capacity/allocatable via the status subresource; KWOK
  maintains the Ready condition and lease.
- CRs are created via the **dynamic client** (`janitor.dgxc.nvidia.com/v1alpha1`),
  so no dependency on the janitor Go module.
- **PostgreSQL is not yet supported** by `reconcile` (tracked Phase-0 gap).

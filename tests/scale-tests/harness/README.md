# NVSentinel Scale-Test Harness

Next-generation scale testing per **NVSentinel Scale Test Requirements**: a
two-phase, KWOK-based, reproducible suite that replaces the one-shot 1,500-real-node
campaign under `../` (which stays as historical reference).

- **Phase 0 – Harness** (this milestone): prove the harness itself before any
  benchmark counts.
- **Phase 1 – Microbenchmarks** (`MB-*`): per-component baselines + config cost.
- **Phase 2 – System Scale** (`SYS-*`): full pipeline at the node ceiling.

## Implementation status

> **Phase 0 (P0.1–P0.5) is implemented.** Phase 1 and Phase 2 are not built yet.

| Requirement | Scope | Status |
|-------------|-------|--------|
| **§9 Phase 0 – Harness** (P0.1–P0.5) | idempotent bring-up, node ceiling, event-ID reconciliation, janitor action path, connector-pool | ✅ implemented (`harnessctl`) |
| **§10 Phase 1 – Microbenchmarks** | load benchmarks `MB-1…MB-7` + config-cost `MB-C1…MB-C5` | ⛔ TODO |
| **§11 Phase 2 – System Scale** | full pipeline at 50k nodes / ~4,000 ev/s, mass-failure profiles `SYS-*` | ⛔ TODO |

Phase 1/2 additionally require shared measurement plumbing not yet present:
per-event latency tracking (V1), warmup + N≥3 repetitions with median/min-max
(V2/V3), per-component resource + API-server attribution collection (O3), and
QoS/pinning of the component under test (V4). These are expected to land as new
`harnessctl` subcommands (e.g. `mb`, `sys`) on top of a shared `internal/measure`
package that reuses the Phase 0 injector, node scaler, and Prometheus helper.

## Design: one self-contained Go CLI

Orchestration, Helm bring-up, node scaling, waiters, CR handling, event injection
and reconciliation all live in a single operator binary, **`harnessctl`**
(`./harnessctl/`), built on `client-go` and the **Helm Go SDK**. Values and KWOK
stages are **embedded** (`harnessctl/assets/`), so `stack bringup` needs no
`helm`/`kubectl` on `PATH` and does not invoke `phase0/*.sh`.

The same tooling runs in two roles:

- **Operator CLI (`harnessctl`)** — `stack`, `nodes`, `events`, `janitor`, `pool`
  on your laptop. All inputs are command-line flags (no env file).
- **In-cluster injector (`harness-inject`)** — slim multi-arch image
  (`--injector-image`, default `ghcr.io/nvidia/nvsentinel/harness-inject:latest`).
  `pool create` deploys it; the host CLI execs inject/reconcile primitives into
  those pods via client-go remotecommand.

The sibling `phase0/*.sh` scripts remain as a manual/reference path; they are
not used by `harnessctl`.

Command form is AWS-CLI-style noun-verb: `harnessctl <group> <command> [--flags]`.
Legacy single-token names (`bringup`, `scale-nodes`, `inject`, …) still work as
hidden aliases. Full flag lists live in [`harnessctl/README.md`](harnessctl/README.md)
and `harnessctl <group> <command> -h`.

## Layout

```
harness/
├── config/harness.env           # tunables for phase0/*.sh only (not read by harnessctl)
├── lib/common.sh                # shared bash helpers for the install scripts
├── monitoring/                  # kube-prometheus-stack values (KWOK-scale + Kind overlay)
├── kwok/stages-custom.yaml      # custom KWOK stages (also embedded in harnessctl/assets/)
├── nvsentinel/
│   ├── values-harness.yaml      # KWOK-scale NVSentinel values (embedded for bringup)
│   └── values-harness-kind.yaml # slim overlay for Kind smoke
├── kind/nvs-harness-kind.yaml   # Kind cluster: schedulable CP + GPU labels
├── phase0/                      # optional/manual helm install scripts (bringup is in Go)
│   ├── 10-install-monitoring.sh
│   ├── 15-install-metrics-server.sh
│   ├── 20-install-kwok.sh
│   ├── 25-install-cert-manager.sh
│   ├── 30-install-nvsentinel.sh
│   └── 40-parallel-inject.sh    # legacy bash inject path; prefer `events inject`
├── harnessctl/                  # the Go CLI (see harnessctl/README.md)
│   ├── assets/                  # embedded Helm values + KWOK stages
│   ├── Dockerfile               # slim multi-arch harness-inject image
│   └── …
├── run_e2e.sh                   # Phase 0 E2E driver for one node count
├── findings/                    # scale-attributable bugs found during harness runs
└── results/                     # generated artifacts (git-ignored)
```

## Phase 0 acceptance criteria

| Check | Requirement | Command |
|-------|-------------|---------|
| **P0.1** | Scripted, idempotent bring-up (monitoring + metrics-server + KWOK + cert-manager + NVSentinel) | `harnessctl stack bringup` |
| **P0.2** | GPU-shaped KWOK nodes Ready; API server within bounds; ceiling recorded | `harnessctl nodes scale --count N` (or `nodes ceiling` to ramp) |
| **P0.3** | Events attributed to KWOK node names, every event ID reconciled | `harnessctl events inject` then `events reconcile --run-id …` |
| **P0.4** | RebootNode Job completes + node cycles bootID; GPUReset Job completes | `harnessctl janitor check` |
| **P0.5** | Harness-owned connector pool + resident injectors staged on real nodes | `harnessctl pool create` |

`stack bringup` installs only what is missing or version-mismatched. Monitoring
and metrics-server are optional (a failure is warned-and-skipped); KWOK,
cert-manager, and NVSentinel are required. The janitor ships inside the
NVSentinel chart (not a separate install).

## Prerequisites

- A Kubernetes cluster reachable via your kubeconfig. Scale runs assume roughly
  ~10 CPU/GPU worker nodes (16 vCPU / 32 GB class). Kind is for **functional
  smoke only** — see [Kind smoke test](#kind-smoke-test).
- **Go 1.26** (`go.mod` pins `go 1.26.0` / toolchain `go1.26.3`) to build
  `harnessctl`. Helm and kubectl are **not** required on `PATH` for the Go CLI
  (Helm Go SDK + client-go). They are still needed if you run `phase0/*.sh` by
  hand, and kubectl is useful for debugging.
- **Docker** (and `docker buildx` for a multi-arch injector image). Kind also
  needs a working Docker daemon.
- Cluster pull access to an injector image: either the default
  `ghcr.io/nvidia/nvsentinel/harness-inject:latest`, or an image you
  [build and push](#build-and-push-the-injector-image) and pass with
  `--injector-image`.
- On **managed clusters** (AKS/EKS/GKE), pass `--provider-id-scheme kwok` to
  `nodes scale` / `nodes ceiling`. Without a synthetic `spec.providerID`, the
  cloud node-lifecycle controller silently deletes KWOK nodes.

### Bare / from-scratch clusters (e.g. Kind)

Managed clusters (AKS/EKS/OKE) already satisfy most of these. A bare cluster
does not, and `bringup` installs NVSentinel with Helm `--wait`, so an unmet
item leaves a pod never-Ready and the install eventually times out. For a
full Kind walkthrough see [Kind smoke test](#kind-smoke-test).

1. **At least one node labeled `nvidia.com/gpu.present=true` must exist**
   *(manual on a bare cluster; Kind config already stamps it)*.
   `fault-quarantine`'s startup circuit breaker uses the count of GPU-labeled
   nodes as its denominator. With zero such nodes it exits fatally at startup
   and crashloops — the log says `GetTotalNodes returning 0 … NodeInformer cache
   sync issues`, which is misleading: it is the deterministic "no GPU nodes"
   branch, not a cache race. `nodes scale` stamps this label on every KWOK node,
   so it is a non-issue at scale; a bare cluster needs one **before** `bringup`.

   Preferred: use `kind/nvs-harness-kind.yaml`, which labels both nodes and
   drops the control-plane taint so the stack can schedule. Otherwise label an
   existing worker:

   ```bash
   kubectl label node <worker> nvidia.com/gpu.present=true --overwrite
   ```

2. **`event-exporter-oidc-secret`** *(handled automatically)*. event-exporter
   mounts this Secret as a required (`optional:false`) volume, so without it the
   pod is stuck `ContainerCreating`/`FailedMount` and blocks Helm `--wait`.
   `bringup` seeds a placeholder if the secret is absent (only-if-absent, so a
   real tenant/GitOps secret is never overwritten); the pod then serves
   `/healthz` and goes Ready. Only actual event egress fails, which the harness
   does not exercise. To wire real egress, create the secret with the tenant
   OIDC client secret before `bringup`.

3. **cert-manager webhook** *(handled automatically)*. `bringup` probes the
   webhook with a throwaway dry-run and, if it is broken (commonly an expired
   1-year serving cert that cert-manager will not rotate post-expiry),
   regenerates the CA and restarts it before installing NVSentinel.

## Build the operator CLI (`harnessctl`)

The operator binary is what you run on your laptop. Build it from
`harnessctl/` so the `replace` of `github.com/nvidia/nvsentinel/data-models`
(`../../../../data-models`) resolves inside the NVSentinel checkout:

```bash
cd tests/scale-tests/harness/harnessctl
go mod tidy
CGO_ENABLED=0 go build -o harnessctl .
./harnessctl -h
```

From the harness directory you can instead use `go build -C harnessctl -o harnessctl .`.
`CGO_ENABLED=0` keeps the binary static. The built file
`harnessctl/harnessctl` is git-ignored.

Optional: run the unit tests in the same directory with `go test ./...`.

The CLI talks to whatever cluster your current kubeconfig context points at
(`kubectl config current-context`). Set that before any `stack` / `nodes` /
`pool` command.

## Build and push the injector image

`pool create` deploys a resident injector DaemonSet. Those pods run the **slim**
`harness-inject` binary (build tag `injector`: inject + reconcile primitives
only — no Helm, no KWOK, no pool orchestration). The host CLI execs into those
pods; it does not copy the operator binary in.

The image **must** be built from the **NVSentinel repo root**, not from
`harnessctl/`. The Dockerfile copies `data-models/` and
`tests/scale-tests/harness/harnessctl/` so the same `replace` path works inside
the build.

### Multi-arch image for a real cluster (push to a registry)

Nodes pull `linux/amd64` or `linux/arm64` automatically. Build and push both:

```bash
# From the NVSentinel repo root.
export HARNESS_INJECT_IMAGE=ghcr.io/<org>/harness-inject:<tag>   # any registry the cluster can pull

docker buildx create --use --name nvs-harness >/dev/null 2>&1 || docker buildx use nvs-harness
docker login ghcr.io   # or your registry

docker buildx build \
  -f tests/scale-tests/harness/harnessctl/Dockerfile \
  --platform linux/amd64,linux/arm64 \
  -t "$HARNESS_INJECT_IMAGE" \
  --push .
```

Then pass that tag on every command that stages or execs injectors (defaults to
`ghcr.io/nvidia/nvsentinel/harness-inject:latest` if omitted):

```bash
BIN=./tests/scale-tests/harness/harnessctl/harnessctl
$BIN pool create --injector-image "$HARNESS_INJECT_IMAGE" --results-dir ./results
$BIN events inject --injector-image "$HARNESS_INJECT_IMAGE" --results-dir ./results
$BIN events reconcile --run-id "$RID" --injector-image "$HARNESS_INJECT_IMAGE" --results-dir ./results
```

`--push` is required for a multi-platform build (`--load` only supports one
platform). The cluster must be able to pull `$HARNESS_INJECT_IMAGE` the same
way it pulls other public `ghcr.io/nvidia/nvsentinel/*` images (no pull secret).

### Local binary only (optional)

To inspect the slim binary without Docker:

```bash
cd tests/scale-tests/harness/harnessctl
CGO_ENABLED=0 go build -tags injector -o harness-inject .
./harness-inject -h    # inject | reconcile only
```

This is **not** what `pool create` deploys — the cluster needs the container
image above.

## Quick start (real cluster)

```bash
cd tests/scale-tests/harness

# 1. Build the operator CLI (see "Build the operator CLI" above).
(cd harnessctl && go mod tidy && CGO_ENABLED=0 go build -o harnessctl .)
BIN=./harnessctl/harnessctl

# 2. Optional: build + push the injector if the cluster cannot pull the default
#    ghcr.io/nvidia/nvsentinel/harness-inject:latest (see "Build and push").
#    Then add --injector-image "$HARNESS_INJECT_IMAGE" to pool / inject / reconcile.

# 3. Bring up the stack (idempotent; pin versions when you want a specific tag).
$BIN stack bringup --nvs-chart-version v1.16.0

# 4. Run Phase 0 at a chosen node count (or use ./run_e2e.sh 20000).
$BIN stack cleanup --pool
$BIN nodes scale --count 20000 --provider-id-scheme kwok --results-dir ./results
$BIN pool create --per-node-pod-limit 10 --results-dir ./results
$BIN janitor check --results-dir ./results
RID=$($BIN events inject --fatal-fraction 0.08 --results-dir ./results | tail -1)
$BIN events reconcile --run-id "$RID" --results-dir ./results
$BIN stack report --title "scale 20k" --window 1h --results-dir ./results
```

`run_e2e.sh <node-count>` is the same sequence (cleanup → scale → pool → janitor
→ inject → reconcile → report), writing to `results/<YYYY-MM-DD>/<N>/`. It does
**not** run `stack bringup`; the stack must already be installed. Override
injection knobs with `FATAL_FRACTION`, `FATAL_EVENT`, `MECHANISM`, `PATTERN`,
`PROCESSING_STRATEGY`, and connector density with `PER_NODE_POD_LIMIT` (default
10 — keep this low; ~50 connectors/node saturates the 1.5-core MongoDB primary,
see `findings/`).

A run is only "proven" when `phase0-results.json` in that results directory has
no FAIL.

## Kind smoke test

Kind is a **functional** check that `harnessctl` can bring up the stack, create
KWOK nodes, stage a tiny connector pool, inject, and reconcile. It is **not** a
scale run: the Kind overlays shrink Prometheus/Mongo/NVSentinel so they fit on
a ~2–4 vCPU Docker VM, switch MongoDB to the Percona operator (ARM-friendly;
Bitnami images are amd64-only), and must not be used to judge scale behaviour.

### What you need

- `kind`, Docker, and Go 1.26 (kubectl optional).
- Enough Docker resources to schedule the slim stack (roughly 4 CPU / 8 GiB).
- Network from the Kind nodes to pull NVSentinel/KWOK/cert-manager images
  (or a mirror). The injector image can be loaded locally instead of pulled.

### 1. Create the cluster

`kind/nvs-harness-kind.yaml` already does the two things a bare cluster would
need by hand: it **drops the control-plane taint** (so the stack can schedule
when each Kind node advertises the whole VM's CPU) and stamps
`nvidia.com/gpu.present=true` on both nodes (fault-quarantine's startup
circuit breaker). Cluster name is `nvs-harness-kind`.

```bash
cd tests/scale-tests/harness
kind create cluster --config kind/nvs-harness-kind.yaml
# kubectl context is now kind-nvs-harness-kind
kubectl get nodes --show-labels | grep gpu.present
```

Do **not** pass `--provider-id-scheme` on Kind (empty is correct; there is no
cloud node-lifecycle controller).

### 2. Build the CLI

```bash
(cd harnessctl && CGO_ENABLED=0 go build -o harnessctl .)
BIN=./harnessctl/harnessctl
$BIN -h
```

### 3. Injector image on Kind

Kind nodes pull through the cluster, not your laptop Docker daemon. The default
`ghcr.io/nvidia/nvsentinel/harness-inject:latest` is not a public package (unlike
the other NVSentinel images), so anonymous pulls fail with `401 Unauthorized` /
`ImagePullBackOff`. Other NVSentinel components use empty `imagePullSecrets`;
do the same here — **load a local image** instead of adding a pull secret:

```bash
# From the NVSentinel repo root. Prefer `docker build` if `docker buildx`
# fails (NVIDIA driver mismatch on some hosts).
docker build -f tests/scale-tests/harness/harnessctl/Dockerfile -t harness-inject:dev .
kind load docker-image harness-inject:dev --name nvs-harness-kind
INJECTOR=harness-inject:dev
```

`pool create` uses `imagePullPolicy: IfNotPresent` so a Kind-loaded image is
used without hitting ghcr. Pass `--injector-image "$INJECTOR"` on `pool create`
only (`events inject` / `reconcile` exec into those pods and do not need the
image flag again).

### 4. Bring up the stack

Layer the Kind overlays **on top of** the embedded KWOK-scale values (Helm
overlay semantics). Paths are relative to the current working directory:

```bash
cd tests/scale-tests/harness
$BIN stack bringup \
  --nvsentinel-values nvsentinel/values-harness-kind.yaml \
  --monitoring-values monitoring/values-kind.yaml
```

Bring-up is idempotent: re-run it after a failure. Optional components
(kube-prometheus-stack, metrics-server) are warned-and-skipped on failure so
they do not block NVSentinel. Confirm:

```bash
kubectl get deploy,ds,sts -n nvsentinel
kubectl get deploy -n kube-system kwok-controller metrics-server
```

### 5. Exercise Phase 0 at tiny scale

Keep node/event counts small. `pool create` sizes itself from the live KWOK
fleet; `--per-node-pod-limit 2` is enough on a 2-node Kind cluster.

```bash
RESULTS=./results/kind-smoke
mkdir -p "$RESULTS"

$BIN stack cleanup --pool
$BIN nodes scale --count 20 --results-dir "$RESULTS"
$BIN pool create --per-node-pod-limit 2 --injector-image "$INJECTOR" --results-dir "$RESULTS"
$BIN janitor check --results-dir "$RESULTS"
RID=$($BIN events inject --count 100 --rate 20 --injector-image "$INJECTOR" --results-dir "$RESULTS" | tail -1)
$BIN events reconcile --run-id "$RID" --injector-image "$INJECTOR" --results-dir "$RESULTS"
$BIN stack report --title "Kind smoke" --window 30m --results-dir "$RESULTS"
```

PASS/FAIL lands in `$RESULTS/phase0-results.json`.

### 6. Tear down

```bash
$BIN stack cleanup --pool          # KWOK nodes + harness pool + orphaned CRs
kind delete cluster --name nvs-harness-kind
```

`stack cleanup` does **not** uninstall NVSentinel / KWOK / cert-manager /
Prometheus; deleting the Kind cluster is the full wipe.

## Commands

| Command | Phase | What it does |
|---------|-------|--------------|
| `stack bringup` | P0.1 | detect + install missing/version-mismatched stack; embedded values + optional `--nvsentinel-values` / `--monitoring-values` overlays |
| `stack cleanup [--pool=false]` | — | delete `type=kwok` nodes, orphaned janitor CRs, and (default on) the harness-owned pool; never touches live `platform-connectors` |
| `stack report [--title … --window …]` | — | collect latency/throughput/resource/CR/mongo metrics → `report.md` / `report.json` |
| `nodes scale --count N` | P0.2 | create GPU-shaped KWOK nodes, informer-wait Ready, record ceiling (+ apiserver p99) |
| `nodes ceiling [--start --step --max]` | P0.2 | ramp node count until degradation and attribute it (harness vs api/etcd) |
| `events inject [--pattern --fatal-event --mechanism …]` | P0.3 | fire every resident injector; attribute events to KWOK nodes; stamp correlation id; prints run-id on stdout |
| `events reconcile --run-id ID [--direct]` | P0.3 | account every injected id vs the datastore, emit report |
| `events coldstart [--count --remediation-ratio …]` | — | seed a MongoDB haystack, cold-start a consumer, measure initial scan time |
| `janitor check` | P0.4 | create RebootNode + GPUReset CRs, cycle bootID, verify completion |
| `pool create` | P0.5 | stage harness connector StatefulSet (`nvs-harness-*`) + one resident injector per real node; leaves the pool up for inject/reconcile |
| `pool teardown` | P0.5 | delete harness-owned pool + injectors only; never touches live `platform-connectors` |
| `pool startup-burst` | P0.5 | recreate N connectors starting in parallel across `--burst-steps` client-go burst values; measure API-server APF (rejected / inqueue / wait p99) via Prometheus |
| `pool connection-sweep` | P0.5 | self-contained: create → scale `--replica-steps` → measure Mongo → teardown. Connections from mongod `connectionCount` logs; CPU/mem from metrics-server (`kubectl top`) with kubelet `stats/summary` fallback. No injectors; Prometheus not required |

Default baked-in versions when `stack bringup` must install (empty version flag
= "accept whatever is already installed"): NVSentinel `v1.16.0`, KWOK `v0.6.1`,
cert-manager `v1.16.2`, metrics-server `v0.7.2`, kube-prometheus-stack `65.5.0`.
Chart: `oci://ghcr.io/nvidia/nvsentinel`.

Default namespaces: `nvsentinel`, `prometheus`, `cert-manager`, KWOK in
`kube-system`, platform-managed janitor in `dgxc-janitor-system`.

## Configuration

**`harnessctl` reads no config/env file.** Every input is a `--kebab-case` flag;
`-h` on any command lists only the flags that command uses.

`config/harness.env` is sourced by `phase0/*.sh` (via `lib/common.sh`) and by
`run_e2e.sh` so a few values can be forwarded as flags (`MONITORING_NAMESPACE`,
`KWOK_PROVIDER_ID_SCHEME`). Editing it does not change `harnessctl` behaviour
unless you pass the matching flags.

Two internal-only exceptions still read an env var (never part of the flag
surface): poll-interval tuning knobs (`P03_DRAIN_*`, `HARNESS_MONITOR_*`,
`NODE_READY_STALL_SECONDS`) and `MONGO_URI`, which the distributed orchestrator
sets inside injector pods so mTLS credentials stay off the command line.

## Design notes

- **Fake nodes, real connectors.** KWOK nodes have no kubelet, so the
  platform-connector DaemonSet is pinned OFF them (`type != kwok` affinity in
  `nvsentinel/values-harness.yaml`). `pool create` packs cloned connector pods
  onto real nodes (capped by `--per-node-pod-limit`) and deploys one resident
  injector per connector node. Injection ingresses through those real connectors
  but attributes each event to a KWOK node name — the logical connector-pool
  model the requirements doc describes (MB-3b aggregate plane). Two pool
  experiments reuse that packing without a full inject: `startup-burst`
  (API-server APF at simultaneous connector start) and `connection-sweep`
  (Mongo connection density vs mongod CPU/mem; idle connectors, no events).
  Keep `--per-node-pod-limit` low on E2E runs (~50 connectors/node saturates
  the 1.5-core MongoDB primary; see `findings/`).
- **Ceiling attribution (P0.2).** `nodes scale` / `nodes ceiling` record whether
  the KWOK controller saturated (a harness limit to tune away) or the API
  server/etcd saturated (the real ceiling that bounds Phase 2), including the
  apiserver p99 and real-node CPU/memory guardrails.
- **Event-ID reconciliation (P0.3).** `events inject` stamps a unique id into
  `HealthEvent.id` and `metadata[nvs_harness_id]`, plus a run label. Default
  mechanism is `grpc` (through the platform-connector). `mongo` inserts
  directly for storage/change-stream stress. `events reconcile` diffs the
  injection ledger against `HealthEventsDatabase.HealthEvents`
  (`healthevent.metadata.*`). This same code backs the zero-loss checks in
  SYS-2 / MB-5 / SYS-5 later.
- **Janitor on KWOK (P0.4).** A KWOK custom stage completes janitor Job pods
  after a delay; `janitor check` simulates the node reboot cycle (NotReady →
  Ready + fresh bootID) so the janitor's reconciliation sees a genuine reboot.
  Run this on a quiet fleet (the E2E driver does janitor **before** inject).
- **Cleanup.** Prior runs leave KWOK nodes and orphaned `RebootNode`/`GPUReset`
  CRs that wedge in Terminating. `stack cleanup` clears both and, by default,
  the harness-owned pool (`nvs-harness-*` only).

## Results

Commands that write artifacts take `--results-dir` (default `./results`). Typical
files:

| File | Source |
|------|--------|
| `phase0-results.json` / `.xml` | pass/fail roll-up (JUnit) from scale / janitor / pool / … |
| `p0.2-node-ceiling.json` | `nodes scale` |
| `p0.2-ceiling-sweep.json` | `nodes ceiling` |
| `p0.4-janitor-actions.json` | `janitor check` |
| `p0.5-connector-pool.json` | `pool create` |
| `connector-startup-burst.json` | `pool startup-burst` |
| `connector-connection-sweep.json` | `pool connection-sweep` |
| `reconcile-report.json` | `events reconcile` |
| `reconcile-health.jsonl` / `reconcile-health-summary.json` | health monitor during reconcile |
| `report.md` / `report.json` | `stack report` |

Scale-attributable defects found while running the harness are written up under
[`findings/`](findings/README.md).

## Known integration points to confirm on your cluster

These are intentionally left as explicit knobs rather than guesses:

1. **MongoDB reachability** — `events reconcile` runs **in-cluster** via a
   resident injector and derives the (mTLS) connection from the NVSentinel
   datastore secret. Override `--mongo-service` / `--mongo-tls-secret` if your
   install differs (Bitnami `mongodb-headless` vs Percona `mongodb-rs0` is
   auto-detected). `--direct` + `--uri` is the rare out-of-cluster path.
2. **KWOK `Stage` schema** — validate `kwok/stages-custom.yaml` against the
   pinned `KWOK_VERSION` (`v0.6.1`); the v1alpha1 schema evolves between
   releases.
3. **CR group/version** — `harnessctl` creates `RebootNode`/`GPUReset` in
   `janitor.dgxc.nvidia.com/v1alpha1` via the dynamic client; confirm on your
   build.
4. **Node label for "real" nodes** — nodes without `type=kwok` are treated as
   real (runner) nodes; adjust if your CSP already sets a `type` label.
5. **PostgreSQL** — `reconcile` currently supports MongoDB only.
6. **Injector image** — default `ghcr.io/nvidia/nvsentinel/harness-inject:latest`;
   pass `--injector-image` if the cluster cannot pull that tag.

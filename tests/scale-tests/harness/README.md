# NVSentinel Scale-Test Harness

Next-generation scale testing per **NVSentinel Scale Test Requirements**: a
two-phase, KWOK-based, reproducible suite that replaces the one-shot 1,500-real-node
campaign under `../` (which stays as historical reference).

- **Phase 0 – Harness** (this milestone): prove the harness itself before any
  benchmark counts.
- **Phase 1 – Microbenchmarks** (`MB-*`): per-component baselines + config cost.
- **Phase 2 – System Scale** (`SYS-*`): full pipeline at the node ceiling.

## Implementation status

> **Only §9 / Phase 0 is implemented today.** Phase 1 and Phase 2 are not built yet.

| Requirement | Scope | Status |
|-------------|-------|--------|
| **§9 Phase 0 – Harness** (P0.1–P0.4) | idempotent bring-up, 50k-node ceiling, event-ID reconciliation, janitor action path | ✅ implemented (`harnessctl`) |
| **§10 Phase 1 – Microbenchmarks** | load benchmarks `MB-1…MB-7` + config-cost `MB-C1…MB-C5` | ⛔ TODO |
| **§11 Phase 2 – System Scale** | full pipeline at 50k nodes / ~4,000 ev/s, mass-failure profiles `SYS-*` | ⛔ TODO |

Phase 1/2 additionally require shared measurement plumbing not yet present:
per-event latency tracking (V1), warmup + N≥3 repetitions with median/min-max
(V2/V3), per-component resource + API-server attribution collection (O3), and
QoS/pinning of the component under test (V4). These are expected to land as new
`harnessctl` subcommands (e.g. `mb`, `sys`) on top of a shared `internal/measure`
package that reuses the Phase 0 injector, node scaler, and Prometheus helper.

## Design: one Go binary + thin helm shell

Orchestration, node scaling, waiters, CR handling, event injection and
reconciliation all live in a single Go CLI, **`harnessctl`** (`./harnessctl/`),
built on `client-go`. This gives typed API access, informer-based waits, and
structured JSON/JUnit results suitable for unattended, per-release runs
(requirements goal G7) — instead of bash parsing `kubectl` output.

Only the **helm installs stay as shell** (`phase0/10|20|30-install-*.sh`), since
`helm upgrade --install` is simpler and more transparent as a one-liner and
rewriting it in Go buys no robustness.

The same `harnessctl` image is used two ways: as the operator CLI, and as the
in-cluster Job image for `inject` / `reconcile` (launched by `phase0`).

## Layout

```
harness/
├── config/harness.env          # ALL tunables (node count, HARNESS_IMAGE, thresholds…)
├── lib/common.sh               # shared bash helpers for the install scripts
├── monitoring/                 # kube-prometheus-stack values (KWOK-scale)
├── kwok/stages-custom.yaml      # custom KWOK stages (graceful delete, janitor Job complete)
├── nvsentinel/values-harness.yaml
├── phase0/                      # helm install scripts (P0.1 bring-up only)
│   ├── 10-install-monitoring.sh
│   ├── 20-install-kwok.sh
│   └── 30-install-nvsentinel.sh
├── harnessctl/                  # the Go CLI (see harnessctl/README.md)
│   ├── main.go, config.go, common.go, kube.go, prom.go, jobs.go
│   ├── cmd_core.go, cmd_inject.go, cmd_reconcile.go, cmd_janitor.go, cmd_phase0.go
│   ├── Dockerfile, README.md, go.mod
└── results/                     # generated artifacts (git-ignored)
```

## Phase 0 acceptance criteria

| Check | Requirement | Command |
|-------|-------------|---------|
| **P0.1** | Scripted, idempotent bring-up (monitoring + KWOK + NVSentinel) | `harnessctl bringup` (wraps `phase0/10│20│30-install-*.sh`) |
| **P0.2** | `KWOK_NODE_COUNT` GPU-shaped nodes Ready; API server within bounds; ceiling recorded | `harnessctl scale-nodes` |
| **P0.3** | Events attributed to KWOK node names, every event ID reconciled | `harnessctl phase0 --only inject` |
| **P0.4** | RebootNode Job completes + node cycles bootID; GPUReset Job completes | `harnessctl janitor-check` |

## Prerequisites

- A real Kubernetes cluster (the Constraints table assumes ~10 CPU nodes,
  16 vCPU / 32 GB) reachable via your kubeconfig.
- `kubectl`, `helm` (for installs), `go` and `docker` (to build/push `harnessctl`).
- A container registry the cluster can pull from, for the `harnessctl` image.

### Bare / from-scratch clusters (e.g. Kind)

Managed clusters (AKS/EKS/OKE) already satisfy these; a bare cluster does not, and
`bringup` installs NVSentinel with `helm --wait`, so an unmet item leaves a pod
never-Ready and the install eventually times out.

1. **At least one node labeled `nvidia.com/gpu.present=true` must exist** *(manual
   on a bare cluster)*. `fault-quarantine`'s startup circuit breaker uses the count
   of GPU-labeled nodes as its denominator (`fault-quarantine/pkg/informer/node_informer.go`
   lists nodes matching `nvidia.com/gpu.present=true`). With zero such nodes it exits
   fatally at startup and crashloops — the log says `GetTotalNodes returning 0 …
   NodeInformer cache sync issues`, which is misleading: it is the deterministic
   "no GPU nodes" branch, not a cache race. `scale-nodes` stamps this label on every
   KWOK node, so it is a non-issue at scale; a bare cluster needs one **before**
   `bringup`. Label an existing worker:

   ```bash
   kubectl label node <worker> nvidia.com/gpu.present=true --overwrite
   ```

   …or, if there is no schedulable worker (single-node Kind), create an untainted
   KWOK node carrying the label (the `NoSchedule` fake-node taint would make it
   unschedulable):

   ```bash
   kubectl apply -f - <<'EOF'
   apiVersion: v1
   kind: Node
   metadata:
     name: kwok-seed-0
     annotations: { kwok.x-k8s.io/node: fake, node.alpha.kubernetes.io/ttl: "0" }
     labels:
       type: kwok
       nvidia.com/gpu.present: "true"
       kubernetes.io/hostname: kwok-seed-0
       kubernetes.io/os: linux
   EOF
   ```

2. **`event-exporter-oidc-secret`** *(handled automatically)*. event-exporter mounts
   this Secret as a required (`optional:false`) volume, so without it the pod is stuck
   `ContainerCreating`/`FailedMount` and blocks `helm --wait`. `bringup` seeds a
   placeholder if the secret is absent (only-if-absent, so a real tenant/GitOps secret
   is never overwritten); the pod then serves `/healthz` and goes Ready. Only actual
   event egress fails, which the harness does not exercise. To wire real egress, create
   the secret with the tenant OIDC client secret before `bringup`.

3. **cert-manager webhook** *(handled automatically)*. `bringup` probes the webhook
   with a throwaway dry-run and, if it is broken (commonly an expired 1-year serving
   cert that cert-manager will not rotate post-expiry), regenerates the CA and restarts
   it before installing NVSentinel.

## Quick start

```bash
cd tests/scale-tests/harness

# 1. Review and edit the single config file.
$EDITOR config/harness.env       # set HARNESS_IMAGE_REGISTRY, KWOK_NODE_COUNT, HARNESS_MONGO_URI…
set -a; source config/harness.env; set +a

# 2. Build + push the one harness image.
(cd harnessctl && go mod tidy && CGO_ENABLED=0 go build -o harnessctl . \
    && docker build -t "$HARNESS_IMAGE" . && docker push "$HARNESS_IMAGE")

# 3. Bring up the stack (helm), then run Phase 0.
./harnessctl/harnessctl bringup                      # runs phase0/10|20|30-install-*.sh
./harnessctl/harnessctl phase0                       # nodes + inject/reconcile + janitor
# …or single steps:
./harnessctl/harnessctl scale-nodes --count 20000
./harnessctl/harnessctl phase0 --only inject
./harnessctl/harnessctl janitor-check
# run installs from the orchestrator too:
./harnessctl/harnessctl phase0 --install-dir phase0
```

Results (ceiling record, reconcile report, JSON + JUnit pass/fail) are written to
`results/`. A run is only "proven" when `results/phase0-results.json` has no FAIL.

## Design notes

- **Fake nodes, real connectors.** KWOK nodes have no kubelet, so the
  platform-connector DaemonSet is pinned OFF them (`type != kwok` affinity in
  `nvsentinel/values-harness.yaml`). Injection ingresses through connectors on
  the real nodes but attributes each event to a KWOK node name — the logical
  connector-pool model the requirements doc describes (MB-3b aggregate plane).
- **Ceiling attribution (P0.2).** `scale-nodes` records whether the KWOK
  controller saturated (a harness limit to tune away) or the API server/etcd
  saturated (the real ceiling that bounds Phase 2), including the apiserver p99.
- **Event-ID reconciliation (P0.3).** `inject` stamps a unique id into
  `HealthEvent.id` and `metadata[nvs_harness_id]`, plus a run label. `reconcile`
  diffs the injection ledger against `HealthEventsDatabase.HealthEvents`
  (`healthevent.metadata.*`). This same code backs the zero-loss checks in
  SYS-2 / MB-5 / SYS-5 later.
- **Janitor on KWOK (P0.4).** A KWOK custom stage completes janitor Job pods
  after a delay; `harnessctl sim-reboot` performs the node reboot cycle
  (NotReady → Ready + fresh bootID) so the janitor's reconciliation sees a
  genuine reboot.

## Known integration points to confirm on your cluster

These are intentionally left as explicit knobs rather than guesses:

1. **`HARNESS_MONGO_URI`** — `reconcile` needs a URI reachable from the
   NVSentinel namespace. `phase0` tries to derive it from a `mongodb-store`
   secret; set it explicitly if your install differs.
2. **KWOK `Stage` schema** — validate `kwok/stages-custom.yaml` against the
   pinned `KWOK_VERSION`; the v1alpha1 schema evolves between releases.
3. **CR group/version** — `harnessctl` creates `RebootNode`/`GPUReset` in
   `janitor.dgxc.nvidia.com/v1alpha1` via the dynamic client; confirm on your build.
4. **Node label for "real" nodes** — nodes without `type=kwok` are treated as
   real (runner) nodes; adjust if your CSP already sets a `type` label.
5. **PostgreSQL** — `reconcile` currently supports MongoDB only.

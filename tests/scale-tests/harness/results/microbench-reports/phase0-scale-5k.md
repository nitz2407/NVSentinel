# NVSentinel Phase 0 Scale Harness — Microbenchmark Results (5k nodes)

End-to-end Phase-0 scale run at **5,000 simulated nodes**. Same measurement layout as the module microbenchmarks (#1515 MongoDB, #1517 node-drainer, #1519 fault-remediation, #1525 platform-connector) so rungs diff directly.

- **Run parameters:** default fatal-fraction (~0.08), gpu-reset, mechanism=grpc
- **NVSentinel version:** release at run date
- **Generated (UTC):** 2026-07-30T13:52:08Z  ·  **Prometheus peak window:** 1h
- **Run id:** `run-1785419208-168cb97b`

---

## Test Environment

| Component | Detail |
| --- | --- |
| Cluster (apiserver) | `https://aks-h6rr9wnc.hcp.southcentralus.azmk8s.io:443` |
| Kubernetes | v1.32.5 (managed AKS, South Central US) |
| Real worker nodes | 32 (3 system tainted, 3 CPU, 26 GPU) |
| Simulated (KWOK) nodes | 5,000 (Ready 5,000) |
| Connector-pool pods | 320 (platform-connector density under test) |
| SUT | NVSentinel + Janitor on real nodes; KWOK provides Node objects only |
| etcd metrics | not exposed (managed control plane) |

---

## Metrics Used

Peaks are sampled by `harnessctl report` as `max_over_time((<query>)[<window>:1m])`.

| Metric | PromQL | What it measures |
| --- | --- | --- |
| apiserver all-verb p99 | `histogram_quantile(0.99, sum(rate(apiserver_request_duration_seconds_bucket{verb!~"WATCH|CONNECT"}[5m])) by (le))` | End-to-end control-plane request latency; primary health guardrail (<1.0s) |
| apiserver read p99 (GET|LIST) | `histogram_quantile(0.99, sum(rate(apiserver_request_duration_seconds_bucket{verb=~"GET|LIST"}[5m])) by (le))` | Read-path latency; rises first as cached-collection reads grow |
| apiserver write p99 | `histogram_quantile(0.99, sum(rate(apiserver_request_duration_seconds_bucket{verb=~"POST|PUT|PATCH|DELETE"}[5m])) by (le))` | Write-path latency (node heartbeats, cordon patches, CR writes) |
| apiserver LIST-nodes p99 | `histogram_quantile(0.99, sum(rate(apiserver_request_duration_seconds_bucket{verb="LIST",resource="nodes"}[5m])) by (le))` | Heaviest large-collection read — leading indicator of the node-scale ceiling |
| apiserver LIST-nodes rate | `sum(rate(apiserver_request_total{verb="LIST",resource="nodes"}[5m]))` | How often the full node collection is listed (informer resync fan-out) |
| apiserver total request rate | `sum(rate(apiserver_request_total[5m]))` | Aggregate control-plane request throughput (all verbs/resources) |
| apiserver in-flight (peak) | `sum(apiserver_current_inflight_requests)` | Concurrent requests in APF; saturation approaches the APF seat ceiling |
| apiserver 5xx rate (peak) | `sum(rate(apiserver_request_total{code=~"5.."}[5m]))` | Server-error rate; separates healthy load from control-plane distress |
| etcd db size (MiB) | `sum(apiserver_storage_db_total_size_in_bytes)/1024/1024   # fallback: etcd_db_total_size_in_bytes/1024/1024` | etcd backing-store size (n/a on managed AKS control plane) |
| etcd wal fsync p99 | `histogram_quantile(0.99, sum(rate(etcd_disk_wal_fsync_duration_seconds_bucket[5m])) by (le))` | etcd disk sync latency (n/a on managed AKS) |
| etcd backend commit p99 | `histogram_quantile(0.99, sum(rate(etcd_disk_backend_commit_duration_seconds_bucket[5m])) by (le))` | etcd commit latency (n/a on managed AKS) |
| janitor gpureset workqueue depth (peak) | `max(workqueue_depth{name="gpureset"})` | Remediation backlog — the Phase-2 remediation-throughput bottleneck signal |
| janitor gpureset reconcile rate (peak) | `sum(rate(controller_runtime_reconcile_total{controller="gpureset"}[5m]))` | GPUReset reconcile throughput |
| janitor restarts (window) | `max(max_over_time(kube_pod_container_status_restarts_total{namespace="<janitor-ns>",pod=~"dgxc-janitor-controller-manager.*"}[<window>]))` | Janitor controller stability over the run |
| component memory (working set) | `container_memory_working_set_bytes{namespace="nvsentinel",container="<component>"}` | Per-component OS RSS; peak drives limit sizing |
| cordoned nodes (dashboard source) | `sum(kube_node_spec_unschedulable)` | Fleet cordon count (harness reads live LIST; KSM metric for Grafana) |

---

## Scale footprint (live LIST at report time)

| Object | Count |
| --- | --- |
| Total nodes | 5,032 |
| KWOK nodes (Ready) | 5,000 (5,000) |
| Cordoned nodes | 427 |
| GPUReset CRs | 106 |
| Reset Jobs | 106 |
| Node-lock leases | 41 |
| Connector-pool pods | 320 |

---

## 1. Node scaling (P0.2)

| Metric | Value |
| --- | --- |
| Target nodes | 5,000 |
| Ready | 5,000 (100.0%) |
| Failed creates | 0 |
| Time-to-ready | 187s (~3.1 min) |
| apiserver p99 during scale | 0.042s (guardrail 1s) |
| KWOK Ready at report time | 5,000 / 5,000 (100.0%) |

---

## 2. Control plane — API server (Prometheus peaks over window)

| Signal | Value |
| --- | --- |
| all-verb p99 | 0.622s |
| read/LIST p99 | 0.090s |
| write p99 | 0.817s |
| LIST-nodes p99 | 4.932s |
| LIST-nodes rate | 50.7/s |
| total request rate | 381,934/s |
| in-flight (peak) | 108 |
| 5xx rate (peak) | 348.4/s |

> etcd metrics not exposed (managed control plane) — collected none.

---

## 3. Fault -> cordon -> GPUReset remediation (P0.4)

| Metric | Value |
| --- | --- |
| GPUReset CRs total | 106 |
| CR phases | InProgress=40, Succeeded=66 |
| GPUReset latency (start->complete) | count=66 · min 1s · p50 31s · p90 32s · p99 35s · max 38s · mean 21.1s |
| Reset Jobs (succeeded/total) | 67/106 |
| janitor workqueue depth (peak) | 0 |
| janitor reconcile rate (peak) | 17.325/s |

---

## 4. Event injection + reconciliation (P0.3)

| Metric | Value |
| --- | --- |
| injected | 5,120 |
| acked | 5,120 |
| stored for run | 5,120 |
| accounted | 5,120 |
| missing | 0 |
| loss fraction | 0 (max 0) |
| NodeName attribution | 5,040/5,040 matched |
| verdict | ✅ PASS |

---

## 5. Ceiling attribution

> REAL ceiling: apiserver LIST-nodes p99 4.932s exceeded guardrail 1.000s — large-collection LIST against apiserver/etcd is the bound for node scale (Phase 2). Overall read p99 stays low (0.090s) and in-flight peak 108, so it's LIST-fanout cost, not general control-plane saturation.

---

## 6. Pass criteria

- ✅ apiserver all-verb p99 0.622s within guardrail 1s
- ⚠️ LIST-nodes p99 4.932s (large-collection read cost at this scale)
- ✅ KWOK readiness 100.0% (kwok-controller heartbeat ceiling is a harness limit, not the SUT)
- ⚠️ 5xx peaked 348.4/s — attribute vs shared-cluster background
- ✅ event delivery: loss 0, attribution 5,040/5,040

---

## Relevant code locations

- `tests/scale-tests/harness/harnessctl/` — CLI (bringup, scale-nodes, connector-pool, inject, reconcile, janitor-check, report)
- `tests/scale-tests/harness/harnessctl/cmd_report.go` + `prom.go` — the PromQL above
- `tests/scale-tests/harness/run_e2e.sh` — the P0.2→P0.5→P0.4→P0.3→report driver
- Raw artifacts: `results/2026-07-30/5000/report.json` + `report.md`

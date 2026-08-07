# NVSentinel Phase 0 Scale Harness — Microbenchmark Results (40k nodes)

End-to-end Phase-0 scale run at **40,000 simulated nodes**. Same measurement layout as the module microbenchmarks (#1515 MongoDB, #1517 node-drainer, #1519 fault-remediation, #1525 platform-connector) so rungs diff directly.

- **Run parameters:** fatal-fraction=1.0, fatal-event=node-reboot; report.json not persisted (reconstructed)
- **NVSentinel version:** v1.16.0
- **Generated (UTC):** 2026-08-03T13:59:20Z  ·  **Prometheus peak window:** n/a (reconstructed — no Prometheus snapshot)
- **Run id:** `run-1785765405-4e5b9471`
- **Cordon convergence:** reached **39,997/40,000 nodes cordoned** (~2.3h to peak, held over an 5.8h monitored window)
- **⚠️ Reconstructed report:** `report.json` (the Prometheus control-plane snapshot) was never persisted for this run. Node-scaling (P0.2), event delivery (P0.3), janitor check (P0.4), and cordon convergence are recovered from on-disk artifacts; **API-server / etcd peak tables are unavailable** (use the Promxy dashboard for this rung).

---

## Test Environment

| Component | Detail |
| --- | --- |
| Cluster (apiserver) | `https://aks-h6rr9wnc.hcp.southcentralus.azmk8s.io:443` |
| Kubernetes | v1.32.5 (managed AKS, South Central US) |
| Real worker nodes | 32 (3 system tainted, 3 CPU, 26 GPU) |
| Simulated (KWOK) nodes | 40,000 (Ready 40,000) |
| Connector-pool pods | 640 (platform-connector density under test) |
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
| Total nodes | 40,032 |
| KWOK nodes (Ready) | 40,000 (40,000) |
| Cordoned nodes | 39,997 (peak) · 73 (report-time snapshot) |
| GPUReset CRs | 0 |
| Reset Jobs | 0 |
| Node-lock leases | 0 |
| Connector-pool pods | 640 |

---

## 1. Node scaling (P0.2)

| Metric | Value |
| --- | --- |
| Target nodes | 40,000 |
| Ready | 40,000 (100.0%) |
| Failed creates | 0 |
| Time-to-ready | 18s |
| apiserver p99 during scale | 0.060s (guardrail 1s) |
| KWOK Ready at report time | 40,000 / 40,000 (100.0%) |

---

## 2. Control plane — API server (Prometheus peaks over window)

> ➖ API-server / etcd peaks are **unavailable for this run** — `report.json` (the Prometheus snapshot) was not persisted. During scale-up the P0.2 apiserver p99 was **0.060s** (guardrail 1s). For the full peak curve at this rung, use the Promxy dashboard with the queries in *Metrics Used*.

---

## 3. Fault -> cordon -> GPUReset remediation (P0.4)

| Metric | Value |
| --- | --- |
| GPUReset CRs total | 0 |
| CR phases | (none) |
| GPUReset latency | (no completed CRs in window) |
| Reset Jobs (succeeded/total) | 0/0 |
| RebootNode CRs (node-reboot remediation) | 4,146 |
| janitor workqueue depth (peak) | n/a |
| janitor reconcile rate (peak) | n/a/s |
| P0.4 janitor-check | gpureset=PASS, reboot=FAIL |

### Fleet-wide cordon convergence (`sum(kube_node_spec_unschedulable)`)

Under `fatal-fraction=1.0` every node receives a fatal event, so fault-quarantine cordons the **entire fleet** (throttled by its 50%/5m circuit breaker) before node-reboot remediation drains it back. The `report.json` cordon count is a single late snapshot; the authoritative peak is the convergence time series below.

| Metric | Value |
| --- | --- |
| Peak cordoned | **39,997 / 40,000** (100.0%) |
| First sample | 18,980 cordoned |
| Time to peak | ~2.3h |
| Monitored window | 5.8h (62 samples) |

> Cordon-count query (matches the Grafana/Promxy dashboard):

```promql
sum(kube_node_spec_unschedulable{cluster="nvs-dgxc-k8s-azr-scus-dev1"})
```

---

## 4. Event injection + reconciliation (P0.3)

| Metric | Value |
| --- | --- |
| injected | 40,320 |
| acked | 40,320 |
| stored for run | 40,320 |
| accounted | 40,320 |
| missing | 0 |
| loss fraction | 0 (max 0) |
| NodeName attribution | 5,063/5,063 matched |
| verdict | ✅ PASS |

---

## 5. Ceiling attribution

> Reconstructed run — control-plane Prometheus peaks were not persisted. P0.2 scale, P0.3 delivery, and full-fleet cordon convergence are recovered from on-disk artifacts; use the Promxy dashboard for the apiserver/LIST-nodes curve at this rung.

---

## 6. Pass criteria

- ➖ apiserver control-plane peaks unavailable (report.json/Prometheus snapshot not persisted for this run) — reconstructed from P0.2/P0.3/P0.4 + converge artifacts
- ✅ KWOK readiness 100.0% (kwok-controller heartbeat ceiling is a harness limit, not the SUT)
- ✅ fault-quarantine cordoned 39,997/40,000 nodes (100.0%) under a fleet-wide fatal storm — full-fleet cordon convergence achieved
- ✅ event delivery: loss 0, attribution 5,063/5,063
- ❌ P0.4 janitor-check: gpureset=PASS, reboot=FAIL  (NodeReady≠True within 5m — janitor reboot-provider wiring; GPUReset path OK)

---

## Relevant code locations

- `tests/scale-tests/harness/harnessctl/` — CLI (bringup, scale-nodes, connector-pool, inject, reconcile, janitor-check, report)
- `tests/scale-tests/harness/harnessctl/cmd_report.go` + `prom.go` — the PromQL above
- `tests/scale-tests/harness/run_e2e.sh` — the P0.2→P0.5→P0.4→P0.3→report driver
- Raw artifacts: `results/2026-08-03/40000/report.json` + `report.md`

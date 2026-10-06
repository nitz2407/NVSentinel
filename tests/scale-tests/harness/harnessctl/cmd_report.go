//go:build !injector

/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// runReport collects the full performance picture of a scale run — Prometheus
// control-plane latency/throughput, etcd, node/cordon state, RebootNode/GPUReset
// & reset-Job latency, component CPU/memory (Prometheus max_over_time over --window, with
// kubelet/metrics-server only if Prometheus has no series), MongoDB counts and a ceiling
// attribution — and writes report.md + report.json. It replaces the ad-hoc
// kubectl/PromQL commands previously run by hand.
func runReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stack report", flag.ExitOnError)
	cfg := defaultConfig()
	bindNvsNamespaceFlag(fs, &cfg)
	bindResultsFlag(fs, &cfg)
	bindPromFlags(fs, &cfg)
	bindJanitorNamespaceFlag(fs, &cfg)
	bindMaxAPIServerP99Flag(fs, &cfg)
	bindNodePrefixFlag(fs, &cfg)
	out := fs.String("out", "", "Markdown report output path (default: <results-dir>/report.md)")
	window := fs.String("window", windowAuto,
		"PromQL lookback window for peak (max_over_time) queries; \"auto\" spans from when the fleet reached target size")
	title := fs.String("title", "NVSentinel scale run", "report title")
	// The A1 section covers this run only. Build the cross-run curve with
	// `harnessctl sizing curve --runs <parent-of-run-folders>`, which reads the
	// run folders instead of merging into shared state.
	runID := fs.String("run-id", "", "identifier recorded on this run's sizing point (default: results-dir name)")
	_ = fs.Parse(args)
	resolvedWindow, windowNote, err := resolveReportWindow(*window, cfg)
	if err != nil {
		return err
	}
	cfg.ReportWindow, cfg.ReportWindowNote = resolvedWindow, windowNote
	if *out == "" {
		*out = filepath.Join(cfg.ResultsDir, "report.md")
	}

	c, err := newClients(cfg)
	if err != nil {
		return err
	}

	stepf("report: collecting metrics (window=%s)", cfg.ReportWindow)
	infof("report: %s", cfg.ReportWindowNote)
	data := c.collectReport(ctx, cfg, *title)
	data.A1 = buildA1Sizing(data.Sizing, *title, defStr(*runID, filepath.Base(cfg.ResultsDir)))
	if cfg.ReportWindowNote != "" {
		data.A1.Caveats = append(data.A1.Caveats, cfg.ReportWindowNote)
	}

	writeArtifact(cfg.ResultsDir, "report.json", data)
	writeArtifact(cfg.ResultsDir, "component-sizing.json", data.Sizing)
	writeArtifact(cfg.ResultsDir, "a1-component-sizing.json", data.A1)
	md := renderReportMarkdown(data)
	if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	infof("report written: %s (+ %s/report.json)", *out, cfg.ResultsDir)
	return nil
}

// ---- data model -----------------------------------------------------------

type metric struct {
	Value float64 `json:"value"`
	OK    bool    `json:"ok"`
}

// Fallback thresholds, used only when the apiserver exposes no process start
// time. A hopping endpoint resets on a large share of scrapes — measured at
// 0.25 to 0.38 on a six-replica AKS control plane, flat across window lengths —
// while a restart contributes one reset total. The sample floor is what keeps
// the two apart: at 15 samples even three restarts land at 0.2, so anything
// above the bound is hopping, and below the floor the test declines to judge
// rather than guess. A 20-minute window at a 1-minute scrape sits just over it.
const (
	apiserverMaxResetFraction = 0.2
	apiserverMinResetSamples  = 15
)

// apiserverTrust records whether the apiserver_* series are one counter or
// several multiplexed into one.
//
// A managed control plane (AKS, EKS, GKE) fronts N apiserver replicas behind a
// single endpoint, and scraping that endpoint through one Service yields one
// series whose value hops between the replicas' independent counters. The
// damage is specific to rate(): a dip reads as a reset, so rate() discards it
// and then counts the recovery as fresh traffic, and the two errors compound
// every scrape. A 1000-node AKS run reported 1,032,299 req/s and 298,248
// lease PUTs/s against an actual lease load near 100/s — three orders of
// magnitude, with latency quantiles off the same hopping buckets. None of it
// looks wrong in the output, which is why this is probed rather than left to
// the reader.
//
// The test is a backwards step in the apiserver's own process start time. One
// process can only ever move that forward, so a single decrease is proof that
// the endpoint answered from an older replica — no threshold to tune and no
// false positives. It is also why counting resets on apiserver_request_total is
// only a fallback: reset counts grow with the number of samples in the window,
// so any fixed bound on them is really a bound on window length. An 8-minute
// window on the cluster above showed a median of 2 request-counter resets and
// would have passed a bound of 3, while the start time still went backwards 3
// times in the same window.
type apiserverTrust struct {
	// Reversals counts backwards steps in the apiserver's process start time.
	Reversals int `json:"start_time_reversals"`
	// ResetFraction is the fallback signal: resets of apiserver_request_total
	// as a share of the samples in the window.
	ResetFraction float64 `json:"request_reset_fraction,omitempty"`
	// Signal names the test that decided, so a verdict can be re-derived later.
	Signal  string `json:"signal,omitempty"`
	ProbeOK bool   `json:"probe_ok"`
	Usable  bool   `json:"usable"`
}

// reason explains an unusable verdict in one line, for the report body.
func (t apiserverTrust) reason() string {
	const consequence = "so this endpoint is multiplexing several apiserver replicas into one series, " +
		"and every rate() and histogram_quantile() over them is inflated by an unknown factor"
	if t.Signal == apiserverSignalStartTime {
		return fmt.Sprintf("the apiserver's reported process start time moved backwards %d times over the window, "+
			"which one process cannot do, %s", t.Reversals, consequence)
	}
	return fmt.Sprintf("%.0f%% of apiserver counter samples in the window follow a reset, far more than restarts "+
		"could account for, %s", t.ResetFraction*100, consequence)
}

const (
	apiserverSignalStartTime = "process_start_time_reversal"
	apiserverSignalResets    = "request_counter_reset_fraction"
)

// apiserverCounterTrust probes the counters the API-server table is built from.
//
// An inconclusive probe leaves the metrics usable: Prometheus may not expose
// the series, and refusing to report on that basis would be a worse error than
// reporting. Only a positive detection withdraws the numbers.
func apiserverCounterTrust(inst func(string) metric, job, win string) apiserverTrust {
	if job != "" {
		rev := inst(fmt.Sprintf(`max(resets(process_start_time_seconds{job=%q}[%s]))`, job, win))
		if rev.OK {
			return apiserverTrust{
				Reversals: int(rev.Value),
				Signal:    apiserverSignalStartTime,
				ProbeOK:   true,
				Usable:    rev.Value == 0,
			}
		}
	}
	resets := inst(fmt.Sprintf(`quantile(0.5, resets(apiserver_request_total[%s]))`, win))
	samples := inst(fmt.Sprintf(`quantile(0.5, count_over_time(apiserver_request_total[%s]))`, win))
	if !resets.OK || !samples.OK || samples.Value < apiserverMinResetSamples {
		return apiserverTrust{ProbeOK: false, Usable: true}
	}
	frac := resets.Value / samples.Value
	return apiserverTrust{
		ResetFraction: frac,
		Signal:        apiserverSignalResets,
		ProbeOK:       true,
		Usable:        frac <= apiserverMaxResetFraction,
	}
}

// apiserverJob finds the job label the apiserver's metrics carry, so the start
// time probed is the apiserver's and not some other target's. Discovering it
// beats matching a name pattern, which differs between Prometheus deployments.
func (c *clients) apiserverJob(ctx context.Context, cfg Config) string {
	best := ""
	bestN := 0.0
	for _, s := range c.promQueryVector(ctx, cfg, `count by (job) (apiserver_request_total)`) {
		if j := s.Metric["job"]; j != "" && s.Value > bestN {
			best, bestN = j, s.Value
		}
	}
	return best
}

type latencyStats struct {
	Count int     `json:"count"`
	Min   float64 `json:"min_s"`
	P50   float64 `json:"p50_s"`
	P90   float64 `json:"p90_s"`
	P99   float64 `json:"p99_s"`
	Max   float64 `json:"max_s"`
	Mean  float64 `json:"mean_s"`
}

type componentUsage struct {
	Name string `json:"name"`
	Pods int    `json:"pods"`
	// Replaced counts pods that had series in the window but no longer exist,
	// and whose usage is therefore excluded from the totals below.
	Replaced            int     `json:"replaced_pods,omitempty"`
	CPUMilli            int64   `json:"cpu_millicores"`
	MemMi               int64   `json:"mem_mib"`
	Source              string  `json:"source,omitempty"`
	NVSentinel          bool    `json:"nvsentinel,omitempty"`
	CPUMilliPerKwokNode float64 `json:"cpu_millicores_per_kwok_node,omitempty"`
	MemMiPerKwokNode    float64 `json:"mem_mib_per_kwok_node,omitempty"`
}

// fleetSizing answers "how much CPU and memory does NVSentinel need for N
// nodes with P pods per node?" for the live fleet. P is the mean pod count on
// KWOK (simulated GPU) nodes — not harness connector-pool pods on real nodes.
type fleetSizing struct {
	Question               string           `json:"question"`
	NKwok                  int              `json:"n_kwok_nodes"`
	NKwokReady             int              `json:"n_kwok_ready"`
	NReal                  int              `json:"n_real_nodes"`
	PodsOnKwok             int              `json:"pods_on_kwok"`
	PodsOnKwokDaemonSet    int              `json:"pods_on_kwok_daemonset"`
	PodsOnKwokOther        int              `json:"pods_on_kwok_non_daemonset"`
	PodsPerKwokNode        float64          `json:"p_pods_per_kwok_node"`
	PSource                string           `json:"p_source,omitempty"`
	Window                 string           `json:"window,omitempty"`
	NVSCPUMilli            int64            `json:"nvsentinel_cpu_millicores"`
	NVSMemMi               int64            `json:"nvsentinel_mem_mib"`
	NVSCPUMilliPerKwokNode float64          `json:"nvsentinel_cpu_millicores_per_kwok_node"`
	NVSMemMiPerKwokNode    float64          `json:"nvsentinel_mem_mib_per_kwok_node"`
	ClusterCPUPct          float64          `json:"cluster_cpu_pct,omitempty"`
	ClusterMemPct          float64          `json:"cluster_mem_pct,omitempty"`
	ClusterMetricsOK       bool             `json:"cluster_metrics_ok"`
	ClusterMetricsSource   string           `json:"cluster_metrics_source,omitempty"`
	Components             []componentUsage `json:"components"`
	Note                   string           `json:"note,omitempty"`
}

type nodeStats struct {
	Total     int `json:"total"`
	Kwok      int `json:"kwok"`
	KwokReady int `json:"kwok_ready"`
	Cordoned  int `json:"cordoned"`
	// Cordon-convergence span: derived from the timeAdded of the auto-applied
	// node.kubernetes.io/unschedulable taint across cordoned KWOK nodes. This is
	// the wall-clock the remediation storm took to cordon the fleet (nodes carry
	// no cordon timestamp on spec.unschedulable, but the taint does).
	CordonWithTS  int     `json:"cordon_with_timestamp"`
	CordonFirstTS string  `json:"cordon_first_ts,omitempty"`
	CordonLastTS  string  `json:"cordon_last_ts,omitempty"`
	CordonSpanSec float64 `json:"cordon_span_seconds"`
}

type crStats struct {
	Total   int            `json:"total"`
	Phase   map[string]int `json:"phase"`
	Latency latencyStats   `json:"latency"`
	// Creation-convergence span: first→last CR creationTimestamp for this run's
	// nodes. This is the wall-clock the remediation engine took to create the
	// fleet's CRs (distinct from each CR's own start→complete latency).
	CreateWithTS  int     `json:"create_with_timestamp"`
	CreateFirstTS string  `json:"create_first_ts,omitempty"`
	CreateLastTS  string  `json:"create_last_ts,omitempty"`
	CreateSpanSec float64 `json:"create_span_seconds"`
}

type mongoStats struct {
	OK        bool   `json:"ok"`
	TotalDocs int64  `json:"total_docs"`
	Note      string `json:"note,omitempty"`
}

// ceilingArtifact is the P0.2c sweep record written by `harnessctl ceiling`
// (results/p0.2-ceiling-sweep.json). report loads it when present so the
// node-ceiling scenario appears in the same document as the run metrics.
type ceilingArtifact struct {
	Targets               []int         `json:"targets"`
	Steps                 []ceilingStep `json:"steps"`
	CeilingNodesProven    int           `json:"ceiling_nodes_proven"`
	FirstOverGuardrail    *ceilingStep  `json:"first_over_guardrail"`
	ListP99Guardrail      float64       `json:"list_p99_guardrail"`
	APIServerP99Guardrail float64       `json:"apiserver_p99_guardrail"`
	KwokCPUGuardrail      float64       `json:"kwok_cpu_guardrail_cores"`
	ClusterCPUGuardrail   float64       `json:"cluster_cpu_guardrail_pct"`
	ClusterMemGuardrail   float64       `json:"cluster_mem_guardrail_pct"`
}

type reportData struct {
	Title        string               `json:"title"`
	Cluster      string               `json:"cluster"`
	KubeVersion  string               `json:"kube_version,omitempty"`
	GeneratedAt  string               `json:"generated_at_utc"`
	Window       string               `json:"window"`
	Footprint    map[string]int       `json:"footprint"`
	Nodes        nodeStats            `json:"nodes"`
	APIServer    map[string]metric    `json:"apiserver"`
	Etcd         map[string]metric    `json:"etcd"`
	GPUReset     crStats              `json:"gpureset_crs"`
	RebootNode   crStats              `json:"rebootnode_crs"`
	ResetJobs    latencyStats         `json:"reset_jobs"`
	ResetJobsN   map[string]int       `json:"reset_jobs_counts"`
	Components   []componentUsage     `json:"components"`
	Sizing       fleetSizing          `json:"sizing"`
	A1           a1Sizing             `json:"a1_component_sizing"`
	Mongo        mongoStats           `json:"mongodb"`
	APIServerOK  apiserverTrust       `json:"apiserver_trust"`
	Ceiling      string               `json:"ceiling_attribution"`
	CeilingSweep *ceilingArtifact     `json:"ceiling_sweep,omitempty"`
	NodeCeiling  *nodeCeilingArtifact `json:"node_ceiling,omitempty"`
	Reconcile    *ReconcileReport     `json:"reconcile,omitempty"`
	Guardrails   map[string]float64   `json:"guardrails"`
	Notes        map[string]string    `json:"notes,omitempty"`
}

// loadCeilingSweep reads the P0.2c sweep artifact if a prior `harnessctl ceiling`
// run wrote one; returns nil when absent so the section is simply omitted.
func loadCeilingSweep(dir string) *ceilingArtifact {
	b, err := os.ReadFile(filepath.Join(dir, "p0.2-ceiling-sweep.json"))
	if err != nil {
		return nil
	}
	var a ceilingArtifact
	if err := json.Unmarshal(b, &a); err != nil || len(a.Steps) == 0 {
		return nil
	}
	return &a
}

// nodeCeilingArtifact is the single-rung P0.2 record written by
// `harnessctl scale-nodes` (results/p0.2-node-ceiling.json). report renders it
// as the P0.2 scenario when no multi-rung ceiling sweep exists.
type nodeCeilingArtifact struct {
	TargetNodes     int     `json:"target_nodes"`
	ReadyNodes      int     `json:"ready_nodes"`
	Failed          int     `json:"failed"`
	TimeToReadySec  float64 `json:"time_to_ready_seconds"`
	ReadyAtUTC      string  `json:"ready_at_utc,omitempty"`
	APIServerP99Sec string  `json:"apiserver_p99_seconds"`
	GuardrailP99Sec float64 `json:"guardrail_p99_seconds"`
	ClusterCPUPct   float64 `json:"cluster_cpu_pct"`
	ClusterMemPct   float64 `json:"cluster_mem_pct"`
	ClusterCPUCores float64 `json:"cluster_cpu_used_cores"`
	ClusterMemMi    float64 `json:"cluster_mem_used_mi"`
	ClusterRealNode int     `json:"cluster_real_nodes"`
	MetricsPresent  bool    `json:"cluster_metrics_present"`
}

// loadNodeCeiling reads the single-rung scale-nodes artifact if present.
func loadNodeCeiling(dir string) *nodeCeilingArtifact {
	b, err := os.ReadFile(filepath.Join(dir, "p0.2-node-ceiling.json"))
	if err != nil {
		return nil
	}
	var a nodeCeilingArtifact
	if err := json.Unmarshal(b, &a); err != nil || a.TargetNodes == 0 {
		return nil
	}
	return &a
}

// fleetReadyArtifact records when the fleet a run measures finished becoming
// Ready (results/fleet-ready.json), written by whoever provisioned it.
//
// It exists so the report window can be anchored to a fleet without also
// claiming a P0.2 node-ceiling measurement. Those two were one artifact while
// harnessctl built its own fleet, which was fine until provisioning moved out:
// nothing writes p0.2-node-ceiling.json any more, so a run had no anchor
// either, and `--window auto` silently fell back to a fixed lookback untied to
// any fleet — the very thing auto exists to avoid.
type fleetReadyArtifact struct {
	ReadyAtUTC string `json:"ready_at_utc"`
	ReadyNodes int    `json:"ready_nodes"`
}

// loadFleetReady reads the fleet-ready anchor if present.
func loadFleetReady(dir string) *fleetReadyArtifact {
	b, err := os.ReadFile(filepath.Join(dir, "fleet-ready.json"))
	if err != nil {
		return nil
	}
	var a fleetReadyArtifact
	if err := json.Unmarshal(b, &a); err != nil || a.ReadyAtUTC == "" {
		return nil
	}
	return &a
}

// loadReconcileReport reads the P0.3 reconcile artifact written by
// `harnessctl reconcile` (results/reconcile-report.json) so the report surfaces
// zero-loss accounting + end-to-end NodeName attribution directly, without
// needing CLI access to the cluster-internal mTLS event store.
func loadReconcileReport(dir string) *ReconcileReport {
	b, err := os.ReadFile(filepath.Join(dir, "reconcile-report.json"))
	if err != nil {
		return nil
	}
	var r ReconcileReport
	if err := json.Unmarshal(b, &r); err != nil || r.RunID == "" {
		return nil
	}
	return &r
}

// ---- collection -----------------------------------------------------------

func (c *clients) collectReport(ctx context.Context, cfg Config, title string) reportData {
	d := reportData{
		Title:       title,
		Cluster:     c.rest.Host,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Window:      cfg.ReportWindow,
		Notes:       map[string]string{},
	}
	if d.Cluster == "" {
		d.Cluster = "(current-context)"
	}
	if v, err := c.kube.Discovery().ServerVersion(); err == nil {
		d.KubeVersion = v.GitVersion
	}
	d.Guardrails = map[string]float64{"apiserver_p99_s": cfg.MaxAPIServerP99}
	d.CeilingSweep = loadCeilingSweep(cfg.ResultsDir)
	d.NodeCeiling = loadNodeCeiling(cfg.ResultsDir)
	d.Reconcile = loadReconcileReport(cfg.ResultsDir)

	win := cfg.ReportWindow
	peak := func(inner string) metric {
		v, ok := c.promInstantQuery(ctx, cfg, fmt.Sprintf("max_over_time((%s)[%s:1m])", inner, win))
		return metric{v, ok}
	}
	inst := func(q string) metric {
		v, ok := c.promInstantQuery(ctx, cfg, q)
		return metric{v, ok}
	}

	// --- API server (peaks over the window) ---
	d.APIServer = map[string]metric{
		"read_p99_s":       peak(`histogram_quantile(0.99, sum(rate(apiserver_request_duration_seconds_bucket{verb=~"GET|LIST"}[5m])) by (le))`),
		"write_p99_s":      peak(`histogram_quantile(0.99, sum(rate(apiserver_request_duration_seconds_bucket{verb=~"POST|PUT|PATCH|DELETE"}[5m])) by (le))`),
		"all_p99_s":        peak(apiserverP99WindowQuery("5m")),
		"list_nodes_p99_s": peak(apiserverResourceP99Query("LIST", "nodes", "5m")),
		"list_nodes_rate":  peak(`sum(rate(apiserver_request_total{verb="LIST",resource="nodes"}[5m]))`),
		"req_rate":         peak(`sum(rate(apiserver_request_total[5m]))`),
		"inflight":         peak(`sum(apiserver_current_inflight_requests)`),
		"err5xx_rate":      peak(`sum(rate(apiserver_request_total{code=~"5.."}[5m]))`),
	}
	d.APIServerOK = apiserverCounterTrust(inst, c.apiserverJob(ctx, cfg), win)

	// --- etcd (often unavailable on managed control planes) ---
	etcdSize := inst(`sum(apiserver_storage_db_total_size_in_bytes)/1024/1024`)
	if !etcdSize.OK {
		etcdSize = peak(`etcd_db_total_size_in_bytes/1024/1024`)
	}
	d.Etcd = map[string]metric{
		"db_size_mib":          etcdSize,
		"wal_fsync_p99_s":      peak(`histogram_quantile(0.99, sum(rate(etcd_disk_wal_fsync_duration_seconds_bucket[5m])) by (le))`),
		"backend_commit_p99_s": peak(`histogram_quantile(0.99, sum(rate(etcd_disk_backend_commit_duration_seconds_bucket[5m])) by (le))`),
	}
	if !d.Etcd["db_size_mib"].OK {
		d.Notes["etcd"] = "etcd metrics not exposed (managed control plane) — collected none"
	}

	// Janitor workqueue/reconcile metrics are deliberately NOT collected.
	//
	// They measured the CSP simulator, not NVSentinel. With csp=kind the
	// provider fakes a reboot by sleeping 3-5s, and the janitor makes that call
	// synchronously on controller-runtime's default single reconcile worker, so
	// the queue depth and reconcile rate are fixed by that sleep (~0.3/s) at
	// any fleet size. Reporting them invited the reader — and the ceiling
	// attribution — to call the janitor a scaling bottleneck when the number
	// came from the test harness.

	// --- nodes / cordon (authoritative from a live LIST) ---
	d.Nodes = c.collectNodeStats(ctx)

	// --- object footprint ---
	d.GPUReset = c.collectGPUResetStats(ctx, cfg.NodePrefix)
	d.RebootNode = c.collectRebootNodeStats(ctx, cfg.NodePrefix)
	jobsTotal, jobsDone, jobLat := c.collectResetJobStats(ctx, cfg.JanitorNamespace)
	d.ResetJobs = jobLat
	d.ResetJobsN = map[string]int{"total": jobsTotal, "succeeded": jobsDone}
	d.Footprint = map[string]int{
		"kwok_nodes":       d.Nodes.Kwok,
		"total_nodes":      d.Nodes.Total,
		"gpureset_crs":     d.GPUReset.Total,
		"rebootnode_crs":   d.RebootNode.Total,
		"reset_jobs":       jobsTotal,
		"node_lock_leases": c.leaseCount(ctx, cfg.JanitorNamespace),
		"connector_pool":   c.podCountByPrefix(ctx, cfg.NVSNamespace, connectorPoolName),
	}

	// --- component CPU/memory (metrics-server, kubelet, or Prometheus) ---
	d.Components = c.collectComponents(ctx, cfg)
	d.Sizing = c.buildFleetSizing(ctx, cfg, d)

	// --- MongoDB (best-effort) ---
	d.Mongo = mongoStatsBestEffort(ctx, cfg)

	// --- ceiling attribution ---
	d.Ceiling = attributeReportCeiling(cfg, d)
	return d
}

// collectNodeStats pages the whole fleet. A 50k-node walk takes ~100 pages, long
// enough that etcd can compact the continue token out from under us
// ("continue parameter is too old"). Returning the pages gathered so far would
// report a fraction of the fleet as the whole of it — and N is the denominator of
// every per-node sizing rate — so an expired token restarts the walk from scratch
// instead.
func (c *clients) collectNodeStats(ctx context.Context) nodeStats {
	ns, err := walkWithRestarts("report: list nodes", func() (nodeStats, error) {
		return c.collectNodeStatsOnce(ctx)
	})
	if err != nil {
		warnf("%v; node counts unavailable", err)
		return nodeStats{}
	}
	return ns
}

func (c *clients) collectNodeStatsOnce(ctx context.Context) (nodeStats, error) {
	var ns nodeStats
	opts := metav1.ListOptions{Limit: kwokListPageSize}
	var cordonFirst, cordonLast time.Time
	for {
		list, err := c.kube.CoreV1().Nodes().List(ctx, opts)
		if err != nil {
			return nodeStats{}, err
		}
		for i := range list.Items {
			n := &list.Items[i]
			ns.Total++
			isKwok := n.Labels["type"] == "kwok"
			if isKwok {
				ns.Kwok++
			}
			if n.Spec.Unschedulable {
				ns.Cordoned++
				// The node-lifecycle controller stamps timeAdded on the
				// node.kubernetes.io/unschedulable taint when a node is cordoned;
				// use it as the per-node cordon time (KWOK nodes only, to scope to
				// the simulated fleet).
				if isKwok {
					if ta := cordonTaintTime(n); !ta.IsZero() {
						ns.CordonWithTS++
						if cordonFirst.IsZero() || ta.Before(cordonFirst) {
							cordonFirst = ta
						}
						if ta.After(cordonLast) {
							cordonLast = ta
						}
					}
				}
			}
			if isKwok {
				for _, cond := range n.Status.Conditions {
					if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
						ns.KwokReady++
					}
				}
			}
		}
		if list.Continue == "" {
			break
		}
		opts.Continue = list.Continue
	}
	if !cordonFirst.IsZero() && !cordonLast.IsZero() {
		ns.CordonFirstTS = cordonFirst.UTC().Format(time.RFC3339)
		ns.CordonLastTS = cordonLast.UTC().Format(time.RFC3339)
		ns.CordonSpanSec = cordonLast.Sub(cordonFirst).Seconds()
	}
	return ns, nil
}

// cordonTaintTime returns the timeAdded of the node.kubernetes.io/unschedulable
// taint (the cordon timestamp), or the zero time if absent.
func cordonTaintTime(n *corev1.Node) time.Time {
	for _, t := range n.Spec.Taints {
		if t.Key == "node.kubernetes.io/unschedulable" && t.TimeAdded != nil {
			return t.TimeAdded.Time
		}
	}
	return time.Time{}
}

func (c *clients) collectGPUResetStats(ctx context.Context, nodePrefix string) crStats {
	return c.collectRemediationCRStats(ctx, gpuresetGVR, nodePrefix, gpuResetPhase)
}

// collectRebootNodeStats measures the RebootNode CRs that the DEFAULT fatal
// event produces. The injector's default `--fatal-event node-reboot` carries
// RecommendedAction RESTART_BM, which the node-drainer turns into a RebootNode;
// only `--fatal-event gpu-reset` (COMPONENT_RESET) yields a GPUReset. Reporting
// GPUReset alone therefore showed "no data" for P0.4 on every default run even
// when remediation had in fact cordoned, drained and rebooted the fleet.
func (c *clients) collectRebootNodeStats(ctx context.Context, nodePrefix string) crStats {
	return c.collectRemediationCRStats(ctx, rebootGVR, nodePrefix, rebootNodePhase)
}

// gpuResetPhase reads the phase GPUReset publishes directly.
func gpuResetPhase(obj map[string]any) string {
	phase, _, _ := unstructured.NestedString(obj, "status", "phase")
	if phase == "" {
		return "(pending)"
	}
	return phase
}

// rebootNodePhase derives a phase for RebootNode, which publishes none: its
// status carries only startTime, completionTime and a condition list. The
// lifecycle buckets below are what the remediation-state table wants anyway,
// and they make a stalled reboot storm visible as a pile-up in "(pending)".
func rebootNodePhase(obj map[string]any) string {
	if comp, _, _ := unstructured.NestedString(obj, "status", "completionTime"); comp != "" {
		return "Completed"
	}
	if start, _, _ := unstructured.NestedString(obj, "status", "startTime"); start != "" {
		return "InProgress"
	}
	return "(pending)"
}

// collectRemediationCRStats counts one janitor CR kind for this run's nodes and
// measures both per-CR start→complete latency and the fleet-wide first→last
// creation span. phaseOf maps a CR to its state bucket, since the kinds do not
// agree on how (or whether) they publish one.
func (c *clients) collectRemediationCRStats(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	nodePrefix string,
	phaseOf func(map[string]any) string,
) crStats {
	st := crStats{Phase: map[string]int{}}
	var lat []float64
	var createFirst, createLast time.Time
	cont := ""
	for {
		list, err := c.dynamic.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 500, Continue: cont})
		if err != nil {
			warnf("report: list %s: %v", gvr.Resource, err)
			break
		}
		for i := range list.Items {
			obj := list.Items[i].Object
			node, _, _ := unstructured.NestedString(obj, "spec", "nodeName")
			if nodePrefix != "" && !strings.HasPrefix(node, nodePrefix) {
				continue
			}
			st.Total++
			st.Phase[phaseOf(obj)]++
			if cts := list.Items[i].GetCreationTimestamp(); !cts.IsZero() {
				st.CreateWithTS++
				if createFirst.IsZero() || cts.Time.Before(createFirst) {
					createFirst = cts.Time
				}
				if cts.Time.After(createLast) {
					createLast = cts.Time
				}
			}
			start, _, _ := unstructured.NestedString(obj, "status", "startTime")
			comp, _, _ := unstructured.NestedString(obj, "status", "completionTime")
			if start != "" && comp != "" {
				if t0, e1 := time.Parse(time.RFC3339, start); e1 == nil {
					if t1, e2 := time.Parse(time.RFC3339, comp); e2 == nil {
						lat = append(lat, t1.Sub(t0).Seconds())
					}
				}
			}
		}
		cont = list.GetContinue()
		if cont == "" {
			break
		}
	}
	if !createFirst.IsZero() && !createLast.IsZero() {
		st.CreateFirstTS = createFirst.UTC().Format(time.RFC3339)
		st.CreateLastTS = createLast.UTC().Format(time.RFC3339)
		st.CreateSpanSec = createLast.Sub(createFirst).Seconds()
	}
	st.Latency = computeLatency(lat)
	return st
}

func (c *clients) collectResetJobStats(ctx context.Context, ns string) (int, int, latencyStats) {
	var lat []float64
	total, done := 0, 0
	cont := ""
	for {
		jl, err := c.kube.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{Limit: 500, Continue: cont})
		if err != nil {
			warnf("report: list jobs in %s: %v", ns, err)
			break
		}
		for i := range jl.Items {
			j := &jl.Items[i]
			if !strings.Contains(j.Name, "reset-job") {
				continue
			}
			total++
			if j.Status.Succeeded > 0 {
				done++
			}
			if j.Status.StartTime != nil && j.Status.CompletionTime != nil {
				lat = append(lat, j.Status.CompletionTime.Sub(j.Status.StartTime.Time).Seconds())
			}
		}
		cont = jl.Continue
		if cont == "" {
			break
		}
	}
	return total, done, computeLatency(lat)
}

func (c *clients) leaseCount(ctx context.Context, ns string) int {
	l, err := c.kube.CoordinationV1().Leases(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0
	}
	return len(l.Items)
}

func (c *clients) podCountByPrefix(ctx context.Context, ns, prefix string) int {
	pods, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0
	}
	n := 0
	for i := range pods.Items {
		if strings.HasPrefix(pods.Items[i].Name, prefix) {
			n++
		}
	}
	return n
}

// ---- metrics-server component usage ---------------------------------------

type podUsageItem struct {
	name     string
	cpuMilli int64
	memMi    int64
	source   string
	// gone marks a pod that has series in the window but no longer exists, so
	// its usage must not be added to its component's total. See dropReplacedPods.
	gone bool
}

// podUsage reads per-pod CPU/memory from metrics.k8s.io (metrics-server) for a
// namespace, since not every Prometheus scrapes cadvisor/container metrics.
func (c *clients) podUsage(ctx context.Context, namespace string) ([]podUsageItem, error) {
	raw, err := c.kube.CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces", namespace, "pods").
		DoRaw(ctx)
	if err != nil {
		return nil, err
	}
	var pm struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Containers []struct {
				Usage struct {
					CPU    string `json:"cpu"`
					Memory string `json:"memory"`
				} `json:"usage"`
			} `json:"containers"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &pm); err != nil {
		return nil, err
	}
	out := make([]podUsageItem, 0, len(pm.Items))
	for _, it := range pm.Items {
		var cpu, mem int64
		for _, ctr := range it.Containers {
			if q, err := resource.ParseQuantity(ctr.Usage.CPU); err == nil {
				cpu += q.MilliValue()
			}
			if q, err := resource.ParseQuantity(ctr.Usage.Memory); err == nil {
				mem += q.Value() / (1024 * 1024)
			}
		}
		out = append(out, podUsageItem{name: it.Metadata.Name, cpuMilli: cpu, memMi: mem, source: "metrics-server"})
	}
	return out, nil
}

// componentTarget maps a friendly name to (namespace, pod-name prefix).
// excludePrefix skips a more-specific sibling (janitor vs janitor-provider).
// sut marks NVSentinel product processes that count toward fleet sizing.
type componentTarget struct {
	name, ns, prefix, exclude string
	sut                       bool
}

func nvsComponentTargets(cfg Config) []componentTarget {
	return []componentTarget{
		{"janitor-controller", cfg.JanitorNamespace, "dgxc-janitor-controller-manager", "", true},
		{"kwok-controller", "kube-system", "kwok-controller", "", false},
		{"node-drainer", cfg.NVSNamespace, "node-drainer", "", true},
		{"fault-quarantine", cfg.NVSNamespace, "fault-quarantine", "", true},
		{"fault-remediation", cfg.NVSNamespace, "fault-remediation", "", true},
		{"labeler", cfg.NVSNamespace, "labeler", "", true},
		{"health-events-analyzer", cfg.NVSNamespace, "health-events-analyzer", "", true},
		{"janitor", cfg.NVSNamespace, "janitor-", "janitor-provider", true},
		{"kubernetes-object-monitor", cfg.NVSNamespace, "kubernetes-object-monitor", "", true},
		{"platform-connectors", cfg.NVSNamespace, "platform-connectors", "", true},
		{"mongodb", cfg.NVSNamespace, "mongodb", "", true},
		{"connector-pool", cfg.NVSNamespace, connectorPoolName, "", false},
		{"pool-injector", cfg.NVSNamespace, poolInjectorDaemonSet, "", false},
	}
}

func matchPodPrefix(name, prefix, exclude string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	return exclude == "" || !strings.HasPrefix(name, exclude)
}

func (c *clients) collectComponents(ctx context.Context, cfg Config) []componentUsage {
	targets := nvsComponentTargets(cfg)
	byNS := map[string][]podUsageItem{}
	srcByNS := map[string]string{}
	out := make([]componentUsage, 0, len(targets))
	for _, t := range targets {
		items, ok := byNS[t.ns]
		if !ok {
			items, srcByNS[t.ns] = c.podUsageBestEffort(ctx, cfg, t.ns)
			byNS[t.ns] = items
		}
		cu := componentUsage{Name: t.name, NVSentinel: t.sut}
		srcCount := map[string]int{}
		for _, it := range items {
			if matchPodPrefix(it.name, t.prefix, t.exclude) {
				if it.gone {
					cu.Replaced++
					continue
				}
				cu.Pods++
				cu.CPUMilli += it.cpuMilli
				cu.MemMi += it.memMi
				if it.source != "" {
					srcCount[it.source]++
				}
			}
		}
		switch len(srcCount) {
		case 0:
			cu.Source = srcByNS[t.ns]
		case 1:
			for s := range srcCount {
				cu.Source = s
			}
		default:
			cu.Source = "prometheus+kubelet"
		}
		out = append(out, cu)
	}
	return out
}

func (c *clients) podUsageBestEffort(ctx context.Context, cfg Config, namespace string) ([]podUsageItem, string) {
	if items, src := c.podUsageFromPrometheus(ctx, cfg, namespace); len(items) > 0 {
		if kube, err := c.podUsageFromKubelet(ctx, namespace); err == nil {
			if merged, filled := mergePodUsageGaps(items, kube); filled {
				return merged, src + "+kubelet-summary"
			}
		}
		return items, src
	}
	if items, err := c.podUsageFromKubelet(ctx, namespace); err == nil && len(items) > 0 {
		return items, "kubelet-summary"
	} else if err != nil {
		warnf("report: kubelet summary for ns %s: %v", namespace, err)
	}
	msCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	items, err := c.podUsage(msCtx, namespace)
	cancel()
	if err == nil && len(items) > 0 {
		return items, "metrics-server"
	}
	if err != nil {
		warnf("report: metrics-server for ns %s: %v", namespace, err)
	}
	return nil, ""
}

func reportWindow(cfg Config) string {
	if cfg.ReportWindow == "" {
		return "3h"
	}
	return cfg.ReportWindow
}

func promSubqueryPeak(inner, win string, podLabel string) string {
	return fmt.Sprintf("max_over_time((sum by (%s) (%s))[%s:1m])", podLabel, inner, win)
}

func promProcessMemInner(ns string) string {
	return fmt.Sprintf(`process_resident_memory_bytes{namespace="%s"}`, ns)
}

func promProcessCPUInner(ns string) string {
	return fmt.Sprintf(`rate(process_cpu_seconds_total{namespace="%s"}[5m])`, ns)
}

func promContainerMemInner(ns string) string {
	return fmt.Sprintf(`container_memory_working_set_bytes{namespace="%s",container!="",container!="POD"}`, ns)
}

func promContainerCPUInner(ns string) string {
	return fmt.Sprintf(`rate(container_cpu_usage_seconds_total{namespace="%s",container!="",container!="POD"}[5m])`, ns)
}

func (c *clients) promPeakByPod(ctx context.Context, cfg Config, inner, win string) []promVectorSample {
	if s := c.promQueryVector(ctx, cfg, promSubqueryPeak(inner, win, "pod")); len(s) > 0 {
		return s
	}
	return c.promQueryVector(ctx, cfg, promSubqueryPeak(inner, win, "exported_pod"))
}

// Source strings name the memory and CPU series separately, because the two
// fall back independently. Only the memory wording drives the "RSS, not working
// set" caveat, so a run with accurate memory and fallback CPU must not be
// labelled as though its memory were RSS.
const (
	srcPromContainer    = "prometheus-container"
	srcPromProcess      = "prometheus-process"
	srcPromContainerMem = "prometheus-container-mem+process-cpu"
	srcPromProcessMem   = "prometheus-process-mem+container-cpu"
)

func (c *clients) podUsageFromPrometheus(ctx context.Context, cfg Config, namespace string) ([]podUsageItem, string) {
	win := reportWindow(cfg)
	mem := c.promPeakByPod(ctx, cfg, promContainerMemInner(namespace), win)
	cpu := c.promPeakByPod(ctx, cfg, promContainerCPUInner(namespace), win)
	memFromContainer, cpuFromContainer := len(mem) > 0, len(cpu) > 0
	if !memFromContainer {
		mem = c.promPeakByPod(ctx, cfg, promProcessMemInner(namespace), win)
	}
	if !cpuFromContainer {
		cpu = c.promPeakByPod(ctx, cfg, promProcessCPUInner(namespace), win)
	}
	src := promUsageSource(memFromContainer, cpuFromContainer)
	out := mergePromUsage(mem, cpu)
	if len(out) == 0 {
		return nil, ""
	}
	for i := range out {
		out[i].source = src
	}
	if live, err := c.livePodNames(ctx, namespace); err == nil {
		dropReplacedPods(out, live)
	} else {
		// Without the live set there is no way to tell a replaced pod from a
		// running one, and dropping nothing is the safe direction: an
		// overstated component is visible and caveated, whereas silently
		// discarding a live pod understates the figure with no trace.
		warnf("report: listing pods in ns %s to exclude replaced pods: %v", namespace, err)
	}
	return out, src
}

// livePodNames is the set of pods that exist in the namespace right now.
func (c *clients) livePodNames(ctx context.Context, namespace string) (map[string]bool, error) {
	list, err := c.kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(list.Items))
	for i := range list.Items {
		live[list.Items[i].Name] = true
	}
	return live, nil
}

// dropReplacedPods marks the pods that have series in the window but are gone
// from the cluster.
//
// Peaks are per-pod and components sum every pod matching their prefix, so a
// rollout inside the window contributes the old pod and the new one and the
// component reads as roughly double. That is not a small distortion: a 1000-node
// AKS run measured platform-connectors at 1.2 GB across 56 pods for a 28-pod
// DaemonSet, and NVSentinel's component total at 2.2 GB against an actual
// 1.0 GB. It is also not a question the window can settle, because a terminating
// pod keeps exposing metrics for a minute or two after it is replaced, and
// moving the window past every restart would discard the storm the run exists
// to measure whenever a component restarts mid-run.
//
// Judging by existence now, rather than by counting pods against a replica
// count, is what makes this work for a DaemonSet: its pod count is legitimately
// large and varies with the node pool, so there is no fixed number to compare
// against.
//
// The cost is that a pod replaced mid-window is measured only over the part of
// the window it existed, so its peak is a floor if the real peak predated it.
// a1Caveats names the count so that is visible rather than assumed.
func dropReplacedPods(items []podUsageItem, live map[string]bool) {
	for i := range items {
		if !live[items[i].name] {
			items[i].gone = true
		}
	}
}

func promUsageSource(memFromContainer, cpuFromContainer bool) string {
	switch {
	case memFromContainer && cpuFromContainer:
		return srcPromContainer
	case memFromContainer:
		return srcPromContainerMem
	case cpuFromContainer:
		return srcPromProcessMem
	default:
		return srcPromProcess
	}
}

func podNameFromSample(s promVectorSample) string {
	if p := s.Metric["pod"]; p != "" {
		return p
	}
	return s.Metric["exported_pod"]
}

func mergePromUsage(rss, cpu []promVectorSample) []podUsageItem {
	byPod := map[string]*podUsageItem{}
	for _, s := range rss {
		pod := podNameFromSample(s)
		if pod == "" {
			continue
		}
		it := byPod[pod]
		if it == nil {
			it = &podUsageItem{name: pod}
			byPod[pod] = it
		}
		it.memMi += int64(s.Value) / (1024 * 1024)
	}
	for _, s := range cpu {
		pod := podNameFromSample(s)
		if pod == "" {
			continue
		}
		it := byPod[pod]
		if it == nil {
			it = &podUsageItem{name: pod}
			byPod[pod] = it
		}
		it.cpuMilli += int64(s.Value * 1000)
	}
	out := make([]podUsageItem, 0, len(byPod))
	for _, it := range byPod {
		out = append(out, *it)
	}
	return out
}

// mergePodUsageGaps appends kubelet (or other) samples for pods Prometheus did
// not scrape. Prom peaks are left untouched.
func mergePodUsageGaps(dst []podUsageItem, extra []podUsageItem) ([]podUsageItem, bool) {
	have := map[string]bool{}
	for _, it := range dst {
		have[it.name] = true
	}
	filled := false
	for _, it := range extra {
		if have[it.name] {
			continue
		}
		dst = append(dst, it)
		have[it.name] = true
		filled = true
	}
	return dst, filled
}

func (c *clients) podUsageFromKubelet(ctx context.Context, namespace string) ([]podUsageItem, error) {
	list, err := c.kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	summaries := map[string]*kubeletSummary{}
	var firstErr error
	out := make([]podUsageItem, 0, len(list.Items))
	for i := range list.Items {
		p := &list.Items[i]
		node := p.Spec.NodeName
		if node == "" || isKwokNodeName(node) {
			continue
		}
		sum, cached := summaries[node]
		if !cached {
			raw, err := c.kubeletStatsSummaryRaw(ctx, node)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				summaries[node] = nil
				continue
			}
			var parsed kubeletSummary
			if err := json.Unmarshal(raw, &parsed); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				summaries[node] = nil
				continue
			}
			sum = &parsed
			summaries[node] = sum
		}
		if sum == nil {
			continue
		}
		for _, ps := range sum.Pods {
			if ps.PodRef.Name != p.Name {
				continue
			}
			if ps.PodRef.Namespace != "" && ps.PodRef.Namespace != namespace {
				continue
			}
			out = append(out, podUsageItem{
				name:     p.Name,
				cpuMilli: nanoCoresToMilli(ps.CPU.UsageNanoCores),
				memMi:    ps.Memory.WorkingSetBytes / (1024 * 1024),
				source:   "kubelet-summary",
			})
			break
		}
	}
	if len(out) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, fmt.Errorf("no kubelet stats for pods in %s", namespace)
	}
	return out, nil
}

func isKwokNodeName(name string) bool {
	return strings.HasPrefix(name, "kwok-")
}

func (c *clients) buildFleetSizing(ctx context.Context, cfg Config, d reportData) fleetSizing {
	real := d.Nodes.Total - d.Nodes.Kwok
	if real < 0 {
		real = 0
	}
	s := fleetSizing{
		Question:   "How much CPU and memory does NVSentinel need for a fleet of N nodes with P pods per node?",
		NKwok:      d.Nodes.Kwok,
		NKwokReady: d.Nodes.KwokReady,
		NReal:      real,
		Window:     reportWindow(cfg),
		Components: d.Components,
	}
	s.PodsOnKwok, s.PodsOnKwokDaemonSet, s.PodsOnKwokOther, s.PSource = c.countPodsOnKwok(ctx, cfg)
	if s.NKwok > 0 {
		s.PodsPerKwokNode = float64(s.PodsOnKwok) / float64(s.NKwok)
	}
	for i := range s.Components {
		applyPerNodeRates(&s.Components[i], s.NKwok)
		if s.Components[i].NVSentinel {
			s.NVSCPUMilli += s.Components[i].CPUMilli
			s.NVSMemMi += s.Components[i].MemMi
		}
	}
	if s.NKwok > 0 {
		s.NVSCPUMilliPerKwokNode = float64(s.NVSCPUMilli) / float64(s.NKwok)
		s.NVSMemMiPerKwokNode = float64(s.NVSMemMi) / float64(s.NKwok)
	}
	if util := c.clusterNodeUtil(ctx); util.OK {
		s.ClusterMetricsOK = true
		s.ClusterCPUPct = util.CPUPct
		s.ClusterMemPct = util.MemPct
		s.ClusterMetricsSource = util.Source
	}
	sources := map[string]bool{}
	for _, comp := range s.Components {
		if comp.Pods > 0 && comp.Source != "" {
			sources[comp.Source] = true
		}
	}
	var notes []string
	if s.PodsPerKwokNode == 0 && s.NKwok > 0 {
		notes = append(notes, "P≈0 on KWOK nodes (Node objects only). NVSentinel CPU/mem is still the live cost of watching N nodes. To measure pod-driven cost, schedule P pods per KWOK node and re-run stack report.")
	} else if s.PodsOnKwokOther == 0 && s.PodsOnKwokDaemonSet > 0 {
		notes = append(notes, "P is DaemonSet pods on KWOK nodes (not application replicas). NVSentinel CPU/mem is still primarily a function of N. Schedule non-DS pods per KWOK node to measure pod-driven cost.")
	}
	prom := false
	kube := false
	for src := range sources {
		if strings.HasPrefix(src, "prometheus") {
			prom = true
		}
		if strings.Contains(src, "kubelet") {
			kube = true
		}
	}
	if prom {
		notes = append(notes, fmt.Sprintf("Component CPU/mem are Prometheus peaks (max_over_time over %s).", reportWindow(cfg)))
		if kube {
			notes = append(notes, "Pods Prometheus does not scrape (often Mongo) are filled from a live kubelet working-set sample.")
		}
	} else if kube {
		notes = append(notes, "Prometheus had no process/container series; component CPU/mem fell back to a live kubelet working-set sample.")
	}
	s.Note = strings.Join(notes, " ")
	return s
}

func applyPerNodeRates(cu *componentUsage, nKwok int) {
	if nKwok <= 0 || cu.Pods == 0 {
		return
	}
	cu.CPUMilliPerKwokNode = float64(cu.CPUMilli) / float64(nKwok)
	cu.MemMiPerKwokNode = float64(cu.MemMi) / float64(nKwok)
}

func (c *clients) countPodsOnKwok(ctx context.Context, cfg Config) (total, ds, other int, source string) {
	nodeSel := `node=~"kwok-.*"`
	v, ok := c.promInstantQuery(ctx, cfg, fmt.Sprintf("count(kube_pod_info{%s})", nodeSel))
	if !ok {
		v, ok = c.promInstantQuery(ctx, cfg, `count(kube_pod_info{exported_node=~"kwok-.*"})`)
		if ok {
			nodeSel = `exported_node=~"kwok-.*"`
		}
	}
	if !ok {
		return countPodsOnKwokFromAPI(ctx, c)
	}
	total = int(v)
	source = "prometheus"
	if v, ok := c.promInstantQuery(ctx, cfg, fmt.Sprintf(`count(kube_pod_info{%s,created_by_kind="DaemonSet"})`, nodeSel)); ok {
		ds = int(v)
	}
	if v, ok := c.promInstantQuery(ctx, cfg, fmt.Sprintf(`count(kube_pod_info{%s,created_by_kind!="DaemonSet"})`, nodeSel)); ok {
		other = int(v)
	}
	return total, ds, other, source
}

func countPodsOnKwokFromAPI(ctx context.Context, c *clients) (total, ds, other int, source string) {
	list, err := c.kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		warnf("report: list pods for P: %v", err)
		return 0, 0, 0, ""
	}
	for i := range list.Items {
		p := &list.Items[i]
		if !isKwokNodeName(p.Spec.NodeName) {
			continue
		}
		total++
		if ownerKind(p) == "DaemonSet" {
			ds++
		} else {
			other++
		}
	}
	return total, ds, other, "apiserver-list"
}

func ownerKind(p *corev1.Pod) string {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller {
			return o.Kind
		}
	}
	if len(p.OwnerReferences) > 0 {
		return p.OwnerReferences[0].Kind
	}
	return ""
}

// ---- MongoDB (best-effort) -------------------------------------------------

func mongoStatsBestEffort(ctx context.Context, cfg Config) mongoStats {
	var ms mongoStats
	if cfg.MongoURI == "" {
		ms.Note = "unavailable from CLI (mTLS store is cluster-internal); set HARNESS_MONGO_URI or reconcile in-cluster"
		return ms
	}
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client, err := mongo.Connect(qctx, options.Client().ApplyURI(cfg.MongoURI))
	if err != nil {
		ms.Note = "connect failed: " + err.Error()
		return ms
	}
	defer client.Disconnect(context.Background())
	coll := client.Database(cfg.MongoDB).Collection(cfg.MongoColl)
	n, err := coll.EstimatedDocumentCount(qctx)
	if err != nil {
		ms.Note = "count failed: " + err.Error()
		return ms
	}
	ms.TotalDocs = n
	ms.OK = true
	return ms
}

// ---- helpers ---------------------------------------------------------------

func computeLatency(v []float64) latencyStats {
	var s latencyStats
	if len(v) == 0 {
		return s
	}
	sort.Float64s(v)
	s.Count = len(v)
	s.Min = v[0]
	s.Max = v[len(v)-1]
	s.P50 = pctile(v, 0.50)
	s.P90 = pctile(v, 0.90)
	s.P99 = pctile(v, 0.99)
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	s.Mean = sum / float64(len(v))
	return s
}

func pctile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * q)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func attributeReportCeiling(cfg Config, d reportData) string {
	readP99 := d.APIServer["read_p99_s"]
	listP99 := d.APIServer["list_nodes_p99_s"]
	inflight := d.APIServer["inflight"]
	// Every branch below reads an apiserver latency, so an untrustworthy source
	// invalidates the attribution rather than just one row of it. Naming no
	// ceiling is the honest answer: "control plane is the bound" is the kind of
	// conclusion that gets carried into a sizing decision, and it is worse to
	// state it from a metric off by three orders of magnitude than to say the
	// run could not measure it.
	if !d.APIServerOK.Usable {
		return fmt.Sprintf("No saturation attributed: %s. Re-run against a Prometheus that scrapes each apiserver replica as its own target, or read the ceiling off the KWOK readiness fraction and the client-side LIST timings instead.",
			d.APIServerOK.reason())
	}
	// No remediation-throughput branch: the janitor queue it keyed on was
	// bounded by the csp=kind simulator's synthetic sleep on a single reconcile
	// worker, so a deep queue said nothing about how NVSentinel scales.
	switch {
	case readP99.OK && readP99.Value > cfg.MaxAPIServerP99:
		return fmt.Sprintf("REAL ceiling: apiserver read p99 %.3fs exceeded guardrail %.3fs — control plane (apiserver/etcd) is the bound.", readP99.Value, cfg.MaxAPIServerP99)
	case listP99.OK && listP99.Value > cfg.CeilingListP99:
		return fmt.Sprintf("REAL ceiling: apiserver LIST-nodes p99 %.3fs exceeded guardrail %.3fs — large-collection LIST against apiserver/etcd is the bound for node scale (Phase 2). Overall read p99 stays low (%s) and in-flight peak %s, so it's LIST-fanout cost, not general control-plane saturation.",
			listP99.Value, cfg.CeilingListP99, mfmt(readP99, 1, "s", 3), mfmt(inflight, 1, "", 0))
	default:
		return fmt.Sprintf("No saturation attributed: apiserver read p99=%s, LIST-nodes p99=%s, inflight peak=%s.",
			mfmt(readP99, 1, "s", 3), mfmt(listP99, 1, "s", 3), mfmt(inflight, 1, "", 0))
	}
}

// ---- markdown renderer -----------------------------------------------------

func mfmt(m metric, scale float64, unit string, prec int) string {
	if !m.OK {
		return "n/a"
	}
	return fmt.Sprintf("%.*f%s", prec, m.Value*scale, unit)
}

func lrow(name string, s latencyStats) string {
	if s.Count == 0 {
		return fmt.Sprintf("| %s | (no data) |\n", name)
	}
	return fmt.Sprintf("| %s | count=%d · min %.0fs · p50 %.0fs · p90 %.0fs · p99 %.0fs · max %.0fs · mean %.1fs |\n",
		name, s.Count, s.Min, s.P50, s.P90, s.P99, s.Max, s.Mean)
}

// renderReportMarkdown emits what this run measured, and nothing else.
//
// Every section here is populated from run data: Summary → Test environment →
// Benchmark scenarios → Required measurements → Component sizing → Pass
// criteria. Sections whose signals are all absent suppress themselves rather
// than printing a table of "n/a", so the length of a report tracks how much it
// actually has to say.
//
// Deliberately absent: the static spec scaffolding (motivation, deliverables,
// source-tree inventory, non-goals, acceptance criteria) that used to be copied
// in from issues #1512 / #1518. It is identical in every report, so it carried
// no per-run information and buried the numbers that do.
func renderReportMarkdown(d reportData) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", d.Title)

	renderSummary(&b, d)
	renderTestEnvironment(&b, d)
	renderScenarios(&b, d)
	renderRequiredMeasurements(&b, d)
	renderA1(&b, d)
	renderPassCriteria(&b, d)
	return b.String()
}

func renderSummary(b *strings.Builder, d reportData) {
	var scen []string
	if d.CeilingSweep != nil {
		scen = append(scen, "node-count ceiling sweep (P0.2c)")
	} else if d.NodeCeiling != nil {
		scen = append(scen, "node scaling (P0.2)")
	}
	if d.GPUReset.Total > 0 || d.RebootNode.Total > 0 || d.Nodes.Cordoned > 0 {
		scen = append(scen, "fault→cordon→drain→reboot/GPUReset remediation (P0.4)")
	}
	if d.Reconcile != nil {
		scen = append(scen, "event injection + reconciliation (P0.3/P0.5)")
	}
	if len(scen) == 0 {
		scen = append(scen, "control-plane + component footprint")
	}
	// Second sentence dropped: it listed what the report contains, which was
	// identical in every report and is visible from the section headings.
	b.WriteString("## Summary\n\n")
	fmt.Fprintf(b, "Scale run over a %d-node fleet (%d simulated / KWOK, %d Ready) exercising: %s.\n\n",
		d.Nodes.Total, d.Nodes.Kwok, d.Nodes.KwokReady, strings.Join(scen, "; "))
}

func renderTestEnvironment(b *strings.Builder, d reportData) {
	real := d.Nodes.Total - d.Nodes.Kwok
	if real < 0 {
		real = 0
	}
	kubeVer := d.KubeVersion
	if kubeVer == "" {
		kubeVer = "(unknown)"
	}
	b.WriteString("## Test environment\n\n")
	fmt.Fprintf(b, "- Cluster (apiserver): `%s`\n", d.Cluster)
	fmt.Fprintf(b, "- Kubernetes: %s\n", kubeVer)
	fmt.Fprintf(b, "- Real worker nodes: %d\n", real)
	fmt.Fprintf(b, "- Simulated (KWOK) nodes: %d (Ready %d)\n", d.Nodes.Kwok, d.Nodes.KwokReady)
	if s := d.Sizing; s.NKwok > 0 {
		fmt.Fprintf(b, "- P (pods per KWOK node): %.3f (%d pods: %d DaemonSet / %d other, via %s)\n",
			s.PodsPerKwokNode, s.PodsOnKwok, s.PodsOnKwokDaemonSet, s.PodsOnKwokOther, defStr(s.PSource, "n/a"))
		if s.ClusterMetricsOK {
			fmt.Fprintf(b, "- Real-node utilization: %.1f%% CPU / %.1f%% memory (%s)\n",
				s.ClusterCPUPct*100, s.ClusterMemPct*100, s.ClusterMetricsSource)
		}
	}
	b.WriteString("- Real Kubernetes API server and etcd; KWOK provides Node objects only\n")
	b.WriteString("- NVSentinel + Janitor run on real worker nodes (system under test); janitor `csp=kind`\n")
	fmt.Fprintf(b, "- Prometheus peak window: %s\n", d.Window)
	fmt.Fprintf(b, "- Generated (UTC): %s\n\n", d.GeneratedAt)
}

// renderScenarios lists the scenarios exercised, numbered like the sibling
// specs, each with its own result table (or a "not exercised" note).
func renderScenarios(b *strings.Builder, d reportData) {
	b.WriteString("## Benchmark scenarios\n\n")
	b.WriteString("Scale footprint at report time:\n\n| Object | Count |\n|--------|-------|\n")
	fmt.Fprintf(b, "| KWOK nodes (Ready) | %d (%d) |\n", d.Nodes.Kwok, d.Nodes.KwokReady)
	fmt.Fprintf(b, "| Total nodes | %d |\n", d.Nodes.Total)
	fmt.Fprintf(b, "| Cordoned nodes | %d |\n", d.Nodes.Cordoned)
	fmt.Fprintf(b, "| RebootNode CRs | %d |\n", d.Footprint["rebootnode_crs"])
	fmt.Fprintf(b, "| GPUReset CRs | %d |\n", d.Footprint["gpureset_crs"])
	fmt.Fprintf(b, "| Reset Jobs | %d |\n", d.Footprint["reset_jobs"])
	fmt.Fprintf(b, "| Node-lock leases | %d |\n", d.Footprint["node_lock_leases"])
	fmt.Fprintf(b, "| Connector-pool pods | %d |\n\n", d.Footprint["connector_pool"])

	if d.CeilingSweep != nil {
		b.WriteString("### 1. Steady-population node ceiling sweep (P0.2c)\n\n")
	} else {
		b.WriteString("### 1. Node scaling (P0.2)\n\n")
	}
	renderCeilingBody(b, d)
	b.WriteString("### 2. Fault → cordon → drain → reboot/GPUReset remediation (P0.4)\n\n")
	renderRemediationBody(b, d)
	b.WriteString("### 3. Event injection + reconciliation (P0.3 / P0.5)\n\n")
	renderMongoBody(b, d)
}

func renderRequiredMeasurements(b *strings.Builder, d reportData) {
	b.WriteString("## Required measurements\n\n")
	fmt.Fprintf(b, "Prometheus peaks over the %s window, except where a row names its own source.\n\n", d.Window)
	renderAPIServer(b, d)
	renderEtcd(b, d)
	renderHarnessCost(b, d)
}

// renderCeilingBody prints the P0.2c per-rung curve when a sweep artifact
// exists; otherwise it notes the scenario was not run in this session.
func renderCeilingBody(b *strings.Builder, d reportData) {
	a := d.CeilingSweep
	if a == nil {
		renderNodeCeilingSingle(b, d)
		return
	}
	fmt.Fprintf(b, "Guardrails (advisory): LIST-nodes p99 ≤ %.2fs · apiserver p99 ≤ %.2fs · kwok-controller ≤ %.1f cores · cluster CPU/mem ≤ %.0f%%/%.0f%%.\n\n",
		a.ListP99Guardrail, a.APIServerP99Guardrail, a.KwokCPUGuardrail, a.ClusterCPUGuardrail*100, a.ClusterMemGuardrail*100)
	b.WriteString("| Rung (nodes) | Ready | Ready % | Time-to-ready | apiserver p99 | LIST-nodes p99 | client LIST | kwok-ctrl CPU | cluster CPU/mem | over guardrail? |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for _, s := range a.Steps {
		fmt.Fprintf(b, "| %d | %d | %.1f%% | %.0fs | %ss | %ss | %.2fs | %s | %s | %s |\n",
			s.TargetNodes, s.ReadyNodes, s.ReadyFraction*100, s.TimeToReadySec,
			s.APIServerP99Sec, s.ListNodesP99Sec, s.ClientListSec,
			cpuCell(s.KwokControllerCPU), clusterCell(s.ClusterCPUPct, s.ClusterMemPct),
			yesno(s.Degraded))
	}
	b.WriteString("\n")
	if a.FirstOverGuardrail != nil {
		fmt.Fprintf(b, "First rung over the advisory guardrail: **%d nodes** — %s.\n\n",
			a.FirstOverGuardrail.TargetNodes, a.FirstOverGuardrail.Attribution)
	}
}

// renderNodeCeilingSingle renders the single-target P0.2 record from
// `scale-nodes` (p0.2-node-ceiling.json) when no multi-rung sweep exists.
func renderNodeCeilingSingle(b *strings.Builder, d reportData) {
	a := d.NodeCeiling
	if a == nil {
		b.WriteString("_Not exercised in this run — run `harnessctl scale-nodes -count N` (or `harnessctl ceiling`) to populate._\n\n")
		return
	}
	readyPct := 0.0
	if a.TargetNodes > 0 {
		readyPct = float64(a.ReadyNodes) / float64(a.TargetNodes) * 100
	}
	cluster := "n/a"
	if a.MetricsPresent {
		cluster = fmt.Sprintf("%.1f%% / %.1f%% (%.0f cores / %.0f Mi over %d real nodes)",
			a.ClusterCPUPct*100, a.ClusterMemPct*100, a.ClusterCPUCores, a.ClusterMemMi, a.ClusterRealNode)
	}
	b.WriteString("Single-target scale (P0.2 via `scale-nodes`):\n\n| Metric | Value |\n|--------|-------|\n")
	fmt.Fprintf(b, "| Target nodes | %d |\n", a.TargetNodes)
	fmt.Fprintf(b, "| Ready | %d (%.1f%%) |\n", a.ReadyNodes, readyPct)
	fmt.Fprintf(b, "| Failed creates | %d |\n", a.Failed)
	fmt.Fprintf(b, "| Time-to-ready | %.0fs |\n", a.TimeToReadySec)
	fmt.Fprintf(b, "| apiserver p99 (overall) | %ss (guardrail %.2fs) |\n", a.APIServerP99Sec, a.GuardrailP99Sec)
	fmt.Fprintf(b, "| cluster CPU / mem | %s |\n\n", cluster)
	b.WriteString("_Single rung; run `harnessctl ceiling` for a multi-rung curve._\n\n")
}

func renderAPIServer(b *strings.Builder, d reportData) {
	a := d.APIServer
	fmt.Fprintf(b, "### Control plane — API server (Prometheus peaks over %s)\n\n| Signal | Value |\n|--------|-------|\n", d.Window)
	fmt.Fprintf(b, "| read/LIST p99 | %s |\n", mfmt(a["read_p99_s"], 1, "s", 3))
	fmt.Fprintf(b, "| write p99 | %s |\n", mfmt(a["write_p99_s"], 1, "s", 3))
	fmt.Fprintf(b, "| all-verb p99 | %s |\n", mfmt(a["all_p99_s"], 1, "s", 3))
	fmt.Fprintf(b, "| LIST nodes p99 | %s |\n", mfmt(a["list_nodes_p99_s"], 1, "s", 3))
	fmt.Fprintf(b, "| LIST nodes rate | %s/s |\n", mfmt(a["list_nodes_rate"], 1, "", 1))
	fmt.Fprintf(b, "| total request rate | %s/s |\n", mfmt(a["req_rate"], 1, "", 0))
	fmt.Fprintf(b, "| in-flight (peak) | %s |\n", mfmt(a["inflight"], 1, "", 0))
	fmt.Fprintf(b, "| 5xx rate (peak) | %s/s |\n\n", mfmt(a["err5xx_rate"], 1, "", 2))
	if !d.APIServerOK.Usable {
		// The values stay in the table on purpose. They are still the raw
		// evidence of the fault, and blanking them hides the one clue that
		// explains the warning — a request rate larger than the cluster could
		// physically serve.
		fmt.Fprintf(b, "> ⚠️ **Do not use this table**: %s. The rows above are left in as evidence, not as measurements; the in-flight peak is a gauge and so is the only one unaffected.\n\n",
			d.APIServerOK.reason())
	}
}

// renderEtcd prints etcd's signals, or — on a managed control plane that
// exposes none of them — just the one-line reason.
//
// A three-row table of "n/a" above that reason told the reader nothing the
// reason did not, so the table only appears when at least one signal resolved.
func renderEtcd(b *strings.Builder, d reportData) {
	e := d.Etcd
	keys := []struct {
		label, unit string
		metric      string
		prec        int
	}{
		{"db size", " MiB", "db_size_mib", 0},
		{"wal fsync p99", "s", "wal_fsync_p99_s", 3},
		{"backend commit p99", "s", "backend_commit_p99_s", 3},
	}
	any := false
	for _, k := range keys {
		if e[k.metric].OK {
			any = true
			break
		}
	}
	b.WriteString("### Control plane — etcd\n\n")
	if any {
		b.WriteString("| Signal | Value |\n|--------|-------|\n")
		for _, k := range keys {
			fmt.Fprintf(b, "| %s | %s |\n", k.label, mfmt(e[k.metric], 1, k.unit, k.prec))
		}
		b.WriteString("\n")
	}
	if n, ok := d.Notes["etcd"]; ok {
		fmt.Fprintf(b, "> %s\n\n", n)
	}
}

func renderRemediationBody(b *strings.Builder, d reportData) {
	if d.GPUReset.Total == 0 && d.RebootNode.Total == 0 && d.Nodes.Cordoned == 0 {
		b.WriteString("_Not exercised in this run (no RebootNode/GPUReset CRs and no cordoned nodes) — run `harnessctl janitor check` / inject fatal events to populate._\n\n")
		return
	}
	renderStormConvergence(b, d)

	// Only stages this run actually drove get a row. A run injects one
	// RecommendedAction, so the other kind's stages have no data by
	// construction, and printing them as "(no data)" read like a failure.
	stages := []struct {
		name string
		lat  latencyStats
	}{
		{"RebootNode CR (start→complete)", d.RebootNode.Latency},
		{"GPUReset CR (start→complete)", d.GPUReset.Latency},
		{"Reset Job (start→complete)", d.ResetJobs},
	}
	var rows strings.Builder
	for _, s := range stages {
		if s.lat.Count > 0 {
			rows.WriteString(lrow(s.name, s.lat))
		}
	}
	if rows.Len() > 0 {
		b.WriteString("Latency from CR / Job timestamps:\n\n| Stage | Distribution |\n|-------|--------------|\n")
		b.WriteString(rows.String())
		b.WriteString("\n")
	}
	fmt.Fprintf(b, "> Remediation kind for this run: %s.\n\n", remediationKindNote(d))
	if note := rebootLatencyCaveat(d); note != "" {
		b.WriteString(note)
	}
	renderCRPhases(b, "RebootNode", d.RebootNode)
	renderCRPhases(b, "GPUReset", d.GPUReset)
	if d.ResetJobsN["total"] > 0 {
		fmt.Fprintf(b, "Reset Jobs succeeded: %d/%d\n\n", d.ResetJobsN["succeeded"], d.ResetJobsN["total"])
	}
}

// rebootLatencyCaveat warns that RebootNode start→complete measures the CSP
// simulator, not NVSentinel's remediation speed.
//
// With csp=kind — the chart default, and what the harness runs — the provider
// does not touch the node at all: SendRebootSignal sleeps a synthetic 3-5s and
// IsNodeReady returns true at random ~95% of the time. So these CRs can and do
// complete on a KWOK fleet. What bounds them is throughput: the janitor calls
// SendRebootSignal synchronously inside Reconcile and sets no
// MaxConcurrentReconciles, so controller-runtime's default of one worker turns
// that sleep into a hard ceiling near 0.3 CRs/s however large the fleet is.
// Past a few hundred CRs the queue therefore grows faster than it drains, and
// the unfinished ones are waiting in line rather than failing.
//
// Cordon convergence and CR-creation throughput above are unaffected: both come
// from timestamps the harness controls, and neither passes through the janitor
// worker.
func rebootLatencyCaveat(d reportData) string {
	if d.RebootNode.Total == 0 {
		return ""
	}
	incomplete := d.RebootNode.Total - d.RebootNode.Phase["Completed"]
	if incomplete <= 0 {
		return ""
	}
	return fmt.Sprintf("> RebootNode completion latency here is a property of the harness, not of NVSentinel: %d of %d CRs are unfinished. With `csp=kind` the provider fakes a reboot by sleeping 3-5s and reporting the node ready, and the janitor makes that call synchronously on a single reconcile worker, which caps remediation near 0.3 CRs/s at any fleet size. The unfinished CRs are queued behind that cap, not failing. Read the cordon and CR-creation convergence above as the P0.4 result.\n\n",
		incomplete, d.RebootNode.Total)
}

// remediationKindNote names the CR kind(s) this run actually produced.
func remediationKindNote(d reportData) string {
	switch {
	case d.RebootNode.Total > 0 && d.GPUReset.Total > 0:
		return fmt.Sprintf("both (%d RebootNode, %d GPUReset)", d.RebootNode.Total, d.GPUReset.Total)
	case d.RebootNode.Total > 0:
		return fmt.Sprintf("RebootNode (%d CRs)", d.RebootNode.Total)
	case d.GPUReset.Total > 0:
		return fmt.Sprintf("GPUReset (%d CRs)", d.GPUReset.Total)
	default:
		return "none — nodes were cordoned but no remediation CR was created"
	}
}

// renderCRPhases writes one kind's state breakdown, skipping a kind that this
// run never produced so the table stays about what happened.
func renderCRPhases(b *strings.Builder, kind string, st crStats) {
	if st.Total == 0 {
		return
	}
	fmt.Fprintf(b, "#### %s remediation state\n\n| Phase | Count |\n|-------|-------|\n", kind)
	phases := make([]string, 0, len(st.Phase))
	for k := range st.Phase {
		phases = append(phases, k)
	}
	sort.Strings(phases)
	for _, p := range phases {
		fmt.Fprintf(b, "| %s | %d |\n", p, st.Phase[p])
	}
	b.WriteString("\n")
}

// renderStormConvergence prints the fleet-wide wall-clock the remediation storm
// took to cordon the nodes and to create the remediation CRs (first→last).
// These are aggregate throughput durations, distinct from each object's own
// start→complete latency above. Omitted when the timestamps aren't available.
func renderStormConvergence(b *strings.Builder, d reportData) {
	n := d.Nodes
	if n.CordonSpanSec <= 0 && d.GPUReset.CreateSpanSec <= 0 && d.RebootNode.CreateSpanSec <= 0 {
		return
	}
	b.WriteString("Storm convergence (fleet-wide wall-clock, first→last):\n\n")
	b.WriteString("| Stage | Count | First | Last | Duration |\n|-------|-------|-------|------|----------|\n")
	if n.CordonWithTS > 0 {
		fmt.Fprintf(b, "| Cordon nodes | %d | %s | %s | %s |\n",
			n.CordonWithTS, n.CordonFirstTS, n.CordonLastTS, fmtDur(n.CordonSpanSec))
	}
	for _, cr := range []struct {
		kind string
		st   crStats
	}{{"RebootNode", d.RebootNode}, {"GPUReset", d.GPUReset}} {
		if cr.st.CreateWithTS > 0 {
			fmt.Fprintf(b, "| Create %s CRs | %d | %s | %s | %s |\n",
				cr.kind, cr.st.CreateWithTS, cr.st.CreateFirstTS, cr.st.CreateLastTS, fmtDur(cr.st.CreateSpanSec))
		}
	}
	b.WriteString("\n> Cordon time is from the `node.kubernetes.io/unschedulable` taint `timeAdded`; CR-creation time is from each CR's `creationTimestamp`. Both are first→last over the fleet, i.e. aggregate storm throughput, not per-object latency.\n\n")
}

// fmtDur renders a seconds duration as a compact human string (e.g. "1h19m18s").
func fmtDur(sec float64) string {
	if sec <= 0 {
		return "n/a"
	}
	return (time.Duration(sec) * time.Second).Round(time.Second).String()
}

// renderHarnessCost lists only the pods that are NOT the system under test.
//
// The NVSentinel components themselves are covered once, in A1, with shares and
// totals; repeating them here as a second flat table said the same thing twice
// and invited the two tables to be read as different measurements. What A1
// legitimately omits is the harness's own cost — kwok-controller, the connector
// pool, the injectors — which matters for sizing the *test rig* and must stay
// out of any NVSentinel sizing claim. That is what this table is for, so it
// prints only when such pods exist.
func renderHarnessCost(b *strings.Builder, d reportData) {
	var rows []componentUsage
	for _, comp := range d.Components {
		if comp.Pods > 0 && !comp.NVSentinel {
			rows = append(rows, comp)
		}
	}
	if len(rows) == 0 {
		return
	}
	b.WriteString("### Harness overhead (not the system under test)\n\n")
	b.WriteString("| Component | Pods | CPU (cores) | Memory (MiB) | Source |\n|-----------|------|-------------|--------------|--------|\n")
	for _, comp := range rows {
		fmt.Fprintf(b, "| %s | %d | %.2f | %d | %s |\n",
			comp.Name, comp.Pods, float64(comp.CPUMilli)/1000.0, comp.MemMi, defStr(comp.Source, "n/a"))
	}
	b.WriteString("\n")
}

func renderMongoBody(b *strings.Builder, d reportData) {
	// Prefer the P0.3 reconcile artifact: it proves zero-loss accounting (every
	// injected event id landed) AND end-to-end NodeName attribution, produced
	// in-cluster so it works against the mTLS store the CLI can't reach directly.
	if r := d.Reconcile; r != nil {
		verdict := "✅ PASS"
		if r.Verdict != "PASS" {
			verdict = "❌ " + r.Verdict
		}
		b.WriteString("P0.3 accounting (per-event id + end-to-end NodeName), distributed across the connector pool:\n\n")
		b.WriteString("| Metric | Value |\n|--------|-------|\n")
		fmt.Fprintf(b, "| run id | `%s` |\n", r.RunID)
		fmt.Fprintf(b, "| injected | %d |\n", r.Injected)
		fmt.Fprintf(b, "| acked | %d |\n", r.Acked)
		fmt.Fprintf(b, "| stored for run | %d |\n", r.StoredForRun)
		fmt.Fprintf(b, "| accounted | %d |\n", r.Accounted)
		fmt.Fprintf(b, "| missing | %d |\n", r.Missing)
		fmt.Fprintf(b, "| loss fraction | %.4f (max %.4f) |\n", r.LossFraction, r.MaxLoss)
		fmt.Fprintf(b, "| NodeName attribution | %d/%d matched |\n", r.NodeMatched, r.NodeChecked)
		fmt.Fprintf(b, "| verdict | %s |\n\n", verdict)
		if r.NodeAttrNote != "" {
			fmt.Fprintf(b, "> %s\n\n", r.NodeAttrNote)
		}
		return
	}
	if d.Mongo.OK {
		fmt.Fprintf(b, "Event store (MongoDB) accounting:\n\n| Metric | Value |\n|--------|-------|\n| documents (estimated) | %d |\n", d.Mongo.TotalDocs)
		b.WriteString("\n")
		return
	}
	fmt.Fprintf(b, "> %s\n\n", d.Mongo.Note)
}

func renderPassCriteria(b *strings.Builder, d reportData) {
	b.WriteString("## Pass criteria\n\n**At expected peak:**\n\n")
	guard := d.Guardrails["apiserver_p99_s"]
	all := d.APIServer["all_p99_s"]
	err5xx := d.APIServer["err5xx_rate"]
	if d.APIServerOK.Usable {
		b.WriteString(check(all.OK && all.Value <= guard,
			fmt.Sprintf("apiserver all-verb p99 %s within guardrail %.2fs", mfmt(all, 1, "s", 3), guard),
			fmt.Sprintf("apiserver all-verb p99 %s over guardrail %.2fs", mfmt(all, 1, "s", 3), guard)))
		b.WriteString(check(!err5xx.OK || err5xx.Value == 0,
			"no apiserver 5xx over the window",
			fmt.Sprintf("apiserver 5xx rate peaked at %s/s (shared-cluster background if not test-attributable)", mfmt(err5xx, 1, "", 2))))
	} else {
		// Neither criterion can pass or fail here, and a ✅ or a ⚠️ would both
		// be assertions this run cannot support. A 5xx rate read off a
		// multiplexed counter is the worse of the two: it manufactures errors
		// that were never served.
		fmt.Fprintf(b, "- ❔ apiserver all-verb p99 and 5xx rate not assessed: %s\n", d.APIServerOK.reason())
	}
	if d.Nodes.Kwok > 0 {
		frac := float64(d.Nodes.KwokReady) / float64(d.Nodes.Kwok)
		b.WriteString(check(frac >= 0.99,
			fmt.Sprintf("KWOK node readiness %.1f%% (≥99%%)", frac*100),
			fmt.Sprintf("KWOK node readiness %.1f%% (<99%%) — kwok-controller heartbeat ceiling", frac*100)))
	}
	if d.ResetJobsN["total"] > 0 {
		b.WriteString(check(d.ResetJobsN["succeeded"] == d.ResetJobsN["total"],
			fmt.Sprintf("reset Jobs succeeded %d/%d", d.ResetJobsN["succeeded"], d.ResetJobsN["total"]),
			fmt.Sprintf("reset Jobs succeeded only %d/%d", d.ResetJobsN["succeeded"], d.ResetJobsN["total"])))
	}
	b.WriteString("\n**At saturation:**\n\n")
	fmt.Fprintf(b, "- Limiting resource: %s\n", d.Ceiling)
	if d.CeilingSweep != nil && d.CeilingSweep.FirstOverGuardrail != nil {
		fmt.Fprintf(b, "- Node sweep: first rung over the advisory guardrail at %d nodes — %s\n",
			d.CeilingSweep.FirstOverGuardrail.TargetNodes, d.CeilingSweep.FirstOverGuardrail.Attribution)
	}
	b.WriteString("- Degradation is observable via the per-rung LIST-nodes p99 curve and the node-readiness fraction\n\n")
	b.WriteString("> ⚠️ Single run on a possibly-shared cluster, so absolute latencies include background load — prefer the trend across rungs over any single value.\n\n")
}

// ---- small cell formatters -------------------------------------------------

func cpuCell(v string) string {
	if v == "" || v == "unknown" {
		return "n/a"
	}
	return v
}

func clusterCell(cpuFrac, memFrac float64) string {
	if cpuFrac == 0 && memFrac == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%% / %.1f%%", cpuFrac*100, memFrac*100)
}

func yesno(v bool) string {
	if v {
		return "⚠️ yes"
	}
	return "ok"
}

func check(pass bool, okMsg, failMsg string) string {
	if pass {
		return "- ✅ " + okMsg + "\n"
	}
	return "- ⚠️ " + failMsg + "\n"
}

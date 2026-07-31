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

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// runReport collects the full performance picture of a scale run — Prometheus
// control-plane latency/throughput, etcd, node/cordon state, GPUReset & reset-Job
// latency, component CPU/memory (metrics-server), MongoDB counts and a ceiling
// attribution — and writes report.md + report.json. It replaces the ad-hoc
// kubectl/PromQL commands previously run by hand.
func runReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	cfg := loadConfig()
	out := fs.String("out", filepath.Join(cfg.ResultsDir, "report.md"), "Markdown report output path")
	window := fs.String("window", cfg.ReportWindow, "PromQL lookback window for peak (max_over_time) queries")
	mongoRunID := fs.String("mongo-run-id", "", "if set (and Mongo reachable), count stored docs for this run label")
	title := fs.String("title", "NVSentinel scale run", "report title")
	_ = fs.Parse(args)
	cfg.ReportWindow = *window

	c, err := newClients(cfg)
	if err != nil {
		return err
	}

	stepf("report: collecting metrics (window=%s)", cfg.ReportWindow)
	data := c.collectReport(ctx, cfg, *title, *mongoRunID)

	writeArtifact(cfg.ResultsDir, "report.json", data)
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
	Name     string `json:"name"`
	Pods     int    `json:"pods"`
	CPUMilli int64  `json:"cpu_millicores"`
	MemMi    int64  `json:"mem_mib"`
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
	// Creation-convergence span: first→last GPUReset CR creationTimestamp for
	// this run's nodes. This is the wall-clock the remediation engine took to
	// create the fleet's CRs (distinct from each CR's own start→complete latency).
	CreateWithTS  int     `json:"create_with_timestamp"`
	CreateFirstTS string  `json:"create_first_ts,omitempty"`
	CreateLastTS  string  `json:"create_last_ts,omitempty"`
	CreateSpanSec float64 `json:"create_span_seconds"`
}

type mongoStats struct {
	OK        bool   `json:"ok"`
	TotalDocs int64  `json:"total_docs"`
	RunDocs   int64  `json:"run_docs"`
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
	Janitor      map[string]metric    `json:"janitor_controller"`
	GPUReset     crStats              `json:"gpureset_crs"`
	ResetJobs    latencyStats         `json:"reset_jobs"`
	ResetJobsN   map[string]int       `json:"reset_jobs_counts"`
	Components   []componentUsage     `json:"components"`
	Mongo        mongoStats           `json:"mongodb"`
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

func (c *clients) collectReport(ctx context.Context, cfg Config, title, mongoRunID string) reportData {
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

	// --- janitor gpureset controller (the Phase-2 bottleneck signals) ---
	d.Janitor = map[string]metric{
		"workqueue_depth_peak": peak(`max(workqueue_depth{name="gpureset"})`),
		"reconcile_rate_peak":  peak(`sum(rate(controller_runtime_reconcile_total{controller="gpureset"}[5m]))`),
		"restarts": inst(fmt.Sprintf(
			`max(max_over_time(kube_pod_container_status_restarts_total{namespace="%s",pod=~"dgxc-janitor-controller-manager.*"}[%s]))`,
			cfg.JanitorNamespace, win)),
	}

	// --- nodes / cordon (authoritative from a live LIST) ---
	d.Nodes = c.collectNodeStats(ctx)

	// --- object footprint ---
	d.GPUReset = c.collectGPUResetStats(ctx, cfg.NodePrefix)
	jobsTotal, jobsDone, jobLat := c.collectResetJobStats(ctx, cfg.JanitorNamespace)
	d.ResetJobs = jobLat
	d.ResetJobsN = map[string]int{"total": jobsTotal, "succeeded": jobsDone}
	d.Footprint = map[string]int{
		"kwok_nodes":       d.Nodes.Kwok,
		"total_nodes":      d.Nodes.Total,
		"gpureset_crs":     d.GPUReset.Total,
		"reset_jobs":       jobsTotal,
		"node_lock_leases": c.leaseCount(ctx, cfg.JanitorNamespace),
		"connector_pool":   c.podCountByPrefix(ctx, cfg.NVSNamespace, connectorPoolName),
	}

	// --- component CPU/memory (metrics-server) ---
	d.Components = c.collectComponents(ctx, cfg)

	// --- MongoDB (best-effort) ---
	d.Mongo = mongoStatsBestEffort(ctx, cfg, mongoRunID)

	// --- ceiling attribution ---
	d.Ceiling = attributeReportCeiling(cfg, d)
	return d
}

func (c *clients) collectNodeStats(ctx context.Context) nodeStats {
	var ns nodeStats
	list, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		warnf("report: list nodes: %v", err)
		return ns
	}
	var cordonFirst, cordonLast time.Time
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
	if !cordonFirst.IsZero() && !cordonLast.IsZero() {
		ns.CordonFirstTS = cordonFirst.UTC().Format(time.RFC3339)
		ns.CordonLastTS = cordonLast.UTC().Format(time.RFC3339)
		ns.CordonSpanSec = cordonLast.Sub(cordonFirst).Seconds()
	}
	return ns
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
	st := crStats{Phase: map[string]int{}}
	var lat []float64
	var createFirst, createLast time.Time
	cont := ""
	for {
		list, err := c.dynamic.Resource(gpuresetGVR).List(ctx, metav1.ListOptions{Limit: 500, Continue: cont})
		if err != nil {
			warnf("report: list gpuresets: %v", err)
			break
		}
		for i := range list.Items {
			obj := list.Items[i].Object
			node, _, _ := unstructured.NestedString(obj, "spec", "nodeName")
			if nodePrefix != "" && !strings.HasPrefix(node, nodePrefix) {
				continue
			}
			st.Total++
			phase, _, _ := unstructured.NestedString(obj, "status", "phase")
			if phase == "" {
				phase = "(pending)"
			}
			st.Phase[phase]++
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
		out = append(out, podUsageItem{it.Metadata.Name, cpu, mem})
	}
	return out, nil
}

// componentTarget maps a friendly name to (namespace, pod-name prefix).
type componentTarget struct{ name, ns, prefix string }

func (c *clients) collectComponents(ctx context.Context, cfg Config) []componentUsage {
	targets := []componentTarget{
		{"janitor-controller", cfg.JanitorNamespace, "dgxc-janitor-controller-manager"},
		{"kwok-controller", "kube-system", "kwok-controller"},
		{"node-drainer", cfg.NVSNamespace, "node-drainer"},
		{"fault-quarantine", cfg.NVSNamespace, "fault-quarantine"},
		{"fault-remediation", cfg.NVSNamespace, "fault-remediation"},
		{"kubernetes-object-monitor", cfg.NVSNamespace, "kubernetes-object-monitor"},
		{"mongodb", cfg.NVSNamespace, "mongodb"},
		{"connector-pool", cfg.NVSNamespace, connectorPoolName},
		{"pool-injector", cfg.NVSNamespace, poolInjectorDaemonSet},
	}
	// Cache pod usage per namespace so we hit metrics-server once per ns.
	byNS := map[string][]podUsageItem{}
	out := make([]componentUsage, 0, len(targets))
	for _, t := range targets {
		items, ok := byNS[t.ns]
		if !ok {
			var err error
			items, err = c.podUsage(ctx, t.ns)
			if err != nil {
				warnf("report: metrics-server for ns %s: %v", t.ns, err)
			}
			byNS[t.ns] = items
		}
		cu := componentUsage{Name: t.name}
		for _, it := range items {
			if strings.HasPrefix(it.name, t.prefix) {
				cu.Pods++
				cu.CPUMilli += it.cpuMilli
				cu.MemMi += it.memMi
			}
		}
		out = append(out, cu)
	}
	return out
}

// ---- MongoDB (best-effort) -------------------------------------------------

func mongoStatsBestEffort(ctx context.Context, cfg Config, runID string) mongoStats {
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
	if runID != "" {
		runKey := fmt.Sprintf("%s.metadata.%s", cfg.FieldPrefix, cfg.RunLabel)
		if rn, err := coll.CountDocuments(qctx, bson.M{runKey: runID}); err == nil {
			ms.RunDocs = rn
		}
	}
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
	wq := d.Janitor["workqueue_depth_peak"]
	// Find janitor memory usage vs a nominal ceiling for the narrative.
	var janitorMem int64
	for _, comp := range d.Components {
		if comp.Name == "janitor-controller" {
			janitorMem = comp.MemMi
		}
	}
	switch {
	case readP99.OK && readP99.Value > cfg.MaxAPIServerP99:
		return fmt.Sprintf("REAL ceiling: apiserver read p99 %.3fs exceeded guardrail %.3fs — control plane (apiserver/etcd) is the bound.", readP99.Value, cfg.MaxAPIServerP99)
	case wq.OK && wq.Value > 1000:
		return fmt.Sprintf("Remediation throughput bound: janitor gpureset workqueue peaked at %.0f (janitor-controller mem ~%dMi). Raise janitor memory limit + shrink cache footprint (Phase 2). Control plane healthy (read p99=%s, inflight peak=%s).",
			wq.Value, janitorMem, mfmt(readP99, 1, "s", 3), mfmt(inflight, 1, "", 0))
	case listP99.OK && listP99.Value > cfg.CeilingListP99:
		return fmt.Sprintf("REAL ceiling: apiserver LIST-nodes p99 %.3fs exceeded guardrail %.3fs — large-collection LIST against apiserver/etcd is the bound for node scale (Phase 2). Overall read p99 stays low (%s) and in-flight peak %s, so it's LIST-fanout cost, not general control-plane saturation.",
			listP99.Value, cfg.CeilingListP99, mfmt(readP99, 1, "s", 3), mfmt(inflight, 1, "", 0))
	default:
		return fmt.Sprintf("No saturation attributed: apiserver read p99=%s, LIST-nodes p99=%s, inflight peak=%s, gpureset workqueue peak=%s.",
			mfmt(readP99, 1, "s", 3), mfmt(listP99, 1, "s", 3), mfmt(inflight, 1, "", 0), mfmt(wq, 1, "", 0))
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

// renderReportMarkdown emits the report in the exact section structure of the
// NVSentinel microbench specs (issues #1512 / #1518): Summary → Motivation →
// Test environment → Benchmark scenarios → Required measurements → Measurement
// validity → Pass criteria → Deliverables → Existing code/configuration →
// Non-goals → Acceptance criteria. Populated with THIS run's results so it diffs
// directly against the module microbenchmarks.
func renderReportMarkdown(d reportData) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Microbenchmark: %s (NVSentinel Phase 0 Harness)\n\n", d.Title)
	b.WriteString("> Report format follows the NVSentinel microbench specs " +
		"[#1512 (kubernetes-object-monitor)](https://github.com/NVIDIA/NVSentinel/issues/1512) and " +
		"[#1518 (fault-quarantine)](https://github.com/NVIDIA/NVSentinel/issues/1518) so results diff directly against the module microbenchmarks.  \n")
	b.WriteString("> Auto-generated by `harnessctl report` (Prometheus via apiserver proxy + metrics-server + live LIST + ceiling artifact).\n\n")

	renderSummary(&b, d)
	renderMotivation(&b)
	renderTestEnvironment(&b, d)
	renderScenarios(&b, d)
	renderRequiredMeasurements(&b, d)
	renderValidity(&b, d)
	renderPassCriteria(&b, d)
	renderDeliverables(&b)
	renderExisting(&b)
	renderNonGoals(&b)
	renderAcceptance(&b)
	return b.String()
}

func renderSummary(b *strings.Builder, d reportData) {
	var scen []string
	if d.CeilingSweep != nil {
		scen = append(scen, "node-count ceiling sweep (P0.2c)")
	} else if d.NodeCeiling != nil {
		scen = append(scen, "node scaling (P0.2)")
	}
	if d.GPUReset.Total > 0 || d.Nodes.Cordoned > 0 {
		scen = append(scen, "fault→cordon→drain→GPUReset remediation (P0.4)")
	}
	if d.Mongo.OK && d.Mongo.RunDocs > 0 {
		scen = append(scen, "event injection + reconciliation (P0.3/P0.5)")
	}
	if len(scen) == 0 {
		scen = append(scen, "control-plane + component footprint")
	}
	b.WriteString("## Summary\n\n")
	fmt.Fprintf(b, "Scale run over a %d-node fleet (%d simulated / KWOK, %d Ready) exercising: %s. ",
		d.Nodes.Total, d.Nodes.Kwok, d.Nodes.KwokReady, strings.Join(scen, "; "))
	b.WriteString("Records control-plane latency/throughput, remediation latency, component CPU/memory, " +
		"and a harness-vs-real ceiling attribution so the binding constraint for Phase 2 is explicit.\n\n")
}

func renderMotivation(b *strings.Builder) {
	b.WriteString("## Motivation\n\n")
	b.WriteString("Phase 2 (full-system scale) needs a defensible node/cluster ceiling and a way to separate test-harness cost from real cluster cost. Node-scaling cost is driven by:\n\n")
	b.WriteString("- Served/cached Node object count → API server LIST response size + duration (etcd range reads)\n")
	b.WriteString("- Node heartbeat-lease churn → API server write/watch traffic\n")
	b.WriteString("- kwok-controller capacity to keep every simulated node's lease fresh (the harness limit)\n")
	b.WriteString("- client-go QPS/burst throttling on the scale-up path\n")
	b.WriteString("- Remediation throughput (fault→cordon→drain→GPUReset) under a large fleet\n\n")
	b.WriteString("We need current-release baselines, configuration guardrails, and an explicit statement of which resource is the binding constraint.\n\n")
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
	b.WriteString("### 2. Fault → cordon → drain → GPUReset remediation (P0.4)\n\n")
	renderRemediationBody(b, d)
	b.WriteString("### 3. Event injection + reconciliation (P0.3 / P0.5)\n\n")
	renderMongoBody(b, d)
}

func renderRequiredMeasurements(b *strings.Builder, d reportData) {
	b.WriteString("## Required measurements\n\n")
	b.WriteString("Control-plane and resource signals captured for the run (API server / etcd / janitor are Prometheus peaks over the window; component usage is live from metrics-server):\n\n")
	renderAPIServer(b, d)
	renderEtcd(b, d)
	renderJanitor(b, d)
	renderComponents(b, d)
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
	b.WriteString("_Live `LIST nodes` p99 at this scale is under Required measurements → API server; run `harnessctl ceiling` for a multi-rung curve._\n\n")
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
}

func renderEtcd(b *strings.Builder, d reportData) {
	e := d.Etcd
	b.WriteString("### Control plane — etcd\n\n| Signal | Value |\n|--------|-------|\n")
	fmt.Fprintf(b, "| db size | %s |\n", mfmt(e["db_size_mib"], 1, " MiB", 0))
	fmt.Fprintf(b, "| wal fsync p99 | %s |\n", mfmt(e["wal_fsync_p99_s"], 1, "s", 3))
	fmt.Fprintf(b, "| backend commit p99 | %s |\n", mfmt(e["backend_commit_p99_s"], 1, "s", 3))
	if n, ok := d.Notes["etcd"]; ok {
		fmt.Fprintf(b, "\n> %s\n", n)
	}
	b.WriteString("\n")
}

func renderRemediationBody(b *strings.Builder, d reportData) {
	if d.GPUReset.Total == 0 && d.Nodes.Cordoned == 0 {
		b.WriteString("_Not exercised in this run (no GPUReset CRs / cordoned nodes) — run `harnessctl janitor-check` / inject fatal events to populate._\n\n")
		return
	}
	renderStormConvergence(b, d)
	b.WriteString("Latency from CR / Job timestamps:\n\n| Stage | Distribution |\n|-------|--------------|\n")
	b.WriteString(lrow("GPUReset CR (start→complete)", d.GPUReset.Latency))
	b.WriteString(lrow("Reset Job (start→complete)", d.ResetJobs))
	b.WriteString("\n#### GPUReset remediation state\n\n| Phase | Count |\n|-------|-------|\n")
	phases := make([]string, 0, len(d.GPUReset.Phase))
	for k := range d.GPUReset.Phase {
		phases = append(phases, k)
	}
	sort.Strings(phases)
	for _, p := range phases {
		fmt.Fprintf(b, "| %s | %d |\n", p, d.GPUReset.Phase[p])
	}
	fmt.Fprintf(b, "| reset-Jobs succeeded | %d/%d |\n\n", d.ResetJobsN["succeeded"], d.ResetJobsN["total"])
}

// renderStormConvergence prints the fleet-wide wall-clock the remediation storm
// took to cordon the nodes and to create the GPUReset CRs (first→last). These
// are aggregate throughput durations, distinct from each object's own
// start→complete latency above. Omitted when the timestamps aren't available.
func renderStormConvergence(b *strings.Builder, d reportData) {
	n := d.Nodes
	cr := d.GPUReset
	if n.CordonSpanSec <= 0 && cr.CreateSpanSec <= 0 {
		return
	}
	b.WriteString("Storm convergence (fleet-wide wall-clock, first→last):\n\n")
	b.WriteString("| Stage | Count | First | Last | Duration |\n|-------|-------|-------|------|----------|\n")
	if n.CordonWithTS > 0 {
		fmt.Fprintf(b, "| Cordon nodes | %d | %s | %s | %s |\n",
			n.CordonWithTS, n.CordonFirstTS, n.CordonLastTS, fmtDur(n.CordonSpanSec))
	}
	if cr.CreateWithTS > 0 {
		fmt.Fprintf(b, "| Create GPUReset CRs | %d | %s | %s | %s |\n",
			cr.CreateWithTS, cr.CreateFirstTS, cr.CreateLastTS, fmtDur(cr.CreateSpanSec))
	}
	b.WriteString("\n> Cordon time is from the `node.kubernetes.io/unschedulable` taint `timeAdded`; CR-creation time is from GPUReset `creationTimestamp`s. Both are first→last over the fleet, i.e. aggregate storm throughput, not per-object latency.\n\n")
}

// fmtDur renders a seconds duration as a compact human string (e.g. "1h19m18s").
func fmtDur(sec float64) string {
	if sec <= 0 {
		return "n/a"
	}
	return (time.Duration(sec) * time.Second).Round(time.Second).String()
}

func renderJanitor(b *strings.Builder, d reportData) {
	j := d.Janitor
	b.WriteString("### Janitor gpureset controller (Prometheus)\n\n| Signal | Value |\n|--------|-------|\n")
	fmt.Fprintf(b, "| workqueue depth (peak) | %s |\n", mfmt(j["workqueue_depth_peak"], 1, "", 0))
	fmt.Fprintf(b, "| reconcile rate (peak) | %s/s |\n", mfmt(j["reconcile_rate_peak"], 1, "", 1))
	fmt.Fprintf(b, "| container restarts (window) | %s |\n\n", mfmt(j["restarts"], 1, "", 0))
}

func renderComponents(b *strings.Builder, d reportData) {
	b.WriteString("### Component resource usage (metrics-server, live)\n\n| Component | Pods | CPU (cores) | Memory (MiB) |\n|-----------|------|-------------|--------------|\n")
	for _, comp := range d.Components {
		if comp.Pods == 0 {
			continue
		}
		fmt.Fprintf(b, "| %s | %d | %.2f | %d |\n", comp.Name, comp.Pods, float64(comp.CPUMilli)/1000.0, comp.MemMi)
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
		if d.Mongo.RunDocs > 0 {
			fmt.Fprintf(b, "| documents for run | %d |\n", d.Mongo.RunDocs)
		}
		b.WriteString("\n")
		return
	}
	fmt.Fprintf(b, "> %s\n\n", d.Mongo.Note)
}

func renderValidity(b *strings.Builder, d reportData) {
	b.WriteString("## Measurement validity\n\n")
	b.WriteString("- Stable object IDs: simulated nodes have deterministic names (`kwok-gpu-<n>`); injected events carry a per-event correlation id\n")
	fmt.Fprintf(b, "- Warmup / window: Prometheus peaks taken as `max_over_time` over a %s window; ceiling rungs settle before sampling\n", d.Window)
	b.WriteString("- Hard timeout: node-readiness is capped per rung (`NODE_READY_TIMEOUT`) so a rung terminates instead of hanging\n")
	b.WriteString("- Harness vs real saturation is distinguished (kwok-controller vs apiserver/etcd) — see Pass criteria → At saturation\n")
	b.WriteString("- Live state: node/cordon/CR/Job counts come from an authoritative LIST at report time, not cached\n")
	b.WriteString("- ⚠️ Single run on a possibly-shared cluster; absolute latencies include background load — prefer the trend over any single value. The sibling specs call for ≥3 repetitions on a quiet cluster\n\n")
}

func renderPassCriteria(b *strings.Builder, d reportData) {
	b.WriteString("## Pass criteria\n\n**At expected peak:**\n\n")
	guard := d.Guardrails["apiserver_p99_s"]
	all := d.APIServer["all_p99_s"]
	err5xx := d.APIServer["err5xx_rate"]
	b.WriteString(check(all.OK && all.Value <= guard,
		fmt.Sprintf("apiserver all-verb p99 %s within guardrail %.2fs", mfmt(all, 1, "s", 3), guard),
		fmt.Sprintf("apiserver all-verb p99 %s over guardrail %.2fs", mfmt(all, 1, "s", 3), guard)))
	b.WriteString(check(!err5xx.OK || err5xx.Value == 0,
		"no apiserver 5xx over the window",
		fmt.Sprintf("apiserver 5xx rate peaked at %s/s (shared-cluster background if not test-attributable)", mfmt(err5xx, 1, "", 2))))
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
}

func renderDeliverables(b *strings.Builder) {
	b.WriteString("## Deliverables\n\n")
	b.WriteString("- Benchmark runner — `harnessctl` (bringup, scale-nodes, ceiling, connector-pool, inject, reconcile, janitor-check, report)\n")
	b.WriteString("- Prometheus queries — apiserver overall/read/write/LIST-nodes p99, request/LIST rate, in-flight, 5xx; etcd size/fsync/commit; janitor workqueue/reconcile\n")
	b.WriteString("- Machine-readable results — `results/report.json`, `results/p0.2-ceiling-sweep.json`, `results/phase0-results.json` (+ JUnit `.xml`)\n")
	b.WriteString("- Markdown summary — this document\n\n")
}

func renderExisting(b *strings.Builder) {
	b.WriteString("## Existing code/configuration\n\n")
	b.WriteString("- `tests/scale-tests/harness/harnessctl/` — CLI (bringup, scale-nodes, ceiling, connector-pool, inject, reconcile, janitor-check, report)\n")
	b.WriteString("- `tests/scale-tests/harness/harnessctl/config.go` — guardrails + `CEILING_*`, `METRICS_WINDOW`, `NODE_READY_TIMEOUT`\n")
	b.WriteString("- `tests/scale-tests/harness/config/harness.env` — commonly-edited knobs\n")
	b.WriteString("- `tests/scale-tests/harness/{kwok,monitoring,nvsentinel}/` — KWOK, monitoring, NVSentinel/janitor bringup (P0.1)\n\n")
}

func renderNonGoals(b *strings.Builder) {
	b.WriteString("## Non-goals\n\n")
	b.WriteString("- KOM / fault-quarantine module microbenchmarks (covered by #1512 / #1518)\n")
	b.WriteString("- Treating kwok-controller capacity as a product target (it is the harness, not the SUT)\n")
	b.WriteString("- Clean-room absolute latency baselines (this may run on a shared cluster)\n\n")
}

func renderAcceptance(b *strings.Builder) {
	b.WriteString("## Acceptance criteria\n\n")
	b.WriteString("- One command per phase runs setup-free (bringup, ceiling, connector-pool, inject, reconcile, janitor-check, report)\n")
	b.WriteString("- Results identify the node knee and its cause (apiserver/etcd LIST vs kwok-controller)\n")
	b.WriteString("- Harness saturation vs real ceiling are distinguished\n")
	b.WriteString("- Results are machine-readable and diffable between runs/releases\n")
	b.WriteString("- Reproducible via `harnessctl ceiling -start … -step … -max …` and `harnessctl report`\n\n")
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

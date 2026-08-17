/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/


//go:build !injector

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ceilingStep is one rung of the P0.2 ceiling sweep, recorded to the artifact so
// the "push until degradation + attribute it" result is diffable, unattended.
type ceilingStep struct {
	TargetNodes       int     `json:"target_nodes"`
	ReadyNodes        int     `json:"ready_nodes"`
	FailedCreates     int     `json:"failed_creates"`
	ReadyFraction     float64 `json:"ready_fraction"`
	TimeToReadySec    float64 `json:"time_to_ready_seconds"`
	APIServerP99Sec   string  `json:"apiserver_p99_seconds"`
	ListNodesP99Sec   string  `json:"apiserver_list_nodes_p99_seconds"`
	ClientListSec     float64 `json:"client_list_nodes_seconds"`
	KwokControllerCPU string  `json:"kwok_controller_cpu_cores"`
	ClusterCPUPct     float64 `json:"cluster_cpu_pct"`
	ClusterMemPct     float64 `json:"cluster_mem_pct"`
	Degraded          bool    `json:"degraded"`
	Attribution       string  `json:"attribution,omitempty"`
	Detail            string  `json:"detail,omitempty"`
}

func runCeiling(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("nodes ceiling", flag.ExitOnError)
	cfg := defaultConfig()
	bindResultsFlag(fs, &cfg)
	bindPromFlags(fs, &cfg)
	bindNodeGuardrailFlags(fs, &cfg)
	bindNodeShapeFlags(fs, &cfg)
	start := fs.Int("start", cfg.CeilingStart, "first node count in the ramp")
	step := fs.Int("step", cfg.CeilingStep, "increment between ramp steps")
	maxN := fs.Int("max", cfg.CeilingMax, "final node count to attempt")
	settle := fs.Int("settle-seconds", cfg.CeilingSettle, "seconds to probe/settle at each step before measuring")
	listP99 := fs.Float64("list-nodes-p99", cfg.CeilingListP99, "LIST-nodes p99 guardrail (seconds) — the real-ceiling signal")
	kwokCPU := fs.Float64("kwok-cpu-cores", cfg.CeilingKwokCPU, "KWOK controller CPU (cores) above which it's saturated")
	window := fs.String("metrics-window", cfg.MetricsWindow, "PromQL rate window for per-step measurements")
	_ = fs.Parse(args)

	cfg.CeilingStart, cfg.CeilingStep, cfg.CeilingMax = *start, *step, *maxN
	cfg.CeilingSettle, cfg.CeilingListP99, cfg.CeilingKwokCPU, cfg.MetricsWindow = *settle, *listP99, *kwokCPU, *window

	c, err := newClients(cfg)
	if err != nil {
		return err
	}
	rs := newResultSet(cfg.ResultsDir)
	res := c.checkCeiling(ctx, cfg)
	rs.add(res)
	_ = rs.write()
	if !res.passed() {
		return fmt.Errorf("%s", res.Message)
	}
	return nil
}

// ceilingTargets is the ascending list of node counts to ramp through, built
// from start/step/max (e.g. start=10000 step=10000 max=50000 -> 10k,20k,…,50k).
func ceilingTargets(cfg Config) []int {
	var out []int
	for n := cfg.CeilingStart; n <= cfg.CeilingMax; n += cfg.CeilingStep {
		out = append(out, n)
		if cfg.CeilingStep <= 0 {
			break
		}
	}
	return out
}

// checkCeiling implements P0.2's "record the ceiling" non-interactively: ramp the
// node count step by step through the full start..max range, and at each step
// measure node readiness, the API server p99 (overall + the heavy LIST-nodes
// read), the client-facing LIST latency, and the KWOK controller's CPU. The
// built-in guardrails are advisory: each rung is annotated as over/under and the
// likely cause (harness KWOK saturation vs the real API server / etcd ceiling)
// is noted, but the ramp never stops early — the operator decides the ceiling
// from the recorded per-step numbers.
func (c *clients) checkCeiling(ctx context.Context, cfg Config) CheckResult {
	started := time.Now()
	targets := ceilingTargets(cfg)
	stepf("P0.2 ceiling: ramp %v, record per-step numbers + advisory guardrail flags", targets)
	res := CheckResult{ID: "P0.2c", Name: "node ceiling (sweep)", Started: started, Metrics: map[string]any{}}
	if len(targets) == 0 {
		res.Verdict, res.Message, res.Finished = "FAIL", "no ceiling targets configured", time.Now()
		return res
	}

	window := cfg.MetricsWindow
	readyTO := time.Duration(cfg.NodeReadyTO) * time.Second
	var rows []ceilingStep
	lastHealthy := 0
	var firstBreach *ceilingStep

	for _, target := range targets {
		stepf("ceiling step: %d nodes", target)
		cfg.NodeCount = target
		stepStart := time.Now()
		_, _, failed := c.scaleNodes(ctx, cfg)
		// Ceiling measures each rung's natural readiness (to attribute kwok-vs-
		// control-plane saturation), so no self-heal restart here.
		ready, _ := c.waitNodesReady(ctx, target, readyTO, nil)
		ttr := time.Since(stepStart).Seconds()

		// Exercise the heavy large-collection read for `settle` seconds so the
		// LIST-nodes histogram + client latency reflect steady state at this scale.
		clientList := c.probeListNodes(ctx, time.Duration(cfg.CeilingSettle)*time.Second)

		p99all, allOK := c.promInstantQuery(ctx, cfg, apiserverP99WindowQuery(window))
		p99list, listOK := c.promInstantQuery(ctx, cfg, apiserverResourceP99Query("LIST", "nodes", window))
		// KWOK controller CPU: metrics-server is the portable source; fall back to
		// Prometheus cAdvisor (not ingested on every cluster).
		kwokCPU, cpuOK := c.kwokControllerCPUCores(ctx)
		if !cpuOK {
			kwokCPU, cpuOK = c.promInstantQuery(ctx, cfg, kwokControllerCPUQuery(window))
		}
		util := c.clusterNodeUtil(ctx)

		readyFrac := float64(ready) / float64(target)
		row := ceilingStep{
			TargetNodes: target, ReadyNodes: ready, FailedCreates: failed,
			ReadyFraction: readyFrac, TimeToReadySec: ttr,
			APIServerP99Sec: fmtFloat(p99all, allOK), ListNodesP99Sec: fmtFloat(p99list, listOK),
			ClientListSec: clientList, KwokControllerCPU: fmtFloat(kwokCPU, cpuOK),
			ClusterCPUPct: util.CPUPct, ClusterMemPct: util.MemPct,
		}
		infof("step %d: ready=%d/%d (%.1f%%) apiserver_p99=%s list_nodes_p99=%s client_list=%.2fs kwok_cpu=%s %s",
			target, ready, target, readyFrac*100, row.APIServerP99Sec, row.ListNodesP99Sec, clientList, row.KwokControllerCPU,
			func() string { _, s := util.breaches(cfg); return s }())

		degraded, attribution, detail := attributeCeiling(cfg, readyFrac, p99all, allOK, p99list, listOK, clientList, kwokCPU, cpuOK, util)
		row.Degraded, row.Attribution, row.Detail = degraded, attribution, detail
		rows = append(rows, row)
		if degraded {
			// Advisory only: flag the rung and keep ramping so the operator sees
			// the full curve and decides the ceiling from the numbers.
			if firstBreach == nil {
				b := row
				firstBreach = &b
			}
			warnf("advisory: %d nodes over guardrail — %s (%s); continuing ramp", target, attribution, detail)
			continue
		}
		if firstBreach == nil {
			lastHealthy = target
		}
	}

	artifact := map[string]any{
		"targets":                   targets,
		"steps":                     rows,
		"ceiling_nodes_proven":      lastHealthy,
		"list_p99_guardrail":        cfg.CeilingListP99,
		"apiserver_p99_guardrail":   cfg.MaxAPIServerP99,
		"kwok_cpu_guardrail_cores":  cfg.CeilingKwokCPU,
		"cluster_cpu_guardrail_pct": cfg.MaxClusterCPUPct,
		"cluster_mem_guardrail_pct": cfg.MaxClusterMemPct,
	}
	if firstBreach != nil {
		artifact["first_over_guardrail"] = firstBreach
	}
	writeArtifact(cfg.ResultsDir, "p0.2-ceiling-sweep.json", artifact)

	res.Metrics["ceiling_nodes_proven"] = lastHealthy
	res.Metrics["steps"] = len(rows)
	res.Finished = time.Now()
	// The full ramp always runs; recording the per-step curve is the success
	// condition. The guardrails are advisory annotations that flag the likely
	// ceiling — the operator decides from the numbers in p0.2-ceiling-sweep.json.
	res.Verdict = "PASS"
	maxTested := targets[len(targets)-1]
	switch {
	case firstBreach == nil:
		res.Message = fmt.Sprintf("full ramp recorded to %d nodes; no rung exceeded the advisory guardrails — ceiling is >= %d", maxTested, maxTested)
	default:
		res.Metrics["first_over_guardrail_nodes"] = firstBreach.TargetNodes
		res.Metrics["first_over_guardrail_note"] = firstBreach.Attribution
		res.Message = fmt.Sprintf("full ramp recorded to %d nodes; first rung over the advisory guardrail at %d — %s (%s); healthy through %d. Decide the ceiling from the per-step numbers.",
			maxTested, firstBreach.TargetNodes, firstBreach.Attribution, firstBreach.Detail, lastHealthy)
	}
	return res
}

// attributeCeiling classifies a step: API server / etcd latency breaching the
// guardrail is the REAL ceiling (bounds Phase 2); nodes failing to reach Ready
// while the API server stays healthy is a HARNESS limit (KWOK controller or
// client throttling) to be tuned away.
func attributeCeiling(cfg Config, readyFrac, p99all float64, allOK bool, p99list float64, listOK bool, clientList, kwokCPU float64, cpuOK bool, util clusterUtil) (bool, string, string) {
	apiBreach := (listOK && p99list > cfg.CeilingListP99) || (allOK && p99all > cfg.MaxAPIServerP99)
	// If Prometheus is unavailable, fall back to the client-observed LIST latency.
	if !listOK && !allOK && clientList > cfg.CeilingListP99 {
		apiBreach = true
	}
	clusterBreach, clusterDetail := util.breaches(cfg)
	nodesLag := readyFrac < 0.999

	switch {
	case apiBreach:
		return true, "apiserver/etcd saturation (REAL ceiling — bounds Phase 2)",
			fmt.Sprintf("list_nodes_p99=%s overall_p99=%s client_list=%.2fs vs guardrails list=%.2fs overall=%.2fs",
				fmtFloat(p99list, listOK), fmtFloat(p99all, allOK), clientList, cfg.CeilingListP99, cfg.MaxAPIServerP99)
	case clusterBreach:
		return true, "cluster CPU/memory saturation (REAL ceiling — bounds Phase 2)", clusterDetail
	case nodesLag && cpuOK && kwokCPU >= cfg.CeilingKwokCPU:
		return true, "kwok-controller saturation (HARNESS limit — tune away: raise KWOK QPS/replicas)",
			fmt.Sprintf("ready_fraction=%.3f kwok_cpu=%.2f cores >= %.2f while apiserver healthy (p99=%s)",
				readyFrac, kwokCPU, cfg.CeilingKwokCPU, fmtFloat(p99all, allOK))
	case nodesLag:
		return true, "nodes not Ready with API server healthy (HARNESS limit — likely client QPS throttling or KWOK; tune harness)",
			fmt.Sprintf("ready_fraction=%.3f apiserver_p99=%s kwok_cpu=%s", readyFrac, fmtFloat(p99all, allOK), fmtFloat(kwokCPU, cpuOK))
	default:
		return false, "", ""
	}
}

// kwokControllerCPUCores reads the KWOK controller's current CPU (cores) from
// the metrics.k8s.io API (metrics-server). Used to attribute "nodes not going
// Ready" to KWOK-controller saturation rather than the API server.
func (c *clients) kwokControllerCPUCores(ctx context.Context) (float64, bool) {
	raw, err := c.kube.CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces", "kube-system", "pods").
		Param("labelSelector", "app=kwok-controller").
		DoRaw(ctx)
	if err != nil {
		return 0, false
	}
	var pm struct {
		Items []struct {
			Containers []struct {
				Usage struct {
					CPU string `json:"cpu"`
				} `json:"usage"`
			} `json:"containers"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &pm); err != nil {
		return 0, false
	}
	total, found := 0.0, false
	for _, it := range pm.Items {
		for _, ctr := range it.Containers {
			if q, err := resource.ParseQuantity(ctr.Usage.CPU); err == nil {
				total += float64(q.MilliValue()) / 1000.0
				found = true
			}
		}
	}
	return total, found
}

// clusterUtil is the real (non-kwok) node CPU/memory utilization, so P0.2 can
// assert the cluster itself stays within normal bounds while the KWOK fabric is
// scaled. KWOK nodes are excluded because their capacity/usage are synthetic and
// would mask the real control-plane/worker pressure that actually bounds Phase 2.
type clusterUtil struct {
	OK               bool    `json:"ok"`
	RealNodes        int     `json:"real_nodes"`
	CPUUsedCores     float64 `json:"cpu_used_cores"`
	CPUCapacityCores float64 `json:"cpu_capacity_cores"`
	CPUPct           float64 `json:"cpu_pct"`
	MemUsedMi        int64   `json:"mem_used_mi"`
	MemCapacityMi    int64   `json:"mem_capacity_mi"`
	MemPct           float64 `json:"mem_pct"`
}

// clusterNodeUtil sums metrics-server usage over the real nodes and divides by
// their allocatable capacity. Returns OK=false if metrics-server is unavailable
// or no real node has capacity (so callers can skip the guardrail rather than
// fail spuriously).
func (c *clients) clusterNodeUtil(ctx context.Context) clusterUtil {
	var u clusterUtil
	nodes, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return u
	}
	real := map[string]bool{}
	var capCPUmilli, capMemMi int64
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Labels["type"] == "kwok" {
			continue
		}
		real[n.Name] = true
		if q, ok := n.Status.Allocatable[corev1.ResourceCPU]; ok {
			capCPUmilli += q.MilliValue()
		}
		if q, ok := n.Status.Allocatable[corev1.ResourceMemory]; ok {
			capMemMi += q.Value() / (1024 * 1024)
		}
	}
	if len(real) == 0 || capCPUmilli == 0 {
		return u
	}

	raw, err := c.kube.CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/nodes").DoRaw(ctx)
	if err != nil {
		return u
	}
	var nm struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Usage struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"usage"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &nm); err != nil {
		return u
	}
	var useCPUmilli, useMemMi int64
	for _, it := range nm.Items {
		if !real[it.Metadata.Name] {
			continue
		}
		if q, err := resource.ParseQuantity(it.Usage.CPU); err == nil {
			useCPUmilli += q.MilliValue()
		}
		if q, err := resource.ParseQuantity(it.Usage.Memory); err == nil {
			useMemMi += q.Value() / (1024 * 1024)
		}
	}

	u.OK = true
	u.RealNodes = len(real)
	u.CPUUsedCores = float64(useCPUmilli) / 1000.0
	u.CPUCapacityCores = float64(capCPUmilli) / 1000.0
	u.CPUPct = float64(useCPUmilli) / float64(capCPUmilli)
	u.MemUsedMi = useMemMi
	u.MemCapacityMi = capMemMi
	if capMemMi > 0 {
		u.MemPct = float64(useMemMi) / float64(capMemMi)
	}
	return u
}

// breaches reports whether the cluster CPU/memory is out of normal bounds and a
// human-readable summary of the utilization vs the guardrails.
func (u clusterUtil) breaches(cfg Config) (bool, string) {
	if !u.OK {
		return false, "cluster cpu/mem: unavailable (metrics-server absent)"
	}
	over := u.CPUPct > cfg.MaxClusterCPUPct || u.MemPct > cfg.MaxClusterMemPct
	return over, fmt.Sprintf("cluster cpu=%.1f%% (%.1f/%.1f cores) mem=%.1f%% (%d/%d Mi) over %d real nodes vs guardrails cpu=%.0f%% mem=%.0f%%",
		u.CPUPct*100, u.CPUUsedCores, u.CPUCapacityCores, u.MemPct*100, u.MemUsedMi, u.MemCapacityMi, u.RealNodes,
		cfg.MaxClusterCPUPct*100, cfg.MaxClusterMemPct*100)
}

// probeListNodes repeatedly does a full quorum LIST of the KWOK nodes for `dur`,
// exercising the heavy large-collection read path (what `kubectl get nodes`
// does) so the API server LIST-nodes histogram and the client-side latency
// reflect this node count. Returns the max client-observed LIST duration.
func (c *clients) probeListNodes(ctx context.Context, dur time.Duration) float64 {
	if dur <= 0 {
		dur = 30 * time.Second
	}
	deadline := time.Now().Add(dur)
	var maxSec float64
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		t0 := time.Now()
		// ResourceVersion unset => quorum read (hits etcd), matching kubectl.
		_, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: kwokNodeLabel})
		if err == nil {
			if s := time.Since(t0).Seconds(); s > maxSec {
				maxSec = s
			}
		}
		if time.Now().After(deadline) {
			return maxSec
		}
		select {
		case <-ctx.Done():
			return maxSec
		case <-tick.C:
		}
	}
}

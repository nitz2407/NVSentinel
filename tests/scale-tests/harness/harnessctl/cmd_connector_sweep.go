/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

// connector-pool experiment sub-modes:
//
//   - startup-burst: recreate the pool with N connectors starting SIMULTANEOUSLY
//     across a sweep of client-go burst values, measuring API-priority-and-fairness
//     (APF) saturation at startup (rejected requests, inqueue peak, wait p99). This
//     reproduces the "connection storm" a large connector DaemonSet inflicts on the
//     API server the moment a cluster (re)starts.
//   - connection-sweep: scale the pool across a sweep of replica counts and record
//     MongoDB connection count + mongod CPU/memory at each step, mapping the
//     connector-density -> datastore-pressure curve (the MongoDB saturation ceiling).

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// poolConfigConfigMap holds the burst-overridden connector config a startup-burst
// experiment mounts over the connector's own config. Torn down with the pool.
const poolConfigConfigMap = "nvs-harness-connector-pool-config"

// ---- APF / MongoDB PromQL ----

func flowRejectedQuery(w string) string {
	return fmt.Sprintf(`sum(increase(apiserver_flowcontrol_rejected_requests_total[%s]))`, w)
}
func flowInqueueQuery(w string) string {
	return fmt.Sprintf(`max_over_time(sum(apiserver_flowcontrol_current_inqueue_requests)[%s:])`, w)
}
func flowWaitP99Query(w string) string {
	return fmt.Sprintf(`histogram_quantile(0.99, sum(rate(apiserver_flowcontrol_request_wait_duration_seconds_bucket[%s])) by (le))`, w)
}
func mongoConnQuery() string {
	// Best-effort across exporters; "n/a" when no mongodb exporter is scraped.
	return `max(mongodb_ss_connections{conn_type="current"} or mongodb_connections{state="current"})`
}

func (c *clients) mongodTop(ctx context.Context, cfg Config, window string) (cpu, memMi string) {
	cpuQ := fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total{namespace=%q,pod=~"mongodb.*",container!="",container!="POD"}[%s]))`, cfg.NVSNamespace, window)
	memQ := fmt.Sprintf(`sum(container_memory_working_set_bytes{namespace=%q,pod=~"mongodb.*",container!="",container!="POD"})/1024/1024`, cfg.NVSNamespace)
	return c.promStr(ctx, cfg, cpuQ), c.promStr(ctx, cfg, memQ)
}

func (c *clients) promStr(ctx context.Context, cfg Config, q string) string {
	v, ok := c.promInstantQuery(ctx, cfg, q)
	return fmtProm(v, ok)
}

func fmtProm(v float64, ok bool) string {
	if !ok {
		return "n/a"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// parseIntCSV parses "5,10,50" into []int, skipping blanks and non-positive.
func parseIntCSV(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if n, err := strconv.Atoi(f); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

// ---- startup-burst ----

type burstRow struct {
	Burst        int     `json:"burst"`
	Replicas     int     `json:"replicas"`
	Ready        bool    `json:"ready"`
	ReadySec     float64 `json:"ready_seconds"`
	RejectedReqs string  `json:"rejected_requests"`
	InqueuePeak  string  `json:"inqueue_peak"`
	WaitP99Sec   string  `json:"wait_p99_seconds"`
	Verdict      string  `json:"verdict"`
}

func (c *clients) startupBurstSweep(ctx context.Context, cfg Config, replicas int, burstSteps []int, window string) error {
	emulated := c.countKwokNodes(ctx)
	if emulated <= 0 {
		return fmt.Errorf("no live KWOK nodes; run `scale-nodes -count N` first")
	}
	realNodes, err := c.schedulableRealNodes(ctx)
	if err != nil {
		return err
	}
	if replicas <= 0 {
		if cur := c.currentPoolReplicas(ctx, cfg); cur > 0 {
			replicas = cur
		} else {
			replicas = computePoolSizing(emulated, realNodes, cfg.ConnectorPoolPerNodeLimit).RealConnectors
		}
	}

	var rows []burstRow
	for _, b := range burstSteps {
		sizing := computePoolSizing(emulated, realNodes, cfg.ConnectorPoolPerNodeLimit)
		sts, svc, err := c.buildConnectorPool(ctx, cfg, sizing)
		if err != nil {
			return err
		}
		r := int32(replicas)
		sts.Spec.Replicas = &r
		if err := c.overrideConnectorBurst(ctx, cfg, sts, b); err != nil {
			return fmt.Errorf("burst override: %w", err)
		}
		infof("burst step: burst=%d replicas=%d simultaneous≈%d — recreating pool (parallel start)", b, replicas, b*replicas)
		if err := c.applyConnectorPool(ctx, cfg, sts, svc); err != nil {
			return err
		}
		t0 := time.Now()
		readyTO := time.Duration(replicas*2+180) * time.Second
		_, ok := c.waitStatefulSetReady(ctx, cfg.NVSNamespace, connectorPoolName, replicas, readyTO)
		readySec := time.Since(t0).Seconds()
		// Let the burst's APF samples land in the rate window before measuring.
		sleepCtx(ctx, 20*time.Second)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		row := burstRow{
			Burst: b, Replicas: replicas, Ready: ok, ReadySec: readySec,
			RejectedReqs: c.promStr(ctx, cfg, flowRejectedQuery(window)),
			InqueuePeak:  c.promStr(ctx, cfg, flowInqueueQuery(window)),
			WaitP99Sec:   c.promStr(ctx, cfg, flowWaitP99Query(window)),
		}
		row.Verdict = "PASS"
		if !ok {
			row.Verdict = "FAIL"
		}
		infof("  burst=%d ready=%v in %.0fs rejected=%s waitP99=%ss -> %s", b, ok, readySec, row.RejectedReqs, row.WaitP99Sec, row.Verdict)
		rows = append(rows, row)
	}
	printBurstTable(rows)
	writeArtifact(cfg.ResultsDir, "connector-startup-burst.json", map[string]any{"replicas": replicas, "window": window, "steps": rows})
	return nil
}

func printBurstTable(rows []burstRow) {
	stepf("startup-burst results")
	infof("%-8s %-9s %-7s %-9s %-12s %-12s %-11s %s", "burst", "replicas", "ready", "readySec", "rejected", "inqueuePk", "waitP99s", "verdict")
	for _, r := range rows {
		infof("%-8d %-9d %-7v %-9.0f %-12s %-12s %-11s %s", r.Burst, r.Replicas, r.Ready, r.ReadySec, r.RejectedReqs, r.InqueuePeak, r.WaitP99Sec, r.Verdict)
	}
}

// overrideConnectorBurst clones the connector's config ConfigMap with the client-go
// burst value swapped to `burst`, into poolConfigConfigMap, and repoints the pool
// pod's config volume at it. If the connector has no ConfigMap-backed config (or no
// burst key), it warns and leaves the chart default in place.
func (c *clients) overrideConnectorBurst(ctx context.Context, cfg Config, sts *appsv1.StatefulSet, burst int) error {
	volName, cmName := connectorConfigMount(&sts.Spec.Template.Spec)
	if cmName == "" {
		warnf("burst override: connector has no ConfigMap-backed config volume; using chart-default burst")
		return nil
	}
	src, err := c.kube.CoreV1().ConfigMaps(cfg.NVSNamespace).Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get connector config %s: %w", cmName, err)
	}
	burstRe := regexp.MustCompile(`(?mi)(k8sconnectorburst|burst)(\s*[:=]\s*)[0-9]+`)
	data := map[string]string{}
	replaced := false
	for k, v := range src.Data {
		data[k] = burstRe.ReplaceAllStringFunc(v, func(m string) string {
			replaced = true
			sub := burstRe.FindStringSubmatch(m)
			return fmt.Sprintf("%s%s%d", sub[1], sub[2], burst)
		})
	}
	if !replaced {
		warnf("burst override: no burst key in %s; pool uses chart-default burst (still measuring startup)", cmName)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: poolConfigConfigMap, Namespace: cfg.NVSNamespace, Labels: map[string]string{connectorPoolLabel: connectorPoolName}}, Data: data}
	_, err = c.kube.CoreV1().ConfigMaps(cfg.NVSNamespace).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = c.kube.CoreV1().ConfigMaps(cfg.NVSNamespace).Update(ctx, cm, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("write override config: %w", err)
	}
	for i := range sts.Spec.Template.Spec.Volumes {
		v := &sts.Spec.Template.Spec.Volumes[i]
		if v.Name == volName && v.ConfigMap != nil {
			v.ConfigMap.Name = poolConfigConfigMap
		}
	}
	return nil
}

// connectorConfigMount finds the pod's ConfigMap-backed config volume, returning
// its volume name and the source ConfigMap name.
func connectorConfigMount(podSpec *corev1.PodSpec) (volName, cmName string) {
	for _, v := range podSpec.Volumes {
		if v.ConfigMap != nil {
			return v.Name, v.ConfigMap.Name
		}
	}
	return "", ""
}

// ---- connection-sweep ----

type connRow struct {
	Replicas    int    `json:"replicas"`
	Ready       bool   `json:"ready"`
	MongoConns  string `json:"mongo_connections"`
	MongodCPU   string `json:"mongod_cpu_cores"`
	MongodMemMi string `json:"mongod_mem_mi"`
}

func (c *clients) connectionSweep(ctx context.Context, cfg Config, replicaSteps []int, settle int, window string) error {
	if c.currentPoolReplicas(ctx, cfg) < 0 {
		return fmt.Errorf("no connector pool deployed; run `connector-pool` first")
	}
	var rows []connRow
	for _, n := range replicaSteps {
		if err := c.scalePoolReplicas(ctx, cfg, n); err != nil {
			return fmt.Errorf("scale to %d: %w", n, err)
		}
		readyTO := time.Duration(n*2+180) * time.Second
		_, ok := c.waitStatefulSetReady(ctx, cfg.NVSNamespace, connectorPoolName, n, readyTO)
		sleepCtx(ctx, time.Duration(settle)*time.Second)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cpu, mem := c.mongodTop(ctx, cfg, window)
		row := connRow{Replicas: n, Ready: ok, MongoConns: c.promStr(ctx, cfg, mongoConnQuery()), MongodCPU: cpu, MongodMemMi: mem}
		infof("  replicas=%d ready=%v mongoConns=%s mongodCPU=%s mongodMem=%sMi", n, ok, row.MongoConns, cpu, mem)
		rows = append(rows, row)
	}
	printConnTable(rows)
	writeArtifact(cfg.ResultsDir, "connector-connection-sweep.json", map[string]any{"window": window, "settle_seconds": settle, "steps": rows})
	return nil
}

func printConnTable(rows []connRow) {
	stepf("connection-sweep results")
	infof("%-9s %-7s %-14s %-14s %s", "replicas", "ready", "mongoConns", "mongodCPU", "mongodMemMi")
	for _, r := range rows {
		infof("%-9d %-7v %-14s %-14s %s", r.Replicas, r.Ready, r.MongoConns, r.MongodCPU, r.MongodMemMi)
	}
}

func (c *clients) scalePoolReplicas(ctx context.Context, cfg Config, n int) error {
	sts, err := c.kube.AppsV1().StatefulSets(cfg.NVSNamespace).Get(ctx, connectorPoolName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	r := int32(n)
	sts.Spec.Replicas = &r
	_, err = c.kube.AppsV1().StatefulSets(cfg.NVSNamespace).Update(ctx, sts, metav1.UpdateOptions{})
	return err
}

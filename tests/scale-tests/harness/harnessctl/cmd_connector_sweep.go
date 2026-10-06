//go:build !injector

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
//   - connection-sweep: self-contained create → sweep → teardown. Stages a
//     connector pool, scales it across a sweep of replica counts while recording
//     MongoDB pressure the same way the 2026-07-30 saturation finding did:
//     mongod logs for connectionCount, metrics-server (kubectl top) for CPU/mem
//     with kubelet stats/summary as fallback when metrics-server is down, and
//     pod Ready/restart health. Prometheus is not required. Tears the pool
//     down at the end. Does not require a prior `pool create`.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	emulated := c.countKwokNodesOrZero(ctx)
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
	// Match JSON ("K8sConnectorBurst": 10) and looser forms (burst: 10 / burst = 10).
	// Quotes around the key are required in config.json and were previously missed,
	// which made every startup-burst trial a no-op on the real chart.
	burstRe := regexp.MustCompile(`(?i)("?)(k8sconnectorburst|burst)("?)(\s*[:=]\s*)[0-9]+`)
	data := map[string]string{}
	replaced := false
	for k, v := range src.Data {
		data[k] = burstRe.ReplaceAllStringFunc(v, func(m string) string {
			replaced = true
			sub := burstRe.FindStringSubmatch(m)
			return fmt.Sprintf("%s%s%s%s%d", sub[1], sub[2], sub[3], sub[4], burst)
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
	Replicas        int    `json:"replicas"`
	Ready           bool   `json:"ready"`
	MongoConns      string `json:"mongo_connections"`
	MongodCPU       string `json:"mongod_cpu_cores"`
	MongodMemMi     string `json:"mongod_mem_mi"`
	MongodReady     string `json:"mongod_ready"`
	MongodRestarts  int    `json:"mongod_restarts"`
	CPULimitHit     bool   `json:"cpu_limit_hit"`
	FQRestarts      int    `json:"fault_quarantine_restarts"`
	DrainerRestarts int    `json:"node_drainer_restarts"`
	CPUSource       string `json:"cpu_source,omitempty"`
}

func (c *clients) connectionSweep(ctx context.Context, cfg Config, replicaSteps []int, settle int, window string) error {
	emulated := c.countKwokNodesOrZero(ctx)
	if emulated <= 0 {
		return fmt.Errorf("no live KWOK nodes; run `nodes scale --count N` first")
	}
	realNodes, err := c.schedulableRealNodes(ctx)
	if err != nil {
		return err
	}
	ceiling := computePoolSizing(emulated, realNodes, cfg.ConnectorPoolPerNodeLimit).PodCeiling

	// Always leave the cluster clean, even if a mid-sweep scale/wait fails.
	defer func() {
		stepf("connection-sweep: teardown")
		if err := c.teardownConnectorPool(ctx, cfg); err != nil {
			warnf("connection-sweep teardown: %v", err)
		}
	}()

	// Create at the first step so an ascending sweep only grows (no shrink-then-
	// grow waste). Injectors are skipped: this experiment only needs connectors
	// holding Mongo connections.
	first := replicaSteps[0]
	if first > ceiling {
		warnf("connection-sweep: clamping first step %d to pod ceiling %d", first, ceiling)
		first = ceiling
	}
	stepf("connection-sweep: create pool at %d replicas (ceiling %d)", first, ceiling)
	if err := c.deployPoolForSweep(ctx, cfg, emulated, realNodes, first); err != nil {
		return fmt.Errorf("create pool: %w", err)
	}

	var rows []connRow
	for _, n := range replicaSteps {
		if n > ceiling {
			warnf("connection-sweep: clamping step %d to pod ceiling %d", n, ceiling)
			n = ceiling
		}
		if err := c.scalePoolReplicas(ctx, cfg, n); err != nil {
			return fmt.Errorf("scale to %d: %w", n, err)
		}
		readyTO := time.Duration(n*2+180) * time.Second
		_, ok := c.waitStatefulSetReady(ctx, cfg.NVSNamespace, connectorPoolName, n, readyTO)
		sleepCtx(ctx, time.Duration(settle)*time.Second)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		row := c.sampleMongoPressure(ctx, cfg, parseLookback(window, settle))
		row.Replicas = n
		row.Ready = ok
		infof("  replicas=%d ready=%v mongoConns=%s mongodCPU=%s mongodMem=%sMi mongodReady=%s restarts=%d limitHit=%v source=%s",
			n, ok, row.MongoConns, row.MongodCPU, row.MongodMemMi, row.MongodReady, row.MongodRestarts, row.CPULimitHit, row.CPUSource)
		rows = append(rows, row)
	}
	printConnTable(rows)
	writeArtifact(cfg.ResultsDir, "connector-connection-sweep.json", map[string]any{
		"source": "mongod-logs+metrics-server|kubelet-summary", "window": window, "settle_seconds": settle,
		"steps": rows, "pod_ceiling": ceiling,
	})
	return nil
}

// deployPoolForSweep stages only the connector StatefulSet at the requested
// replica count (no resident injectors). Used by connection-sweep so the
// experiment owns its own create/teardown lifecycle.
func (c *clients) deployPoolForSweep(ctx context.Context, cfg Config, emulated, realNodes, replicas int) error {
	sizing := computePoolSizing(emulated, realNodes, cfg.ConnectorPoolPerNodeLimit)
	sts, svc, err := c.buildConnectorPool(ctx, cfg, sizing)
	if err != nil {
		return err
	}
	r := int32(replicas)
	sts.Spec.Replicas = &r
	if err := c.applyConnectorPool(ctx, cfg, sts, svc); err != nil {
		return err
	}
	readyTO := time.Duration(replicas*3+120) * time.Second
	ready, ok := c.waitStatefulSetReady(ctx, cfg.NVSNamespace, connectorPoolName, replicas, readyTO)
	if !ok {
		return fmt.Errorf("only %d/%d connector pods Ready within %s", ready, replicas, readyTO)
	}
	return nil
}

func printConnTable(rows []connRow) {
	stepf("connection-sweep results")
	infof("%-9s %-7s %-12s %-12s %-12s %-12s %-9s %-9s %-8s %s",
		"replicas", "ready", "mongoConns", "mongodCPU", "mongodMemMi", "mongodReady", "restarts", "limitHit", "fqRest", "drainerRest")
	for _, r := range rows {
		infof("%-9d %-7v %-12s %-12s %-12s %-12s %-9d %-9v %-8d %d",
			r.Replicas, r.Ready, r.MongoConns, r.MongodCPU, r.MongodMemMi, r.MongodReady,
			r.MongodRestarts, r.CPULimitHit, r.FQRestarts, r.DrainerRestarts)
	}
}

// sampleMongoPressure reads datastore pressure without Prometheus: metrics-server
// for CPU/memory (same source as `kubectl top`), mongod logs for connectionCount,
// and live pod status for Ready/restarts. Peak mongod replica is the saturation
// signal (one member hitting its CPU limit).
func (c *clients) sampleMongoPressure(ctx context.Context, cfg Config, lookback time.Duration) connRow {
	row := connRow{
		MongoConns: "n/a", MongodCPU: "n/a", MongodMemMi: "n/a", MongodReady: "0/0",
	}
	pods, err := c.listMongoServerPods(ctx, cfg.NVSNamespace)
	if err != nil {
		warnf("connection-sweep: list mongodb pods: %v", err)
		return row
	}
	ready, restarts := 0, 0
	for i := range pods {
		if podReady(&pods[i]) {
			ready++
		}
		restarts += int(containerRestartCount(&pods[i], mongoServerContainer(&pods[i])))
	}
	row.MongodReady = fmt.Sprintf("%d/%d", ready, len(pods))
	row.MongodRestarts = restarts

	if cpu, mem, src, hit, err := c.mongoTopFromMetrics(ctx, cfg.NVSNamespace, pods); err == nil {
		row.MongodCPU, row.MongodMemMi, row.CPULimitHit, row.CPUSource = cpu, mem, hit, src
	} else if len(pods) > 0 {
		row.CPUSource = src
		warnf("connection-sweep: mongodb CPU/mem unavailable: %v", err)
	}

	if n, ok := c.readMongoConnectionCount(ctx, cfg.NVSNamespace, pods, lookback); ok {
		row.MongoConns = strconv.Itoa(n)
	} else if len(pods) > 0 {
		warnf("connection-sweep: no connectionCount in mongod logs (lookback %s)", lookback)
	}

	row.FQRestarts = c.prefixPodRestarts(ctx, cfg.NVSNamespace, "fault-quarantine")
	row.DrainerRestarts = c.prefixPodRestarts(ctx, cfg.NVSNamespace, "node-drainer")
	return row
}

func (c *clients) listMongoServerPods(ctx context.Context, ns string) ([]corev1.Pod, error) {
	for _, sel := range []string{
		"app.kubernetes.io/name=mongodb,app.kubernetes.io/component=mongodb",
		"app.kubernetes.io/name=mongodb",
		"app=mongodb",
	} {
		list, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return nil, err
		}
		if out := filterMongoServerPods(list.Items); len(out) > 0 {
			return out, nil
		}
	}
	list, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return filterMongoServerPods(list.Items), nil
}

func filterMongoServerPods(pods []corev1.Pod) []corev1.Pod {
	var out []corev1.Pod
	for i := range pods {
		if isMongoDBServerPod(&pods[i]) {
			out = append(out, pods[i])
		}
	}
	return out
}

func (c *clients) mongoTopFromMetrics(ctx context.Context, ns string, pods []corev1.Pod) (cpu, memMi, source string, limitHit bool, err error) {
	if len(pods) == 0 {
		return "n/a", "n/a", "", false, fmt.Errorf("no mongodb server pods")
	}
	byName, source, err := c.mongoResourceByPod(ctx, ns, pods)
	if err != nil {
		return "n/a", "n/a", source, false, err
	}
	peakMilli, peakMem := int64(-1), int64(-1)
	for i := range pods {
		u, found := byName[pods[i].Name]
		if !found {
			continue
		}
		if u.cpuMilli > peakMilli {
			peakMilli = u.cpuMilli
		}
		if u.memMi > peakMem {
			peakMem = u.memMi
		}
		limit := cpuLimitMilli(&pods[i], mongoServerContainer(&pods[i]))
		if limit > 0 && u.mongodMilli*100 >= limit*95 {
			limitHit = true
		}
	}
	if peakMilli < 0 {
		return "n/a", "n/a", source, false, fmt.Errorf("no usage samples for mongodb pods")
	}
	return fmtCores(peakMilli), strconv.FormatInt(peakMem, 10), source, limitHit, nil
}

type mongoResUsage struct {
	cpuMilli    int64
	memMi       int64
	mongodMilli int64
}

func (c *clients) mongoResourceByPod(ctx context.Context, ns string, pods []corev1.Pod) (map[string]mongoResUsage, string, error) {
	want := map[string]bool{}
	for i := range pods {
		want[pods[i].Name] = true
	}
	var msErr error
	msCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	items, err := c.podUsage(msCtx, ns)
	cancel()
	if err == nil {
		out := map[string]mongoResUsage{}
		for _, it := range items {
			if !want[it.name] {
				continue
			}
			out[it.name] = mongoResUsage{cpuMilli: it.cpuMilli, memMi: it.memMi, mongodMilli: it.cpuMilli}
		}
		if len(out) > 0 {
			return out, "metrics-server", nil
		}
	} else {
		msErr = err
	}
	kubeOut, kErr := c.mongoUsageFromKubelet(ctx, pods)
	if len(kubeOut) > 0 {
		return kubeOut, "kubelet-summary", nil
	}
	switch {
	case msErr != nil && kErr != nil:
		return nil, "", fmt.Errorf("metrics-server: %v; kubelet summary: %w", msErr, kErr)
	case kErr != nil:
		return nil, "", fmt.Errorf("kubelet summary: %w", kErr)
	case msErr != nil:
		return nil, "", fmt.Errorf("metrics-server: %w", msErr)
	default:
		return nil, "", fmt.Errorf("mongodb pods missing from metrics-server and kubelet summary")
	}
}

func (c *clients) mongoUsageFromKubelet(ctx context.Context, pods []corev1.Pod) (map[string]mongoResUsage, error) {
	out := map[string]mongoResUsage{}
	summaries := map[string]*kubeletSummary{}
	var firstErr error
	for i := range pods {
		node := pods[i].Spec.NodeName
		if node == "" {
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
		if u, ok := lookupMongoUsage(sum, pods[i].Namespace, pods[i].Name); ok {
			out[pods[i].Name] = u
		}
	}
	if len(out) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, fmt.Errorf("mongodb pods not present in kubelet stats/summary")
	}
	return out, nil
}

type kubeletSummary struct {
	Node struct {
		CPU struct {
			UsageNanoCores int64 `json:"usageNanoCores"`
		} `json:"cpu"`
		Memory struct {
			WorkingSetBytes int64 `json:"workingSetBytes"`
		} `json:"memory"`
	} `json:"node"`
	Pods []kubeletPodStats `json:"pods"`
}

type kubeletPodStats struct {
	PodRef struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"podRef"`
	CPU struct {
		UsageNanoCores int64 `json:"usageNanoCores"`
	} `json:"cpu"`
	Memory struct {
		WorkingSetBytes int64 `json:"workingSetBytes"`
	} `json:"memory"`
	Containers []kubeletContainerStats `json:"containers"`
}

type kubeletContainerStats struct {
	Name string `json:"name"`
	CPU  struct {
		UsageNanoCores int64 `json:"usageNanoCores"`
	} `json:"cpu"`
	Memory struct {
		WorkingSetBytes int64 `json:"workingSetBytes"`
	} `json:"memory"`
}

func lookupMongoUsage(sum *kubeletSummary, ns, name string) (mongoResUsage, bool) {
	for i := range sum.Pods {
		p := &sum.Pods[i]
		if p.PodRef.Name != name || (p.PodRef.Namespace != "" && p.PodRef.Namespace != ns) {
			continue
		}
		u := mongoResUsage{
			cpuMilli: nanoCoresToMilli(p.CPU.UsageNanoCores),
			memMi:    p.Memory.WorkingSetBytes / (1024 * 1024),
		}
		for _, c := range p.Containers {
			if isMongoDataContainerName(c.Name) {
				u.mongodMilli += nanoCoresToMilli(c.CPU.UsageNanoCores)
			}
		}
		if u.mongodMilli == 0 {
			u.mongodMilli = u.cpuMilli
		}
		return u, true
	}
	return mongoResUsage{}, false
}

func nanoCoresToMilli(nano int64) int64 {
	if nano <= 0 {
		return 0
	}
	return (nano + 500_000) / 1_000_000
}

func (c *clients) readMongoConnectionCount(ctx context.Context, ns string, pods []corev1.Pod, lookback time.Duration) (int, bool) {
	max, found := 0, false
	since := int64(lookback.Seconds())
	if since < 1 {
		since = 60
	}
	tail := int64(2000)
	for i := range pods {
		cname := mongoServerContainer(&pods[i])
		opts := &corev1.PodLogOptions{Container: cname, TailLines: &tail, SinceSeconds: &since}
		req := c.kube.CoreV1().Pods(ns).GetLogs(pods[i].Name, opts)
		stream, err := req.Stream(ctx)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(stream)
		stream.Close()
		if n, ok := parseMaxConnectionCount(string(data)); ok && n >= max {
			max, found = n, true
		}
	}
	return max, found
}

func (c *clients) prefixPodRestarts(ctx context.Context, ns, prefix string) int {
	list, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0
	}
	sum := 0
	for i := range list.Items {
		if !strings.HasPrefix(list.Items[i].Name, prefix) {
			continue
		}
		for _, st := range list.Items[i].Status.ContainerStatuses {
			sum += int(st.RestartCount)
		}
	}
	return sum
}

var (
	mongoSTSPodRe          = regexp.MustCompile(`^mongodb-\d+$`)
	mongoConnectionCountRe = regexp.MustCompile(`(?i)"?connectionCount"?\s*[:=]\s*(\d+)`)
)

func isMongoDBServerPod(p *corev1.Pod) bool {
	if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return false
	}
	if _, job := p.Labels["job-name"]; job {
		return false
	}
	name := p.Name
	if strings.Contains(name, "create-") || strings.Contains(name, "backup") || strings.Contains(name, "exporter") {
		return false
	}
	switch p.Labels["app.kubernetes.io/component"] {
	case "arbiter", "metrics", "exporter":
		return false
	}
	if mongoSTSPodRe.MatchString(name) {
		return true
	}
	if p.Labels["app.kubernetes.io/name"] != "mongodb" && p.Labels["app"] != "mongodb" {
		return false
	}
	return strings.HasPrefix(name, "mongodb")
}

func mongoServerContainer(p *corev1.Pod) string {
	for _, want := range []string{"mongodb", "mongod", "database"} {
		for _, c := range p.Spec.Containers {
			if strings.EqualFold(c.Name, want) {
				return c.Name
			}
		}
	}
	for _, c := range p.Spec.Containers {
		if isMetricsSidecarName(c.Name) {
			continue
		}
		return c.Name
	}
	if len(p.Spec.Containers) > 0 {
		return p.Spec.Containers[0].Name
	}
	return ""
}

func isMongoDataContainerName(name string) bool {
	n := strings.ToLower(name)
	return n == "mongodb" || n == "mongod" || n == "database"
}

func isMetricsSidecarName(name string) bool {
	n := strings.ToLower(name)
	return n == "metrics" || n == "exporter" || strings.Contains(n, "log")
}

func parseMaxConnectionCount(logs string) (int, bool) {
	matches := mongoConnectionCountRe.FindAllStringSubmatch(logs, -1)
	if len(matches) == 0 {
		return 0, false
	}
	max := 0
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	return max, true
}

func containerRestartCount(p *corev1.Pod, container string) int32 {
	for _, st := range p.Status.ContainerStatuses {
		if st.Name == container {
			return st.RestartCount
		}
	}
	return 0
}

func cpuLimitMilli(p *corev1.Pod, container string) int64 {
	for _, c := range p.Spec.Containers {
		if c.Name != container {
			continue
		}
		if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
			return q.MilliValue()
		}
	}
	return 0
}

func parseLookback(window string, settleSec int) time.Duration {
	d, err := time.ParseDuration(window)
	if err != nil || d <= 0 {
		d = 5 * time.Minute
	}
	settle := time.Duration(settleSec)*time.Second + 30*time.Second
	if settle > d {
		return settle
	}
	return d
}

func fmtCores(milli int64) string {
	return strconv.FormatFloat(float64(milli)/1000.0, 'f', 3, 64)
}

func (c *clients) scalePoolReplicas(ctx context.Context, cfg Config, n int) error {
	// Only the harness-owned pool STS is ever scaled — never platform-connectors.
	if err := refuseIfNotHarnessManaged("StatefulSet", connectorPoolName); err != nil {
		return err
	}
	sts, err := c.kube.AppsV1().StatefulSets(cfg.NVSNamespace).Get(ctx, connectorPoolName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	r := int32(n)
	sts.Spec.Replicas = &r
	_, err = c.kube.AppsV1().StatefulSets(cfg.NVSNamespace).Update(ctx, sts, metav1.UpdateOptions{})
	return err
}

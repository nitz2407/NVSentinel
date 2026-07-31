/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

// Live SUT health monitor for long P0.3 operations. While `harnessctl reconcile`
// drains + accounts events, this samples the cluster on a fixed interval and
// records anomalies that would otherwise be invisible in the reconcile output.
// The harness exists to scale-test NVSentinel, so the monitor watches EVERY
// NVSentinel + janitor component (a full-namespace sweep) for every kind of
// failure, not just OOM and not a hand-picked few:
//
//   - node cordoning progress (are fatal events turning into cordons at all?)
//   - remediation CR creation (GPUReset / RebootNode)
//   - pod OOMKilled events (e.g. the labeler OOMing at a 256Mi limit at 5k nodes)
//   - container restarts / CrashLoopBackOff (regular + init containers)
//   - bad container states: ImagePullBackOff, CreateContainerConfigError,
//     RunContainerError, non-OOM non-zero terminations
//   - pods stuck Pending (unschedulable / insufficient resources) or Failed/Evicted
//   - Running-but-NotReady pods (readiness probe failing)
//   - workloads below desired replicas (Deployments / StatefulSets / DaemonSets)
//     — catches pods missing entirely, which a pod sweep can't see
//   - Kubernetes Warning events (FailedScheduling, Unhealthy, FailedMount,
//     FailedCreatePodSandBox, BackOff, Evicted, …) as a catch-all
//   - MongoDB replicaSet quorum loss (the failure mode that collapses the event
//     store under the connector pool — see
//     findings/2026-07-30-mongodb-saturation-under-connector-pool.md)
//   - pod resource saturation (container CPU near its limit, memory near its
//     limit = OOM risk)
//
// The harness's own scaffolding (the connector pool + resident injectors) is
// excluded server-side — it is test load, not the system under test.
//
// Each detected issue is logged (WARN/ERROR) and appended to
// <results>/reconcile-health.jsonl; a roll-up lands in
// <results>/reconcile-health-summary.json when the monitor stops.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// healthFinding is one detected anomaly, serialized to reconcile-health.jsonl.
type healthFinding struct {
	Time     string         `json:"time"`
	Severity string         `json:"severity"` // WARN | ERROR
	Category string         `json:"category"` // cordon | cr | restart | mongo | resource
	Message  string         `json:"message"`
	Details  map[string]any `json:"details,omitempty"`
}

// sutNamespaces are the namespaces holding NVSentinel + janitor components (the
// system under test). Everything in them is watched except harness scaffolding.
func sutNamespaces(cfg Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, ns := range []string{cfg.NVSNamespace, cfg.JanitorNamespace} {
		if ns != "" && !seen[ns] {
			seen[ns] = true
			out = append(out, ns)
		}
	}
	return out
}

// notHarnessSelector excludes the harness's own pods (connector pool + resident
// injectors) server-side, so a full-namespace pod LIST returns only real SUT
// components and never the ~1600 connector pods.
const notHarnessSelector = "!" + connectorPoolLabel + ",!" + poolInjectorLabel

// componentOf derives a stable component name for a pod from its standard labels,
// falling back to the pod's ReplicaSet/DaemonSet name stem.
func componentOf(p *corev1.Pod) string {
	for _, k := range []string{"app.kubernetes.io/name", "app.kubernetes.io/instance", "app"} {
		if v := p.Labels[k]; v != "" {
			return v
		}
	}
	name := p.Name
	// Trim the trailing "-<replicaset-hash>-<pod-hash>" / "-<ordinal>" suffixes.
	for i := 0; i < 2; i++ {
		if idx := strings.LastIndexByte(name, '-'); idx > 0 {
			name = name[:idx]
		}
	}
	return name
}

// reconcileHealthSummary is the roll-up written when the monitor stops.
type reconcileHealthSummary struct {
	RunID            string          `json:"run_id"`
	Started          string          `json:"started_utc"`
	Stopped          string          `json:"stopped_utc"`
	Samples          int             `json:"samples"`
	CordonStart      int             `json:"cordoned_start"`
	CordonEnd        int             `json:"cordoned_end"`
	KwokReadyStart   int             `json:"kwok_ready_start"`
	KwokReadyEnd     int             `json:"kwok_ready_end"`
	GPUResetStart    int             `json:"gpureset_crs_start"`
	GPUResetEnd      int             `json:"gpureset_crs_end"`
	RebootStart      int             `json:"rebootnode_crs_start"`
	RebootEnd        int             `json:"rebootnode_crs_end"`
	MaxRestarts      map[string]int  `json:"max_restarts_by_component,omitempty"`
	OOMKilled        []string        `json:"oomkilled_during_run,omitempty"`
	PriorOOMKilled   []string        `json:"prior_oomkilled_containers,omitempty"`
	PreExisting      []string        `json:"pre_existing_issues,omitempty"`
	IssuesByCategory map[string]int  `json:"issues_by_category,omitempty"`
	MongoQuorumLost  bool            `json:"mongodb_quorum_lost"`
	MongoCPUPinned   bool            `json:"mongodb_cpu_pinned"`
	Findings         []healthFinding `json:"findings"`
	Verdict          string          `json:"verdict"` // HEALTHY | DEGRADED (run-attributable only)
	Note             string          `json:"note,omitempty"`
}

type healthMonitor struct {
	c          *clients
	cfg        Config
	runID      string
	interval   time.Duration
	namespaces []string // SUT namespaces to sweep

	jsonl *os.File

	startedAt time.Time // wall clock at monitor start (for event filtering)

	mu          sync.Mutex
	reported    map[string]bool  // dedup issue keys already logged
	baseRestart map[string]int32 // "ns/pod/container" -> restartCount first seen
	cbase       map[string]cbase // "ns/pod/container" -> state at first sighting
	oomed       map[string]bool  // active OOMs already recorded in summary
	priorOomed  map[string]bool  // pre-existing OOMs already recorded in summary
	workloadBad map[string]int   // "kind/ns/name" -> consecutive samples below desired
	sum         reconcileHealthSummary
	metricsWarn bool // logged the "metrics unavailable" note once
	eventsWarn  bool // logged the "events unavailable" note once
	firstSample bool
}

// cbase captures a container's state the first time the monitor sees it, so
// later samples can distinguish run-attributable failures (new/worsened during
// the run) from pre-existing conditions (already broken before the run began).
type cbase struct {
	restarts int32
	oom      bool // current or last-termination OOM at first sighting
	badWait  bool // bad waiting reason at first sighting
}

// startReconcileMonitor launches the monitor in the background and returns a stop
// function that halts sampling and writes the summary. Best-effort: monitoring
// must never fail the reconcile, so all errors are swallowed into findings/notes.
// Disable via HARNESS_RECONCILE_MONITOR=false.
func (c *clients) startReconcileMonitor(ctx context.Context, cfg Config, runID string) func() {
	if !envBool("HARNESS_RECONCILE_MONITOR", true) {
		return func() {}
	}
	m := &healthMonitor{
		c:           c,
		cfg:         cfg,
		runID:       runID,
		interval:    time.Duration(envInt("HARNESS_MONITOR_INTERVAL_SECONDS", 15)) * time.Second,
		namespaces:  sutNamespaces(cfg),
		startedAt:   time.Now(),
		reported:    map[string]bool{},
		baseRestart: map[string]int32{},
		cbase:       map[string]cbase{},
		oomed:       map[string]bool{},
		priorOomed:  map[string]bool{},
		workloadBad: map[string]int{},
		firstSample: true,
	}
	_ = os.MkdirAll(cfg.ResultsDir, 0o755)
	if f, err := os.Create(filepath.Join(cfg.ResultsDir, "reconcile-health.jsonl")); err == nil {
		m.jsonl = f
	} else {
		warnf("health monitor: cannot open reconcile-health.jsonl: %v", err)
	}
	m.sum = reconcileHealthSummary{
		RunID: runID, Started: ts(),
		MaxRestarts: map[string]int{}, IssuesByCategory: map[string]int{},
	}

	mctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go m.loop(mctx, done)

	stepf("health monitor started (interval %s) — full-namespace watch of %v: cordoning, CRs, OOM/restarts/crashloops, image-pull & config errors, Pending/Failed/Evicted, NotReady, workload replicas, Warning events, MongoDB quorum, resource saturation", m.interval, m.namespaces)
	return func() {
		cancel()
		<-done
		m.finish()
	}
}

func (m *healthMonitor) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	// Sample immediately so a run that ends fast still gets one snapshot.
	m.sample(ctx)
	tick := time.NewTicker(m.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.sample(ctx)
		}
	}
}

func (m *healthMonitor) sample(ctx context.Context) {
	// Skip sampling once the parent context is cancelled (reconcile finished): an
	// in-flight sample would get aborted LISTs and record a spurious all-zero
	// snapshot, corrupting the summary's end-of-run counts.
	if ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	cordoned, kwokReady, ok := m.sampleNodes(ctx)
	if !ok {
		// Node LIST failed (usually cancellation) — don't count this as a sample
		// or overwrite the end-of-run values with an invalid reading.
		return
	}
	m.sum.Samples++

	gpureset := m.countCRs(ctx, gpuresetGVR)
	reboot := m.countCRs(ctx, rebootGVR)
	mongoReady, mongoTotal := m.sampleComponents(ctx)
	m.sampleWorkloads(ctx)
	m.sampleEvents(ctx)

	if m.firstSample {
		m.sum.CordonStart, m.sum.KwokReadyStart = cordoned, kwokReady
		m.sum.GPUResetStart, m.sum.RebootStart = gpureset, reboot
		m.firstSample = false
	}
	m.sum.CordonEnd, m.sum.KwokReadyEnd = cordoned, kwokReady
	m.sum.GPUResetEnd, m.sum.RebootEnd = gpureset, reboot

	// MongoDB majority check: a 3-member RS needs >=2 fully-Ready mongod pods to
	// keep a primary; fewer means the replicaSet is likely primary-less.
	if mongoTotal > 0 && mongoReady < (mongoTotal/2+1) {
		m.sum.MongoQuorumLost = true
		m.record("ERROR", "mongo",
			fmt.Sprintf("MongoDB replicaSet likely has no primary: only %d/%d mongod pods Ready (remediation consumers will crash-loop)", mongoReady, mongoTotal),
			map[string]any{"ready": mongoReady, "total": mongoTotal},
			"mongo-no-primary")
	}

	infof("[monitor] cordoned=%d kwok-ready=%d gpureset-crs=%d rebootnode-crs=%d mongodb-ready=%d/%d",
		cordoned, kwokReady, gpureset, reboot, mongoReady, mongoTotal)
}

// sampleNodes returns cordoned + Ready counts for the KWOK fleet; ok is false when
// the LIST failed (e.g. context cancelled during shutdown) so the caller can skip
// recording an invalid reading.
func (m *healthMonitor) sampleNodes(ctx context.Context) (cordoned, ready int, ok bool) {
	list, err := m.c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: kwokNodeLabel})
	if err != nil {
		return 0, 0, false
	}
	for i := range list.Items {
		n := &list.Items[i]
		if n.Spec.Unschedulable {
			cordoned++
		}
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				ready++
			}
		}
	}
	return cordoned, ready, true
}

// countCRs returns the number of objects of the given CR kind (paginated so a
// large remediation backlog doesn't blow a single response).
func (m *healthMonitor) countCRs(ctx context.Context, gvr schema.GroupVersionResource) int {
	n, cont := 0, ""
	for {
		list, err := m.c.dynamic.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 500, Continue: cont})
		if err != nil {
			return n
		}
		n += len(list.Items)
		cont = list.GetContinue()
		if cont == "" {
			return n
		}
	}
}

// podUsage is a pod's live CPU/memory usage from `kubectl top`.
type podUsage struct {
	cpuMilli int64
	memBytes int64
}

// sampleComponents sweeps every SUT namespace (all NVSentinel + janitor pods,
// minus harness scaffolding and completed job pods) and flags OOMKilled events,
// restarts / CrashLoopBackOff, Running-but-NotReady pods, and CPU/memory
// saturation. Returns Ready vs total pod counts for the mongodb set so the
// caller can run the replicaSet quorum check.
func (m *healthMonitor) sampleComponents(ctx context.Context) (mongoReady, mongoTotal int) {
	for _, ns := range m.namespaces {
		// Exclude harness scaffolding server-side (keeps the ~1600 connector pods
		// out) and drop Succeeded pods so the janitor's finished reset jobs don't
		// flood the sweep.
		pods, err := m.c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			LabelSelector: notHarnessSelector,
			FieldSelector: "status.phase!=Succeeded",
		})
		if err != nil {
			continue
		}
		usage := m.topByPod(ctx, ns)
		for i := range pods.Items {
			p := &pods.Items[i]
			comp := componentOf(p)
			ready := podReady(p)
			if comp == "mongodb" {
				mongoTotal++
				if ready && p.Status.Phase == corev1.PodRunning {
					mongoReady++
				}
			}
			m.checkPodPhase(ns, comp, p)
			m.checkContainers(ns, comp, p)
			if p.Status.Phase == corev1.PodRunning && !ready {
				m.record("WARN", "notready",
					fmt.Sprintf("%s pod %s is Running but NotReady (readiness probe failing)", comp, p.Name),
					map[string]any{"ns": ns, "pod": p.Name},
					"notready:"+ns+"/"+p.Name)
			}
			if u, ok := usage[p.Name]; ok {
				m.checkResource(ns, comp, p, u)
			}
		}
	}
	return mongoReady, mongoTotal
}

// badWaitingReasons are container "waiting" states that always indicate a real
// problem (never a transient healthy startup state).
var badWaitingReasons = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"InvalidImageName":           true,
	"ErrImageNeverPull":          true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"RunContainerError":          true,
	"CreateContainerConfigErr":   true,
}

// checkPodPhase flags pods that are Failed/Evicted, or stuck Pending well past
// startup (unschedulable, image pull, etc.).
func (m *healthMonitor) checkPodPhase(ns, comp string, p *corev1.Pod) {
	switch p.Status.Phase {
	case corev1.PodFailed:
		msg := strings.TrimSpace(p.Status.Reason + " " + p.Status.Message)
		m.record("ERROR", "phase",
			fmt.Sprintf("%s pod %s is Failed: %s", comp, p.Name, orNA(msg)),
			map[string]any{"ns": ns, "pod": p.Name, "reason": p.Status.Reason},
			"failed:"+ns+"/"+p.Name)
	case corev1.PodPending:
		// Ignore pods that only just appeared — Pending is normal at startup.
		if time.Since(p.CreationTimestamp.Time) < 90*time.Second {
			return
		}
		reason := "Pending"
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
				reason = strings.TrimSpace(cond.Reason + ": " + cond.Message)
			}
		}
		m.record("WARN", "pending",
			fmt.Sprintf("%s pod %s stuck Pending for %s (%s)", comp, p.Name, age(p.CreationTimestamp.Time), reason),
			map[string]any{"ns": ns, "pod": p.Name, "reason": reason},
			"pending:"+ns+"/"+p.Name)
	}
}

// checkContainers flags OOMKilled containers, other bad container states
// (CrashLoopBackOff / ImagePull* / config errors / non-OOM terminations) and
// restart-count increases, across both init and regular containers.
func (m *healthMonitor) checkContainers(ns, comp string, p *corev1.Pod) {
	m.checkContainerSet(ns, comp, p, p.Status.InitContainerStatuses, true)
	m.checkContainerSet(ns, comp, p, p.Status.ContainerStatuses, false)
}

func (m *healthMonitor) checkContainerSet(ns, comp string, p *corev1.Pod, statuses []corev1.ContainerStatus, init bool) {
	kind := "container"
	if init {
		kind = "init-container"
	}
	for _, cs := range statuses {
		key := ns + "/" + p.Name + "/" + cs.Name
		oomNow := cs.State.Terminated != nil && cs.State.Terminated.Reason == "OOMKilled"
		oomEver := oomReason(cs) // current or last-termination OOM
		var waitReason string
		if w := cs.State.Waiting; w != nil && badWaitingReasons[w.Reason] {
			waitReason = w.Reason
		}

		base, seen := m.cbase[key]
		if !seen {
			// First sighting: whatever we see here predates the run window, so it is
			// pre-existing (WARN) and never counts toward the run-attributable verdict.
			m.cbase[key] = cbase{restarts: cs.RestartCount, oom: oomEver, badWait: waitReason != ""}
			m.baseRestart[key] = cs.RestartCount
			if cs.RestartCount > 0 {
				m.bumpRestarts(comp, int(cs.RestartCount))
				m.addPreExisting(fmt.Sprintf("%s (%d prior restarts)", comp, cs.RestartCount))
				m.record("WARN", "restart",
					fmt.Sprintf("%s %s %s/%s entered monitoring with %d prior restart(s) — pre-existing", comp, kind, p.Name, cs.Name, cs.RestartCount),
					map[string]any{"ns": ns, "pod": p.Name, "container": cs.Name, "restarts": cs.RestartCount, "when": "pre-existing"},
					"prior-restart:"+key)
			}
			if oomEver {
				m.recordOOM(comp, p.Name, cs.Name, cs.RestartCount, false)
			}
			if waitReason != "" {
				m.addPreExisting(fmt.Sprintf("%s (%s)", comp, waitReason))
				m.record("WARN", "waiting",
					fmt.Sprintf("%s %s %s/%s entered monitoring already %s: %s — pre-existing", comp, kind, p.Name, cs.Name, waitReason, orNA(cs.State.Waiting.Message)),
					map[string]any{"ns": ns, "pod": p.Name, "container": cs.Name, "reason": waitReason, "when": "pre-existing"},
					"prior-waiting:"+key)
			}
			continue
		}

		// Subsequent samples: only NEW or worsening conditions are run-attributable.
		restarted := cs.RestartCount > base.restarts
		if restarted {
			m.bumpRestarts(comp, int(cs.RestartCount))
			m.record("ERROR", "restart",
				fmt.Sprintf("%s %s %s/%s restarted %d time(s) during the run (total %d) — CrashLoopBackOff", comp, kind, p.Name, cs.Name, int(cs.RestartCount-base.restarts), cs.RestartCount),
				map[string]any{"ns": ns, "pod": p.Name, "container": cs.Name, "restarts": cs.RestartCount, "when": "during-run"},
				fmt.Sprintf("restart:%s:%d", key, cs.RestartCount))
		}
		// Active OOM: a restart during the run whose last termination was OOM, or a
		// container currently sitting in an OOM-terminated state.
		if (restarted && oomEver) || oomNow {
			m.recordOOM(comp, p.Name, cs.Name, cs.RestartCount, true)
		}
		// Newly entered a bad waiting state not present at baseline.
		if waitReason != "" && !base.badWait {
			m.record("ERROR", "waiting",
				fmt.Sprintf("%s %s %s/%s entered %s during the run: %s", comp, kind, p.Name, cs.Name, waitReason, orNA(cs.State.Waiting.Message)),
				map[string]any{"ns": ns, "pod": p.Name, "container": cs.Name, "reason": waitReason, "when": "during-run"},
				fmt.Sprintf("waiting:%s:%s", key, waitReason))
		}
		// Non-OOM error termination that surfaced with a restart during the run.
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 && t.Reason != "OOMKilled" && restarted {
			m.record("ERROR", "terminated",
				fmt.Sprintf("%s %s %s/%s terminated: %s (exit %d)", comp, kind, p.Name, cs.Name, orNA(t.Reason), t.ExitCode),
				map[string]any{"ns": ns, "pod": p.Name, "container": cs.Name, "reason": t.Reason, "exit_code": t.ExitCode, "when": "during-run"},
				fmt.Sprintf("term:%s:%d", key, cs.RestartCount))
		}
	}
}

// addPreExisting records a de-duplicated pre-existing instability summary line.
func (m *healthMonitor) addPreExisting(s string) {
	for _, e := range m.sum.PreExisting {
		if e == s {
			return
		}
	}
	m.sum.PreExisting = append(m.sum.PreExisting, s)
}

// checkResource flags a pod whose live usage is near its container limits: memory
// >=85% of limit (OOM risk) or CPU >=90% of limit (throttling/saturation).
func (m *healthMonitor) checkResource(ns, comp string, p *corev1.Pod, u podUsage) {
	cpuLimit, memLimit := podLimits(p)
	if memLimit > 0 && u.memBytes > 0 && float64(u.memBytes) >= 0.85*float64(memLimit) {
		m.record("WARN", "resource",
			fmt.Sprintf("%s pod %s memory %s is >=85%% of its %s limit (OOM risk)", comp, p.Name, humanBytes(u.memBytes), humanBytes(memLimit)),
			map[string]any{"ns": ns, "pod": p.Name, "mem_bytes": u.memBytes, "mem_limit_bytes": memLimit},
			"mem-sat:"+ns+"/"+p.Name)
	}
	if cpuLimit > 0 && u.cpuMilli > 0 && float64(u.cpuMilli) >= 0.9*float64(cpuLimit) {
		if comp == "mongodb" {
			m.sum.MongoCPUPinned = true
		}
		m.record("WARN", "resource",
			fmt.Sprintf("%s pod %s CPU %dm is >=90%% of its %dm limit (saturation)", comp, p.Name, u.cpuMilli, cpuLimit),
			map[string]any{"ns": ns, "pod": p.Name, "cpu_milli": u.cpuMilli, "cpu_limit_milli": cpuLimit},
			"cpu-sat:"+ns+"/"+p.Name)
	}
}

// recordOOM logs an OOMKilled container once. active=true means the kill
// happened during the run (ERROR, degrades the run verdict); active=false means
// it is pre-existing OOM history seen at baseline (WARN, informational — the
// memory limit is marginal at this scale but the run itself didn't trigger it).
func (m *healthMonitor) recordOOM(comp, pod, container string, restarts int32, active bool) {
	key := comp + "/" + pod + "/" + container
	m.bumpRestarts(comp, int(restarts))
	if active {
		if !m.oomed[key] {
			m.oomed[key] = true
			m.sum.OOMKilled = append(m.sum.OOMKilled, key)
		}
		m.record("ERROR", "oom",
			fmt.Sprintf("%s container %s/%s was OOMKilled during the run (memory limit too low for this scale; %d restart(s))", comp, pod, container, restarts),
			map[string]any{"pod": pod, "container": container, "restarts": restarts, "when": "during-run"},
			"oom:"+key)
		return
	}
	if !m.priorOomed[key] {
		m.priorOomed[key] = true
		m.sum.PriorOOMKilled = append(m.sum.PriorOOMKilled, key)
	}
	m.addPreExisting(fmt.Sprintf("%s (prior OOMKilled, %d restarts)", comp, restarts))
	m.record("WARN", "oom",
		fmt.Sprintf("%s container %s/%s has prior OOMKilled history (%d restart(s)) — pre-existing; memory limit marginal at this scale", comp, pod, container, restarts),
		map[string]any{"pod": pod, "container": container, "restarts": restarts, "when": "pre-existing"},
		"prior-oom:"+key)
}

func (m *healthMonitor) bumpRestarts(comp string, n int) {
	if n > m.sum.MaxRestarts[comp] {
		m.sum.MaxRestarts[comp] = n
	}
}

// topByPod runs `kubectl top pod` once for the namespace and returns per-pod
// usage keyed by pod name. Best-effort: empty if metrics-server is unavailable.
func (m *healthMonitor) topByPod(ctx context.Context, ns string) map[string]podUsage {
	out, err := m.c.kubectl(ctx, nil, "top", "pod", "-n", ns, "-l", notHarnessSelector, "--no-headers")
	if err != nil {
		if !m.metricsWarn {
			m.metricsWarn = true
			warnf("[monitor] kubectl top unavailable (metrics-server saturated?): %v — skipping resource-saturation checks", err)
		}
		return nil
	}
	res := map[string]podUsage{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		res[f[0]] = podUsage{cpuMilli: parseMilliCPU(f[1]), memBytes: parseBytes(f[2])}
	}
	return res
}

// sampleWorkloads flags Deployments/StatefulSets/DaemonSets running below their
// desired replica count — catches components whose pods are missing entirely
// (evicted, unschedulable) which the per-pod sweep can't see. Requires the
// shortfall to persist across two samples to skip normal rollout blips.
func (m *healthMonitor) sampleWorkloads(ctx context.Context) {
	opts := metav1.ListOptions{LabelSelector: notHarnessSelector}
	for _, ns := range m.namespaces {
		if deps, err := m.c.kube.AppsV1().Deployments(ns).List(ctx, opts); err == nil {
			for i := range deps.Items {
				d := &deps.Items[i]
				m.flagWorkload("deploy", ns, d.Name, d.Status.ReadyReplicas, replicaDesired(d.Spec.Replicas))
			}
		}
		if sts, err := m.c.kube.AppsV1().StatefulSets(ns).List(ctx, opts); err == nil {
			for i := range sts.Items {
				s := &sts.Items[i]
				m.flagWorkload("statefulset", ns, s.Name, s.Status.ReadyReplicas, replicaDesired(s.Spec.Replicas))
			}
		}
		if ds, err := m.c.kube.AppsV1().DaemonSets(ns).List(ctx, opts); err == nil {
			for i := range ds.Items {
				d := &ds.Items[i]
				m.flagWorkload("daemonset", ns, d.Name, d.Status.NumberReady, d.Status.DesiredNumberScheduled)
			}
		}
	}
}

func (m *healthMonitor) flagWorkload(kind, ns, name string, ready, desired int32) {
	wkey := kind + "/" + ns + "/" + name
	if desired > 0 && ready < desired {
		m.workloadBad[wkey]++
		if m.workloadBad[wkey] >= 2 { // persisted across >=2 samples
			m.record("WARN", "workload",
				fmt.Sprintf("%s %s/%s has %d/%d ready replicas (pods missing/not ready)", kind, ns, name, ready, desired),
				map[string]any{"ns": ns, "name": name, "ready": ready, "desired": desired},
				"workload:"+wkey)
		}
		return
	}
	delete(m.workloadBad, wkey)
}

// sampleEvents scoops up Kubernetes Warning events in the SUT namespaces — the
// catch-all for problems the object-state sweeps miss: FailedScheduling, probe
// Unhealthy, FailedMount, FailedCreatePodSandBox, Evicted, BackOff, etc. Only
// events at/after monitor start are considered, deduped per object+reason.
func (m *healthMonitor) sampleEvents(ctx context.Context) {
	grace := m.startedAt.Add(-30 * time.Second)
	for _, ns := range m.namespaces {
		evs, err := m.c.kube.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "type=Warning"})
		if err != nil {
			if !m.eventsWarn {
				m.eventsWarn = true
				warnf("[monitor] cannot list Warning events in %s: %v", ns, err)
			}
			continue
		}
		for i := range evs.Items {
			e := &evs.Items[i]
			if e.Type != corev1.EventTypeWarning {
				continue
			}
			when := eventTime(e)
			if !when.IsZero() && when.Before(grace) {
				continue // stale event from before this run
			}
			obj := e.InvolvedObject
			if strings.HasPrefix(obj.Name, connectorPoolName) || strings.HasPrefix(obj.Name, poolInjectorDaemonSet) {
				continue // harness scaffolding, not the SUT
			}
			m.record("WARN", "event",
				fmt.Sprintf("%s/%s: %s — %s (x%d)", strings.ToLower(obj.Kind), obj.Name, e.Reason, orNA(strings.TrimSpace(e.Message)), maxInt32(e.Count, 1)),
				map[string]any{"ns": ns, "kind": obj.Kind, "name": obj.Name, "reason": e.Reason, "count": e.Count},
				fmt.Sprintf("event:%s/%s/%s/%s", ns, obj.Kind, obj.Name, e.Reason))
		}
	}
}

func replicaDesired(r *int32) int32 {
	if r == nil {
		return 1
	}
	return *r
}

func eventTime(e *corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.FirstTimestamp.Time
}

func maxInt32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

func orNA(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
}

func age(t time.Time) string {
	return time.Since(t).Round(time.Second).String()
}

// oomReason reports whether a container's current or last-termination state was
// an OOM kill.
func oomReason(cs corev1.ContainerStatus) bool {
	if t := cs.State.Terminated; t != nil && t.Reason == "OOMKilled" {
		return true
	}
	if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" {
		return true
	}
	return false
}

// podLimits sums the CPU (millicores) and memory (bytes) limits across a pod's
// containers; 0 means unlimited/unset for that resource.
func podLimits(p *corev1.Pod) (cpuMilli, memBytes int64) {
	for _, ct := range p.Spec.Containers {
		if q, ok := ct.Resources.Limits[corev1.ResourceCPU]; ok {
			cpuMilli += q.MilliValue()
		}
		if q, ok := ct.Resources.Limits[corev1.ResourceMemory]; ok {
			memBytes += q.Value()
		}
	}
	return cpuMilli, memBytes
}

func parseMilliCPU(s string) int64 {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.MilliValue()
}

func parseBytes(s string) int64 {
	// `kubectl top` prints memory as e.g. "196Mi"; ParseQuantity handles the suffix.
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.Value()
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.0f%ci", float64(b)/float64(div), "KMGTPE"[exp])
}

// record logs a finding once per dedup key and appends it to the jsonl stream.
func (m *healthMonitor) record(sev, cat, msg string, details map[string]any, key string) {
	if m.reported[key] {
		return
	}
	m.reported[key] = true
	if m.sum.IssuesByCategory == nil {
		m.sum.IssuesByCategory = map[string]int{}
	}
	m.sum.IssuesByCategory[cat]++
	f := healthFinding{Time: ts(), Severity: sev, Category: cat, Message: msg, Details: details}
	m.sum.Findings = append(m.sum.Findings, f)
	if sev == "ERROR" {
		errorf("[monitor] %s", msg)
	} else {
		warnf("[monitor] %s", msg)
	}
	if m.jsonl != nil {
		if b, err := json.Marshal(f); err == nil {
			_, _ = m.jsonl.Write(append(b, '\n'))
		}
	}
}

// finish writes the roll-up summary and closes the jsonl stream.
func (m *healthMonitor) finish() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sum.Stopped = ts()

	// Verdict: degraded if any error-severity finding fired or Mongo lost quorum.
	degraded := m.sum.MongoQuorumLost
	for _, f := range m.sum.Findings {
		if f.Severity == "ERROR" {
			degraded = true
		}
	}
	// Cordoning never advanced despite the run: call it out (may just mean no fatal
	// events landed on real nodes, but with a healthy pipeline it should move).
	cordonDelta := m.sum.CordonEnd - m.sum.CordonStart
	crDelta := (m.sum.GPUResetEnd - m.sum.GPUResetStart) + (m.sum.RebootEnd - m.sum.RebootStart)
	if degraded {
		m.sum.Verdict = "DEGRADED"
	} else {
		m.sum.Verdict = "HEALTHY"
	}
	notes := []string{}
	if len(m.sum.OOMKilled) > 0 {
		sort.Strings(m.sum.OOMKilled)
		notes = append(notes, "OOMKilled during run: "+strings.Join(m.sum.OOMKilled, ", "))
	}
	if len(m.sum.PreExisting) > 0 {
		sort.Strings(m.sum.PreExisting)
		notes = append(notes, "pre-existing instability (not attributed to this run): "+strings.Join(m.sum.PreExisting, ", "))
	}
	if cordonDelta <= 0 && crDelta <= 0 && degraded {
		notes = append(notes, "no new cordons or remediation CRs while the pipeline was degraded")
	}
	sort.Slice(m.sum.Findings, func(i, j int) bool { return m.sum.Findings[i].Time < m.sum.Findings[j].Time })
	m.sum.Note = strings.Join(notes, "; ")

	writeArtifact(m.cfg.ResultsDir, "reconcile-health-summary.json", m.sum)
	if m.jsonl != nil {
		_ = m.jsonl.Close()
	}
	stepf("health monitor: verdict=%s (run-attributable) samples=%d cordoned %d→%d gpureset-crs %d→%d oomkilled-during-run=%v prior-oom=%d pre-existing=%d mongo-quorum-lost=%t findings=%d by-category=%v",
		m.sum.Verdict, m.sum.Samples, m.sum.CordonStart, m.sum.CordonEnd,
		m.sum.GPUResetStart, m.sum.GPUResetEnd, m.sum.OOMKilled, len(m.sum.PriorOOMKilled), len(m.sum.PreExisting), m.sum.MongoQuorumLost, len(m.sum.Findings), m.sum.IssuesByCategory)
}

/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// runInstallScript runs a single helm-install shell script from dir. Used by
// P0.1 bring-up to install any missing components.
func runInstallScript(ctx context.Context, dir, name string, extraEnv []string) error {
	path := filepath.Join(dir, name)
	infof("running install script: %s", path)
	cmd := exec.CommandContext(ctx, "bash", path)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	// Carry the version targets so a version-triggered reinstall lands on the
	// requested tag; empty targets are omitted so the script keeps its default.
	cmd.Env = append(os.Environ(), extraEnv...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// versionEnv builds the KEY=VALUE overrides passed to the install scripts from
// the non-empty version targets. Each script reads only its own variable.
func versionEnv(cfg Config) []string {
	var env []string
	add := func(k, v string) {
		if v != "" {
			env = append(env, k+"="+v)
		}
	}
	add("NVS_CHART_VERSION", cfg.NVSChartVersion)
	add("KWOK_VERSION", cfg.KWOKVersion)
	add("CERT_MANAGER_VERSION", cfg.CertManagerVersion)
	add("METRICS_SERVER_VERSION", cfg.MetricsServerVersion)
	return env
}

// component is one P0.1 building block: how to detect whether it is already
// present, and which install script brings it up (or reconciles it to harness
// defaults).
type component struct {
	id     string
	script string
	// required marks whether a failed install aborts bring-up. Observability
	// niceties (monitoring, metrics-server) are optional: a failure there is
	// warned-and-skipped so it never blocks the critical NVSentinel path.
	required bool
	detect   func(ctx context.Context, c *clients, cfg Config) (present bool, detail string)
}

// p01Components lists the P0.1 stack in install order (numeric script prefixes
// sort naturally: 10 → 15 → 20 → 25 → 30). The janitor is NOT a separate entry:
// it ships inside the NVSentinel chart (30-install-nvsentinel.sh), so its status
// is reported as an informational sub-line under nvsentinel in runBringup.
// monitoring + metrics-server are optional (observability niceties); kwok,
// cert-manager and nvsentinel are required.
func p01Components() []component {
	return []component{
		{"kube-prometheus-stack", "10-install-monitoring.sh", false, detectMonitoring},
		{"metrics-server", "15-install-metrics-server.sh", false, detectMetricsServer},
		{"kwok", "20-install-kwok.sh", true, detectKwok},
		{"cert-manager", "25-install-cert-manager.sh", true, detectCertManager},
		{"nvsentinel", "30-install-nvsentinel.sh", true, detectNVSentinel},
	}
}

// deployPresent reports whether any of the named Deployments exists in ns.
func (c *clients) deployPresent(ctx context.Context, ns string, names ...string) (bool, string) {
	for _, n := range names {
		if _, err := c.kube.AppsV1().Deployments(ns).Get(ctx, n, metav1.GetOptions{}); err == nil {
			return true, fmt.Sprintf("deploy/%s in %s", n, ns)
		}
	}
	return false, ""
}

// dsPresent reports whether any of the named DaemonSets exists in ns.
func (c *clients) dsPresent(ctx context.Context, ns string, names ...string) (bool, string) {
	for _, n := range names {
		if _, err := c.kube.AppsV1().DaemonSets(ns).Get(ctx, n, metav1.GetOptions{}); err == nil {
			return true, fmt.Sprintf("ds/%s in %s", n, ns)
		}
	}
	return false, ""
}

// detectMonitoring is presence-only. Unlike the other components it is NOT
// version-gated: the harness pins kube-prometheus-stack by Helm CHART version
// (KPS_CHART_VERSION, e.g. 65.5.0), which is unrelated to the operator's
// container image tag — so an image-tag comparison would be meaningless.
func detectMonitoring(ctx context.Context, c *clients, cfg Config) (bool, string) {
	// With fullnameOverride=prometheus the operator Deployment is
	// "prometheus-operator"; the other names cover a stock (no-override) install.
	return c.deployPresent(ctx, cfg.MonitoringNamespace,
		"prometheus-operator", "prometheus-kube-prometheus-operator", "kube-prometheus-stack-operator")
}

func detectKwok(ctx context.Context, c *clients, cfg Config) (bool, string) {
	present, detail := c.deployPresent(ctx, cfg.KWOKNamespace, "kwok-controller")
	if !present {
		return false, ""
	}
	return gateVersion("kwok", cfg.KWOKVersion,
		c.deployImageTag(ctx, cfg.KWOKNamespace, "kwok-controller"), detail)
}

// detectMetricsServer checks for metrics-server (always in kube-system). It
// powers P0.2's real-node CPU/mem guardrail; managed clusters ship it, Kind
// does not.
func detectMetricsServer(ctx context.Context, c *clients, cfg Config) (bool, string) {
	present, detail := c.deployPresent(ctx, "kube-system", "metrics-server")
	if !present {
		return false, ""
	}
	return gateVersion("metrics-server", cfg.MetricsServerVersion,
		c.deployImageTag(ctx, "kube-system", "metrics-server"), detail)
}

func detectCertManager(ctx context.Context, c *clients, cfg Config) (bool, string) {
	present, detail := c.deployPresent(ctx, cfg.CertManagerNamespace, "cert-manager-webhook", "cert-manager")
	if !present {
		return false, ""
	}
	return gateVersion("cert-manager", cfg.CertManagerVersion,
		c.deployImageTag(ctx, cfg.CertManagerNamespace, "cert-manager", "cert-manager-controller", "cert-manager-webhook"), detail)
}

// detectNVSentinel is version-aware: if NVSentinel is present AND a target version
// (-nvs-chart-version) is set AND the installed image tag differs, it reports the
// component as MISSING so runBringup re-runs 30-install-nvsentinel.sh — which does
// a `helm upgrade --install` to the target tag. When the versions already match
// (or no target is set) it reports PRESENT and is left untouched.
func detectNVSentinel(ctx context.Context, c *clients, cfg Config) (bool, string) {
	present, detail := c.deployPresent(ctx, cfg.NVSNamespace,
		"health-events-analyzer", "fault-quarantine", "node-drainer", "fault-remediation")
	if !present {
		if ok, d := c.dsPresent(ctx, cfg.NVSNamespace, "platform-connectors"); ok {
			present, detail = true, d
		}
	}
	if !present {
		return false, ""
	}
	installed := c.deployImageTag(ctx, cfg.NVSNamespace,
		"fault-quarantine", "health-events-analyzer", "node-drainer", "fault-remediation")
	return gateVersion("nvsentinel", cfg.NVSChartVersion, installed, detail)
}

// gateVersion applies an optional version gate to an already-present component.
// With no target, an undeterminable installed tag, or a matching tag it reports
// PRESENT (appending the version to the detail line when known). On a real
// mismatch it warns and reports MISSING so bringup reinstalls/upgrades to target.
func gateVersion(id, target, installed, detail string) (bool, string) {
	if target == "" || installed == "" || installed == target {
		if installed != "" {
			return true, detail + ", version " + installed
		}
		return true, detail
	}
	warnf("  %s present at %s but target is %s -> will upgrade", id, installed, target)
	return false, ""
}

// deployImageTag returns the container image tag of the first of the named
// Deployments that exists in namespace, or "" if none is found / all untagged.
func (c *clients) deployImageTag(ctx context.Context, namespace string, names ...string) string {
	for _, name := range names {
		d, err := c.kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		for _, ct := range d.Spec.Template.Spec.Containers {
			if tag := imageTag(ct.Image); tag != "" {
				return tag
			}
		}
	}
	return ""
}

// imageTag returns the tag portion of a container image ref (after the final
// ':', ignoring a registry port and any @digest), or "" if untagged.
func imageTag(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon > slash {
		return image[colon+1:]
	}
	return ""
}

func detectJanitor(ctx context.Context, c *clients, cfg Config) (bool, string) {
	if ok, d := c.deployPresent(ctx, cfg.NVSNamespace, "janitor"); ok {
		return true, d
	}
	// Platform-managed janitor (e.g. ArgoCD) lives in its own namespace.
	return c.deployPresent(ctx, cfg.JanitorNamespace, "dgxc-janitor-controller-manager")
}

// runBringup is the single P0.1 command. It (1) verifies cluster reachability,
// (2) detects which of the harness components are already present at the required
// version, (3) installs whatever is missing or version-mismatched, and (4) prints
// the final node inventory. It is fully declarative and non-interactive: a
// component that is already present at the target version is skipped, everything
// else is installed. NVSentinel version-awareness is handled in detectNVSentinel
// (a version mismatch surfaces as MISSING, triggering a helm upgrade to the
// target -nvs-chart-version). Installs are thin `helm upgrade --install` shell
// scripts — transparent one-liners that Go orchestrates but does not replace.
func runBringup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stack bringup", flag.ExitOnError)
	cfg := defaultConfig()
	bindNvsNamespaceFlag(fs, &cfg)
	bindMonitoringNamespaceFlag(fs, &cfg)
	bindCertManagerNamespaceFlag(fs, &cfg)
	bindKwokNamespaceFlag(fs, &cfg)
	bindJanitorNamespaceFlag(fs, &cfg)
	bindVersionFlags(fs, &cfg)
	dir := fs.String("dir", defaultInstallDir(), "directory holding the 10|15|20|25|30-install-*.sh scripts")
	_ = fs.Parse(args)

	c, err := newClients(cfg)
	if err != nil {
		return err
	}

	// (1) Reachability.
	stepf("P0.1 bring-up: verifying cluster reachability")
	ver, err := c.kube.Discovery().ServerVersion()
	if err != nil {
		return fmt.Errorf("cannot reach cluster: %w", err)
	}
	infof("cluster reachable: kubernetes %s", ver.GitVersion)

	// (2) Detect.
	stepf("P0.1 bring-up: detecting components")
	comps := p01Components()
	present := make([]bool, len(comps))
	for i, comp := range comps {
		ok, detail := comp.detect(ctx, c, cfg)
		present[i] = ok
		if ok {
			infof("  %-22s PRESENT  (%s)", comp.id, detail)
		} else {
			infof("  %-22s MISSING", comp.id)
		}
		// The janitor ships inside the NVSentinel chart, so it is not a standalone
		// component; surface its status as an info sub-line under nvsentinel.
		if comp.id == "nvsentinel" {
			if jok, jd := detectJanitor(ctx, c, cfg); jok {
				infof("    - janitor (in-chart)  PRESENT  (%s)", jd)
			} else {
				infof("    - janitor (in-chart)  MISSING")
			}
		}
	}

	// (3) Decide, per component, whether to run its install script: install what
	// is missing (or version-mismatched, which detect surfaces as MISSING), skip
	// what is already present at the target version. No prompting.
	run := make([]bool, len(comps))
	for i := range comps {
		run[i] = !present[i]
	}

	// Build the ordered, de-duplicated task set, carrying each script's required
	// flag (a script is required if ANY component mapping to it is required).
	type installTask struct {
		script   string
		required bool
	}
	var tasks []installTask
	idxOf := map[string]int{}
	for i, comp := range comps {
		if !run[i] {
			continue
		}
		if j, ok := idxOf[comp.script]; ok {
			tasks[j].required = tasks[j].required || comp.required
			continue
		}
		idxOf[comp.script] = len(tasks)
		tasks = append(tasks, installTask{script: comp.script, required: comp.required})
	}
	if len(tasks) == 0 {
		infof("nothing to install; all P0.1 components already present")
	} else {
		stepf("P0.1 bring-up: installing %d script(s) from %s", len(tasks), *dir)
		scriptEnv := versionEnv(cfg)
		for _, t := range tasks {
			if err := runInstallScript(ctx, *dir, t.script, scriptEnv); err != nil {
				if t.required {
					return err
				}
				// Optional component (observability niceties): don't let it block
				// the critical NVSentinel path — warn and continue.
				warnf("optional component %s failed to install: %v — continuing", t.script, err)
			}
		}
	}

	// (5) Verify reachability + node inventory.
	stepf("P0.1 bring-up: verifying nodes")
	real, kwok, err := c.nodeInventory(ctx)
	if err != nil {
		return err
	}
	infof("nodes: %d real, %d kwok", real, kwok)
	if real < 1 {
		warnf("no real (non-kwok) nodes detected; check the 'type' label convention")
	}
	infof("bring-up complete")
	return nil
}

// nodeInventory returns the count of real vs. simulated (KWOK) nodes.
func (c *clients) nodeInventory(ctx context.Context) (real, kwok int, err error) {
	nodes, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, fmt.Errorf("list nodes: %w", err)
	}
	for _, n := range nodes.Items {
		if n.Labels["type"] == "kwok" {
			kwok++
		} else {
			real++
		}
	}
	return real, kwok, nil
}

// defaultInstallDir points at the sibling phase0/ dir whether harnessctl is run
// from the harness root or from within harnessctl/.
func defaultInstallDir() string {
	for _, cand := range []string{"phase0", "../phase0"} {
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand
		}
	}
	return "phase0"
}

func runScaleNodes(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("nodes scale", flag.ExitOnError)
	cfg := defaultConfig()
	bindResultsFlag(fs, &cfg)
	bindPromFlags(fs, &cfg)
	bindNodeGuardrailFlags(fs, &cfg)
	bindNodeShapeFlags(fs, &cfg)
	count := fs.Int("count", cfg.NodeCount, "target KWOK node count (required, e.g. --count 10000)")
	_ = fs.Parse(args)
	if *count <= 0 {
		return fmt.Errorf("--count is required: pass the target KWOK node count, e.g. --count 10000")
	}
	cfg.NodeCount = *count

	c, err := newClients(cfg)
	if err != nil {
		return err
	}
	rs := newResultSet(cfg.ResultsDir)
	res := checkScaleNodes(ctx, c, cfg)
	rs.add(res)
	_ = rs.write()
	if !res.passed() {
		return fmt.Errorf("%s", res.Message)
	}
	return nil
}

// checkScaleNodes performs P0.2: create nodes, wait Ready, record the ceiling.
func checkScaleNodes(ctx context.Context, c *clients, cfg Config) CheckResult {
	started := time.Now()
	stepf("P0.2: scale simulated nodes to %d", cfg.NodeCount)

	created, skipped, failed := c.scaleNodes(ctx, cfg)
	infof("scale: created=%d skipped=%d failed=%d", created, skipped, failed)

	ready, ok := c.waitNodesReady(ctx, cfg.NodeCount, time.Duration(cfg.NodeReadyTO)*time.Second,
		func() error { return c.restartKwokController(ctx, cfg) })
	elapsed := time.Since(started)

	p99, p99ok := c.promInstantQuery(ctx, cfg, apiserverP99Query)
	util := c.clusterNodeUtil(ctx)
	clusterBreach, clusterDetail := util.breaches(cfg)

	res := CheckResult{
		ID:       "P0.2",
		Name:     "node ceiling",
		Started:  started,
		Finished: time.Now(),
		Metrics: map[string]any{
			"target_nodes":            cfg.NodeCount,
			"ready_nodes":             ready,
			"created":                 created,
			"failed":                  failed,
			"time_to_ready_seconds":   elapsed.Seconds(),
			"apiserver_p99_seconds":   fmtFloat(p99, p99ok),
			"guardrail_p99_seconds":   cfg.MaxAPIServerP99,
			"cluster_cpu_pct":         util.CPUPct,
			"cluster_mem_pct":         util.MemPct,
			"cluster_cpu_used_cores":  util.CPUUsedCores,
			"cluster_mem_used_mi":     util.MemUsedMi,
			"cluster_real_nodes":      util.RealNodes,
			"cluster_cpu_guardrail":   cfg.MaxClusterCPUPct,
			"cluster_mem_guardrail":   cfg.MaxClusterMemPct,
			"cluster_metrics_present": util.OK,
		},
	}
	writeArtifact(cfg.ResultsDir, "p0.2-node-ceiling.json", res.Metrics)
	infof("cluster resources: %s", clusterDetail)

	switch {
	case !ok:
		res.Verdict = "FAIL"
		res.Message = fmt.Sprintf("only %d/%d nodes Ready within %ds — attribute what saturated first (kwok controller vs api server/etcd)",
			ready, cfg.NodeCount, cfg.NodeReadyTO)
	case p99ok && p99 > cfg.MaxAPIServerP99:
		res.Verdict = "FAIL"
		res.Message = fmt.Sprintf("api server p99 %.3fs exceeded guardrail %.3fs at %d nodes — this is the real ceiling",
			p99, cfg.MaxAPIServerP99, ready)
	case clusterBreach:
		res.Verdict = "FAIL"
		res.Message = fmt.Sprintf("cluster resources out of normal bounds at %d nodes (REAL ceiling): %s", ready, clusterDetail)
	default:
		res.Verdict = "PASS"
		res.Message = fmt.Sprintf("ready=%d/%d in %.0fs, apiserver p99=%s s, %s",
			ready, cfg.NodeCount, elapsed.Seconds(), fmtFloat(p99, p99ok), clusterDetail)
	}
	return res
}

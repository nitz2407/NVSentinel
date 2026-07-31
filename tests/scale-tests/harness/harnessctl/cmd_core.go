/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"bufio"
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
func runInstallScript(ctx context.Context, dir, name string) error {
	path := filepath.Join(dir, name)
	infof("running install script: %s", path)
	cmd := exec.CommandContext(ctx, "bash", path)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// component is one P0.1 building block: how to detect whether it is already
// present, and which install script brings it up (or reconciles it to harness
// defaults).
type component struct {
	id     string
	script string
	detect func(ctx context.Context, c *clients, cfg Config) (present bool, detail string)
}

// p01Components lists the P0.1 stack in install order (numeric script prefixes
// sort naturally: 10 → 20 → 25 → 30). NVSentinel and Janitor ship from the same
// chart/script, so they share 30-install-nvsentinel.sh; the runner dedupes.
func p01Components() []component {
	return []component{
		{"kube-prometheus-stack", "10-install-monitoring.sh", detectMonitoring},
		{"metrics-server", "15-install-metrics-server.sh", detectMetricsServer},
		{"kwok", "20-install-kwok.sh", detectKwok},
		{"cert-manager", "25-install-cert-manager.sh", detectCertManager},
		{"nvsentinel", "30-install-nvsentinel.sh", detectNVSentinel},
		{"janitor", "30-install-nvsentinel.sh", detectJanitor},
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

func detectMonitoring(ctx context.Context, c *clients, cfg Config) (bool, string) {
	// With fullnameOverride=prometheus the operator Deployment is
	// "prometheus-operator"; the other names cover a stock (no-override) install.
	return c.deployPresent(ctx, cfg.MonitoringNamespace,
		"prometheus-operator", "prometheus-kube-prometheus-operator", "kube-prometheus-stack-operator")
}

func detectKwok(ctx context.Context, c *clients, cfg Config) (bool, string) {
	return c.deployPresent(ctx, cfg.KWOKNamespace, "kwok-controller")
}

// detectMetricsServer checks for metrics-server (always in kube-system). It
// powers P0.2's real-node CPU/mem guardrail; managed clusters ship it, Kind
// does not.
func detectMetricsServer(ctx context.Context, c *clients, cfg Config) (bool, string) {
	return c.deployPresent(ctx, "kube-system", "metrics-server")
}

func detectCertManager(ctx context.Context, c *clients, cfg Config) (bool, string) {
	return c.deployPresent(ctx, cfg.CertManagerNamespace, "cert-manager-webhook", "cert-manager")
}

func detectNVSentinel(ctx context.Context, c *clients, cfg Config) (bool, string) {
	if ok, d := c.deployPresent(ctx, cfg.NVSNamespace,
		"health-events-analyzer", "fault-quarantine", "node-drainer", "fault-remediation"); ok {
		return true, d
	}
	return c.dsPresent(ctx, cfg.NVSNamespace, "platform-connectors")
}

func detectJanitor(ctx context.Context, c *clients, cfg Config) (bool, string) {
	if ok, d := c.deployPresent(ctx, cfg.NVSNamespace, "janitor"); ok {
		return true, d
	}
	// Platform-managed janitor (e.g. ArgoCD) lives in its own namespace.
	return c.deployPresent(ctx, cfg.JanitorNamespace, "dgxc-janitor-controller-manager")
}

// runBringup is the single P0.1 command. It (1) verifies cluster reachability,
// (2) detects which of the harness components are already present, (3) installs
// whatever is missing, (4) optionally reconciles already-present components to
// harness defaults (prompting unless -yes/-no-override is given), and (5) prints
// the final node inventory. Installs are thin `helm upgrade --install` shell
// scripts — transparent one-liners that Go orchestrates but does not replace.
func runBringup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bringup", flag.ExitOnError)
	dir := fs.String("dir", defaultInstallDir(), "directory holding the 10|15|20|25|30-install-*.sh scripts")
	assumeYes := fs.Bool("yes", false, "reinstall/override already-present components with harness defaults without prompting")
	noOverride := fs.Bool("no-override", false, "never touch already-present components; install only what is missing")
	_ = fs.Parse(args)

	cfg := loadConfig()
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
	}

	// (3)+(4) Decide, per component, whether to run its install script.
	//   missing              -> always install
	//   present + -yes       -> reinstall (override to harness defaults)
	//   present + -no-override -> skip
	//   present, interactive -> prompt
	run := make([]bool, len(comps))
	for i, comp := range comps {
		switch {
		case !present[i]:
			run[i] = true
		case *assumeYes:
			run[i] = true
		case *noOverride:
			run[i] = false
		case stdinIsTTY():
			run[i] = promptYesNo(fmt.Sprintf("%s is already present. Override with harness defaults (%s)?", comp.id, comp.script))
		default:
			warnf("  %s already present; not overriding (non-interactive; use -yes to override)", comp.id)
			run[i] = false
		}
	}

	// Build the ordered, de-duplicated script set (nvsentinel+janitor share 30).
	var scripts []string
	seen := map[string]bool{}
	for i, comp := range comps {
		if run[i] && !seen[comp.script] {
			seen[comp.script] = true
			scripts = append(scripts, comp.script)
		}
	}
	if len(scripts) == 0 {
		infof("nothing to install; all P0.1 components already present")
	} else {
		stepf("P0.1 bring-up: installing %d script(s) from %s", len(scripts), *dir)
		for _, s := range scripts {
			if err := runInstallScript(ctx, *dir, s); err != nil {
				return err
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

// stdinIsTTY reports whether stdin is an interactive terminal, so bringup only
// prompts when a human can actually answer.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// promptYesNo asks a yes/no question on stdin, defaulting to no.
func promptYesNo(question string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N]: ", question)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
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
	fs := flag.NewFlagSet("scale-nodes", flag.ExitOnError)
	cfg := loadConfig()
	count := fs.Int("count", cfg.NodeCount, "target KWOK node count (required, e.g. -count 10000)")
	_ = fs.Parse(args)
	if *count <= 0 {
		return fmt.Errorf("-count is required: pass the target KWOK node count, e.g. -count 10000")
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

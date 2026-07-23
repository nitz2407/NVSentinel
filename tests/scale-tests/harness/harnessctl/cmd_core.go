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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func runPreflight(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("preflight", flag.ExitOnError)
	_ = fs.Parse(args)
	cfg := loadConfig()

	stepf("P0.1 preflight: cluster reachability")
	c, err := newClients(cfg)
	if err != nil {
		return err
	}
	ver, err := c.kube.Discovery().ServerVersion()
	if err != nil {
		return fmt.Errorf("cannot reach cluster: %w", err)
	}
	infof("kubernetes server version: %s", ver.GitVersion)

	nodes, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	real, kwok := 0, 0
	for _, n := range nodes.Items {
		if n.Labels["type"] == "kwok" {
			kwok++
		} else {
			real++
		}
	}
	infof("nodes: %d real, %d kwok", real, kwok)
	if real < 1 {
		warnf("no real (non-kwok) nodes detected; check the 'type' label convention")
	}
	infof("preflight OK")
	return nil
}

// runBringup runs the helm install scripts. This is the one place that stays as
// shell — `helm upgrade --install` is simpler and more transparent as a
// one-liner, and rewriting it in Go buys no robustness.
func runBringup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bringup", flag.ExitOnError)
	dir := fs.String("dir", defaultInstallDir(), "directory holding the 10|20|30-install-*.sh scripts")
	_ = fs.Parse(args)
	stepf("P0.1 bring-up: helm installs from %s", *dir)
	if err := runInstallScripts(ctx, *dir); err != nil {
		return err
	}
	infof("bring-up complete")
	return nil
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
	count := fs.Int("count", 0, "target node count (default: KWOK_NODE_COUNT)")
	_ = fs.Parse(args)
	cfg := loadConfig()
	if *count > 0 {
		cfg.NodeCount = *count
	}

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

	ready, ok := c.waitNodesReady(ctx, cfg.NodeCount, time.Duration(cfg.NodeReadyTO)*time.Second)
	elapsed := time.Since(started)

	p99, p99ok := c.promInstantQuery(ctx, cfg, apiserverP99Query)

	res := CheckResult{
		ID:       "P0.2",
		Name:     "node ceiling",
		Started:  started,
		Finished: time.Now(),
		Metrics: map[string]any{
			"target_nodes":          cfg.NodeCount,
			"ready_nodes":           ready,
			"created":               created,
			"failed":                failed,
			"time_to_ready_seconds": elapsed.Seconds(),
			"apiserver_p99_seconds": fmtFloat(p99, p99ok),
			"guardrail_p99_seconds": cfg.MaxAPIServerP99,
		},
	}
	writeArtifact(cfg.ResultsDir, "p0.2-node-ceiling.json", res.Metrics)

	switch {
	case !ok:
		res.Verdict = "FAIL"
		res.Message = fmt.Sprintf("only %d/%d nodes Ready within %ds — attribute what saturated first (kwok controller vs api server/etcd)",
			ready, cfg.NodeCount, cfg.NodeReadyTO)
	case p99ok && p99 > cfg.MaxAPIServerP99:
		res.Verdict = "FAIL"
		res.Message = fmt.Sprintf("api server p99 %.3fs exceeded guardrail %.3fs at %d nodes — this is the real ceiling",
			p99, cfg.MaxAPIServerP99, ready)
	default:
		res.Verdict = "PASS"
		res.Message = fmt.Sprintf("ready=%d/%d in %.0fs, apiserver p99=%s s",
			ready, cfg.NodeCount, elapsed.Seconds(), fmtFloat(p99, p99ok))
	}
	return res
}

func runTeardownNodes(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("teardown-nodes", flag.ExitOnError)
	_ = fs.Parse(args)
	cfg := loadConfig()
	c, err := newClients(cfg)
	if err != nil {
		return err
	}
	stepf("teardown: deleting kwok nodes")
	n := c.countKwokNodes(ctx)
	infof("deleting %d kwok nodes (cascades fake pods)", n)
	if err := c.teardownNodes(ctx); err != nil {
		return err
	}
	infof("delete issued")
	return nil
}

func runSimReboot(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sim-reboot", flag.ExitOnError)
	node := fs.String("node", "", "node to reboot (required)")
	down := fs.Duration("down", 10*time.Second, "how long the node stays NotReady")
	_ = fs.Parse(args)
	if *node == "" {
		return fmt.Errorf("-node is required")
	}
	cfg := loadConfig()
	c, err := newClients(cfg)
	if err != nil {
		return err
	}
	boot := fmt.Sprintf("%d-%d", time.Now().Unix(), time.Now().Nanosecond())
	infof("simulating reboot of %s (down %s, new bootID %s)", *node, *down, boot)
	return c.simulateReboot(ctx, *node, *down, boot)
}

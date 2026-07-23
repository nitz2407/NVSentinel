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
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func runPhase0(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("phase0", flag.ExitOnError)
	installDir := fs.String("install-dir", "", "if set, run the helm install shell scripts in this dir (10/20/30-install-*.sh) first")
	only := fs.String("only", "", "run a single step: nodes|inject|janitor")
	nodes := fs.Int("nodes", 0, "override target node count")
	_ = fs.Parse(args)

	cfg := loadConfig()
	if *nodes > 0 {
		cfg.NodeCount = *nodes
	}

	c, err := newClients(cfg)
	if err != nil {
		return err
	}
	rs := newResultSet(cfg.ResultsDir)

	if *installDir != "" && *only == "" {
		if err := runInstallScripts(ctx, *installDir); err != nil {
			rs.add(CheckResult{ID: "P0.1", Name: "bring-up", Verdict: "FAIL", Message: err.Error(), Started: time.Now(), Finished: time.Now()})
			_ = rs.write()
			return err
		}
		rs.add(CheckResult{ID: "P0.1", Name: "bring-up", Verdict: "PASS", Message: "helm install scripts completed", Started: time.Now(), Finished: time.Now()})
	}

	switch *only {
	case "nodes":
		rs.add(checkScaleNodes(ctx, c, cfg))
	case "inject":
		rs.add(checkInjectReconcile(ctx, c, cfg))
	case "janitor":
		rs.add(checkJanitor(ctx, c, cfg))
	case "":
		rs.add(checkScaleNodes(ctx, c, cfg))
		rs.add(checkInjectReconcile(ctx, c, cfg))
		rs.add(checkJanitor(ctx, c, cfg))
	default:
		return fmt.Errorf("unknown --only step: %s (expected nodes|inject|janitor)", *only)
	}

	_ = rs.write()
	stepf("Phase 0 summary")
	for _, r := range rs.results {
		infof("%-5s %-20s %s — %s", r.ID, r.Name, r.Verdict, r.Message)
	}
	if rs.anyFailed() {
		return fmt.Errorf("phase 0 has failing checks; the harness is not yet proven")
	}
	infof("Phase 0 proven; proceed to Phase 1.")
	return nil
}

// checkInjectReconcile performs P0.3 end to end by launching the injector and
// reconciler as in-cluster Jobs (same runner node, shared hostPath ledger).
func checkInjectReconcile(ctx context.Context, c *clients, cfg Config) CheckResult {
	started := time.Now()
	stepf("P0.3: inject + reconcile")
	res := CheckResult{ID: "P0.3", Name: "inject + reconcile", Started: started, Metrics: map[string]any{}}

	runner, err := c.firstRealNode(ctx)
	if err != nil {
		res.Verdict, res.Message, res.Finished = "FAIL", err.Error(), time.Now()
		return res
	}
	infof("runner node: %s", runner)
	if err := c.labelNode(ctx, runner, runnerLabelKey, "true"); err != nil {
		res.Verdict, res.Message, res.Finished = "FAIL", "label runner: "+err.Error(), time.Now()
		return res
	}

	conn, err := c.deriveMongoConn(ctx, cfg)
	if err != nil {
		res.Verdict, res.Message, res.Finished = "FAIL", "resolve mongo connection: "+err.Error(), time.Now()
		return res
	}
	if conn.tlsSecret != "" {
		infof("mongo: mTLS/X.509 via secret %s", conn.tlsSecret)
	} else {
		infof("mongo: plain connection (no client-cert secret found)")
	}

	runID := fmt.Sprintf("p03-%d", time.Now().Unix())
	res.Metrics["run_id"] = runID

	// Injector Job.
	injectTO := time.Duration(float64(cfg.EventCount)/max1(cfg.EventRate))*time.Second + 3*time.Minute
	injectArgs := []string{
		"inject",
		"-socket=/var/run/nvsentinel.sock",
		fmt.Sprintf("-node-prefix=%s", cfg.NodePrefix),
		fmt.Sprintf("-nodes=%d", cfg.NodeCount),
		fmt.Sprintf("-count=%d", cfg.EventCount),
		fmt.Sprintf("-rate=%g", cfg.EventRate),
		fmt.Sprintf("-run-id=%s", runID),
		fmt.Sprintf("-run-label=%s", cfg.RunLabel),
		fmt.Sprintf("-id-label=%s", cfg.IDLabel),
		"-ledger=/results/injection-ledger.jsonl",
	}
	if _, err := c.runJob(ctx, cfg, jobSpec{name: "nvs-harness-injector", args: injectArgs, mountSocket: true}, injectTO); err != nil {
		res.Verdict, res.Message, res.Finished = "FAIL", "injector: "+err.Error(), time.Now()
		return res
	}
	infof("injection complete; draining before reconcile")
	sleepCtx(ctx, 30*time.Second)

	// Reconciler Job.
	recArgs := []string{
		"reconcile",
		fmt.Sprintf("-run-id=%s", runID),
		fmt.Sprintf("-db=%s", cfg.MongoDB),
		fmt.Sprintf("-collection=%s", cfg.MongoColl),
		fmt.Sprintf("-field-prefix=%s", cfg.FieldPrefix),
		fmt.Sprintf("-run-label=%s", cfg.RunLabel),
		fmt.Sprintf("-id-label=%s", cfg.IDLabel),
		"-ledger=/results/injection-ledger.jsonl",
		"-report=/results/reconcile-report.json",
		fmt.Sprintf("-max-loss-fraction=%g", cfg.MaxLossFrac),
	}
	if conn.tlsSecret != "" {
		recArgs = append(recArgs, "-tls-cert-dir=/etc/mongo-certs")
		if conn.authMechanism != "" {
			recArgs = append(recArgs, "-auth-mechanism="+conn.authMechanism)
		}
		if conn.authSource != "" {
			recArgs = append(recArgs, "-auth-source="+conn.authSource)
		}
	}
	logs, jobErr := c.runJob(ctx, cfg,
		jobSpec{
			name:        "nvs-harness-reconciler",
			args:        recArgs,
			env:         singleEnv("MONGO_URI", conn.uri),
			mongoSecret: conn.tlsSecret,
		},
		3*time.Minute)

	rep := extractReport(logs)
	if rep != nil {
		res.Metrics["injected"] = rep.Injected
		res.Metrics["missing"] = rep.Missing
		res.Metrics["loss_fraction"] = rep.LossFraction
		writeArtifact(cfg.ResultsDir, "p0.3-reconcile-report.json", rep)
	}
	res.Finished = time.Now()
	switch {
	case rep != nil && rep.Verdict == "PASS":
		res.Verdict = "PASS"
		res.Message = fmt.Sprintf("injected=%d missing=%d loss=%.4f", rep.Injected, rep.Missing, rep.LossFraction)
	case rep != nil:
		res.Verdict = "FAIL"
		res.Message = fmt.Sprintf("injected=%d missing=%d loss=%.4f exceeds max %.4f", rep.Injected, rep.Missing, rep.LossFraction, rep.MaxLoss)
	default:
		res.Verdict = "FAIL"
		if jobErr != nil {
			res.Message = "reconciler: " + jobErr.Error()
		} else {
			res.Message = "reconciler produced no parseable report"
		}
	}
	return res
}

func runInstallScripts(ctx context.Context, dir string) error {
	for _, s := range []string{"10-install-monitoring.sh", "20-install-kwok.sh", "30-install-nvsentinel.sh"} {
		path := filepath.Join(dir, s)
		infof("running install script: %s", path)
		cmd := exec.CommandContext(ctx, "bash", path)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

// extractReport pulls the ReconcileReport JSON object out of merged pod logs.
func extractReport(logs string) *ReconcileReport {
	start := strings.Index(logs, "{")
	end := strings.LastIndex(logs, "}")
	if start < 0 || end <= start {
		return nil
	}
	var rep ReconcileReport
	if err := json.Unmarshal([]byte(logs[start:end+1]), &rep); err != nil {
		return nil
	}
	return &rep
}

func max1(f float64) float64 {
	if f < 1 {
		return 1
	}
	return f
}

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
	"strings"
	"time"
)

func runJanitorCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("janitor check", flag.ExitOnError)
	cfg := defaultConfig()
	bindResultsFlag(fs, &cfg)
	fs.IntVar(&cfg.ActionTimeout, "action-timeout", cfg.ActionTimeout, "seconds to wait for a janitor action (P0.4)")
	fs.IntVar(&cfg.JobCompleteDelay, "job-complete-delay", cfg.JobCompleteDelay, "seconds before KWOK marks a Job complete")
	_ = fs.Parse(args)
	c, err := newClients(cfg)
	if err != nil {
		return err
	}
	rs := newResultSet(cfg.ResultsDir)
	res := checkJanitor(ctx, c, cfg)
	rs.add(res)
	_ = rs.write()
	if !res.passed() {
		return fmt.Errorf("%s", res.Message)
	}
	return nil
}

// checkJanitor performs P0.4: RebootNode Job completes + node cycles bootID;
// GPUReset Job completes (phase Succeeded).
func checkJanitor(ctx context.Context, c *clients, cfg Config) CheckResult {
	started := time.Now()
	stepf("P0.4: janitor reboot + GPU reset on a KWOK node")
	res := CheckResult{ID: "P0.4", Name: "janitor action path", Started: started, Metrics: map[string]any{}}

	node, err := c.firstKwokNode(ctx)
	if err != nil {
		res.Verdict, res.Message, res.Finished = "FAIL", err.Error(), time.Now()
		return res
	}
	infof("target kwok node: %s", node)
	res.Metrics["node"] = node

	timeout := time.Duration(cfg.ActionTimeout) * time.Second
	var notes []string

	// ---- RebootNode ----
	rebootName := "p04-reboot-" + node
	rebootOK := false
	if err := c.applyRebootNode(ctx, rebootName, node); err != nil {
		notes = append(notes, "reboot create: "+err.Error())
	} else {
		// Let the janitor create the reboot Job + KWOK complete it, then cycle the node.
		sleepCtx(ctx, time.Duration(cfg.JobCompleteDelay+10)*time.Second)
		bootBefore := c.nodeBootID(ctx, node)
		boot := fmt.Sprintf("%d-%d", time.Now().Unix(), time.Now().Nanosecond())
		if err := c.simulateReboot(ctx, node, 10*time.Second, boot); err != nil {
			notes = append(notes, "sim reboot: "+err.Error())
		}
		bootAfter := c.nodeBootID(ctx, node)
		res.Metrics["boot_before"], res.Metrics["boot_after"] = bootBefore, bootAfter

		if waitCR(ctx, timeout, func() bool {
			s, _ := c.conditionStatus(ctx, rebootGVR, rebootName, "NodeReady")
			return s == "True"
		}) {
			if bootAfter != "" && bootAfter != bootBefore {
				rebootOK = true
			} else {
				notes = append(notes, fmt.Sprintf("NodeReady set but bootID unchanged (%s)", bootBefore))
			}
		} else {
			notes = append(notes, fmt.Sprintf("NodeReady!=True within %s (check janitor reboot provider wiring)", timeout))
		}
	}
	res.Metrics["reboot"] = passStr(rebootOK)
	infof("RebootNode result: %s", passStr(rebootOK))

	// ---- GPUReset ----
	resetName := "p04-gpureset-" + node
	resetOK := false
	if err := c.applyGPUReset(ctx, resetName, node); err != nil {
		notes = append(notes, "gpureset create: "+err.Error())
	} else if waitCR(ctx, timeout, func() bool {
		ph, _ := c.phase(ctx, gpuresetGVR, resetName)
		return ph == "Succeeded"
	}) {
		resetOK = true
	} else {
		ph, _ := c.phase(ctx, gpuresetGVR, resetName)
		notes = append(notes, fmt.Sprintf("gpureset phase=%q != Succeeded within %s", ph, timeout))
	}
	res.Metrics["gpureset"] = passStr(resetOK)
	infof("GPUReset result: %s", passStr(resetOK))

	res.Finished = time.Now()
	if rebootOK && resetOK {
		res.Verdict = "PASS"
		res.Message = fmt.Sprintf("reboot=PASS gpureset=PASS on %s", node)
	} else {
		res.Verdict = "FAIL"
		res.Message = fmt.Sprintf("reboot=%s gpureset=%s; %s", passStr(rebootOK), passStr(resetOK), strings.Join(notes, "; "))
	}
	writeArtifact(cfg.ResultsDir, "p0.4-janitor-actions.json", res.Metrics)
	return res
}

func passStr(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func waitCR(ctx context.Context, timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

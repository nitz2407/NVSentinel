//go:build !injector

/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"context"
	"fmt"
	"strings"
)

// mongoDistOptions parameterizes the distributed dispatch (one `inject` fires one
// resident injector to do the whole insert; MongoDB is the shared bottleneck, so
// fan-out across nodes buys nothing and only multiplies connection pressure).
type mongoDistOptions struct {
	total          int
	workers        int
	batch          int
	nodeCount      int
	nodeOffset     int
	runID          string
	coldstartRatio float64
}

// injectMongoAcrossPool runs the direct-Mongo primitive inside ONE resident
// injector (in-cluster mTLS reachability). MongoDB is the shared write bottleneck,
// so a single high-worker injector is the faithful stressor; fanning out would
// only multiply mTLS connection pressure without raising the achievable insert rate.
func (c *clients) injectMongoAcrossPool(ctx context.Context, cfg Config, o mongoDistOptions) error {
	stepf("direct-mongo inject (bypassing the platform-connector)")
	geo, err := c.resolvePoolGeometry(ctx, cfg)
	if err != nil {
		return err
	}
	pod := firstInjector(geo)
	if pod == "" {
		return fmt.Errorf("no resident injector available (deploy the connector pool first)")
	}
	// Direct-mongo writes no shard ledger; drop any stale gRPC led-*.jsonl so a
	// later reconcile can never misread a previous run's ledger for this run.
	c.clearPoolLedgers(ctx, cfg.NVSNamespace, geo)
	conn, err := c.deriveMongoConn(ctx, cfg)
	if err != nil {
		return err
	}

	nodes := o.nodeCount
	if nodes <= 0 {
		nodes = c.countKwokNodesOrZero(ctx)
	}
	if nodes <= 0 {
		nodes = 1
	}
	total := o.total
	if total <= 0 {
		total = nodes
	}

	args := []string{
		"inject", "-direct-mongo",
		"-run-id=" + o.runID,
		"-run-label=" + cfg.RunLabel,
		"-id-label=" + cfg.IDLabel,
		"-db=" + cfg.MongoDB,
		"-collection=" + cfg.MongoColl,
		"-node-prefix=" + cfg.NodePrefix,
		fmt.Sprintf("-node-offset=%d", o.nodeOffset),
		fmt.Sprintf("-nodes=%d", nodes),
		fmt.Sprintf("-count=%d", total),
		fmt.Sprintf("-mongo-workers=%d", o.workers),
		fmt.Sprintf("-mongo-batch=%d", o.batch),
		"-fatal-event=" + defStr(cfg.FatalEvent, fatalEventNodeReboot),
		"-pattern=" + defStr(cfg.Pattern, patternFleetStorm),
		"-processing-strategy=" + defStr(cfg.ProcessingStrategy, procStrategyDefault),
		fmt.Sprintf("-fatal-fraction=%g", cfg.FatalFraction),
	}
	if conn.tlsSecret != "" {
		args = append(args, "-tls-cert-dir=/etc/mongo-certs")
		if conn.authMechanism != "" {
			args = append(args, "-auth-mechanism="+conn.authMechanism)
		}
		if conn.authSource != "" {
			args = append(args, "-auth-source="+conn.authSource)
		}
	}
	if o.coldstartRatio > 0 {
		args = append(args, fmt.Sprintf("-coldstart-ratio=%g", o.coldstartRatio))
	}

	if o.coldstartRatio > 0 {
		infof("direct-mongo cold-start seed: injector=%s nodes=%d count=%d ratio=%g workers=%d batch=%d run-id=%s",
			pod, nodes, total, o.coldstartRatio, o.workers, o.batch, o.runID)
	} else {
		infof("direct-mongo inject: injector=%s nodes=%d count=%d workers=%d batch=%d pattern=%s proc-strategy=%s run-id=%s",
			pod, nodes, total, o.workers, o.batch, defStr(cfg.Pattern, patternFleetStorm), defStr(cfg.ProcessingStrategy, procStrategyDefault), o.runID)
	}

	out, err := c.execShEnv(ctx, cfg.NVSNamespace, pod, map[string]string{"MONGO_URI": conn.uri}, shellQuoteRun(cfg.injectorBinPath(), args))
	if err != nil {
		return fmt.Errorf("direct-mongo inject on %s: %w\n%s", pod, err, out)
	}
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(ln, "done: sent=") {
			infof("  %s", strings.TrimSpace(ln))
		}
	}
	// Record how this run was injected so a standalone `reconcile -run-id` (which
	// sees none of the inject flags) accounts it by run-label count instead of the
	// per-ID shard-ledger path the mongo mechanism never populates.
	writeRunManifest(cfg, o.runID, mechanismMongo, total)
	// Emit the run id last so callers can capture it for `reconcile -run-id`.
	fmt.Println(o.runID)
	return nil
}

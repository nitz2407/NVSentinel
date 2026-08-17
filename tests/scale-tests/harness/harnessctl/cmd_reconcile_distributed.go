//go:build !injector

/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// runReconcileDistributed accounts every injected event for a run without a
// centralized ledger: it fans a shard-scoped per-ID + NodeName reconcile out to
// every resident injector (each reads ITS OWN shard ledgers against a datastore
// query scoped to just those IDs, inheriting in-cluster MongoDB reachability +
// mTLS), then aggregates the summaries into a fleet-wide verdict. This is the
// operator counterpart to `inject` firing all injectors.
func runReconcileDistributed(ctx context.Context, cfg Config, runID string) error {
	c, err := newClients(cfg)
	if err != nil {
		return err
	}
	geo, err := c.resolvePoolGeometry(ctx, cfg)
	if err != nil {
		return err
	}
	conn, err := c.deriveMongoConn(ctx, cfg)
	if err != nil {
		return err
	}

	// Watch the SUT (cordoning, remediation CRs, control-plane restarts, MongoDB
	// quorum/CPU saturation) for the whole drain+reconcile so failures like the
	// connector-pool MongoDB collapse are recorded, not silently swallowed.
	stopMonitor := c.startReconcileMonitor(ctx, cfg, runID)
	defer stopMonitor()

	// Wait for the async write plane to drain instead of a fixed sleep: poll the
	// datastore's stored count for the run until it stops growing (or reaches the
	// expected total), then reconcile.
	if err := c.waitStoredStable(ctx, cfg, geo, conn, runID); err != nil {
		warnf("drain wait: %v (reconciling anyway)", err)
	}

	// Pick the accounting mode. The direct-`mongo` inject bypasses the connector
	// pool and writes NO per-connector shard ledger, so the ledger-based per-ID
	// reconcile has nothing to diff (and would otherwise read leftover gRPC
	// ledgers). Its inject leaves a run manifest recording mechanism + expected
	// count; when present for a mongo run, reconcile by counting stored docs for
	// the run label against that expected total. Absent manifest => ledger-based
	// per-ID + NodeName reconcile, the gRPC pool default (older runs unaffected).
	var rep *ReconcileReport
	if m, ok := readRunManifest(cfg, runID); ok && m.Mechanism == mechanismMongo && m.Expected > 0 {
		infof("run %s was injected via mechanism=mongo (expected=%d); reconciling by run-label count", runID, m.Expected)
		rep, err = c.reconcileByCountPool(ctx, cfg, geo, conn, runID, m.Expected)
	} else {
		rep, err = c.reconcilePerNode(ctx, cfg, geo, conn, runID)
	}
	if err != nil {
		return fmt.Errorf("distributed reconcile: %w", err)
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(b))
	writeArtifact(cfg.ResultsDir, "reconcile-report.json", rep)
	infof("[P0.3 reconcile] run-id=%s injected=%d accounted=%d missing=%d loss=%.4f node=%d/%d verdict=%s",
		runID, rep.Injected, rep.Accounted, rep.Missing, rep.LossFraction, rep.NodeMatched, rep.NodeChecked, rep.Verdict)
	if rep.NodeAttrNote != "" {
		infof("%s", rep.NodeAttrNote)
	}
	if rep.Verdict != "PASS" {
		return fmt.Errorf("reconcile FAIL: missing=%d loss=%.4f node_mismatched=%d/%d",
			rep.Missing, rep.LossFraction, rep.NodeMismatched, rep.NodeChecked)
	}
	return nil
}


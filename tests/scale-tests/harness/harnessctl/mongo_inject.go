/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

// Direct-MongoDB event injection: write HealthEvent documents straight into the
// datastore, byte-compatible with the platform-connector's write
// (model.HealthEventWithStatus), BYPASSING the connector's gRPC ingress. This is
// the `-mechanism mongo` path — for storage / change-stream stress at rates the
// connector cannot reach (STORE_ONLY floods, remediation floods) and for
// cold-start pre-seeding (a large haystack a restarted consumer must scan). It
// runs inside ONE resident injector (in-cluster mTLS/X.509 reachability, reusing
// the same connection plumbing as reconcile) with a bounded InsertMany worker
// pool. Distributed dispatch (injectMongoAcrossPool) fires that primitive over
// `kubectl exec`, mirroring the gRPC pool path.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// mongoInjectOptions parameterizes the primitive direct-Mongo insert (one process
// inside one resident injector).
type mongoInjectOptions struct {
	nodes        []string
	total        int
	workers      int
	batch        int
	pattern      string
	procStrategy string
	fatalFrac    float64
	fatalAgent   string
	fatalEvent   string
	runID        string
	runLabel     string
	idLabel      string
	ledgerPath   string
	// coldstartRatio > 0 selects cold-start seed generation: the first `ratio`
	// fraction of docs are remediation-ready needles, the rest STORE_ONLY noise.
	coldstartRatio float64
}

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

// mongoErrOnce logs only the first insert error so a sustained failure under a
// flood does not drown the logs.
var mongoErrOnce sync.Once

// genColdStartEvent builds one document for cold-start replay seeding. The first
// `ratio` fraction of the run (by index) are remediation-ready fatal events with
// an UNSPECIFIED processingStrategy — the "needles" a cold-started consumer
// (node-drainer / fault-remediation) must find and act on. The remainder are
// STORE_ONLY noise the consumer skips — the "haystack" that inflates the initial
// pre-change-stream scan. Determinism-by-index keeps the needle set stable across
// seed re-runs.
func genColdStartEvent(node, id, runID, runLabel, idLabel string, i, total int, ratio float64, fatalAgent, fatalEvent string) (*pb.HealthEvent, string) {
	meta := map[string]string{runLabel: runID, idLabel: id}
	needles := int(float64(total) * ratio)
	if i < needles {
		return buildFatalEvent(node, id, meta, fatalAgent, fatalEvent), "needle"
	}
	evt, _ := buildHealthyEvent(node, id, meta)
	evt.ProcessingStrategy = pb.ProcessingStrategy_STORE_ONLY
	return evt, "noise"
}

// runInjectMongoPrimitive inserts o.total HealthEvent documents into MongoDB with
// a bounded InsertMany worker pool. It is invoked inside a resident injector via
// `inject -direct-mongo`, reusing reconcile's mongoClientOptions so it talks to a
// plain, TLS, or mTLS/X.509 store unchanged.
func runInjectMongoPrimitive(ctx context.Context, p reconcileParams, o mongoInjectOptions) error {
	if len(o.nodes) == 0 {
		return fmt.Errorf("no node names to attribute events to")
	}
	if o.total <= 0 {
		return fmt.Errorf("count must be > 0")
	}
	if o.workers < 1 {
		o.workers = 1
	}
	if o.batch < 1 {
		o.batch = 1
	}

	if o.coldstartRatio > 0 {
		needles := int(float64(o.total) * o.coldstartRatio)
		infof("mongo injector (cold-start seed) run-id=%s nodes=%d count=%d needles=%d noise=%d ratio=%g workers=%d batch=%d fatal-event=%s",
			o.runID, len(o.nodes), o.total, needles, o.total-needles, o.coldstartRatio, o.workers, o.batch, o.fatalEvent)
	} else {
		infof("mongo injector run-id=%s nodes=%d count=%d workers=%d batch=%d pattern=%s fatal-event=%s proc-strategy=%s",
			o.runID, len(o.nodes), o.total, o.workers, o.batch, o.pattern, o.fatalEvent, normalizeProcStrategyName(o.procStrategy))
	}

	opts, err := mongoClientOptions(p)
	if err != nil {
		return fmt.Errorf("mongo options: %w", err)
	}
	connCtx, cancel := context.WithTimeout(ctx, p.timeout)
	client, err := mongo.Connect(connCtx, opts)
	cancel()
	if err != nil {
		return fmt.Errorf("mongo connect: %w", err)
	}
	defer client.Disconnect(context.Background())
	coll := client.Database(p.db).Collection(p.coll)

	type span struct{ start, end int }
	jobs := make(chan span)
	var acked, failed int64
	start := time.Now()

	var wg sync.WaitGroup
	for w := 0; w < o.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range jobs {
				docs := make([]interface{}, 0, b.end-b.start)
				for i := b.start; i < b.end; i++ {
					node := o.nodes[i%len(o.nodes)]
					id := fmt.Sprintf("%s-%08d-%s", o.runID, i, randHex(4))
					var evt *pb.HealthEvent
					if o.coldstartRatio > 0 {
						evt, _ = genColdStartEvent(node, id, o.runID, o.runLabel, o.idLabel, i, o.total, o.coldstartRatio, o.fatalAgent, o.fatalEvent)
					} else {
						evt, _ = genEvent(node, id, o.runID, o.runLabel, o.idLabel, i, o.pattern, o.fatalFrac, o.fatalAgent, o.fatalEvent, o.procStrategy)
					}
					docs = append(docs, model.HealthEventWithStatus{
						CreatedAt:         time.Now().UTC(),
						HealthEvent:       evt,
						HealthEventStatus: &pb.HealthEventStatus{},
					})
				}
				ictx, ic := context.WithTimeout(ctx, p.timeout)
				_, err := coll.InsertMany(ictx, docs, options.InsertMany().SetOrdered(false))
				ic()
				if err != nil {
					mongoErrOnce.Do(func() { errorf("first insert error (logged once): %v", err) })
					atomic.AddInt64(&failed, int64(len(docs)))
					continue
				}
				atomic.AddInt64(&acked, int64(len(docs)))
			}
		}()
	}

	for s := 0; s < o.total; s += o.batch {
		e := s + o.batch
		if e > o.total {
			e = o.total
		}
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return ctx.Err()
		case jobs <- span{s, e}:
		}
		if done := int(atomic.LoadInt64(&acked) + atomic.LoadInt64(&failed)); done > 0 && done%10000 < o.batch {
			infof("progress: ~%d/%d (acked=%d failed=%d, %.0f/s)", done, o.total, atomic.LoadInt64(&acked), atomic.LoadInt64(&failed), float64(done)/time.Since(start).Seconds())
		}
	}
	close(jobs)
	wg.Wait()

	infof("done: sent=%d acked=%d failed=%d run-id=%s", o.total, acked, failed, o.runID)
	fmt.Println(o.runID)
	if failed > 0 {
		return fmt.Errorf("%d documents failed to insert", failed)
	}
	return nil
}

// injectMongoAcrossPool runs the direct-Mongo primitive inside ONE resident
// injector (in-cluster mTLS reachability), staging the binary first. MongoDB is
// the shared write bottleneck, so a single high-worker injector is the faithful
// stressor; fanning out would only multiply mTLS connection pressure without
// raising the achievable insert rate.
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
	localBin, err := resolveLocalBinary(cfg.HarnessBin)
	if err != nil {
		return err
	}
	wantSum, err := localBinarySum(localBin)
	if err != nil {
		return err
	}
	if err := c.stageBinaryToInjector(ctx, cfg.NVSNamespace, pod, localBin, wantSum, false); err != nil {
		return err
	}
	conn, err := c.deriveMongoConn(ctx, cfg)
	if err != nil {
		return err
	}

	nodes := o.nodeCount
	if nodes <= 0 {
		nodes = c.countKwokNodes(ctx)
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

	out, err := c.execShEnv(ctx, cfg.NVSNamespace, pod, map[string]string{"MONGO_URI": conn.uri}, shellQuoteRun(args))
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

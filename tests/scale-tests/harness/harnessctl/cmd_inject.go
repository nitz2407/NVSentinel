/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	mrand "math/rand"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// Fatal-event kinds (selectable via -fatal-event / HARNESS_FATAL_EVENT): which
// remediation the fatal events drive end to end.
const (
	fatalEventNodeReboot = "node-reboot" // RESTART_BM     => node-drainer => RebootNode CR
	fatalEventGPUReset   = "gpu-reset"   // COMPONENT_RESET => node-drainer => GPUReset CR
)

// Generation patterns (selectable via -pattern / HARNESS_INJECT_PATTERN): the
// shape of the fatal/healthy mix over the injected node set.
const (
	patternFleetStorm      = "fleet-storm"       // independent per-event fatal draw at fatal-fraction across the fleet
	patternFlappy          = "flappy"            // alternate fatal/healthy so nodes repeatedly flap state
	patternSingleNodeBurst = "single-node-burst" // every event fatal (a burst concentrated on the caller's node set)
)

// HealthEvent processingStrategy overrides (selectable via -processing-strategy /
// HARNESS_PROCESSING_STRATEGY). default leaves the field UNSPECIFIED so the
// connector/analyzer decides; the others force a downstream handling path.
const (
	procStrategyDefault            = "default"
	procStrategyStoreOnly          = "store-only"
	procStrategyStoreAndAnalyse    = "store-and-analyse"
	procStrategyExecuteRemediation = "execute-remediation"
)

// Injection mechanisms (selectable via -mechanism / HARNESS_INJECT_MECHANISM):
//   - grpc: send through the platform-connector's gRPC UDS ingress (the faithful
//     production path; exercises the connector's dedup/transform/APF/write plus the
//     downstream FQ->ND->FR->janitor pipeline). Requires a deployed connector pool.
//   - mongo: insert HealthEvent documents straight into MongoDB, byte-compatible with
//     the connector's write (model.HealthEventWithStatus), BYPASSING the connector.
//     For storage/change-stream stress at rates the connector can't reach (STORE_ONLY
//     flood, remediation flood) and cold-start pre-seeding. Runs inside one resident
//     injector (in-cluster mTLS reachability) with a worker pool.
const (
	mechanismGRPC  = "grpc"
	mechanismMongo = "mongo"
)

func normalizeFatalEvent(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), fatalEventGPUReset) {
		return fatalEventGPUReset
	}
	return fatalEventNodeReboot
}

func normalizePattern(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case patternFlappy:
		return patternFlappy
	case patternSingleNodeBurst:
		return patternSingleNodeBurst
	default:
		return patternFleetStorm
	}
}

func normalizeProcStrategyName(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case procStrategyStoreOnly:
		return procStrategyStoreOnly
	case procStrategyStoreAndAnalyse:
		return procStrategyStoreAndAnalyse
	case procStrategyExecuteRemediation:
		return procStrategyExecuteRemediation
	default:
		return procStrategyDefault
	}
}

func normalizeMechanism(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), mechanismMongo) {
		return mechanismMongo
	}
	return mechanismGRPC
}

type ledgerEntry struct {
	ID       string `json:"id"`
	Node     string `json:"node"`
	Type     string `json:"type"`
	SentUnix int64  `json:"sent_unix_ms"`
	Acked    bool   `json:"acked"`
}

// runInject is the P0.3 injector. It has two modes:
//
//   - Distributed (default, no -socket): a single invocation fires every resident
//     injector deployed by connector-pool, each driving its local connector
//     shards in parallel — "one inject fires all injectors". This is the
//     operator-facing command.
//   - Primitive (-socket set): drive one connector's Unix socket. This is what
//     each resident injector runs internally, one process per node.
//
// Both attribute events to KWOK node names and stamp a correlation id into
// HealthEvent.id + metadata that the reconciler accounts.
func runInject(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("events inject", flag.ExitOnError)
	cfg := defaultConfig()
	bindNvsNamespaceFlag(fs, &cfg)
	bindResultsFlag(fs, &cfg)
	bindMongoFlags(fs, &cfg)
	bindInjectorFlags(fs, &cfg)
	socket := fs.String("socket", "", "connector Unix socket; empty => distributed mode: fan out across every resident injector deployed by connector-pool")
	nodePrefix := fs.String("node-prefix", "kwok-gpu", "simulated node name prefix")
	nodeCount := fs.Int("nodes", 50000, "number of simulated node names to spread events across")
	nodeOffset := fs.Int("node-offset", 0, "start index for generated node names (P0.5 pool sharding: pod N owns [offset, offset+nodes))")
	nodesFrom := fs.String("nodes-from", "", "optional file of node names (one per line)")
	total := fs.Int("count", 10000, "total events to inject")
	rate := fs.Float64("rate", 500, "target events/sec")
	runID := fs.String("run-id", "", "correlation run id (default: random)")
	runLabel := fs.String("run-label", "nvs_harness_run", "metadata key stamped with the run id")
	idLabel := fs.String("id-label", "nvs_harness_id", "metadata key stamped with the per-event id")
	ledgerPath := fs.String("ledger", "/results/injection-ledger.jsonl", "path to write the injection ledger (JSONL)")
	fatalFrac := fs.Float64("fatal-fraction", 0.08, "fraction of fatal events (fleet-storm pattern)")
	fatalAgent := fs.String("fatal-agent", "gpu-health-monitor", "agent for fatal events (gpu-health-monitor => FQM cordons)")
	fatalEvent := fs.String("fatal-event", fatalEventNodeReboot, "fatal event to inject: node-reboot (RESTART_BM => RebootNode) or gpu-reset (COMPONENT_RESET => GPUReset)")
	pattern := fs.String("pattern", patternFleetStorm, "generation pattern: fleet-storm | flappy | single-node-burst")
	procStrategy := fs.String("processing-strategy", procStrategyDefault, "HealthEvent processingStrategy: default | store-only | store-and-analyse | execute-remediation")
	mechanism := fs.String("mechanism", mechanismGRPC, "injection mechanism: grpc (through platform-connector) | mongo (direct MongoDB insert, bypasses connector)")
	directMongo := fs.Bool("direct-mongo", false, "run the primitive direct-MongoDB insert inline (set automatically inside an injector); otherwise mongo mechanism orchestrates one injector")
	mWorkers := fs.Int("mongo-workers", 50, "direct-mongo: concurrent InsertMany workers")
	mBatch := fs.Int("mongo-batch", 500, "direct-mongo: InsertMany batch size")
	coldstartRatio := fs.Float64("coldstart-ratio", 0, "direct-mongo: cold-start seed mix — fraction of docs that are remediation-ready needles (rest are STORE_ONLY noise); 0 disables (used by the `coldstart` command)")
	// Direct-mongo connection knobs (shared defaults with reconcile).
	// -uri default falls back to the MONGO_URI env var. This is NOT user config:
	// the distributed orchestrator discovers the mTLS URI and injects it into the
	// resident-injector pod via env (execShEnv) so credentials never appear on the
	// command line (argv is visible in `ps`/logs). Empty env => local default.
	mURI := fs.String("uri", internalMongoURIDefault(), "direct-mongo: MongoDB URI (defaults to $MONGO_URI when set by the in-cluster orchestrator)")
	mDB := fs.String("db", "HealthEventsDatabase", "direct-mongo: database")
	mColl := fs.String("collection", "HealthEvents", "direct-mongo: collection")
	mTLSDir := fs.String("tls-cert-dir", "", "direct-mongo: dir with ca.crt (+ tls.crt/tls.key for mTLS)")
	mTLSInsecure := fs.Bool("tls-insecure", false, "direct-mongo: skip TLS server verification")
	mAuthMech := fs.String("auth-mechanism", "", "direct-mongo: auth mechanism, e.g. MONGODB-X509")
	mAuthSrc := fs.String("auth-source", "", "direct-mongo: auth source db, e.g. $external")
	mTimeout := fs.Duration("mongo-timeout", 60*time.Second, "direct-mongo: MongoDB op timeout")
	_ = fs.Parse(args)

	fe := normalizeFatalEvent(*fatalEvent)
	pat := normalizePattern(*pattern)
	ps := *procStrategy

	if *runID == "" {
		*runID = fmt.Sprintf("run-%d-%s", time.Now().Unix(), randHex(4))
	}

	// Primitive direct-MongoDB insert (checked before distributed dispatch so it
	// does not recurse — mirrors reconcile's -direct). Set automatically inside an
	// injector by the mongo-mechanism / coldstart orchestration.
	if *directMongo {
		nodes := buildNodeNames(*nodesFrom, *nodePrefix, *nodeOffset, *nodeCount)
		return runInjectMongoPrimitive(ctx,
			reconcileParams{uri: *mURI, db: *mDB, coll: *mColl, timeout: *mTimeout,
				tlsCertDir: *mTLSDir, tlsInsecure: *mTLSInsecure, authMech: *mAuthMech, authSource: *mAuthSrc},
			mongoInjectOptions{nodes: nodes, total: *total, workers: *mWorkers, batch: *mBatch,
				pattern: pat, procStrategy: ps, fatalFrac: *fatalFrac, fatalAgent: *fatalAgent, fatalEvent: fe,
				runID: *runID, runLabel: *runLabel, idLabel: *idLabel, ledgerPath: *ledgerPath,
				coldstartRatio: *coldstartRatio})
	}

	// Distributed mode: one command fires every resident injector in the pool. The
	// mechanism (grpc through the connectors vs mongo direct insert) comes from the
	// -mechanism flag when set to mongo, else from config (HARNESS_INJECT_MECHANISM).
	if *socket == "" {
		// CLI flags fully determine this run (no env).
		cfg.FatalEvent, cfg.Pattern, cfg.ProcessingStrategy = fe, pat, ps
		c, err := newClients(cfg)
		if err != nil {
			return err
		}
		mech := normalizeMechanism(cfg.Mechanism)
		if normalizeMechanism(*mechanism) == mechanismMongo {
			mech = mechanismMongo // explicit CLI override
		}
		if mech == mechanismMongo {
			return c.injectMongoAcrossPool(ctx, cfg, mongoDistOptions{
				total: *total, workers: *mWorkers, batch: *mBatch,
				nodeCount: *nodeCount, nodeOffset: *nodeOffset, runID: *runID,
				coldstartRatio: *coldstartRatio,
			})
		}
		return c.injectAcrossPool(ctx, cfg, *rate, *runID)
	}

	infof("injector run-id=%s socket=%s nodes=%d count=%d rate=%.1f/s pattern=%s fatal-event=%s proc-strategy=%s",
		*runID, *socket, *nodeCount, *total, *rate, pat, fe, normalizeProcStrategyName(ps))

	nodes := buildNodeNames(*nodesFrom, *nodePrefix, *nodeOffset, *nodeCount)
	if len(nodes) == 0 {
		return fmt.Errorf("no node names to attribute events to")
	}

	ledger, err := os.Create(*ledgerPath)
	if err != nil {
		return fmt.Errorf("create ledger %s: %w", *ledgerPath, err)
	}
	defer ledger.Close()
	lw := bufio.NewWriter(ledger)
	defer lw.Flush()

	conn, err := grpc.NewClient("unix://"+*socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("connect %s: %w", *socket, err)
	}
	defer conn.Close()
	client := pb.NewPlatformConnectorClient(conn)

	interval := time.Duration(float64(time.Second) / *rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	acked, failed := 0, 0
	start := time.Now()
	for i := 0; i < *total; i++ {
		select {
		case <-ctx.Done():
			warnf("interrupted at %d/%d", i, *total)
			lw.Flush()
			return ctx.Err()
		case <-ticker.C:
		}
		node := nodes[i%len(nodes)]
		id := fmt.Sprintf("%s-%08d-%s", *runID, i, randHex(4))
		evt, kind := genEvent(node, id, *runID, *runLabel, *idLabel, i, pat, *fatalFrac, *fatalAgent, fe, ps)
		ok := sendEvent(ctx, client, evt)
		if ok {
			acked++
		} else {
			failed++
		}
		writeLedger(lw, ledgerEntry{ID: id, Node: node, Type: kind, SentUnix: time.Now().UnixMilli(), Acked: ok})
		if (i+1)%1000 == 0 {
			infof("progress: %d/%d (acked=%d failed=%d, %.0f/s)", i+1, *total, acked, failed, float64(i+1)/time.Since(start).Seconds())
		}
	}
	lw.Flush()
	infof("done: sent=%d acked=%d failed=%d run-id=%s", *total, acked, failed, *runID)
	fmt.Println(*runID)
	if failed > 0 {
		return fmt.Errorf("%d events failed to ack", failed)
	}
	return nil
}

func buildNodeNames(from, prefix string, offset, count int) []string {
	if from != "" {
		f, err := os.Open(from)
		if err != nil {
			errorf("open -nodes-from %s: %v", from, err)
			return nil
		}
		defer f.Close()
		var out []string
		s := bufio.NewScanner(f)
		for s.Scan() {
			if line := s.Text(); line != "" {
				out = append(out, line)
			}
		}
		return out
	}
	if offset < 0 {
		offset = 0
	}
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, fmt.Sprintf("%s-%d", prefix, offset+i))
	}
	return out
}

// genEvent builds one HealthEvent for injection index i under the given pattern.
// The pattern decides whether this event is fatal; buildFatalEvent/buildHealthyEvent
// build the body; applyProcStrategy stamps the optional processingStrategy override.
//
//	fleet-storm       independent per-event fatal draw at fatalFrac (a realistic mix)
//	flappy            alternate fatal/healthy by index so nodes repeatedly flap
//	single-node-burst every event fatal (the caller narrows the node set to a few)
func genEvent(node, id, runID, runLabel, idLabel string, i int, pattern string, fatalFrac float64, fatalAgent, fatalEvent, procStrategy string) (*pb.HealthEvent, string) {
	meta := map[string]string{runLabel: runID, idLabel: id}
	fatal := false
	switch normalizePattern(pattern) {
	case patternSingleNodeBurst:
		fatal = true
	case patternFlappy:
		fatal = i%2 == 0
	default: // fleet-storm
		fatal = mrand.Float64() < fatalFrac
	}
	var evt *pb.HealthEvent
	var kind string
	if fatal {
		evt, kind = buildFatalEvent(node, id, meta, fatalAgent, fatalEvent), "fatal"
	} else {
		evt, kind = buildHealthyEvent(node, id, meta)
	}
	applyProcStrategy(evt, procStrategy)
	return evt, kind
}

// buildFatalEvent emits a fatal HealthEvent whose RecommendedAction selects the
// remediation: node-reboot => RESTART_BM (node-drainer emits a RebootNode CR),
// gpu-reset => COMPONENT_RESET (a GPUReset CR). Both carry a supported GPU_UUID
// impacted entity (+ PCI) so the node-drainer's partial-drain path finds a
// supported entity and advances (cordon -> drain -> CR) instead of failing with
// "no supported entities for a partial drain". A stable per-node UUID keeps
// re-runs idempotent.
func buildFatalEvent(node, id string, meta map[string]string, fatalAgent, fatalEvent string) *pb.HealthEvent {
	action := pb.RecommendedAction_RESTART_BM
	check, msg := "NodeRebootRequired", "fatal fault requires node reboot (harness)"
	errCode := []string{"reboot"}
	if normalizeFatalEvent(fatalEvent) == fatalEventGPUReset {
		action = pb.RecommendedAction_COMPONENT_RESET
		check, msg = "GpuXidError", "XID 79 - GPU has fallen off the bus (harness)"
		errCode = []string{"79"}
	}
	return &pb.HealthEvent{
		Version: 1, Id: id, Agent: fatalAgent, ComponentClass: "GPU", CheckName: check,
		IsFatal: true, IsHealthy: false, Message: msg,
		RecommendedAction: action, ErrorCode: errCode,
		EntitiesImpacted: []*pb.Entity{
			{EntityType: "PCI", EntityValue: "0000:03:00"},
			{EntityType: "GPU_UUID", EntityValue: gpuUUIDForNode(node)},
		},
		Metadata: meta, NodeName: node, GeneratedTimestamp: timestamppb.Now(),
	}
}

// buildHealthyEvent emits a non-fatal heartbeat (GPU-health or system-info),
// mirroring the base 70/30 mix so a fleet-storm run still carries realistic noise.
func buildHealthyEvent(node, id string, meta map[string]string) (*pb.HealthEvent, string) {
	if mrand.Intn(100) < 70 {
		return &pb.HealthEvent{
			Version: 1, Id: id, Agent: "event-generator", ComponentClass: "GPU", CheckName: "GpuHealth",
			IsFatal: false, IsHealthy: true, Message: "GPU operating normally (harness)",
			RecommendedAction: pb.RecommendedAction_NONE,
			EntitiesImpacted:  []*pb.Entity{{EntityType: "gpu", EntityValue: "0"}},
			Metadata:          meta, NodeName: node, GeneratedTimestamp: timestamppb.Now(),
		}, "healthy"
	}
	return &pb.HealthEvent{
		Version: 1, Id: id, Agent: "event-generator", ComponentClass: "System", CheckName: "SystemInfo",
		IsFatal: false, IsHealthy: true, Message: "System heartbeat (harness)",
		RecommendedAction: pb.RecommendedAction_NONE,
		Metadata:          meta, NodeName: node, GeneratedTimestamp: timestamppb.Now(),
	}, "system"
}

// applyProcStrategy stamps the optional processingStrategy override onto an event.
// default leaves it UNSPECIFIED (0) so the connector/analyzer decides.
func applyProcStrategy(evt *pb.HealthEvent, ps string) {
	if evt == nil {
		return
	}
	switch normalizeProcStrategyName(ps) {
	case procStrategyStoreOnly:
		evt.ProcessingStrategy = pb.ProcessingStrategy_STORE_ONLY
	case procStrategyStoreAndAnalyse:
		evt.ProcessingStrategy = pb.ProcessingStrategy_STORE_AND_ANALYSE
	case procStrategyExecuteRemediation:
		evt.ProcessingStrategy = pb.ProcessingStrategy_EXECUTE_REMEDIATION
	}
}

var sendErrLogged bool

func sendEvent(ctx context.Context, client pb.PlatformConnectorClient, evt *pb.HealthEvent) bool {
	_, err := client.HealthEventOccurredV1(ctx, &pb.HealthEvents{Version: 1, Events: []*pb.HealthEvent{evt}})
	if err != nil && !sendErrLogged {
		errorf("first send error (logged once): %v", err)
		sendErrLogged = true
	}
	return err == nil
}

func writeLedger(w *bufio.Writer, e ledgerEntry) {
	b, _ := json.Marshal(e)
	_, _ = w.Write(b)
	_ = w.WriteByte('\n')
}

// gpuUUIDForNode derives a stable, realistic GPU UUID (GPU-8-4-4-4-12 hex form)
// from the node name so a given node always maps to the same GPU_UUID entity.
// This matches the entity type the node-drainer supports for COMPONENT_RESET
// partial drains (model.EntityTypeToResourceNames["GPU_UUID"]).
func gpuUUIDForNode(node string) string {
	h := sha256.Sum256([]byte("gpu0/" + node))
	return fmt.Sprintf("GPU-%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func randHex(n int) string {
	const hex = "0123456789abcdef"
	b := make([]byte, n*2)
	for i := range b {
		idx, err := rand.Int(rand.Reader, big.NewInt(16))
		if err != nil {
			b[i] = hex[mrand.Intn(16)]
			continue
		}
		b[i] = hex[idx.Int64()]
	}
	return string(b)
}

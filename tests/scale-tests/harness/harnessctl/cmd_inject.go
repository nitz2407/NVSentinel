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
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

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
	fs := flag.NewFlagSet("inject", flag.ExitOnError)
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
	fatalFrac := fs.Float64("fatal-fraction", 0.08, "fraction of fatal GPU XID events")
	fatalAgent := fs.String("fatal-agent", "gpu-health-monitor", "agent for fatal events (gpu-health-monitor => FQM cordons)")
	_ = fs.Parse(args)

	if *runID == "" {
		*runID = fmt.Sprintf("run-%d-%s", time.Now().Unix(), randHex(4))
	}

	// Distributed mode: one command fires every resident injector in the pool.
	if *socket == "" {
		cfg := loadConfig()
		c, err := newClients(cfg)
		if err != nil {
			return err
		}
		return c.injectAcrossPool(ctx, cfg, *rate, *runID)
	}

	infof("injector run-id=%s socket=%s nodes=%d count=%d rate=%.1f/s", *runID, *socket, *nodeCount, *total, *rate)

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
		evt, kind := buildEvent(node, id, *runID, *runLabel, *idLabel, *fatalFrac, *fatalAgent)
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

func buildEvent(node, id, runID, runLabel, idLabel string, fatalFrac float64, fatalAgent string) (*pb.HealthEvent, string) {
	meta := map[string]string{runLabel: runID, idLabel: id}
	switch {
	case mrand.Float64() < fatalFrac:
		// Emit the real syslog GPU-XID signature: RecommendedAction=COMPONENT_RESET
		// with a supported GPU_UUID impacted entity (+ PCI), so the node-drainer's
		// partial-drain path finds a supported entity and advances (cordon ->
		// drain -> GPUReset CR) instead of failing with "no supported entities for
		// a partial drain". A stable per-node UUID keeps re-runs idempotent.
		return &pb.HealthEvent{
			Version: 1, Id: id, Agent: fatalAgent, ComponentClass: "GPU", CheckName: "GpuXidError",
			IsFatal: true, IsHealthy: false, Message: "XID 79 - GPU has fallen off the bus (harness)",
			RecommendedAction: pb.RecommendedAction_COMPONENT_RESET, ErrorCode: []string{"79"},
			EntitiesImpacted: []*pb.Entity{
				{EntityType: "PCI", EntityValue: "0000:03:00"},
				{EntityType: "GPU_UUID", EntityValue: gpuUUIDForNode(node)},
			},
			Metadata: meta, NodeName: node, GeneratedTimestamp: timestamppb.Now(),
		}, "fatal"
	case mrand.Intn(100) < 70:
		return &pb.HealthEvent{
			Version: 1, Id: id, Agent: "event-generator", ComponentClass: "GPU", CheckName: "GpuHealth",
			IsFatal: false, IsHealthy: true, Message: "GPU operating normally (harness)",
			RecommendedAction: pb.RecommendedAction_NONE,
			EntitiesImpacted:  []*pb.Entity{{EntityType: "gpu", EntityValue: "0"}},
			Metadata:          meta, NodeName: node, GeneratedTimestamp: timestamppb.Now(),
		}, "healthy"
	default:
		return &pb.HealthEvent{
			Version: 1, Id: id, Agent: "event-generator", ComponentClass: "System", CheckName: "SystemInfo",
			IsFatal: false, IsHealthy: true, Message: "System heartbeat (harness)",
			RecommendedAction: pb.RecommendedAction_NONE,
			Metadata:          meta, NodeName: node, GeneratedTimestamp: timestamppb.Now(),
		}, "system"
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

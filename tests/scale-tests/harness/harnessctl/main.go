/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// harnessctl is the NVSentinel scale-test harness controller: a single,
// image-free Go binary (bringup, scale-nodes, connector-pool, …). It
// replaces the fragile bash orchestration with client-go, giving typed API
// access, informer-based waits, and structured (JSON/JUnit) results suitable
// for unattended, per-release runs (requirements goal G7). In-cluster work
// (inject/reconcile) is done by staging this same binary onto resident injector
// pods over `kubectl cp`/`exec` — no container image to build or push. Helm
// installs stay as thin shell wrappers.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

type command struct {
	name string
	desc string
	run  func(ctx context.Context, args []string) error
}

func commands() []command {
	return []command{
		{"bringup", "detect + install missing P0.1 stack (monitoring, metrics-server, KWOK, cert-manager, NVSentinel/Janitor) + verify (P0.1)", runBringup},
		{"scale-nodes", "register N GPU-shaped KWOK nodes and wait Ready + record ceiling (P0.2)", runScaleNodes},
		{"ceiling", "ramp node count until degradation and attribute it: harness vs api/etcd (P0.2)", runCeiling},
		{"cleanup", "clear prior-run debris: KWOK nodes + orphaned janitor CRs (+ optional pool)", runCleanup},
		{"inject", "fire every resident injector in the connector pool, attributing events to KWOK node names with correlation IDs (P0.3); -socket drives a single connector", runInject},
		{"reconcile", "account every injected event for a run in-cluster via a resident injector (P0.3); -direct connects to MongoDB directly", runReconcile},
		{"janitor-check", "create RebootNode + GPUReset CRs and verify completion (P0.4)", runJanitorCheck},
		{"connector-pool", "stage a real connector pool + one resident injector per node (P0.5); or run -startup-burst / -connection-sweep experiments", runConnectorPool},
		{"coldstart", "seed a MongoDB haystack (needles + STORE_ONLY noise), cold-start a consumer, and measure its initial scan time", runColdStart},
		{"report", "auto-collect latency/throughput/resource/CR/mongo metrics into report.md + report.json", runReport},
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "harnessctl — NVSentinel scale-test harness controller\n\nUsage:\n  harnessctl <command> [flags]\n\nCommands:\n")
	for _, c := range commands() {
		fmt.Fprintf(os.Stderr, "  %-16s %s\n", c.name, c.desc)
	}
	fmt.Fprintf(os.Stderr, "\nRun 'harnessctl <command> -h' for command flags.\n")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	name := os.Args[1]
	if name == "-h" || name == "--help" || name == "help" {
		usage()
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	for _, c := range commands() {
		if c.name == name {
			if err := c.run(ctx, os.Args[2:]); err != nil {
				errorf("%s: %v", name, err)
				os.Exit(1)
			}
			return
		}
	}
	errorf("unknown command: %s", name)
	usage()
	os.Exit(2)
}

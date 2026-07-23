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

// harnessctl is the NVSentinel scale-test harness controller. A single binary
// that runs both as an operator CLI (preflight, scale-nodes, phase0, …) and as
// the in-cluster Job image (inject, reconcile). It replaces the fragile bash
// orchestration with client-go, giving typed API access, informer-based waits,
// and structured (JSON/JUnit) results suitable for unattended, per-release runs
// (requirements goal G7). Helm installs stay as thin shell wrappers.
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
		{"preflight", "verify tooling + cluster reachability (P0.1)", runPreflight},
		{"bringup", "run the helm install scripts (monitoring + KWOK + NVSentinel) (P0.1)", runBringup},
		{"scale-nodes", "register N GPU-shaped KWOK nodes and wait Ready + record ceiling (P0.2)", runScaleNodes},
		{"teardown-nodes", "delete all simulated KWOK nodes", runTeardownNodes},
		{"inject", "inject events attributed to KWOK node names with correlation IDs (P0.3)", runInject},
		{"reconcile", "account every injected event ID against the datastore (P0.3)", runReconcile},
		{"janitor-check", "create RebootNode + GPUReset CRs and verify completion (P0.4)", runJanitorCheck},
		{"sim-reboot", "simulate a node reboot: NotReady -> Ready + fresh bootID", runSimReboot},
		{"phase0", "run the full Phase 0 acceptance suite and emit structured results", runPhase0},
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

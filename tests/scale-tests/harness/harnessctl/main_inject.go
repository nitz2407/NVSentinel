//go:build injector

/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// Slim in-cluster binary: only the inject/reconcile primitives the operator
// execs into resident injector pods. No Helm, KWOK, pool orchestration, etc.

func usage() {
	fmt.Fprintf(os.Stderr, "harness-inject — in-cluster NVSentinel scale-test injector\n\n")
	fmt.Fprintf(os.Stderr, "Usage:\n  harness-inject inject [flags]\n  harness-inject reconcile [flags]\n\n")
	fmt.Fprintf(os.Stderr, "Run 'harness-inject <command> -h' for flags.\n")
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var run func(context.Context, []string) error
	switch cmd {
	case "inject":
		run = runInject
	case "reconcile":
		run = runReconcile
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err := run(ctx, args); err != nil {
		errorf("%s: %v", cmd, err)
		os.Exit(1)
	}
}

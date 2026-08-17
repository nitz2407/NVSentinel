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
)

type injectDispatchOpts struct {
	mechanism      string
	rate           float64
	runID          string
	total          int
	workers        int
	batch          int
	nodeCount      int
	nodeOffset     int
	coldstartRatio float64
}

func dispatchInjectDistributed(context.Context, Config, injectDispatchOpts) error {
	return fmt.Errorf("distributed inject requires the operator harnessctl binary (pass -socket or -direct-mongo for in-cluster primitives)")
}

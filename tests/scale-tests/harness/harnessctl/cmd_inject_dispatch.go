//go:build !injector

/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import "context"

// injectDispatchOpts carries the distributed-inject knobs from runInject.
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

// dispatchInjectDistributed fans inject across the connector pool (operator CLI).
func dispatchInjectDistributed(ctx context.Context, cfg Config, o injectDispatchOpts) error {
	c, err := newClients(cfg)
	if err != nil {
		return err
	}
	mech := normalizeMechanism(cfg.Mechanism)
	if normalizeMechanism(o.mechanism) == mechanismMongo {
		mech = mechanismMongo
	}
	if mech == mechanismMongo {
		return c.injectMongoAcrossPool(ctx, cfg, mongoDistOptions{
			total: o.total, workers: o.workers, batch: o.batch,
			nodeCount: o.nodeCount, nodeOffset: o.nodeOffset, runID: o.runID,
			coldstartRatio: o.coldstartRatio,
		})
	}
	// -count reaches the mongo path as mongoDistOptions.total but had no route
	// into the pool path, which reads cfg.EventCount. The flag was therefore
	// inert over gRPC: every distributed run injected the config default
	// regardless of what was asked for.
	//
	// Assigned unconditionally, including 0: o.total is already the resolved
	// flag, so a 0 here means the caller stated no volume and injectAcrossPool
	// should fall back to one event per emulated node. Guarding on > 0 would
	// leave Config's own non-zero default in place and make that fallback
	// unreachable. cfg is a value copy, so this affects only this dispatch.
	cfg.EventCount = o.total
	return c.injectAcrossPool(ctx, cfg, o.rate, o.runID)
}

//go:build !injector

/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import "testing"

func TestClusterUtilBreaches(t *testing.T) {
	cfg := Config{MaxClusterCPUPct: 0.85, MaxClusterMemPct: 0.85}

	if over, _ := (clusterUtil{OK: false}).breaches(cfg); over {
		t.Error("unavailable metrics must not breach")
	}
	if over, _ := (clusterUtil{OK: true, CPUPct: 0.5, MemPct: 0.5}).breaches(cfg); over {
		t.Error("within-bounds util must not breach")
	}
	if over, _ := (clusterUtil{OK: true, CPUPct: 0.9, MemPct: 0.5}).breaches(cfg); !over {
		t.Error("cpu over guardrail must breach")
	}
	if over, _ := (clusterUtil{OK: true, CPUPct: 0.5, MemPct: 0.95}).breaches(cfg); !over {
		t.Error("mem over guardrail must breach")
	}
}

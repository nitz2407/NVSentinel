/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/


//go:build !injector

package main

import "testing"

func TestComputeLatency(t *testing.T) {
	if s := computeLatency(nil); s.Count != 0 {
		t.Fatalf("empty input should give zero-count stats, got %+v", s)
	}

	// 1..100 seconds.
	in := make([]float64, 0, 100)
	for i := 1; i <= 100; i++ {
		in = append(in, float64(i))
	}
	s := computeLatency(in)
	if s.Count != 100 {
		t.Errorf("count = %d, want 100", s.Count)
	}
	if s.Min != 1 || s.Max != 100 {
		t.Errorf("min/max = %v/%v, want 1/100", s.Min, s.Max)
	}
	if s.Mean != 50.5 {
		t.Errorf("mean = %v, want 50.5", s.Mean)
	}
	// pctile uses nearest-rank on (len-1)*q: p50 -> idx 49 -> value 50.
	if s.P50 != 50 {
		t.Errorf("p50 = %v, want 50", s.P50)
	}
	if s.P90 != 90 {
		t.Errorf("p90 = %v, want 90", s.P90)
	}
	// (100-1)*0.99 = 98.01 -> idx 98 -> value 99.
	if s.P99 != 99 {
		t.Errorf("p99 = %v, want 99", s.P99)
	}
}

func TestMfmt(t *testing.T) {
	if got := mfmt(metric{OK: false}, 1, "s", 3); got != "n/a" {
		t.Errorf("not-ok metric = %q, want n/a", got)
	}
	if got := mfmt(metric{Value: 0.0955, OK: true}, 1, "s", 3); got != "0.096s" {
		t.Errorf("rounded metric = %q, want 0.096s", got)
	}
	if got := mfmt(metric{Value: 12, OK: true}, 1, "", 0); got != "12" {
		t.Errorf("int metric = %q, want 12", got)
	}
}

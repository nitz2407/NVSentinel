/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import "testing"

func TestVerifyNodeAttribution(t *testing.T) {
	cases := []struct {
		name          string
		injected      map[string]string
		storedNode    map[string]string
		sample        int
		wantVerdict   string
		wantMismatch  int
		wantChecked   int
		wantNoteEmpty bool
	}{
		{
			name:        "all_match",
			injected:    map[string]string{"a": "n1", "b": "n2", "c": "n3"},
			storedNode:  map[string]string{"a": "n1", "b": "n2", "c": "n3"},
			sample:      200,
			wantVerdict: "PASS",
			wantChecked: 3,
		},
		{
			name:         "one_mismatch_fails",
			injected:     map[string]string{"a": "n1", "b": "n2"},
			storedNode:   map[string]string{"a": "n1", "b": "nX"},
			sample:       200,
			wantVerdict:  "FAIL",
			wantMismatch: 1,
			wantChecked:  2,
		},
		{
			name:        "all_empty_stored_is_unverified_not_fail",
			injected:    map[string]string{"a": "n1", "b": "n2"},
			storedNode:  map[string]string{"a": "", "b": ""},
			sample:      200,
			wantVerdict: "PASS",
			wantChecked: 0,
		},
		{
			name:        "disabled_sample",
			injected:    map[string]string{"a": "n1"},
			storedNode:  map[string]string{"a": "nX"},
			sample:      0,
			wantVerdict: "PASS",
			wantChecked: 0,
		},
		{
			name:        "unaccounted_ids_skipped",
			injected:    map[string]string{"a": "n1", "gone": "n9"},
			storedNode:  map[string]string{"a": "n1"},
			sample:      200,
			wantVerdict: "PASS",
			wantChecked: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := &ReconcileReport{Verdict: "PASS"}
			verifyNodeAttribution(rep, tc.injected, tc.storedNode, tc.sample)
			if rep.Verdict != tc.wantVerdict {
				t.Errorf("verdict = %q, want %q (note: %s)", rep.Verdict, tc.wantVerdict, rep.NodeAttrNote)
			}
			if rep.NodeMismatched != tc.wantMismatch {
				t.Errorf("mismatched = %d, want %d", rep.NodeMismatched, tc.wantMismatch)
			}
			if rep.NodeChecked != tc.wantChecked {
				t.Errorf("checked = %d, want %d", rep.NodeChecked, tc.wantChecked)
			}
			if rep.NodeAttrNote == "" {
				t.Errorf("expected a non-empty NodeAttrNote")
			}
		})
	}
}

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

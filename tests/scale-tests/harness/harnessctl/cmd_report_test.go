//go:build !injector

/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

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

func TestMatchPodPrefix(t *testing.T) {
	if !matchPodPrefix("janitor-7d9f", "janitor-", "janitor-provider") {
		t.Fatal("janitor controller should match")
	}
	if matchPodPrefix("janitor-provider-abc", "janitor-", "janitor-provider") {
		t.Fatal("janitor-provider should be excluded")
	}
	if !matchPodPrefix("labeler-abc", "labeler", "") {
		t.Fatal("labeler should match")
	}
}

func TestApplyPerNodeRates(t *testing.T) {
	cu := componentUsage{Name: "labeler", Pods: 1, CPUMilli: 2000, MemMi: 16000}
	applyPerNodeRates(&cu, 50000)
	if cu.MemMiPerKwokNode != 16000.0/50000.0 {
		t.Fatalf("mem per node = %v, want %v", cu.MemMiPerKwokNode, 16000.0/50000.0)
	}
	applyPerNodeRates(&cu, 0)
	// n=0 must not divide
}

func TestPromSubqueryPeak(t *testing.T) {
	got := promSubqueryPeak(promProcessMemInner("nvsentinel"), "4d", "pod")
	want := `max_over_time((sum by (pod) (process_resident_memory_bytes{namespace="nvsentinel"}))[4d:1m])`
	if got != want {
		t.Fatalf("query = %s, want %s", got, want)
	}
}

func TestMergePromUsage(t *testing.T) {
	items := mergePromUsage(
		[]promVectorSample{{Metric: map[string]string{"pod": "labeler-a"}, Value: 16 << 30}},
		[]promVectorSample{{Metric: map[string]string{"pod": "labeler-a"}, Value: 1.5}},
	)
	if len(items) != 1 || items[0].name != "labeler-a" {
		t.Fatalf("items = %+v", items)
	}
	if items[0].memMi != 16384 {
		t.Fatalf("memMi = %d, want 16384", items[0].memMi)
	}
	if items[0].cpuMilli != 1500 {
		t.Fatalf("cpuMilli = %d, want 1500", items[0].cpuMilli)
	}
}

func TestMergePodUsageGaps(t *testing.T) {
	dst := []podUsageItem{{name: "labeler-a", memMi: 100}}
	extra := []podUsageItem{{name: "labeler-a", memMi: 1}, {name: "mongodb-0", memMi: 50}}
	got, filled := mergePodUsageGaps(dst, extra)
	if !filled || len(got) != 2 {
		t.Fatalf("got=%+v filled=%v", got, filled)
	}
	if got[0].memMi != 100 {
		t.Fatal("prometheus peak must not be overwritten")
	}
}

func TestParsePromQueryResponse(t *testing.T) {
	raw := []byte(`{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"pod":"labeler-a"},"value":[1,"16777216"]},
		{"metric":{"pod":"fault-quarantine-b"},"value":[1,"1048576"]}
	]}}`)
	samples, err := parsePromQueryResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || samples[0].Metric["pod"] != "labeler-a" || samples[0].Value != 16777216 {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestIsMetricsServerNanny(t *testing.T) {
	if !isMetricsServerNanny(corev1.Container{Name: "metrics-server-vpa", Image: "mcr.microsoft.com/oss/v2/kubernetes/autoscaler/addon-resizer:v1.8.23-28"}) {
		t.Fatal("AKS addon-resizer should be detected as nanny")
	}
	if isMetricsServerNanny(corev1.Container{Name: "metrics-server", Image: "mcr.microsoft.com/oss/v2/kubernetes/metrics-server:v0.7.2-24"}) {
		t.Fatal("metrics-server itself is not a nanny")
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

// The default fatal event (RESTART_BM) produces RebootNode CRs, never GPUReset.
// Reporting only GPUReset made every default run print "not exercised" even
// though remediation had cordoned, drained and rebooted the fleet.
func TestRemediationBodyReportsRebootNodeRuns(t *testing.T) {
	d := reportData{
		Nodes: nodeStats{
			Cordoned: 835, CordonWithTS: 835,
			CordonFirstTS: "2026-09-16T08:28:02Z", CordonLastTS: "2026-09-16T08:33:02Z",
			CordonSpanSec: 300,
		},
		RebootNode: crStats{
			Total:         835,
			Phase:         map[string]int{"Completed": 800, "InProgress": 30, "(pending)": 5},
			Latency:       computeLatency([]float64{5, 10, 15}),
			CreateWithTS:  835,
			CreateSpanSec: 240,
			CreateFirstTS: "2026-09-16T08:28:00Z", CreateLastTS: "2026-09-16T08:32:00Z",
		},
		Footprint: map[string]int{"rebootnode_crs": 835},
	}

	var b strings.Builder
	renderRemediationBody(&b, d)
	got := b.String()

	if strings.Contains(got, "Not exercised") {
		t.Fatalf("a reboot-driven run must not read as unexercised, got:\n%s", got)
	}
	for _, want := range []string{
		"RebootNode CR (start→complete)",
		"| Create RebootNode CRs | 835 |", // cordon/CR storm throughput
		"| Cordon nodes | 835 |",
		"#### RebootNode remediation state",
		"| Completed | 800 |",
		"RebootNode (835 CRs)", // states which path was exercised
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// A kind this run never produced should not contribute an empty table.
	if strings.Contains(got, "#### GPUReset remediation state") {
		t.Error("GPUReset produced no CRs, so it should not get a state table")
	}
}

func TestRemediationBodyStillUnexercisedWhenNothingHappened(t *testing.T) {
	var b strings.Builder
	renderRemediationBody(&b, reportData{Footprint: map[string]int{}})
	if !strings.Contains(b.String(), "Not exercised") {
		t.Fatalf("no cordons and no CRs is genuinely unexercised, got:\n%s", b.String())
	}
}

func TestRebootNodePhaseDerivesLifecycle(t *testing.T) {
	cases := []struct {
		name   string
		status map[string]any
		want   string
	}{
		{"completed", map[string]any{"startTime": "t0", "completionTime": "t1"}, "Completed"},
		{"started", map[string]any{"startTime": "t0"}, "InProgress"},
		{"no status yet", nil, "(pending)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := map[string]any{}
			if tc.status != nil {
				obj["status"] = tc.status
			}
			if got := rebootNodePhase(obj); got != tc.want {
				t.Errorf("rebootNodePhase = %q, want %q", got, tc.want)
			}
		})
	}
}

// A start time that moves backwards is proof, so one reversal is enough and a
// clean series is trusted without consulting the fallback.
func TestAPIServerCounterTrustUsesStartTimeReversals(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reversals  float64
		wantUsable bool
	}{
		{"single apiserver, start time never moves back", 0, true},
		{"one reversal is already proof", 1, false},
		{"six replicas behind one endpoint", 27, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := apiserverCounterTrust(func(q string) metric {
				if !strings.Contains(q, "process_start_time_seconds") {
					t.Fatalf("fallback consulted despite a usable start time: %s", q)
				}
				return metric{Value: tc.reversals, OK: true}
			}, "apiserver", "20m")
			if got.Usable != tc.wantUsable {
				t.Fatalf("Usable = %v, want %v", got.Usable, tc.wantUsable)
			}
			if got.Signal != apiserverSignalStartTime {
				t.Fatalf("Signal = %q, want the start-time test", got.Signal)
			}
		})
	}
}

// This is the case that slipped through the first version of this probe: an 8m
// window held a median of 2 request-counter resets, under a fixed bound of 3,
// even though the endpoint was plainly multiplexed. Reset counts scale with the
// samples in the window, so the fallback has to be a fraction.
func TestAPIServerCounterTrustFallbackIsWindowLengthIndependent(t *testing.T) {
	probe := func(resets, samples float64) apiserverTrust {
		return apiserverCounterTrust(func(q string) metric {
			switch {
			case strings.Contains(q, "process_start_time_seconds"):
				return metric{OK: false} // not exposed, forcing the fallback
			case strings.Contains(q, "count_over_time"):
				return metric{Value: samples, OK: true}
			default:
				return metric{Value: resets, OK: true}
			}
		}, "apiserver", "20m")
	}

	// The same hopping fraction must read the same at every window length.
	for _, c := range []struct{ resets, samples float64 }{{7, 19}, {23, 60}, {45, 120}} {
		if got := probe(c.resets, c.samples); got.Usable {
			t.Errorf("%v resets over %v samples is multiplexing, got usable", c.resets, c.samples)
		}
	}
	// A couple of restarts over a decent window is not.
	if got := probe(2, 60); !got.Usable {
		t.Errorf("2 resets over 60 samples is a restart, not multiplexing: %+v", got)
	}
	// Too few samples to tell a restart from hopping: decline, do not guess.
	if got := probe(2, 8); !got.Usable || got.ProbeOK {
		t.Errorf("a window too short to judge must be inconclusive and usable, got %+v", got)
	}
}

// The window has to reach the queries, or the probe measures a different span
// than the metrics it is vouching for.
func TestAPIServerCounterTrustUsesTheReportWindow(t *testing.T) {
	var asked []string
	apiserverCounterTrust(func(q string) metric {
		asked = append(asked, q)
		return metric{Value: 0, OK: true}
	}, "apiserver", "39m")
	if len(asked) == 0 || !strings.Contains(asked[0], "[39m]") {
		t.Fatalf("probe query %q does not range over the report window", asked)
	}
}

// Without the job label the start-time test cannot be aimed at the apiserver,
// so it must not be attempted against every target in the cluster.
func TestAPIServerCounterTrustSkipsStartTimeWithoutAJob(t *testing.T) {
	apiserverCounterTrust(func(q string) metric {
		if strings.Contains(q, "process_start_time_seconds") {
			t.Fatalf("start time probed with no job to scope it: %s", q)
		}
		return metric{Value: 0, OK: true}
	}, "", "20m")
}

// An unusable counter must not produce a ceiling. "Control plane is the bound"
// gets carried into sizing decisions, so a latency that cannot be trusted has
// to yield no attribution rather than a plausible one.
func TestReportCeilingRefusesAnUntrustworthyAPIServer(t *testing.T) {
	cfg := Config{MaxAPIServerP99: 1.0, CeilingListP99: 5.0}
	d := reportData{
		APIServer: map[string]metric{
			"read_p99_s":       {Value: 5.208, OK: true}, // over guardrail
			"list_nodes_p99_s": {Value: 12.124, OK: true},
			"inflight":         {Value: 69, OK: true},
		},
		APIServerOK: apiserverTrust{Reversals: 27, Signal: apiserverSignalStartTime, ProbeOK: true, Usable: false},
	}
	got := attributeReportCeiling(cfg, d)
	if !strings.Contains(got, "No saturation attributed") {
		t.Fatalf("an unusable apiserver counter must attribute no ceiling, got: %s", got)
	}
	if strings.Contains(got, "REAL ceiling") {
		t.Fatalf("ceiling claimed from an untrustworthy latency: %s", got)
	}

	d.APIServerOK.Usable = true
	if got := attributeReportCeiling(cfg, d); !strings.Contains(got, "REAL ceiling") {
		t.Fatalf("a trustworthy counter over the guardrail must still attribute a ceiling, got: %s", got)
	}
}

// Pass criteria must not render a ✅ or a ⚠️ off the bad counter; both are
// assertions the run cannot support.
func TestPassCriteriaWithholdsAPIServerVerdicts(t *testing.T) {
	d := reportData{
		Guardrails: map[string]float64{"apiserver_p99_s": 1.0},
		APIServer: map[string]metric{
			"all_p99_s":   {Value: 0.985, OK: true},
			"err5xx_rate": {Value: 120.88, OK: true},
		},
		APIServerOK: apiserverTrust{Reversals: 27, Signal: apiserverSignalStartTime, ProbeOK: true, Usable: false},
		Nodes:       nodeStats{Kwok: 1000, KwokReady: 1000},
	}
	var b strings.Builder
	renderPassCriteria(&b, d)
	got := b.String()

	if strings.Contains(got, "within guardrail") || strings.Contains(got, "5xx rate peaked") {
		t.Fatalf("apiserver criteria asserted off an unusable counter:\n%s", got)
	}
	if !strings.Contains(got, "not assessed") {
		t.Fatalf("withheld criteria must say so:\n%s", got)
	}
	// Unrelated criteria still have to be reported.
	if !strings.Contains(got, "KWOK node readiness") {
		t.Fatalf("KWOK readiness is independent of the apiserver counter and must survive:\n%s", got)
	}
}

// The table keeps its numbers, because they are the evidence a reader needs to
// believe the warning.
func TestAPIServerTableWarnsButKeepsTheEvidence(t *testing.T) {
	d := reportData{
		Window: "20m",
		APIServer: map[string]metric{
			"req_rate": {Value: 1032299, OK: true},
		},
		APIServerOK: apiserverTrust{Reversals: 27, Signal: apiserverSignalStartTime, ProbeOK: true, Usable: false},
	}
	var b strings.Builder
	renderAPIServer(&b, d)
	got := b.String()

	if !strings.Contains(got, "Do not use this table") {
		t.Fatalf("unusable apiserver metrics must be called out:\n%s", got)
	}
	if !strings.Contains(got, "1032299") {
		t.Fatalf("the implausible value is the evidence and must stay:\n%s", got)
	}

	d.APIServerOK.Usable = true
	var ok strings.Builder
	renderAPIServer(&ok, d)
	if strings.Contains(ok.String(), "Do not use this table") {
		t.Fatalf("a trustworthy table must carry no warning:\n%s", ok.String())
	}
}

// The AKS 1k run that prompted this: a platform-connectors rollout left 56
// pods with series for a 28-pod DaemonSet, and summing them read as 1.2 GB
// against an actual 547 MiB.
func TestDropReplacedPodsExcludesPodsThatAreGone(t *testing.T) {
	items := []podUsageItem{
		{name: "platform-connectors-new1", memMi: 20},
		{name: "platform-connectors-old1", memMi: 20},
		{name: "labeler-new", memMi: 59},
		{name: "labeler-old", memMi: 137},
	}
	live := map[string]bool{"platform-connectors-new1": true, "labeler-new": true}
	dropReplacedPods(items, live)

	for _, tc := range []struct {
		name     string
		wantGone bool
	}{
		{"platform-connectors-new1", false},
		{"platform-connectors-old1", true},
		{"labeler-new", false},
		{"labeler-old", true},
	} {
		var got podUsageItem
		for _, it := range items {
			if it.name == tc.name {
				got = it
			}
		}
		if got.gone != tc.wantGone {
			t.Errorf("%s: gone = %v, want %v", tc.name, got.gone, tc.wantGone)
		}
	}
}

// A DaemonSet legitimately runs many pods, so existence — not a count — has to
// be the test. This is the case the old `Pods > 1` heuristic had to special-case
// by name, and therefore never caught.
func TestDropReplacedPodsKeepsAFullDaemonSet(t *testing.T) {
	var items []podUsageItem
	live := map[string]bool{}
	for i := 0; i < 28; i++ {
		n := fmt.Sprintf("platform-connectors-%d", i)
		items = append(items, podUsageItem{name: n, memMi: 20})
		live[n] = true
	}
	dropReplacedPods(items, live)
	for _, it := range items {
		if it.gone {
			t.Fatalf("%s is live and must be measured", it.name)
		}
	}
}

// Marking rather than deleting matters: an empty live set must not silently
// zero a component, it must be visible as excluded pods.
func TestCollectComponentsSeparatesLiveFromReplaced(t *testing.T) {
	items := []podUsageItem{
		{name: "labeler-new", memMi: 59, cpuMilli: 10, source: srcPromContainer},
		{name: "labeler-old", memMi: 137, cpuMilli: 450, source: srcPromContainer},
	}
	dropReplacedPods(items, map[string]bool{"labeler-new": true})

	cu := componentUsage{Name: "labeler"}
	for _, it := range items {
		if matchPodPrefix(it.name, "labeler", "") {
			if it.gone {
				cu.Replaced++
				continue
			}
			cu.Pods++
			cu.MemMi += it.memMi
			cu.CPUMilli += it.cpuMilli
		}
	}
	if cu.Pods != 1 || cu.MemMi != 59 || cu.CPUMilli != 10 {
		t.Fatalf("live pod only: pods=%d mem=%d cpu=%d, want 1/59/10", cu.Pods, cu.MemMi, cu.CPUMilli)
	}
	if cu.Replaced != 1 {
		t.Fatalf("Replaced = %d, want 1 so the caveat can name it", cu.Replaced)
	}
}

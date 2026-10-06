//go:build !injector

/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gbToMiB converts the decimal GB figures quoted in NVSentinel#1705 into the
// MiB the harness stores, so tests can be written in the issue's own units.
func gbToMiB(gb float64) int64 { return int64(math.Round(gb * 1e9 / bytesPerMiB)) }

func nearly(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s = %v, want %v (±%v)", what, got, want, tol)
	}
}

func TestBuildA1SizingSplitsMongoAndIgnoresHarnessPods(t *testing.T) {
	s := fleetSizing{
		NKwok:           50000,
		PodsPerKwokNode: 2,
		Window:          "40m",
		Components: []componentUsage{
			{Name: "labeler", Pods: 1, MemMi: 4000, CPUMilli: 500, NVSentinel: true, Source: "prometheus"},
			{Name: "fault-quarantine", Pods: 2, MemMi: 6000, CPUMilli: 250, NVSentinel: true, Source: "prometheus"},
			{Name: "janitor", Pods: 1, MemMi: 1000, CPUMilli: 100, NVSentinel: true, Source: "prometheus"},
			{Name: mongoComponentName, Pods: 3, MemMi: 9000, CPUMilli: 900, NVSentinel: true, Source: "kubelet"},
			// Harness scaffolding: must not land in any A1 column.
			{Name: "connector-pool", Pods: 400, MemMi: 99000, CPUMilli: 9000, NVSentinel: false, Source: "prometheus"},
			{Name: "kwok-controller", Pods: 1, MemMi: 50000, CPUMilli: 4000, NVSentinel: false, Source: "prometheus"},
		},
	}

	a := buildA1Sizing(s, "unit", "")

	if got, want := a.Current.MongoMi, int64(9000); got != want {
		t.Fatalf("mongodb column = %d MiB, want %d", got, want)
	}
	if got, want := a.Current.ComponentsMi, int64(11000); got != want {
		t.Fatalf("components column = %d MiB, want %d (mongo and harness pods excluded)", got, want)
	}
	// #1705's "Control plane" column is the sum of the other two.
	if got, want := a.Current.ControlPlaneMi, a.Current.MongoMi+a.Current.ComponentsMi; got != want {
		t.Fatalf("control plane = %d MiB, want %d", got, want)
	}

	if len(a.Components) != 3 {
		t.Fatalf("component ranking = %d rows, want 3 (mongo broken out, harness pods dropped)", len(a.Components))
	}
	if a.Components[0].Name != "fault-quarantine" || a.Components[1].Name != "labeler" {
		t.Fatalf("ranking not sorted by memory descending: %v, %v", a.Components[0].Name, a.Components[1].Name)
	}
	nearly(t, "top-two share", a.TopTwoShare, 10000.0/11000.0, 1e-9)
	nearly(t, "fault-quarantine share", a.Components[0].ShareOfNVS, 6000.0/11000.0, 1e-9)

	// 11000 MiB over 50k nodes, expressed in the decimal MB/node of #1705.
	nearly(t, "components MB/node", a.ComponentMBPer, 11000*bytesPerMiB/1e6/50000, 1e-6)
	nearly(t, "control-plane MB/node", a.MBPerNode, 20000*bytesPerMiB/1e6/50000, 1e-6)

	if a.Current.Nodes != 50000 {
		t.Fatalf("the section should carry this run's fleet size, got %+v", a.Current)
	}
	// The single-fleet-size limitation belongs in exactly one place. It used to
	// appear in the glance, again as a caveat, and again in a trailing note; the
	// caveat is the one that survives, so the glance must stay pure narrative.
	if strings.Contains(a.Glance, "harnessctl sizing curve") {
		t.Errorf("glance should not repeat the curve pointer that the caveats carry, got %q", a.Glance)
	}
	if !strings.Contains(strings.Join(a.Caveats, "\n"), "harnessctl sizing curve") {
		t.Errorf("caveats should send the reader to the curve for per-1k cost, got %q", a.Caveats)
	}
}

// TestA1FitsMatchIssue1705 feeds the two endpoint rows quoted in #1705 and
// checks the derived rates match the issue's own prose: components grow 0.71 GB
// per 1,000 nodes from 25k to 100k, and the control-plane column is ~2.35
// MB/node at 100k with no meaningful fixed term.
func TestA1FitsMatchIssue1705(t *testing.T) {
	rows := []struct {
		n               int
		mongoGB, compGB float64
	}{
		{10000, 17, 4.6},
		{25000, 43, 11.4},
		{50000, 85, 27.5},
		{100000, 170, 64.9},
	}
	var points []a1Point
	for _, r := range rows {
		mongo, comp := gbToMiB(r.mongoGB), gbToMiB(r.compGB)
		points = append(points, a1Point{
			Nodes:          r.n,
			MongoMi:        mongo,
			ComponentsMi:   comp,
			ControlPlaneMi: mongo + comp,
		})
	}

	// The issue's Control plane column: 22 / 54 / 113 / 235 GB. The tolerance
	// covers the issue's own rounding of the summed columns (112.5 → 113).
	wantCP := []float64{22, 54, 113, 235}
	for i, p := range points {
		nearly(t, "control plane GB", p.controlPlaneGB(), wantCP[i], 0.55)
	}

	fits := a1Fits(points)
	comp := fitFor(fits, "nvsentinel_components")
	if comp == nil {
		t.Fatal("no components fit")
	}
	if comp.FromNodes != 10000 || comp.ToNodes != 100000 || comp.Points != 4 {
		t.Fatalf("components fit spans %d→%d over %d points, want 10000→100000 over 4", comp.FromNodes, comp.ToNodes, comp.Points)
	}
	nearly(t, "components marginal 10k→100k GB/1k", comp.MarginalGBPerK, 0.67, 0.01)

	// #1705: "with no meaningful fixed term". Four sizes make the terms
	// separable, and the intercept lands at ~2% of the 100k value.
	cp := fitFor(fits, "control_plane")
	if cp == nil {
		t.Fatal("no control-plane fit")
	}
	if !cp.Separable {
		t.Fatal("four fleet sizes should separate fixed from marginal cost")
	}
	if cp.FixedMeaningful || cp.SuperLinear {
		t.Fatalf("#1705 reports no meaningful fixed term, got %.2f GB", cp.FixedGB)
	}
	nearly(t, "control plane MB/node at 100k", mibToMBPerNode(points[3].ControlPlaneMi, 100000), 2.35, 0.02)

	// The component column's marginal rate climbs with N (0.46 MB/node at 10k
	// against 0.65 at 100k), which the fit surfaces as super-linear growth
	// rather than a nonsensical negative fixed cost.
	if !comp.SuperLinear || comp.FixedMeaningful {
		t.Fatalf("components should read as super-linear, got fixed %.2f GB (meaningful=%v, super=%v)",
			comp.FixedGB, comp.FixedMeaningful, comp.SuperLinear)
	}
}

// TestA1FitsUseDistinctFleetSizes guards the case where the same N is measured
// at two pod densities: that is not a size curve, so a fit needs distinct N.
func TestA1FitsUseDistinctFleetSizes(t *testing.T) {
	points := []a1Point{
		{Nodes: 50000, PodsPerNode: 0, ComponentsMi: 1000, ControlPlaneMi: 1000},
		{Nodes: 50000, PodsPerNode: 10, ComponentsMi: 3000, ControlPlaneMi: 3000},
	}
	if fits := a1Fits(points); fits != nil {
		t.Fatalf("two densities at one fleet size must not produce a size fit, got %+v", fits)
	}
}

func TestA1CaveatsNameUnmeasuredColumns(t *testing.T) {
	s := fleetSizing{
		NKwok:           50000,
		PodsPerKwokNode: 0,
		Components: []componentUsage{
			{Name: "labeler", Pods: 1, MemMi: 4000, NVSentinel: true, Source: "prometheus"},
			{Name: "kubernetes-object-monitor", Pods: 0, NVSentinel: true},
			{Name: mongoComponentName, Pods: 0, NVSentinel: true},
		},
	}
	a := buildA1Sizing(s, "unit", "")
	joined := strings.Join(a.Caveats, "\n")

	for _, want := range []string{
		"kubernetes-object-monitor",
		"MongoDB memory is 0",
		"P=0 pods per KWOK node",
		"One fleet size (N=50,000) measured in this run",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("caveats missing %q, got:\n%s", want, joined)
		}
	}
}

func TestA1SizingHandlesEmptyFleet(t *testing.T) {
	a := buildA1Sizing(fleetSizing{}, "unit", "")
	if a.MBPerNode != 0 || a.ComponentMBPer != 0 {
		t.Fatalf("no nodes should give no per-node rate, got %+v", a)
	}
	if !strings.Contains(a.Glance, "cannot be expressed per node") {
		t.Fatalf("glance should explain the empty fleet, got %q", a.Glance)
	}
	var b strings.Builder
	renderA1(&b, reportData{A1: a})
	if b.Len() != 0 {
		t.Fatalf("empty A1 should render nothing, got %q", b.String())
	}
}

func TestRenderA1MatchesIssueTableShape(t *testing.T) {
	s := fleetSizing{
		NKwok:           50000,
		PodsPerKwokNode: 1.5,
		Window:          "40m",
		Components: []componentUsage{
			{Name: "labeler", Pods: 1, MemMi: gbToMiB(3.9), NVSentinel: true, Source: "prometheus"},
			{Name: "fault-quarantine", Pods: 2, MemMi: gbToMiB(8.6), NVSentinel: true, Source: "prometheus"},
			{Name: mongoComponentName, Pods: 3, MemMi: gbToMiB(85), NVSentinel: true, Source: "kubelet"},
		},
	}
	var b strings.Builder
	renderA1(&b, reportData{A1: buildA1Sizing(s, "unit", ""), Window: "40m"})
	md := b.String()

	for _, want := range []string{
		"Component sizing",
		"How much CPU and memory NVSentinel needs",
		"| Nodes | Control plane | MongoDB | NVSentinel components |",
		"| 50,000 |",
		"12.5 GB", // 3.9 + 8.6 components
		"85.0 GB", // mongo
		"97.5 GB", // control plane
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("rendered A1 missing %q, got:\n%s", want, md)
		}
	}
}

// A report must describe its own run and nothing else. The previous design
// merged every run into a shared history file, so re-reading an old report
// showed rows that a later run introduced and a re-run could overwrite a good
// measurement. The A1 section therefore carries exactly one row now, and points
// at the aggregate command for the curve.
func TestRenderA1IsSingleRunOnly(t *testing.T) {
	s := fleetSizing{
		NKwok:           50000,
		PodsPerKwokNode: 1,
		Window:          "40m",
		Components: []componentUsage{
			{Name: "labeler", Pods: 1, MemMi: gbToMiB(19), NVSentinel: true, Source: "prometheus"},
			{Name: mongoComponentName, Pods: 3, MemMi: gbToMiB(85), NVSentinel: true, Source: "prometheus"},
		},
	}
	a := buildA1Sizing(s, "unit", "run-50k")
	if a.Current.RunID != "run-50k" {
		t.Fatalf("point must carry its run id for later attribution, got %q", a.Current.RunID)
	}

	var b strings.Builder
	renderA1(&b, reportData{A1: a, Window: "40m"})
	md := b.String()

	if !strings.Contains(md, "| 50,000 |") {
		t.Fatalf("A1 should show this run's row, got:\n%s", md)
	}
	for _, unwanted := range []string{"◀ this run", "| Series | Fixed term | Slope | Marginal |", "accumulate in"} {
		if strings.Contains(md, unwanted) {
			t.Fatalf("A1 must not carry cross-run content %q, got:\n%s", unwanted, md)
		}
	}
	if !strings.Contains(md, "harnessctl sizing curve") {
		t.Fatalf("A1 should point at the aggregate command, got:\n%s", md)
	}
	// A single run cannot separate a fixed cost from a per-node one, so it must
	// not imply it has.
	if strings.Contains(a.Glance, "fixed term of") {
		t.Fatalf("one run must not claim a fixed term, got %q", a.Glance)
	}
}

// The report window decides WHICH fleet the peaks describe, so "auto" must span
// only from the moment this run's fleet reached target size. A fixed window is
// what let a 10k fleet report more memory than the 50k fleet it replaced.
func TestResolveReportWindowAuto(t *testing.T) {
	writeCeiling := func(t *testing.T, readyAt string) Config {
		t.Helper()
		dir := t.TempDir()
		body := `{"target_nodes":25000,"ready_nodes":24998,"ready_at_utc":"` + readyAt + `"}`
		if err := os.WriteFile(filepath.Join(dir, "p0.2-node-ceiling.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return Config{ResultsDir: dir, ReportWindow: "3h"}
	}

	// Fleet ready 20 minutes ago -> 20m span + 2m margin, rounded up to the
	// whole minute (so 22m or 23m depending on sub-second truncation), and
	// crucially not the 3h default.
	cfg := writeCeiling(t, time.Now().UTC().Add(-20*time.Minute).Format(time.RFC3339))
	got, note, err := resolveReportWindow(windowAuto, cfg)
	if err != nil {
		t.Fatalf("anchored window must resolve: %v", err)
	}
	if got != "22m" && got != "23m" {
		t.Fatalf("auto window = %q, want 22m or 23m", got)
	}
	if !strings.Contains(note, "24998 nodes") || !strings.Contains(note, "exclude any fleet this run replaced") {
		t.Fatalf("note should explain the derivation, got %q", note)
	}

	// A report run immediately after scale still needs enough scrape points.
	cfg = writeCeiling(t, time.Now().UTC().Format(time.RFC3339))
	if got, _, err = resolveReportWindow(windowAuto, cfg); err != nil || got != "6m" {
		t.Fatalf("window should clamp up to the floor, got %q (%v)", got, err)
	}

	// A stale timestamp must not reach back over unrelated history.
	cfg = writeCeiling(t, time.Now().UTC().Add(-30*time.Hour).Format(time.RFC3339))
	if got, note, err = resolveReportWindow(windowAuto, cfg); err != nil || got != "360m" {
		t.Fatalf("window should cap at 6h, got %q (%s) (%v)", got, note, err)
	}

	// An explicit window always wins, for re-reporting a past window.
	cfg = writeCeiling(t, time.Now().UTC().Add(-20*time.Minute).Format(time.RFC3339))
	if got, note, err = resolveReportWindow("15m", cfg); err != nil || got != "15m" || !strings.Contains(note, "explicitly") {
		t.Fatalf("explicit window must win, got %q (%s) (%v)", got, note, err)
	}

	// With no anchor at all, refusing is the point: a defaulted window is the
	// 10k-reports-more-than-50k bug wearing the look of a successful report.
	if _, _, err = resolveReportWindow(windowAuto, Config{ResultsDir: t.TempDir(), ReportWindow: "3h"}); err == nil {
		t.Fatal("a missing anchor must be an error, not a defaulted window")
	} else if !strings.Contains(err.Error(), "needs a fleet-ready anchor") {
		t.Fatalf("error should name the missing anchor, got %q", err)
	}

	// An unparseable timestamp cannot anchor anything either.
	cfg = writeCeiling(t, "not-a-timestamp")
	if _, _, err = resolveReportWindow(windowAuto, cfg); err == nil || !strings.Contains(err.Error(), "not RFC3339") {
		t.Fatalf("a bad timestamp must be an error naming the problem, got %v", err)
	}
}

// fleet-ready.json is what decouples the window anchor from the P0.2
// measurement: provisioning moved to kaps-bench, so a run has a fleet whose
// ready time is known even though no node-ceiling measurement was taken.
func TestResolveReportWindowPrefersFleetReadyArtifact(t *testing.T) {
	dir := t.TempDir()
	readyAt := time.Now().UTC().Add(-40 * time.Minute).Format(time.RFC3339)
	body := `{"ready_at_utc":"` + readyAt + `","ready_nodes":1000}`
	if err := os.WriteFile(filepath.Join(dir, "fleet-ready.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, note, err := resolveReportWindow(windowAuto, Config{ResultsDir: dir})
	if err != nil {
		t.Fatalf("fleet-ready.json must anchor the window: %v", err)
	}
	if got != "42m" && got != "43m" {
		t.Fatalf("auto window = %q, want 42m or 43m", got)
	}
	if !strings.Contains(note, "1000 nodes") {
		t.Fatalf("note should name the fleet it anchored to, got %q", note)
	}

	// It also wins over a P0.2 record, so a re-report of a run that has both
	// describes the fleet the run measured rather than the one it ramped.
	stale := `{"target_nodes":25000,"ready_nodes":24998,"ready_at_utc":"` +
		time.Now().UTC().Add(-5*time.Hour).Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(dir, "p0.2-node-ceiling.json"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, note, err = resolveReportWindow(windowAuto, Config{ResultsDir: dir}); err != nil ||
		!strings.Contains(note, "1000 nodes") {
		t.Fatalf("fleet-ready.json must win over p0.2-node-ceiling.json, got %q (%v)", note, err)
	}
}

func TestPromDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "1m"},
		{90 * time.Second, "2m"}, // rounds up, never ends short of fleet-ready
		{15 * time.Minute, "15m"},
		{6 * time.Hour, "360m"},
	} {
		if got := promDuration(tc.in); got != tc.want {
			t.Fatalf("promDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestThousands(t *testing.T) {
	for in, want := range map[int]string{0: "0", 50: "50", 1000: "1,000", 50000: "50,000", 100000: "100,000", 1234567: "1,234,567"} {
		if got := thousands(in); got != want {
			t.Fatalf("thousands(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestGBStr(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{{0.42, "420 MB"}, {1.0, "1.0 GB"}, {64.94, "64.9 GB"}} {
		if got := gbStr(tc.in); got != tc.want {
			t.Fatalf("gbStr(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Memory and CPU fall back independently once cAdvisor is only partly
// available, and only the memory source may raise the "RSS, not working set"
// caveat. Mislabelling the mixed case either suppresses a caveat on an RSS
// figure or prints one over an accurate working-set figure.
func TestPromUsageSourceNamesMemoryAndCPUSeparately(t *testing.T) {
	for _, tc := range []struct {
		mem, cpu bool
		want     string
		memIsRSS bool
	}{
		{true, true, srcPromContainer, false},
		{false, false, srcPromProcess, true},
		{true, false, srcPromContainerMem, false},
		{false, true, srcPromProcessMem, true},
	} {
		got := promUsageSource(tc.mem, tc.cpu)
		if got != tc.want {
			t.Fatalf("promUsageSource(mem=%v, cpu=%v) = %q, want %q", tc.mem, tc.cpu, got, tc.want)
		}
		if memIsProcessRSS(got) != tc.memIsRSS {
			t.Fatalf("memIsProcessRSS(%q) = %v, want %v", got, memIsProcessRSS(got), tc.memIsRSS)
		}
	}
}

// Compound sources survive extra suffixes appended when only some pods of a
// component carry series.
func TestMemIsProcessRSSHandlesCompoundSources(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want bool
	}{
		{srcPromProcess + "+kubelet-summary", true},
		{srcPromContainer + "+kubelet-summary", false},
		{srcPromContainerMem + "+kubelet-summary", false},
		{"prometheus+kubelet", false},
		{"", false},
	} {
		if got := memIsProcessRSS(tc.src); got != tc.want {
			t.Fatalf("memIsProcessRSS(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
}

// The caveat must fire off the memory source only. A row with working-set
// memory and fallback CPU previously printed the RSS caveat over accurate data.
func TestAnyProcessSourceIgnoresCPUOnlyFallback(t *testing.T) {
	if anyProcessSource([]a1Component{{Name: "labeler", MemGB: 20, Source: srcPromContainerMem}}) {
		t.Fatal("CPU-only fallback must not raise the process-RSS memory caveat")
	}
	if !anyProcessSource([]a1Component{{Name: "labeler", MemGB: 13, Source: srcPromProcessMem}}) {
		t.Fatal("memory from process RSS must raise the caveat even when CPU is accurate")
	}
}

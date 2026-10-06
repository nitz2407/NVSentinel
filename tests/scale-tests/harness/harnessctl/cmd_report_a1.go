//go:build !injector

/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// A1 component sizing: the report shape asked for in NVSentinel#1705 "A1.
// Component sizing" — one row per fleet size carrying Control plane / MongoDB /
// NVSentinel components memory, the per-node and marginal cost, and the
// component ranking behind the totals.
//
// "Control plane" in #1705 is the sum of the MongoDB and component columns
// (17+4.6≈22, 43+11.4≈54, 85+27.5≈113, 170+64.9≈235), not apiserver or etcd
// memory — managed control planes do not expose those, and the arithmetic in
// the issue confirms the column is derived.
//
// #1705 quotes decimal units (235 GB over 100k nodes = 2.35 MB per node), so
// this view converts the harness's MiB totals to decimal GB/MB to stay
// directly comparable. Raw MiB is kept alongside for the KPI pipeline.

const bytesPerMiB = 1024 * 1024

// windowAuto asks report to derive its own PromQL lookback.
const windowAuto = "auto"

const (
	// autoWindowFloor keeps the lookback long enough to contain at least a few
	// 1m scrape points, so a report run moments after scale still sees series.
	autoWindowFloor = 6 * time.Minute
	// autoWindowCeil bounds a stale or wrong ready_at_utc from reaching back
	// over unrelated history.
	autoWindowCeil = 6 * time.Hour
	// autoWindowMargin covers the scrape interval and clock skew between the
	// harness host and the cluster.
	autoWindowMargin = 2 * time.Minute
)

// resolveReportWindow turns the --window flag into a concrete PromQL duration
// plus a note explaining the choice.
//
// Component CPU/memory are max_over_time peaks, so the lookback decides which
// fleet the numbers describe. A fixed default is the wrong tool: a run that
// tears down a large fleet and builds a smaller one leaves the old fleet's peaks
// inside any generous window, and a 10k fleet then reports more memory than the
// 50k fleet it replaced. "auto" instead spans only from the moment this run's
// fleet reached target size, which is what the caller actually means by "this
// run". An explicit duration always wins, for re-reporting a past window.
//
// With no anchor this refuses rather than picking a duration. Defaulting here
// produces the bug auto was written to prevent while looking like a successful
// report: the numbers are plausible, wrong, and attributed to this run.
func resolveReportWindow(flagValue string, cfg Config) (window, note string, err error) {
	if flagValue != windowAuto && flagValue != "" {
		return flagValue, fmt.Sprintf("Window %s was set explicitly.", flagValue), nil
	}
	readyAtUTC, readyNodes, source := fleetReadyAnchor(cfg.ResultsDir)
	if source == "" {
		return "", "", fmt.Errorf("--window %s needs a fleet-ready anchor and %s has none "+
			"(no fleet-ready.json, and no p0.2-node-ceiling.json carrying ready_at_utc). "+
			"Whoever provisions the fleet writes that anchor; without it a window cannot be tied to "+
			"this run's fleet and the peaks would describe whatever fleet preceded it. "+
			"Pass --window <duration> to report a window on purpose",
			windowAuto, cfg.ResultsDir)
	}
	readyAt, err := time.Parse(time.RFC3339, readyAtUTC)
	if err != nil {
		return "", "", fmt.Errorf("fleet-ready timestamp %q in %s/%s is not RFC3339: %w. "+
			"Pass --window <duration> to report a window on purpose",
			readyAtUTC, cfg.ResultsDir, source, err)
	}

	span := time.Since(readyAt) + autoWindowMargin
	clamped := ""
	if span < autoWindowFloor {
		span, clamped = autoWindowFloor, " (raised to the floor)"
	}
	if span > autoWindowCeil {
		span, clamped = autoWindowCeil, " (capped)"
	}
	window = promDuration(span)
	return window, fmt.Sprintf("Window %s was derived to start when the fleet reached %d nodes at %s%s, so peaks exclude any fleet this run replaced.",
		window, readyNodes, readyAtUTC, clamped), nil
}

// fleetReadyAnchor returns the moment this run's fleet became Ready, preferring
// the dedicated artifact and falling back to the P0.2 record that carried the
// same timestamp while harnessctl still built the fleet itself. The fallback is
// what keeps run folders from before provisioning moved out re-reportable.
func fleetReadyAnchor(dir string) (readyAtUTC string, readyNodes int, source string) {
	if a := loadFleetReady(dir); a != nil {
		return a.ReadyAtUTC, a.ReadyNodes, "fleet-ready.json"
	}
	if a := loadNodeCeiling(dir); a != nil && a.ReadyAtUTC != "" {
		return a.ReadyAtUTC, a.ReadyNodes, "p0.2-node-ceiling.json"
	}
	return "", 0, ""
}

// promDuration renders d as a whole-minute PromQL duration, rounding up so the
// window never ends just short of the moment the fleet became ready.
func promDuration(d time.Duration) string {
	mins := int(math.Ceil(d.Minutes()))
	if mins < 1 {
		mins = 1
	}
	return fmt.Sprintf("%dm", mins)
}

// mongoComponentName is the component whose memory #1705 breaks out of the
// NVSentinel column into its own.
const mongoComponentName = "mongodb"

func mibToGB(mib int64) float64 { return float64(mib) * bytesPerMiB / 1e9 }

func mibToMBPerNode(mib int64, nodes int) float64 {
	if nodes <= 0 {
		return 0
	}
	return float64(mib) * bytesPerMiB / 1e6 / float64(nodes)
}

// a1Component is one row of the ranking that explains a fleet's component total.
type a1Component struct {
	Name       string  `json:"name"`
	Pods       int     `json:"pods"`
	Replaced   int     `json:"replaced_pods,omitempty"`
	MemMi      int64   `json:"mem_mib"`
	MemGB      float64 `json:"mem_gb"`
	MBPerNode  float64 `json:"mb_per_node"`
	ShareOfNVS float64 `json:"share_of_components"`
	CPUCores   float64 `json:"cpu_cores"`
	Source     string  `json:"source,omitempty"`
}

// a1Point is one fleet size measured in one run — a row of the #1705 table.
//
// A point belongs to exactly one run and is never rewritten: it is written once
// into that run's own results folder. `harnessctl sizing curve` reads the points
// back out of those folders to build the multi-size curve, so a run's numbers
// stay immutable and attributable to RunID after the fact.
type a1Point struct {
	RunID          string           `json:"run_id,omitempty"`
	Nodes          int              `json:"nodes"`
	PodsPerNode    float64          `json:"pods_per_node"`
	ControlPlaneMi int64            `json:"control_plane_mib"`
	MongoMi        int64            `json:"mongodb_mib"`
	ComponentsMi   int64            `json:"components_mib"`
	ComponentsCPU  float64          `json:"components_cpu_cores"`
	ComponentMemMi map[string]int64 `json:"component_mem_mib,omitempty"`
	Window         string           `json:"window,omitempty"`
	MeasuredAt     string           `json:"measured_at_utc,omitempty"`
	Title          string           `json:"title,omitempty"`
}

func (p a1Point) controlPlaneGB() float64 { return mibToGB(p.ControlPlaneMi) }
func (p a1Point) mongoGB() float64        { return mibToGB(p.MongoMi) }
func (p a1Point) componentsGB() float64   { return mibToGB(p.ComponentsMi) }

// a1Sizing is the whole A1 section for ONE run: its measured point and the
// component ranking behind the totals.
//
// It deliberately holds no cross-run table. A report describes the run that
// produced it and nothing else, so re-reading an old folder can never show
// numbers that a later run introduced. The N-vs-memory curve is a separate
// artifact built by `harnessctl sizing curve`.
type a1Sizing struct {
	Heading        string        `json:"heading"`
	Glance         string        `json:"glance"`
	Current        a1Point       `json:"current"`
	MBPerNode      float64       `json:"control_plane_mb_per_node"`
	ComponentMBPer float64       `json:"components_mb_per_node"`
	Components     []a1Component `json:"components"`
	TopTwoShare    float64       `json:"top_two_share"`
	Caveats        []string      `json:"caveats,omitempty"`
}

// measured returns the components that actually ran. Components with no pods
// contribute nothing to the totals and are reported through the caveats, so
// neither the narrative nor the breakdown table should list them as rows.
func (a a1Sizing) measured() []a1Component {
	out := make([]a1Component, 0, len(a.Components))
	for _, c := range a.Components {
		if c.Pods > 0 {
			out = append(out, c)
		}
	}
	return out
}

// buildA1Sizing derives the A1 section from the fleet sizing already collected.
// runID labels the point so `harnessctl sizing curve` can attribute the row back
// to the folder it came from.
func buildA1Sizing(s fleetSizing, title, runID string) a1Sizing {
	a := a1Sizing{Heading: "Component sizing"}

	var mongoMi, compMi int64
	var compCPU float64
	mongoSource := ""
	memByName := map[string]int64{}
	for _, c := range s.Components {
		if !c.NVSentinel {
			continue
		}
		if c.Name == mongoComponentName {
			mongoMi += c.MemMi
			mongoSource = c.Source
			continue
		}
		compMi += c.MemMi
		compCPU += float64(c.CPUMilli) / 1000.0
		if c.Pods > 0 {
			memByName[c.Name] = c.MemMi
		}
		a.Components = append(a.Components, a1Component{
			Name:      c.Name,
			Pods:      c.Pods,
			Replaced:  c.Replaced,
			MemMi:     c.MemMi,
			MemGB:     mibToGB(c.MemMi),
			MBPerNode: mibToMBPerNode(c.MemMi, s.NKwok),
			CPUCores:  float64(c.CPUMilli) / 1000.0,
			Source:    c.Source,
		})
	}
	sort.Slice(a.Components, func(i, j int) bool {
		if a.Components[i].MemMi != a.Components[j].MemMi {
			return a.Components[i].MemMi > a.Components[j].MemMi
		}
		return a.Components[i].Name < a.Components[j].Name
	})
	for i := range a.Components {
		if compMi > 0 {
			a.Components[i].ShareOfNVS = float64(a.Components[i].MemMi) / float64(compMi)
		}
	}
	if len(a.Components) >= 2 && compMi > 0 {
		a.TopTwoShare = float64(a.Components[0].MemMi+a.Components[1].MemMi) / float64(compMi)
	}

	a.Current = a1Point{
		RunID:          runID,
		Nodes:          s.NKwok,
		PodsPerNode:    s.PodsPerKwokNode,
		ControlPlaneMi: mongoMi + compMi,
		MongoMi:        mongoMi,
		ComponentsMi:   compMi,
		ComponentsCPU:  compCPU,
		ComponentMemMi: memByName,
		Window:         s.Window,
		MeasuredAt:     time.Now().UTC().Format(time.RFC3339),
		Title:          title,
	}
	a.MBPerNode = mibToMBPerNode(a.Current.ControlPlaneMi, s.NKwok)
	a.ComponentMBPer = mibToMBPerNode(compMi, s.NKwok)
	a.Caveats = a1Caveats(s, a, mongoMi, mongoSource)
	a.Glance = a1Glance(a)
	return a
}

// ---- narrative -------------------------------------------------------------

// a1Glance writes the "Memory at a glance" paragraph in the same shape as #1705:
// per-node control-plane cost and the components that dominate it.
//
// Everything here describes this run alone. Fixed-vs-marginal cost is a property
// of several fleet sizes, not of one, so it is stated by `harnessctl sizing
// curve` instead — a single run cannot tell a fixed term from a per-node one.
func a1Glance(a a1Sizing) string {
	if a.Current.Nodes == 0 {
		return "No KWOK nodes in the fleet; component memory cannot be expressed per node."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "At %s nodes the control plane costs %s, about %.2f MB per node. ",
		thousands(a.Current.Nodes), gbStr(a.Current.controlPlaneGB()), a.MBPerNode)

	named := a.measured()
	if len(named) >= 2 {
		fmt.Fprintf(&b, "Two components are most of NVSentinel's own share: at %s nodes %s is %s and %s %s, together %.0f%% of the %s component total",
			thousands(a.Current.Nodes), named[0].Name, gbStr(named[0].MemGB),
			named[1].Name, gbStr(named[1].MemGB), a.TopTwoShare*100, gbStr(a.Current.componentsGB()))
		rest := named[2:]
		if len(rest) > 0 {
			var mids []string
			for _, c := range rest[:min(2, len(rest))] {
				mids = append(mids, fmt.Sprintf("%s is %s", c.Name, gbStr(c.MemGB)))
			}
			fmt.Fprintf(&b, "; %s", strings.Join(mids, ", "))
			if tail := rest[min(2, len(rest)):]; len(tail) > 0 {
				fmt.Fprintf(&b, ", and everything else under %s", gbStr(tail[0].MemGB))
			}
		}
		b.WriteString(". ")
	} else if len(named) == 1 {
		fmt.Fprintf(&b, "Only %s reported memory, at %s. ", named[0].Name, gbStr(named[0].MemGB))
	}

	// The single-fleet-size limitation is stated once, as a caveat below the
	// table; repeating it here and again in a trailing note made the same point
	// three times in one section.
	return b.String()
}

// a1Caveats flags the conditions that make an A1 row read low or otherwise not
// comparable, so a reader never takes a missing component for a cheap one.
func a1Caveats(s fleetSizing, a a1Sizing, mongoMi int64, mongoSource string) []string {
	var out []string
	var absent []string
	for _, c := range s.Components {
		if c.NVSentinel && c.Pods == 0 {
			absent = append(absent, c.Name)
		}
	}
	if len(absent) > 0 {
		out = append(out, fmt.Sprintf("Not deployed in this run, so contributing 0 to the totals: %s. #1705 attributes a large share of its component column to kubernetes-object-monitor, so a row missing it is not comparable.",
			strings.Join(absent, ", ")))
	}
	if mongoMi == 0 {
		out = append(out, "MongoDB memory is 0: no mongodb pod was measured. The MongoDB and Control plane columns are therefore floors, not totals.")
	} else if isSampledSource(mongoSource) {
		// "prometheus+kubelet" means only some mongo pods had series and the
		// rest were point-sampled, which is still a lower bound — so match on
		// the presence of a sampled source, not on the absence of a prefix.
		out = append(out, fmt.Sprintf("MongoDB memory came from %q, so at least one pod was an instantaneous sample rather than a peak over the window. A crash-looping mongodb metrics sidecar produces exactly this, and leaves the figure at process RSS rather than the mongod working set. Treat the MongoDB and Control plane columns as lower bounds.", defStr(mongoSource, "a live sample")))
	}
	if s.PodsPerKwokNode == 0 {
		out = append(out, "P=0 pods per KWOK node, so this row is the cost of watching N Node objects only and carries no pod-driven term.")
	}
	var sampled []string
	for _, c := range a.measured() {
		if isSampledSource(c.Source) {
			sampled = append(sampled, c.Name)
		}
	}
	if len(sampled) > 0 {
		out = append(out, fmt.Sprintf("Measured from a single live kubelet/metrics-server sample rather than a windowed peak, so understating the run's maximum: %s.",
			strings.Join(sampled, ", ")))
	}
	// A rollout during the window leaves the old and new pods both carrying
	// series. Their usage is excluded (dropReplacedPods), which keeps a
	// component from reading as roughly double, but it also means the pods that
	// remain were measured over only part of the window — so these rows are
	// floors if a component's true peak predated its current pods.
	//
	// This used to flag `Pods > 1` and skip platform-connectors by name, which
	// inverted the problem: the DaemonSet whose pod count is legitimately large
	// was the one component that could silently double, and it was the largest
	// row in the table.
	var rolled []string
	for _, c := range a.measured() {
		if c.Replaced > 0 {
			rolled = append(rolled, fmt.Sprintf("%s (%d)", c.Name, c.Replaced))
		}
	}
	if len(rolled) > 0 {
		out = append(out, fmt.Sprintf("Pods were replaced during the window and are excluded from the totals, so these rows cover only the part of the window their current pods existed for and are floors: %s. A window starting after the rollout settled measures them in full.",
			strings.Join(rolled, ", ")))
	}
	if anyProcessSource(a.measured()) {
		out = append(out, "Memory is process resident set size, not container working set: no cAdvisor series are scraped here. RSS understates what the container is charged for, and for a process that exports no Prometheus metrics of its own (mongod) the figure is its sidecar exporter's RSS rather than the database. Cross-check the large rows against `kubectl top pod --containers`.")
	}
	out = append(out, fmt.Sprintf("One fleet size (N=%s) measured in this run, so per-node cost here cannot be separated into fixed and marginal terms. `harnessctl sizing curve --runs <dir>` aggregates several run folders into that curve.",
		thousands(a.Current.Nodes)))
	return out
}

// isSampledSource reports whether any part of a component's memory came from a
// point-in-time kubelet or metrics-server read instead of a windowed Prometheus
// peak. Sources can be compound ("prometheus+kubelet") when only some pods of a
// component are scraped, and those are lower bounds too.
func isSampledSource(source string) bool {
	return strings.Contains(source, "kubelet") || strings.Contains(source, "metrics-server")
}

// anyProcessSource reports whether any component's memory came from
// process_resident_memory_bytes, which is what the report falls back to when
// cAdvisor's container_memory_working_set_bytes is not scraped.
func anyProcessSource(components []a1Component) bool {
	for _, c := range components {
		if memIsProcessRSS(c.Source) {
			return true
		}
	}
	return false
}

// memIsProcessRSS reports whether a component's MEMORY figure came from
// process_resident_memory_bytes rather than cAdvisor's container working set.
//
// Memory and CPU fall back independently, so this matches the memory marker
// rather than the mere presence of "process": a row measured as
// "prometheus-container-mem+process-cpu" has an accurate working-set figure and
// must not carry the caveat, while "prometheus-process-mem+container-cpu" must.
func memIsProcessRSS(source string) bool {
	if strings.Contains(source, "container-mem") {
		return false
	}
	return strings.Contains(source, "process")
}

// shareStr keeps a small-but-real contributor from rounding away to "0%".
func shareStr(share float64) string {
	if share > 0 && share < 0.01 {
		return "<1%"
	}
	return fmt.Sprintf("%.0f%%", share*100)
}

func gbStr(gb float64) string {
	if math.Abs(gb) < 1 {
		return fmt.Sprintf("%.0f MB", gb*1000)
	}
	return fmt.Sprintf("%.1f GB", gb)
}

// thousands renders 50000 as "50,000" to match the table in #1705.
func thousands(n int) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	return strings.Join(append([]string{s}, parts...), ",")
}

// ---- markdown --------------------------------------------------------------

func renderA1(b *strings.Builder, d reportData) {
	a := d.A1
	if a.Current.Nodes == 0 && len(a.Components) == 0 {
		return
	}
	fmt.Fprintf(b, "## %s\n\n", defStr(a.Heading, "Component sizing"))
	b.WriteString("How much CPU and memory NVSentinel needs for a fleet of N nodes with P pods per node:\n\n")
	fmt.Fprintf(b, "%s\n\n", a.Glance)

	b.WriteString("| Nodes | Control plane | MongoDB | NVSentinel components |\n")
	b.WriteString("|-------|---------------|---------|-----------------------|\n")
	fmt.Fprintf(b, "| %s | %s | %s | %s |\n\n",
		thousands(a.Current.Nodes), gbStr(a.Current.controlPlaneGB()),
		gbStr(a.Current.mongoGB()), gbStr(a.Current.componentsGB()))

	fmt.Fprintf(b, "Component breakdown at N=%s, P=%.2f (window %s):\n\n",
		thousands(a.Current.Nodes), a.Current.PodsPerNode, defStr(a.Current.Window, d.Window))
	b.WriteString("| Component | Pods | Memory | Share | MB / node | CPU (cores) | Source |\n")
	b.WriteString("|-----------|------|--------|-------|-----------|-------------|--------|\n")
	for _, c := range a.measured() {
		fmt.Fprintf(b, "| %s | %d | %s | %s | %.3f | %.2f | %s |\n",
			c.Name, c.Pods, gbStr(c.MemGB), shareStr(c.ShareOfNVS), c.MBPerNode, c.CPUCores, defStr(c.Source, "n/a"))
	}
	fmt.Fprintf(b, "| **NVSentinel components** | | **%s** | 100%% | **%.3f** | **%.2f** | |\n",
		gbStr(a.Current.componentsGB()), a.ComponentMBPer, a.Current.ComponentsCPU)
	fmt.Fprintf(b, "| MongoDB | | %s | | %.3f | | |\n",
		gbStr(a.Current.mongoGB()), mibToMBPerNode(a.Current.MongoMi, a.Current.Nodes))
	fmt.Fprintf(b, "| **Control plane (MongoDB + components)** | | **%s** | | **%.3f** | | |\n\n",
		gbStr(a.Current.controlPlaneGB()), a.MBPerNode)

	if len(a.Caveats) > 0 {
		b.WriteString("Read with:\n\n")
		for _, c := range a.Caveats {
			fmt.Fprintf(b, "- %s\n", c)
		}
		b.WriteString("\n")
	}
}

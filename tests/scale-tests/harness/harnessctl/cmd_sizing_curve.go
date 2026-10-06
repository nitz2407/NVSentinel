//go:build !injector

/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Cross-run fleet-sizing curve: the multi-size table NVSentinel#1705 asks for,
// assembled by READING the per-run report folders rather than by accumulating
// into a shared file.
//
// Why aggregation is a separate command. One run measures exactly one fleet
// size, so a curve necessarily spans runs. The earlier design merged each run
// into a mutable sizing-history.json keyed on (N, P), which had three failure
// modes worth not repeating: a re-run whose P drifted slightly appended a
// duplicate row instead of replacing one; a later run at the same (N, P)
// silently overwrote an earlier measurement, so a bad run landing last
// destroyed a good one; and a row carried no run id, so it could not be traced
// back to the folder that produced it or dropped without hand-editing shared
// state. Deriving the curve instead keeps every run's numbers immutable and
// attributable, and makes the curve reproducible from the folders at any time.

// sizingCurveRow is one run's measured point plus where it was read from.
type sizingCurveRow struct {
	a1Point
	Folder string `json:"folder"`
	// UsedInFit records whether this row fed the scaling fit. Every run is
	// listed, including several at the same N, but a fit needs one value per N.
	UsedInFit bool `json:"used_in_fit"`
}

// sizingCurve is the derived artifact: every run found, and the fit over them.
type sizingCurve struct {
	RunsDir string           `json:"runs_dir"`
	Rows    []sizingCurveRow `json:"rows"`
	Fits    []a1Fit          `json:"fits,omitempty"`
	Notes   []string         `json:"notes,omitempty"`
}

// runSizingCurve builds the N-vs-memory curve from per-run report folders.
//
// It only reads the run folders. The curve is written next to them as a derived
// file that can be deleted and regenerated, so no run's own results are ever
// touched.
func runSizingCurve(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("sizing curve", flag.ExitOnError)
	runs := fs.String("runs", ".", "directory holding one sub-directory per run (each with a1-component-sizing.json)")
	out := fs.String("out", "", "Markdown output path (default: <runs>/sizing-curve.md)")
	outJSON := fs.String("json", "", "JSON output path (default: <runs>/sizing-curve.json)")
	_ = fs.Parse(args)

	rows, skipped, err := loadSizingRows(*runs)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("no sizing points under %s: expected <runs>/*/a1-component-sizing.json written by `harnessctl stack report`", *runs)
	}

	curve := sizingCurve{RunsDir: absOrSelf(*runs), Rows: rows}
	fitPoints := markFitRows(curve.Rows)
	curve.Fits = a1Fits(fitPoints)
	curve.Notes = sizingCurveNotes(curve.Rows, fitPoints, skipped)

	mdPath := defStr(*out, filepath.Join(*runs, "sizing-curve.md"))
	jsonPath := defStr(*outJSON, filepath.Join(*runs, "sizing-curve.json"))

	if err := os.WriteFile(mdPath, []byte(renderSizingCurve(curve)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", mdPath, err)
	}
	b, err := json.MarshalIndent(curve, "", "  ")
	if err != nil {
		return fmt.Errorf("encode curve: %w", err)
	}
	if err := os.WriteFile(jsonPath, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", jsonPath, err)
	}
	infof("sizing curve: %d run(s), %d fleet size(s) -> %s", len(curve.Rows), countDistinctN(curve.Rows), mdPath)
	return nil
}

// loadSizingRows reads every run folder's A1 artifact under dir. A folder that
// has no artifact yet (a run that failed before report) is skipped by name
// rather than silently, so a missing rung is visible in the output.
func loadSizingRows(dir string) (rows []sizingCurveRow, skipped []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read runs dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "a1-component-sizing.json")
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			if !os.IsNotExist(readErr) {
				warnf("sizing curve: %s: %v", path, readErr)
			}
			skipped = append(skipped, e.Name())
			continue
		}
		var a a1Sizing
		if jsonErr := json.Unmarshal(raw, &a); jsonErr != nil {
			warnf("sizing curve: %s unreadable: %v", path, jsonErr)
			skipped = append(skipped, e.Name())
			continue
		}
		if a.Current.Nodes <= 0 {
			skipped = append(skipped, e.Name())
			continue
		}
		p := a.Current
		if p.RunID == "" {
			// Reports written before run ids were stamped: the folder name is
			// the run id under kaps-bench's <output-root>/<run-id> layout.
			p.RunID = e.Name()
		}
		rows = append(rows, sizingCurveRow{a1Point: p, Folder: e.Name()})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Nodes != rows[j].Nodes {
			return rows[i].Nodes < rows[j].Nodes
		}
		return rows[i].MeasuredAt < rows[j].MeasuredAt
	})
	sort.Strings(skipped)
	return rows, skipped, nil
}

// markFitRows picks the row that represents each fleet size and flags it.
//
// Every run stays listed, but a scaling fit needs one value per N, so the most
// recent measurement at each N wins — and because the choice is recorded on the
// row and restated in the notes, a reader can see which run the slope came from
// instead of having to trust it.
func markFitRows(rows []sizingCurveRow) []a1Point {
	newest := map[int]int{}
	for i, r := range rows {
		if j, seen := newest[r.Nodes]; !seen || rows[i].MeasuredAt >= rows[j].MeasuredAt {
			newest[r.Nodes] = i
		}
	}
	idx := make([]int, 0, len(newest))
	for _, i := range newest {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	pts := make([]a1Point, 0, len(idx))
	for _, i := range idx {
		rows[i].UsedInFit = true
		pts = append(pts, rows[i].a1Point)
	}
	return pts
}

func sizingCurveNotes(rows []sizingCurveRow, fitPoints []a1Point, skipped []string) []string {
	var notes []string

	var superseded []string
	for _, r := range rows {
		if !r.UsedInFit {
			superseded = append(superseded, fmt.Sprintf("%s (N=%s, measured %s)", r.RunID, thousands(r.Nodes), defStr(r.MeasuredAt, "unknown")))
		}
	}
	if len(superseded) > 0 {
		notes = append(notes, fmt.Sprintf("More than one run measured the same fleet size. Every run is listed, but the fit used the most recent per size; not fitted: %s.",
			strings.Join(superseded, "; ")))
	}
	if len(fitPoints) < 2 {
		notes = append(notes, "One fleet size only, so there is no curve yet: a slope needs a second size and a fixed term needs a third.")
	} else if len(fitPoints) == 2 {
		notes = append(notes, "Two fleet sizes determine a line exactly, so the marginal rate is sound but the fixed term is not yet evidence. A third size separates them.")
	}

	// P is part of the answer #1705 asks for (N nodes x P pods/node), so a curve
	// whose rows disagree about P is not a clean single-variable sweep.
	ps := map[string]bool{}
	for _, p := range fitPoints {
		ps[fmt.Sprintf("%.2f", p.PodsPerNode)] = true
	}
	if len(ps) > 1 {
		var list []string
		for _, p := range fitPoints {
			list = append(list, fmt.Sprintf("N=%s P=%.2f", thousands(p.Nodes), p.PodsPerNode))
		}
		notes = append(notes, fmt.Sprintf("P differs across the fitted sizes (%s), so this curve varies N and P together and the slope cannot be read as a pure per-node cost.",
			strings.Join(list, ", ")))
	}
	if len(skipped) > 0 {
		notes = append(notes, fmt.Sprintf("Folders with no sizing point, so contributing no row (a run that failed before `stack report` looks like this): %s.",
			strings.Join(skipped, ", ")))
	}
	return notes
}

func countDistinctN(rows []sizingCurveRow) int {
	seen := map[int]bool{}
	for _, r := range rows {
		seen[r.Nodes] = true
	}
	return len(seen)
}

func absOrSelf(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// renderSizingCurve writes the curve as Markdown in #1705's shape.
func renderSizingCurve(c sizingCurve) string {
	var b strings.Builder
	b.WriteString("# NVSentinel fleet-sizing curve (N nodes x P pods/node)\n\n")
	fmt.Fprintf(&b, "> Derived from %d run folder(s) under `%s` by `harnessctl sizing curve`. Each row is one run, read from that run's `a1-component-sizing.json`; nothing is merged into shared state, so this file can be deleted and rebuilt at any time.\n\n", len(c.Rows), c.RunsDir)

	b.WriteString("| Nodes | P | Control plane | MongoDB | NVSentinel components | Components CPU | Window | Measured (UTC) | Run |\n")
	b.WriteString("|-------|---|---------------|---------|-----------------------|----------------|--------|----------------|-----|\n")
	for _, r := range c.Rows {
		fit := ""
		if !r.UsedInFit {
			fit = " (not fitted)"
		}
		fmt.Fprintf(&b, "| %s | %.2f | %s | %s | %s | %.2f | %s | %s | `%s`%s |\n",
			thousands(r.Nodes), r.PodsPerNode, gbStr(r.controlPlaneGB()), gbStr(r.mongoGB()),
			gbStr(r.componentsGB()), r.ComponentsCPU, defStr(r.Window, "n/a"),
			defStr(r.MeasuredAt, "n/a"), r.RunID, fit)
	}
	b.WriteString("\n")

	if len(c.Fits) > 0 {
		b.WriteString("Scaling across the fitted sizes (least-squares fixed term + slope, and the endpoint-to-endpoint marginal rate):\n\n")
		b.WriteString("| Series | Fixed term | Slope | Marginal |\n|--------|-----------|-------|----------|\n")
		for _, f := range c.Fits {
			var fixed string
			switch {
			case !f.Separable:
				fixed = fmt.Sprintf("not separable (%d sizes)", f.Points)
			case f.FixedMeaningful:
				fixed = gbStr(f.FixedGB)
			case f.SuperLinear:
				fixed = fmt.Sprintf("none; super-linear (%s)", gbStr(f.FixedGB))
			default:
				fixed = "none meaningful"
			}
			fmt.Fprintf(&b, "| %s | %s | %.2f GB / 1k nodes | %.2f GB / 1k nodes (%s->%s) |\n",
				f.Series, fixed, f.SlopeGBPerK, f.MarginalGBPerK, thousands(f.FromNodes), thousands(f.ToNodes))
		}
		b.WriteString("\n")
	}

	if per := perComponentCurve(c.Rows); per != "" {
		b.WriteString("Per-component memory (MiB) across the fitted sizes:\n\n")
		b.WriteString(per)
	}

	if len(c.Notes) > 0 {
		b.WriteString("Read with:\n\n")
		for _, n := range c.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
		b.WriteString("\n")
	}
	b.WriteString("Each row's own report, including its caveats and measurement sources, is in that run's folder — read it before quoting the row.\n")
	return b.String()
}

// perComponentCurve shows how each component grows with N, which is what turns
// "components cost X" into "labeler is the thing that scales".
func perComponentCurve(rows []sizingCurveRow) string {
	var fitted []sizingCurveRow
	for _, r := range rows {
		if r.UsedInFit && len(r.ComponentMemMi) > 0 {
			fitted = append(fitted, r)
		}
	}
	if len(fitted) < 2 {
		return ""
	}
	names := map[string]bool{}
	for _, r := range fitted {
		for n := range r.ComponentMemMi {
			names[n] = true
		}
	}
	var sorted []string
	for n := range names {
		sorted = append(sorted, n)
	}
	// Rank by the largest fleet's footprint: the reader wants the component that
	// dominates at scale first, not alphabetical order.
	largest := fitted[len(fitted)-1].ComponentMemMi
	sort.Slice(sorted, func(i, j int) bool {
		if largest[sorted[i]] != largest[sorted[j]] {
			return largest[sorted[i]] > largest[sorted[j]]
		}
		return sorted[i] < sorted[j]
	})

	var b strings.Builder
	b.WriteString("| Component |")
	for _, r := range fitted {
		fmt.Fprintf(&b, " %s |", thousands(r.Nodes))
	}
	b.WriteString("\n|-----------|")
	for range fitted {
		b.WriteString("------|")
	}
	b.WriteString("\n")
	for _, n := range sorted {
		fmt.Fprintf(&b, "| %s |", n)
		for _, r := range fitted {
			fmt.Fprintf(&b, " %d |", r.ComponentMemMi[n])
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

// ---- scaling fit -----------------------------------------------------------

// a1Fit is the scaling law across two or more fleet sizes: a least-squares line
// plus the endpoint-to-endpoint marginal rate #1705 quotes ("0.71 GB per 1,000
// nodes" from 25k to 100k).
//
// Two fleet sizes determine a line exactly, so its intercept carries no
// evidence about a fixed cost; only three or more sizes can separate a fixed
// term from the marginal one. FixedMeaningful and SuperLinear therefore stay
// false until then, and a negative intercept is reported as growth outpacing
// fleet size rather than as a negative fixed cost.
type a1Fit struct {
	Series          string  `json:"series"`
	Points          int     `json:"points"`
	FromNodes       int     `json:"from_nodes"`
	ToNodes         int     `json:"to_nodes"`
	MarginalGBPerK  float64 `json:"marginal_gb_per_1k_nodes"`
	SlopeGBPerK     float64 `json:"lsq_slope_gb_per_1k_nodes"`
	FixedGB         float64 `json:"lsq_fixed_gb"`
	FixedMeaningful bool    `json:"lsq_fixed_meaningful"`
	SuperLinear     bool    `json:"super_linear"`
	Separable       bool    `json:"fixed_and_marginal_separable"`
}

// a1Fits fits each memory series against fleet size. Callers pass one point per
// fleet size; duplicates at the same N would weight that size twice.
func a1Fits(points []a1Point) []a1Fit {
	byN := map[int]a1Point{}
	var ns []int
	for _, p := range points {
		if p.Nodes <= 0 {
			continue
		}
		if _, seen := byN[p.Nodes]; !seen {
			ns = append(ns, p.Nodes)
		}
		byN[p.Nodes] = p
	}
	if len(ns) < 2 {
		return nil
	}
	sort.Ints(ns)

	series := []struct {
		name string
		get  func(a1Point) float64
	}{
		{"control_plane", a1Point.controlPlaneGB},
		{"mongodb", a1Point.mongoGB},
		{"nvsentinel_components", a1Point.componentsGB},
	}
	var out []a1Fit
	for _, sr := range series {
		xs := make([]float64, 0, len(ns))
		ys := make([]float64, 0, len(ns))
		for _, n := range ns {
			xs = append(xs, float64(n))
			ys = append(ys, sr.get(byN[n]))
		}
		slope, fixed := leastSquares(xs, ys)
		lo, hi := ns[0], ns[len(ns)-1]
		marginal := (sr.get(byN[hi]) - sr.get(byN[lo])) / (float64(hi-lo) / 1000.0)
		atHi := sr.get(byN[hi])
		separable := len(ns) >= 3
		significant := separable && atHi > 0 && math.Abs(fixed)/atHi > 0.05
		out = append(out, a1Fit{
			Series:          sr.name,
			Points:          len(ns),
			FromNodes:       lo,
			ToNodes:         hi,
			MarginalGBPerK:  marginal,
			SlopeGBPerK:     slope * 1000,
			FixedGB:         fixed,
			FixedMeaningful: significant && fixed > 0,
			SuperLinear:     significant && fixed < 0,
			Separable:       separable,
		})
	}
	return out
}

// fitFor picks one series out of a fit set, so callers can speak about
// component growth without re-deriving it.
func fitFor(fits []a1Fit, series string) *a1Fit {
	for i := range fits {
		if fits[i].Series == series {
			return &fits[i]
		}
	}
	return nil
}

func leastSquares(xs, ys []float64) (slope, intercept float64) {
	n := float64(len(xs))
	if n < 2 {
		return 0, 0
	}
	var sx, sy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
	}
	mx, my := sx/n, sy/n
	var num, den float64
	for i := range xs {
		num += (xs[i] - mx) * (ys[i] - my)
		den += (xs[i] - mx) * (xs[i] - mx)
	}
	if den == 0 {
		return 0, my
	}
	slope = num / den
	return slope, my - slope*mx
}

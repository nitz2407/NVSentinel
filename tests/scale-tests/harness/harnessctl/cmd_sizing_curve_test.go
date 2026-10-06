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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRunFolder fakes what `harnessctl stack report` leaves behind: one folder
// per run holding that run's own sizing point.
func writeRunFolder(t *testing.T, root, name string, p a1Point) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(a1Sizing{Heading: "A1. Component sizing", Current: p})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a1-component-sizing.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func point(runID string, nodes int, compGB, mongoGB float64, measuredAt string) a1Point {
	return a1Point{
		RunID:          runID,
		Nodes:          nodes,
		PodsPerNode:    1,
		ComponentsMi:   gbToMiB(compGB),
		MongoMi:        gbToMiB(mongoGB),
		ControlPlaneMi: gbToMiB(compGB) + gbToMiB(mongoGB),
		ComponentMemMi: map[string]int64{"labeler": gbToMiB(compGB * 0.8), "janitor": gbToMiB(compGB * 0.2)},
		Window:         "15m",
		MeasuredAt:     measuredAt,
	}
}

// The curve is the whole reason runs stay separate: it must assemble N-vs-memory
// from the folders without touching them.
func TestSizingCurveAggregatesRunFoldersReadOnly(t *testing.T) {
	root := t.TempDir()
	writeRunFolder(t, root, "run-10k", point("run-10k", 10000, 5.7, 6.9, "2026-09-15T10:16:16Z"))
	writeRunFolder(t, root, "run-25k", point("run-25k", 25000, 7.3, 7.0, "2026-09-15T16:07:40Z"))
	writeRunFolder(t, root, "run-50k", point("run-50k", 50000, 12.9, 7.3, "2026-09-15T16:38:16Z"))

	artifact := filepath.Join(root, "run-10k", "a1-component-sizing.json")
	before, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}

	if err := runSizingCurve(context.Background(), []string{"--runs", root}); err != nil {
		t.Fatalf("sizing curve: %v", err)
	}

	after, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("aggregation must not rewrite a run's own results")
	}

	md, err := os.ReadFile(filepath.Join(root, "sizing-curve.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| 10,000 |", "| 25,000 |", "| 50,000 |",
		"`run-10k`", "`run-50k`", // every row is attributable to its folder
		"| Series | Fixed term | Slope | Marginal |",
		"nvsentinel_components",
		"labeler", // per-component growth across sizes
	} {
		if !strings.Contains(string(md), want) {
			t.Fatalf("curve missing %q, got:\n%s", want, md)
		}
	}

	var c sizingCurve
	raw, err := os.ReadFile(filepath.Join(root, "sizing-curve.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 3 {
		t.Fatalf("expected one row per run, got %d", len(c.Rows))
	}
	comp := fitFor(c.Fits, "nvsentinel_components")
	if comp == nil {
		t.Fatal("three sizes should yield a components fit")
	}
	nearly(t, "components marginal GB/1k", comp.MarginalGBPerK, (12.9-5.7)/40, 0.05)
	if !comp.Separable {
		t.Fatal("three fleet sizes must separate fixed from marginal cost")
	}
}

// Two runs at the same fleet size are both real measurements, so both are shown
// — the old merge silently destroyed the earlier one. Only the newest feeds the
// fit, and the report has to say which.
func TestSizingCurveShowsDuplicateSizesAndFitsNewest(t *testing.T) {
	root := t.TempDir()
	writeRunFolder(t, root, "run-50k-old", point("run-50k-old", 50000, 17.1, 7.0, "2026-09-14T09:00:00Z"))
	writeRunFolder(t, root, "run-50k-new", point("run-50k-new", 50000, 12.9, 7.3, "2026-09-15T16:38:16Z"))
	writeRunFolder(t, root, "run-10k", point("run-10k", 10000, 5.7, 6.9, "2026-09-15T15:46:16Z"))
	// A run that died before `stack report` leaves a folder with no point.
	if err := os.MkdirAll(filepath.Join(root, "run-failed"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := runSizingCurve(context.Background(), []string{"--runs", root}); err != nil {
		t.Fatalf("sizing curve: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(root, "sizing-curve.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c sizingCurve
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 3 {
		t.Fatalf("both 50k runs must survive as their own rows, got %d", len(c.Rows))
	}

	fitted := map[string]bool{}
	for _, r := range c.Rows {
		fitted[r.RunID] = r.UsedInFit
	}
	if fitted["run-50k-old"] {
		t.Fatal("the superseded 50k run must not feed the fit")
	}
	if !fitted["run-50k-new"] || !fitted["run-10k"] {
		t.Fatalf("newest per size must feed the fit, got %+v", fitted)
	}

	notes := strings.Join(c.Notes, "\n")
	if !strings.Contains(notes, "run-50k-old") {
		t.Fatalf("notes must name the run that was not fitted, got:\n%s", notes)
	}
	if !strings.Contains(notes, "run-failed") {
		t.Fatalf("a folder with no sizing point must be called out, got:\n%s", notes)
	}

	md, err := os.ReadFile(filepath.Join(root, "sizing-curve.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "(not fitted)") {
		t.Fatalf("the duplicate row should be marked in the table, got:\n%s", md)
	}
}

// P is half the question #1705 asks (N nodes x P pods/node). A curve whose rows
// disagree about P is not a single-variable sweep, and saying so is the
// difference between a sizing law and a coincidence.
func TestSizingCurveFlagsMixedPodDensity(t *testing.T) {
	root := t.TempDir()
	a := point("run-10k", 10000, 5.7, 6.9, "2026-09-15T10:00:00Z")
	a.PodsPerNode = 2.07
	b := point("run-100k", 100000, 30.3, 8.7, "2026-09-15T17:00:46Z")
	b.PodsPerNode = 0
	writeRunFolder(t, root, "run-10k", a)
	writeRunFolder(t, root, "run-100k", b)

	if err := runSizingCurve(context.Background(), []string{"--runs", root}); err != nil {
		t.Fatalf("sizing curve: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "sizing-curve.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c sizingCurve
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(c.Notes, "\n")
	if !strings.Contains(notes, "P differs across the fitted sizes") {
		t.Fatalf("mixed P must be flagged, got:\n%s", notes)
	}
	if !strings.Contains(notes, "Two fleet sizes determine a line exactly") {
		t.Fatalf("two sizes must not imply a fixed term, got:\n%s", notes)
	}
}

// A folder with no reports at all is a user error worth naming, not an empty
// curve that looks like a measurement.
func TestSizingCurveErrorsWhenNoPointsFound(t *testing.T) {
	err := runSizingCurve(context.Background(), []string{"--runs", t.TempDir()})
	if err == nil {
		t.Fatal("expected an error when no run folder holds a sizing point")
	}
	if !strings.Contains(err.Error(), "a1-component-sizing.json") {
		t.Fatalf("error should say what it looked for, got %v", err)
	}
}

// Reports written before run ids were stamped must still be attributable, or
// the existing curve folders cannot be aggregated at all.
func TestSizingCurveFallsBackToFolderNameAsRunID(t *testing.T) {
	root := t.TempDir()
	p := point("", 25000, 7.3, 7.0, "2026-09-15T16:07:40Z")
	writeRunFolder(t, root, "01a0a497-legacy", p)
	writeRunFolder(t, root, "run-10k", point("run-10k", 10000, 5.7, 6.9, "2026-09-15T15:46:16Z"))

	rows, _, err := loadSizingRows(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Nodes == 25000 && r.RunID != "01a0a497-legacy" {
			t.Fatalf("legacy point should inherit its folder name, got %q", r.RunID)
		}
	}
}

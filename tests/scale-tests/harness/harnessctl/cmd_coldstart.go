/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/


//go:build !injector

package main

// Cold-start replay orchestration. Seed MongoDB with a large haystack of
// documents (a configurable mix of remediation-ready "needles" and STORE_ONLY
// "noise"), CountDocuments to verify the seed actually landed, then cold-start a
// consumer (node-drainer / fault-remediation / fault-quarantine) and measure how
// long its initial pre-change-stream scan/replay takes to reach Ready — the
// metric that exposes replay/scan cost at datastore scale. Seeding reuses the
// direct-Mongo injector (`inject -mechanism mongo -coldstart-ratio`), so no
// separate seeding path exists.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

type coldStartResult struct {
	Component   string  `json:"component"`
	Kind        string  `json:"kind"`
	RunID       string  `json:"run_id"`
	Seeded      int     `json:"seeded"`
	Stored      int     `json:"stored"` // MongoDB CountDocuments for the run label (verified after seed)
	Needles     int     `json:"needles"`
	Noise       int     `json:"noise"`
	ReadyDurSec float64 `json:"ready_duration_seconds"`
	Ready       bool    `json:"ready"`
	LogMatchSec float64 `json:"log_match_seconds,omitempty"`
	LogMatched  bool    `json:"log_matched"`
	Verdict     string  `json:"verdict"`
	Message     string  `json:"message"`
	GeneratedAt string  `json:"generated_at_utc"`
}

func runColdStart(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("events coldstart", flag.ExitOnError)
	cfg := defaultConfig()
	bindNvsNamespaceFlag(fs, &cfg)
	bindResultsFlag(fs, &cfg)
	bindMongoFlags(fs, &cfg)
	bindInjectorFlags(fs, &cfg)
	count := fs.Int("count", 100000, "documents to seed into the haystack")
	ratio := fs.Float64("remediation-ratio", 0.01, "fraction of seeded docs that are remediation-ready needles (rest are STORE_ONLY noise)")
	component := fs.String("component", "node-drainer", "consumer to cold-start (e.g. node-drainer, fault-remediation, fault-quarantine)")
	kind := fs.String("kind", "deployment", "workload kind of the consumer: deployment | statefulset")
	nodes := fs.Int("nodes", 0, "node-name spread for the seed (0 => live KWOK fleet)")
	workers := fs.Int("workers", 50, "direct-mongo InsertMany workers for seeding")
	batch := fs.Int("batch", 500, "direct-mongo InsertMany batch size for seeding")
	readyLog := fs.String("ready-log", "", "optional regex; if set, also measure time until it appears in the consumer log (e.g. 'change stream (opened|resumed)')")
	skipSeed := fs.Bool("skip-seed", false, "reuse docs already present in MongoDB; do not seed")
	seedOnly := fs.Bool("seed-only", false, "seed the haystack and exit without restarting the consumer")
	restartTO := fs.Duration("restart-timeout", 15*time.Minute, "how long to wait for the consumer to become Ready after restart")
	runID := fs.String("run-id", "", "correlation run id for the seed (default: random)")
	_ = fs.Parse(args)

	if *runID == "" {
		*runID = fmt.Sprintf("coldstart-%d-%s", time.Now().Unix(), randHex(4))
	}
	needles := int(float64(*count) * *ratio)

	c, err := newClients(cfg)
	if err != nil {
		return err
	}

	nodeCount := *nodes
	if nodeCount <= 0 {
		nodeCount = c.countKwokNodesOrZero(ctx)
	}
	if nodeCount <= 0 {
		nodeCount = 1
	}

	// 1) Seed the haystack via the direct-Mongo injector (one resident injector),
	// then CountDocuments in MongoDB for the run label so "seeded" is not just
	// InsertMany acks.
	stored := 0
	if !*skipSeed {
		if err := c.injectMongoAcrossPool(ctx, cfg, mongoDistOptions{
			total: *count, workers: *workers, batch: *batch,
			nodeCount: nodeCount, nodeOffset: 0, runID: *runID, coldstartRatio: *ratio,
		}); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
		var verr error
		stored, verr = c.verifyColdStartSeed(ctx, cfg, *runID, *count)
		if verr != nil {
			return fmt.Errorf("verify seed: %w", verr)
		}
	} else {
		infof("cold-start: -skip-seed set; using docs already present in MongoDB")
	}
	if *seedOnly {
		infof("cold-start: -seed-only set; haystack seeded+verified stored=%d/%d, not restarting %s/%s",
			stored, *count, *kind, *component)
		writeArtifact(cfg.ResultsDir, "coldstart-"+*component+".json", coldStartResult{
			Component: *component, Kind: *kind, RunID: *runID,
			Seeded: *count, Stored: stored, Needles: needles, Noise: *count - needles,
			Verdict: "PASS", Message: fmt.Sprintf("seed-only: verified MongoDB stored=%d/%d", stored, *count),
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		})
		return nil
	}

	// 2) Cold-start the target and time its initial scan.
	res := coldStartResult{Component: *component, Kind: *kind, RunID: *runID,
		Seeded: *count, Stored: stored, Needles: needles, Noise: *count - needles,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339)}

	t0, err := c.rolloutRestart(ctx, cfg.NVSNamespace, *kind, *component)
	if err != nil {
		return fmt.Errorf("restart %s/%s: %w", *kind, *component, err)
	}
	infof("cold-start: restarted %s/%s at %s; waiting for Ready (timeout %s)…",
		*kind, *component, t0.Format(time.RFC3339), *restartTO)

	// Optional log-marker scan, concurrent with the readiness wait.
	logDone := make(chan struct{})
	if *readyLog != "" {
		re, cerr := regexp.Compile(*readyLog)
		if cerr != nil {
			return fmt.Errorf("bad -ready-log regex: %w", cerr)
		}
		go func() {
			d, ok := c.waitPodLogRegex(ctx, cfg.NVSNamespace, *kind, *component, re, t0, *restartTO)
			res.LogMatchSec, res.LogMatched = d.Seconds(), ok
			close(logDone)
		}()
	} else {
		close(logDone)
	}

	ok, _ := c.waitRolloutComplete(ctx, cfg.NVSNamespace, *kind, *component, *restartTO)
	res.ReadyDurSec = time.Since(t0).Seconds()
	res.Ready = ok
	<-logDone

	if ok {
		res.Verdict = "PASS"
		res.Message = fmt.Sprintf("cold-started in %.1fs (scan over %d docs stored=%d, %d needles)",
			res.ReadyDurSec, res.Seeded, res.Stored, res.Needles)
	} else {
		res.Verdict = "FAIL"
		res.Message = fmt.Sprintf("did not become Ready within %s — initial scan/replay may be stuck at this seed size", *restartTO)
	}

	writeArtifact(cfg.ResultsDir, "coldstart-"+*component+".json", res)
	printColdStartSummary(res)
	if !ok {
		return fmt.Errorf("%s", res.Message)
	}
	return nil
}

// verifyColdStartSeed CountDocuments in MongoDB for the seed run label (via one
// resident injector) and fails unless stored == expect. This is the automatic
// check that InsertMany acks actually landed in the datastore.
func (c *clients) verifyColdStartSeed(ctx context.Context, cfg Config, runID string, expect int) (int, error) {
	stepf("cold-start: verify seeded docs in MongoDB (run-id=%s expect=%d)", runID, expect)
	geo, err := c.resolvePoolGeometry(ctx, cfg)
	if err != nil {
		return 0, err
	}
	conn, err := c.deriveMongoConn(ctx, cfg)
	if err != nil {
		return 0, err
	}
	rep, err := c.reconcileByCountPool(ctx, cfg, geo, conn, runID, expect)
	if err != nil {
		return 0, err
	}
	if rep.StoredForRun != expect {
		return rep.StoredForRun, fmt.Errorf("mongo stored_for_run=%d want=%d", rep.StoredForRun, expect)
	}
	infof("cold-start: verified MongoDB stored=%d/%d for run %s", rep.StoredForRun, expect, runID)
	return rep.StoredForRun, nil
}

func printColdStartSummary(r coldStartResult) {
	stepf("cold-start summary: %s (%s)", r.Component, r.Verdict)
	infof("  seeded=%d stored=%d needles=%d noise=%d", r.Seeded, r.Stored, r.Needles, r.Noise)
	infof("  ready=%v in %.1fs", r.Ready, r.ReadyDurSec)
	if r.LogMatched || r.LogMatchSec > 0 {
		infof("  ready-log matched=%v in %.1fs", r.LogMatched, r.LogMatchSec)
	}
	infof("  %s", r.Message)
}

// rolloutRestart bumps the workload's pod-template restart annotation (the same
// thing `kubectl rollout restart` does) and returns the restart instant.
func (c *clients) rolloutRestart(ctx context.Context, ns, kind, name string) (time.Time, error) {
	t0 := time.Now()
	patch := fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		t0.UTC().Format(time.RFC3339),
	)
	var err error
	if isStatefulKind(kind) {
		_, err = c.kube.AppsV1().StatefulSets(ns).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	} else {
		_, err = c.kube.AppsV1().Deployments(ns).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	}
	if err != nil {
		return t0, fmt.Errorf("rollout restart %s/%s: %w", kind, name, err)
	}
	return t0, nil
}

// waitRolloutComplete polls until the workload's updated generation is fully
// rolled out and Ready (or timeout). Returns whether it converged.
func (c *clients) waitRolloutComplete(ctx context.Context, ns, kind, name string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		if isStatefulKind(kind) {
			s, err := c.kube.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
			if err == nil && statefulRolled(s) {
				return true, nil
			}
		} else {
			d, err := c.kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
			if err == nil && deployRolled(d) {
				return true, nil
			}
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-tick.C:
		}
	}
}

func deployRolled(d *appsv1.Deployment) bool {
	if d.Status.ObservedGeneration < d.Generation {
		return false
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	return d.Status.UpdatedReplicas >= want && d.Status.AvailableReplicas >= want && d.Status.UnavailableReplicas == 0
}

func statefulRolled(s *appsv1.StatefulSet) bool {
	if s.Status.ObservedGeneration < s.Generation {
		return false
	}
	want := int32(1)
	if s.Spec.Replicas != nil {
		want = *s.Spec.Replicas
	}
	return s.Status.UpdatedReplicas >= want && s.Status.ReadyReplicas >= want && s.Status.CurrentRevision == s.Status.UpdateRevision
}

// waitPodLogRegex scans the workload's logs (since the restart instant) for a
// marker regex, returning how long it took and whether it matched.
func (c *clients) waitPodLogRegex(ctx context.Context, ns, kind, name string, re *regexp.Regexp, since time.Time, timeout time.Duration) (time.Duration, bool) {
	deadline := time.Now().Add(timeout)
	for {
		out, _ := c.workloadLogs(ctx, ns, kind, name, since)
		if re.MatchString(out) {
			return time.Since(since), true
		}
		if time.Now().After(deadline) {
			return time.Since(since), false
		}
		select {
		case <-ctx.Done():
			return time.Since(since), false
		case <-time.After(5 * time.Second):
		}
	}
}

// workloadLogs concatenates logs from pods owned by a Deployment or StatefulSet
// (equivalent to `kubectl logs deploy/name --since-time=…`).
func (c *clients) workloadLogs(ctx context.Context, ns, kind, name string, since time.Time) (string, error) {
	sel, err := c.workloadSelector(ctx, ns, kind, name)
	if err != nil {
		return "", err
	}
	pods, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	opts := &corev1.PodLogOptions{SinceTime: &metav1.Time{Time: since}}
	for i := range pods.Items {
		req := c.kube.CoreV1().Pods(ns).GetLogs(pods.Items[i].Name, opts)
		stream, err := req.Stream(ctx)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(stream)
		stream.Close()
		b.Write(data)
	}
	return b.String(), nil
}

func (c *clients) workloadSelector(ctx context.Context, ns, kind, name string) (string, error) {
	if isStatefulKind(kind) {
		s, err := c.kube.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		return labels.Set(s.Spec.Selector.MatchLabels).String(), nil
	}
	d, err := c.kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return labels.Set(d.Spec.Selector.MatchLabels).String(), nil
}

func isStatefulKind(kind string) bool {
	switch kind {
	case "statefulset", "statefulsets", "sts", "StatefulSet":
		return true
	default:
		return false
	}
}

/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/


//go:build !injector

package main

// Distributed injection + reconciliation via the resident per-node injector
// DaemonSet (slim multi-arch harness-inject image). This is the self-contained
// engine behind the P0.3 / P0.5 commands: no manual `kubectl exec`, `kubectl cp`,
// or port-forward. It ports phase0/40-parallel-inject.sh into the controller so a
// single `harnessctl` invocation fans out injection across every connector node
// in parallel and reconciles — with the operator doing nothing by hand.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// extractReport pulls the ReconcileReport JSON object out of merged pod logs.
func extractReport(logs string) *ReconcileReport {
	start := strings.Index(logs, "{")
	end := strings.LastIndex(logs, "}")
	if start < 0 || end <= start {
		return nil
	}
	var rep ReconcileReport
	if err := json.Unmarshal([]byte(logs[start:end+1]), &rep); err != nil {
		return nil
	}
	return &rep
}

// poolLedgerDir is the on-node hostPath (under the injector's /pool-sockets
// mount) where each connector shard's injection ledger is written, so a later
// per-ID + NodeName reconcile can read a real shard's ledger with no cross-node
// gather.
const poolLedgerDir = "/pool-sockets/ledgers"

// poolGeometry is the deployed pool's shard layout: how connectors map to nodes,
// which resident injector serves each node, and how many emulated nodes each
// connector represents (NPC).
type poolGeometry struct {
	npc       int
	byNode    map[string][]int // node -> connector StatefulSet ordinals
	injByNode map[string]string
	totalConn int
}

// injectOptions parameterizes a distributed injection run.
type injectOptions struct {
	count     int
	rate      float64
	fatalFrac float64
	runID     string
	// Event-generation knobs threaded to each resident injector so a distributed
	// gRPC run honors the operator's -pattern / -fatal-event / -processing-strategy
	// (or their HARNESS_* config defaults), not just fatal-fraction.
	fatalEvent   string
	pattern      string
	procStrategy string
}

var ackedRe = regexp.MustCompile(`acked=([0-9]+)`)

// injectAcrossPool is the operator-facing distributed injector behind
// `harnessctl inject` (no -socket): one invocation fans injection out through
// the resident injectors in parallel — each connector injects one event per
// emulated node it represents (count/connector = nodes-per-connector).
// Injection only; accounting is the separate `reconcile` command.
func (c *clients) injectAcrossPool(ctx context.Context, cfg Config, rate float64, runID string) error {
	stepf("P0.3 distributed inject across the connector pool")
	geo, err := c.resolvePoolGeometry(ctx, cfg)
	if err != nil {
		return err
	}
	expect := geo.totalConn * geo.npc
	infof("distributed inject: nodes=%d connectors=%d nodes/conn=%d expect=%d run-id=%s",
		len(geo.byNode), geo.totalConn, geo.npc, expect, runID)
	acked, err := c.injectViaInjectors(ctx, cfg, geo, injectOptions{
		count: geo.npc, rate: rate, fatalFrac: cfg.FatalFraction, runID: runID,
		fatalEvent: cfg.FatalEvent, pattern: cfg.Pattern, procStrategy: cfg.ProcessingStrategy,
	})
	if err != nil {
		return err
	}
	infof("[P0.3 inject] run-id=%s connectors=%d expect=%d acked=%d", runID, geo.totalConn, expect, acked)
	// Emit the run id last so callers can capture it for `reconcile -run-id`.
	fmt.Println(runID)
	return nil
}

// resolvePoolGeometry reads the deployed pool + resident injectors and returns
// the shard layout used to drive distributed injection.
func (c *clients) resolvePoolGeometry(ctx context.Context, cfg Config) (poolGeometry, error) {
	var g poolGeometry
	sts, err := c.kube.AppsV1().StatefulSets(cfg.NVSNamespace).Get(ctx, connectorPoolName, metav1.GetOptions{})
	if err != nil {
		return g, fmt.Errorf("connector pool %s not found (deploy it first): %w", connectorPoolName, err)
	}
	g.npc = 1
	if v := sts.Annotations["nvs-harness/nodes-per-connector"]; v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			g.npc = n
		}
	}

	inj, err := c.injectorPodsByNode(ctx, cfg.NVSNamespace)
	if err != nil {
		return g, err
	}
	g.injByNode = inj

	pods, err := c.kube.CoreV1().Pods(cfg.NVSNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: connectorPoolLabel + "=" + connectorPoolName,
	})
	if err != nil {
		return g, err
	}
	g.byNode = map[string][]int{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning || p.Spec.NodeName == "" {
			continue
		}
		ord, err := strconv.Atoi(p.Name[strings.LastIndex(p.Name, "-")+1:])
		if err != nil {
			continue
		}
		g.byNode[p.Spec.NodeName] = append(g.byNode[p.Spec.NodeName], ord)
		g.totalConn++
	}
	if g.totalConn == 0 {
		return g, fmt.Errorf("no Running pool pods found for %s=%s", connectorPoolLabel, connectorPoolName)
	}
	for n := range g.byNode {
		sort.Ints(g.byNode[n])
	}
	return g, nil
}

// injectViaInjectors fans out injection across every connector node in parallel:
// each node's resident injector drives a disjoint node shard into every connector
// socket present on that node. Returns the total acknowledged events.
func (c *clients) injectViaInjectors(ctx context.Context, cfg Config, g poolGeometry, opt injectOptions) (int, error) {
	stepf("distributed inject: %d connectors across %d nodes (resident injector per node, parallel)", g.totalConn, len(g.byNode))
	type res struct {
		node  string
		acked int
		err   error
	}
	ch := make(chan res, len(g.byNode))
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for node, ords := range g.byNode {
		pod := g.injByNode[node]
		if pod == "" {
			warnf("node %s: no resident injector; skipping its %d connectors", node, len(ords))
			continue
		}
		wg.Add(1)
		go func(node, pod string, ords []int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ordStr := make([]string, len(ords))
			for i, o := range ords {
				ordStr[i] = strconv.Itoa(o)
			}
			env := map[string]string{
				"ORDS": strings.Join(ordStr, " "), "NPC": strconv.Itoa(g.npc),
				"PREFIX": cfg.NodePrefix, "COUNT": strconv.Itoa(opt.count),
				"RATE": strconv.FormatFloat(opt.rate, 'g', -1, 64), "FATAL": strconv.FormatFloat(opt.fatalFrac, 'g', -1, 64),
				"RUNID": opt.runID, "RUNLABEL": cfg.RunLabel, "IDLABEL": cfg.IDLabel,
				"BIN": cfg.injectorBinPath(), "POOL": connectorPoolName, "LEDGERS": poolLedgerDir,
				"FATALEVENT": defStr(opt.fatalEvent, fatalEventNodeReboot),
				"PATTERN":    defStr(opt.pattern, patternFleetStorm),
				"PROCSTRAT":  defStr(opt.procStrategy, procStrategyDefault),
			}
			out, err := c.execShEnv(ctx, cfg.NVSNamespace, pod, env, nodeInjectShell())
			ch <- res{node, sumAcked(out), err}
		}(node, pod, ords)
	}
	wg.Wait()
	close(ch)
	total, errs := 0, 0
	for r := range ch {
		if r.err != nil {
			warnf("inject on %s: %v", r.node, r.err)
			errs++
		}
		total += r.acked
		if r.acked > 0 {
			infof("node %s: acked=%d", r.node, r.acked)
		}
	}
	if total == 0 {
		return 0, fmt.Errorf("no events acknowledged across %d nodes (%d errored)", len(g.byNode), errs)
	}
	return total, nil
}

// nodeInjectShell injects every connector shard present on the node, writing each
// shard's ledger to the shared on-node hostPath (poolLedgerDir) so a later per-ID
// reconcile can read a real shard.
func nodeInjectShell() string {
	return `set +e
mkdir -p "$LEDGERS"
# Drop stale shard ledgers from earlier runs / pool topologies so the merged
# ledger a later reconcile reads is scoped to just this run (reconcile also
# filters by run-id, but this keeps the file from growing unbounded across runs).
rm -f "$LEDGERS"/led-*.jsonl
for ORD in $ORDS; do
  SOCK="/pool-sockets/${POOL}-$ORD/nvsentinel.sock"
  if [ ! -S "$SOCK" ]; then echo "skip ord=$ORD (no socket)"; continue; fi
  OFF=$((ORD * NPC)); END=$((OFF + NPC))
  F="/tmp/n-$ORD.txt"; : > "$F"; i=$OFF
  while [ "$i" -lt "$END" ]; do echo "$PREFIX-$i" >> "$F"; i=$((i + 1)); done
  "$BIN" inject -socket="$SOCK" -nodes-from="$F" -count="$COUNT" -rate="$RATE" \
    -fatal-fraction="$FATAL" -fatal-event="$FATALEVENT" -pattern="$PATTERN" \
    -processing-strategy="$PROCSTRAT" \
    -run-id="$RUNID" -run-label="$RUNLABEL" -id-label="$IDLABEL" \
    -ledger="$LEDGERS/led-$ORD.jsonl" 2>&1 | grep "done: sent="
done
echo POOL_NODE_INJECT_COMPLETE
`
}

func sumAcked(out string) int {
	total := 0
	for _, m := range ackedRe.FindAllStringSubmatch(out, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			total += n
		}
	}
	return total
}

// reconcileArgs assembles the reconcile CLI args shared by count-only and
// ledger-based (per-ID + NodeName) modes.
func reconcileArgs(cfg Config, conn mongoConn, runID string) []string {
	args := []string{
		"reconcile",
		// -direct: run the primitive direct-to-MongoDB reconcile inside the
		// injector; without it `reconcile` would recurse into distributed mode.
		"-direct",
		"-run-id=" + runID,
		"-db=" + cfg.MongoDB,
		"-collection=" + cfg.MongoColl,
		"-field-prefix=" + cfg.FieldPrefix,
		"-run-label=" + cfg.RunLabel,
		"-id-label=" + cfg.IDLabel,
		fmt.Sprintf("-max-loss-fraction=%g", cfg.MaxLossFrac),
	}
	if conn.tlsSecret != "" {
		args = append(args, "-tls-cert-dir=/etc/mongo-certs")
		if conn.authMechanism != "" {
			args = append(args, "-auth-mechanism="+conn.authMechanism)
		}
		if conn.authSource != "" {
			args = append(args, "-auth-source="+conn.authSource)
		}
	}
	return args
}

// poolMergedLedger is the on-node scratch file each injector merges its shard
// ledgers into before reconciling.
const poolMergedLedger = "/tmp/nvs-harness-merged-ledger.jsonl"

// reconcilePerNode fans a shard-scoped per-ID + NodeName reconcile out to every
// resident injector: each merges ITS OWN shard ledgers and reconciles them
// against a datastore read scoped to just those IDs (so no node re-reads the
// whole run), then the controller aggregates the per-node summaries into a
// fleet-wide per-ID + NodeName verdict — no central ledger gather.
func (c *clients) reconcilePerNode(ctx context.Context, cfg Config, g poolGeometry, conn mongoConn, runID string) (*ReconcileReport, error) {
	stepf("distributed reconcile: per-ID + NodeName across %d connectors on %d nodes (resident injector per node, parallel)", g.totalConn, len(g.byNode))

	// The reconcile command each injector runs on its merged shard ledger.
	// reconcileArgs already carries -direct (the primitive path) so this does not
	// recurse back into distributed mode.
	args := reconcileArgs(cfg, conn, runID)
	args = append(args,
		"-ledger="+poolMergedLedger,
		fmt.Sprintf("-node-sample=%d", cfg.NodeSample),
		"-report=/tmp/reconcile-shard.json",
	)
	script := nodeReconcileShell(shellQuoteRun(cfg.injectorBinPath(), args))

	type res struct {
		node string
		rep  *ReconcileReport
		err  error
	}
	ch := make(chan res, len(g.byNode))
	// Keep concurrency modest: every injector opens its own mTLS/X.509 connection
	// to the same MongoDB, and a large simultaneous handshake burst is what makes
	// individual shard reconciles (which pass on their own) fail under fan-out.
	sem := make(chan struct{}, 5)
	var wg sync.WaitGroup
	for node := range g.byNode {
		pod := g.injByNode[node]
		if pod == "" {
			warnf("node %s: no resident injector; cannot reconcile its %d connectors", node, len(g.byNode[node]))
			continue
		}
		wg.Add(1)
		go func(node, pod string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// Retry transient datastore hiccups (connection/handshake under load):
			// a shard that produced no parseable report is re-run a few times with
			// backoff before giving up.
			var out string
			var err error
			for attempt := 1; attempt <= 3; attempt++ {
				out, err = c.execShEnv(ctx, cfg.NVSNamespace, pod, map[string]string{"MONGO_URI": conn.uri}, script)
				if rep := extractReport(out); rep != nil {
					ch <- res{node, rep, nil}
					return
				}
				select {
				case <-ctx.Done():
					ch <- res{node, nil, ctx.Err()}
					return
				case <-time.After(time.Duration(attempt*3) * time.Second):
				}
			}
			ch <- res{node, nil, err}
		}(node, pod)
	}
	wg.Wait()
	close(ch)

	agg := &ReconcileReport{
		RunID:       runID,
		MaxLoss:     cfg.MaxLossFrac,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}
	reported, errs := 0, 0
	for r := range ch {
		if r.err != nil {
			warnf("reconcile on %s: %v", r.node, r.err)
			errs++
		}
		if r.rep == nil {
			continue
		}
		reported++
		agg.Injected += r.rep.Injected
		agg.Acked += r.rep.Acked
		agg.StoredForRun += r.rep.StoredForRun
		agg.Accounted += r.rep.Accounted
		agg.Missing += r.rep.Missing
		agg.NodeChecked += r.rep.NodeChecked
		agg.NodeMatched += r.rep.NodeMatched
		agg.NodeMismatched += r.rep.NodeMismatched
		agg.MissingSample = appendCapped(agg.MissingSample, r.rep.MissingSample, 20)
		agg.NodeMismatchSample = appendCapped(agg.NodeMismatchSample, r.rep.NodeMismatchSample, 20)
	}
	if reported == 0 {
		return nil, fmt.Errorf("no injector produced a reconcile report across %d nodes (%d errored)", len(g.byNode), errs)
	}
	if agg.Injected == 0 {
		return nil, fmt.Errorf("no injected events found in any shard ledger (was `inject` run for run-id %s?)", runID)
	}
	agg.LossFraction = float64(agg.Missing) / float64(agg.Injected)
	agg.Verdict = "PASS"
	if agg.LossFraction > cfg.MaxLossFrac || agg.NodeMismatched > 0 {
		agg.Verdict = "FAIL"
	}
	switch {
	case agg.NodeMismatched > 0:
		agg.NodeAttrNote = fmt.Sprintf("node attribution FAIL: %d/%d sampled events stored the wrong NodeName", agg.NodeMismatched, agg.NodeChecked)
	case agg.NodeChecked == 0:
		agg.NodeAttrNote = "node attribution UNVERIFIED: no accounted events sampled (schema/path?)"
	default:
		agg.NodeAttrNote = fmt.Sprintf("node attribution OK: %d/%d sampled events attributed to the correct node", agg.NodeMatched, agg.NodeChecked)
	}
	return agg, nil
}

// reconcileByCountPool is the count-only distributed reconcile used for runs with
// no per-connector shard ledger (the direct-`mongo` mechanism). MongoDB is a
// single shared datastore, so one resident injector counts the docs stored under
// the run label and diffs that against the expected injected total — no per-ID
// ledger and no fan-out needed. Node attribution is not verified in this mode
// (there is no per-event ledger to compare stored NodeName against).
func (c *clients) reconcileByCountPool(ctx context.Context, cfg Config, g poolGeometry, conn mongoConn, runID string, expected int) (*ReconcileReport, error) {
	pod := firstInjector(g)
	if pod == "" {
		return nil, fmt.Errorf("no resident injector available to reconcile run %s", runID)
	}
	args := reconcileArgs(cfg, conn, runID)
	// Empty -ledger + -expect-injected selects the primitive's count-only mode.
	args = append(args, "-ledger=", fmt.Sprintf("-expect-injected=%d", expected), "-report=/tmp/reconcile-count.json")
	out, err := c.execShEnv(ctx, cfg.NVSNamespace, pod, map[string]string{"MONGO_URI": conn.uri}, shellQuoteRun(cfg.injectorBinPath(), args)+" 2>/dev/null")
	rep := extractReport(out)
	if rep == nil {
		if err != nil {
			return nil, fmt.Errorf("count-only reconcile on %s: %w", pod, err)
		}
		return nil, fmt.Errorf("count-only reconcile on %s produced no parseable report", pod)
	}
	return rep, nil
}

// clearPoolLedgers removes stale per-connector shard ledgers from every injector
// node. The gRPC inject clears its own node's ledgers before writing, but a run
// that produces NO ledger (the direct-`mongo` mechanism) would otherwise leave a
// previous gRPC run's led-*.jsonl in place for a later reconcile to misread, so
// the mongo inject clears them fleet-wide up front. Best-effort: a failure to
// clear one node is warned, not fatal.
func (c *clients) clearPoolLedgers(ctx context.Context, ns string, g poolGeometry) {
	for node, pod := range g.injByNode {
		if pod == "" {
			continue
		}
		if _, err := c.execSh(ctx, ns, pod, "rm -f "+poolLedgerDir+"/led-*.jsonl"); err != nil {
			warnf("clear stale shard ledgers on %s: %v", node, err)
		}
	}
}

// waitStoredStable replaces a fixed drain sleep: it polls the datastore's stored
// count for the run (via a single count query inside one resident injector) until
// the count stops changing across consecutive polls or reaches the expected
// total (connectors x nodes-per-connector), or the overall timeout elapses.
// Overridable via P03_DRAIN_POLL_SECONDS / P03_DRAIN_TIMEOUT_SECONDS /
// P03_DRAIN_STABLE_POLLS.
func (c *clients) waitStoredStable(ctx context.Context, cfg Config, g poolGeometry, conn mongoConn, runID string) error {
	pod := firstInjector(g)
	if pod == "" {
		return fmt.Errorf("no resident injector available to poll stored count")
	}
	expect := g.totalConn * g.npc
	interval := time.Duration(envInt("P03_DRAIN_POLL_SECONDS", 10)) * time.Second
	timeout := time.Duration(envInt("P03_DRAIN_TIMEOUT_SECONDS", 300)) * time.Second
	stableNeed := envInt("P03_DRAIN_STABLE_POLLS", 2)

	stepf("drain: polling stored count for run %s (expect=%d, stable after %d equal polls, timeout %s)", runID, expect, stableNeed, timeout)
	deadline := time.Now().Add(timeout)
	last, stable := -1, 0
	for {
		n, err := c.poolStoredCount(ctx, cfg, pod, conn, runID, expect)
		if err != nil {
			return err
		}
		infof("drain: stored=%d/%d for run %s", n, expect, runID)
		if expect > 0 && n >= expect {
			infof("drain: reached expected total")
			return nil
		}
		if n == last {
			if stable++; stable >= stableNeed {
				infof("drain: stored count stable at %d", n)
				return nil
			}
		} else {
			last, stable = n, 0
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("stored count did not stabilize within %s (last=%d/%d)", timeout, n, expect)
		}
		sleepCtx(ctx, interval)
	}
}

// poolStoredCount returns the datastore's current stored count for the run,
// obtained by a single count-only reconcile inside one resident injector.
func (c *clients) poolStoredCount(ctx context.Context, cfg Config, pod string, conn mongoConn, runID string, expect int) (int, error) {
	if expect < 1 {
		expect = 1
	}
	args := reconcileArgs(cfg, conn, runID)
	args = append(args, "-ledger=", fmt.Sprintf("-expect-injected=%d", expect), "-report=/tmp/reconcile-poll.json")
	out, err := c.execShEnv(ctx, cfg.NVSNamespace, pod, map[string]string{"MONGO_URI": conn.uri}, shellQuoteRun(cfg.injectorBinPath(), args)+" 2>/dev/null")
	rep := extractReport(out)
	if rep == nil {
		if err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("count poll produced no parseable report")
	}
	return rep.StoredForRun, nil
}

// firstInjector returns any resident injector pod from the pool geometry.
func firstInjector(g poolGeometry) string {
	for _, pod := range g.injByNode {
		if pod != "" {
			return pod
		}
	}
	return ""
}

// nodeReconcileShell merges every shard ledger the node holds into one file, then
// runs the supplied reconcile command against it. A node with no ledgers reports
// a clean zero so it neither fails nor skews the fleet aggregate.
func nodeReconcileShell(reconcileCmd string) string {
	return `set +e
: > "` + poolMergedLedger + `"
for f in "` + poolLedgerDir + `"/led-*.jsonl; do
  [ -f "$f" ] && cat "$f" >> "` + poolMergedLedger + `"
done
if [ ! -s "` + poolMergedLedger + `" ]; then
  echo '{"injected":0,"acked":0,"stored_for_run":0,"accounted":0,"missing":0,"node_checked":0,"node_matched":0,"node_mismatched":0,"verdict":"PASS"}'
  exit 0
fi
` + reconcileCmd + ` 2>/dev/null
`
}

// appendCapped appends src to dst up to a total length of max.
func appendCapped(dst, src []string, max int) []string {
	for _, s := range src {
		if len(dst) >= max {
			break
		}
		dst = append(dst, s)
	}
	return dst
}

// shellQuoteRun renders `<bin> <args...>` as a single shell command line safe for `sh -c`.
func shellQuoteRun(bin string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, bin)
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\$&|;<>()") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

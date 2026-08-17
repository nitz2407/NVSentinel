/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ReconcileReport is the machine-diffable P0.3 / zero-loss result.
type ReconcileReport struct {
	RunID         string   `json:"run_id"`
	Injected      int      `json:"injected"`
	Acked         int      `json:"acked"`
	StoredForRun  int      `json:"stored_for_run"`
	Accounted     int      `json:"accounted"`
	Missing       int      `json:"missing"`
	Unexpected    int      `json:"unexpected"`
	LossFraction  float64  `json:"loss_fraction"`
	MaxLoss       float64  `json:"max_loss_fraction"`
	Verdict       string   `json:"verdict"`
	MissingSample []string `json:"missing_sample,omitempty"`
	// P0.3 end-to-end node visibility: for a sample of accounted events, assert
	// the NodeName stored in the datastore matches the node the harness injected
	// against — proving events are attributed to the right node, not just landed.
	NodeChecked        int      `json:"node_checked"`
	NodeMatched        int      `json:"node_matched"`
	NodeMismatched     int      `json:"node_mismatched"`
	NodeMismatchSample []string `json:"node_mismatch_sample,omitempty"`
	NodeAttrNote       string   `json:"node_attr_note,omitempty"`
	GeneratedAt        string   `json:"generated_at_utc"`
}

// runReconcile is the in-cluster P0.3 reconciler: reads the injection ledger and
// confirms every injected event id landed in the datastore. Reused by SYS-2 /
// MB-5 / SYS-5 zero-loss checks.
func runReconcile(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("events reconcile", flag.ExitOnError)
	cfg := defaultConfig()
	bindNvsNamespaceFlag(fs, &cfg)
	bindResultsFlag(fs, &cfg)
	bindMongoFlags(fs, &cfg)
	bindInjectorFlags(fs, &cfg)
	direct := fs.Bool("direct", false, "connect straight to MongoDB (default: run in-cluster via a resident injector, deriving the expected total from the live pool). Set automatically when invoked inside an injector.")
	// -uri default falls back to the MONGO_URI env var set by the in-cluster
	// orchestrator (execShEnv) so mTLS credentials stay off the command line. This
	// is an internal mechanism, not user config; empty env => local default.
	uri := fs.String("uri", internalMongoURIDefault(), "MongoDB connection URI (defaults to $MONGO_URI when set by the in-cluster orchestrator)")
	db := fs.String("db", "HealthEventsDatabase", "database name")
	coll := fs.String("collection", "HealthEvents", "collection name")
	fieldPfx := fs.String("field-prefix", "healthevent", "stored sub-document holding the health event")
	runLabel := fs.String("run-label", "nvs_harness_run", "metadata key holding the run id")
	idLabel := fs.String("id-label", "nvs_harness_id", "metadata key holding the per-event id")
	runID := fs.String("run-id", "", "correlation run id to reconcile (required)")
	ledgerPath := fs.String("ledger", "/results/injection-ledger.jsonl", "injection ledger path (empty + -expect-injected => count-only mode)")
	expectInjected := fs.Int("expect-injected", 0, "count-only mode (P0.5 pool): expected injected total when no single ledger exists")
	reportPath := fs.String("report", "/results/reconcile-report.json", "where to write the JSON report")
	maxLoss := fs.Float64("max-loss-fraction", 0.0, "max acceptable missing fraction for PASS")
	nodeSample := fs.Int("node-sample", 200, "sample size for verifying stored NodeName attribution (0 disables; ignored in count-only mode)")
	timeout := fs.Duration("timeout", 60*time.Second, "datastore query timeout")
	// TLS / X.509 knobs so reconcile works against a MongoDB with requireTLS +
	// mTLS (the NVSentinel mongodb-store chart default) as well as plain installs.
	tlsCertDir := fs.String("tls-cert-dir", "", "dir with ca.crt (+ tls.crt/tls.key for mTLS); empty = no TLS")
	tlsInsecure := fs.Bool("tls-insecure", false, "skip TLS server verification")
	authMech := fs.String("auth-mechanism", "", "auth mechanism, e.g. MONGODB-X509")
	authSource := fs.String("auth-source", "", "auth source db, e.g. $external")
	_ = fs.Parse(args)

	if *runID == "" {
		return fmt.Errorf("--run-id is required")
	}

	// Distributed mode (default): account the run in-cluster via a resident
	// injector — the operator counterpart to `inject` firing all injectors.
	if !*direct {
		return runReconcileDistributed(ctx, cfg, *runID)
	}

	rep, err := reconcile(ctx, reconcileParams{
		uri: *uri, db: *db, coll: *coll, fieldPfx: *fieldPfx,
		runLabel: *runLabel, idLabel: *idLabel, runID: *runID,
		ledgerPath: *ledgerPath, expectInjected: *expectInjected, maxLoss: *maxLoss, nodeSample: *nodeSample, timeout: *timeout,
		tlsCertDir: *tlsCertDir, tlsInsecure: *tlsInsecure,
		authMech: *authMech, authSource: *authSource,
	})
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(b))
	if err := os.WriteFile(*reportPath, b, 0o644); err != nil {
		warnf("could not write report %s: %v", *reportPath, err)
	}
	if rep.Verdict != "PASS" {
		return fmt.Errorf("reconcile FAIL: missing=%d loss=%.4f node_mismatched=%d/%d",
			rep.Missing, rep.LossFraction, rep.NodeMismatched, rep.NodeChecked)
	}
	if rep.NodeAttrNote != "" {
		infof("%s", rep.NodeAttrNote)
	}
	return nil
}

type reconcileParams struct {
	uri, db, coll, fieldPfx  string
	runLabel, idLabel, runID string
	ledgerPath               string
	expectInjected           int
	maxLoss                  float64
	nodeSample               int
	timeout                  time.Duration
	tlsCertDir               string
	tlsInsecure              bool
	authMech, authSource     string
}

// mongoClientOptions builds driver options from a base URI plus optional TLS and
// auth settings so the reconciler can talk to a plain, TLS, or mTLS/X.509 store.
func mongoClientOptions(p reconcileParams) (*options.ClientOptions, error) {
	opts := options.Client().ApplyURI(p.uri)
	// Under the distributed P0.3 reconcile, ~10 injectors open mTLS/X.509
	// connections to the same MongoDB at once; the driver's default 30s server
	// selection can lapse during that handshake burst. Give selection/connect the
	// full query timeout and keep each shard's pool tiny so concurrent injectors
	// don't multiply connection pressure.
	if p.timeout > 0 {
		opts.SetServerSelectionTimeout(p.timeout).SetConnectTimeout(p.timeout)
	}
	opts.SetMaxPoolSize(4)
	if p.tlsCertDir != "" {
		tlsCfg, err := buildMongoTLSConfig(p.tlsCertDir, p.tlsInsecure)
		if err != nil {
			return nil, err
		}
		opts.SetTLSConfig(tlsCfg)
	}
	if p.authMech != "" {
		cred := options.Credential{AuthMechanism: p.authMech}
		if p.authSource != "" {
			cred.AuthSource = p.authSource
		}
		opts.SetAuth(cred)
	}
	return opts, nil
}

// buildMongoTLSConfig loads ca.crt (server trust) and, when present, tls.crt +
// tls.key (client cert for mTLS / X.509 auth) from a mounted secret directory.
func buildMongoTLSConfig(dir string, insecure bool) (*tls.Config, error) {
	cfg := &tls.Config{InsecureSkipVerify: insecure} //nolint:gosec // opt-in via -tls-insecure
	caPath := filepath.Join(dir, "ca.crt")
	if b, err := os.ReadFile(caPath); err == nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("parse CA bundle %s", caPath)
		}
		cfg.RootCAs = pool
	} else if !insecure {
		return nil, fmt.Errorf("read CA %s: %w", caPath, err)
	}
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	if fileExists(certPath) && fileExists(keyPath) {
		crt, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("load client keypair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{crt}
	}
	return cfg, nil
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func reconcile(ctx context.Context, p reconcileParams) (ReconcileReport, error) {
	// Count-only mode (P0.5 connector pool): with many pod-local ledgers there is
	// no single injected-ID set to diff, so account for the run by comparing the
	// datastore's stored count for the run id against the expected injected total.
	if p.ledgerPath == "" {
		return reconcileByCount(ctx, p)
	}

	injected, acked, err := readLedger(p.ledgerPath, p.runID)
	if err != nil {
		return ReconcileReport{}, err
	}
	if len(injected) == 0 {
		return ReconcileReport{}, fmt.Errorf("ledger %s had no entries for run-id %s", p.ledgerPath, p.runID)
	}
	infof("ledger: injected=%d acked=%d run-id=%s", len(injected), acked, p.runID)

	qctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	opts, err := mongoClientOptions(p)
	if err != nil {
		return ReconcileReport{}, fmt.Errorf("mongo options: %w", err)
	}
	client, err := mongo.Connect(qctx, opts)
	if err != nil {
		return ReconcileReport{}, fmt.Errorf("mongo connect: %w", err)
	}
	defer client.Disconnect(context.Background())

	runKey := fmt.Sprintf("%s.metadata.%s", p.fieldPfx, p.runLabel)
	idKey := fmt.Sprintf("%s.metadata.%s", p.fieldPfx, p.idLabel)
	nodeKeyLower := fmt.Sprintf("%s.nodename", p.fieldPfx)
	nodeKey := fmt.Sprintf("%s.nodeName", p.fieldPfx)
	nodeKeySnake := fmt.Sprintf("%s.node_name", p.fieldPfx)

	// Scope the datastore read to just this ledger's own IDs, not the whole run.
	// For a single-shard `-direct` run that's simply tighter; for the distributed
	// P0.3 reconcile it's what makes per-node fan-out possible — each injector
	// reads only its shard instead of every node re-reading the entire run.
	ids := make([]string, 0, len(injected))
	for id := range injected {
		ids = append(ids, id)
	}
	cur, err := client.Database(p.db).Collection(p.coll).Find(qctx,
		bson.M{runKey: p.runID, idKey: bson.M{"$in": ids}},
		options.Find().SetProjection(bson.M{idKey: 1, nodeKeyLower: 1, nodeKey: 1, nodeKeySnake: 1}))
	if err != nil {
		return ReconcileReport{}, fmt.Errorf("mongo find: %w", err)
	}
	defer cur.Close(qctx)

	// id -> stored NodeName (empty string means the doc had no node field).
	storedNode := map[string]string{}
	for cur.Next(qctx) {
		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			continue
		}
		if id := extractStoredID(doc, p.fieldPfx, p.idLabel); id != "" {
			storedNode[id] = extractStoredNode(doc, p.fieldPfx)
		}
	}
	if err := cur.Err(); err != nil {
		return ReconcileReport{}, fmt.Errorf("cursor: %w", err)
	}

	accounted := 0
	missing := make([]string, 0)
	for id := range injected {
		if _, ok := storedNode[id]; ok {
			accounted++
		} else {
			missing = append(missing, id)
		}
	}
	unexpected := 0
	for id := range storedNode {
		if _, ok := injected[id]; !ok {
			unexpected++
		}
	}
	loss := float64(len(missing)) / float64(len(injected))
	verdict := "PASS"
	if loss > p.maxLoss {
		verdict = "FAIL"
	}
	sample := missing
	if len(sample) > 20 {
		sample = sample[:20]
	}

	rep := ReconcileReport{
		RunID: p.runID, Injected: len(injected), Acked: acked, StoredForRun: len(storedNode),
		Accounted: accounted, Missing: len(missing), Unexpected: unexpected,
		LossFraction: loss, MaxLoss: p.maxLoss, Verdict: verdict,
		MissingSample: sample, GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}
	verifyNodeAttribution(&rep, injected, storedNode, p.nodeSample)
	return rep, nil
}

// verifyNodeAttribution asserts end-to-end node visibility (P0.3): for a sample
// of accounted events, the NodeName the datastore stored must equal the node the
// harness injected against. A mismatch fails the reconcile. If the datastore has
// no node field at all for the whole sample (e.g. a chart serializes it under an
// unexpected path), attribution is reported as unverified with a note rather
// than failing an otherwise clean zero-loss run.
func verifyNodeAttribution(rep *ReconcileReport, injected, storedNode map[string]string, sampleMax int) {
	if sampleMax <= 0 {
		rep.NodeAttrNote = "node attribution check disabled (-node-sample 0)"
		return
	}
	matched, mismatched, emptyStored := 0, 0, 0
	mmSample := make([]string, 0, 20)
	for id, want := range injected {
		got, ok := storedNode[id]
		if !ok {
			continue // not accounted; already counted as missing
		}
		if got == "" {
			// No node field stored: not verifiable, don't count as checked.
			emptyStored++
			continue
		}
		if got == want {
			matched++
		} else {
			mismatched++
			if len(mmSample) < 20 {
				mmSample = append(mmSample, fmt.Sprintf("%s: injected=%q stored=%q", id, want, got))
			}
		}
		if matched+mismatched >= sampleMax {
			break
		}
	}

	checked := matched + mismatched
	rep.NodeChecked = checked
	rep.NodeMatched = matched
	rep.NodeMismatched = mismatched

	switch {
	case checked == 0 && emptyStored > 0:
		// Every accounted doc lacked a node field: almost certainly a schema/path
		// mismatch, not real misattribution. Surface it without failing the run.
		rep.NodeAttrNote = fmt.Sprintf("node attribution UNVERIFIED: datastore had no NodeName on all %d accounted docs (schema/path?)", emptyStored)
	case checked == 0:
		rep.NodeAttrNote = "no accounted events to sample for node attribution"
	case mismatched > 0:
		rep.NodeMismatchSample = mmSample
		rep.NodeAttrNote = fmt.Sprintf("node attribution FAIL: %d/%d sampled events stored the wrong NodeName", mismatched, checked)
		rep.Verdict = "FAIL"
	default:
		rep.NodeAttrNote = fmt.Sprintf("node attribution OK: %d/%d sampled events attributed to the correct node", matched, checked)
	}
}

// reconcileByCount accounts for a run without an injection ledger: it counts the
// documents the datastore holds for the run id and compares that against the
// expected injected total (P0.5 pool, where each connector keeps its own ledger).
func reconcileByCount(ctx context.Context, p reconcileParams) (ReconcileReport, error) {
	if p.expectInjected <= 0 {
		return ReconcileReport{}, fmt.Errorf("count-only reconcile needs -expect-injected > 0")
	}
	qctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	opts, err := mongoClientOptions(p)
	if err != nil {
		return ReconcileReport{}, fmt.Errorf("mongo options: %w", err)
	}
	client, err := mongo.Connect(qctx, opts)
	if err != nil {
		return ReconcileReport{}, fmt.Errorf("mongo connect: %w", err)
	}
	defer client.Disconnect(context.Background())

	runKey := fmt.Sprintf("%s.metadata.%s", p.fieldPfx, p.runLabel)
	stored, err := client.Database(p.db).Collection(p.coll).CountDocuments(qctx, bson.M{runKey: p.runID})
	if err != nil {
		return ReconcileReport{}, fmt.Errorf("mongo count: %w", err)
	}
	storedN := int(stored)

	missing := p.expectInjected - storedN
	if missing < 0 {
		missing = 0
	}
	accounted := p.expectInjected - missing
	loss := float64(missing) / float64(p.expectInjected)
	verdict := "PASS"
	if loss > p.maxLoss {
		verdict = "FAIL"
	}
	infof("count-only reconcile: expect=%d stored=%d missing=%d run-id=%s", p.expectInjected, storedN, missing, p.runID)
	return ReconcileReport{
		RunID: p.runID, Injected: p.expectInjected, Acked: p.expectInjected, StoredForRun: storedN,
		Accounted: accounted, Missing: missing, Unexpected: 0,
		LossFraction: loss, MaxLoss: p.maxLoss, Verdict: verdict,
		NodeAttrNote: "node attribution not verified in count-only mode (no per-event ledger)",
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// readLedger returns each injected event id mapped to the node the harness
// attributed it to (for P0.3 NodeName verification), plus the acked count.
// Entries are scoped to runID: the merged shard ledger on a pool node can retain
// stale led-*.jsonl files from earlier runs / pool topologies, and every event id
// is "<run-id>-<seq>-<hex>", so filtering by the run-id prefix keeps reconcile
// correct regardless of leftover ledgers. An empty runID disables filtering.
func readLedger(path, runID string) (map[string]string, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open ledger %s: %w", path, err)
	}
	defer f.Close()
	prefix := ""
	if runID != "" {
		prefix = runID + "-"
	}
	idNode := map[string]string{}
	acked := 0
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for s.Scan() {
		line := s.Bytes()
		if len(line) == 0 {
			continue
		}
		var e ledgerEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if e.ID == "" || (prefix != "" && !strings.HasPrefix(e.ID, prefix)) {
			continue
		}
		idNode[e.ID] = e.Node
		if e.Acked {
			acked++
		}
	}
	return idNode, acked, nil
}

func extractStoredID(doc bson.M, prefix, idLabel string) string {
	sub, ok := doc[prefix].(bson.M)
	if !ok {
		return ""
	}
	meta, ok := sub["metadata"].(bson.M)
	if !ok {
		return ""
	}
	if v, ok := meta[idLabel].(string); ok {
		return v
	}
	return ""
}

// extractStoredNode pulls the NodeName the datastore recorded for the event. The
// datastore serializes the health_event proto with all-lowercase field names, so
// the node lands at `<prefix>.nodename`; the camelCase/snake_case variants are
// accepted as fallbacks across chart/serializer versions.
func extractStoredNode(doc bson.M, prefix string) string {
	sub, ok := doc[prefix].(bson.M)
	if !ok {
		return ""
	}
	for _, k := range []string{"nodename", "nodeName", "node_name"} {
		if v, ok := sub[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// runManifest records how a run was injected so a later `reconcile` — invoked
// standalone, without the inject-time flags — can pick the right accounting mode.
// The gRPC pool path needs none (its per-connector shard ledgers drive a per-ID
// reconcile); the direct-`mongo` path writes one, because it has no shard ledger
// and must be reconciled by run-label count. Absent manifest => ledger-based
// reconcile, so older runs and external callers are unaffected.
type runManifest struct {
	Mechanism string `json:"mechanism"`
	Expected  int    `json:"expected"`
}

// runManifestPath is the operator-side manifest location for a run. Written by
// the inject process and read by the reconcile process; both run operator-side
// against the same HARNESS_RESULTS_DIR, so no in-cluster round-trip is needed.
func runManifestPath(cfg Config, runID string) string {
	return filepath.Join(cfg.ResultsDir, "runs", runID+".json")
}

func writeRunManifest(cfg Config, runID, mechanism string, expected int) {
	p := runManifestPath(cfg, runID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		warnf("run manifest dir %s: %v", filepath.Dir(p), err)
		return
	}
	b, _ := json.MarshalIndent(runManifest{Mechanism: mechanism, Expected: expected}, "", "  ")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		warnf("write run manifest %s: %v", p, err)
	}
}

func readRunManifest(cfg Config, runID string) (runManifest, bool) {
	var m runManifest
	b, err := os.ReadFile(runManifestPath(cfg, runID))
	if err != nil {
		return m, false
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, false
	}
	return m, true
}

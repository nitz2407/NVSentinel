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
	GeneratedAt   string   `json:"generated_at_utc"`
}

// runReconcile is the in-cluster P0.3 reconciler: reads the injection ledger and
// confirms every injected event id landed in the datastore. Reused by SYS-2 /
// MB-5 / SYS-5 zero-loss checks.
func runReconcile(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ExitOnError)
	uri := fs.String("uri", env("MONGO_URI", "mongodb://localhost:27017"), "MongoDB connection URI")
	db := fs.String("db", env("MONGO_DATABASE", "HealthEventsDatabase"), "database name")
	coll := fs.String("collection", env("MONGO_COLLECTION", "HealthEvents"), "collection name")
	fieldPfx := fs.String("field-prefix", "healthevent", "stored sub-document holding the health event")
	runLabel := fs.String("run-label", "nvs_harness_run", "metadata key holding the run id")
	idLabel := fs.String("id-label", "nvs_harness_id", "metadata key holding the per-event id")
	runID := fs.String("run-id", "", "correlation run id to reconcile (required)")
	ledgerPath := fs.String("ledger", "/results/injection-ledger.jsonl", "injection ledger path")
	reportPath := fs.String("report", "/results/reconcile-report.json", "where to write the JSON report")
	maxLoss := fs.Float64("max-loss-fraction", 0.0, "max acceptable missing fraction for PASS")
	timeout := fs.Duration("timeout", 60*time.Second, "datastore query timeout")
	// TLS / X.509 knobs so reconcile works against a MongoDB with requireTLS +
	// mTLS (the NVSentinel mongodb-store chart default) as well as plain installs.
	tlsCertDir := fs.String("tls-cert-dir", env("MONGO_TLS_CERT_DIR", ""), "dir with ca.crt (+ tls.crt/tls.key for mTLS); empty = no TLS")
	tlsInsecure := fs.Bool("tls-insecure", envBool("MONGO_TLS_INSECURE", false), "skip TLS server verification")
	authMech := fs.String("auth-mechanism", env("MONGO_AUTH_MECHANISM", ""), "auth mechanism, e.g. MONGODB-X509")
	authSource := fs.String("auth-source", env("MONGO_AUTH_SOURCE", ""), "auth source db, e.g. $external")
	_ = fs.Parse(args)

	if *runID == "" {
		return fmt.Errorf("-run-id is required")
	}
	rep, err := reconcile(ctx, reconcileParams{
		uri: *uri, db: *db, coll: *coll, fieldPfx: *fieldPfx,
		runLabel: *runLabel, idLabel: *idLabel, runID: *runID,
		ledgerPath: *ledgerPath, maxLoss: *maxLoss, timeout: *timeout,
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
		return fmt.Errorf("reconcile FAIL: missing=%d loss=%.4f", rep.Missing, rep.LossFraction)
	}
	return nil
}

type reconcileParams struct {
	uri, db, coll, fieldPfx  string
	runLabel, idLabel, runID string
	ledgerPath               string
	maxLoss                  float64
	timeout                  time.Duration
	tlsCertDir               string
	tlsInsecure              bool
	authMech, authSource     string
}

// mongoClientOptions builds driver options from a base URI plus optional TLS and
// auth settings so the reconciler can talk to a plain, TLS, or mTLS/X.509 store.
func mongoClientOptions(p reconcileParams) (*options.ClientOptions, error) {
	opts := options.Client().ApplyURI(p.uri)
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
	injected, acked, err := readLedger(p.ledgerPath)
	if err != nil {
		return ReconcileReport{}, err
	}
	if len(injected) == 0 {
		return ReconcileReport{}, fmt.Errorf("ledger %s had no entries", p.ledgerPath)
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

	cur, err := client.Database(p.db).Collection(p.coll).Find(qctx,
		bson.M{runKey: p.runID}, options.Find().SetProjection(bson.M{idKey: 1}))
	if err != nil {
		return ReconcileReport{}, fmt.Errorf("mongo find: %w", err)
	}
	defer cur.Close(qctx)

	stored := map[string]struct{}{}
	for cur.Next(qctx) {
		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			continue
		}
		if id := extractStoredID(doc, p.fieldPfx, p.idLabel); id != "" {
			stored[id] = struct{}{}
		}
	}
	if err := cur.Err(); err != nil {
		return ReconcileReport{}, fmt.Errorf("cursor: %w", err)
	}

	accounted := 0
	missing := make([]string, 0)
	for id := range injected {
		if _, ok := stored[id]; ok {
			accounted++
		} else {
			missing = append(missing, id)
		}
	}
	unexpected := 0
	for id := range stored {
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
	return ReconcileReport{
		RunID: p.runID, Injected: len(injected), Acked: acked, StoredForRun: len(stored),
		Accounted: accounted, Missing: len(missing), Unexpected: unexpected,
		LossFraction: loss, MaxLoss: p.maxLoss, Verdict: verdict,
		MissingSample: sample, GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func readLedger(path string) (map[string]struct{}, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open ledger %s: %w", path, err)
	}
	defer f.Close()
	ids := map[string]struct{}{}
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
		if e.ID != "" {
			ids[e.ID] = struct{}{}
		}
		if e.Acked {
			acked++
		}
	}
	return ids, acked, nil
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

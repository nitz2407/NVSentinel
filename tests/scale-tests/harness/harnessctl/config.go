/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"os"
	"strconv"
)

// Config mirrors config/harness.env. Every value is overridable by the matching
// environment variable so the Go CLI and the shell install scripts share one
// source of truth. Flags on individual subcommands override these defaults.
type Config struct {
	NVSNamespace         string
	MonitoringNamespace  string
	JanitorNamespace     string
	CertManagerNamespace string
	KWOKNamespace        string

	// ReportWindow is the PromQL lookback the `report` command uses for peak
	// (max_over_time) queries — long enough to span a full run.
	ReportWindow string

	// NodeCount is the KWOK scale target. It has NO config-file default: pass it
	// per run on the CLI (`scale-nodes -count` or `connector-pool -emulated-nodes`).
	// The KWOK_NODE_COUNT env var is still an optional shell override, but 0
	// (unset) means "the command must be told".
	NodeCount   int
	NodePrefix  string
	NodeBatch   int
	GPUCount    int
	NodeCPU     string
	NodeMemory  string
	NodeMaxPods int
	// ProviderIDScheme, when non-empty, stamps each fake node with
	// spec.providerID = "<scheme>://<node-name>". On managed clusters (e.g. AKS)
	// the cloud-node-lifecycle controller deletes Nodes whose providerID the cloud
	// provider cannot resolve to a real instance; giving KWOK nodes a scheme the
	// cloud provider errors on (rather than a definitive "not found") keeps them
	// from being reaped. Empty = no providerID (default; correct for Kind).
	ProviderIDScheme string

	MaxAPIServerP99 float64
	NodeReadyTO     int

	// P0.2 cluster-resource guardrails: real (non-kwok) node CPU/memory
	// utilization above which the cluster is considered out of "normal bounds".
	MaxClusterCPUPct float64
	MaxClusterMemPct float64

	// P0.2 ceiling sweep: ramp node count from start to max in step increments
	// until degradation, then attribute it — non-interactively.
	CeilingStart   int
	CeilingStep    int
	CeilingMax     int
	CeilingSettle  int     // seconds to probe/settle at each step before measuring
	CeilingListP99 float64 // LIST-nodes p99 guardrail (the real-ceiling signal)
	CeilingKwokCPU float64 // KWOK controller CPU (cores) above which it's "saturated"
	MetricsWindow  string  // PromQL rate window for per-step measurements

	EventCount    int
	EventRate     float64
	FatalFraction float64 // fraction of injected events that are fatal (drive cordon/remediation)
	RunLabel      string
	IDLabel       string
	MaxLossFrac   float64
	NodeSample    int // per-shard sample size for P0.3 NodeName attribution (0 disables)
	MongoURI      string
	MongoDB       string
	MongoColl     string
	FieldPrefix   string

	// Mongo connection discovery (used to build the reconciler's connection when
	// HARNESS_MONGO_URI is unset). Defaults match the NVSentinel mongodb-store
	// chart; override via env for a different datastore layout.
	MongoService    string // headless service name
	MongoReplicaSet string // replica set name ("" = standalone)
	MongoPort       string
	MongoTLSSecret  string // cert-manager TLS secret for mTLS/X.509 (empty disables auto-TLS)
	MongoRootSecret string // fallback secret holding a root password (non-TLS installs)

	MonPromSvc  string
	MonPromPort string

	JobCompleteDelay int
	ActionTimeout    int

	// HarnessBin, when set, is the local linux/amd64 harnessctl binary staged into
	// the resident injectors for the image-free path (default: the running binary).
	HarnessBin string

	// P0.5 platform-connector pool simulation. In production the connector is a
	// per-node DaemonSet; KWOK nodes have no kubelet, so the harness emulates the
	// connector plane by packing many *real* connector pods (cloned from the live
	// DaemonSet) onto real nodes, then covering the emulated remainder with a
	// per-connector event-rate multiplier.
	ConnectorDaemonSet        string // DaemonSet whose pod template the pool clones
	ConnectorPoolPerNodeLimit int    // connector pods/node density cap; count auto-sizes to min(emulated, realNodes*this)

	ResultsDir string
}

func loadConfig() Config {
	return Config{
		NVSNamespace:         env("NVS_NAMESPACE", "nvsentinel"),
		MonitoringNamespace:  env("MONITORING_NAMESPACE", "monitoring"),
		JanitorNamespace:     env("JANITOR_NAMESPACE", "dgxc-janitor-system"),
		CertManagerNamespace: env("CERT_MANAGER_NAMESPACE", "cert-manager"),
		KWOKNamespace:        env("KWOK_NAMESPACE", "kube-system"),
		ReportWindow:         env("REPORT_WINDOW", "3h"),

		NodeCount:   envInt("KWOK_NODE_COUNT", 0),
		NodePrefix:  env("KWOK_NODE_PREFIX", "kwok-gpu"),
		NodeBatch:   envInt("KWOK_NODE_BATCH", 500),
		GPUCount:    envInt("KWOK_GPU_COUNT", 8),
		NodeCPU:     env("KWOK_NODE_CPU", "192"),
		NodeMemory:  env("KWOK_NODE_MEMORY", "2048Gi"),
		NodeMaxPods: envInt("KWOK_NODE_PODS", 110),

		ProviderIDScheme: env("KWOK_PROVIDER_ID_SCHEME", ""),

		MaxAPIServerP99: envFloat("MAX_APISERVER_P99_SECONDS", 1.0),
		NodeReadyTO:     envInt("NODE_READY_TIMEOUT", 1800),

		MaxClusterCPUPct: envFloat("MAX_CLUSTER_CPU_PCT", 0.85),
		MaxClusterMemPct: envFloat("MAX_CLUSTER_MEM_PCT", 0.85),

		CeilingStart:   envInt("CEILING_START", 10000),
		CeilingStep:    envInt("CEILING_STEP", 10000),
		CeilingMax:     envInt("CEILING_MAX", 50000),
		CeilingSettle:  envInt("CEILING_SETTLE_SECONDS", 300),
		CeilingListP99: envFloat("CEILING_LIST_NODES_P99_SECONDS", 1.0),
		CeilingKwokCPU: envFloat("CEILING_KWOK_CPU_CORES", 3.5),
		MetricsWindow:  env("METRICS_WINDOW", "5m"),

		EventCount:    envInt("P03_EVENT_COUNT", 10000),
		EventRate:     envFloat("P03_EVENT_RATE", 500),
		FatalFraction: envFloat("HARNESS_FATAL_FRACTION", 0.08),
		RunLabel:      env("HARNESS_RUN_LABEL", "nvs_harness_run"),
		IDLabel:       env("HARNESS_ID_LABEL", "nvs_harness_id"),
		MaxLossFrac:   envFloat("P03_MAX_LOSS_FRACTION", 0.0),
		NodeSample:    envInt("P03_NODE_SAMPLE", 200),
		MongoURI:      env("HARNESS_MONGO_URI", ""),
		MongoDB:       env("MONGO_DATABASE", "HealthEventsDatabase"),
		MongoColl:     env("MONGO_COLLECTION", "HealthEvents"),
		FieldPrefix:   env("MONGO_FIELD_PREFIX", "healthevent"),

		MongoService:    env("MONGO_SERVICE", "mongodb-headless"),
		MongoReplicaSet: env("MONGO_REPLICA_SET", "rs0"),
		MongoPort:       env("MONGO_PORT", "27017"),
		MongoTLSSecret:  env("MONGO_TLS_SECRET", "mongo-app-client-cert-secret"),
		MongoRootSecret: env("MONGO_ROOT_SECRET", "mongodb"),

		MonPromSvc:  env("PROM_SERVICE", "prometheus-prometheus"),
		MonPromPort: env("PROM_PORT", "9090"),

		JobCompleteDelay: envInt("KWOK_JOB_COMPLETE_DELAY", 30),
		ActionTimeout:    envInt("P04_ACTION_TIMEOUT", 300),

		HarnessBin: env("HARNESS_BIN", ""),

		ConnectorDaemonSet:        env("CONNECTOR_DAEMONSET", "platform-connectors"),
		ConnectorPoolPerNodeLimit: envInt("CONNECTOR_POOL_PER_NODE_LIMIT", 50),

		ResultsDir: env("HARNESS_RESULTS_DIR", "./results"),
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

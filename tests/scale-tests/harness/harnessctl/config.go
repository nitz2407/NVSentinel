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
	KubeContext string

	NVSNamespace        string
	MonitoringNamespace string

	NodeCount   int
	NodePrefix  string
	NodeBatch   int
	GPUCount    int
	NodeCPU     string
	NodeMemory  string
	NodeMaxPods int

	MaxAPIServerP99 float64
	NodeReadyTO     int

	EventCount  int
	EventRate   float64
	RunLabel    string
	IDLabel     string
	MaxLossFrac float64
	MongoURI    string
	MongoDB     string
	MongoColl   string
	FieldPrefix string

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

	HarnessImage string

	ResultsDir string
}

func loadConfig() Config {
	return Config{
		KubeContext:         env("HARNESS_KUBE_CONTEXT", ""),
		NVSNamespace:        env("NVS_NAMESPACE", "nvsentinel"),
		MonitoringNamespace: env("MONITORING_NAMESPACE", "monitoring"),

		NodeCount:   envInt("KWOK_NODE_COUNT", 50000),
		NodePrefix:  env("KWOK_NODE_PREFIX", "kwok-gpu"),
		NodeBatch:   envInt("KWOK_NODE_BATCH", 500),
		GPUCount:    envInt("KWOK_GPU_COUNT", 8),
		NodeCPU:     env("KWOK_NODE_CPU", "192"),
		NodeMemory:  env("KWOK_NODE_MEMORY", "2048Gi"),
		NodeMaxPods: envInt("KWOK_NODE_PODS", 110),

		MaxAPIServerP99: envFloat("MAX_APISERVER_P99_SECONDS", 1.0),
		NodeReadyTO:     envInt("NODE_READY_TIMEOUT", 1800),

		EventCount:  envInt("P03_EVENT_COUNT", 10000),
		EventRate:   envFloat("P03_EVENT_RATE", 500),
		RunLabel:    env("HARNESS_RUN_LABEL", "nvs_harness_run"),
		IDLabel:     env("HARNESS_ID_LABEL", "nvs_harness_id"),
		MaxLossFrac: envFloat("P03_MAX_LOSS_FRACTION", 0.0),
		MongoURI:    env("HARNESS_MONGO_URI", ""),
		MongoDB:     env("MONGO_DATABASE", "HealthEventsDatabase"),
		MongoColl:   env("MONGO_COLLECTION", "HealthEvents"),
		FieldPrefix: env("MONGO_FIELD_PREFIX", "healthevent"),

		MongoService:    env("MONGO_SERVICE", "mongodb-headless"),
		MongoReplicaSet: env("MONGO_REPLICA_SET", "rs0"),
		MongoPort:       env("MONGO_PORT", "27017"),
		MongoTLSSecret:  env("MONGO_TLS_SECRET", "mongo-app-client-cert-secret"),
		MongoRootSecret: env("MONGO_ROOT_SECRET", "mongodb"),

		MonPromSvc:  env("PROM_SERVICE", "prometheus-kube-prometheus-prometheus"),
		MonPromPort: env("PROM_PORT", "9090"),

		JobCompleteDelay: envInt("KWOK_JOB_COMPLETE_DELAY", 30),
		ActionTimeout:    envInt("P04_ACTION_TIMEOUT", 300),

		HarnessImage: env("HARNESS_IMAGE", "CHANGE_ME.example.com/nvsentinel-harness/harnessctl:v1"),

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

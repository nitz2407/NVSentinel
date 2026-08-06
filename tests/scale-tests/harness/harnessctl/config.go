/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"flag"
	"os"
	"strconv"
)

// Config is the fully-resolved harness configuration. Every value is set from a
// command-line flag (AWS-CLI style: all inputs on the command line, no env file).
// defaultConfig() supplies the built-in defaults; the small composable bind*Flags
// helpers register the relevant settings as --kebab-case flags, and each command
// wires only the groups it reads. A few deep internal tuning knobs (drain/monitor poll
// intervals) still read an env var as a last-resort override — they are not part
// of the documented input surface and have no harness.env entry.
type Config struct {
	NVSNamespace         string
	MonitoringNamespace  string
	JanitorNamespace     string
	CertManagerNamespace string
	KWOKNamespace        string

	// ReportWindow is the PromQL lookback the `report` command uses for peak
	// (max_over_time) queries — long enough to span a full run.
	ReportWindow string

	// NVSChartVersion is the TARGET NVSentinel version for version-aware bringup:
	// the container image tag (e.g. v1.16.0). When set, `bringup` compares it
	// against the running NVSentinel and helm-upgrades on mismatch; empty leaves
	// whatever is installed untouched. Sourced from NVS_CHART_VERSION so the Go
	// CLI and 30-install-nvsentinel.sh share one target.
	NVSChartVersion string

	// KWOKVersion, CertManagerVersion and MetricsServerVersion are TARGET versions
	// for version-aware bringup of the supporting components. Like NVSChartVersion
	// they are compared against the running container image tag: when set and the
	// installed tag differs, `bringup` reports the component MISSING and re-runs
	// its install script (passing the target down via KWOK_VERSION /
	// CERT_MANAGER_VERSION / METRICS_SERVER_VERSION). Empty leaves whatever is
	// installed untouched and lets the install script use its own default.
	//
	// NOTE: kube-prometheus-stack is intentionally NOT version-gated here — its
	// knob (KPS_CHART_VERSION) is a Helm CHART version, not the operator image
	// tag, so an image-tag comparison would be meaningless.
	KWOKVersion          string
	CertManagerVersion   string
	MetricsServerVersion string

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
	// FatalEvent selects the remediation the fatal events drive: node-reboot
	// (RESTART_BM => RebootNode CR) or gpu-reset (COMPONENT_RESET => GPUReset CR).
	FatalEvent string
	// Pattern is the event-generation shape: fleet-storm | flappy |
	// single-node-burst. ProcessingStrategy is the HealthEvent processingStrategy
	// override: default | store-only | store-and-analyse | execute-remediation.
	// Mechanism selects the write path: grpc (through the platform-connector) or
	// mongo (direct MongoDB insert, bypassing the connector).
	Pattern            string
	ProcessingStrategy string
	Mechanism          string
	RunLabel           string
	IDLabel            string
	MaxLossFrac        float64
	NodeSample         int // per-shard sample size for P0.3 NodeName attribution (0 disables)
	MongoURI           string
	MongoDB            string
	MongoColl          string
	FieldPrefix        string

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

// defaultConfig returns the built-in defaults. There are NO environment reads:
// every value is either this default or whatever the corresponding CLI flag sets.
func defaultConfig() Config {
	return Config{
		NVSNamespace:         "nvsentinel",
		MonitoringNamespace:  "prometheus",
		JanitorNamespace:     "dgxc-janitor-system",
		CertManagerNamespace: "cert-manager",
		KWOKNamespace:        "kube-system",
		NVSChartVersion:      "",
		KWOKVersion:          "",
		CertManagerVersion:   "",
		MetricsServerVersion: "",
		ReportWindow:         "3h",

		NodeCount:   0,
		NodePrefix:  "kwok-gpu",
		NodeBatch:   500,
		GPUCount:    8,
		NodeCPU:     "192",
		NodeMemory:  "2048Gi",
		NodeMaxPods: 110,

		ProviderIDScheme: "",

		MaxAPIServerP99: 1.0,
		NodeReadyTO:     1800,

		MaxClusterCPUPct: 0.85,
		MaxClusterMemPct: 0.85,

		CeilingStart:   10000,
		CeilingStep:    10000,
		CeilingMax:     50000,
		CeilingSettle:  300,
		CeilingListP99: 1.0,
		CeilingKwokCPU: 3.5,
		MetricsWindow:  "5m",

		EventCount:         10000,
		EventRate:          500,
		FatalFraction:      0.08,
		FatalEvent:         "node-reboot",
		Pattern:            "fleet-storm",
		ProcessingStrategy: "default",
		Mechanism:          "grpc",
		RunLabel:           "nvs_harness_run",
		IDLabel:            "nvs_harness_id",
		MaxLossFrac:        0.0,
		NodeSample:         200,
		MongoURI:           "",
		MongoDB:            "HealthEventsDatabase",
		MongoColl:          "HealthEvents",
		FieldPrefix:        "healthevent",

		MongoService:    "mongodb-headless",
		MongoReplicaSet: "rs0",
		MongoPort:       "27017",
		MongoTLSSecret:  "mongo-app-client-cert-secret",
		MongoRootSecret: "mongodb",

		MonPromSvc:  "prometheus-prometheus",
		MonPromPort: "9090",

		JobCompleteDelay: 30,
		ActionTimeout:    300,

		HarnessBin: "",

		ConnectorDaemonSet:        "platform-connectors",
		ConnectorPoolPerNodeLimit: 50,

		ResultsDir: "./results",
	}
}

// Flags are grouped into small, composable binders instead of one fat
// "common" set, so each subcommand registers only the flags it actually reads.
// Every binder uses the value already in c (typically defaultConfig()) as the
// flag default; call the ones a command needs before fs.Parse. This keeps the
// harness fully operable from the command line (no env file) while keeping each
// command's -h focused. A flag needed by several groups is factored into its own
// single-flag binder (e.g. bindMaxAPIServerP99Flag) and reused, so help text
// never drifts.

// --- single-flag binders (namespaces + reused primitives) ---

func bindNvsNamespaceFlag(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.NVSNamespace, "nvs-namespace", c.NVSNamespace, "NVSentinel namespace")
}

func bindMonitoringNamespaceFlag(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.MonitoringNamespace, "monitoring-namespace", c.MonitoringNamespace, "kube-prometheus-stack namespace")
}

func bindKwokNamespaceFlag(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.KWOKNamespace, "kwok-namespace", c.KWOKNamespace, "KWOK controller namespace")
}

func bindJanitorNamespaceFlag(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.JanitorNamespace, "janitor-namespace", c.JanitorNamespace, "janitor controller namespace")
}

func bindCertManagerNamespaceFlag(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.CertManagerNamespace, "cert-manager-namespace", c.CertManagerNamespace, "cert-manager namespace")
}

func bindResultsFlag(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.ResultsDir, "results-dir", c.ResultsDir, "directory for JSON/JUnit/report artifacts")
}

func bindMaxAPIServerP99Flag(fs *flag.FlagSet, c *Config) {
	fs.Float64Var(&c.MaxAPIServerP99, "max-apiserver-p99", c.MaxAPIServerP99, "apiserver p99 latency guardrail (seconds)")
}

func bindNodePrefixFlag(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.NodePrefix, "node-prefix", c.NodePrefix, "simulated node name prefix")
}

func bindMongoTLSSecretFlag(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.MongoTLSSecret, "mongo-tls-secret", c.MongoTLSSecret, "cert-manager TLS secret for mTLS/X.509 (empty disables auto-TLS)")
}

// --- grouped binders ---

// bindPromFlags registers Prometheus discovery: commands that run PromQL through
// the API-server proxy (nodes scale/ceiling, report, pool sweeps).
func bindPromFlags(fs *flag.FlagSet, c *Config) {
	bindMonitoringNamespaceFlag(fs, c)
	fs.StringVar(&c.MonPromSvc, "prom-service", c.MonPromSvc, "Prometheus service name")
	fs.StringVar(&c.MonPromPort, "prom-port", c.MonPromPort, "Prometheus service port")
}

// bindMongoFlags registers MongoDB discovery used when deriving a connection
// (events inject/reconcile/coldstart). Used only when a direct URI is not given.
func bindMongoFlags(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.MongoService, "mongo-service", c.MongoService, "MongoDB headless service name")
	fs.StringVar(&c.MongoReplicaSet, "mongo-replica-set", c.MongoReplicaSet, "MongoDB replica set name (empty = standalone)")
	fs.StringVar(&c.MongoPort, "mongo-port", c.MongoPort, "MongoDB port")
	bindMongoTLSSecretFlag(fs, c)
	fs.StringVar(&c.MongoRootSecret, "mongo-root-secret", c.MongoRootSecret, "secret holding a root password (non-TLS installs)")
}

// bindNodeGuardrailFlags registers the node/control-plane guardrails and node
// readiness timeout used by the node-scaling commands (nodes scale/ceiling).
func bindNodeGuardrailFlags(fs *flag.FlagSet, c *Config) {
	bindMaxAPIServerP99Flag(fs, c)
	fs.IntVar(&c.NodeReadyTO, "node-ready-timeout", c.NodeReadyTO, "seconds to wait for nodes to become Ready")
	fs.Float64Var(&c.MaxClusterCPUPct, "max-cluster-cpu-pct", c.MaxClusterCPUPct, "real-node CPU utilization guardrail (fraction)")
	fs.Float64Var(&c.MaxClusterMemPct, "max-cluster-mem-pct", c.MaxClusterMemPct, "real-node memory utilization guardrail (fraction)")
}

// bindInjectorFlags registers the flags for commands that stage this binary into
// resident injector pods (events inject/reconcile/coldstart).
func bindInjectorFlags(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.HarnessBin, "harness-bin", c.HarnessBin, "local linux/amd64 harnessctl staged into injectors (default: running binary)")
}

// bindVersionFlags registers the version-aware bringup targets (bringup only):
// empty = leave installed / use install-script default.
func bindVersionFlags(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.NVSChartVersion, "nvs-chart-version", c.NVSChartVersion, "target NVSentinel version for version-aware bringup (empty = leave installed)")
	fs.StringVar(&c.KWOKVersion, "kwok-version", c.KWOKVersion, "target KWOK version for version-aware bringup (empty = leave installed)")
	fs.StringVar(&c.CertManagerVersion, "cert-manager-version", c.CertManagerVersion, "target cert-manager version for version-aware bringup (empty = leave installed)")
	fs.StringVar(&c.MetricsServerVersion, "metrics-server-version", c.MetricsServerVersion, "target metrics-server version for version-aware bringup (empty = leave installed)")
}

// bindNodeShapeFlags registers KWOK node-shaping flags. Commands that create or
// ramp nodes (nodes scale / nodes ceiling) call this; injectors that only spread
// event load across node names bind their own -node-prefix.
func bindNodeShapeFlags(fs *flag.FlagSet, c *Config) {
	bindNodePrefixFlag(fs, c)
	fs.IntVar(&c.NodeBatch, "node-batch", c.NodeBatch, "node creation batch size")
	fs.IntVar(&c.GPUCount, "gpu-count", c.GPUCount, "GPUs advertised per simulated node")
	fs.StringVar(&c.NodeCPU, "node-cpu", c.NodeCPU, "CPU advertised per simulated node")
	fs.StringVar(&c.NodeMemory, "node-memory", c.NodeMemory, "memory advertised per simulated node")
	fs.IntVar(&c.NodeMaxPods, "node-max-pods", c.NodeMaxPods, "max pods advertised per simulated node")
	fs.StringVar(&c.ProviderIDScheme, "provider-id-scheme", c.ProviderIDScheme, "spec.providerID scheme for KWOK nodes (empty = none; set on managed clusters)")
}

// envInt / envBool remain ONLY for a few deep internal tuning knobs (drain and
// monitor poll intervals, node-ready stall) that are not part of the documented
// CLI input surface and have no harness.env / flag entry. All user-facing
// configuration is set exclusively via command-line flags.

// internalMongoURIDefault returns the default for the primitive inject/reconcile
// -uri flag. It reads the MONGO_URI env var, which the distributed orchestrator
// sets inside the resident-injector pod (execShEnv) so the discovered mTLS URI —
// which may carry credentials — is passed via env rather than argv (argv is
// visible in `ps`/logs). This is an internal mechanism, not user configuration;
// when unset it falls back to a local MongoDB.
func internalMongoURIDefault() string {
	if v := os.Getenv("MONGO_URI"); v != "" {
		return v
	}
	return "mongodb://localhost:27017"
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
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

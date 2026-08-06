#!/usr/bin/env bash
# Phase 0 Harness end-to-end driver for a single node count.
# Runs P0.2 -> P0.5 -> P0.3 (inject+reconcile) -> P0.4 -> report, each writing to
# results/<YYYY-MM-DD>/<N>/. Does NOT use the `ceiling` command for P0.2 (uses
# scale-nodes). Override the date folder with RUN_DATE=YYYY-MM-DD.
#
# Usage: run_e2e.sh <node-count>
set -uo pipefail

N="${1:?usage: run_e2e.sh <node-count>}"
HARN="$(cd "$(dirname "$0")" && pwd)"
BIN="$HARN/harnessctl/harnessctl"

# Connector density (connectors packed per real node). This is the dominant driver
# of MongoDB connection/mTLS load: at 50/node (~1600 connectors) the 1.5-core
# mongod loses its primary and the remediation pipeline crash-loops (see
# findings/2026-07-30-mongodb-saturation-under-connector-pool.md). Default kept low
# so Mongo stays healthy and fault→cordon→GPUReset can be validated end to end;
# override with PER_NODE_POD_LIMIT to probe the ceiling.
PER_NODE_POD_LIMIT="${PER_NODE_POD_LIMIT:-10}"

# Fraction of injected events that are fatal GPU XID faults (drive cordon/remediation).
# Empty => harness default (0.08). Set FATAL_FRACTION=1.0 to make every event fatal
# (fleet-wide remediation storm — note fault-quarantine's 50%/5m circuit breaker will
# likely cap cordoning near ~50% of the fleet rather than 100%).
FATAL_FRACTION="${FATAL_FRACTION:-}"

# Injection knobs (empty => harness defaults). FATAL_EVENT: node-reboot | gpu-reset.
# MECHANISM: grpc (through the platform-connector) | mongo (direct MongoDB insert).
# PATTERN: fleet-storm | flappy | single-node-burst. PROCESSING_STRATEGY: default |
# store-only | store-and-analyse | execute-remediation.
FATAL_EVENT="${FATAL_EVENT:-}"
MECHANISM="${MECHANISM:-}"
PATTERN="${PATTERN:-}"
PROCESSING_STRATEGY="${PROCESSING_STRATEGY:-}"

# harnessctl is now flags-only (AWS-CLI style: no env file). We still source
# harness.env because the Helm *install* shell scripts read a few vars from it
# (NVS_CHART, KWOK_VERSION, …) and because it carries the two behaviour-critical
# values below — which we then pass to harnessctl explicitly as flags.
set -a; source "$HARN/config/harness.env"; set +a

# Results are grouped by run date, then node count: results/<YYYY-MM-DD>/<N>/.
# Override the date with RUN_DATE (e.g. to append to an earlier day's set).
RUN_DATE="${RUN_DATE:-$(date +%F)}"
RESULTS_DIR="$HARN/results/$RUN_DATE/$N"
mkdir -p "$RESULTS_DIR"
LOG="$RESULTS_DIR/run.log"

# --- harnessctl flag sets (replace the old env-var config surface) ------------
# Flags are scoped per command (each subcommand only accepts the flags it reads),
# so we build focused arrays rather than one blanket set:
#   RESULTS - artifact dir; accepted by every command that writes results
#             (scale/pool/janitor/inject/reconcile/report) but NOT stack cleanup.
#   MON     - monitoring-namespace override; only for PromQL commands
#             (scale/pool/report). Defaults (prometheus/prometheus-prometheus/9090)
#             are baked into harnessctl, so pass an override only when it differs.
RESULTS=(--results-dir "$RESULTS_DIR")
MON=()
[ "${MONITORING_NAMESPACE:-prometheus}" != "prometheus" ] && MON+=(--monitoring-namespace "$MONITORING_NAMESPACE")

# P0.2 node creation. --provider-id-scheme is REQUIRED on managed clusters
# (AKS/EKS/GKE) or KWOK nodes are reaped by the cloud node-lifecycle controller.
SCALE_FLAGS=()
[ -n "${KWOK_PROVIDER_ID_SCHEME:-}" ] && SCALE_FLAGS+=(--provider-id-scheme "$KWOK_PROVIDER_ID_SCHEME")

# P0.3 injection strategy (only pass overrides; harnessctl carries the defaults).
INJECT_FLAGS=()
[ -n "$FATAL_FRACTION" ] && INJECT_FLAGS+=(--fatal-fraction "$FATAL_FRACTION")
[ -n "$FATAL_EVENT" ] && INJECT_FLAGS+=(--fatal-event "$FATAL_EVENT")
[ -n "$MECHANISM" ] && INJECT_FLAGS+=(--mechanism "$MECHANISM")
[ -n "$PATTERN" ] && INJECT_FLAGS+=(--pattern "$PATTERN")
[ -n "$PROCESSING_STRATEGY" ] && INJECT_FLAGS+=(--processing-strategy "$PROCESSING_STRATEGY")

log() { echo "[$(date +%T)] $*" | tee -a "$LOG"; }
run() { # phase-label command...
  local label="$1"; shift
  log "===== $label ====="
  if "$@" >>"$LOG" 2>&1; then log "$label OK"; else log "$label FAILED (exit $?)"; return 1; fi
}

log "######## Phase 0 E2E for $N nodes -> results/$RUN_DATE/$N/ ########"

# Clean slate: delete prior nodes (+restart kwok-controller to clear stale lease
# cache), gc orphaned janitor CRs, and tear down the previous connector pool.
run "cleanup" "$BIN" stack cleanup --pool || exit 1

# P0.2: scale to N nodes (nodes scale, NOT nodes ceiling).
run "P0.2 nodes scale --count $N" "$BIN" nodes scale --count "$N" "${SCALE_FLAGS[@]}" "${RESULTS[@]}" "${MON[@]}" || exit 1

# P0.5: deploy connector pool (+ resident injectors), PER_NODE_POD_LIMIT/real-node.
run "P0.5 pool create (per-node-pod-limit=$PER_NODE_POD_LIMIT)" "$BIN" pool create --per-node-pod-limit "$PER_NODE_POD_LIMIT" "${RESULTS[@]}" "${MON[@]}" || exit 1

# P0.4: janitor reboot + GPU reset on a KWOK node — BEFORE inject, so janitor check
# targets a quiet fleet. Under a fatal storm (e.g. FATAL_FRACTION=1.0) its target
# node could already be mid-remediation and the check would time out.
run "P0.4 janitor check" "$BIN" janitor check "${RESULTS[@]}" || exit 1

# P0.3: fire all injectors (distributed), capture run-id, then reconcile.
log "===== P0.3 events inject ====="
RID="$("$BIN" events inject "${INJECT_FLAGS[@]}" "${RESULTS[@]}" 2>>"$LOG" | tail -1)"
log "P0.3 inject run-id=$RID"
if [ -z "$RID" ]; then log "P0.3 inject produced no run-id"; exit 1; fi
run "P0.3 events reconcile --run-id $RID" "$BIN" events reconcile --run-id "$RID" "${RESULTS[@]}" || exit 1

# Report.
FF_TITLE=""
[ -n "$FATAL_FRACTION" ] && FF_TITLE="$FF_TITLE, fatal-fraction=$FATAL_FRACTION"
[ -n "$FATAL_EVENT" ] && FF_TITLE="$FF_TITLE, fatal-event=$FATAL_EVENT"
[ -n "$MECHANISM" ] && FF_TITLE="$FF_TITLE, mechanism=$MECHANISM"
[ -n "$PATTERN" ] && FF_TITLE="$FF_TITLE, pattern=$PATTERN"
run "report" "$BIN" stack report --title "Phase 0 Harness E2E — Azure ($N nodes$FF_TITLE)" --window 1h "${RESULTS[@]}" "${MON[@]}" || exit 1

log "######## DONE $N -> $RESULTS_DIR/report.md (run-id=$RID) ########"

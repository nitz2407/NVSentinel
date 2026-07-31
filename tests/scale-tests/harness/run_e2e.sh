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

set -a; source "$HARN/config/harness.env"; set +a
export MONITORING_NAMESPACE=prometheus PROM_SERVICE=prometheus-prometheus PROM_PORT=9090
# Apply the fatal-fraction override AFTER sourcing harness.env so it wins.
[ -n "$FATAL_FRACTION" ] && export HARNESS_FATAL_FRACTION="$FATAL_FRACTION"
# Results are grouped by run date, then node count: results/<YYYY-MM-DD>/<N>/.
# Override the date with RUN_DATE (e.g. to append to an earlier day's set).
RUN_DATE="${RUN_DATE:-$(date +%F)}"
export HARNESS_RESULTS_DIR="$HARN/results/$RUN_DATE/$N"
mkdir -p "$HARNESS_RESULTS_DIR"
LOG="$HARNESS_RESULTS_DIR/run.log"

log() { echo "[$(date +%T)] $*" | tee -a "$LOG"; }
run() { # phase-label command...
  local label="$1"; shift
  log "===== $label ====="
  if "$@" >>"$LOG" 2>&1; then log "$label OK"; else log "$label FAILED (exit $?)"; return 1; fi
}

log "######## Phase 0 E2E for $N nodes -> results/$RUN_DATE/$N/ ########"

# Clean slate: delete prior nodes (+restart kwok-controller to clear stale lease
# cache), gc orphaned janitor CRs, and tear down the previous connector pool.
run "cleanup" "$BIN" cleanup -pool || exit 1

# P0.2: scale to N nodes (scale-nodes, NOT ceiling).
run "P0.2 scale-nodes -count $N" "$BIN" scale-nodes -count "$N" || exit 1

# P0.5: deploy connector pool (+ resident injectors), PER_NODE_POD_LIMIT/real-node.
run "P0.5 connector-pool (per-node-pod-limit=$PER_NODE_POD_LIMIT)" "$BIN" connector-pool -per-node-pod-limit "$PER_NODE_POD_LIMIT" || exit 1

# P0.3: fire all injectors (distributed), capture run-id, then reconcile.
log "===== P0.3 inject ====="
RID="$("$BIN" inject 2>>"$LOG" | tail -1)"
log "P0.3 inject run-id=$RID"
if [ -z "$RID" ]; then log "P0.3 inject produced no run-id"; exit 1; fi
run "P0.3 reconcile -run-id $RID" "$BIN" reconcile -run-id "$RID" || exit 1

# P0.4: janitor reboot + GPU reset on a KWOK node.
run "P0.4 janitor-check" "$BIN" janitor-check || exit 1

# Report.
FF_TITLE=""; [ -n "$FATAL_FRACTION" ] && FF_TITLE=", fatal-fraction=$FATAL_FRACTION"
run "report" "$BIN" report -title "Phase 0 Harness E2E — Azure ($N nodes$FF_TITLE)" -window 1h || exit 1

log "######## DONE $N -> $HARNESS_RESULTS_DIR/report.md (run-id=$RID) ########"

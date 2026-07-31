#!/usr/bin/env bash
# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Shared helpers for the NVSentinel scale-test harness. Source this at the top
# of every script:
#
#   HARNESS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
#   source "${HARNESS_ROOT}/lib/common.sh"
#
# Everything here is idempotent-friendly and safe to re-run.

set -euo pipefail

# Resolve harness root regardless of caller CWD.
HARNESS_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export HARNESS_ROOT="$(cd "${HARNESS_LIB_DIR}/.." && pwd)"

# Load central config exactly once.
if [[ -z "${_HARNESS_ENV_LOADED:-}" ]]; then
  # shellcheck source=/dev/null
  source "${HARNESS_ROOT}/config/harness.env"
  export _HARNESS_ENV_LOADED=1
fi

# Fixed cluster constants — the same on every target cluster, so they are not
# surfaced in harness.env. Still overridable via the environment for the rare
# non-standard cluster. (harnessctl carries the identical defaults in config.go.)
export NVS_NAMESPACE="${NVS_NAMESPACE:-nvsentinel}"
export MONITORING_NAMESPACE="${MONITORING_NAMESPACE:-monitoring}"
export CERT_MANAGER_NAMESPACE="${CERT_MANAGER_NAMESPACE:-cert-manager}"
# KWOK's upstream kwok.yaml is applied verbatim and hardcodes namespace
# kube-system, so the controller can only ever land there; this just matches it.
export KWOK_NAMESPACE="${KWOK_NAMESPACE:-kube-system}"

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
_ts() { date -u +"%Y-%m-%dT%H:%M:%SZ"; }
log()   { printf '%s [INFO]  %s\n'  "$(_ts)" "$*" >&2; }
warn()  { printf '%s [WARN]  %s\n'  "$(_ts)" "$*" >&2; }
err()   { printf '%s [ERROR] %s\n'  "$(_ts)" "$*" >&2; }
fatal() { err "$*"; exit 1; }

# Section banner for readable multi-step logs.
step() { printf '\n%s ===== %s =====\n' "$(_ts)" "$*" >&2; }

# ---------------------------------------------------------------------------
# kubectl / helm wrappers — use whatever context the caller has set. Setting the
# correct context before running is the operator's responsibility.
# ---------------------------------------------------------------------------
kc()  { kubectl "$@"; }
hlm() { helm "$@"; }

# ---------------------------------------------------------------------------
# Preconditions
# ---------------------------------------------------------------------------
require_cmd() {
  local missing=0 c
  for c in "$@"; do
    if ! command -v "$c" >/dev/null 2>&1; then
      err "required command not found: $c"
      missing=1
    fi
  done
  [[ "$missing" -eq 0 ]] || fatal "install the missing tools above and retry"
}

ensure_namespace() {
  local ns="$1"
  if ! kc get namespace "$ns" >/dev/null 2>&1; then
    log "creating namespace ${ns}"
    kc create namespace "$ns"
  fi
}

# ---------------------------------------------------------------------------
# Waiters
# ---------------------------------------------------------------------------
# wait_for <timeout-seconds> <interval-seconds> <description> <command...>
# Retries <command> until it exits 0 or the timeout elapses.
wait_for() {
  local timeout="$1" interval="$2" desc="$3"; shift 3
  local deadline=$(( $(date +%s) + timeout ))
  log "waiting (<=${timeout}s) for: ${desc}"
  while true; do
    if "$@"; then
      log "ready: ${desc}"
      return 0
    fi
    if [[ $(date +%s) -ge $deadline ]]; then
      err "timeout after ${timeout}s waiting for: ${desc}"
      return 1
    fi
    sleep "$interval"
  done
}

# ---------------------------------------------------------------------------
# Results
# ---------------------------------------------------------------------------
ensure_results_dir() {
  mkdir -p "${HARNESS_RESULTS_DIR}"
}

# record_result <check-id> <PASS|FAIL> <message>
# Appends a line to the Phase 0 results ledger.
record_result() {
  ensure_results_dir
  local id="$1" status="$2" msg="$3"
  printf '%s\t%s\t%s\t%s\n' "$(_ts)" "$id" "$status" "$msg" \
    >> "${HARNESS_RESULTS_DIR}/phase0-results.tsv"
  if [[ "$status" == "PASS" ]]; then
    log "[$id] PASS - $msg"
  else
    err "[$id] FAIL - $msg"
  fi
}

# ---------------------------------------------------------------------------
# Prometheus helper — run an instant query via the in-cluster Prometheus.
# Prints the first scalar/vector value or empty string.
# ---------------------------------------------------------------------------
prom_query() {
  local query="$1"
  # Matches fullnameOverride=prometheus in values-kube-prometheus-stack.yaml.
  local svc="${PROM_SERVICE:-prometheus-prometheus}"
  # Uses `kubectl exec` into the prometheus pod's curl-less environment is
  # unreliable, so we port-forward briefly. Callers that need many queries
  # should hold a port-forward open themselves.
  local port=19090
  kc -n "${MONITORING_NAMESPACE}" port-forward "svc/${svc}" ${port}:9090 >/dev/null 2>&1 &
  local pf_pid=$!
  # shellcheck disable=SC2064
  trap "kill ${pf_pid} >/dev/null 2>&1 || true" RETURN
  sleep 3
  curl -sG "http://localhost:${port}/api/v1/query" \
    --data-urlencode "query=${query}" 2>/dev/null \
    | { command -v jq >/dev/null 2>&1 && jq -r '.data.result[0].value[1] // ""' || cat; }
}

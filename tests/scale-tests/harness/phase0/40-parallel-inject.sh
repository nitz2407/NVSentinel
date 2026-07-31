#!/usr/bin/env bash
# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
#
# ---------------------------------------------------------------------------
# P0.5 on-demand PARALLEL injection into the connector pool.
#
# This DOES NOT create any pods. The pool deploy already co-located a PERSISTENT
# injector on every connector node (the nvs-harness-pool-injector DaemonSet,
# stock alpine). This script:
#   1. stages the LOCALLY BUILT harnessctl binary ONCE onto each node's hostPath
#      (${SOCKET_ROOT}/bin/harnessctl) via the resident injector pod — it
#      survives injector-pod restarts and is reused by every later run (NO image
#      is built or pulled for the binary);
#   2. triggers injection by `exec`-ing each node's injector to drive a disjoint
#      node shard into every connector socket present on that node.
#
# One injector per node handles ALL connectors packed on that node.
#
# Prereq: the pool + injectors are deployed:
#     harnessctl connector-pool [-per-node-pod-limit <N>]
#
# Usage (set your kubectl context first; the script uses the current one):
#     ./phase0/40-parallel-inject.sh
#
# Common overrides:
#     COUNT=200 RATE=20 FATAL_FRACTION=1.0 RUN_ID=my-run ./phase0/40-parallel-inject.sh
#     HARNESS_BIN=/path/to/harnessctl   # skip the build, use this binary
#     FORCE_STAGE=1                     # re-stage the binary even if up to date
# ---------------------------------------------------------------------------

HARNESS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=/dev/null
source "${HARNESS_ROOT}/lib/common.sh"

require_cmd kubectl

POOL_NAME="${POOL_NAME:-nvs-harness-connector-pool}"
POOL_LABEL="${POOL_LABEL:-nvs-harness/pool}"
INJECTOR_LABEL="${INJECTOR_LABEL:-nvs-harness/pool-injector}"
SOCKET_ROOT="${SOCKET_ROOT:-/var/run/nvs-harness-pool}"
# Binary lives on the node hostPath (persistent across injector-pod restarts).
BIN_ON_NODE="/pool-sockets/bin/harnessctl"

NODE_PREFIX="${KWOK_NODE_PREFIX:-kwok-gpu}"
COUNT="${COUNT:-${CONNECTOR_POOL_PER_CONN_COUNT:-200}}"
RATE="${RATE:-${CONNECTOR_POOL_PER_CONN_RATE:-20}}"
FATAL_FRACTION="${FATAL_FRACTION:-0.08}"
RUN_ID="${RUN_ID:-p05-$(date +%s)}"
RUN_LABEL="${HARNESS_RUN_LABEL:-nvs_harness_run}"
ID_LABEL="${HARNESS_ID_LABEL:-nvs_harness_id}"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}" 2>/dev/null || true' EXIT

# --- 1. Build the injector binary locally (linux/amd64), unless one is given ---
if [[ -z "${HARNESS_BIN:-}" ]]; then
  require_cmd go
  HARNESS_BIN="${WORKDIR}/harnessctl"
  step "building harnessctl (linux/amd64, stripped) — no image"
  ( cd "${HARNESS_ROOT}/harnessctl" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "${HARNESS_BIN}" . )
fi
[[ -x "${HARNESS_BIN}" ]] || fatal "harnessctl binary not found/executable: ${HARNESS_BIN}"
WANT_SUM="$(sha256sum "${HARNESS_BIN}" | awk '{print $1}')"
log "using binary: ${HARNESS_BIN} (sha256=${WANT_SUM:0:12}…)"

# --- 2. Recover the shard geometry from the deployed pool ---
NPC="$(kc -n "${NVS_NAMESPACE}" get statefulset "${POOL_NAME}" \
        -o jsonpath='{.metadata.annotations.nvs-harness/nodes-per-connector}' 2>/dev/null || true)"
[[ -n "${NPC}" ]] || fatal "pool ${POOL_NAME} not found or missing nodes-per-connector annotation (deploy with: harnessctl connector-pool)"
log "shard geometry: nodes-per-connector=${NPC} count/conn=${COUNT} rate/conn=${RATE} fatal-fraction=${FATAL_FRACTION} run-id=${RUN_ID}"

# --- 3. Map node -> resident injector pod (from the persistent DaemonSet) ---
declare -A INJ_POD
while read -r pod node; do
  [[ -z "$pod" || -z "$node" ]] && continue
  INJ_POD["$node"]="$pod"
done < <(kc -n "${NVS_NAMESPACE}" get pods -l "${INJECTOR_LABEL}=true" \
            --field-selector=status.phase=Running \
            -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.nodeName}{"\n"}{end}' 2>/dev/null)
[[ "${#INJ_POD[@]}" -gt 0 ]] || fatal "no running injector pods for ${INJECTOR_LABEL}=true (deploy the pool first: harnessctl connector-pool)"

# --- 4. Group running pool pods by node -> "ord ord ord" ---
declare -A NODE_ORDS
TOTAL_CONN=0
while read -r pod node; do
  [[ -z "$pod" || -z "$node" ]] && continue
  ord="${pod##*-}"
  NODE_ORDS["$node"]="${NODE_ORDS[$node]:-} ${ord}"
  TOTAL_CONN=$((TOTAL_CONN + 1))
done < <(kc -n "${NVS_NAMESPACE}" get pods -l "${POOL_LABEL}=${POOL_NAME}" \
            --field-selector=status.phase=Running \
            -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.nodeName}{"\n"}{end}' 2>/dev/null)

NODE_COUNT="${#NODE_ORDS[@]}"
[[ "${NODE_COUNT}" -gt 0 ]] || fatal "no running pool pods found for ${POOL_LABEL}=${POOL_NAME}"
step "parallel inject: ${TOTAL_CONN} connectors across ${NODE_COUNT} nodes (resident injector per node, concurrent)"

# --- Per-node worker: stage binary once into the resident injector, inject shard ---
inject_node() {
  local node="$1" ords="$2" idx="$3"
  local hp="${INJ_POD[$node]}"
  local out="${WORKDIR}/${idx}.out"
  if [[ -z "${hp}" ]]; then
    echo "ACKED=0 NODE=${node} ERR=no-injector" > "${out}"
    warn "node ${node}: no resident injector pod"; return
  fi

  # Stage the binary ONCE per node (idempotent via sha256). It lands on the node
  # hostPath, so subsequent runs skip this. Atomic mv avoids a torn binary.
  local have=""
  if [[ "${FORCE_STAGE:-0}" != "1" ]]; then
    have="$(kc -n "${NVS_NAMESPACE}" exec "${hp}" -- sh -c "sha256sum ${BIN_ON_NODE} 2>/dev/null | awk '{print \$1}'" 2>/dev/null || true)"
  fi
  if [[ "${have}" != "${WANT_SUM}" ]]; then
    local staged=0 attempt
    for attempt in 1 2 3 4; do
      if kc -n "${NVS_NAMESPACE}" cp "${HARNESS_BIN}" "${hp}:${BIN_ON_NODE}.tmp" >/dev/null 2>&1; then
        if kc -n "${NVS_NAMESPACE}" exec "${hp}" -- sh -c "chmod +x ${BIN_ON_NODE}.tmp && mv -f ${BIN_ON_NODE}.tmp ${BIN_ON_NODE} && test -x ${BIN_ON_NODE}" >/dev/null 2>&1; then
          staged=1; break
        fi
      fi
      sleep $((attempt * 5))
    done
    if [[ "${staged}" != "1" ]]; then
      echo "ACKED=0 NODE=${node} ERR=cp-failed" > "${out}"
      warn "node ${node}: binary staging failed after retries"; return
    fi
  fi

  # Inject every connector shard on this node (serial within node; nodes parallel).
  local logs
  logs="$(kc -n "${NVS_NAMESPACE}" exec "${hp}" -- env \
      ORDS="${ords}" NPC="${NPC}" PREFIX="${NODE_PREFIX}" COUNT="${COUNT}" RATE="${RATE}" \
      FATAL="${FATAL_FRACTION}" RUNID="${RUN_ID}" RUNLABEL="${RUN_LABEL}" IDLABEL="${ID_LABEL}" \
      BIN="${BIN_ON_NODE}" POOL="${POOL_NAME}" \
      sh -c '
set +e
for ORD in $ORDS; do
  SOCK="/pool-sockets/${POOL}-$ORD/nvsentinel.sock"
  if [ ! -S "$SOCK" ]; then echo "skip ord=$ORD (no socket)"; continue; fi
  OFF=$((ORD * NPC)); END=$((OFF + NPC))
  F="/tmp/n-$ORD.txt"; : > "$F"; i=$OFF
  while [ "$i" -lt "$END" ]; do echo "$PREFIX-$i" >> "$F"; i=$((i + 1)); done
  "$BIN" inject -socket="$SOCK" -nodes-from="$F" -count="$COUNT" -rate="$RATE" \
    -fatal-fraction="$FATAL" -run-id="$RUNID" -run-label="$RUNLABEL" -id-label="$IDLABEL" \
    -ledger=/tmp/led-$ORD.jsonl 2>&1 | grep "done: sent="
done
' 2>&1)"
  local acked
  acked="$(printf '%s\n' "${logs}" | sed -n 's/.*acked=\([0-9]*\).*/\1/p' | awk '{s+=$1} END{print s+0}')"
  echo "ACKED=${acked} NODE=${node}" > "${out}"
  log "node ${node}: injected shard (acked=${acked})"
}

# --- 5. Fan out: one worker per node. Bound concurrency (MAX_PARALLEL). ---
MAX_PARALLEL="${MAX_PARALLEL:-10}"
idx=0
for node in "${!NODE_ORDS[@]}"; do
  inject_node "${node}" "${NODE_ORDS[$node]}" "${idx}" &
  idx=$((idx + 1))
  while [[ "$(jobs -r | wc -l)" -ge "${MAX_PARALLEL}" ]]; do sleep 1; done
done
wait

# --- 6. Summarize ---
TOTAL_ACKED=0
while read -r f; do
  a="$(sed -n 's/^ACKED=\([0-9]*\).*/\1/p' "$f")"
  TOTAL_ACKED=$((TOTAL_ACKED + ${a:-0}))
done < <(find "${WORKDIR}" -maxdepth 1 -name '*.out')

EXPECT=$((TOTAL_CONN * COUNT))
step "parallel inject complete"
log "run-id=${RUN_ID}  connectors=${TOTAL_CONN}  expected=${EXPECT}  acked=${TOTAL_ACKED}"
log "reconcile with:  harnessctl reconcile -run-id=${RUN_ID} -expect-injected=${EXPECT} ..."
echo "${RUN_ID}"

#!/usr/bin/env bash
# P0.1 (KWOK): install the KWOK controller + default stages into the real
# cluster (S1), then layer the harness custom stages.
# Idempotent: uses `kubectl apply`.

HARNESS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../lib/common.sh
source "${HARNESS_ROOT}/lib/common.sh"

step "P0.1 KWOK: controller + stages (${KWOK_VERSION})"
require_cmd kubectl

KWOK_RELEASE="https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}"

log "applying KWOK CRDs + controller"
kc apply -f "${KWOK_RELEASE}/kwok.yaml"

log "applying KWOK upstream default stages (node heartbeat, pod lifecycle)"
kc apply -f "${KWOK_RELEASE}/stage-fast.yaml"

log "waiting for KWOK controller rollout"
wait_for 300 10 "kwok-controller rollout" \
  kc -n "${KWOK_NAMESPACE}" rollout status deploy/kwok-controller --timeout=10s

# Render + apply custom stages with the configured job-completion delay.
delay_ms=$(( KWOK_JOB_COMPLETE_DELAY * 1000 ))
rendered="$(mktemp)"
trap 'rm -f "${rendered}"' EXIT
sed "s/__JOB_COMPLETE_DELAY__/${delay_ms}/g" \
  "${HARNESS_ROOT}/kwok/stages-custom.yaml" > "${rendered}"

log "applying harness custom stages (graceful delete + janitor job completion)"
kc apply -f "${rendered}"

log "KWOK ready. Tune the controller's client QPS/burst + node lease durations"
log "and RECORD them (S1) before running Phase 2 at the node ceiling."

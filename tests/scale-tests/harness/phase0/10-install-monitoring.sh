#!/usr/bin/env bash
# P0.1 (monitoring): install kube-prometheus-stack tuned for KWOK scale.
# Idempotent: safe to re-run; uses `helm upgrade --install`.

HARNESS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../lib/common.sh
source "${HARNESS_ROOT}/lib/common.sh"

step "P0.1 monitoring: kube-prometheus-stack"
require_cmd kubectl helm

ensure_namespace "${MONITORING_NAMESPACE}"

log "adding prometheus-community helm repo (idempotent)"
hlm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
hlm repo update prometheus-community >/dev/null

log "installing/upgrading kube-prometheus-stack ${KPS_CHART_VERSION}"
hlm upgrade --install prometheus prometheus-community/kube-prometheus-stack \
  --namespace "${MONITORING_NAMESPACE}" \
  --version "${KPS_CHART_VERSION}" \
  --values "${HARNESS_ROOT}/monitoring/values-kube-prometheus-stack.yaml" \
  --wait --timeout 15m

log "waiting for prometheus + kube-state-metrics to be ready"
wait_for 300 10 "prometheus operator rollout" \
  kc -n "${MONITORING_NAMESPACE}" rollout status deploy/prometheus-kube-prometheus-operator --timeout=10s
wait_for 300 10 "kube-state-metrics rollout" \
  kc -n "${MONITORING_NAMESPACE}" rollout status deploy/prometheus-kube-state-metrics --timeout=10s

log "monitoring stack ready in namespace ${MONITORING_NAMESPACE}"

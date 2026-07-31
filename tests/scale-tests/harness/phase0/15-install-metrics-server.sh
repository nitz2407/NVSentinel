#!/usr/bin/env bash
# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
#
# ---------------------------------------------------------------------------
# P0.1 (metrics-server): install metrics-server so P0.2 can measure real-node
# CPU/memory — the cluster-resource guardrail that distinguishes the real
# ceiling (cluster saturation) from the harness limit (KWOK controller).
#
# Managed clusters (AKS/EKS/GKE) ship metrics-server already, so `harnessctl
# bringup` detects it and skips this script; bare clusters (Kind) need it.
#
# Idempotent: `kubectl apply` plus a patch guarded by a grep so re-runs are
# no-ops.
# ---------------------------------------------------------------------------

HARNESS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../lib/common.sh
source "${HARNESS_ROOT}/lib/common.sh"

# Pin the release; override via env. Kept out of harness.env to stay lean.
METRICS_SERVER_VERSION="${METRICS_SERVER_VERSION:-v0.7.2}"

step "P0.1 metrics-server: install ${METRICS_SERVER_VERSION}"
require_cmd kubectl

manifest="https://github.com/kubernetes-sigs/metrics-server/releases/download/${METRICS_SERVER_VERSION}/components.yaml"

log "applying metrics-server ${METRICS_SERVER_VERSION}"
kc apply -f "${manifest}"

# Self-signed kubelet serving certs (Kind and many bare clusters) make the
# default metrics-server unable to scrape kubelets. Add --kubelet-insecure-tls
# once, idempotently.
current_args="$(kc -n kube-system get deploy metrics-server \
  -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null || true)"
if ! grep -q -- '--kubelet-insecure-tls' <<<"${current_args}"; then
  log "enabling --kubelet-insecure-tls (self-signed kubelet serving certs)"
  kc -n kube-system patch deploy metrics-server --type=json \
    -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
fi

log "waiting for metrics-server rollout"
wait_for 180 10 "metrics-server rollout" \
  kc -n kube-system rollout status deploy/metrics-server --timeout=10s

# Prove the metrics API actually serves node metrics before returning, so P0.2
# never races an Available-but-not-yet-serving metrics.k8s.io.
metrics_ready() { kubectl top nodes >/dev/null 2>&1; }
log "waiting for metrics.k8s.io to serve node metrics"
wait_for 120 10 "metrics.k8s.io node metrics" metrics_ready

log "metrics-server ready in namespace kube-system"

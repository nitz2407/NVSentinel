#!/usr/bin/env bash
# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
#
# ---------------------------------------------------------------------------
# P0.1 (cert-manager): install cert-manager before NVSentinel.
#
# NVSentinel's admission webhooks and the janitor CRDs are served with
# cert-manager-issued certificates, so cert-manager MUST be Ready before
# 30-install-nvsentinel.sh runs. This was previously a manual prerequisite;
# folding it into bring-up makes P0.1 non-interactive and idempotent.
#
# Idempotent: safe to re-run; uses `helm upgrade --install` and only reinstalls
# CRDs it does not already own.
# ---------------------------------------------------------------------------

HARNESS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../lib/common.sh
source "${HARNESS_ROOT}/lib/common.sh"

step "P0.1 cert-manager: helm install ${CERT_MANAGER_VERSION}"
require_cmd kubectl helm

ensure_namespace "${CERT_MANAGER_NAMESPACE}"

log "adding jetstack helm repo (idempotent)"
hlm repo add jetstack https://charts.jetstack.io >/dev/null 2>&1 || true
hlm repo update jetstack >/dev/null

log "installing/upgrading cert-manager ${CERT_MANAGER_VERSION} (with CRDs)"
hlm upgrade --install cert-manager jetstack/cert-manager \
  --namespace "${CERT_MANAGER_NAMESPACE}" \
  --version "${CERT_MANAGER_VERSION}" \
  --set crds.enabled=true \
  --wait --timeout 10m

log "waiting for cert-manager control plane to be ready"
for d in cert-manager cert-manager-webhook cert-manager-cainjector; do
  wait_for 300 10 "${d} rollout" \
    kc -n "${CERT_MANAGER_NAMESPACE}" rollout status "deploy/${d}" --timeout=10s
done

# The webhook can accept a Deployment rollout as complete a moment before it is
# actually serving; a trivial self-signed Issuer server-dry-run round-trips the
# whole path so NVSentinel's install doesn't race the webhook coming up.
probe_manifest="$(mktemp)"
trap 'rm -f "${probe_manifest}"' EXIT
cat >"${probe_manifest}" <<EOF
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: nvs-harness-cert-manager-probe
  namespace: ${CERT_MANAGER_NAMESPACE}
spec:
  selfSigned: {}
EOF

log "verifying the cert-manager webhook is admitting requests"
wait_for 120 5 "cert-manager webhook admitting Issuers" \
  kc apply --dry-run=server -f "${probe_manifest}"

log "cert-manager ready in namespace ${CERT_MANAGER_NAMESPACE}"

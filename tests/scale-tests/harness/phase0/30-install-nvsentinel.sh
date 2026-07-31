#!/usr/bin/env bash
# P0.1 (NVSentinel): install NVSentinel from the public Helm chart with the
# checked-in harness values. Idempotent via `helm upgrade --install`.

HARNESS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../lib/common.sh
source "${HARNESS_ROOT}/lib/common.sh"

step "P0.1 NVSentinel: helm install"
require_cmd kubectl helm

ensure_namespace "${NVS_NAMESPACE}"

version_args=()
if [[ -n "${NVS_CHART_VERSION}" ]]; then
  version_args=(--version "${NVS_CHART_VERSION}")
  log "installing NVSentinel ${NVS_CHART_VERSION} from ${NVS_CHART}"
elif [[ "${NVS_CHART}" == oci://* ]]; then
  # OCI registries expose no "latest" index; Helm fails with a cryptic
  # "Unable to locate any tags" error. Fail early with actionable guidance.
  fatal "NVS_CHART_VERSION is empty but ${NVS_CHART} is an OCI chart — pin a tag (e.g. v1.16.0) in config/harness.env"
else
  log "installing latest NVSentinel from ${NVS_CHART}"
fi

hlm upgrade --install nvsentinel "${NVS_CHART}" \
  --namespace "${NVS_NAMESPACE}" \
  "${version_args[@]}" \
  --values "${HARNESS_ROOT}/nvsentinel/values-harness.yaml" \
  --wait --timeout 20m

log "waiting for core singletons to be ready"
for d in fault-quarantine node-drainer fault-remediation; do
  wait_for 300 10 "${d} rollout" \
    kc -n "${NVS_NAMESPACE}" rollout status "deploy/${d}" --timeout=10s || \
    warn "${d} not ready (may be disabled or named differently in this chart version)"
done

log "NVSentinel installed. Verify the platform-connector DaemonSet scheduled"
log "ONLY on real nodes: kubectl -n ${NVS_NAMESPACE} get pods -o wide -l app.kubernetes.io/name=nvsentinel"

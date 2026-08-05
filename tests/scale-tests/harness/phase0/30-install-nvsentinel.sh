#!/usr/bin/env bash
# P0.1 (NVSentinel): install/upgrade NVSentinel from the public Helm chart with
# the checked-in harness values. Idempotent via `helm upgrade --install`, and
# self-healing across the failure modes a from-scratch or GitOps-managed cluster
# throws at it (broken cert-manager webhook, missing event-exporter OIDC secret,
# ArgoCD-owned resources, immutable StatefulSet/Job fields on a chart-lineage
# change, a stuck pending-upgrade lock) so no manual intervention is required.

HARNESS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../lib/common.sh
source "${HARNESS_ROOT}/lib/common.sh"

step "P0.1 NVSentinel: helm install"
require_cmd kubectl helm

ensure_namespace "${NVS_NAMESPACE}"

# Preflight: the cert-manager webhook must be FUNCTIONAL, not just present. The
# NVSentinel chart creates cert-manager Certificate/Issuer objects gated by that
# webhook, so a broken webhook fails the whole install. A long-lived cert-manager
# is a common culprit: its webhook serving cert is a 1-year self-signed cert that
# cert-manager does NOT rotate once it has already expired, so every webhook call
# then fails with "certificate has expired". Probe the webhook with a throwaway
# server-side dry-run; if it fails, regenerate the CA (delete the CA secret +
# restart the webhook) and re-probe. Idempotent and a no-op on a healthy cluster.
ensure_certmanager_webhook() {
  local ns="${CERT_MANAGER_NAMESPACE}"
  # Skip if cert-manager is not installed here (a fresh 25-install brings up a
  # valid webhook; nothing to heal).
  kc -n "${ns}" get deploy cert-manager-webhook >/dev/null 2>&1 || return 0

  probe_webhook() {
    kc apply --dry-run=server -f - >/dev/null 2>&1 <<'ISSUER'
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: harness-webhook-probe
  namespace: cert-manager
spec:
  selfSigned: {}
ISSUER
  }

  if probe_webhook; then
    log "cert-manager webhook healthy"
    return 0
  fi

  warn "cert-manager webhook not functional (commonly an expired webhook CA); regenerating CA + restarting webhook"
  kc -n "${ns}" delete secret cert-manager-webhook-ca --ignore-not-found
  kc -n "${ns}" rollout restart deploy/cert-manager-webhook >/dev/null 2>&1 || true
  kc -n "${ns}" rollout status deploy/cert-manager-webhook --timeout=120s 2>/dev/null || true

  # Re-probe with a short retry window (caBundle re-injection + serving-cert
  # regeneration take a few seconds after the pod comes back).
  local i
  for i in $(seq 1 18); do
    if probe_webhook; then
      log "cert-manager webhook healthy after regeneration"
      return 0
    fi
    sleep 5
  done
  warn "cert-manager webhook still failing after heal attempt; the NVSentinel install may fail on cert-manager-gated resources"
}
ensure_certmanager_webhook

# Preflight: event-exporter mounts the Secret `event-exporter-oidc-secret` as a
# REQUIRED (optional:false) volume, so without it the pod is stuck ContainerCreating
# (FailedMount) and blocks `helm --wait` — failing the whole install. Managed/tenant
# clusters provide the real OIDC client secret out of band; a self-contained cluster
# (Kind, fresh install) has none. Seed a PLACEHOLDER so the pod can start: it serves
# /healthz and becomes Ready; only actual event egress (which the harness does not
# exercise) fails. Only-if-absent, so a real secret an operator/GitOps already created
# is never overwritten. Idempotent and a no-op on a cluster that already has it.
ensure_event_exporter_oidc_secret() {
  local secret="event-exporter-oidc-secret"
  if kc -n "${NVS_NAMESPACE}" get secret "${secret}" >/dev/null 2>&1; then
    log "event-exporter OIDC secret present; leaving as-is"
    return 0
  fi
  warn "event-exporter OIDC secret '${secret}' missing; creating a PLACEHOLDER (harness only — real event egress will not function)"
  kc -n "${NVS_NAMESPACE}" create secret generic "${secret}" \
    --from-literal=oidc-client-secret=placeholder >/dev/null
}
ensure_event_exporter_oidc_secret

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

# A prior upgrade interrupted mid-flight leaves the release locked in
# `pending-upgrade`/`pending-install`/`pending-rollback`; a fresh `helm upgrade`
# then errors with "another operation (install/upgrade/rollback) is in progress".
# Roll back to the last deployed revision so this run can proceed unattended.
# `|| true` keeps `set -e` from aborting when there is no release yet.
rel_status="$(hlm status nvsentinel -n "${NVS_NAMESPACE}" 2>/dev/null | awk '/^STATUS:/{print $2}' || true)"
if [[ "${rel_status}" == pending-* ]]; then
  warn "release nvsentinel stuck in '${rel_status}' (prior interrupted upgrade); rolling back to last deployed revision"
  hlm rollback nvsentinel -n "${NVS_NAMESPACE}" 2>/dev/null || \
    hlm rollback nvsentinel 0 -n "${NVS_NAMESPACE}" 2>/dev/null || \
    warn "rollback failed; attempting the upgrade anyway"
fi

# helm_upgrade runs the install/upgrade. --take-ownership lets helm adopt objects
# a GitOps tool (ArgoCD/Kustomize) applied but that lack Helm's release metadata,
# so an existing out-of-band install upgrades cleanly instead of erroring with
# "invalid ownership metadata".
helm_upgrade() {
  hlm upgrade --install nvsentinel "${NVS_CHART}" \
    --namespace "${NVS_NAMESPACE}" \
    "${version_args[@]}" \
    --values "${HARNESS_ROOT}/nvsentinel/values-harness.yaml" \
    --take-ownership \
    --wait --timeout 20m
}

log "helm upgrade --install (attempt 1)"
if out="$(helm_upgrade 2>&1)"; then
  printf '%s\n' "${out}"
else
  printf '%s\n' "${out}"
  # Immutable field changes on adopt/upgrade (common when moving between chart
  # lineages, e.g. an old GitOps install on a different registry). Recreate the
  # affected MongoDB objects so helm can adopt/replace them, then retry once.
  if grep -qiE 'updates to statefulset|forbidden|is immutable|cannot patch.*with kind Job' <<<"${out}"; then
    log "immutable field change detected; recreating affected MongoDB objects"
    # StatefulSet: delete but ORPHAN its pods/PVCs (mongod stays up, no data loss)
    # so helm recreates the object adopting the live pods.
    while read -r sts; do
      [[ -n "${sts}" ]] || continue
      log "  kubectl delete ${sts} --cascade=orphan"
      kc -n "${NVS_NAMESPACE}" delete "${sts}" --cascade=orphan --ignore-not-found
    done < <(kc -n "${NVS_NAMESPACE}" get sts -o name 2>/dev/null | grep -iE 'mongo' || true)
    # Job: a Job's spec.template is immutable, so a chart bump that changes the
    # init-Job pod spec cannot be patched. Delete the one-shot DB-init Job(s) so
    # helm recreates them; the job is idempotent (guards each collection/user with
    # existence checks), so re-running is safe.
    while read -r job; do
      [[ -n "${job}" ]] || continue
      log "  kubectl delete ${job}"
      kc -n "${NVS_NAMESPACE}" delete "${job}" --ignore-not-found
    done < <(kc -n "${NVS_NAMESPACE}" get jobs -o name 2>/dev/null | grep -iE 'mongo' || true)
  fi
  log "helm upgrade --install (attempt 2, post-recovery)"
  helm_upgrade || fatal "helm upgrade failed after self-healing retry"
fi

log "waiting for core singletons to be ready"
for d in fault-quarantine node-drainer fault-remediation; do
  wait_for 300 10 "${d} rollout" \
    kc -n "${NVS_NAMESPACE}" rollout status "deploy/${d}" --timeout=10s || \
    warn "${d} not ready (may be disabled or named differently in this chart version)"
done

log "NVSentinel installed. Verify the platform-connector DaemonSet scheduled"
log "ONLY on real nodes: kubectl -n ${NVS_NAMESPACE} get pods -o wide -l app.kubernetes.io/name=nvsentinel"

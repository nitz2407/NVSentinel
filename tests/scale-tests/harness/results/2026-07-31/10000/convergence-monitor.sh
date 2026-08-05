#!/usr/bin/env bash
# Lightweight convergence + reconciler-health sampler for the 10k run.
# Samples CR/job counts + fault-remediation/mongo/OOM-prone component health.
# Cordoned nodes (expensive 10k LIST) sampled every 5th iteration only.
set -uo pipefail
NS=nvsentinel
JNS=dgxc-janitor-system
LOG="${1:-converge.log}"
i=0
while true; do
  i=$((i+1))
  ts=$(date -u +%H:%M:%S)
  cr=$(kubectl get gpuresets.janitor.dgxc.nvidia.com -A --no-headers 2>/dev/null | wc -l)
  crready=$(kubectl get gpuresets.janitor.dgxc.nvidia.com -A -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | grep -c True)
  jobs=$(kubectl get jobs -n "$JNS" --no-headers 2>/dev/null | grep -c reset)
  # fault-remediation health
  frline=$(kubectl get pod -n "$NS" -l app.kubernetes.io/name=fault-remediation --no-headers 2>/dev/null | head -1)
  frrst=$(echo "$frline" | awk '{print $4}')
  frstat=$(echo "$frline" | awk '{print $3}')
  froom=$(kubectl get pod -n "$NS" -l app.kubernetes.io/name=fault-remediation -o jsonpath='{.items[0].status.containerStatuses[0].lastState.terminated.reason}' 2>/dev/null)
  # fault-quarantine
  fqrst=$(kubectl get pod -n "$NS" -l app.kubernetes.io/name=fault-quarantine --no-headers 2>/dev/null | head -1 | awk '{print $4}')
  # node-drainer
  ndrst=$(kubectl get pod -n "$NS" -l app.kubernetes.io/name=node-drainer --no-headers 2>/dev/null | head -1 | awk '{print $4}')
  # mongo quorum
  mready=$(kubectl get pod -n "$NS" -l app.kubernetes.io/name=mongodb -o jsonpath='{range .items[*]}{.status.containerStatuses[?(@.name=="mongodb")].ready}{"\n"}{end}' 2>/dev/null | grep -c true)
  # oom-prone
  nvcf=$(kubectl get pod -n "$NS" -l app.kubernetes.io/name=nvcf-drainer --no-headers 2>/dev/null | head -1 | awk '{print $3" r="$4}')
  lab=$(kubectl get pod -n "$NS" -l app.kubernetes.io/name=labeler --no-headers 2>/dev/null | head -1 | awk '{print $3" r="$4}')
  cordon="(skipped)"
  if [ $((i % 5)) -eq 1 ]; then
    cordon=$(kubectl get nodes -l type=kwok --no-headers 2>/dev/null | grep -c SchedulingDisabled)
  fi
  echo "[$ts] cordon=$cordon gpureset_cr=$cr ready=$crready jobs=$jobs | fault-remediation=$frstat rst=$frrst lastTerm=$froom | fq_rst=$fqrst nd_rst=$ndrst mongo_ready=$mready/3 | nvcf=$nvcf labeler=$lab" | tee -a "$LOG"
  # Flag reconciler trouble loudly
  if [ "$frstat" != "Running" ] || [ "$froom" = "OOMKilled" ]; then
    echo "[$ts] !!! RECONCILER-ISSUE fault-remediation status=$frstat lastTerm=$froom rst=$frrst" | tee -a "$LOG"
  fi
  if [ "${mready:-0}" -lt 3 ]; then
    echo "[$ts] !!! MONGO-QUORUM-DEGRADED ready=$mready/3" | tee -a "$LOG"
  fi
  sleep 120
done

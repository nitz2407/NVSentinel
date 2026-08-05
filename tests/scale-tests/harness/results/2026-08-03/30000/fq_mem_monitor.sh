#!/usr/bin/env bash
# Samples fault-quarantine RSS (vs 1Gi limit) + cordon count every 2 min via the
# apiserver->prometheus proxy (no persistent port-forward). Evidence for whether
# fault-quarantine OOMs during the 30k cordon convergence.
OUT="$(dirname "$0")/fq_mem.log"
PROM='/api/v1/namespaces/prometheus/services/prometheus-prometheus:9090/proxy/api/v1/query'
val() { kubectl get --raw "$PROM?query=$1" 2>/dev/null | python3 -c 'import sys,json
try:
  r=json.load(sys.stdin)["data"]["result"]
  print("%.1f"%float(r[0]["value"][1]) if r else "na")
except Exception:
  print("na")'; }
while true; do
  ts=$(date -u +%FT%TZ)
  rss=$(val 'process_resident_memory_bytes%7Bnamespace%3D%22nvsentinel%22%2Cpod%3D~%22fault-quarantine.%2A%22%7D%2F1024%2F1024')
  heap=$(val 'go_memstats_heap_inuse_bytes%7Bnamespace%3D%22nvsentinel%22%2Cpod%3D~%22fault-quarantine.%2A%22%7D%2F1024%2F1024')
  gor=$(val 'go_goroutines%7Bnamespace%3D%22nvsentinel%22%2Cpod%3D~%22fault-quarantine.%2A%22%7D')
  cord=$(kubectl get nodes -l type=kwok --no-headers 2>/dev/null | awk '$2 ~ /SchedulingDisabled/{c++} END{print c+0}')
  rst=$(kubectl get pod -n nvsentinel -l app.kubernetes.io/name=fault-quarantine -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}' 2>/dev/null)
  echo "$ts fq_rss_MiB=$rss/1024 heap_MiB=$heap goroutines=$gor fq_restarts=$rst cordoned=$cord" >> "$OUT"
  sleep 120
done

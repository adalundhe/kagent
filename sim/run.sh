#!/bin/bash
# One evidence run. Usage: sim/run.sh before|after
# Points the kagent Harness at the chosen Go ADK runtime image, streams worker
# and controller logs, and runs the evidence driver against a 30s controller outage.
set -o pipefail
label=$1
case "$label" in before|after) ;; *) echo "usage: $0 before|after"; exit 2 ;; esac
export PATH="$HOME/i556/bin:$HOME/go/bin:/usr/local/go/bin:$PATH" GOTOOLCHAIN=auto KUBECONFIG="$HOME/i556/kubeconfig-oss"
cd "$(git rev-parse --show-toplevel)" || exit 2
SIM="$HOME/i556/sim"; OUT="$SIM/evidence/$label"
rm -rf "$OUT" && mkdir -p "$OUT"
set -a; . "$SIM/images.env"; . "$SIM/before.env"; set +a
if [ "$label" = before ]; then KAGENT_E2E_RUNTIME_IMAGE=$KAGENT_E2E_BEFORE_RUNTIME_IMAGE; else KAGENT_E2E_RUNTIME_IMAGE=$KAGENT_E2E_AFTER_RUNTIME_IMAGE; fi
export KAGENT_E2E_RUNTIME_IMAGE
log() { echo "[$(date +%T)] $*"; }

# The cluster reaches mock models on this host through KAGENT_E2E_LOCAL_HOST.
if [ -z "${KAGENT_E2E_LOCAL_HOST:-}" ]; then
  python3 -m http.server 18555 --bind 0.0.0.0 >/dev/null 2>&1 & probe=$!
  sleep 1
  for host in 172.17.0.1 $(docker network inspect kind -f '{{range .IPAM.Config}}{{.Gateway}} {{end}}'); do
    case "$host" in *:*) continue ;; esac
    if docker exec issue556oss-control-plane curl -s -m 3 -o /dev/null "http://$host:18555/"; then KAGENT_E2E_LOCAL_HOST=$host; break; fi
  done
  kill $probe 2>/dev/null
  [ -n "${KAGENT_E2E_LOCAL_HOST:-}" ] || { log "no host address the cluster can reach"; exit 1; }
fi
export KAGENT_E2E_LOCAL_HOST
log "label=$label runtime=$KAGENT_E2E_RUNTIME_IMAGE local_host=$KAGENT_E2E_LOCAL_HOST api=$KAGENT_E2E_API_URL"

envsubst < go/core/test/e2e/manifests/lifecycle.yaml.tmpl | kubectl apply -f - > "$OUT/apply.log" 2>&1 || { cat "$OUT/apply.log"; exit 1; }
kubectl get harness kagent -n kagent -o jsonpath='{.spec.workload.image}{"\n"}' | tee "$OUT/harness-image.txt"
for _ in $(seq 1 120); do
  [ "$(kubectl get agent smoke -n kagent -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = True ] && break; sleep 5
done
log "smoke agent Ready=$(kubectl get agent smoke -n kagent -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"

# The kind node runs out of inotify watchers, so log streams with --follow fail.
# Snapshot the controller that will be stopped until its Pod is gone, and read
# the workers once the run ends.
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
old=$(kubectl get pods -n kagent -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.name}')
( while kubectl get pod -n kagent "$old" >/dev/null 2>&1; do
    kubectl logs -n kagent "$old" --timestamps --since-time="$started" > "$OUT/.controller-stopped.tmp" 2>/dev/null && mv "$OUT/.controller-stopped.tmp" "$OUT/controller-stopped.log"
    sleep 1
  done ) & stopped=$!
trap 'kill $stopped 2>/dev/null' EXIT

(cd go && go test ./core/test/e2e -run '^TestTaskStoreOutageEvidence$' -count=1 -v -timeout 60m \
  -args -evidence-label="$label" -evidence-dir="$OUT" -evidence-outage=30s -evidence-hold="${HOLD:-0s}") 2>&1 | tee "$OUT/driver.log"
result=${PIPESTATUS[0]}

new=$(kubectl get pods -n kagent -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.name}')
kubectl logs -n kagent "$new" --timestamps > "$OUT/controller-restarted.log" 2>&1
: > "$OUT/workers.log"
for p in $(kubectl get pods -n kagent -l ate.dev/worker-pool -o name); do
  kubectl logs -n kagent "$p" --all-containers --prefix --timestamps --since-time="$started" >> "$OUT/workers.log" 2>&1
done
kill $stopped 2>/dev/null
kubectl get events -n kagent --sort-by=.lastTimestamp > "$OUT/events.txt" 2>&1
log "driver exit $result; evidence in $OUT"
ls -la "$OUT"

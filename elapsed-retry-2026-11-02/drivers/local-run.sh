#!/usr/bin/env bash
# One local evidence run. Usage: sim/local-run.sh head|final
# Points the kagent Harness at the arm's Go ADK image, releases the turn's final
# save into a 30s controller outage, then collects logs, traces and screenshots.
# Scratch; not part of any PR. Run from the repository root.
set -o pipefail
umask 022
unset GOROOT
label=$1
case "$label" in head|final) ;; *) echo "usage: $0 head|final"; exit 2 ;; esac
export PATH="$TOOLS_BIN:$PATH" GOWORK=off GOFLAGS=-buildvcs=false
export SIM="$HOME/i556-local/wfr" KUBECONFIG="$HOME/i556-local/kubeconfig"
OUT="$SIM/evidence/$label"
rm -rf "$OUT" && mkdir -p "$OUT"
set -a; . "$SIM/images.env"; [ -f "$SIM/arms.env" ] && . "$SIM/arms.env"; set +a
var="KAGENT_E2E_$(echo "$label" | tr '[:lower:]' '[:upper:]')_RUNTIME_IMAGE"
export KAGENT_E2E_RUNTIME_IMAGE=${!var}
test -n "$KAGENT_E2E_RUNTIME_IMAGE" || { echo "no image for $label"; exit 2; }
log() { echo "[$(date +%T)] $*"; }
log "label=$label runtime=$KAGENT_E2E_RUNTIME_IMAGE api=$KAGENT_E2E_API_URL"

envsubst < go/core/test/e2e/manifests/lifecycle.yaml.tmpl | kubectl apply -f - > "$OUT/apply.log" 2>&1 || { cat "$OUT/apply.log"; exit 1; }
kubectl get harness kagent -n kagent -o jsonpath='{.spec.workload.image}{"\n"}' | tee "$OUT/harness-image.txt"
for _ in $(seq 1 120); do
  [ "$(kubectl get agent smoke -n kagent -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = True ] && break; sleep 5
done
log "smoke agent Ready=$(kubectl get agent smoke -n kagent -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"

# A port-forward stays on the Pod it started with, so follow the controller
# Pod across the restart. The UI Pod does not restart.
ready_controller() {
  kubectl get pods -n kagent -l app.kubernetes.io/component=controller \
    -o jsonpath='{range .items[*]}{.metadata.name} {.status.conditions[?(@.type=="Ready")].status} {.metadata.deletionTimestamp}{"\n"}{end}' 2>/dev/null |
    awk '$2 == "True" && $3 == "" { print $1; exit }'
}
( current=""; pf=""
  while true; do
    pod=$(ready_controller)
    if [ -n "$pod" ] && [ "$pod" != "$current" ]; then
      [ -n "$pf" ] && kill "$pf" 2>/dev/null
      kubectl port-forward -n kagent "pod/$pod" 18083:8083 >> "$OUT/port-forward-controller.log" 2>&1 & pf=$!
      current=$pod; echo "$(date -u +%T) forwarding to $pod" >> "$OUT/port-forward-controller.log"
    fi
    sleep 0.5
  done ) & follow=$!
kubectl port-forward -n kagent svc/kagent-ui 18082:8080 > "$OUT/port-forward-ui.log" 2>&1 & ui=$!
cleanup() { kill "$follow" "$ui" "${stopped:-}" 2>/dev/null; pkill -f 'port-forward -n kagent pod/kagent-controller' 2>/dev/null; }
trap cleanup EXIT
for _ in $(seq 1 60); do nc -z 127.0.0.1 18083 2>/dev/null && break; sleep 1; done

started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
old=$(ready_controller)
( while kubectl get pod -n kagent "$old" >/dev/null 2>&1; do
    kubectl logs -n kagent "$old" --timestamps --since-time="$started" > "$OUT/.controller-stopped.tmp" 2>/dev/null && mv "$OUT/.controller-stopped.tmp" "$OUT/controller-stopped.log"
    sleep 1
  done ) & stopped=$!

(cd go && go test ./core/test/e2e -run '^TestTaskStoreOutageEvidence$' -count=1 -v -timeout 60m \
  -args -evidence-label="$label" -evidence-dir="$OUT" -evidence-outage=30s -evidence-hold=10m) > "$OUT/driver.log" 2>&1 & driver=$!
for _ in $(seq 1 1200); do [ -f "$OUT/$label.json" ] && break; kill -0 "$driver" 2>/dev/null || break; sleep 1; done
if [ -f "$OUT/$label.json" ]; then
  docker run --rm -v "$SIM/npm:/npm" -v "$OUT:/out" mcr.microsoft.com/playwright:v1.64.1-noble \
    node /npm/shots.mjs http://host.docker.internal:18082 /out "$label" > "$OUT/shots.log" 2>&1
  log "shots exit $?"
  touch "$OUT/$label.release"
fi
wait "$driver"; result=$?
grep -E '^\s+zz_taskstore_outage_evidence_test.go:[0-9]+: \[' "$OUT/driver.log" | sed 's/^ *zz_taskstore_outage_evidence_test.go:[0-9]*: //'

new=$(ready_controller)
kubectl logs -n kagent "$new" --timestamps > "$OUT/controller-restarted.log" 2>&1
: > "$OUT/workers.log"
for p in $(kubectl get pods -n kagent -l ate.dev/worker-pool -o name); do
  kubectl logs -n kagent "$p" --all-containers --prefix --timestamps --tail=-1 --since-time="$started" >> "$OUT/workers.log" 2>&1
done
kubectl logs -n ate-system deploy/atenet-egress --timestamps --tail=-1 --since-time="$started" > "$OUT/egress.log" 2>&1
kubectl exec -n kagent deploy/e2e-otlp -c reader -- cat /data/traces.jsonl > "$OUT/traces.jsonl" 2>&1
kubectl get events -n kagent --sort-by=.lastTimestamp > "$OUT/events.txt" 2>&1
log "driver exit $result; evidence in $OUT"
ls -la "$OUT"

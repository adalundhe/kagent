#!/bin/bash
# One evidence arm for the review of kagent-dev/kagent#3397. Scratch; not part of any PR.
# Usage: sim-rv3397/run.sh before|after
# Swaps only the controller image, then runs the scratch e2e drivers against it.
set -o pipefail
label=$1
case "$label" in before) tag=v0.0.1-rv3397-before ;; after) tag=v0.0.1-rv3397-after ;; *) echo "usage: $0 before|after"; exit 2 ;; esac
export PATH="$HOME/i556/bin:$HOME/go/bin:/usr/local/go/bin:$PATH" GOTOOLCHAIN=auto KUBECONFIG="$HOME/rv3397/kubeconfig" GOFLAGS=-buildvcs=false
cd "$(git rev-parse --show-toplevel)" || exit 2
SIM="$HOME/rv3397/sim"; OUT="$SIM/evidence/$label"
rm -rf "$OUT" && mkdir -p "$OUT"
set -a; . "$SIM/images.env"; set +a
export KAGENT_E2E_RUNTIME_IMAGE=$KAGENT_E2E_HEAD_RUNTIME_IMAGE KAGENT_E2E_LOCAL_HOST=${KAGENT_E2E_LOCAL_HOST:-172.17.0.1}
log() { echo "[$(date +%T)] $*"; }
kubectl set image deploy/kagent-controller -n kagent "controller=localhost:5001/kagent-dev/kagent/controller:${tag}" >/dev/null
kubectl rollout status deploy/kagent-controller -n kagent --timeout=300s || exit 1
kubectl get deploy kagent-controller -n kagent -o jsonpath='{.spec.template.spec.containers[*].image}{"\n"}' | tee "$OUT/controller-image.txt"
kubectl get pods -n kagent -l app.kubernetes.io/component=controller -o jsonpath='{.items[*].status.containerStatuses[*].imageID}{"\n"}' | tee -a "$OUT/controller-image.txt"
envsubst < go/core/test/e2e/manifests/lifecycle.yaml.tmpl | kubectl apply -f - > "$OUT/apply.log" 2>&1 || { cat "$OUT/apply.log"; exit 1; }
for _ in $(seq 1 120); do
  [ "$(kubectl get agent smoke -n kagent -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = True ] && break; sleep 5
done
# Pausing the Substrate API leaves its TCP sockets open, so a caller waits for its
# own deadline instead of failing fast; that is how a slow control plane looks.
export RV_PAUSE="docker exec rv3397-control-plane bash -c 'crictl ps -q --name ate-api-server | xargs -r -n1 ctr -n k8s.io task pause && ctr -n k8s.io task ls | grep -i paused'"
export RV_RESUME="docker exec rv3397-control-plane bash -c 'crictl ps -q --name ate-api-server | xargs -r -n1 ctr -n k8s.io task resume; ctr -n k8s.io task ls | grep -ci paused || true'"
[ "$label" = after ] && export RV_EXPECT_STRIPPED=1
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
log "label=$label controller=$tag"
(cd go && go test ./core/test/e2e -run '^TestRV3397' -count=1 -v -timeout 30m) 2>&1 | tee "$OUT/driver.log"
result=${PIPESTATUS[0]}
kubectl logs -n kagent deploy/kagent-controller --timestamps --since-time="$started" > "$OUT/controller.log" 2>&1
kubectl exec -n kagent deploy/e2e-otlp -c reader -- cat /data/traces.jsonl > "$OUT/traces.jsonl" 2>/dev/null
grep 'RV3397' "$OUT/driver.log" | sed 's/^ *//' > "$OUT/summary.txt"
log "driver exit $result"; cat "$OUT/summary.txt"

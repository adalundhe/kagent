#!/bin/bash
# One evidence arm for kagent-dev/kagent#3398. Not part of any PR.
# Usage: arm.sh <label> <ui-image-tag> <scenario...>
set -o pipefail
label=$1; tag=$2; shift 2
export PATH="$HOME/i556/bin:$PATH" KUBECONFIG="$HOME/rv3398/kubeconfig"
SIM="$HOME/rv3398/sim"; OUT="$SIM/evidence"; NPM="$HOME/rv3398/npm"
mkdir -p "$OUT"
log() { echo "[$(date +%T)] $*"; }
[ -d "$NPM" ] || cp -r "$HOME/i556/sim/npm" "$NPM"
cp "$SIM/drive.mjs" "$NPM/drive.mjs"

container=$(kubectl -n kagent get deploy kagent-ui -o jsonpath='{.spec.template.spec.containers[0].name}')
image="localhost:5001/kagent-dev/kagent/ui:${tag}"
kubectl -n kagent set image deploy/kagent-ui "${container}=${image}" >/dev/null
kubectl -n kagent rollout status deploy/kagent-ui --timeout=300s || exit 1
log "ui image $(kubectl -n kagent get deploy kagent-ui -o jsonpath='{.spec.template.spec.containers[0].image}')"
port=$(kubectl -n kagent get svc kagent-ui -o jsonpath='{.spec.ports[0].port}')

# A port-forward dies with its Pod, so keep one running for the whole arm.
( while true; do kubectl -n kagent port-forward svc/kagent-ui 18080:"$port" >/dev/null 2>&1; sleep 1; done ) & pf=$!
trap 'kill $pf 2>/dev/null; pkill -f "port-forward svc/kagent-ui 18080" 2>/dev/null' EXIT
for _ in $(seq 1 30); do curl -sf -o /dev/null http://127.0.0.1:18080/ && break; sleep 1; done

for scenario in "$@"; do
  started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  # Workers come back after each kill; wait for both before the next scenario.
  kubectl -n kagent rollout status deploy/kagent-default --timeout=300s >/dev/null
  kubectl -n kagent wait --for=condition=Ready pod -l ate.dev/worker-pool --timeout=300s >/dev/null
  log "$label/$scenario start"
  docker run --rm --network host -v "$NPM:/npm" -v "$OUT:/out" \
    -v /usr/bin/kubectl:/usr/bin/kubectl:ro -v "$KUBECONFIG:/kubeconfig:ro" -e KUBECONFIG=/kubeconfig \
    mcr.microsoft.com/playwright:v1.64.1-noble node /npm/drive.mjs http://127.0.0.1:18080 /out "$label" "$scenario" \
    > "$OUT/$label-$scenario.driver.log" 2>&1
  log "$label/$scenario driver exit $?"
  kubectl -n kagent logs deploy/kagent-controller --since-time="$started" --timestamps > "$OUT/$label-$scenario.controller.log" 2>&1
  tail -5 "$OUT/$label-$scenario.driver.log"
done

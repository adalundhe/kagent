#!/bin/bash
# One evidence arm for the #3395 review. Usage: sim/run.sh old|new. Scratch.
# Points the controller at that arm's image, then runs the evidence driver.
set -o pipefail
arm=$1
case "$arm" in old|new) ;; *) echo "usage: $0 old|new"; exit 2 ;; esac
export PATH="$HOME/i556/bin:$HOME/go/bin:/usr/local/go/bin:$PATH" GOTOOLCHAIN=auto GOFLAGS=-buildvcs=false
SIM="$HOME/rv3395"; OUT="$SIM/evidence/$arm"; export KUBECONFIG="$SIM/kubeconfig"
cd "$(git rev-parse --show-toplevel)" || exit 2
rm -rf "$OUT" && mkdir -p "$OUT" "$SIM/admit-image"
log() { echo "[$(date +%T)] $*"; }
set -a; . "$SIM/images-new.env"; set +a
kubectl -n kagent set image deployment/kagent-controller controller="localhost:5001/kagent-dev/kagent/controller:v0.0.1-$arm-rv3395" > "$OUT/set-image.log" 2>&1
kubectl -n kagent rollout status deployment/kagent-controller --timeout=300s || exit 1
kubectl -n kagent get deploy kagent-controller -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}' | tee "$OUT/controller-image.txt"
kubectl -n kagent get pods -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].status.containerStatuses[0].imageID}{"\n"}' | tee -a "$OUT/controller-image.txt"
envsubst < go/core/test/e2e/manifests/lifecycle.yaml.tmpl | kubectl apply -f - > "$OUT/apply.log" 2>&1 || true  # only the kagent Harness is used

pf() { kubectl -n "$1" port-forward "$2" "$3" > "$OUT/pf-$4.log" 2>&1 & echo $!; }
api_pf=$(pf kagent svc/kagent-controller 18395:8083 api)
trap 'kill $api_pf 2>/dev/null' EXIT
sleep 3
(cd go && CGO_ENABLED=0 go build -o "$SIM/admit-image/admit" ./core/cmd/zz-rv3395-admit) || exit 1
printf 'FROM busybox:1.37\nCOPY admit /admit\n' > "$SIM/admit-image/Dockerfile"
export RV_ADMIT_IMAGE=localhost:5001/rv3395/admit:$(date +%s)
docker build -q -t "$RV_ADMIT_IMAGE" "$SIM/admit-image" >/dev/null && docker push -q "$RV_ADMIT_IMAGE" >/dev/null || exit 1
export RV_ADMIT="$PWD/sim/admit.sh"
export KAGENT_E2E_API_URL=http://127.0.0.1:18395 KAGENT_E2E_LOCAL_HOST=${KAGENT_E2E_LOCAL_HOST:-172.17.0.1}
log "arm=$arm api=$KAGENT_E2E_API_URL"
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
(cd go && go test ./core/test/e2e -run "${RUN:-^TestRV3395}" -count=1 -v -timeout 30m) > "$OUT/driver.log" 2>&1
result=$?
kubectl -n kagent logs deploy/kagent-controller --timestamps --since-time="$started" > "$OUT/controller.log" 2>&1
kubectl -n ate-system logs deploy/ate-api-server --timestamps --since-time="$started" > "$OUT/ate-api.log" 2>&1
kubectl -n kagent exec deploy/e2e-otlp -c reader -- cat /data/traces.jsonl > "$OUT/traces.jsonl" 2>/dev/null
log "driver exit $result"
grep -E '\[rv3395|^(---|===|ok|FAIL|PASS)' "$OUT/driver.log"

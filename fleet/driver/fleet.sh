#!/bin/bash
# Scratch fleet for the review of kagent-dev/kagent#3397. Not part of any PR.
#
# With RECREATE=1 it recreates the rv3397 kind cluster with CI's own
# `make create-kind-cluster`. It then builds this tree's images and installs kagent
# the way CI's e2e job does, with the Substrate release go.mod pins (the worker
# image follows it, as in CI). The UI is enabled for screenshots.
set -o pipefail
export PATH="$HOME/i556/bin:$HOME/go/bin:/usr/local/go/bin:$PATH" GOTOOLCHAIN=auto CI=true OPENAI_API_KEY=fake KIND_SANDBOX_CLASS=gvisor
export KUBECONFIG="$HOME/rv3397/kubeconfig" VERSION=v0.0.1-rv3397
cd "$(git rev-parse --show-toplevel)" || exit 2
SIM="$HOME/rv3397/sim"; CLUSTER=rv3397
mkdir -p "$SIM/logs" && rm -rf "$SIM/runtime-images" && mkdir -p "$SIM/runtime-images"
log() { echo "[$(date +%T)] $*"; }
load_image() { docker save "$1" | docker exec -i "${CLUSTER}-control-plane" ctr --namespace=k8s.io images import --platform linux/amd64 --digests --snapshotter=overlayfs - >/dev/null 2>&1; }
# Pods that cannot pull an external image get it from the host's Docker.
preload() {
  local img
  for img in $(kubectl get pods -A -o jsonpath='{range .items[*]}{range .status.initContainerStatuses[*]}{.image}{" "}{.state.waiting.reason}{"\n"}{end}{range .status.containerStatuses[*]}{.image}{" "}{.state.waiting.reason}{"\n"}{end}{end}' 2>/dev/null | awk '$2 ~ /ImagePullBackOff|ErrImagePull/ {print $1}' | sort -u); do
    case "$img" in localhost:5001/*) continue ;; esac
    docker image inspect "$img" >/dev/null 2>&1 || docker pull -q "$img" >/dev/null || continue
    load_image "$img" && log "loaded $img"
  done
}
SUBSTRATE_VERSION=$(awk '$1 == "replace" && $2 == "go.ate.dev/substrate" { print $NF }' go/go.mod); SUBSTRATE_VERSION=${SUBSTRATE_VERSION#v}
test -n "$SUBSTRATE_VERSION" || { log "no Substrate version in go/go.mod"; exit 1; }
log "start at $(git log --oneline -1); substrate ${SUBSTRATE_VERSION}"

if [ -n "${RECREATE:-}" ]; then
  kind delete cluster --name "$CLUSTER" > "$SIM/logs/kind.log" 2>&1
  KIND_CLUSTER_NAME=$CLUSTER KIND_KUBECONFIG="$SIM/kind-config" make create-kind-cluster >> "$SIM/logs/kind.log" 2>&1 || { tail -30 "$SIM/logs/kind.log"; exit 1; }
  SUBNET=$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' | tr ' ' '\n' | grep -E '^[0-9]+\.' | head -1 | cut -d. -f1,2)
  kubectl apply -f - <<YAML >> "$SIM/logs/kind.log" 2>&1
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: address-pool
  namespace: metallb-system
spec:
  addresses: ["${SUBNET}.253.97-${SUBNET}.253.110"]
YAML
  kubectl get ipaddresspool -n metallb-system -o jsonpath='{range .items[*]}{.metadata.name}={.spec.addresses}{"\n"}{end}'
  log "cluster recreated"
fi

digest() { docker buildx imagetools inspect "localhost:5001/kagent-dev/kagent/$1:${VERSION}" | awk '$1 == "Digest:" { print $2 }'; }
if [ -z "${SKIP_BUILD:-}" ]; then
  make buildx-create >/dev/null 2>&1 || { log "buildx-create failed"; exit 1; }
  build() {
    RUNTIME_IMAGE_METADATA_DIR="$SIM/runtime-images" DOCKER_BUILD_ARGS="--platform=linux/amd64 --push" \
      make GIT_COMMIT=e2e BUILD_DATE=1970-01-01 "build-$1" > "$SIM/logs/build-$1.log" 2>&1
  }
  export -f build; export SIM
  failed=$(printf '%s\n' controller ui golang-adk claude-harness codex-harness byo-a2a sandbox-guest |
    xargs -P4 -I{} bash -c 'for try in 1 2 3; do build {} && { echo "built {}" >&2; exit 0; }; sleep 15; done; echo {}' 2>&2)
  [ -z "$failed" ] || { log "build failed: $failed"; for f in $failed; do tail -25 "$SIM/logs/build-$f.log"; done; exit 1; }
  log "images built"
  rm -rf dist
  make build-cli-local helm-release RUNTIME_IMAGE_METADATA_DIR="$SIM/runtime-images" RUNTIME_IMAGE_REPOSITORY=localhost:5001/kagent-dev/kagent > "$SIM/logs/package.log" 2>&1 || { tail -30 "$SIM/logs/package.log"; exit 1; }
  rm -rf "$SIM/release-charts" && mkdir -p "$SIM/release-charts"
  tar -xzf "dist/kagent-${VERSION}.tgz" -C "$SIM/release-charts" && tar -xzf "dist/kagent-crds-${VERSION}.tgz" -C "$SIM/release-charts" || exit 1
  log "packaged"
  # The previous PR head's controller, for before/after comparisons. Only the controller differs.
  if [ -n "${BEFORE_REF:-}" ]; then
    rm -rf "$SIM/before" && git worktree prune && git worktree add -q --detach "$SIM/before" "$BEFORE_REF"
    (cd "$SIM/before" && DOCKER_BUILD_ARGS="--platform=linux/amd64 --push" make GIT_COMMIT=e2e BUILD_DATE=1970-01-01 VERSION=${VERSION}-before build-controller > "$SIM/logs/build-before.log" 2>&1) || { tail -25 "$SIM/logs/build-before.log"; exit 1; }
    log "before controller built from $BEFORE_REF"
  fi
fi
GUEST_DIGEST=$(digest sandbox-guest); test -n "$GUEST_DIGEST" || exit 1
# TRACES=1 sends controller and runtime spans to the in-cluster e2e-otlp sink,
# using the endpoint CI's e2e install uses.
TELEMETRY_RUNTIME=""; TELEMETRY_CONTROLLER=""
if [ -n "${TRACES:-}" ]; then
  kubectl apply -f sim-rv3397/otel.yaml > "$SIM/logs/otel.log" 2>&1 || { cat "$SIM/logs/otel.log"; exit 1; }
  kubectl rollout status deploy/e2e-otlp -n kagent --timeout=300s || exit 1
  TELEMETRY_RUNTIME=$'    telemetry:\n      traces:\n        endpoint: http://e2e-otlp.kagent.svc.cluster.local:14317'
  TELEMETRY_CONTROLLER=$'  telemetry:\n    traces:\n      endpoint: http://e2e-otlp.kagent.svc.cluster.local:14317'
fi

cat > "$SIM/kagent-values.yaml" <<YAML
registry: localhost:5001
tag: "${VERSION}"
imagePullPolicy: Always
controller:
  image:
    pullPolicy: Always
  service:
    type: LoadBalancer
  startupProbe:
    httpGet:
      path: /health
      port: http
    periodSeconds: 1
    failureThreshold: 60
  readinessProbe:
    httpGet:
      path: /health
      port: http
    periodSeconds: 1
config:
  push:
    allowHttp: true
    allowPrivateNetworks: true
  sandboxes:
    guestImage: "localhost:5001/kagent-dev/kagent/sandbox-guest@${GUEST_DIGEST}"
  runtimeDefaults:
    tokenPropagation:
      enabled: true
${TELEMETRY_RUNTIME}
${TELEMETRY_CONTROLLER}
ui:
  replicas: 1
  service:
    type: LoadBalancer
kagent-tools:
  enabled: false
substrateWorkerPool:
  create: true
  replicas: 4
  sandboxClass: gvisor
  workerImage: ghcr.io/kagent-dev/substrate/ateworker-gvisor:v${SUBSTRATE_VERSION}
YAML
cat > "$SIM/substrate-values.yaml" <<YAML
credentialProvider:
  namespacePolicies:
    - atespace: kagent
      allowedNamespaces: [kagent, kagent-shared-models, kagent-shared-tools]
atelet:
  resources:
    limits:
      memory: 2Gi
  extraArgs: [--localhost-registry-replacement=kind-registry:5000]
ateApi:
  extraArgs: [--template-resync-interval=250ms]
YAML
cat > "$SIM/kagent-cli.yaml" <<YAML
install:
  kagent:
    repository: "$SIM/release-charts/"
    version: "${VERSION}"
    valuesFiles: ["$SIM/kagent-values.yaml"]
  substrate:
    valuesFiles: ["$SIM/substrate-values.yaml"]
YAML
( while true; do preload; sleep 20; done ) & preload_pid=$!
trap 'kill $preload_pid 2>/dev/null' EXIT
for t in 1 2 3 4; do
  ./go/core/bin/kagent-local --config "$SIM/kagent-cli.yaml" install --namespace kagent > "$SIM/logs/install.log" 2>&1 && break
  log "install attempt $t failed: $(tail -1 "$SIM/logs/install.log")"; sleep 20
done
grep -q 'installed successfully' "$SIM/logs/install.log" || { tail -20 "$SIM/logs/install.log"; exit 1; }
kubectl rollout status deployment/kagent-controller -n kagent --timeout=600s || exit 1
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/component=controller -n kagent --timeout=300s || exit 1
log "installed"

{
  echo "KAGENT_E2E_HEAD_RUNTIME_IMAGE=localhost:5001/kagent-dev/kagent/golang-adk@$(digest golang-adk)"
  echo "KAGENT_E2E_BYO_IMAGE=localhost:5001/kagent-dev/kagent/byo-a2a@$(digest byo-a2a)"
  echo "KAGENT_E2E_CLAUDE_IMAGE=localhost:5001/kagent-dev/kagent/claude-harness@$(digest claude-harness)"
  echo "KAGENT_E2E_CODEX_IMAGE=localhost:5001/kagent-dev/kagent/codex-harness@$(digest codex-harness)"
  echo "KAGENT_E2E_API_URL=http://$(kubectl get svc -n kagent kagent-controller -o jsonpath='{.status.loadBalancer.ingress[0].ip}'):8083"
  echo "KAGENT_UI_URL=http://$(kubectl get svc -n kagent kagent-ui -o jsonpath='{.status.loadBalancer.ingress[0].ip}'):$(kubectl get svc -n kagent kagent-ui -o jsonpath='{.spec.ports[0].port}')"
} > "$SIM/images.env"
cat "$SIM/images.env"
kubectl get pods -A -o wide --no-headers | awk '{print $1, $2, $4, $8}' | column -t
kubectl -n ate-system get deploy -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.template.spec.containers[*].image}{"\n"}{end}'
log "done"

#!/bin/bash
# Scratch fleet for the kagent-dev/kagent#3395 review. Not part of any PR.
# Adapted from the issue-556 fleet script: own kind cluster rv3395, own
# kubeconfig and state under ~/rv3395, no MetalLB (avoids LB ranges shared with
# other clusters on the kind network), image tags suffixed -rv3395.
# ARM=new|old picks the image tag suffix; SKIP_BUILD=1 reuses images.
set -o pipefail
export PATH="$HOME/i556/bin:$HOME/go/bin:/usr/local/go/bin:$PATH" GOTOOLCHAIN=auto CI=true OPENAI_API_KEY=fake KIND_SANDBOX_CLASS=gvisor CONTAINER_RUNTIME=docker
SIM="$HOME/rv3395"; CLUSTER=rv3395
export KUBECONFIG="$SIM/kubeconfig" VERSION=v0.0.1-${ARM:-new}-rv3395
cd "$(git rev-parse --show-toplevel)" || exit 2
mkdir -p "$SIM/logs" && rm -rf "$SIM/runtime-images-$ARM" && mkdir -p "$SIM/runtime-images-$ARM"
log() { echo "[$(date +%T)] $*"; }
load_image() { docker save "$1" | docker exec -i "${CLUSTER}-control-plane" ctr --namespace=k8s.io images import --platform linux/amd64 --digests --snapshotter=overlayfs - >/dev/null 2>&1; }
preload() {
  local img
  for img in $(kubectl get pods -A -o jsonpath='{range .items[*]}{range .status.initContainerStatuses[*]}{.image}{" "}{.state.waiting.reason}{"\n"}{end}{range .status.containerStatuses[*]}{.image}{" "}{.state.waiting.reason}{"\n"}{end}{end}' 2>/dev/null | awk '$2 ~ /ImagePullBackOff|ErrImagePull/ {print $1}' | sort -u); do
    case "$img" in localhost:5001/*) continue ;; esac
    docker image inspect "$img" >/dev/null 2>&1 || docker pull -q "$img" >/dev/null || continue
    load_image "$img" && log "loaded $img"
  done
}
SUBSTRATE_VERSION=$(awk '$1 == "replace" && $2 == "go.ate.dev/substrate" { print $NF }' go/go.mod); SUBSTRATE_VERSION=${SUBSTRATE_VERSION#v}
log "start at $(git log --oneline -1); substrate ${SUBSTRATE_VERSION}; version ${VERSION}"

if [ -n "${RECREATE:-}" ]; then
  kind delete cluster --name "$CLUSTER" > "$SIM/logs/kind.log" 2>&1
  KIND_CLUSTER_NAME=$CLUSTER bash ./scripts/kind/setup-kind.sh >> "$SIM/logs/kind.log" 2>&1 || { tail -30 "$SIM/logs/kind.log"; exit 1; }
  kind get kubeconfig --name "$CLUSTER" > "$KUBECONFIG"
  log "cluster recreated"
fi

digest() { docker buildx imagetools inspect "localhost:5001/kagent-dev/kagent/$1:${VERSION}" | awk '$1 == "Digest:" { print $2 }'; }
if [ -z "${SKIP_BUILD:-}" ]; then
  make buildx-create >/dev/null 2>&1 || { log "buildx-create failed"; exit 1; }
  build() {
    RUNTIME_IMAGE_METADATA_DIR="$SIM/runtime-images-$ARM" DOCKER_BUILD_ARGS="--platform=linux/amd64 --push" \
      make GIT_COMMIT=e2e BUILD_DATE=1970-01-01 "build-$1" > "$SIM/logs/build-$ARM-$1.log" 2>&1
  }
  export -f build; export SIM ARM
  failed=$(printf '%s\n' controller ui golang-adk claude-harness codex-harness byo-a2a sandbox-guest |
    xargs -P4 -I{} bash -c 'for try in 1 2 3; do build {} && { echo "built {}" >&2; exit 0; }; sleep 15; done; echo {}' 2>&2)
  [ -z "$failed" ] || { log "build failed: $failed"; for f in $failed; do tail -25 "$SIM/logs/build-$ARM-$f.log"; done; exit 1; }
  log "images built"
  rm -rf dist
  make build-cli-local helm-release RUNTIME_IMAGE_METADATA_DIR="$SIM/runtime-images-$ARM" RUNTIME_IMAGE_REPOSITORY=localhost:5001/kagent-dev/kagent > "$SIM/logs/package-$ARM.log" 2>&1 || { tail -30 "$SIM/logs/package-$ARM.log"; exit 1; }
  rm -rf "$SIM/release-charts-$ARM" && mkdir -p "$SIM/release-charts-$ARM"
  tar -xzf "dist/kagent-${VERSION}.tgz" -C "$SIM/release-charts-$ARM" && tar -xzf "dist/kagent-crds-${VERSION}.tgz" -C "$SIM/release-charts-$ARM" || exit 1
  cp go/core/bin/kagent-local "$SIM/kagent-local-$ARM"
  log "packaged"
fi
[ -n "${BUILD_ONLY:-}" ] && exit 0
GUEST_DIGEST=$(digest sandbox-guest); test -n "$GUEST_DIGEST" || exit 1
kubectl create namespace kagent >/dev/null 2>&1
kubectl apply -f sim/otel.yaml > "$SIM/logs/otel.log" 2>&1 || { cat "$SIM/logs/otel.log"; exit 1; }
cat > "$SIM/kagent-values.yaml" <<YAML
registry: localhost:5001
tag: "${VERSION}"
imagePullPolicy: Always
controller:
  image:
    pullPolicy: Always
config:
  push:
    allowHttp: true
    allowPrivateNetworks: true
  sessions:
    quiescenceRecoveryInterval: 5s
  sandboxes:
    guestImage: "localhost:5001/kagent-dev/kagent/sandbox-guest@${GUEST_DIGEST}"
  runtimeDefaults:
    tokenPropagation:
      enabled: true
    telemetry:
      traces:
        endpoint: http://e2e-otlp.kagent.svc.cluster.local:14317
  telemetry:
    traces:
      endpoint: http://e2e-otlp.kagent.svc.cluster.local:14317
ui:
  replicas: 1
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
    repository: "$SIM/release-charts-$ARM/"
    version: "${VERSION}"
    valuesFiles: ["$SIM/kagent-values.yaml"]
  substrate:
    valuesFiles: ["$SIM/substrate-values.yaml"]
YAML
( while true; do preload; sleep 20; done ) & preload_pid=$!
trap 'kill $preload_pid 2>/dev/null' EXIT
for t in 1 2 3 4; do
  "$SIM/kagent-local-$ARM" --config "$SIM/kagent-cli.yaml" install --namespace kagent > "$SIM/logs/install-$ARM.log" 2>&1 && break
  log "install attempt $t failed: $(tail -1 "$SIM/logs/install-$ARM.log")"; sleep 20
done
grep -q 'installed successfully' "$SIM/logs/install-$ARM.log" || { tail -20 "$SIM/logs/install-$ARM.log"; exit 1; }
kubectl rollout status deployment/kagent-controller -n kagent --timeout=600s || exit 1
kubectl rollout status deploy/e2e-otlp -n kagent --timeout=300s || exit 1
{
  echo "KAGENT_E2E_RUNTIME_IMAGE=localhost:5001/kagent-dev/kagent/golang-adk@$(digest golang-adk)"
  echo "KAGENT_E2E_CLI=$SIM/kagent-local-$ARM"
} > "$SIM/images-$ARM.env"
cat "$SIM/images-$ARM.env"
kubectl get pods -A --no-headers | awk '{print $1, $2, $4}' | column -t
log "done"

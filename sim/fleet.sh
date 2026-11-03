#!/bin/bash
# Scratch fleet for reviewing kagent-dev/kagent#3398. Not part of any PR.
# Derived from the i556 fleet.sh: its own kind cluster (rv3398), kubeconfig and image
# tag, no MetalLB (the shared kind network's pools overlap), and only the images the
# chat recovery path needs. Services stay ClusterIP; reach them by port-forward.
set -o pipefail
export CONTAINER_RUNTIME=docker PATH="$HOME/i556/bin:$HOME/go/bin:/usr/local/go/bin:$PATH" GOTOOLCHAIN=auto CI=true OPENAI_API_KEY=fake KIND_SANDBOX_CLASS=gvisor
SIM="$HOME/rv3398/sim"; CLUSTER=rv3398
export KUBECONFIG="$HOME/rv3398/kubeconfig" VERSION=${VERSION:-v0.0.1-rv3398}
cd "$HOME/rv3398/kagent" || exit 2
mkdir -p "$SIM/logs" "$SIM/runtime-images"
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
log "start at $(git log --oneline -1); substrate ${SUBSTRATE_VERSION}; tag ${VERSION}"

if ! kind get clusters | grep -qx "$CLUSTER"; then
  KIND_CLUSTER_NAME=$CLUSTER bash ./scripts/kind/setup-kind.sh > "$SIM/logs/kind.log" 2>&1 || { tail -30 "$SIM/logs/kind.log"; exit 1; }
  kind get kubeconfig --name "$CLUSTER" > "$KUBECONFIG"
  log "cluster created"
fi

digest() { docker buildx imagetools inspect "localhost:5001/kagent-dev/kagent/$1:${VERSION}" | awk '$1 == "Digest:" { print $2 }'; }
if [ -z "${SKIP_BUILD:-}" ]; then
  make buildx-create >/dev/null 2>&1 || { log "buildx-create failed"; exit 1; }
  build() {
    RUNTIME_IMAGE_METADATA_DIR="$SIM/runtime-images" DOCKER_BUILD_ARGS="--platform=linux/amd64 --push" \
      make GIT_COMMIT=rv3398 BUILD_DATE=1970-01-01 "build-$1" > "$SIM/logs/build-$1.log" 2>&1
  }
  export -f build; export SIM
  failed=$(printf '%s\n' ${IMAGES:-controller ui golang-adk sandbox-guest} |
    xargs -P4 -I{} bash -c 'for try in 1 2 3; do build {} && { echo "built {}" >&2; exit 0; }; sleep 15; done; echo {}' 2>&2)
  [ -z "$failed" ] || { log "build failed: $failed"; for f in $failed; do tail -25 "$SIM/logs/build-$f.log"; done; exit 1; }
  log "images built"
  [ -n "${SKIP_PACKAGE:-}" ] && exit 0
  rm -rf dist
  make build-cli-local helm-release RUNTIME_IMAGE_METADATA_DIR="$SIM/runtime-images" RUNTIME_IMAGE_REPOSITORY=localhost:5001/kagent-dev/kagent > "$SIM/logs/package.log" 2>&1 || { tail -30 "$SIM/logs/package.log"; exit 1; }
  rm -rf "$SIM/release-charts" && mkdir -p "$SIM/release-charts"
  tar -xzf "dist/kagent-${VERSION}.tgz" -C "$SIM/release-charts" && tar -xzf "dist/kagent-crds-${VERSION}.tgz" -C "$SIM/release-charts" || exit 1
  log "packaged"
fi
GUEST_DIGEST=$(digest sandbox-guest); test -n "$GUEST_DIGEST" || exit 1
kubectl create namespace kagent --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl apply -f "$SIM/otel.yaml" > "$SIM/logs/otel.log" 2>&1 || { cat "$SIM/logs/otel.log"; exit 1; }
( while true; do preload; sleep 20; done ) & preload_pid=$!
trap 'kill $preload_pid 2>/dev/null' EXIT
kubectl rollout status deploy/e2e-otlp -n kagent --timeout=300s || exit 1

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
  replicas: 2
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
for t in 1 2 3 4; do
  ./go/core/bin/kagent-local --config "$SIM/kagent-cli.yaml" install --namespace kagent > "$SIM/logs/install.log" 2>&1 && break
  log "install attempt $t failed: $(tail -1 "$SIM/logs/install.log")"; sleep 20
done
grep -q 'installed successfully' "$SIM/logs/install.log" || { tail -20 "$SIM/logs/install.log"; exit 1; }
kubectl rollout status deployment/kagent-controller -n kagent --timeout=600s || exit 1
echo "KAGENT_E2E_RUNTIME_IMAGE=localhost:5001/kagent-dev/kagent/golang-adk@$(digest golang-adk)" > "$SIM/images.env"
cat "$SIM/images.env"
kubectl get pods -A --no-headers | awk '{print $1, $2, $3, $4}' | column -t
log "done"

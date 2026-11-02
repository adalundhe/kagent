#!/usr/bin/env bash
# Scratch local fleet on this Mac for kagent-dev/kagent#3391. Not part of any PR.
# Its own kind cluster, registry, builder and kubeconfig, so the other kind
# clusters on this machine are untouched. Mirrors CI's e2e install, built for
# arm64, with an in-cluster trace sink. Run from the repository root.
set -o pipefail
umask 022
unset GOROOT
export PATH="$TOOLS_BIN:$PATH" GOWORK=off GOFLAGS=-buildvcs=false
export CI=true OPENAI_API_KEY=fake KIND_SANDBOX_CLASS=gvisor
export CLUSTER=i556wfr KIND_CLUSTER_NAME=i556wfr REG_NAME=i556-registry REG_PORT=5009
export DOCKER_REGISTRY=localhost:5009 BUILDX_BUILDER_NAME=i556-builder VERSION=v0.0.1-wfr
export SIM="$HOME/i556-local/wfr" KUBECONFIG="$HOME/i556-local/kubeconfig"
REPO=localhost:5009/kagent-dev/kagent
mkdir -p "$SIM/logs" && rm -rf "$SIM/runtime-images" && mkdir -p "$SIM/runtime-images"
log() { echo "[$(date +%T)] $*"; }
SUBSTRATE_VERSION=$(awk '$1 == "replace" && $2 == "go.ate.dev/substrate" { print $NF }' go/go.mod); SUBSTRATE_VERSION=${SUBSTRATE_VERSION#v}
log "start at $(git log --oneline -1); substrate ${SUBSTRATE_VERSION}"

if ! kind get clusters | grep -qx "$CLUSTER"; then
  make create-kind-cluster KIND_KUBECONFIG="$SIM/kind-config" > "$SIM/logs/kind.log" 2>&1 || { tail -30 "$SIM/logs/kind.log"; exit 1; }
  log "cluster created"
fi

digest() { docker buildx imagetools inspect "$REPO/$1:${VERSION}" | awk '$1 == "Digest:" { print $2 }'; }
if [ -z "${SKIP_BUILD:-}" ]; then
  make buildx-create > "$SIM/logs/buildx.log" 2>&1 || { tail -20 "$SIM/logs/buildx.log"; exit 1; }
  build() {
    RUNTIME_IMAGE_METADATA_DIR="$SIM/runtime-images" DOCKER_BUILD_ARGS="--platform=linux/arm64 --push" \
      make GIT_COMMIT=e2e BUILD_DATE=1970-01-01 "build-$1" > "$SIM/logs/build-$1.log" 2>&1
  }
  export -f build
  failed=$(printf '%s\n' controller ui golang-adk claude-harness codex-harness byo-a2a sandbox-guest |
    xargs -P4 -I{} bash -c 'for try in 1 2; do build {} && { echo "built {}" >&2; exit 0; }; sleep 10; done; echo {}' 2>&2)
  [ -z "$failed" ] || { log "build failed: $failed"; for f in $failed; do tail -25 "$SIM/logs/build-$f.log"; done; exit 1; }
  log "images built"
  rm -rf dist
  make build-cli-local helm-release RUNTIME_IMAGE_METADATA_DIR="$SIM/runtime-images" RUNTIME_IMAGE_REPOSITORY="$REPO" > "$SIM/logs/package.log" 2>&1 || { tail -30 "$SIM/logs/package.log"; exit 1; }
  rm -rf "$SIM/release-charts" && mkdir -p "$SIM/release-charts"
  tar -xzf "dist/kagent-${VERSION}.tgz" -C "$SIM/release-charts" && tar -xzf "dist/kagent-crds-${VERSION}.tgz" -C "$SIM/release-charts" || exit 1
  log "packaged"
fi
GUEST_DIGEST=$(digest sandbox-guest); test -n "$GUEST_DIGEST" || exit 1

kubectl create namespace kagent --dry-run=client -o yaml | kubectl apply -f - > /dev/null
kubectl apply -f sim/otel.yaml > "$SIM/logs/otel.log" 2>&1 || { cat "$SIM/logs/otel.log"; exit 1; }
OTLP=http://e2e-otlp.kagent.svc.cluster.local:14317
cat > "$SIM/kagent-values.yaml" <<YAML
registry: localhost:5009
tag: "${VERSION}"
imagePullPolicy: Always
controller:
  image:
    pullPolicy: Always
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
    guestImage: "${REPO}/sandbox-guest@${GUEST_DIGEST}"
  telemetry:
    traces:
      endpoint: ${OTLP}
  runtimeDefaults:
    tokenPropagation:
      enabled: true
    telemetry:
      traces:
        endpoint: ${OTLP}
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
  extraArgs: [--localhost-registry-replacement=${REG_NAME}:5000]
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
for t in 1 2 3; do
  ./go/core/bin/kagent-local --config "$SIM/kagent-cli.yaml" install --namespace kagent > "$SIM/logs/install.log" 2>&1 && break
  log "install attempt $t failed: $(tail -1 "$SIM/logs/install.log")"; sleep 20
done
grep -q 'installed successfully' "$SIM/logs/install.log" || { tail -20 "$SIM/logs/install.log"; exit 1; }
kubectl rollout status deployment/kagent-controller -n kagent --timeout=600s || exit 1
kubectl rollout status deploy/e2e-otlp -n kagent --timeout=300s || exit 1
log "installed"
{
  echo "KAGENT_E2E_HEAD_RUNTIME_IMAGE=$REPO/golang-adk@$(digest golang-adk)"
  echo "KAGENT_E2E_BYO_IMAGE=$REPO/byo-a2a@$(digest byo-a2a)"
  echo "KAGENT_E2E_CLAUDE_IMAGE=$REPO/claude-harness@$(digest claude-harness)"
  echo "KAGENT_E2E_CODEX_IMAGE=$REPO/codex-harness@$(digest codex-harness)"
  echo "KAGENT_E2E_API_URL=http://127.0.0.1:18083"
} > "$SIM/images.env"
cat "$SIM/images.env"
kubectl get pods -A --no-headers | awk '$4 !~ /Running|Completed/'
log "done"

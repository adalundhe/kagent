#!/bin/bash
# Scratch. Runs the admission helper in an ephemeral container on the controller
# Pod, which already projects the PostgreSQL client certificate the DSN names.
# Usage: sim/admit.sh <session-id> <actor-uid> <atespace> <snapshot-uri> <fidelity>
set -euo pipefail
export PATH="$HOME/i556/bin:$PATH" KUBECONFIG="$HOME/rv3395/kubeconfig"
SIM="$HOME/rv3395"
pod=$(kubectl -n kagent get pods -l app.kubernetes.io/component=controller --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
mount=$(kubectl -n kagent get pod "$pod" -o json | jq -c '[.spec.containers[0].volumeMounts[] | select(.mountPath | startswith("/run/postgres"))]')
name="rvadmit$(date +%s)"
# Run as the controller does: the Pod requires runAsNonRoot, and the projected
# key is readable only by the controller's user.
sc=$(kubectl -n kagent get pod "$pod" -o json | jq -c '{runAsUser: 65532} + (.spec.containers[0].securityContext // {})')
custom=$(mktemp); echo "{\"volumeMounts\": $mount, \"securityContext\": $sc}" > "$custom"
kubectl -n kagent debug "$pod" --image="$RV_ADMIT_IMAGE" --container="$name" --custom="$custom" -- sleep 600 >/dev/null
for _ in $(seq 1 60); do
  state=$(kubectl -n kagent get pod "$pod" -o jsonpath="{.status.ephemeralContainerStatuses[?(@.name==\"$name\")].state.running.startedAt}")
  [ -n "$state" ] && break; sleep 1
done
dsn=$(kubectl -n kagent get secret kagent-postgres-readwrite -o jsonpath='{.data.readWriteConnectionString}' | base64 -d)
kubectl -n kagent exec "$pod" -c "$name" -- /admit "$dsn" "$@"

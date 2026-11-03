#!/bin/bash
# Scratch. Collects the #3395 review evidence into ~/rv3395/bundle.
set -o pipefail
export PATH="$HOME/i556/bin:$PATH"
E="$HOME/rv3395/evidence"; B="$HOME/rv3395/bundle/3395-review"
rm -rf "$B" && mkdir -p "$B/delete-after-failed-recovery" "$B/start-scan"
rv() { grep -E '\[rv3395|^(---|===|ok|FAIL|PASS)' "$1" | sed -E 's/^ *zz_rv3395_evidence_test.go:[0-9]+: //'; }
session_of() { grep -oE 'Session [0-9a-f-]{36}' "$1" | head -1 | cut -d' ' -f2; }
# spans <traces> <session> : SessionService spans for one Session, with their gRPC status.
spans() {
  jq -R -r --arg s "$2" 'fromjson? | .resourceSpans[]?.scopeSpans[]?.spans[]?
    | select(.name | test("SessionService/(DeleteSession|RevertSession)"))
    | [(.startTimeUnixNano | tonumber / 1e9 | strftime("%H:%M:%S")), .traceId, .name,
       ([.attributes[]? | select(.key | test("status_code")) | "\(.key)=\(.value.intValue // .value.stringValue)"] | join(",")),
       (.status.message // "")] | @tsv' "$1"
}
# The run-time dumps can end mid-line; the collector keeps every run, so read it again.
KUBECONFIG="$HOME/rv3395/kubeconfig" kubectl -n kagent exec deploy/e2e-otlp -c reader -- cat /data/traces.jsonl > "$E/all-traces.jsonl"
for f in "$E"/*/traces.jsonl; do cp "$E/all-traces.jsonl" "$f"; done
for arm in new old; do
  src="$E/new-delete"; [ "$arm" = old ] && src="$E/old-all"
  d="$B/delete-after-failed-recovery"
  rv "$src/driver.log" | awk '/TestRV3395DeleteAfterFailedRecoveryAttempt/,/^--- (PASS|FAIL): TestRV3395DeleteAfterFailedRecoveryAttempt/' > "$d/$arm-driver.txt"
  sid=$(session_of "$d/$arm-driver.txt")
  grep "$sid" "$src/controller.log" | grep -E 'recover crashed session|delete|Delete' | cut -c1-600 > "$d/$arm-controller.txt"
  cp "$src/controller-image.txt" "$d/$arm-controller-image.txt"
  # Only spans inside this scenario's window.
  first=$(grep -m1 -oE '\[rv3395 [0-9:.]+' "$d/$arm-driver.txt" | cut -d' ' -f2 | cut -d. -f1)
  last=$(grep -oE '\[rv3395 [0-9:.]+' "$d/$arm-driver.txt" | tail -1 | cut -d' ' -f2 | cut -d. -f1)
  spans "$src/traces.jsonl" "$sid" | awk -F'\t' -v a="$first" -v b="$last" '$1 >= a && $1 <= b' > "$d/$arm-traces.tsv"
done
src="$E/new-scan-revert"; d="$B/start-scan"
rv "$src/driver.log" > "$d/new-driver.txt"
sid=$(session_of "$d/new-driver.txt")
grep "$sid" "$src/controller.log" | cut -c1-600 > "$d/new-controller-session.txt"
cp "$src/controller-image.txt" "$src/controller-pods-after.txt" "$d/"
spans "$src/traces.jsonl" "$sid" | awk -F'\t' '$3 ~ /RevertSession/' > "$d/new-revert-traces.tsv"
find "$B" -type f | sort | xargs wc -l

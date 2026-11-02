"""Summarize the trace sink's spans for one evidence arm.

Usage: python3 -I traces.py <traces.jsonl> <report.json> <out.log> <out-spans.json>
Keeps the runtime's TaskStore client spans and the turn's invoke_agent span
for the arm's Session and its time window.
"""
import json
import sys
from datetime import datetime, timezone
from pathlib import Path

traces, report_path, out_log, out_json = map(Path, sys.argv[1:5])
report = json.loads(report_path.read_text())
start = datetime.fromisoformat(report["events"][0]["wall"].replace("Z", "+00:00")).timestamp() - 5
end = datetime.fromisoformat(report["events"][-1]["wall"].replace("Z", "+00:00")).timestamp() + 5


def value(attr):
    v = attr.get("value", {})
    return next(iter(v.values()), None) if v else None


spans = []
for line in traces.read_text().splitlines():
    try:
        batch = json.loads(line)
    except ValueError:
        continue
    for resource in batch.get("resourceSpans", []):
        rattrs = {a["key"]: value(a) for a in resource.get("resource", {}).get("attributes", [])}
        for scope in resource.get("scopeSpans", []):
            for span in scope.get("spans", []):
                attrs = {a["key"]: value(a) for a in span.get("attributes", [])}
                t0, t1 = int(span["startTimeUnixNano"]) / 1e9, int(span["endTimeUnixNano"]) / 1e9
                if not (start <= t0 <= end):
                    continue
                spans.append({"service": rattrs.get("service.name"), "name": span["name"], "trace_id": span["traceId"],
                              "span_id": span["spanId"], "kind": span.get("kind"), "start": t0, "duration_ms": round((t1 - t0) * 1000, 1),
                              "status": span.get("status", {}), "attributes": attrs})
session = report["session_id"]
traces_of_turn = {s["trace_id"] for s in spans if s["attributes"].get("gen_ai.conversation.id") == session}
keep = [s for s in spans if "TaskStoreService/" in s["name"] and s["service"] != "kagent-controller"
        and (s["trace_id"] in traces_of_turn or s["attributes"].get("server.address") is not None)]
keep += [s for s in spans if s["attributes"].get("gen_ai.operation.name") == "invoke_agent" and s["trace_id"] in traces_of_turn]
keep.sort(key=lambda s: s["start"])
rows = [f"# {len(keep)} spans for session {session}: runtime TaskStore client calls and the turn's invoke_agent span"]
for s in keep:
    stamp = datetime.fromtimestamp(s["start"], timezone.utc).strftime("%H:%M:%S.%f")[:-3]
    code = s["attributes"].get("rpc.response.status_code") or s["attributes"].get("rpc.grpc.status_code") or ""
    msg = s["status"].get("message", "")
    rows.append(f"{stamp} {s['service']:<14} {s['name'].rsplit('/', 1)[-1]:<14} {s['duration_ms']:>8.1f}ms status={code or s['status'].get('code', '')} trace={s['trace_id'][:16]} {msg}".rstrip())
out_log.write_text("\n".join(rows) + "\n")
out_json.write_text(json.dumps(keep, indent=1) + "\n")
print("\n".join(rows[:60]))

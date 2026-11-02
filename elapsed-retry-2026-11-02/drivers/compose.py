"""Compose review logs from the copied fleet evidence.

Usage: python3 -I compose.py <evidence-dir> <out-dir>
Writes live-fleet.log (all three arms) and one <arm>-live.log per arm.
"""
import json
import re
import sys
from pathlib import Path

evidence, out = Path(sys.argv[1]), Path(sys.argv[2])
out.mkdir(parents=True, exist_ok=True)
IMAGES = {
    "main": ("main 12aa4d46, no change", "sha256:69818a67"),
    "head": ("PR head 7f2d5f83 (WaitForReady + 4 attempts)", "sha256:b66034ab"),
    "eitan": ("7f2d5f83 + proposed 2-minute elapsed-time retry", "sha256:ad222c64"),
    "final": ("final: 2-minute elapsed-time retry with the three review fixes", "sha256:9f7e4e62"),
}
FIELD = re.compile(r'(\S+?)=("(?:[^"\\]|\\.)*"|\S+)')


def egress_lines(arm, session):
    rows = []
    for line in (evidence / arm / "egress-nondebug.log").read_text().splitlines():
        parts = line.split("\t")
        if len(parts) < 3 or not parts[2].startswith("request "):
            continue
        fields = dict((k, v.strip('"')) for k, v in FIELD.findall(parts[2]))
        if fields.get("ate.actor.name") != f"session-{session}" or "TaskStoreService" not in fields.get("http.path", ""):
            continue
        stamp = parts[0].split(" ")[-1][11:23]
        method = fields["http.path"].rsplit("/", 1)[-1]
        detail = fields.get("error", "")
        rows.append(f"{stamp} {parts[1]:<5} conn={fields.get('src.addr')} {method:<10} grpc={fields.get('grpc.status', '0'):<2} {fields.get('duration', ''):>6}  {detail}".rstrip())
    return rows


def runtime_lines(arm):
    keep = {"execute", "taskstore retries exhausted", "agent execution failed", "execution failed", "settle runtime task boundary"}
    rows = []
    for line in (evidence / arm / "workers.log").read_text().splitlines():
        try:
            record = json.loads(line.split("] ", 1)[1].split(" ", 1)[1])
        except (IndexError, ValueError):
            continue
        if record.get("labels", {}).get("ate.actor.container.name") != "kagent" or record.get("msg") not in keep:
            continue
        detail = record.get("error") or (record.get("a2a") or {}).get("cause") or record.get("task_id", "")
        extra = f" attempts={record['attempts']}" if "attempts" in record else ""
        rows.append(f"{record['time'][11:23]} {record['level']:<5} {record['msg']}{extra}  {detail}")
    return rows


def controller_lines(arm):
    rows = []
    for line in (evidence / arm / "controller-restarted.log").read_text().splitlines():
        if "TaskStoreService/" not in line or '"rpc completed"' not in line:
            continue
        record = json.loads(line.split(" ", 1)[1])
        rows.append(f"{record['time'][11:23]} {record['grpc_method'].rsplit('/', 1)[-1]:<10} {record['grpc_code']} {record['duration_ms']}ms peer={record['peer']}")
    return rows


combined = []
for arm in [a for a in ["main", "head", "eitan", "final"] if (evidence / a).is_dir()]:
    report = json.loads((evidence / arm / f"{arm}.json").read_text())
    label, digest = IMAGES[arm]
    timeline = [f"[{e['at']:>8}] {e['wall'][11:23]} {e['what']}" for e in report["events"]]
    egress = egress_lines(arm, report["session_id"])
    runtime = runtime_lines(arm)
    controller = controller_lines(arm)
    # The eitan arm's follow-up turn also appears; keep the first turn's calls.
    text = [
        f"################ arm={arm}: {label}",
        f"# runtime image golang-adk@{digest}… session {report['session_id']} task {report['task_id']}",
        f"$ bash sim/run.sh {arm}    # outage driver: hold the turn, stop the controller, release the turn into the outage",
        *timeline,
        f"report: execution={report['final_execution_state'].rsplit('_', 1)[-1]} task={report['final_task_state']} text={report['final_task_text']!r} follow_up_accepted={report['follow_up_accepted']} {report.get('follow_up_error') or report.get('follow_up_text') or ''}",
        "exit=0",
        "",
        f"$ grep 'ate.actor.name=session-{report['session_id']}' egress.log | grep TaskStoreService    # atenet-egress (agentgateway) access log",
        *(egress or ["(none)"]),
        "",
        f"$ jq 'select(.labels[\"ate.actor.container.name\"]==\"kagent\")' workers.log    # the Go ADK runtime's own lines",
        *(runtime or ["(none)"]),
        "",
        "$ grep 'TaskStoreService/' controller-restarted.log | grep 'rpc completed'    # the controller that came back",
        *(controller or ["(none: no TaskStore call reached the restarted controller)"]),
        "",
    ]
    (out / f"{arm}-live.log").write_text("\n".join(text) + "\n")
    combined.extend(text)
header = [
    "# kagent-dev/kagent#3391: live controller-restart runs, 2026-11-02 (times UTC)",
    "# Fleet: kind issue556oss on kiln-a094a12d, kagent built from 7f2d5f83, Substrate v1.0.0-rc1,",
    "# agentgateway v1.7.0 as atenet-egress. Each arm runs the same fleet and controller; only the kagent",
    "# Harness runtime image differs. The controller is scaled to zero before the turn's final save.",
    "",
]
(out / "live-fleet.log").write_text("\n".join(header + combined) + "\n")
print((out / "live-fleet.log").read_text())

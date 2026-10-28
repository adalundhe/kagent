# TaskStore saves through a controller restart: before and after kagent-dev/kagent#3391

Recorded on 2026-10-28 on a local kind cluster with gVisor workers. All times are UTC.

## Setup

- kagent is installed from the PR head, `f2b34211`, with the UI enabled and one controller replica.
- The two runs differ only in the `kagent` Harness's Go ADK runtime image:
  - before: `golang-adk` built from `main` at `a77e158b`, digest `sha256:3508cbe9…`.
  - after: `golang-adk` built from `f2b34211`, digest `sha256:18188a8d…`.
- Neither run sets `KAGENT_TASKSTORE_*`, so the after run uses the defaults: a 90 s budget and 5 s attempts.

## Scenario

`sim/zz_taskstore_outage_evidence_test.go` drives both runs:

1. A scheduled run sends "What is 2+2?" to a new Go ADK agent, and a mock model holds the request.
2. The controller Deployment scales to zero. Once its Pod is gone, the mock model answers, so the turn's final save meets no controller.
3. After 30 s the controller scales back to one replica.
4. For 3 minutes the driver records the execution and task states. It then sends a follow-up message on the same session.

## Results

| | before (`main`) | after (#3391) |
|---|---|---|
| Controller unavailable | 33 s | 35 s |
| Runtime's final save | fails with `Unavailable … Connection refused` at 17:53:35, 20 s before the controller returns | retried 25 times over 33.1 s, and lands 0.1 s after the controller is Ready |
| Scheduled-run execution | still `RUNNING` 3 minutes later | `SUCCEEDED` within 1 s of the controller being Ready |
| Task | `WORKING`, with no answer | `COMPLETED`: "The answer is 4." |
| Follow-up on the same session | refused: `FailedPrecondition` "resource conflict" | accepted and answered |
| Model calls for the turn | 1 | 1 |

## Files

- `before/` and `after/` each hold:
  - `chat.png` and `schedule.png`: the UI conversation and scheduled-run pages after the run.
  - `report.json`: the driver's timeline and outcome.
  - `runtime.log`: the Go ADK runtime's log lines for the session.
  - `controller-taskstore.log`: the TaskStore and `SendMessage` calls the restarted controller served.
  - `driver.log`, `workers.log`, `controller-restarted.log` and `events.txt`: raw output.
- `after/controller-stopped.log` is the stopped controller's log. The before run has none: the kind node ran out of inotify watchers, and the follow stream for that log failed.
- `sim/` holds the driver and the scripts that produced this.

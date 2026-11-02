# TaskStore retries through a controller restart: kagent-dev/kagent#3391 at `8f893624`

Recorded on 2026-11-02. Times are UTC.

## What changed

`8f893624` bounds the runtime's TaskStore retries by a two-minute budget instead of four attempts. It keeps `grpc.WaitForReady(true)`, the 30 s call timeout, and waits from 100 ms up to 1 s. Three review fixes apply to the suggested loop:

- It logs `taskstore retries exhausted` only when the budget runs out, not when the caller cancels or its own deadline passes.
- On exhaustion it returns the last RPC error, not `context deadline exceeded`.
- It jitters each wait between half and all of the delay.

## Setup

- One local kind cluster with gVisor workers, built from the PR with CI's `make create-kind-cluster` and e2e install.
- Substrate v1.0.0-rc1. AgentGateway v1.7.0 is the egress proxy between the runtime and the controller.
- An OpenTelemetry collector receives controller and runtime spans at the endpoint CI's e2e install uses.
- Both arms use the same cluster and controller. Only the `kagent` Harness's Go ADK image differs:
  - `head`: `7f2d5f83`, `WaitForReady` with four attempts;
  - `final`: `8f893624`.
- The driver holds a turn at a mock model, scales the controller to zero, and releases the turn into the outage. It restores the controller after 30 s and records the task, its scheduled-run execution, and a follow-up message.

## Live results

| | `head` (`7f2d5f83`) | `final` (`8f893624`) |
|---|---|---|
| Controller unavailable | 35 s | 35 s |
| Final save | 4 `UNAVAILABLE` spans in 717 ms, then `taskstore retries exhausted attempts=4` | 38 `UNAVAILABLE` spans, then `UpdateTask` OK and `SettleTask` OK in the same trace |
| Task | `WORKING` after 3 minutes | `COMPLETED` 1 s after the controller was Ready |
| Follow-up on the same Session | refused: `FailedPrecondition` "resource conflict" | answered: "The answer is 4." |

Every failed attempt reached the proxy over one runtime connection that stayed up. The proxy reported `upstream call failed: Connect: Connection refused (os error 111)`. `WaitForReady` never waits in that state, so only an elapsed-time budget outlasts the restart.

- Screenshots: [`screenshots/`](screenshots/), the chat and scheduled-run pages after each run.
- Traces: [`traces/`](traces/), the runtime's TaskStore client spans and the turn's `invoke_agent` span, selected by Session from the collector's export.
- Logs: [`logs/head`](logs/head) and [`logs/final`](logs/final). Each holds the driver timeline and report, the runtime and controller logs, and the egress proxy's access log. `live-summary.log` extracts the lines that matter.
- [`renders/`](renders/) holds PNG renderings of the summaries.

## Adversarial cases

The probes run real gRPC over `bufconn` under `-race`. A fake server answers like the proxy during a restart, over a connection that stays READY. [`logs/tests/edge-cases.log`](logs/tests/edge-cases.log) has the results at `8f893624`:

| Case | Result |
|---|---|
| 2 s outage behind a reachable proxy | lands after 2.1 s and 6 identical attempts, all on a READY channel |
| Settlement through the same outage | settles after 2.3 s |
| Proxy unreachable for 2 s, then the controller down for 2 s | lands after 4.1 s |
| Controller never returns | stops after 2m0.002s and 161 attempts; returns the proxy's `Unavailable` and logs once |
| Caller cancels at 250 ms | returns `context canceled`; logs nothing |
| Caller deadline at 1.5 s | returns `context deadline exceeded` at 1.5 s; logs nothing |

The unit tests in the PR cover the same behavior. They pass 20 runs, and 10 more with every CPU core saturated.

## Review finding, fixed

An independent review found that the suggested loop returned a bare `DeadlineExceeded` when the budget ended during an attempt. That attempt's own deadline error replaced the proxy's error. The context deadline can also fire late under load, so one more attempt could start after the budget had passed.

`8f893624` keeps the error from before a cut-short attempt, and checks the clock before each attempt. [`logs/tests/review-finding.log`](logs/tests/review-finding.log) reproduces it with a proxy that takes 300 ms per attempt:

- the loop before the fix returned `DeadlineExceeded` in 10 of 10 runs; `8f893624` returned the proxy's `Unavailable` in 10 of 10;
- the PR's regression test fails 5 of 5 runs against the loop before the fix, and passes at `8f893624`.

## Controller and PostgreSQL

[`logs/tests/linux-tests.log`](logs/tests/linux-tests.log): `TestRuntimeTaskStoreSaveOutlastsAControllerRestart` runs the controller's gRPC server on PostgreSQL in two variants, direct and through a forwarding proxy. Both pass. With `7f2d5f83`'s `store.go`, the proxy variant fails within 0.76 s. `make lint` reports 0 issues.

## Where the evidence stops

- Each live arm ran once, with one 30 s outage.
- An outage longer than two minutes still fails the save and leaves the task `WORKING`.
- The Python runtime's TaskStore client keeps four attempts and was not tested.

## Reproduce

[`drivers/`](drivers/) holds the fleet and run scripts, the outage driver, the collector manifest, the probes and the summarizers. From a checkout of `8f893624` with those files in place, run `drivers/local-fleet.sh` once, then `drivers/local-run.sh head` and `drivers/local-run.sh final`. `$TOOLS_BIN` stands for a directory with `kind`, `helm`, `kubectl` and Go 1.27.

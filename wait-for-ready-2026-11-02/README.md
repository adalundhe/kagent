# TaskStore WaitForReady evidence, November 2, 2026

Source: [`7f2d5f83`](https://github.com/adalundhe/kagent/commit/7f2d5f835f7d2e44ebcd8efe6e11b6ac9cd851f6), [PR #3391](https://github.com/kagent-dev/kagent/pull/3391).
Baseline: `12aa4d460aaeabc9ee2f923decf8d14372f1bae3`.

The five TaskStore RPCs now use `grpc.WaitForReady(true)`, retaining four attempts and the 30-second per-call deadline.
Settlement drops its shared deadline; each telemetry flush gets `telemetry.FlushTimeout`.
Retry exhaustion emits an error log.
This replaces the custom budget and its configuration.

## Fresh controlled gRPC results

These runs use the actual TaskStore client with controlled gRPC services and `-race`.
They do not deploy a controller, PostgreSQL, or an egress proxy.
The baseline comparison overlays only `store.go` and `executor.go` from `12aa4d46`.

| Case | Observed result |
| --- | --- |
| Baseline, unavailable transport | Expected failure: an RPC returns `Unavailable` before recovery |
| Head, 100ms unavailable transport | All five TaskStore RPCs succeed in 103.4–103.7ms; Memory fails fast in 0.139ms |
| Caller cancellation | Initial return in 259ms; recorded replays vary from 250ms to 851ms; one attempt |
| Earlier caller deadline | Initial return in 260ms; earlier RPC deadline preserved; one attempt |
| Permanent transport outage | Four 30-second attempts; initial return after 120.701851s; replay after 124.071344s; exhaustion error log |
| RPC already in flight when transport stops | 2.002107s outage; identical request reaches replacement service; version 2 acknowledged after 2.857552s total |
| READY server returns `Unavailable` | Four attempts exhaust after 728.646ms; server recovers after 2.002615s |
| Canceled observer and expired telemetry flush | Committed regression passes; settlement and the second flush get fresh deadlines |

The initial boundary run returned after 120.701851s. A replay returned after 124.071344s and failed the driver's `120.7 ± 2s` assertion.
Another replay preserved the earlier deadlines but returned from cancellation and expiry after 851ms. It failed the driver's `250 ± 500ms` assertions.
Both runs retained the expected attempt counts and error codes.
The [first](logs/boundary-replay-before-assertion-fix.log) and [second](logs/boundary-replay-second-timing-failure.log) timing failures remain in the bundle.
Their source checkout paths are redacted.
The driver now checks configured RPC deadlines, preservation of the earlier caller deadline, cancellation and attempt counts.
Observed completion times remain measurements rather than real-time guarantees.
The [final full replay](logs/final-replay-run-results.json) matched all five expected outcomes; exhaustion returned after 120.702011s.
That replay includes the expected baseline failure. The production commit remains `7f2d5f83`; only evidence-driver assertions changed.

The READY-server case tests a real gRPC response from a synthetic service.
It confirms that channel readiness cannot make an application-returned `Unavailable` wait for recovery.
It does not reproduce that condition in a deployed proxy or establish a need for the removed budget.

[Raw logs](logs), [machine-readable results](results.json), and [test drivers](drivers) retain the observations.
The [OTel client export](traces/cold-recovery-client-spans.json) retains six spans with trace IDs, times, durations and RPC statuses.
Its `Unset` OTel span status denotes no explicit span error; `rpc.response.status_code=OK` records successful TaskStore RPCs.

## Current-head CI logs and traces

[CI Build](https://github.com/kagent-dev/kagent/actions/runs/41478342975) passed on merge `37d673491eea6510bd89d586fd9ee018fc6c61da`.
That merge combines head `7f2d5f83` with main `82bddc8d`.

- [Go race tests](https://github.com/kagent-dev/kagent/actions/runs/41478342975/job/125389723464) passed, including TaskStore and the controller gRPC package.
- [Go lint](https://github.com/kagent-dev/kagent/actions/runs/41478342975/job/125389723375) passed.
- [gVisor](https://github.com/kagent-dev/kagent/actions/runs/41478342975/job/125389723738) and [microVM](https://github.com/kagent-dev/kagent/actions/runs/41478342975/job/125389723264) E2E passed.

The [gVisor artifact](https://github.com/kagent-dev/kagent/actions/runs/41478342975/artifacts/13158438957) retains controller, worker, Substrate and test logs, plus 9,001 span records.
It expires November 9.
The original ZIP's SHA-256 is `897da74e49897e2b448e4e6fe8c30613dc70a2c468dd9867735c5d47886c7a27`.
The [retained extract](traces/ci-extract.json) preserves selected span and actor lifecycle records.
The [CI summary](ci-summary.md) states the correlation limits.

The scheduled-restart E2E test releases the model after the replacement controller becomes Ready.
It proves execution continuity across restart, without submitting the final save during the outage.
The selected trace's association with that test is a timing inference; the test log omits its session ID.

## Missing deployed outage proof

Fresh controller/PostgreSQL and Kind runs stopped before container creation because Docker Desktop requires sign-in.
The prepared 2/10/45/100-second controller matrix did not run.
No fresh local deployed runtime logs or traces prove final-save recovery during an outage for this commit.
The existing October 28 before/after deployment evidence covers `f2b34211` and its custom budget.
It remains historical evidence, separate from these results.

## Reproduce the completed controlled checks

These checks take about three minutes, including the full 120-second exhaustion case.
They use Go overlays and keep tracked source unchanged.
Use the Go toolchain required by the source repository's `go/go.mod`.

```sh
git clone https://github.com/adalundhe/kagent.git source
git -C source checkout 7f2d5f835f7d2e44ebcd8efe6e11b6ac9cd851f6
python3 run-evidence.py --repo source --output replay
```

`run-evidence.py` requires the exact source commit and an empty output directory.
It rejects unexpected baseline failures as well as failed head checks.

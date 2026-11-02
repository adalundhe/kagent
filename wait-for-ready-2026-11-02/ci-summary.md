CI evidence for PR #3391 at `7f2d5f835f7d2e44ebcd8efe6e11b6ac9cd851f6`, collected November 2, 2026.

[CI Build](https://github.com/kagent-dev/kagent/actions/runs/41478342975) passed. The checkout was merge `37d673491eea6510bd89d586fd9ee018fc6c61da`, combining this head with main `82bddc8d`.

[Go race tests](https://github.com/kagent-dev/kagent/actions/runs/41478342975/job/125389723464) passed, including `adk/pkg/taskstore` (4.219s) and `core/internal/grpcserver` (55.940s). The job prints package results, without individual test results. Go lint also passed.

[gVisor E2E](https://github.com/kagent-dev/kagent/actions/runs/41478342975/job/125389723738) passed. Its [artifact](https://github.com/kagent-dev/kagent/actions/runs/41478342975/artifacts/13158438957) retains test, controller, worker and Substrate logs, plus 9,001 span records. The artifact expires November 9. The local copy retains the original ZIP and all six extracted files.

The whole suite contains 2,894 successful UpdateTask span records and 589 successful SettleTask span records. These include client and server spans. They are not counts of unique requests or recovery attempts.

Partial restart correlation:

- `TestScheduledRunControllerRestart/kagent` passed in 4.65s. All four harness subtests passed.
- During the isolated test window, trace `a527d7a0718c89e2f9f2f96164f768b0` spans an agent invocation from 16:10:59.210 to 16:11:15.239 UTC.
- Its `gen_ai.conversation.id` is `01a19d07-3a22-72db-adc0-47b5123d573a`; its `a2a.task.id` is `01a19d07-3acd-7986-92c0-9725238f2e01`.
- The worker log matches that session's actor name. Substrate records it running at 16:10:59.143, then suspended at 16:11:15.586. The trace records successful UpdateTask and SettleTask calls before suspension.

The test log does not print that scheduled session ID. Its association with the selected trace is inferred from timing, rather than an explicit test identifier. The retained controller log starts at 16:14:34, so it cannot complete this earlier correlation.

The [test implementation](https://github.com/adalundhe/kagent/blob/7f2d5f835f7d2e44ebcd8efe6e11b6ac9cd851f6/go/core/test/e2e/scheduled_run_test.go#L119) releases the model after the replacement controller is Ready. This evidence covers execution continuity across restart. It does not prove a final save submitted during a controller outage. A new forced-save outage run remains separate validation.

`traces/ci-extract.json` retains the selected test lines, allowlisted span fields, and actor lifecycle records. It excludes arbitrary payloads, credentials, endpoints and log messages. The linked CI artifact identifies the source export.

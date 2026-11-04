# kagent-dev/kagent#3399 review evidence

Dev fleet: kind cluster on Fedora x86-64, Substrate v0.7.0-alpha1, gVisor workers.
`before` is the PR head before the review fixes (23eafdd4). `after` is the head with them.

- `crash-detection.txt`: time from worker loss to recovery, three runs per arm.
- `substrate-rpcs-idle.txt`: ate-api calls an idle controller makes per minute.
- `ui-before-pending.png`, `ui-after-pending.png`, `ui-after-after-revert.png`: the crash
  alert with automatic recovery enabled, and the conversation after Revert.
- `ui-after-revert-span.txt`: the controller span for that Revert.
- `e2e-worker-loss-recovery.txt`: `TestSessionWorkerLossRecovery/kagent` against `after`.

## Pending automatic recovery at the merged head (3fc96de2) and with the fix (5ae5c00a)

Both runs hold the automatic revert failing: after the crash, the atelet is moved off
every node, so the sweep admits the recovery and `RevertActor` fails. The recovery
interval is 4m, so no automatic retry lands before the clicks.

At 3fc96de2, after #3398 merged into the branch:
- `merged-head-pending-revert-disabled.png`: the alert's Revert is disabled and spinning,
  because the admitted recovery keeps its RESUME operation. The click timed out
  (`merged-head-playwright.log`).
- `merged-head-forked.png`: Fork on the snapshot line still opened a fork.

At 5ae5c00a:
- `fix-pending.png`: the same pending state with Revert enabled.
- `fix-forked.png`: the fork opened from the snapshot line while recovery was pending.
- `fix-after-revert.png`: the conversation after Revert, the discarded turn labelled
  "Not remembered after recovery".
- `fix-driver.txt`, `fix-controller.txt`, `fix-spans.txt`, `fix-ui.json`: the matching records.

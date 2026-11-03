# kagent-dev/kagent#3399 review evidence

Dev fleet: kind cluster on Fedora x86-64, Substrate v0.7.0-alpha1, gVisor workers.
`before` is the PR head before the review fixes (23eafdd4). `after` is the head with them.

- `crash-detection.txt`: time from worker loss to recovery, three runs per arm.
- `substrate-rpcs-idle.txt`: ate-api calls an idle controller makes per minute.
- `ui-before-pending.png`, `ui-after-pending.png`, `ui-after-after-revert.png`: the crash
  alert with automatic recovery enabled, and the conversation after Revert.
- `ui-after-revert-span.txt`: the controller span for that Revert.
- `e2e-worker-loss-recovery.txt`: `TestSessionWorkerLossRecovery/kagent` against `after`.

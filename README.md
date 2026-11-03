# kagent-dev/kagent#3396 review evidence

Kind cluster with gVisor workers and Substrate v0.7.0-alpha1. Each arm swaps only the controller image.

- `before/`: PR head `08019da1`.
- `after/`: `f0251704`, which adds the review fixes.

Per arm:

- `revert-outcomes.txt` lists each RevertSession step with its code, ErrorInfo reasons and message.
- `controller-revert-rpcs.jsonl` holds the controller's `rpc completed` lines.
- `revert-spans.jsonl` holds the matching server spans from the in-cluster OTLP collector, joined by trace ID.
- `e2e-summary.txt` holds the test results.

`unit-tests.txt` covers cases the cluster run does not reach, such as a Session at rest in SUSPENDED and an operation in progress.
The `replace` evidence subtest reports FAIL in both arms. Its fixture cleanup cannot delete a Session whose Actor was replaced. The revert outcome above it is what the run records.

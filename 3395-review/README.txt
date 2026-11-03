Evidence for kagent-dev/kagent#3395 review follow-ups. Scratch; not part of any PR.

Fleet: kind cluster on Substrate 0.7.0-alpha1, gVisor workers, recovery scan interval 5s.
Controller images were built from #3396's head (it adds RevertSession) with and without
the two #3395 follow-up commits:
  old = 344ddf38 (previous recovery code)
  new = 344ddf38 + fbf3ecd3 + 05cfb0cc
The "admit" step calls database.Client.BeginCrashedSessionRecovery, the call RevertSession
makes, from beside the controller. It leaves the state a RevertSession leaves when its first
execution attempt does not complete.

unit/                         Postgres-backed unit tests on the previous code and on this change
delete-after-failed-recovery/ admitted recovery, ate-api scaled to 0, then DeleteSession
start-scan/                   crashed Actor, admitted recovery, no RevertSession call; plus
                              TestSessionRevertCrashedRuntime/kagent against the new controller
scripts/                      the fleet, driver and helper used

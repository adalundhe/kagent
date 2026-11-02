#!/usr/bin/env python3
"""Replay the recorded controlled gRPC checks against the pinned source commit."""
import argparse
import json
import os
from pathlib import Path
import shlex
import subprocess

HEAD = '7f2d5f835f7d2e44ebcd8efe6e11b6ac9cd851f6'
BASE = '12aa4d460aaeabc9ee2f923decf8d14372f1bae3'
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--repo', type=Path, required=True)
parser.add_argument('--output', type=Path, required=True)
args = parser.parse_args()
repo, output = args.repo.resolve(), args.output.resolve()
bundle = Path(__file__).resolve().parent
actual = subprocess.check_output(['git', '-C', str(repo), 'rev-parse', 'HEAD'], text=True).strip()
if actual != HEAD:
    raise SystemExit(f'Expected source {HEAD}, got {actual}')
if subprocess.check_output(['git', '-C', str(repo), 'status', '--porcelain'], text=True).strip():
    raise SystemExit('Use a clean source checkout so the commit identifies the tested code')
if output.exists() and any(output.iterdir()):
    raise SystemExit('Choose an empty output directory to preserve earlier results')
output.mkdir(parents=True, exist_ok=True)
env = os.environ.copy()
env.update(GOWORK='off', GOMAXPROCS='4', GOFLAGS='-p=2', EVIDENCE_DIRECTORY=str(output))
commands, results = [], {}

def overlay(name, mapping):
    path = output / (name + '-overlay.json')
    path.write_text(json.dumps({'Replace': {str(repo / key): str(value) for key, value in mapping.items()}}, indent=2))
    return path

def run(name, packages, pattern=None, mapping=None, timeout='3m', expected_failure=False):
    cmd = ['go', 'test', '-race', '-v', '-count=1', '-timeout=' + timeout]
    if mapping:
        cmd.append('-overlay=' + str(overlay(name, mapping)))
    if pattern:
        cmd += ['-run', pattern]
    cmd += packages
    commands.append(shlex.join(cmd))
    (output / 'commands.txt').write_text('\n'.join(commands) + '\n')
    with (output / (name + '.log')).open('w') as log:
        result = subprocess.run(cmd, cwd=repo / 'go', env=env, stdout=log, stderr=subprocess.STDOUT)
    text = (output / (name + '.log')).read_text()
    if expected_failure:
        valid = result.returncode != 0 and 'TaskStore RPC returned before transport recovery' in text and 'code = Unavailable' in text
    else:
        valid = result.returncode == 0
    results[name] = {'exit': result.returncode, 'expected_failure': expected_failure, 'matched_expectation': valid}
    (output / 'run-results.json').write_text(json.dumps({'head': HEAD, 'base': BASE, 'results': results}, indent=2) + '\n')
    print(name, results[name], flush=True)
    if not valid:
        raise SystemExit(f'{name}: unexpected result; inspect {output / (name + ".log")}')

run('focused-race', ['./adk/pkg/taskstore', './adk/pkg/controllerclient'], timeout='90s')
base_mapping = {}
for filename in ('store.go', 'executor.go'):
    relative = 'go/adk/pkg/taskstore/' + filename
    original = output / ('base-' + filename)
    original.write_bytes(subprocess.check_output(['git', '-C', str(repo), 'show', BASE + ':' + relative]))
    base_mapping[relative] = original
run('base-cold-recovery', ['./adk/pkg/taskstore'], '^TestTaskStoreWaitsForTransportRecovery$', base_mapping, '30s', True)
run('cold-recovery-traces', ['./adk/pkg/taskstore'], '^TestEvidenceColdRecoveryClientTraces$', {
    'go/adk/pkg/taskstore/zz_evidence_traces_test.go': bundle / 'drivers/trace_probe_test.go'}, '30s')
run('transport-edges', ['./adk/pkg/taskstore'], '^TestEvidence', {
    'go/adk/pkg/taskstore/zz_evidence_edge_test.go': bundle / 'drivers/edge_test.go'}, '40s')
run('boundaries', ['./adk/pkg/taskstore'], '^TestEvidenceUnavailableTransportBoundaries$', {
    'go/adk/pkg/taskstore/zz_evidence_deadline_test.go': bundle / 'drivers/deadline_probe_test.go'})

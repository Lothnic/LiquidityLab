"""Fixture only: no network, mounted host data, database, account or trading actions."""
import datetime
import json
import os
from pathlib import Path
import secrets
import subprocess

root = Path('/recordings')
old_file = root / 'retained-fixture'
old_file.write_bytes(b'unchanged recording fixture\n')
os.chown(old_file, 10001, 10001)
os.chmod(old_file, 0o600)
private = Path('/run/secrets')
private.mkdir(parents=True, exist_ok=True)
values = [secrets.token_hex(32), secrets.token_hex(32)]
for name, value in zip(('api_token', 'postgres_password'), values):
    (private / name).write_text(value + '\n')
    os.chmod(private / name, 0o600)
policy = Path('/bundle/policy')
policy.mkdir(parents=True, exist_ok=True)
(policy / 'manifest.json').write_text(json.dumps({'policy_sha256': '0' * 64}))
retained_bytes = len(old_file.read_bytes())
recording_budget = retained_bytes + 1024 + 2 * 131072 - 1
env = {**os.environ, 'PAPER_MODE': 'resume', 'PAPER_SOURCE': 'kraken',
       'PAPER_SCHEMA': 'storage_smoke', 'PAPER_ACCOUNT': 'not-created', 'PAPER_DURATION': '1s',
       'PAPER_MAX_RECORDING_BYTES': str(recording_budget), 'PAPER_MAX_CAPTURE_BYTES': '1024'}
# The budget is one byte below retained bytes + raw cap + manifest reservation.
before = sorted(p.name for p in root.iterdir())
process = subprocess.run(['/usr/local/bin/paper-entrypoint'], env=env, capture_output=True, timeout=15)
output = process.stdout + process.stderr
assert process.returncode != 0
assert b'recording storage admission refused' in output
assert b'open isolated paper database' not in output
assert sorted(p.name for p in root.iterdir()) == before
assert old_file.read_bytes() == b'unchanged recording fixture\n'
for value in values:
    assert value.encode() not in output
print(json.dumps({'observed_at_utc': datetime.datetime.now(datetime.timezone.utc).isoformat(),
                  'network': 'none', 'host_volumes': [], 'entrypoint': '/usr/local/bin/paper-entrypoint',
                  'profile': 'kraken resume; dummy runtime secrets and model manifest; admission only',
                  'max_capture_bytes': 1024, 'max_recording_bytes': recording_budget, 'retained_bytes': retained_bytes,
                  'manifest_reserve_bytes': 262144, 'shortfall_bytes': 1,
                  'exit_code': process.returncode, 'admission_refused': True,
                  'database_not_opened': True, 'existing_files_unchanged': True,
                  'fresh_capture_directory_created': False, 'fixture_credentials_withheld': True,
                  'valid_trading_commands_issued': 0, 'limitations': ['No source connection or capture.',
                  'No actual database, account, PPO worker or trained-model validation in this refusal check.',
                  'Logical admission budget only; no disk-full, host quota or performance test.']}))

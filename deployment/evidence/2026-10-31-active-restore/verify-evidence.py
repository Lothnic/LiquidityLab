#!/usr/bin/env python3
"""Verify curated artifact checksums and recovery facts without Docker or secrets."""
import gzip
import hashlib
import json
from decimal import Decimal
from pathlib import Path

ROOT = Path(__file__).resolve().parent

def read(name):
    return json.loads((ROOT / name).read_text())

for line in (ROOT / 'SHA256SUMS').read_text().splitlines():
    expected, name = line.split('  ', 1)
    assert hashlib.sha256((ROOT / name).read_bytes()).hexdigest() == expected, name

source = read('crash-durable.json')
offline = read('restored-offline.json')
after = read('recovered.json')
account = after['accounts'][0]
original = source['accounts'][0]
p = source['current_plan']
recovered = after['current_plan']
checks = {'offline_snapshot_equals_crash': source == offline}
indexes = read('provenance/index-summary.json')['tables']
entries = {}
for table in ('commands', 'observations', 'executions'):
    raw = gzip.decompress((ROOT / f'provenance/{table}-retained-prefix.ndjson.gz').read_bytes())
    assert hashlib.sha256(raw).hexdigest() == indexes[table]['index_uncompressed_sha256']
    rows = [json.loads(line) for line in raw.splitlines()]
    assert len(rows) == indexes[table]['retained_count']
    assert len({json.dumps(row['identity'], sort_keys=True) for row in rows}) == len(rows)
    assert all(digest == indexes[table]['index_uncompressed_sha256'] for digest in indexes[table]['stage_observed_canonical_prefix_sha256'].values())
    assert all(len(row['canonical_row_sha256']) == 64 for row in rows)
    entries[table] = rows
checks['ownership_epoch_incremented'] = account['ownership_epoch'] > original['ownership_epoch']
checks['parent_paused'] = recovered['status'] == 'paused' and all(read(n)['plan_status'] == 'paused' and read(n)['ready'] and not read(n)['recovery_required'] for n in ['recovered-paused-state.json', 'recovered-paused-state-after-3s.json'])
checks['original_parent_deadline_preserved'] = recovered['deadline'] == p['deadline']
checks['original_spec_and_policy_pins_preserved'] = recovered['detail']['spec'] == p['detail']['spec'] and recovered['detail']['policy_bundle'] == p['detail']['policy_bundle']
checks['old_child_settled'] = after['current_child_outbox']['state'] in ('final', 'obsolete') and account['snapshot'].get('child') is None
checks['no_held_reservations'] = all(row['state'] != 'held' for row in read('outbox-no-dispatch-proof.json')['recovered_reservations']) and all(Decimal(row['amount']) == 0 for row in read('recovered-paused-state-after-3s.json')['reserved'])
checks['observed_processed_caught_up'] = account['observed_sequence'] == account['processed_sequence']
checks['no_new_execution'] = after['counts']['executions'] == source['counts']['executions'] == len(entries['executions'])
checks['balances_preserved'] = read('balances-crash-exact.json') == read('balances-recovered-exact.json')
checks['all_previous_accepted_observation_facts_retained'] = len(entries['observations']) == source['counts']['observations']
checks['all_previous_command_results_retained'] = len(entries['commands']) == source['counts']['commands']
balances = {'BTC': Decimal('1'), 'USD': Decimal('10000')}
for row in entries['executions']:
    fill = row['detail']
    quantity, price, fee = (Decimal(fill[name]) for name in ('quantity', 'price', 'fee'))
    sign = 1 if fill['side'] == 'buy' else -1
    balances['BTC'] += sign * quantity
    balances['USD'] -= sign * quantity * price
    balances['BTC' if fill['fee_currency'] == 'base' else 'USD'] -= fee
checks['balances_derived_from_all_accepted_fills'] = all(Decimal(row['total']) == balances[row['currency']] for row in read('balances-recovered-exact.json'))
checks['all_original_pinned_reports_unchanged'] = all(read(str(path.relative_to(ROOT))) == read('pinned-reports/restored-' + path.name) for path in (ROOT / 'pinned-reports').glob('pinned-*.json')) and read('pinned-reports/active-pinned-report.json') == read('pinned-reports/restored-active-pinned-report.json')
proof = read('outbox-no-dispatch-proof.json')
old = {row['intent_id']: row for row in proof['source_outbox']}
new = {row['intent_id']: row for row in proof['recovered_outbox']}
child = proof['queued_child_intent_id']
checks['outbox_intent_ids_unchanged'] = old.keys() == new.keys()
checks['queued_child_attempt_id_unset_after_recovery'] = old[child]['attempt_id'] == new[child]['attempt_id'] == ''
checks['no_previously_unset_attempt_became_bound'] = all(not new[key]['attempt_id'] for key, row in old.items() if not row['attempt_id'])
checks['all_existing_attempt_bindings_preserved'] = all(row['attempt_id'] == new[key]['attempt_id'] for key, row in old.items())
assert len(checks) == 18 and all(checks.values()), checks
assert checks == read('verification-summary.json')['checks']
print(json.dumps({'valid': True, 'computed_checks': len(checks), 'retained_counts': {key: len(value) for key, value in entries.items()}, 'derived_balances': {key: str(value) for key, value in balances.items()}}, indent=2))

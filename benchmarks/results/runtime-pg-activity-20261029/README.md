# Runtime PostgreSQL activity evidence — 2026-10-29

This bundle preserves all seven source report JSON files byte-for-byte inside deterministic gzip members. Decompressing a `trial-*.json.gz` recreates the exact source bytes. The summary omits account and tenant IDs, executable/host paths, addresses, and database names. It does not replace the full raw reports.

## Trial results

| Trial | Mode | Valid/status | Market offered/completed/rejected | Views offered/completed/rejected | Elapsed including drain (ns) | PostgreSQL samples / backend observations | Observed categories (backend observations) | Financial conservation / source debt | Clean source revision |
| --- | --- | --- | --- | --- | ---: | ---: | --- | --- | --- |
| off-01 | http | True/complete | 1000/1000/0 | 400/400/0 | 20000679132 | not sampled / not sampled | not sampled | True / 0 | `35fc7b7a65cb0cc251c618a9e99d70409a893cf0` |
| on-01 | http | True/complete | 1000/1000/0 | 400/400/0 | 20001088393 | 1001 / 2002 | active_client=1, active_wal_io=30, idle=1966, idle_in_transaction=5 | True / 0 | `35fc7b7a65cb0cc251c618a9e99d70409a893cf0` |
| off-02 | http | True/complete | 1000/1000/0 | 400/400/0 | 20000253609 | not sampled / not sampled | not sampled | True / 0 | `35fc7b7a65cb0cc251c618a9e99d70409a893cf0` |
| on-02 | http | True/complete | 1000/1000/0 | 400/400/0 | 20000797834 | 1001 / 2002 | active_unclassified=1, active_wal_io=1, idle=1999, idle_in_transaction=1 | True / 0 | `35fc7b7a65cb0cc251c618a9e99d70409a893cf0` |
| off-03 | http | True/complete | 1000/1000/0 | 400/400/0 | 20000194108 | not sampled / not sampled | not sampled | True / 0 | `35fc7b7a65cb0cc251c618a9e99d70409a893cf0` |
| on-03 | http | True/complete | 1000/1000/0 | 400/400/0 | 20000712887 | 1001 / 2002 | active_client=4, active_unclassified=1, active_wal_io=20, idle=1958, idle_in_transaction=19 | True / 0 | `35fc7b7a65cb0cc251c618a9e99d70409a893cf0` |
| on-ramp-250 | actor | False/invalid | 2500/2475/25 | 20/20/0 | 10231980282 | 513 / 513 | active_client=35, active_other_io=1, active_unclassified=47, active_wal_io=248, idle=46, idle_in_transaction=136 | True / 0 | `35fc7b7a65cb0cc251c618a9e99d70409a893cf0` |

## Store metric snapshot deltas

The following values are computed by subtracting before/after store snapshots. Durations are summed recorded time in nanoseconds; they overlap and must not be added together.

| Trial | Committed transactions | Commit sum (ns) | Acquire sum (ns) | FOR UPDATE roundtrip sum (ns) |
| --- | ---: | ---: | ---: | ---: |
| off-01 | 4176 | 2314286969 | 6175411 | 107860634 |
| on-01 | 4182 | 2311838355 | 5673500 | 112460168 |
| off-02 | 4184 | 2378068543 | 6293710 | 109659044 |
| on-02 | 4186 | 2366467904 | 5842536 | 109589331 |
| off-03 | 4182 | 2380444532 | 5572966 | 114372041 |
| on-03 | 4184 | 2393489224 | 6339548 | 119226722 |
| on-ramp-250 | 7902 | 5769125183 | 7027102 | 174366393 |

## PostgreSQL observer and interpretation

The six ordinary market/HTTP trials used a 50 Hz synthetic market source and 20 HTTP reads/s per account for a 20-second measurement; each report records 1,000 market offers and 400 view offers. The actor ramp used 250 Hz for 10 seconds with 20 actor reads; it is **invalid** because 25 of 2,500 source events were rejected and `source_delivery_valid` is false. Its financial conservation remains valid and final source recovery debt is zero.

For sampled runs, PostgreSQL activity is a 20 ms observer schedule against the shared local PostgreSQL development cluster. Category counts are backend observations (sample × backend), not time occupancy or CPU utilization. The 20 ms sampling interval can phase-lock with the 20 ms, 50 Hz source cadence, so activity-category counts are descriptive snapshots and are not a time-weighted profile. The off runs have no PostgreSQL activity sampler; missing activity is not a measured zero.

`store_metric_deltas` is computed only from before/after snapshots. Counter values and distribution count/sum/bucket counts are differenced; quantiles and extrema are never subtracted. Reported transaction/commit/acquire/row-lock sums overlap and are not additive wall-clock components. Store roundtrip timings include the client/query path and are not pure server execution time.

These are single-account synthetic paper runs on one local PostgreSQL development host. They do not establish multi-account actor capacity, supported capacity, SLOs, recovery headroom, Stage 2 completion, policy quality, or instrument-level economic cost. No capacity or headroom claim is made.

## Source identities and original bytes

Per-trial Go build metadata, clean source revision, runtime identity, report hash, byte lengths, and compressed hashes are in `summary.json` and `manifest.json`. No executable filesystem path or host address is included in bundle metadata. Trial order is fixed as `off-01`, `on-01`, `off-02`, `on-02`, `off-03`, `on-03`, `on-ramp-250`.

Manifest schema `runtime-pg-activity-evidence-v1`; original report bytes: 13099484; compressed bytes: 764132.

## Latency and executable cross-check

All seven reports identify executable SHA-256 `e08db2ef2362c4265cc2738c49f942c3ba3269bffe54f20b214a261c81479b9d` (26,349,981 bytes).

| Trial | Market offered-to-success p99 | Read offered-to-success p99 |
| --- | ---: | ---: |
| off-01 | 10.436 ms | 10.464 ms |
| on-01 | 9.704 ms | 9.977 ms |
| off-02 | 10.403 ms | 10.564 ms |
| on-02 | 11.347 ms | 10.131 ms |
| off-03 | 9.779 ms | 10.255 ms |
| on-03 | 10.455 ms | 9.625 ms |
| on-ramp-250 | 311.322 ms | 7.989 ms |

These nearest-rank p99s were independently recalculated from successful per-call samples; rejected and uncertain demand remain in the raw reports. Read timings include HTTP body consumption/validation for ordinary trials, while the ramp uses actor reads. Resource deltas in the summary include the process’s observer/generator/actor work and do not measure server CPU.

Root independently verified all seven original/compressed hashes and lengths, fourteen input-manifest hashes, offered/admitted/outcome/route counts, p99s, exact fill/fee cashflows and free balances, residual quantities, final cursor/hold/intent state and shutdown gauges. Sampler tick/lifetime/category denominators and selected counter/distribution count/sum/bucket deltas reconcile. No quantile/extrema subtraction was used. All six ordinary trials retain 0.02 BTC fills; the invalid ramp retains 0.01 BTC. Final conservation does not repair its 25 rejected source offers.

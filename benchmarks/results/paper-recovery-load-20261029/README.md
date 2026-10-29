# Paper recovery under HTTP demand — 29 October 2026

Three repeated trials use the same ordinary, untagged Go daemon built from clean
`bf3d4d51c09115094896785104ca1be649403fb6` (Go 1.26.8). Binary SHA-256:
`b8f4f79c4f8e2cfc7257f44dda7e2397a8af13c42eba36990e51a27c29ec7704`.
Each trial uses two independent processes/accounts and a fresh retained schema
on the local PostgreSQL 18.6 development cluster.

## Workload and results

Each process emits an initial snapshot and synthetic trades at **2 Hz**. During
a continuous ten-second interval, each account receives independently scheduled
**20 HTTP reads/s**: 120 state, 40 session-list and 40 report offers. The victim
is SIGKILLed at about three seconds, reaped, left unbound for at least 250 ms,
then resumed with the same account and original plan. HTTP scheduling continues
through the outage. The bystander remains running throughout.

| Trial | Victim HTTP outcomes | Bystander HTTP outcomes | Kill to first observed ready-paused | Cold launch to first observed ready-paused |
| --- | --- | --- | ---: | ---: |
| 01 | 195 completed, 5 endpoint unbound | 200 completed | 291.273 ms | 33.006 ms |
| 02 | 195 completed, 5 endpoint unbound | 200 completed | 296.290 ms | 36.272 ms |
| 03 | 195 completed, 5 endpoint unbound | 200 completed | 298.288 ms | 38.881 ms |

These are three observed durations, not percentiles. Kill-to-ready includes the
deliberate unbound interval. Listener/ready polling and HTTP time are included;
first-ready observation is an upper bound, with 20 ms ready-poll waits plus
request time. The reports preserve exact nanosecond timestamps.

All three trials report:

- 400 scheduled HTTP offers preserved, including every outage failure.
- Victim ownership epoch 1 → 2, recovered **paused**, with original execution
  pins retained. Retrying the original start is a duplicate and does not resume.
- Accepted fills remain 0.006 BTC at 100.01 USD, with 0.00120012 USD quote fees;
  the original 0.05 BTC target retains 0.044 BTC residual.
- Both final accounts hold exactly BTC 0.006 and USD 19.39873988, fully free.
  Parents are cancelled, source cursors are drained and original pins retained.
- Both final account/market/book readiness flags happen to be true; neither has
  recovery-required or clock-discontinuity flags. The exact stopped-source
  shutdown variant is covered separately by deterministic regression checks.
- Zero observed-minus-processed debt at the crash boundary. This does not prove
  recovery of a nonzero backlog. The bystander source advances from 11 to 25;
  the resumed victim ends at 24/24. All three process lifetimes are reaped and
  both graceful shutdowns report known-final.

The configured connection caps are 16 per daemon, two for the audit store, and
at most one simultaneous direct audit SQL connection: total cap 35. These are
configured maxima, not observed occupancy or a capacity recommendation.

## Retained evidence and independent inspection

`trial-NN.json.gz` is deterministic gzip (level 9, zero mtime, empty filename)
of each exact original JSON report. Decompression reproduces the original bytes.
`manifest.json` records compressed and uncompressed sizes and SHA-256 hashes;
`summary.json` projects counts, timings, source identity, plan pins and actual
financial/readiness audit fields. No source sample or failed offer was removed.

Root independently checked all six hashes and lengths against original files,
recounted outcomes/routes and scheduled indices, bounded failures to the victim
outage, and recomputed both accounts' exact signed fill/fee cashflows and free
balances from the reported accepted fills for every trial. It also checked
residual quantities, zero cursor debt, process reaping and known-final flags.
This is independent report inspection, not another venue-data source.

Reproduce with the task's development PostgreSQL DSN and a new output path:

```sh
LIQUIDITYLAB_TEST_POSTGRES_DSN="$TASK_DEVELOPMENT_DSN" \
LIQUIDITYLAB_RECOVERY_LOAD_OUTPUT=/fresh/trial.json \
go test -race -count=1 ./cmd/liquiditylabd \
  -run '^TestPaperProcessLoadRecoveryWithBystanderDemand$' -v
```

See [the harness contract](../../../docs/paper-load-recovery.md) for boundaries.
The ordinary daemon is not race-instrumented; the supervising Go test is.

## Limits

These are two separate daemon processes with separate actors and pools, sharing
a development database. They do not prove shared multi-account supervision,
50 Hz source recovery, supported admission/headroom, production availability,
backup/restore, private exchange behavior or live trading. The synthetic source
has no independent offered-load schedule; only HTTP demand does. Synthetic
events during victim downtime were never accepted and are not replayed. The
persisted accepted paper outcomes are the financial recovery source.

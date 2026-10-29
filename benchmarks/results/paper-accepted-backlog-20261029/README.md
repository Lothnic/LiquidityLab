# Accepted-backlog paper recovery evidence

This package preserves every `trial-*.json` report found in the supplied input directory. Each compressed report is deterministic gzip (mtime zero, empty member filename) over the exact original JSON bytes; decompressing it reproduces the original file byte-for-byte. No trial is filtered by valid/status. Incomplete failed reports retain their raw bytes and an explicit summary error. A source-mismatched cohort aborts packaging rather than omitting a report.

The summary is a projection of report fields. It reports exact decimal strings and exact-decimal sums of the 128 fixture fill rows, but does not independently verify financial correctness or upgrade the source report's status.

## Trial summary

| Trial | Source state | Report valid/status/issues | Prefix before kill / after reap | Debt / held reservation | Batch quantity / fee totals | Kill-to-ready / cold launch-to-ready | Daemon revision / SHA-256 |
| --- | --- | --- | ---: | --- | --- | --- | --- |
| trial-01 | clean | True/complete/0 | 1/2 of 128 | 1 / 0.5964434746875 | 0.003 / `{"quote":"0.000299970000000"}` | 1207468843 / 938208319 ns | `883704c02d925d7585440e67e845751e39e38850` / `5046cdcf4aba516241b7c6d59db14d527fbaca1f988607e99516d5707532d92d` |
| trial-02 | clean | True/complete/0 | 1/1 of 128 | 1 / 0.59879167734375 | 0.003 / `{"quote":"0.000299970000000"}` | 1240807209 / 969231433 ns | `883704c02d925d7585440e67e845751e39e38850` / `5046cdcf4aba516241b7c6d59db14d527fbaca1f988607e99516d5707532d92d` |
| trial-03 | clean | True/complete/0 | 1/2 of 128 | 1 / 0.5964434746875 | 0.003 / `{"quote":"0.000299970000000"}` | 1243104426 / 968068274 ns | `883704c02d925d7585440e67e845751e39e38850` / `5046cdcf4aba516241b7c6d59db14d527fbaca1f988607e99516d5707532d92d` |

## Scope and limits

Each trial authors one 128-fill simulated transport outcome through the owned observation path while the victim is stopped and caught up. That explicit fixture creates accepted backlog for a crash-boundary check; it is not feedback from the ordinary 2 Hz synthetic source and is not a natural source backlog measurement. The two ordinary daemon processes serve two accounts; this is not a shared multi-account actor/process benchmark.

The reported kill-to-ready and cold-launch-to-ready values end at the first ready/paused observation. Poll intervals and request duration are included, so they are observed upper bounds, not SLOs. Fixture preparation is outside the measured schedule. No capacity, source-rate support, throughput, headroom, or production claim follows from these runs.

`summary.json` contains source-reported recovered/final audit flags, exact balance/fill/fee strings, cursor values, original plan pins, workload counts, event times, and process/bystander evidence. `manifest.json` records original and compressed byte lengths and SHA-256 digests. Original report directories and local schema paths are not copied into package metadata; raw report contents remain intact inside the compressed original and must be handled under the same access controls as their source.

Bundle schema: `paper-process-accepted-backlog-recovery-bundle-v1`. Reports retained: 3; total original bytes: 915998; total compressed bytes: 64378.

## Clean source and exact recovery result

All three ordinary daemons were built from clean
`883704c02d925d7585440e67e845751e39e38850` with Go 1.26.8, without race
instrumentation or fault tags. Each has SHA-256
`5046cdcf4aba516241b7c6d59db14d527fbaca1f988607e99516d5707532d92d`.
The supervising tests use the race detector. Each trial uses a fresh retained
schema on development PostgreSQL 18.6. Environment metadata was collected after
the trials; exclusive database/host activity was not established.

After kill and reap, the snapshot and execution prefixes are exactly **2, 1,
and 2 of 128**. All three retain source debt 6−5=1 and a positive reservation.
The batch is 0.003 BTC: 128 unique fills of 0.0000234375 BTC at 99.99 USD, each
with 0.000002343515625 USD maker fee. Each recovered victim stays paused and
retains all 128 accepted fills, total fees 0.00029997 USD, balances BTC 0.003 /
USD 19.69973003 and residual 0.047 BTC. Retrying its original start is a duplicate
and does not resume execution.

Every bystander retains its independent ownership and advances source sequence
5→25. It finishes with BTC 0.006 / USD 19.39873988 and residual 0.044 BTC. Both
accounts end cancelled, with no child, held reservation or nonfinal intent,
matching drained source cursors, and original plan/start/deadline pins. All
three process lifetimes are reaped and both graceful shutdowns are known final.

Each trial preserves all 400 HTTP offers: the victim has 176 successful reads
and 24 endpoint-unbound offers; the bystander has 200 successful reads. The
route counts are 120 state, 40 sessions and 40 report offers per account. No
outage offer is removed from the denominator.

The earlier clean `19a35c9` full-suite attempt failed because account and
execution reads could span a server-side COMMIT after client reap. Commit
`87a0ca3` adds a bounded account-row-lock barrier and a deterministic regression
that demonstrably waits for the pending writer. This cohort uses that fix;
strict prefix, held obligation and exact finance requirements were retained.
The earlier failure remains recorded in the verification history.

## Independent inspection and configured suite

Root independently checked all original/compressed hashes and lengths,
recounted every HTTP index/outcome/route and its admission/completion ordering,
checked outage and kill/reap/launch/ready/proof boundaries, and recomputed exact
signed cashflows with rational arithmetic. Read-only queries against each
retained schema additionally compared accepted observation fills, execution
rows, final snapshots, balances, original pins, owner epochs, drained cursors,
reservation/outbox finality and fee rates. The stored batch JSONB payload hash
also matches the crash observation manifest. `independent-audit.json` records
this local inspection. It is not an independent venue-data source and cannot
reconstruct a past crash prefix from the now-final database; that prefix comes
from the retained bounded harness observation.

The separate full uncached Go race run on the same clean source passes every
package containing tests: 1,078 test/subtest pass events and zero skipped test
cases. Three packages have no tests. The daemon package takes 66.036 s and the
runtime package 82.910 s. `full-go.jsonl.gz` preserves exact Go JSON output;
`full-go-summary.json` records its source state and raw SHA-256. The separate
[combined conformance run](../../../conformance/results/20261029-contracts-883704c.json)
passes all five steps and 216 Python checks with zero skips/failures/errors and
unchanged hashes for all four fixtures.

Reproduce the process check with an explicitly selected development DSN and a
fresh report path using [the accepted-backlog runbook](../../../docs/paper-load-recovery.md#accepted-backlog-process-variant).
This evidence closes a local paper crash-boundary proof. Supported admission,
natural feed backlog/recovery headroom, private sender isolation, live behavior,
soak and database backup/restore remain separate gates.

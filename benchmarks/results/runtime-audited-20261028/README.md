# Audited durable runtime v2 evidence — 2026-10-28

This bundle preserves three one-account, one-instrument development runs: two valid HTTP polling runs and one invalid 250 market-events/s block-profile run. JSON reports are deterministic gzip (`mtime=0`, empty original filename) of the exact report source bytes. `manifest.json` records compressed and uncompressed lengths and SHA-256 values, runner identity/hash, clean launch metadata, workload configuration, validation contract IDs, phase counts, p99s, policy counter snapshots, independent financial audit, final debt/gauges, and profile integrity. `summary.json` gives compact per-run figures. Invocation metadata, logs, and socket/listener addresses are not included.

The exact runner binary was Go 1.26.8, source revision `fc525b151b8d34d32e32376c398bab9822c49517`, embedded `vcs.modified=false`, SHA-256 `0678f239813e12572bdb06b5dd45306268ef8b4af786abd6270757c0170bd930` (26,305,306 bytes). All three runs launched from clean worktrees at that revision. They used a shared local PostgreSQL 18.6 development host, one synthetic account, one BTC/USD instrument, and a four-connection pool; WAL is cluster-wide. This is one trial per configuration, not a capacity study.

## Phase results

Latency is the runner's per-call offered-to-success p99 for that phase, including queueing. HTTP timing includes the complete scoped API response, body consumption, and JSON validation, but excludes React rendering. HTTP route p99s are nearest-rank values from their actual per-call samples.

| Report | Offered market / reads | Measurement outcome | Market p99 | Read p99 | Terminal fills |
| --- | ---: | --- | ---: | ---: | ---: |
| `http-baseline-audited.json.gz` | 50/s / 20/s HTTP | 1,000/1,000 market and 400/400 reads completed | 11.540 ms | 10.971 ms | 2 (0.02 BTC) |
| `http-python-unvalidated-audited.json.gz` | 50/s / 20/s HTTP | 1,000/1,000 market and 400/400 reads completed | 32.506 ms | 35.112 ms | 107 (0.997 BTC) |
| `actor-baseline-block.json.gz` | 250/s / 2/s actor | **Invalid:** 2,210/2,500 market calls completed; 290 rejected | 394.053 ms | 8.065 ms | 1 (0.01 BTC) |

Both HTTP reports declare `scoped-account-plan-v1`; each recorded all measured HTTP responses with status 200. All three declare `exact-cashflow-terminal-v2` for the independent financial conservation audit. The block run did not request HTTP validation. All three financial audits are valid, final source-recovery debt is zero, shutdown gauges are zero, and policy calls are quiescent at shutdown. Those final invariants do not turn the overloaded block-profile run into a valid workload result.

The HTTP baseline route p99s were 11.116 ms for account state (240 responses), 2.462 ms for session list (80), and 3.406 ms for current report (80). For the Python-worker HTTP run they were 37.294 ms for state, 2.227 ms for sessions, and 5.445 ms for reports, with the same route counts. These are local API measurements only.

The external local Python worker used bundle hash `0e73f473892a2939910ea17420eb36d22b8f38c8e479b1b95e0a79d47fad7e6b`, explicitly marked `unvalidated`. Its counters were 10 successful calls before measurement and 110 after measurement: 100 measured requests, all successful, with zero failed, rejected, or expired. The whole-session terminal audit contains 107 durable fills. This is evidence of RPC and paper integration only; it says nothing about model quality. Worker CPU/memory and server idleness were not measured.

Fill counts are terminal whole-session totals; the reports do not attribute each fill to warmup versus measurement. Treat them as economic conservation evidence, not measured-phase fill coverage. Warmup and actor/runner setup are separate from the measurement schedules; no comparison here isolates their overhead.

## Block profile

The single block-profile run used `runtime.SetBlockProfileRate(1)` for the measured schedule and accepted-call drain, then disabled profiling before resource endpoints. The profile is a cumulative snapshot of completed Go channel/synchronization blocking stacks; it does not include PostgreSQL fsync or external worker blocking. It reports 20.77 seconds of aggregate delay and 6,856 contentions, with `runtime.selectgo` accounting for 97.94% of delay and 99.46% of contentions. Aggregate delay can exceed elapsed wall time because goroutines wait concurrently; it is not request latency or a wall-clock stall. This is a single diagnostic profile from an invalid offered-load run, not a controlled estimate of profiler overhead.

The raw block profile and `go tool pprof` outputs are in `profiles/`. Reproduce them against the exact binary whose hash is in the manifest by setting `LIQUIDITYLAB_AUDITED_BINARY`, then running from the repository root:

```sh
go tool pprof -top -sample_index=delay -nodecount=25 "$LIQUIDITYLAB_AUDITED_BINARY" benchmarks/results/runtime-audited-20261028/profiles/actor-baseline-block.pprof
go tool pprof -top -sample_index=contentions -nodecount=25 "$LIQUIDITYLAB_AUDITED_BINARY" benchmarks/results/runtime-audited-20261028/profiles/actor-baseline-block.pprof
go tool pprof -top -cum -sample_index=delay -nodecount=25 "$LIQUIDITYLAB_AUDITED_BINARY" benchmarks/results/runtime-audited-20261028/profiles/actor-baseline-block.pprof
go tool pprof -top -cum -sample_index=contentions -nodecount=25 "$LIQUIDITYLAB_AUDITED_BINARY" benchmarks/results/runtime-audited-20261028/profiles/actor-baseline-block.pprof
```

## Limits

These are synthetic, single-account and single-instrument runs on a shared development host. There is only one trial per configuration. The policy bundle is unvalidated, and no product capacity, SLO, production readiness, real venue, or live-order claim follows from these reports. Database WAL includes concurrent cluster activity; process resource deltas include the runner and observer work.

Telemetry histograms are cumulative snapshots. Counters, histogram bucket counts, and sums may be differenced only across valid same-process endpoints; cumulative min/max and quantiles cannot be differenced into interval values. The runner p99s above are derived from per-call phase samples, not differences of telemetry histogram quantiles.

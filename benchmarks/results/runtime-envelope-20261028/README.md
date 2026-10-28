# Durable runtime v2 envelope evidence — 2026-10-28

This bundle contains seven completed report artifacts from the v2 runner: six valid reports and one retained invalid allocation-profile run. Reports are deterministic gzip (`mtime=0`, empty original filename) of the exact JSON source bytes. `manifest.json` records compressed and uncompressed lengths and SHA-256 values, profile hashes, workload configuration, validity, financial audit, final debt/gauges, policy counters, and the exact runner binary identity. `summary.json` contains compact per-run counts, p99s, policy deltas, and resource deltas. No invocation log or run log is included.

The runner binary was Go 1.26.8, source revision `929522ced593d89aaa7fed7574c0d9e83d02b7b0`, embedded `vcs.modified=false`, SHA-256 `f3c90c5194b8fd6168d95f75daa9dbf7f59ed3b055c0c89053d8629d81b47896` (26,271,640 bytes). Launch worktree metadata varies by report and is recorded per report in the manifest; the HTTP baseline and healthy-mock reports launched from clean worktrees; the CPU-profile report and other four reports launched from dirty worktrees. The shared host was an AMD Ryzen 7 7840HS with 16 logical CPUs; PostgreSQL 18.6 used a local ext4/NVMe volume. Each run uses one synthetic account, one BTC/USD instrument, a four-connection pool, and a 64-entry ordered source queue. WAL is cluster-wide. These are local development measurements, not durable-device claims.

## Selected observations

Latency columns are per-call offered-to-success p99s from the measurement phase. HTTP p99 includes the full scoped API response, body consumption, and JSON validation; it does not include React rendering. Route p99s use nearest-rank percentiles from that route's sample records.

| Report | Offered market / reads | Policy workload | Market p99 | Read p99 | Result |
| --- | ---: | --- | ---: | ---: | --- |
| `http-baseline.json.gz` | 50/s / 20/s HTTP | Go baseline | 11.346 ms | 11.078 ms | complete, 0 rejected |
| `http-controlled-healthy.json.gz` | 50/s / 20/s HTTP | Local gRPC mock, 10 ms response, 100 ms timeout | 14.321 ms | 11.147 ms | complete; 50 measured RPCs succeeded |
| `http-controlled-timeout-baseline.json.gz` | 50/s / 20/s HTTP | Local gRPC mock, 250 ms response, 20 ms timeout, baseline fallback | 18.688 ms | 21.094 ms | complete; 50 measured RPCs timed out; baseline continued |
| `http-controlled-timeout-pause.json.gz` | 50/s / 20/s HTTP | Same delayed mock, pause fallback | 11.450 ms | 11.386 ms | complete; warmup timeout paused decisions before measurement (0 measured RPCs) |
| `http-python-unvalidated.json.gz` | 50/s / 20/s HTTP | External local worker, bundle unvalidated | 40.419 ms | 24.354 ms | complete; 100 measured RPCs succeeded |
| `actor-baseline-cpu.json.gz` | 250/s / 2/s actor | Go baseline, CPU profile | 232.590 ms | 9.800 ms | complete, 0 rejected |
| `actor-baseline-allocs.json.gz` | 250/s / 2/s actor | Go baseline, allocation profile | 395.594 ms | 17.323 ms | **invalid**; 375 of 2,500 offered market calls were rejected |

The 250/s CPU and allocation runs are separate trials, not a controlled profiler on/off comparison. Their scheduled fills occurred during warmup: each measured phase was 10 seconds, shorter than the 15-second scheduled-slice cadence, so they provide no measured-phase fill coverage. The same warmup-only fill limitation applies to the 50/s timeout-baseline run. The allocation run remains invalid even though its final financial audit is valid, its source debt is zero, and shutdown gauges and inference in-flight count are zero. Those terminal invariants do not restore the 375 rejected offers or make the latency comparable. Preserve this result as overload evidence, not as a passing rate.

The HTTP baseline's measured route p99s were 11.153 ms for account state (240 calls), 3.146 ms for session list (80), and 2.684 ms for current report (80). The unvalidated local Python-worker run used bundle hash `0e73f473892a2939910ea17420eb36d22b8f38c8e479b1b95e0a79d47fad7e6b`; 100 measured RPCs succeeded and the complete run lifecycle recorded 107 fills. The pause-fallback case timed out once during warmup (client count was 1 before and after the measured phase), so it has zero measured inference RPCs; it demonstrates the pre-measurement paused state, not a measured-phase timeout response. The bundle is explicitly unvalidated; these counts say nothing about policy quality. Worker CPU/memory and server idleness were not measured.

All seven reports independently record a valid financial-conservation audit, zero final source-recovery debt, zero shutdown gauges, and zero policy RPCs in flight at shutdown. This covers the persisted run state, not offered-load success, capacity, production readiness, private venue behavior, or a live order lifecycle.

## Profiles

The two pprof samples are preserved under `profiles/`; the accompanying `*-top*.txt` files were generated against the exact runner executable named above. CPU sampling covered 10.24 seconds and collected 2.32 seconds of samples (22.66%). The flat top includes `internal/runtime/syscall/linux.Syscall6` at 22.84% of sampled CPU. Cumulative view attributes 24.57% to `encoding/json.Unmarshal` and 28.88% to PostgreSQL `GetAccount`. Allocation-space cumulative view attributes 31.55% to `runtime.cloneState` and 23.71% to PostgreSQL `GetAccount`. Cumulative call-tree rows overlap; do not add them. The profiles cover distinct collection windows and are diagnostic samples, not benchmark overhead estimates.

Reproduce the saved text views by setting `LIQUIDITYLAB_V2_BINARY` to the exact runner executable whose SHA-256 is recorded above, then from the repository root run:

```sh
go tool pprof -top -nodecount=25 "$LIQUIDITYLAB_V2_BINARY" benchmarks/results/runtime-envelope-20261028/profiles/actor-baseline-cpu.pprof
go tool pprof -top -cum -nodecount=25 "$LIQUIDITYLAB_V2_BINARY" benchmarks/results/runtime-envelope-20261028/profiles/actor-baseline-cpu.pprof
go tool pprof -top -nodecount=25 "$LIQUIDITYLAB_V2_BINARY" benchmarks/results/runtime-envelope-20261028/profiles/actor-baseline-allocs.pprof
go tool pprof -top -cum -nodecount=25 "$LIQUIDITYLAB_V2_BINARY" benchmarks/results/runtime-envelope-20261028/profiles/actor-baseline-allocs.pprof
```

## Interpretation limits

These were synthetic one-account workloads on a shared development host. The archived HTTP runs used an earlier runner whose 200-response checks validated JSON syntax but did not assert response identity; the current runner is being corrected separately, and these reports remain immutable records of the earlier binary. The v2 runner's HTTP mode measures scoped API polling, not browser rendering. The local controlled gRPC fixture tests serialization, deadlines, cancellation, and fallback, not Python model inference or model quality. The external worker is unvalidated and its host resources are outside the report's process accounting.

Runner latency summaries (`market_latencies` and `view_latencies`) use this phase's per-call samples. Telemetry counters and histogram bucket counts/sums may be differenced between valid same-process endpoints; cumulative min/max and percentile fields cannot be differenced into interval extrema or quantiles. Resource deltas include the generator and observer work; database WAL deltas include other activity on the cluster. No run establishes supported capacity, a service-level objective, or a causal profiling comparison.

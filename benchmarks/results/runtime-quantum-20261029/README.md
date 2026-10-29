# Runtime scheduling workload evidence — 29 October 2026

Four serial trials use the same clean `75d71fd2935104a121d29e7d78c299cfed36a835`
ordinary workload executable. SHA-256:
`599baa9160e7961809ce69e81147cc9f172ab15dfff0c06aca922d85f242513f`
(26,382,957 bytes). The effective actor manifest records four reduction steps,
5 ms cooperative elapsed budget and 64 View slots. Each atomic SQL transition
can exceed that budget. These are single-account synthetic paper workloads.

## Results

Each ordinary trial warms up for 2 seconds, then offers 50 market events/s and
20 HTTP reads/s for 20 seconds. All three complete 1,000/1,000 source updates
and 400/400 reads, with no rejection, failure or unresolved call. Each read mix
contains 240 account states, 80 session lists and 80 session reports. Source
delivery and financial conservation are valid, and final debt is zero.

Empirical offered-to-success p99s, independently recalculated with nearest rank:

- `ordinary-01`: market 9.732216 ms; HTTP read 9.612650 ms.
- `ordinary-02`: market 9.823922 ms; HTTP read 8.760544 ms.
- `ordinary-03`: market 10.167940 ms; HTTP read 9.280617 ms.

Each ordinary final account has two unique 0.01 BTC fills at 101, each with
0.00202 USD quote fees. Final free balances are 10.02 BTC / 99997.97596 USD,
residual quantity is 9.98 BTC, and the parent is cancelled with no child, holds,
intents, pending facts or policy slot. Local cleanup receipt durations are
5.391687, 5.135765 and 5.162884 ms. These are three individual acknowledgements
after the measured schedules, not control-latency percentiles under backlog.

The separate `ramp-250` offers 250 market events/s for 10 seconds, with 2 actor
reads/s and a 20 ms scoped PostgreSQL observer. It completes 2,232 source calls
and rejects **268/2,500** at runner admission. All 20 reads complete. The report
is **invalid** because required source offers were lost. Its market
offered-to-success p99 is 391.115856 ms; actor-read p99 is 8.267463 ms. These
success-only percentiles do not include rejected work as successful latency.
Financial conservation remains valid: one 0.01 BTC fill at 101 with 0.00202 USD
fees; final free balances 10.01 BTC / 99998.98798 USD, residual 9.99 BTC, no
outstanding obligations, cancelled parent and zero debt.

## Persistence observations

Measured before/after snapshot deltas, including phase drain:

- `ordinary-01`: 4,162 commits; 2.368481705 s summed client COMMIT time.
- `ordinary-02`: 4,181 commits; 2.316042729 s summed client COMMIT time.
- `ordinary-03`: 4,169 commits; 2.789416978 s summed client COMMIT time.
- `ramp-250`: 7,096 commits; 5.322231005 s summed client COMMIT time,
  7.492175 ms pool acquisition and 179.395317 ms account FOR UPDATE round trips
  over a 10.296383847 s phase including drain.

These durations overlap and cannot be added as exclusive elapsed components.
They include client/driver/scheduling effects and do not isolate pure fsync,
server CPU or lock-only waits. Full distribution count/sum/bucket deltas are
retained in `summary.json`; cumulative quantiles/extrema are not subtracted.

The ramp observer completes all 516 intended ticks through its stop cutoff,
retaining 516 observations of one store backend: 207 active WAL IO, 129 active
unclassified, 28 active client, one active other IO, 47 idle and 104 idle in
transaction. These are snapshots, not time occupancy or CPU percentages. The
observer has one separate connection; no ordinary trial requested sampling.

## Hardware, interpretation and remaining gates

These reports identify an AMD Ryzen 7 7840HS, **16 logical CPUs/GOMAXPROCS 16**,
Go 1.26.8 and retained local PostgreSQL 18.6 on a shared development host.
The earlier PostgreSQL cohort identifies 24 logical CPUs. Neither host is an
isolated production benchmark environment. Do not treat differences from the
earlier cohort as a controlled scheduling speedup or regression. Wall-clock
decisions and account histories also vary between repetitions despite fixed
source seeds. The higher ramp rejection count is retained explicitly.

The new scheduler passes accounting and modest steady workload checks. These
trials do not exercise a process restart with accepted backlog, multi-account
fairness, production control SLOs or admission with recovery headroom. The
512-fill deterministic tests and ordinary daemon crash matrix are separate
evidence. Stage 2's admission/headroom gate remains open. No exchange orders or
policy-quality claims are involved.

## Reproduction and artifact audit

Build `./benchmarks/runtime` from the stated revision with ordinary Go settings.
Use an explicit development DSN and fresh output paths. All trials use seed 42,
scheduled baseline, 100 ms decision cadence, 50% trades, pool 4, source queue 64,
four read slots and 2 s warmup. Ordinary trials select `--read-mode http`,
`--duration 20s --market-rate 50 --view-rate 20` with sampling disabled. The
ramp selects `--read-mode actor --duration 10s --market-rate 250 --view-rate 2
--db-activity-interval 20ms`. Keep the overload report when the command exits
nonzero. Each invocation creates a new account; no account/schema is reset.

`manifest.json` records original/compressed SHA-256 and byte lengths.
Deterministic gzip members decompress to the exact original JSON bytes. The
summary preserves effective configuration, executable/build identities,
hardware, resource deltas, counts, empirical p99s and store deltas; it does not
replace the raw reports. Root independently checked all scheduled sample IDs,
offered/admitted/outcome counts, monotonic timing, eight input manifest hashes,
HTTP route counts, unique fill fees/cashflows, residuals, terminal snapshot
agreement, cursors, held obligations and shutdown gauges. Observer ticks,
categories and backend denominators, plus metric count/sum/bucket deltas were
also checked against the retained reports.

# Replay throughput comparison

Run the deterministic, bounded synthetic comparison with the explicit Go
replay executable:

```sh
.venv/bin/python benchmarks/replay_comparison.py \
  --binary /path/to/liquiditylabd \
  --episodes-per-env 2 \
  --repetitions 2 \
  --max-seconds 50
```

The benchmark warms Python and Go first, then measures scalar Python reference
episodes against Go replay batches of 1, 8, and 32 environments. Each uses the
same synthetic tape and fixed policy action `0` (HOLD; Go wire action `1`),
with a 30-second plan and 1-second decision interval. Process/create/upload,
initial reset, steady step calls, steady resets, and wall throughput are
reported separately. The Go timings
cover the client, encoding, local gRPC transport, server dispatch, and replay
core together; they do not isolate Go core speed or establish a training
overhead percentage. Terminal reports are checked for valid economic outcomes
and exact semantic parity after canonicalizing decimal scale.

The checked-in `results/reference-host.json` is a raw two-pass capture from the
host recorded in that file. Re-run the command on another host to produce a
new result at the same path. These small synthetic runs are a reproducibility
check, not evidence of production-scale capacity.

## Profile and allocation improvement

The original exact-decimal normalizer divided by ten once per trailing fractional
zero, creating many big integers. The CPU profile showed allocation and big-int
division costs. Commit `3dfb53b` replaces this with one exact normalization division,
private immutable power-of-ten factors, and allocation-free sign/equal-scale
comparison. Monetary arithmetic, rounding, and economic conformance are unchanged.

`native-before.json` used the core at `cc0625f`; `native-after.json` used the
normalization change. Run the native workload with:

```sh
go run ./benchmarks/native --episodes 1000 --cpu-profile /tmp/liquiditylab-replay.pprof
go tool pprof -top /tmp/liquiditylab-replay.pprof
```

On the recorded host, those two native runs measured about 16.4k and 58.6k
transitions/sec, with allocations decreasing from about 2,069 to 523 per
transition. These are single profile runs; they establish an observed improvement
in this workload, not a general speedup guarantee.

`optimized-host.json` repeats the Python/client comparison after the change. Go
with gRPC measured about 4.4–5.0k, 16.4–16.8k, and 23.0–23.2k transitions/sec for
batches of 1, 8, and 32. Python measured about 37.4–45.8k on the same workloads.
All terminal outcomes matched, but the bridge is still slower here. Keep the
Python reference as the default training backend. The native core and the entire
Python/RPC path are distinct measurements; the <=10% training transport overhead
gate remains unverified, and production capacity/soak gates remain open.

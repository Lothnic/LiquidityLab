# Paper runtime metrics

The paper daemon can write one bounded, process-local telemetry snapshot at
shutdown. The recorder has fixed metric IDs, fixed histogram buckets, and no
dynamic labels or account identifiers. It is diagnostic data for a run; it does
not establish supported account capacity, production readiness, or live-execution
safety. One metrics-bearing real public capture is preserved, but it is a single
short capture rather than a repeatable paper-runtime baseline. The separate
durable workload benchmark and its limits are described in
[benchmarks/runtime/README.md](../benchmarks/runtime/README.md).

## Enable a local export

Add `--metrics-output` to the existing `liquiditylabd paper create` command in
[paper-server.md](paper-server.md). For example, keep that command's documented
flags and append a new, unused local filename:

```sh
build/liquiditylabd paper create \
  --listen 127.0.0.1:8080 \
  --schema ll_paper_server_20261023_a \
  --account synthetic_20261023_a \
  --source synthetic \
  --web-dir apps/web/dist \
  --initial-base 0 \
  --initial-quote 100 \
  --duration 30m \
  --metrics-output ./paper-metrics.jsonl
```

Use a path that does not already exist and whose parent directory already
exists. The daemon reserves it with exclusive create and mode `0600` before
database mutation; it will not overwrite an existing file. For Kraken capture,
the metrics path must also differ from the recording and its manifest paths.
The file is local and is never served through the HTTP API. The flag is
optional; when omitted, the daemon does not create a recorder or enable these
collectors. Database URL and bearer-token setup remain as documented in
[paper-server.md](paper-server.md); neither belongs in the metrics command or
file.

The JSONL file contains `paper-metrics-v1` wrapper records. It starts with a
synced record whose `capture_state` is `incomplete`. On shutdown, the daemon
appends and syncs a second record with `capture_state: "shutdown_snapshot"`,
`telemetry-v1` metrics, pool statistics, timestamp, and
`financial_state_known_final`. The metrics and pool snapshots are read
sequentially, not atomically. The file contains no SQL, DSN, bearer token,
account IDs, or policy hash. If the process exits before the final frame is
complete, retain the initial marker and ignore a malformed or unterminated
trailing JSONL record. A file with only the initial marker is not a completed
metrics capture. `financial_state_known_final` describes the daemon's shutdown
account check; it does not certify the telemetry as a capacity result.

## Counters and denominators

Counters are cumulative for this daemon process. The metric names below are the
stable JSON `name` values. Ratios require matching observation windows; at
shutdown, accepted actor work is drained before the final snapshot.

| Group | Counters | Interpretation |
| --- | --- | --- |
| Actor | `actor_admitted`, `actor_rejected`, `actor_completed`, `actor_failed` | Admitted means published to an actor mailbox. Rejected includes pre-cancelled, closed, and full-mailbox submissions. Completed and failed count handler outcomes, not whether the waiting caller received its reply. After a clean drain, use `admitted` as the denominator for completion/failure (`completed + failed` should equal admitted); rejected counts submission attempts separately and is not every caller error. |
| Market | `market_events`, `market_errors` | Market requests that reached the actor handler and those whose handling failed. These do not count offered source events or requests rejected before publication. |
| Database transactions | `db_transactions_started`, `db_transactions_committed`, `db_transactions_rolled_back`, `db_commit_errors`, `db_rollback_errors`, `db_acquire_errors` | A started transaction follows successful pool acquisition and `BEGIN`. Use started as the denominator for committed, explicitly rolled-back, commit-error and rollback-error outcomes; acquisition and begin failures occur before `started`. Committed and rolled back count successful explicit outcomes. Commit errors are separate: a failed commit can be uncertain and is not counted as a rollback. Rollback errors likewise are not successful rollbacks. Acquire errors include failed or cancelled acquisition attempts. |
| Database work | `db_queries`, `db_query_errors`, `db_durable_transitions`, `db_observation_writes`, `db_observation_duplicates` | Query counters follow pgx query tracing. Durable transitions and observation writes count only after successful commit. Duplicate observations count idempotent duplicate recognition. |
| Inference | `inference_requests`, `inference_rejected`, `inference_rpcs`, `inference_errors` | Requests count entry to `Propose`; normally, rejected plus RPCs equals requests. Rejected counts validation, stale, busy, or other pre-RPC failures. RPCs count actual `Predict` calls; use RPCs as the denominator for post-call errors. Errors include incompatible responses and are a subset of RPCs. |
| Policy slots | `policy_decisions`, `policy_fallbacks` | Count newly acknowledged policy-slot resolutions and the fallback subset. A late worker result does not resolve or count a second decision. Fallback pauses include intentional control cancellation of a pending slot; the fallback count alone is not a worker-failure rate. These counters do not count arbitrary UI commands or every baseline decision. |

The fixed gauges are `actor_queued`, `actor_handling`,
`db_transactions_in_flight`, and `inference_in_flight`. They represent current
process-local work, not cumulative counts. Inspect them after shutdown, when
queued/handling work should have drained and transactions should have ended.
Overflow flags mean a gauge adjustment could not be represented safely; do not
use an overflowed gauge in a ratio.

## Distributions and timing boundaries

All duration metric names end in `_ns` and use nanoseconds. Size metrics end in
`_bytes`; `actor_queue_depth` uses `items`.

| Metric names | What the sample spans |
| --- | --- |
| `actor_queue_wait_ns` | Immediately before mailbox publication to dequeue/handler start for admitted actor work. |
| `actor_handle_ns` | One active handler turn, including its synchronous durable work. A yielding operation contributes multiple turn samples; waiting between turns is excluded. |
| `control_ack_ns` | Immediately before public `Do` mailbox submission through successful caller return for priority `pause`, `cancel`, or `deadline` commands. Failed calls are not sampled. This is a local command acknowledgement duration, not a standalone venue-cancel measurement. |
| `public_market_apply_ns` | Sum of active handler turns for one market request, emitted once on completion/failure. Queue wait and time parked between turns are excluded; use runner call timing for total operation latency. |
| `inference_rpc_ns` | Time inside the worker `Predict` RPC call, including failed calls. It does not include request validation or admission wait. |
| `decision_commit_ns` | For a baseline decision command, command-handler entry through acknowledged persistence. For an inference slot, start of decision processing/observation construction through acknowledged slot resolution. Duplicate retries and recovered pending work have no additional fabricated elapsed sample. |
| `db_acquire_ns` | Pool acquisition attempt including connection construction/ping and acquisition overhead; it is not pure pool-queue wait. Failed acquisitions are included. |
| `db_begin_ns` | `BEGIN` round trip after pool acquisition. |
| `db_transaction_ns` | Successful `BEGIN` return through the explicit commit or rollback attempt, sampled just before releasing the pool connection. Asynchronous destruction of a failed connection is excluded. |
| `db_commit_ns`, `db_rollback_ns` | Duration of the respective explicit completion attempt. A failed commit is an unknown/failed commit outcome, never evidence of rollback. |
| `db_account_for_update_roundtrip_ns` | The account `SELECT ... FOR UPDATE` call from entry through query result, including server/round-trip time and any row-lock wait. It does not isolate pure lock wait. |
| `db_snapshot_write_attempt_bytes`, `db_observation_write_attempt_bytes`, `db_journal_write_attempt_bytes` | Serialized payload sizes submitted by the application for SQL writes. They measure attempted payload bytes, including writes later rolled back; they are not relation size, network traffic, WAL bytes, or storage-device writes. |

The recorder also stores fixed histograms for `actor_queue_depth` and all listed
distributions. Duration bounds are 1,000; 5,000; 10,000; 25,000; 50,000;
100,000; 250,000; 500,000; 1,000,000; 2,500,000; 5,000,000; 10,000,000;
25,000,000; 50,000,000; 100,000,000; 150,000,000; 200,000,000;
250,000,000; 500,000,000; 1,000,000,000; 2,500,000,000; 5,000,000,000; and
10,000,000,000 ns, followed by an overflow bucket. Byte bounds are 64, 128,
256, 512, 1,024, 2,048, 4,096, 8,192, 16,384, 32,768, 65,536, 131,072,
262,144, 524,288, 1,048,576, and 2,097,152 bytes, followed by overflow.
Queue-depth bounds are 0, 1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1,024,
4,096, and 8,192 items, followed by overflow. Bucket counts are per-bucket,
not cumulative.

`p50_upper_bound`, `p95_upper_bound`, and `p99_upper_bound` are nearest-rank
histogram upper-bound estimates (`quantile_method: "bucket_upper_bound"`), not
exact sample percentiles. They are `null` when empty or when the requested
quantile falls in the overflow bucket. Sums and counts saturate instead of
wrapping and carry overflow flags. If you subtract two snapshots from the same
process, subtract counter values and histogram bucket counts only when neither
endpoint is overflowed. Do not subtract cumulative histogram min, max, or
quantile estimates to infer interval extrema or quantiles; retain per-interval
samples for that purpose.

## Limits and remaining measurements

Instrumented Kraken capture additionally supplies local source timing:

- `capture_frames` and `capture_input_bytes` count successful WebSocket reads and payload bytes, including non-text frames; bytes exclude WebSocket/network framing. The byte counter has unit `bytes`; other counters use `count` and gauges use `items`.
- `capture_persist_attempts`/`capture_persist_errors` count source-row writes, flushes and file `Sync` attempts, including explicit gaps. `capture_persist_ns` spans each attempt, including failures. The final `capture_end` footer is outside this source-row measurement.
- `capture_read_to_durable_ns` spans successful text-frame read completion through validation, recording/flush and acknowledged `Sync`. Failed persistence produces no successful durability sample. A successful `Sync` is not a storage-device durability proof.
- `capture_delivery_attempts`/`capture_delivery_errors` count normalized delivery attempts, including terminal gap delivery. A raw frame can emit multiple events or none. `capture_receipt_to_consumer_ns` spans a raw frame's local read completion through each normalized callback outcome, including earlier callbacks for that frame; it excludes standalone gap deliveries with no raw-frame origin.
- `public_ingress_requests`/`public_ingress_errors` and `public_ingress_ns` describe the daemon's normalized callback, including ingress serialization, conversion, actor work and any initial readiness recovery. `public_receipt_to_book_ns` samples valid snapshot/book acknowledgements from the native local read origin, including recording and queues, before subsequent initial readiness recovery.
- `public_receipt_origin_missing` counts daemon callbacks with no native origin. Synthetic and offline inputs never manufacture receive-to-book latency from a wall timestamp. The origin is a process-local monotonic `time.Time` carried only in the callback context; it is absent from recordings, normalized event JSON, account snapshots and cross-process messages. It does not measure exchange-to-host or kernel socket backlog.

The [real Kraken BTC/USD capture](../examples/recordings/kraken-btcusd-20261028/README.md)
preserves a 50-frame, 3.92-second run and its daemon telemetry. Its native
receipt-to-book p99 lands in the 200 ms histogram upper-bound bucket, and the
recorded persistence maximum is 180.386 ms for encoding, flush, and `Sync`
together. These are single-run observations, not an SLO pass or capacity claim.
The [durable baseline evidence](../benchmarks/results/durable-runtime-20261028/README.md)
preserves its original executable and v1 report schema; those synthetic runs
predate the public capture and do not measure recording performance.

These aggregate snapshots do not measure real private-event age. They also do
not establish inference load behavior, browser rendering latency, or a full
process/storage/worker fault envelope. Those need dedicated Stage 2 workloads
and fault runs. Do not use these counters or
histogram estimates alone to set concurrency limits or claim a supported
capacity. Run repeated durable workload measurements and review their scope and
interpretation in [benchmarks/runtime/README.md](../benchmarks/runtime/README.md).

# Local replay RPC contract

`liquiditylabd replay --listen unix:/absolute/path/service.sock` serves the generated
ReplayService. Literal loopback TCP addresses are also accepted (default
`127.0.0.1:50051`). The Unix socket has mode 0600. Existing socket paths are never
unlinked. SIGINT/SIGTERM allows five seconds for gRPC draining before forced stop.
This is an unauthenticated local offline worker; there is no public/trading API.

CreateBatch validates its complete episode before returning handles. Engines share
one private immutable tape, with isolated balances, books, liquidity debt, order
IDs, and histories. Optional scalar ReplayConfig values preserve explicit zeros;
absent fields use domain defaults. The conservative fill model is deterministic,
so reset seeds currently have no effect. Money accepts canonical integer
coefficients of up to 128 digits, scale at most 36, with no leading zeros, plus
sign, negative zero, or trailing fractional zeros. Derived monetary results may
use the core's larger internal precision envelope.

Every Reset/Step/Close must supply the next sequence (starting at 1) and a new
request ID. One context-aware mutex serializes each batch. All handles/actions are
validated before mutation. Once admitted to the engine, an operation finishes and
caches its result despite transport cancellation. Retry exactly the same request
identity and contents. A changed duplicate is AlreadyExists; an expired result or
out-of-order sequence is FailedPrecondition. Arithmetic failures or partial batch
failures poison the entire batch, consume their sequence, and cache the error.
Only Close can then mutate that batch. Validation errors do not consume sequence.

Default response retention is eight operations with an 8 MiB byte budget plus
metadata allowance per batch. A bounded identity ledger remembers every admitted
operation until Close, so eviction never permits a second advance. The batch
accepts at most 10,000 mutations, after which Close and recreation are required.
Close retains only its own result in one of 128 bounded tombstones; an evicted
close retry is NotFound. Create results are retained for 128 identities, with active
batches pinned. A 4,096-entry process lifetime identity ledger makes an evicted
Create fail closed rather than create a second batch. At that limit, restart the
worker. A cryptographically random engine instance ID changes on restart; old
handles fail NotFound. There is no cross-process persistence or recovery.

Other defaults are 16 batches, 128 total environments, 32 environments per batch,
32 concurrent admitted RPCs, 20,000 events per tape, 100,000 book levels per tape,
512 levels per event, 8 MiB protobuf messages, 4 MiB detail JSON per report, and
128 MiB retained serialized episode budget (conservatively charged per environment
despite tape sharing). Each episode permits 10,000 steps and 20,000 combined
history entries. History overflow invalidates the batch. Request concurrency
overflow fails ResourceExhausted immediately. Idle batches expire after 15 minutes;
the sweeper never reclaims a locked batch. CLI flags expose the principal process
admission limits; embedding callers can configure all Limits.

Terminal transitions carry compact typed state; GetReports returns typed fills,
amounts, and authoritative score validity plus inspectable complete detail JSON.
GetReports requires terminal environments and preserves request handle order.
Oversized report requests fail ResourceExhausted; callers can request fewer
environments. A final report may also exceed its individual report cap.

## Verification evidence

`go test -race ./internal/replayrpc ./cmd/liquiditylabd` covers concurrent duplicate
steps, create/reset/close retries, mismatched duplicates, identity reuse after
cache eviction, cancellation while queued, simulated lost response over real gRPC
and safe retry, environment isolation, optional zero/default preservation,
noncanonical decimals and action zero rejection, duplicate handles, typed and JSON
terminal reports, engine restart identity, injected partial mutation/panic
containment, idle reclamation with an active-mutation exclusion, and endpoint/socket
permission restrictions.

Proposed further gates: process kill during Step and old-handle rejection after
restart; process signal shutdown with a busy batch; complete Python Gym/PPO
roundtrip; sustained admission-pressure and retained-memory measurements; cache,
report, event, and history-cap boundary fuzzing; and batched throughput profiling.
The RPC tests establish deterministic local behavior, not production readiness or
an exchange execution guarantee.

# Durable replay jobs

**Status:** durable store, paper-daemon worker, and authenticated API are implemented; the web UI is pending.

Replay jobs are a durable queue for analytical work. The store keeps them separate from account financial authority: enqueue, claim, finish, retry, and cancellation do not update account balances, fills, reservations, intents, account snapshots, or account revisions. A replay job sequence is a history cursor, not an account revision.

The persistence contract is implemented by [migration `0005_replay_jobs.sql`](../migrations/0005_replay_jobs.sql) and [the PostgreSQL replay store](../internal/store/postgres/replays.go). The worker and request validation are in [internal/replayjobs](../internal/replayjobs/service.go); the HTTP routes are in [internal/api/replays.go](../internal/api/replays.go).

## Identity and stored bytes

A job is identified by `(tenant_id, account_id, job_id)`. The primary key makes that identity unique. Re-enqueueing the same identity with byte-identical input returns the existing job and does not consume another pending slot. Reusing it with different bytes returns a conflict. The store hashes the exact supplied JSON bytes with SHA-256; it does not canonicalize JSON before hashing or comparing it.

Input must be a valid JSON object, between 1 byte and 64 KiB. A successful result must also be a valid JSON object, between 1 byte and 16 MiB. Both exact byte strings and their SHA-256 digests are stored. The database enforces these size and digest-shape bounds. The store does not validate a versioned replay request schema, verify a market archive, or decide which scenarios a job should run; that semantic validation belongs to the service that will enqueue work.

The versioned worker input is `liquiditylab.replay-job.input.v1`; it contains the verified archive content ID and a canonical, explicit historical-comparison spec. The worker loads and verifies that archive before comparison. The reference and alternatives are all simulated scenarios over the same archive. The report's `source_session_binding` remains null: an observed actual session is not currently accepted as a reference. A future actual-session comparison requires a prospective source-session binding persisted at capture and decision time; symbol or UTC-time coincidence is not a binding. The worker does not infer or manufacture that lineage.

## Admission limits

`EnqueueReplay` serializes admission for the PostgreSQL schema with a transaction-scoped advisory lock. It checks for an existing job identity before checking capacity, then atomically checks pending jobs and inserts a new queued row. Pending means `queued` or `running`.

The first accepted enqueue pins one per-account and global capacity pair in `replay_admission` for that schema. Later callers must use exactly the same pair; they cannot silently change the namespace limits. Supported values are 1–64 pending jobs per account and a global limit from the per-account limit through 1024. `ErrReplayBusy` means one of those pending bounds is full; `ErrReplayConfig` means the caller's capacity pair differs from the persisted configuration.

The paper daemon uses the defaults: at most 4 pending jobs per account and 16 across the schema. The HTTP handler also has a separate four-slot limiter for replay requests, apart from ordinary request lanes. The daemon opens a separate PostgreSQL pool with at most two connections for replay work and runs one worker by default. These bounds describe this process and its namespace configuration; they are not deployment-wide capacity or performance claims. The enqueue transaction takes a schema advisory lock so concurrent admissions observe the pending limits consistently.

## Job lifecycle and worker fencing

The persisted statuses are `queued`, `running`, `succeeded`, `failed`, and `cancelled`.

1. A new accepted job is `queued` with zero attempts.
2. `ClaimReplay` claims the oldest queued job, or a running job whose lease has expired, using `FOR UPDATE SKIP LOCKED`. A claim sets `running`, increments the attempt number, records the worker ID, and sets a lease using database time. Lease duration must be between 1 second and 1 minute; the daemon uses 1 minute.
3. `FinishReplay` accepts a result or one fixed error code only for the current worker ID and attempt while the lease is still unexpired. The attempt number fences off a prior attempt even if a process restarts with the same worker ID. A stale or cancelled worker cannot publish a result.
4. If a running worker's lease expires, a later claim can retry it. After the third attempt expires, the next claim marks the job `failed` with `worker_lost` instead of returning it for execution.
5. `CancelReplay` changes a queued or running job to `cancelled` in a transaction and clears its lease. Terminal jobs are immutable; repeated cancellation returns the existing terminal row. A running computation may continue locally, but its finish is fenced and cannot publish after cancellation.

The worker gives each calculation a cooperative 20-second timeout by default (the service accepts configuration up to 30 seconds), shorter than its 60-second database lease. It periodically checks that the same attempt remains running and cancels local work after remote cancellation or reclamation. Database fencing is still authoritative if cancellation arrives between checks. On daemon shutdown, the worker is cancelled and joined; if shutdown exceeds five seconds, the pool remains open until the computation exits and the unfinished job remains recoverable after its lease expires.

Successful terminal result retries are idempotent only when the exact result bytes and error code match the committed result. The result SHA-256 is calculated over those original bytes. The detail endpoint embeds `input` and `result` as JSON values, which can be reserialized by JSON encoding. Use the `/result` endpoint for the original bytes. A different terminal result conflicts. Fixed failure codes accepted by the store are `input_invalid`, `archive_unavailable`, `execution_failed`, `deadline_exceeded`, and `worker_lost`; failure rows contain no result body. The API exposes these fixed codes without filesystem, SQL, or worker error strings.

## Reading jobs

The authenticated routes are scoped to an exact tenant/account pair authorized by the bearer credential:

- `POST /v1/accounts/{tenant}/{account}/replays` enqueues a job. The request contains `id`, `archive_id`, and `spec`. It returns `202 Accepted`; an exact retry returns `200 OK` with `duplicate: true`.
- `GET /v1/accounts/{tenant}/{account}/replays?before_sequence=0&limit=20` lists at most 50 metadata rows. `sequence` is the exclusive history cursor.
- `GET /v1/accounts/{tenant}/{account}/replays/{id}` returns one job's metadata, canonical input, and JSON result when available.
- `GET /v1/accounts/{tenant}/{account}/replays/{id}/result` downloads the original successful report bytes, including its final newline, with `X-Content-SHA256` and an attachment filename. The server verifies the exact stored digest before sending. Queued/running jobs return `409 result_not_ready`; failed/cancelled jobs return `409 result_unavailable`.
- `POST /v1/accounts/{tenant}/{account}/replays/{id}/cancel` accepts `{}` and returns the terminal or updated job metadata.

The API accepts a JSON spec shaped like [the retained short comparison example](../examples/replay/retained-short-compare.json). This Python example reads that file and submits it as the `spec` value; replace the account and archive placeholders with the daemon's configured account and a content ID from a verified import:

```python
import json
import os
import urllib.request

with open("examples/replay/retained-short-compare.json", encoding="utf-8") as f:
    spec = json.load(f)

body = json.dumps({
    "id": "short-compare-001",
    "archive_id": "<64-character-archive-content-id>",
    "spec": spec,
}).encode()
request = urllib.request.Request(
    "http://127.0.0.1:8080/v1/accounts/<tenant>/<account>/replays",
    data=body,
    headers={
        "Authorization": "Bearer " + os.environ["LIQUIDITYLAB_API_TOKEN"],
        "Content-Type": "application/json",
    },
    method="POST",
)
with urllib.request.urlopen(request) as response:
    print(response.status, response.read().decode())
```

The handler rejects unknown fields and query parameters, uses exact account scope checks, and limits replay requests to a separate four concurrent HTTP slots. Its default body limit is 16 KiB (the service/store input bound is 64 KiB). List responses omit input/result bodies but include hashes, status, attempts, fixed error code, and timestamps. Individual GET includes them as JSON values. Worker ID and lease deadline are never exposed.

At the HTTP boundary, invalid requests map to `400 invalid_replay`; exhausted pending capacity maps to `503 busy` with `Retry-After: 1`; unknown or out-of-scope accounts/jobs map to not found. Other database, conflict, timeout, and ownership errors use the existing sanitized backend mapping. Failure details are fixed codes stored on the job, not raw internal errors.

## Daemon opt-in and remaining work

The paper daemon enables this service only when started with `--replay-archive /absolute/operator/selected/archive/root`; the option must point to an existing ordinary directory, not a symlink. Without it, replay resources and routes are disabled. Requests select archives by content ID, and the worker re-verifies the archive and coverage while executing. The web UI has not yet been wired to these routes.

The replay worker and API do not create account commands or change account financial revisions. Replay output is a historical estimate under explicit assumed rules and fees; it does not establish venue authenticity, actual-session lineage, realized savings, or policy quality.

## Verification

Local race checks exercise the store, service, HTTP API and daemon integration
against development PostgreSQL. A fresh worker pool imports the retained real
recording, publishes the exact comparison report, preserves the complete
financial account and exposes metadata-only history. The daemon check exercises
authenticated enqueue/retry/poll/export, verifies downloaded byte hashes, denies
another account and joins its worker before a fresh pool rereads the result.
Separate checks cover cancellation, expired attempts, uncertain finish retries,
worker limits and saturation of all four replay HTTP slots while financial
controls remain available.

An admission regression configures Repeatable Read as the connection default,
observes all contenders waiting on the advisory lock, then verifies two accepts
and four busy rejections. Replacing the fix with the prior transaction mode in
an isolated Go overlay instead accepts all six despite a limit of two.

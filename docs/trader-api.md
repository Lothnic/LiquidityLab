# Trader HTTP component

`internal/api` implements an authenticated, bounded `http.Handler` over existing
paper account authorities. It does not start a listener or construct an actor,
feed, database, or exchange transport. It does not implement a remote identity
provider, browser session, or live execution eligibility.

## Construction

```go
type Authority interface {
    View(context.Context) (runtime.View, error)
    Do(context.Context, runtime.Command) (postgres.Result, error)
}
type JournalReader interface {
    Journal(context.Context, postgres.Key, int64, int) ([]postgres.JournalEntry, error)
}
type Credential struct {
    Token string
    Scopes []postgres.Key
}
type Config struct {
    Accounts map[postgres.Key]Authority
    Credentials []Credential
    Journal JournalReader
    MaxBodyBytes int64
    RequestTimeout time.Duration
    MaxConcurrent int
}
func New(Config) (*Handler, error)
```

`*runtime.Actor` implements `Authority`; `*postgres.Store` implements
`JournalReader`. Supply an explicit account registry and exact tenant/account
scope pairs for each static credential. `New` copies the registry and grants,
rejects unknown scopes and duplicate tokens, and retains only SHA-256 token
hashes. Each authentication attempt compares all configured hashes with
constant-time comparison. Use cryptographically random tokens with at least 32
non-whitespace characters; these are operator configured credentials, never
exchange credentials. The constructor checks length, not entropy.

Defaults are a 16 KiB request body, five-second dependency deadline, and 64
concurrent authenticated requests. Configuration caps are 1 MiB, 30 seconds,
and 1,024 concurrent requests. The registry and credential list each permit at
most 256 entries. Excess concurrent requests receive 503 immediately; no request
queue or per-tick goroutines are created. Authority and journal implementations
must honor the supplied request context. The enclosing `http.Server` must set
header/read/write/idle timeouts and header-size limits; listener binding, TLS,
static-asset routing, process lifecycle and token rotation belong to the daemon.
Remote access requires authenticated encrypted transport. Serve this handler
under the API paths without a redirect that forwards bearer headers elsewhere.

## HTTP contract

Only `GET /healthz` is unauthenticated. It returns `{"status":"ok"}` and makes
no database/readiness claim. Every `/v1` request requires
`Authorization: Bearer <token>`. There are no cookie or origin-based grants.
Responses use JSON, `Cache-Control: no-store` and `X-Content-Type-Options:
nosniff`. Unsupported methods return 405 with `Allow`; unauthorized account
paths and absent accounts both return 404. Missing/invalid credentials return
401. Unknown routes require authentication before returning 404.

- `GET /v1/accounts`: deterministically ordered summaries of granted accounts.
- `GET /v1/accounts/{tenant}/{account}`: product account projection.
- `POST /v1/accounts/{tenant}/{account}/commands`: a single trader command.
- `POST /v1/accounts/{tenant}/{account}/preview`: pure first-proposal preview.
- `GET /v1/accounts/{tenant}/{account}/journal?after_revision=0&limit=100`:
  ascending durable journal metadata, strictly after the cursor. Limits are
  1–200; responses include `next_after_revision` and `limit`. An empty page
  retains the supplied cursor. Continue until the page is shorter than its
  limit. Each page uses the authorized account key, with no client-supplied
  tenant filter or detail payload.

Tenant and account path segments contain 1–128 ASCII letters, digits, `_`, `-`
or `.` and cannot equal `.` or `..`. Only journal accepts query parameters;
unknown, repeated, malformed and out-of-range journal parameters are rejected.

All POSTs require `Content-Type: application/json`. Unknown JSON fields,
trailing JSON, invalid exact decimals, missing required fields and over-limit
bodies fail before invoking `Do`. Money is always a JSON string: `"0.05"`,
never `0.05`. Financial arithmetic uses the domain decimal parser and existing
execution helpers; floats do not enter money.

Example creation body (the account must have sufficient synthetic balances):

```json
{
  "id": "create-79e82a",
  "expected_revision": 12,
  "kind": "create",
  "plan_id": "plan-79e82a",
  "policy": "scheduled",
  "plan": {
    "symbol": "BTC/USD",
    "side": "buy",
    "quantity": "0.05",
    "limit_price": "100",
    "deadline_ns": 900000000000,
    "max_child_quantity": "0.01",
    "quote_budget": "5.01",
    "base_balance": "0"
  }
}
```

Creation starts paused. The plan deadline is a positive relative duration up to
24 hours; the authority chooses and persists its UTC deadline. `policy` may be
`scheduled` or `liquidity-aware`, or omitted to use the actor's configured
baseline. No arbitrary policy bundle or semantic action can be sent here.
Follow with `{"id":"start-79e82a","expected_revision":13,"kind":"start"}`.
Other permitted kinds are `pause`, `resume`, and `cancel`; they do not accept
`plan`, `plan_id`, or `policy`. Internal `deadline`, `action`, `decision`, and
`dispatch` commands are rejected. IDs are 1–128 characters using the path
character set; reserved runtime receipt prefixes are rejected.

Successful commands return the durable `{"revision":14,"duplicate":false}`
receipt. The actor validates expected revisions, persists idempotency identity,
and handles cancellation races; the HTTP layer does not duplicate that state
machine. Retry the exact ID, expected revision, and payload after a timeout.
Changed payload under an existing ID returns 409 `idempotency_conflict`; stale
revision returns 409 `stale_revision`. HTTP success means a committed command,
not that a child order is known final. A timeout or `outcome_unknown` response
must not be interpreted as rejection or used to invent a replacement ID.

## Durable and authority revisions

Account summaries, account projections and previews expose both `revision` and
`authority_revision`. The former is the durable journal cursor. The latter is
the durable revision of the last financial/control change. An unchanged-child
paper trade receipt or already-applied observation cursor can advance the
journal while preserving authority. Queue depletion, fills, reservations,
controls, effectful decisions, readiness and actor bootstrap conservatively
advance financial/control authority. A HOLD decision that changes only the
explicitly allowed feature/cadence fields preserves it; its decision counter
and durable journal still advance.

Legacy clients supplying only `expected_revision` retain exact revision checks.
A client may additionally send `expected_authority_revision`: it must match the
current authority, and its `expected_revision` must lie between that authority
revision and the current durable revision. This permits a preview-bound command
after receipt-only bookkeeping without accepting a command predating a financial
change. The actor checks the token under its serialized authority and the store
still compares the actual current durable revision atomically. New market books
are revalidated by compilation and dispatch; a preview price is an estimate.

Retain both original revisions, command ID and payload for an unknown-outcome
retry. Changing a token under an existing ID is a payload conflict. Actor restart
advances authority, including for legacy snapshots without a marker. The token
does not grant exchange access, bypass readiness or permit a second account owner.

## Display and preview semantics

Account DTOs include explicit profile, balances, unreflected reserved totals,
market validity/snapshot identity, instrument rules, fee assumptions, plan
status/deadline/policy, exact filled/remaining/average price and fee totals,
child state/remaining quantity/known final/cancel requested, and simulated fills.
The plan also exposes its frozen `arrival_mid`, `benchmark_valid`, and nullable
`executed_cost_quote`. Executed cost combines filled slippage against arrival
with quote-valued fees using `Ledger.MarkedCost(arrival, arrival)`: residual
quantity contributes zero. It is not total cost, P&L, or policy reward. Legacy
snapshots without a positive arrival benchmark show an unavailable cost.
A paused parent can still have a child requesting cancellation. A recovery flag
is independent of cancellation: unknown effects remain unresolved. Paper
profile fills and fees are simulated, never exchange-confirmed executions.

Only the latest 2,048 fills appear in an account response; `fill_count` and
`fills_truncated` expose truncation. Totals use the complete retained parent
fill history. Durable raw snapshots, ownership owner/epoch/lease, decision
features, pending fact payloads, intent authorization records and journal detail
are absent. Account reads and previews reject mismatched authority account keys.

Preview accepts `{"plan":{...},"policy":"scheduled"}`. It calls `View` once,
validates plan/rules/configuration and available plan balances (free minus held,
unreflected local reservations, clamped at zero), then uses shared
`BaselineAction`, `Compile`, `ReservationFor`, and `FeeFor` helpers on a fresh
ledger. It never calls `Do`, mutates the book, creates a reservation, or promises
completion. It reports the account revision, observation time, current market
snapshot, authority revision, spread/visible depth, first candidate intent and its fee/reservation
estimates. A current invalid market or recovery state produces no intent and
an explicit reason. An existing active, paused, or cancel-requested parent or
child produces `creation_unavailable` with `ready:false` and no new intent.
Reflected reservations are already in locked balances and are not subtracted
from free a second time. The timestamp is API observation time, not a feed freshness
proof. Preview does not simulate a complete trajectory or establish live
eligibility; readiness can change before command acceptance or dispatch.

Unexpected dependency errors become a generic 503 `unavailable`; raw errors,
SQL, snapshots and credentials are never returned or logged by this package.
Known insufficient-funds/expired/recovery conditions return safe 409 codes.
Dependency context cancellation maps to 408; deadline/unknown accepted outcomes
map to 504. Clients should fetch fresh state after conflicts. There is no SSE
endpoint in this component: clients poll scoped snapshots and journal pages.

## Checks

`go test -race ./internal/api` uses in-process fake authorities and journals,
with no live network, exchange orders or PostgreSQL dependency. It covers
credential/account isolation, command whitelisting, exact money and pure
preview, receipts/error mapping, DTO redaction, bounded admission/body/query
handling and request-context cancellation. Full daemon/runtime integration,
remote authentication deployment, load capacity and design-user acceptance
remain separate work.

# PostgreSQL account transition foundation

This package persists the account authority's transitions. It does not implement
an actor, an exchange adapter, private stream reconciliation, or order transport.
Database ownership epochs reject stale application writes; they cannot fence an
exchange connection or retract a request already sent.

## API

`Open(ctx, dsn, schema) (*Store, error)` creates or opens an explicitly named schema
and configures a pool with at most 16 connections. Schema names must match
`[a-z][a-z0-9_]{0,62}`. Call `Migrate(ctx)` before account access, and `Close()` when
the runtime has drained. Open and Migrate never drop or reset an existing schema.
The runner embeds ordered SQL from `migrations/`, obtains a schema-scoped advisory
transaction lock, verifies migration names/checksums and supported versions, and
applies missing migrations transactionally. An edited or unknown version fails
with `ErrSchema`.

- `CreateAccount(ctx, Key, []Balance, snapshot) (Account, error)` creates a tenant
  scoped account with exact initial balances and a versioned reducer snapshot.
  Existing identities return `ErrConflict`; creation is not an upsert.
- `GetAccount(ctx, Key) (Account, error)` reads one repeatable account view:
  ownership/lease, revision, readiness, market identity, reducer snapshot, balances,
  active parent, pending intents, and held reservations. Final historical rows
  remain durable but are outside this recovery view.
- `LookupCommand(ctx, Key, commandID)` returns the original command result and
  its immutable reducer snapshot for retry comparison. It does not restore older
  state. `GetIntent` and `GetExecution` read durable identities including finalized
  historical records, without changing account state.
- `ClaimOwnership(ctx, Key, ownerID, lease, RecoveryProof) (Ownership, error)`
  assigns a new epoch, increments account revision, clears account/market readiness,
  and obsoletes unsent intents. `ownerID` must identify a unique process incarnation,
  not a reusable worker slot. Existing ownership requires explicit previous-sender
  isolation and reconciliation evidence, even if its lease expired. A repeated
  claim by the same still-current incarnation returns its current token.
- `RenewOwnership(ctx, Key, Ownership, lease) error` extends a current, unexpired
  lease without changing financial revision. Leases are positive and at most five
  minutes. Database time determines lease validity after acquiring the row lock.
- `Apply(ctx, Change) (Result, error)` atomically records a command, checks ownership
  and expected account revision, and changes the projection, plan, reservations,
  outbox, execution identity, and journal together. Command results have a stable
  revision and `Duplicate` flag. An identical retry returns its saved result;
  changed content under that identity returns `ErrConflict`. SQL commit transport
  errors can have ambiguous outcomes: retry the identical command, never invent a
  replacement command or assume the transition rolled back.
- `AuthorizeDispatch(ctx, Authorization) (Dispatch, error)` rechecks current owner
  and lease, account revision/readiness, plan revision/status/deadline, instrument
  metadata revision, current market snapshot/validity, intent expiry, and held
  funds. It marks the intent `attempted`, records authorization revision/snapshot
  and attempt identity, increments the account revision, and commits before
  returning. A second authorization of an attempted/unknown/acknowledged intent
  fails; it never grants permission to send again.

Account keys and durable IDs are bounded strings. Each account supports at most 32
currencies and 64 pending intents. Change admits at most 64 new intents, 128 new
reservations, 64 resolutions, and 32 reservation adjustments. JSON values are
bounded to 1 MiB each, and the complete change to 2 MiB. A plan begins at revision
one and updates by exactly one. The database's partial unique index permits one
parent in `active`, `paused`, or `cancel_requested` state per account; outstanding
attempted/unknown/acknowledged intents also block creation of a replacement parent.

## Amounts, holds, and reducer responsibilities

Numeric columns reject nonfinite/negative balances and amounts outside the core's
512 coefficient digit/144 scale envelope. No money crosses a float. Stored numeric
text is normalized and decoded through `domain.ParseInternalDecimal`. Each balance
must satisfy `total = free + locked`. Local unreflected held reservations must fit
venue-free funds. Reflected reservations must fit locked funds and are not
subtracted again from free funds.

The caller is a trusted, serialized reducer. It supplies complete replacement
balances for the currencies it changes, validated execution details, versioned
snapshot state, and conservative fee-inclusive reservation amounts. The store
does not independently compile raw order payloads, compute fills/fees, prove asset
conservation from arbitrary JSON, validate the plan's trading limit, or establish
that a private exchange snapshot/stream handoff was consistent. Those are runtime,
domain, adapter, and reconciliation responsibilities. `Ready=true` and
`RecoveryProof` are operational assertions supplied after those checks, not
evidence discovered by the database. Unexpected external activity must clear
readiness in a durable transition.

Every queued intent requires a positive unreflected reservation. A unique execution
identity is scoped to tenant/account/venue and commits its resulting projection
once. A duplicate execution returns its original result without applying balances
again; changed execution content fails. A newly recorded partial fill may reduce
its own hold with `Change.ReservationUpdates` (positive, decrease-only remaining
amount). A known-final `Resolution` releases holds; `attempted`, `unknown`, and
`acknowledged` states retain them. Updating reflected status requires matching
locked balances. Terminal parent status requires outstanding attempted outcomes
to have been resolved first.

Plan revisions, pause/cancel requests, account invalidation, and metadata changes
transactionally obsolete queued intents and release only their unsent holds.
`Change.ObsoleteQueued` can invalidate specifically identified queued children
without fabricating a trader plan revision; attempted children cannot use this
path. Ownership is checked again at the final account projection update, so a
lease that expires during the transaction prevents that projection from applying.
Deadline/intent expiry at authorization also obsoletes the queued plan intents.
PostgreSQL stores absolute deadlines/expiries with microsecond precision; expiry
comparison and persistence conservatively truncate submicrosecond fractions.

## Physical handoff and recovery boundary

The runtime must keep account serialization across authorization and the actual
outbound transport handoff, including pause/cancel processing. The store alone
cannot stop a caller from saving a returned Dispatch, processing a pause later,
then incorrectly sending that stale value. Network and inference calls must happen
outside database transactions. A request handed to transport before pause is
potentially in flight; retain its reservation and reconcile it. A crash or lost
reply after attempted state commits is likewise unknown, not permission to resend.

An expired database lease is insufficient to activate a second sender. Isolate the
previous process/transport, reconcile its outcomes, then claim a new epoch. Rebuild
state from a compatible snapshot and reconcile private balances/orders/executions
before marking ready. The database component cannot verify process isolation or
provide exchange fencing; automated live failover is not implemented here.

## Durable observation inbox

`RecordObservation(ctx, Key, Observation)` appends a stable source fact without
requiring execution ownership. A fill received while an owner loses its lease
still needs durable storage. This API is for authenticated/trusted ingress; it
grants no order-submission authority. Stable IDs deduplicate canonical JSON and
kind; changing the fact behind an existing ID is a conflict. It returns the
account's ordered observation sequence. `GetObservation` reads the original fact
even after processing; `PendingObservations` reads an ordered bounded prefix.

The pending inbox is limited to 4,096 facts and 16MiB of original payload bytes
per account, with at most 1MiB per fact. Full/failed ingress must retain/recover
source facts and invalidate readiness; it must never drop a financial fact and
continue sending. Unprocessed facts block new intent admission and dispatch.
The owning reducer commits its projection with `Change.ConsumeObservationThrough`;
the cursor and pending-byte budget change in that same transaction. Only the owner
may advance the cursor, and a snapshot is required. A repeated execution still
needs its source cursor consumed without applying its economic effect again.

The database persists complete paper transport outcomes, including all fills,
before a runtime applies the individual executions. If it stops partway through,
the retained source fact and unique execution IDs allow processing to resume.
Ingress durability does not replace venue-history reconciliation for a failure
before a fact reaches this database. Inbox history needs the separate retention,
backup, restore, and operational work in the migration plan.

## Executed verification and remaining gates

With `LIQUIDITYLAB_TEST_POSTGRES_DSN` pointing to the isolated local PostgreSQL
18.6 instance, `go test -race -v ./internal/store/postgres` passes against fresh
randomly named schemas. Each test drops only its own schema. Coverage includes
migration repeat/checksum drift, a 512 coefficient digit fractional value roundtrip,
database numeric rejection, rollback across plan/reservation/outbox creation,
command identity and concurrent duplicate application, tenant scope and one active
parent, attempted/unknown/final reservation handling, unique executions, ownership
transfer assertions and stale writer rejection, a concurrent pause/dispatch race,
queued expiry/metadata invalidation, and reflected venue holds. Without the DSN,
these integration tests explicitly skip; a skipped run is not PostgreSQL evidence.

Required integration gates remain actor restart/rehydration at every intent,
acknowledgement, and cancellation boundary; serialized physical handoff under
pause/deadline races; private snapshot/stream readiness; simultaneous fills during
cancel; process isolation and venue recovery; bounded queue overload; durable paper
workflow; and database backup/restore. This package's passing database checks do
not establish those runtime or live trading guarantees.

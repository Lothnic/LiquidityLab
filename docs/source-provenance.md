# Sealed source provenance for paper sessions

`liquiditylab paper --archive /absolute/archive --dataset-id CONTENT_ID ...`
loads and reverifies an immutable archive before creating a recorded-data paper
account. This source selection is exclusive with `--events`. The ordinary paper
input bounds still apply. The CLI checks the reconstructed normalized bytes and
their SHA-256 against the sealed manifest before sending events to the actor.

The account pins the full archive ID, raw/source-manifest/normalized hashes and
byte counts, normalization version, native capture session, instrument and
coverage. Each supplied cursor carries both normalized and raw receipt indexes,
normalized event time and native capture elapsed time. Several normalized events
may share one raw receipt or one elapsed time. These clocks are separate from the
account's execution clock.

The first snapshot must match the manifest. Subsequent new normalized indexes
must be contiguous and raw indexes/time must not decrease. The runtime publisher
is trusted to map each event to the verified archive; runtime checks validate the
descriptor, ordering and exact latest payload, rather than rereading archive
files per event. The parent freezes its starting descriptor/cursor on first
start. Durable baseline decisions retain their source cursor; policy requests
and resolutions retain requested/applied cursors with their existing audit.
Ordinary book updates remain in memory until a durable transition captures them;
the cursor is not an assertion that every book update has its own journal row.

Revision-pinned session reports include optional `source_provenance` with the
curated sealed descriptor, frozen start cursor, current durable cursor, latest
decision cursor and matching policy request/applied cursor pair. They validate
the archive ID against its raw/source-manifest hashes, cursor ordering and
terminal-reference consistency before publishing the projection. No raw book,
receipt, source payload digest or filesystem path is exposed. Unbound legacy
reports omit this field.

Missing or inconsistent bound provenance invalidates readiness. An older accepted
trade retry discovers its frozen durable economic outcome, without rewinding the
book or rematching a child. This lookup proves trade identity, not an independent
mapping of an older cursor to archive bytes. An uncertain reload that loses a
newer in-memory source book preserves durable financial obligations and
quarantines the mismatched book. It does not merge simulated consumption debt
from the lost in-memory snapshot.

Legacy `--events`, synthetic, manual and unsealed live public inputs remain
unbound. No archive association is inferred from symbol, UTC time, filename or a
short generated snapshot ID. Automatic source reconstruction/resume, sealing
live capture chunks and comparing an actual session against alternative
executions remain unfinished. The historical comparison currently compares
simulations and explicitly reports `source_session_binding: null`.

## Verification

Focused normal/race checks cover source guards, immutable pins, exact latest
payload identity, frozen older-trade identity and reload quarantine. A separate
PostgreSQL-backed paper CLI check imports the retained real recording, delivers
all 45 events and reads the revision-pinned session back from the database to
verify the immutable source, parent start cursor, latest decision and final
receipt cursor. This is local paper evidence; it does not establish venue
authenticity, execution quality or source recovery capacity.

# Public market archive operator runbook

`liquiditylab dataset` imports a sealed public Kraken recording into a
content-addressed, read-only-by-convention archive and verifies an existing
archive entry by ID. Import and inspect recheck bytes and reconstruct normalized
events. The archive is an integrity-checked copy of supplied files; hashes do
not prove who captured them or authenticate the venue. `continuous` describes
only the recording's checked receipt continuity and terminal book validity. It
does not establish replay eligibility, policy suitability, or operational
capacity. Archive entries are not yet linked to paper sessions or replay jobs.

The [offline historical comparison](historical-replay.md) reads these entries
and separately checks interval validity and deadline/settlement coverage before
running simulated baseline scenarios.

## Build and inspect command help

From the repository root, build the CLI when a binary is needed:

```sh
mkdir -p build
go build -o build/liquiditylab ./cmd/liquiditylab
build/liquiditylab dataset help
```

The command forms are:

```text
liquiditylab dataset import --raw <capture.jsonl> --manifest <capture.manifest.json> --archive <root>
liquiditylab dataset inspect --archive <root> --id <lowercase-64-hex-ID>
```

Use an archive root selected and controlled by the operator. It may be a new
nested path; the importer creates it. Treat the root and its ancestor directory
permissions as trusted. IDs are strict lowercase 64-character hexadecimal
content IDs, and inspect accepts an ID rather than an arbitrary artifact path.

## Import the retained public capture

The repository retains a 28 October 2026 public Kraken BTC/USD depth-10
recording in [`examples/recordings/kraken-btcusd-20261028/`](../examples/recordings/kraken-btcusd-20261028/).
Choose an archive root outside that source directory, then import its raw
receipts and original capture manifest:

```sh
build/liquiditylab dataset import \
  --raw examples/recordings/kraken-btcusd-20261028/public-capture.jsonl \
  --manifest examples/recordings/kraken-btcusd-20261028/public-capture.manifest.json \
  --archive /absolute/path/to/liquiditylab-market-archive
```

Import prints one JSON archive manifest on success. Record the returned `id`;
do not derive or edit it manually. To reverify the entry, paste that exact ID:

```sh
build/liquiditylab dataset inspect \
  --archive /absolute/path/to/liquiditylab-market-archive \
  --id <id-from-import-output>
```

Inspect prints the canonical summary JSON. A successful duplicate import of the
same exact source bytes returns the same summary after verifying the existing
entry and syncing the archive-root directory. Changed source-manifest bytes,
even harmless formatting changes, produce a different content ID.

## What import validates and stores

The importer reads each input once into bounded memory, checks it, normalizes
from those same bytes, and stores four files in the content-ID directory:
`raw.jsonl`, `source-manifest.json`, `events.jsonl`, and
`archive-manifest.json`. Default bounds are 64 MiB for raw input and normalized
output, 64 KiB for each manifest, 8 MiB per receipt line, and 100,000 normalized
events. `--max-raw-bytes` can lower the raw input limit, but cannot exceed
64 MiB.

Validation requires a complete sealed capture manifest and final `capture_end`,
a verified snapshot, newline-terminated JSONL receipts, contiguous receipt
indices, nondecreasing elapsed receipt time, consistent session/schema/depth/
symbol identity, and counters that match reconstructed frames, book messages,
snapshots, and gaps. The Kraken normalizer rechecks the raw book/checksum and
reconstructs event identity and ordering. Capture statuses accepted for import
are `duration_complete`, `frame_limit_reached`, and `recorded_with_gaps`; an
error or incomplete status fails. Gaps and checksum failures represented in a
sealed capture are preserved in the summary rather than erased.

The content ID commits to the exact raw-file SHA-256, exact source-manifest
SHA-256, archive schema, and normalizer version. The archive manifest also
contains byte counts and SHA-256 fingerprints for all three stored payloads.
Declared capture start and finish timestamps are parsed as RFC3339Nano and
stored in UTC. They remain declared source metadata. Receipt elapsed time
determines event causality. Coverage ends at the `capture_end` receipt and starts
at the first verified snapshot; the recorded quiet tail after the last market
event contributes coverage time but does not invent a normalized event.

`continuous=true` requires no reconstructed gaps, a valid terminal book, and a
capture not marked `recorded_with_gaps`. `continuous=false` or a failed import
must not be changed by hand to make a dataset appear suitable. Even a continuous
archive is not a replay eligibility decision.

## Filesystem behavior and retry procedure

The verified filesystem implementation currently requires Linux. Input files
are opened with no-follow semantics for the final path component, nonblocking
open, and regular-file checks; archive entries are opened relative to the
archive directory with no-follow checks for the directory and artifact files.
Intermediate path components still follow normal OS path resolution, so use
trusted paths and a trusted archive root.

The importer stages an entry in a private temporary directory below the archive
root. It creates files exclusively with mode `0600`, syncs each file, syncs the
staging directory, then atomically promotes it with no-replace rename. It never
overwrites an existing content-ID directory, including an empty or invalid
directory. It syncs the archive root after promotion. Root initialization
creates missing directories with restrictive mode and syncs the complete
absolute ancestor chain.

On Ctrl-C, validation and staging observe cancellation and an unpromoted staging
directory is removed. A failed command returns nonzero and does not print a
success manifest. If a failure happens after promotion while syncing the root,
the entry may be present without the import having confirmed directory
durability. Retry the same import with the same input bytes and root: the
duplicate path reopens and verifies every stored artifact, compares the
reconstructed manifest, then retries the root sync. Do not delete or replace a
partially present content-ID directory as a retry shortcut. A tampered or
conflicting entry fails closed and requires operator investigation.

Inspect rehashes stored raw, source-manifest, and normalized bytes; reruns source
validation and normalization; checks every reconstructed summary field and
canonical archive-manifest encoding; and returns the summary only if the full
comparison succeeds. Wrong-case, malformed, path-like, or missing IDs fail.
Edits, truncation, swapped files, symlinks, extra JSON values, and noncanonical
stored manifests are errors, not repaired imports. Preserve the original raw
capture and manifest separately if they are needed for incident review.

## Limits

This tool verifies the supplied public capture against its source manifest and
the pinned normalizer. It does not establish venue authenticity, trusted wall
clock accuracy, replay episode eligibility, trading-policy quality, private
market access, paper-session provenance, or supported performance. An archive
entry is not currently referenced by a session, a replay job, or a policy
evaluation record. Do not describe archive continuity as replay validation or
as evidence of execution capacity.

## Executed local checkpoint — 29 October 2026

The ordinary CLI imported the retained capture into a new nested archive root,
repeated the import, and inspected the resulting entry. All three decoded
summaries matched. Independent file checks verified the raw and original
source-manifest bytes, all three payload fingerprints and lengths, and the
45 normalized JSONL events (44 book messages and one trade).

The resulting content ID is
`2a409892cbf06e56141fd23bc5bbf33bba4920400f753740e0b0020e2cb7e80a`.
The normalized payload is 14,069 bytes with SHA-256
`bc5e8f923c8cf2c777930c2c4ded27d93bc81387695cc5b461b081ba88b74c50`.
This capture is continuous and has 1,532,152,157 ns of coverage after its first
verified snapshot; total capture duration includes subscription startup.
This short recording does not establish a complete trading episode.

The operator invocation used a review build before the archive commit. Its
successful import is local functional evidence, not a clean-source performance,
power-loss, or backup/restore result.

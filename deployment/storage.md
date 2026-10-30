# Paper beta storage admission

The packaged Kraken profile uses an existing dedicated `/recordings` volume.
The root must be an absolute, clean path to a real directory. Metrics output must
remain outside this root, including through directory aliases.
`PAPER_MAX_RECORDING_BYTES` defaults to 1073741824 (1 GiB) and
`PAPER_MAX_CAPTURE_BYTES` defaults to 67108864 (64 MiB). Synthetic input creates
no recording. Legacy `--capture-output` remains available without an aggregate
storage guarantee; it is exclusive with `--recording-root`.

Before database/account initialization, Linux aggregate admission takes a
nonblocking exclusive lock on the recording directory for the daemon lifetime.
Only one cooperating daemon may record into that root. It counts every regular
file, including unknown, orphan and temporary files, and reserves the next raw
capture cap plus twice the 128 KiB manifest bound. The temporary and published
manifest can briefly have two names, so accounting is conservative. Traversal
counts at most 10,000 entries including the root and reserves entries for the new
directory and files. Symlinks, special files, inspection errors, exhausted budgets
and lock contention cause explicit admission refusal before account mutation.
Non-Linux aggregate admission is unsupported and refuses explicitly.

No existing files are overwritten, expired or deleted. A failed startup after
admission may leave an empty new directory, which future admissions also count.
Keep the root and its ancestors stable, and do not concurrently alter its
contents outside the cooperating recorder. Directory locks do not prevent an
operator or unrelated process from writing, replacing directories or filling the
filesystem. This is admission accounting, not a disk quota or a free-space check.

Raw write exhaustion retains existing capture failure/incomplete-manifest
semantics and invalidates the daemon's public source readiness. Account inspection
and cancellation remain available after source failure. A new daemon may be
refused when retained captures consume the budget; increasing admission does not
reclaim space. Preserve pinned datasets, bundles, recording references and all
account journals/fills/reservations/commands/provenance. Automated deletion needs
a verified reference catalog and remains unimplemented.

Docker logs rotate separately at 10 MB times three files per container. Neither
that byte cap nor recording admission guarantees the proposed retention periods.
Database and bundle growth remain unbounded; named volumes may share host disk.
Provide appropriate filesystem capacity/quotas and monitoring to preserve durable
account writes. Changes to Compose environment or logging require deliberate
container recreation; this implementation does not alter running services.

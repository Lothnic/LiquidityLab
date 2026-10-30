# Offline backup and isolated restore

`backup.py` uses Python 3's standard library, Docker Engine and Compose v2 on a
POSIX host (verified on Linux). Run it where the selected engine can read the
restore directory for file-backed secrets. It
copies the whole selected PostgreSQL application schema, the retained policy
bundle and public-data recordings. It never stops the source, resets an account,
starts an application or submits a trading command. A successful restore supplies
an isolated database and files for inspection; active-session recovery remains a
separate gate described in [paper-recovery.md](../docs/paper-recovery.md).

## Prepare a quiet source

Choose the engine context, existing Compose project and schema explicitly. Keep
the retained `paper` and `db` containers: the script uses their labels, image IDs,
configuration and mounted volume identities. `demo.sh stop` runs Compose `down`
and removes those containers, so use Compose `stop` for this workflow.

From the repository root, for example:

```sh
DOCKER_CONTEXT=default PAPER_MODE=resume docker compose \
  --project-name liquiditylab-rlbeta-20261030 \
  --file deployment/compose.yaml stop paper worker edge
```

Stop any `train` job separately and wait for it to exit. Leave `db` running and
healthy. The backup command refuses running or restarting paper/train containers,
including Compose training one-offs. It also refuses other running containers
with write access to the retained database, bundle or recording volumes. The
worker normally mounts the bundle read-only, but stopping it makes the
maintenance window explicit.

Keep exclusive operator control until backup finishes: do not restart a writer,
run migrations or write directly to PostgreSQL or the volumes. Initial and final
checks detect running writers and changed account state; they cannot prove that
an external client never wrote and then disconnected between checks. The dump
itself uses PostgreSQL's consistent logical snapshot. The quiet window keeps the
database, immutable bundle and recordings aligned.

## Create a backup

Use an existing protected parent directory outside the repository. Each command
requires a new destination; it refuses an existing directory rather than
overwriting an earlier or incomplete backup.

```sh
mkdir -m 700 "$HOME/liquiditylab-backups"
python3 deployment/backup.py backup \
  --context default \
  --project liquiditylab-rlbeta-20261030 \
  --schema paper_beta \
  --destination "$HOME/liquiditylab-backups/paper-20261031-001"
```

For an existing parent, omit `mkdir`. `DOCKER_CONTEXT`, `COMPOSE_PROJECT_NAME` and
`PAPER_SCHEMA` can supply their corresponding options. Explicit flags make an
operation easier to review. This workflow expects the packaged database identity
`liquiditylab`/`liquiditylab` and PostgreSQL 18.6. A different deployment needs a
reviewed adaptation.

A completed backup has these files:

| File | Contents |
| --- | --- |
| `schema.dump` | PostgreSQL custom-format dump of the entire selected schema, including every account and retained financial/history table |
| `bundle.tar` | Paths and ordinary directory/file metadata below the retained bundle volume |
| `recordings.tar` | Paths and ordinary directory/file metadata below the recordings volume |
| `metadata.json` | Source context/project/schema, exact local image IDs, volume identities, allowed paper configuration, account identities, table/sequence fingerprints and archive file inventory |
| `manifest.json` | SHA-256 of every backup payload; published only after collection and final source checks succeed |

Directories are created with mode `0700`, files with mode `0600`. No source
password, API token, application secrets mount or policy socket volume is copied.
The archive helper overrides the paper image's entrypoint with `python3`, mounts
one source volume read-only and has no network. Socket entries are skipped;
symbolic links, hard links represented as links, devices and other special
entries are not accepted as restore input. Source hard-linked regular files are
archived as independent regular files. Archive file ownership, permissions
and timestamps are retained for ordinary entries; the target volume root is
owned by runtime UID/GID `10001:10001` with mode `0755`.

The backup records row counts and SHA-256 fingerprints of sorted JSON rows for
every ordinary application table, including financial records and history. It
also fingerprints each sequence's `last_value` and `is_called` state. The same
inventory is checked before and after collection. Archive inventory records each
path's type, numeric ownership and permissions, plus each file's size and SHA-256.
Restore recomputes the database inventory and reads back both new file volumes
to compare their contents and metadata before reporting success.

The completion path flushes payload files and the pending manifest with `fsync`,
atomically renames the manifest, and flushes the backup directory. These steps
support durable completion on a filesystem and storage stack that honor `fsync`;
they do not establish storage-failure tolerance or replace a verified separate
backup copy.

The database dump and retained recordings still contain sensitive operational
data. Keep them out of Git and public evidence directories. SHA-256 detects
payload corruption relative to the manifest; the manifest is not a signature.
Use a trusted owner-controlled backup and preserve permissions when transporting
it. The backup contains no container images. Exact local image IDs are required
for restore, so a new engine needs the corresponding images through a separate,
verified release/image-transfer procedure. Portable files alone do not establish
fresh-host deployability.

An interrupted or failed command leaves partial files with no completion
manifest. Preserve them for inspection and choose a new destination for another
attempt. Docker and database stderr is withheld because it may disclose secrets;
inspect the selected engine privately to diagnose a failure. The script performs
no automatic source shutdown, container removal or volume deletion.

## Restore into a new isolated project

Choose a unique target project, a new output directory outside the backup and an
engine that already has the exact recorded database and helper images. Reserve
the target project name for this operation: do not run competing Compose or
volume-creation commands under that name.

```sh
python3 deployment/backup.py restore \
  --context default \
  --project liquiditylab-restore-20261031-001 \
  --backup "$HOME/liquiditylab-backups/paper-20261031-001" \
  --destination "$HOME/liquiditylab-backups/restore-20261031-001"
```

Restore first verifies the completion manifest, every SHA-256 digest and all
archive paths/types. It refuses the source project name, any existing target
project containers/volumes/networks, expected target volume/container names or
an existing destination. It never restores over retained data. A failed attempt
reserves its partial resources; use a new unique project and destination for a
later attempt after inspecting the failure.

The command writes a protected `compose.json` containing only `db` and generates
a fresh database password in `postgres_password` beside it, outside the backup.
The source API token and database password are not reused. The new database has
`network_mode: none`, no published port and no restart policy. Local PostgreSQL
socket access through `docker exec` supports initialization and inspection.
One-shot Python helpers populate only the new bundle/recording volumes, without
network, secrets or application entrypoints. No paper, worker, edge or training
service is started. PostgreSQL must be empty before the logical restore. The
script records success as `restore.json` only after its database verification
matches the backup inventory.

Extraction accepts only ordinary directories and files with canonical relative
paths, rejects links and path traversal, and refuses a nonempty target file
volume. File metadata is restored within the isolated target volumes. Database
objects restore under the new `liquiditylab` role without source ownership or
grant replay; this workflow is for the single application-role deployment.

## Inspect and record an offline drill

Read `restore.json` for the exact target container, volume names and verification
results. Record the backup manifest digest, source/target image IDs, project names,
commands and bounded inspection results in credential-free drill evidence. Do
not copy `postgres_password`, full Docker inspection output or secret-bearing
connection strings into evidence.

Inspect only the new target, for example:

```sh
DOCKER_CONTEXT=default docker compose \
  --project-name liquiditylab-restore-20261031-001 \
  --file "$HOME/liquiditylab-backups/restore-20261031-001/compose.json" ps

DOCKER_CONTEXT=default docker compose \
  --project-name liquiditylab-restore-20261031-001 \
  --file "$HOME/liquiditylab-backups/restore-20261031-001/compose.json" \
  exec -T db psql -X -U liquiditylab -d liquiditylab \
  -c 'SELECT tenant_id, account_id, revision FROM paper_beta.accounts ORDER BY tenant_id, account_id;'
```

Before accepting a drill, compare all restored accounts and financial/history
table verification records, confirm bundle policy-manifest/model digests and
recording paths against the backup, and confirm the target has no app containers,
published ports or external network. Query retained reports/fills/reservations
through SQL if those are part of the drill scope. Do not start the production
Compose stack against these volumes as an inspection shortcut.

The source can be resumed separately after the backup window using its original
context, project, schema, account, secrets and compatible worker. Follow the
existing deployment recovery instructions; restoration does not authorize new
trading or demonstrate active-parent recovery. No public hosting, availability,
throughput, model-quality or strategy-value claim follows from this operation.
The retained policy remains unvalidated and live-ineligible.

Target resources remain available for inspection. This tool supplies no cleanup,
reset, overwrite or delete command. Any later cleanup must identify only the
isolated target resources and preserve the source and completed backup.

# Local backup and isolated restore drill — 31 October 2026

The [backup workflow](../../backup-restore.md) refused the running source writer,
then collected a quiescent PostgreSQL schema, immutable policy bundle and
recordings volume after an explicit known-final source stop. The original demo
was later explicitly resumed; its account was ready and both pinned reports
were unchanged. Credentials, database dumps and model archives remain private.

The offline restore ran twice in fresh projects with fresh volumes and database
passwords. It verified every table and sequence against sorted JSON-row SHA-256
fingerprints: 13 tables and one sequence, including 23,922 journal entries,
23,863 command rows, 51 executions and all 51 historical reservation rows. Every
restored bundle file was reread and matched its content hash, UID/GID and mode.
The recordings volume was empty; this does not verify nonempty recording restore.
The second offline restore used the final formatted component implementation.

An explicit additional inspection step started the first clone's Go desk and PPO
worker with fresh auth and private IPC. It shared the database's isolated network
namespace and published no ports. Read-only API requests returned the exact
original revision-3569 completed report (50 fills) and revision-3736 cancelled
report (one fill). The current parent remained cancelled with no child, zero held
reservations and exact BTC 1.101 / USD 9989.87914807 balances. No plan-start or
trading command was sent. Inspection services and target databases were then
stopped; all backup/target volumes were retained. Source deployment stayed separate.

`inspection-compose-record.json` and `inspection-entrypoint-record.sh` record that
specific additional inspection setup. They are evidence fixtures with exact local
image/volume identities, not a reusable deployment template or an automatic step
in `backup.py restore`. The fresh inspection token is intentionally absent.

Negative checks refused a running source writer, the source project as target,
existing target resources, a corrupted payload and a symlinked destination inside
the backup. The 16 focused regressions and full 232-check Python suite pass; Go
and UI source were unchanged from their separately recorded checks. Scope,
commands, source/artifact hashes and file/database verification are adjacent.

This establishes a terminal-account local backup/restore and explicit inspection,
not active-parent recovery, a power-loss drill, image portability to a fresh host,
capacity, uptime, policy quality or public deployment. Full beta gates remain open.

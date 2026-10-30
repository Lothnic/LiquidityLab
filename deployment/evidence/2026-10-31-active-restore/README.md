# Isolated ACTIVE parent backup and restore

This bounded drill preserved a durable ACTIVE parent with a held reservation,
restored it into a separate isolated project, and observed ordinary recovery to
ready PAUSED. All 18 computed checks passed. No trader start or resume command
was issued to the restored account.

The source fixture was `liquiditylab-backup-active-20261031`; the destination was
`liquiditylab-restore-active-20261031`. Each was newly created from protected local
backup data. Their paper, worker and database containers are now stopped. Their
volumes, containers and private backups remain. The longest fixture container
runtime was 260.632 seconds. The protected original project
`liquiditylab-rlbeta-20261030` was not mutated; its paper container was observed
running after the drill. Existing terminal restore evidence projects were not
modified during this drill.

## Durable crash and recovery boundary

The ordinary authenticated create/start commands made parent
`active-restore-20261031-passive-b`: buy .1 BTC, limit 100.01 USD, maximum child .01
BTC and a ten minute deadline. The account retained its synthetic market source.
Real PPO inference ran in shadow mode, with the original 100ms timeout, pause
fallback and 500ms synthetic feed cadence. The policy remains unvalidated.

Only the source fixture paper container received SIGKILL. After Docker reported
that container dead, a PostgreSQL transaction acquired the account row using
`SELECT ... FOR UPDATE`. This waits for any server transaction from the dead
client to finish. The lock was released before the durable snapshot was read;
the paper and worker were stopped and no writer was running during backup.
`post-kill-commit-barrier.json` and the archived harness show this ordering.

The durable source state was revision 24115, ownership epoch 9 and
observed/processed sequence 23492/23492. The parent was ACTIVE. Child
`child-1fca834d7140060c15455a6ba39700c4` had a passive .002 BTC intent at 99.99,
a held .20037996 USD reservation and an empty attempt ID. Its durable outbox state
was **queued**, and its reducer order state was **submitting**. This was a crash
before dispatch; it was not an uncertain send or a demonstrated accepted fill
backlog. All accepted observation facts were already processed.

The restored ordinary Go daemon and real PPO worker advanced ownership to epoch
10, retained the original deadline `2026-10-30T22:28:13.552685Z`, plan specification
and policy hash, made that child obsolete and released its reservation. The
parent remained ready PAUSED across a bounded three second observation, without
a trader command. Recovered observed/processed sequence was 23573/23573.
`outbox-no-dispatch-proof.json` additionally proves that all 52 intent IDs and all
prior attempt bindings were retained, the queued child's attempt ID stayed empty,
and no previously unset attempt became bound. Absence of new fills alone is not
the evidence for absence of dispatch.

All 51 accepted execution rows, 23492 original observation rows, 24055 original
command rows/results and five original pinned reports remained identical. Exact
balances derived from original 1 BTC/10000 USD and every accepted execution are
1.101 BTC and 9989.87914807 USD. New market observations after recovery are expected;
only the retained source prefix is compared for identity.

## Evidence and reproducibility

- `crash-durable.json`, `restored-offline.json` and `recovered.json` contain the
  durable account snapshot, current plan, child outbox/reservation and balances.
- `backup-metadata.json` records exact source image IDs, all 13 ordinary table
  fingerprints plus the sequence, and bundle/recording file inventories.
  `offline-restore-result.json` records the successful restore checks.
- `provenance/*-retained-prefix.ndjson.gz` lists every retained observation,
  command and execution identity with its complete canonical row SHA256 in one deduplicated stream per table. Independently
  computed stream hashes for the source, offline clone and recovered retained
  prefixes are recorded alongside private full snapshot hashes. Observation payload and command
  result digests are included; execution financial facts are included in full.
  `provenance/index-summary.json` defines canonical encoding, counts and
  uncompressed stream digests. This supplies underlying identity evidence rather
  than only a boolean checklist.
- `pinned-reports/` contains the before/after reports at the exact original
  `as_of_revision` values. Current reports may advance after recovery.
- `verification-summary.json` records the 18 assertions, source/target isolation,
  configuration/image IDs, fresh credential comparisons, timestamps, exact
  operator command shapes and final stopped state.
- `harness-*.py.txt`, `inspection-entrypoint.sh.txt` and
  `inspection-compose.json` archive the executed private harness/configuration.
  They contain secret *references*, never credential values. The archived source
  is evidence, not a command to run against the retained project names.
- `private-input-provenance.json` fingerprints the underlying private source
  files. `backup-manifest.json` fingerprints private backup payloads; those dump
  and archive payloads are deliberately absent here.
- `verify-evidence.py` recomputes the 18 curated checks and validates all
  `SHA256SUMS` entries without Docker or credentials. Run:
  `python3 deployment/evidence/2026-10-31-active-restore/verify-evidence.py`.
- `root-independent-audit.json` records the separate root comparison of full
  private SQL snapshots, every public canonical row hash, all 52 attempt bindings,
  five pinned reports and independent rational balance derivation.
- `SHA256SUMS` covers every other file recursively in this curated directory.

The source worker/database/paper images are exact existing local image IDs. Each
database and worker had network mode `none`; each paper container shared only its
own database's isolated namespace. No port was published. Source and target used
fresh, distinct PostgreSQL passwords and API tokens. No credential content,
logical dump, volume archive or model bytes are included in these artifacts.

## Recording preservation and limits

Three nonempty retained public Kraken repository fixture files were copied into
exclusive new recording paths by an exact-image helper with network mode `none`:
`public-capture.jsonl` (31817 bytes), its manifest (778 bytes) and provenance
(3360 bytes). Backup and restore verified every file's SHA256, size, mode and
numeric owner/group. The capture SHA256 is
`d13f0af7da5c57ee9d058a62f439ec8ffc7f396ab924ecbfe2694ad8f5e876eb`.
These files test preservation of nonempty recordings. They did not supply prices
to this plan and establish no account input/source binding.

The first create request used an unsupported `passive` baseline and received
`invalid_policy`; the ordinary supported `liquidity-aware` baseline was then
used. An initial one hour .1 BTC plan made no child because .1/240 slices is below
the .001 BTC minimum lot. That parent was cancelled through the ordinary API;
the corrected ten minute parent produces a .002 BTC slice. No compiler,
timeout, budget or guard was weakened. Preserved correction artifacts show this.

Shadow observation fields distinguish the actual model proposal from the applied
baseline. At the held-child observation both were action 4. A separate preserved
observation from the initial parent has proposed action 6 and applied action 4;
it is not labeled as the later held-child boundary.

This drill proves this queued-child restore boundary only. It does not prove
uncertain-send recovery, nonzero accepted-fill debt recovery, real exchange
connectivity, source-bound Kraken execution, a 24 hour soak, capacity, public
hosting, fresh-host image transfer, or strategy quality. All money was simulated.

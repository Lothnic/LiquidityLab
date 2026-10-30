# Recording admission, logs and verification

[Component `921008f`](../../storage.md) adds exclusive recording-directory admission
before database/account initialization. The packaged Kraken profile reserves the
next bounded raw file and manifest allowance against all retained file sizes.
It refuses exhausted budgets, concurrent cooperating recorders and unsafe or
uninspectable entries. Admission has no file-deletion or financial-history
mutation path. The isolated smoke below verifies retained fixture bytes and
refusal before database access; it contains no account history to compare.

The clean committed source snapshot produced a local paper image. Its real shell
entrypoint was exercised in a disposable container with no network or host volumes.
With retained bytes and a total budget one byte below the next reservation, it
refused startup before opening the database, created no capture directory and left
the retained fixture unchanged. Runtime secrets and a dummy policy manifest were
created only inside that disposable container. `container-smoke.py` records that
container-only fixture setup; it writes fixed container paths and must not be
run on an operator host or with existing data mounted. This negative check does not
exercise a real feed, account or trained-model worker.

Focused race checks cover exact admission boundaries, orphan preservation,
unsafe entries, directory contention in a second process, bounded entry scanning,
startup failure cleanup, metrics aliases and manifest size. Controlled capture
exhaustion retains an incomplete invalid-at-end recording within its raw cap;
the rejected trade is neither delivered nor followed by another frame read.
Safe error classification exposes the configured budget reason without private
details, while preserving higher-priority authority/outcome failures.

The first full Go race run found one existing database sampler integration test
that assumed complete 20 ms coverage on a shared machine. Thirty isolated repeats
passed. A controlled slow first query then reproduced its rejection of a correctly
incomplete report. Test-only commit `14ea160` replaces fixed sleeps with observed
lock/release synchronization and strictly requires incomplete status for every
miss. Twenty parent runs (40 baseline/slow variants) passed; production sampling
and financial logic were unchanged. Both controlled RED and GREEN logs are kept.

The final suite passes 1,208 Go test/subtest events, 233 Python tests and 78 UI
tests, plus the UI production build. It has no failed or skipped test cases;
three Go packages contain no tests. Results and both compressed Go event logs
are recorded alongside this file. Python used the fresh ordinary Go replay binary; UI checks use the tracked
source input hashes and exclude the old untracked replay drafts. The existing
Gymnasium v0 deprecation warning remains. No browser/mobile review was performed.

These bounds do not solve retention, database growth, host disk headroom, alerts,
production transport, workload admission or the 24-hour soak. The original demo
was not recreated; adopting this image and log configuration requires a deliberate
stop/resume operation. No registry push or public deployment occurred.

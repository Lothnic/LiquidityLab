# Native Go + Python paper evidence — 2026-10-30

This is a bounded local synthetic-paper observation, not a strategy evaluation.
The session report is the exact JSON export captured at account revision 302.
It records one accepted policy action (semantic action 6, proposed and applied),
one simulated BTC fill of 0.01 at 100.01 USD, average fill price 100.01, and
10% completion of that particular 0.1 BTC plan. The plan was still in progress
at that revision; its child was attempted with finality unknown. Do not describe
this as a completed plan or as evidence of savings, win rate, or trading quality.

The first proposal in the observed native sequence expired; a later proposal was
accepted. The accepted session report at revision 302 was unchanged after daemon
restart and resume: its canonical JSON SHA-256 is
`99db36807d793eb73ecbf908325424a95402977fbe30d5fbfb15d5af4328a9d1`.
`report-retention.json` records the comparison.

Later, after roughly 1,797 synthetic market receipts, the account reported
`ready=false` and `recovery_required=true`; a cancel attempt returned
`recovery_required` (HTTP 409). The cause was not diagnosed. After the native Go
process received SIGTERM, the operator resumed the same account with the same
bundle hash and the Python worker restored. The account then reported
`ready=true`, `recovery_required=false`; the original second plan retained its
deadline and had expired by then, with no child. Balances were 0.02 BTC and
998.9978998 USD, retaining the first session's 0.01 BTC fill. Supporting
snapshots are included here. This limited observation does not establish a
market-feed availability target or healthy-feed soak, and the source gap must
not be attributed to worker unavailability or container load.

`training-provenance.json` records the PPO bundle's safe provenance and its
synthetic training assumptions. The bundle status is `unvalidated` and
`live_eligible` is false. `SHA256SUMS` covers every artifact in this directory.

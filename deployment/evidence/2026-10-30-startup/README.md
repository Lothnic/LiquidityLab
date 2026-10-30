# Startup availability and retained account recovery

The helper resumed the existing cancelled synthetic account with a 120-second
Compose startup wait. Paper, PostgreSQL and the worker became healthy; the edge
service ran only after paper service availability. Balances, fill count and
child state were retained. No new plan, account reset or volume deletion occurred.

HTTP health was already `ok` while the account required recovery. A source gap
therefore remains an open reliability issue; the availability probe deliberately
does not claim market or account readiness. The explicit resume restored account
readiness. This is a bounded startup/recovery observation, not a soak or SLO.

`SHA256SUMS` covers the curated JSON files. Credentials are omitted.

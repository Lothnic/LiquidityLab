# Free hosted baseline paper demo — 31 October 2026

The synthetic BTC/USD baseline paper demo is deployed at
https://liquiditylab-peach.vercel.app. Vercel routes `/v1/*` and `/healthz` to the
verified Render service https://liquiditylab-paper-demo.onrender.com. Render Free
and Neon Free PostgreSQL 18 run in Singapore; Vercel uses the existing Hobby
workspace. No paid plan or real exchange credential was selected.

Runtime source is `ff50e6814bbcfb3c89b392116c24873cb499fdef`. `deployment.json`
retains provider resource/deployment identities. Automatic Render deployment is
disabled. Later evidence/documentation commits do not change the running image.
Bearer and database credentials remain in runtime configuration and restrictive
local operator files; no credentials are included here or embedded in the UI.

## Observed hosted behavior

- The frontend returns HTTP 200. Both direct Render HTTP and the Vercel API
  proxy return health 200, anonymous account rejection 401 and authenticated
  account 200 with `ready=true`, `recovery_required=false` and `no-store`.
- The first deliberate account creation used 1 synthetic BTC and 10,000
  synthetic USD. Its short local run against the remote database reported an
  uncertain shutdown. Read-only SQL inspection verified exact initial balances,
  no plans, no outbox entries and no executions. Creation was not repeated;
  Render recovered/resumed the retained account successfully.
- An initial test requested 0.001 BTC over three slices. Each computed slice
  rounded below the 0.001 BTC lot size, so it expired without orders or fills.
  That history and its failed test result are retained. No execution guard was
  weakened; the next test required an actual preview intent before creation.
- The subsequent liquidity-aware 0.006 BTC plan began paused, started explicitly,
  completed through three simulated fills, and reached `known_final=true` with
  no child or remaining reservations. An exact duplicate create command returned
  the original committed revision with `duplicate=true`.
- Session history and the revision-pinned JSON report were retrieved through
  Vercel. The report is pinned to revision 544 and has SHA-256
  `a388fa42a2a5795fff747d5b1016f40cc59522769a1509203ad5c150dd98ffbc`.
- An explicit Render restart produced a fresh instance created at
  `2026-10-31T14:39:52Z`. Account authority advanced, readiness recovered, balances
  and zero reservations matched exactly, and the pinned report remained identical
  byte for byte, including all three fill identities.

`SHA256SUMS` covers the adjacent JSON records. Tests used synthetic paper input;
neither the initial expired plan nor the successful fills were exchange orders.

## Scope

This is the smaller baseline demo, not acceptance of the RL-assisted hosted beta.
There is no Python policy worker, Kraken recording, replay archive or live order
transport in this profile. Browser/mobile review, supported workload capacity,
long-duration reliability and the 24-hour soak remain open. Render Free can sleep;
Neon compute/storage quotas are finite. The application token is still required
to connect to its single shared demo account.

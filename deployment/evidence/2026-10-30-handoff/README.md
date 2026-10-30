# Local container execution after bounded result handoff

This is actual local product execution with Docker context `default`, Compose
project `liquiditylab-rlbeta-20261030` and host loopback port 18882. Runtime source
is `b795231d4737aff77c03d45cb7e086f92ee0dc69`. No training or model replacement
occurred: provenance distinguishes the original training Go binary digest from
the new running daemon digest. PPO remains unvalidated and live-ineligible.

The .1 BTC plan genuinely accepted policy actions and filled .1 BTC through 50
simulated fills, reaching completed/known_final at revision 3569. Completion took
about 126 seconds, not the initial 30 second observation window. The .01 child
quantity was a cap: ordinary participation guards compiled .002 quantities. No
operator resumes, source cadence changes or budget changes were used. The initial
observed accepted-action/fill report is pinned to revision 3062 and was retrieved
again from durable history; the completed report is pinned to revision 3569.
These observations establish execution, not strategy quality or profitability.

A separate authorized .02 BTC plan accepted an action and produced one .001 maker
fill. It subsequently paused with reason `expired` at 12:43:56.026 UTC. Docker's
worker SIGTERM event was later, at 12:43:59.459 UTC. Consequently this scenario
**does not prove a worker-loss-caused fallback**: expiry won before the worker was
stopped. The original earlier evidence directory separately records an actual
`worker_unavailable` fallback; it is not relabeled as post-fix evidence. This plan
was explicitly cancelled and reached known_final at revision 3736.

A full own-project stop/resume retained both sessions. Exact parsed reports queried
with the original as_of_revision were equal before/after, including all 50 and 1
execution IDs. Account balances stayed BTC 1.101 and USD 9989.87914807, child was
null, and BTC/USD reservations were zero. The account recovered ready with no
recovery requirement. The first immediate post-start HTTP read encountered a
connection reset before readiness; a subsequent read succeeded. No credentials
are present in these curated artifacts. SHA256SUMS covers all JSON recursively;
original 2026-10-30 evidence was left unchanged.

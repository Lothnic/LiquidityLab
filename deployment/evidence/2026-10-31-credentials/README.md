# Account credential rotation over verified local TLS

The [credential procedure](../../credentials.md) was exercised on the isolated
restored terminal-account fixture from the [TLS check](../2026-10-31-tls/README.md).
The verifier first obtained HTTP 200 with the old token. It stopped the edge and
paper daemon, observed the old process exit with code zero and `known_final`,
replaced the owner-only token file atomically, then recreated paper and edge in
explicit resume mode. The old token was held only in verifier memory.

Over HTTPS with the explicit test CA and hostname verification, the old token
then received 401, the new token 200, no token 401, and the new token for a wrong
account 404. Health returned 200 and the account became ready without recovery
required. No valid trading command was issued. The cancelled parent, child,
balances, reservations, fills and policy configuration/runtime were unchanged.
Both original revision-pinned reports, containing 50 fills and one fill,
matched exactly before and after rotation. Source/account revisions advanced as
ordinary receipts were ingested; this is not whole-database equality.

The fixture was stopped after verification with volumes retained. The original
demo remained running and a separate read-only account check confirmed readiness
and unchanged balances. No token, password or private key is retained here.
`verification.json` was checked against both actual old and new token values in
memory before the verifier exited. Other published files were checked against
the current fixture/source credentials, database password and private key.

The included verifier is a record of this specific private fixture and its local
paths, not a general deployment command. `scope.json` pins its inputs and limits.
This demonstrates disruptive single-token rotation for one account, not public
hosting, database-password rotation, browser behavior, credential delivery or
continuous availability. Active shutdown and crash recovery retain their own
separate evidence and release gates.

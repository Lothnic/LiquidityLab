# Execution engine feasibility

Checked 23 October 2026. LiquidityLab's initial replay environment uses its own narrow execution contracts. Live order submission has not been enabled.

The current [NautilusTrader installation documentation](https://nautilustrader.io/docs/latest/getting_started/installation/) describes the 2.x API, while its stable PyPI line is 1.x. The documentation explicitly says the APIs differ. Its 2.x wheels target Python 3.13–3.14 and are release candidates, whereas the initial LiquidityLab environment uses Python 3.12 for compatibility with the learning stack. A dependency installation is not evidence of a working Kraken live integration.

**Decision:** keep the first executable RL environment and recorder independent of NautilusTrader. Keep venue I/O out of accounting, action compilation, and policy interfaces. This implements the planned feasibility gate rather than adopting an unverified adapter API.

Before choosing the live engine, pin a released version and prove:

1. Kraken Spot instrument metadata and fee/account handling.
2. Controlled replay stepping without missed callbacks.
3. Partial fills, IOC/post-only orders, and expiry behavior.
4. Recovery of a submission with an unknown acknowledgement.
5. Ownership of durable intents and engine state across restart.
6. Compatibility with the chosen interpreter and packaged deployment.

No credentials, private account calls, real orders, or engine integration claims are part of the first replay milestone.

The public recorder uses [Kraken's L2 feed](https://docs.kraken.com/exchange/api-reference/spot-websocket-v2/book) and [documented v2 checksum rules](https://docs.kraken.com/exchange/guides/websockets/book-checksum-v2). It records raw payloads and receipt order, validates checksums, truncates reconstructed books to subscription depth, excludes historical trade snapshots from fresh flow, and marks reconnects/corruption as gaps. A live recording is a market-data artifact, not evidence of simulated execution accuracy.

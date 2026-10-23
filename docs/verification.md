# Verification record

## Paper desk checkpoint — 23 October 2026

Implementation checkpoint `dc521e5`, with authority revision correction
`a102a5e`. Host tools: Go 1.26.8, PostgreSQL 18.6, Python 3.12.11,
Node 26.5.0 and npm 11.17.0. PostgreSQL is a local development instance;
its tests create isolated schemas, not a production database.

Executed checks:

- `go test -race -count=1 ./...` with `LIQUIDITYLAB_TEST_POSTGRES_DSN` set:
  all packages passed, including runtime, store, API and paper CLI/server.
- `npm test` in `apps/web`: 35 tests across four files passed. Component tests
  cover exact decimal plan submission, preview authority binding, immutable
  unknown-outcome retry, late account responses, ready waiting previews,
  readiness rejection and form-edit invalidation. API tests cover nested
  payloads, malformed successful receipts and journal cursor validation.
- `npm run build` in `apps/web`: TypeScript and Vite production build passed.
- `LIQUIDITYLAB_REPLAY_BINARY=<built liquiditylabd> .venv/bin/python -m pytest -q`:
  87 passed, no skips. One existing Gymnasium v0 deprecation warning remains.
  A preliminary run without the binary passed 80 and skipped seven integration
  checks; the configured rerun above exercised them.
- `.venv/bin/ruff check .`: passed.
- `scripts/generate_protocol.py --check`, using the pinned Go plugins: generated
  Go/Python bindings matched.
- `go test -race -count=1 ./internal/inference -run TestPythonPPOWorkerBoundary -v`
  with the real Python/replay paths: passed; a tiny CPU PPO run supplied 20 Go
  features and a semantic action to Go validation/compilation. This does not
  prove actor scheduling, fallback integration or model quality.

### Browser and durable outcome

The in-app browser exercised the actual built React assets, authenticated API,
Go actor and PostgreSQL store against explicitly **synthetic** feed events and
balances. Cohort `ui_preview_03` followed preview → create paused → start →
partial fills → pause → resume → cancel. The retained database outcome was:

- Target 0.05 BTC; seven simulated fills totaling 0.042 BTC, average 100.01 USD.
- 0.008 BTC residual; 0.00840084 USD simulated fees.
- Frozen arrival mid 100 USD; executed cost 0.00882084 USD, excluding residual.
- Final BTC balance 0.042; USD 15.79117916; locked balances zero.
- Parent cancelled; zero held local reservations and zero unresolved intents.
- Graceful server shutdown reported `known_final=true` and retained its schema.

A separate final-build cohort `ui_preview_05` verified a ready preview with no
immediately placeable first child: the desk explained the waiting estimate,
created a paused parent, then cancelled it without starting. The browser error
log was empty during that check. Cohorts used distinct new accounts/schemas;
existing account state was never reset to repeat a run. Every verification
server stopped with a known-final result.

The workflow exposed and corrected perpetual preview expiry from no-effect
paper receipts, authority churn from HOLD bookkeeping, misleading final-state
copy and fee currency labels, and long-plan-ID/header mobile overflow. At the
final narrow layouts, document width was 320 at viewport 320 and 375 at viewport
390 (the desktop browser reserves scrollbar width). The temporary viewport was
reset. Financial tables scroll locally where needed; the document fits.

Local ignored evidence:

- `artifacts/paper-desk-cancelled.jpg`: completed browser workflow with retained fills.
- `artifacts/paper-desk-mobile.jpg`: 320 px long-ID plan layout.
- `artifacts/paper-desk-workflow.json`: scoped durable synthetic account summary.

These files are local run artifacts, not committed real-market datasets. The
browser workflow is development evidence, not design-user acceptance, production
capacity, OS process-kill recovery, private venue integration, or a live pilot.
Full history/session replay, asynchronous actor/model integration, immutable
holdout/promotion, deployment, load/soak/restore and live authorization gates
remain open. Real public capture has still not been demonstrated on this host.

## Historical Python reference slice

The following records the original implementation and was not the suite size
at the current checkpoint.

## Executed checks (original slice)

- `uv run --extra dev --extra rl pytest`: 40 checks passed. The optional integration test trains an 8-step PPO policy on synthetic data, saves it, reloads it on CPU, and performs inference.
- `ruff check .`: passed.
- A separate CPU PPO smoke run completed 128 requested/actual steps with seed 7. Its bundle is unvalidated and is not eligible for live execution.
- Both checked-in baseline reports were generated from the checked-in synthetic tape and plan files. The buy completes 0.060 base units; the sell completes 0.030 of 0.060 and leaves the remainder at its deadline.
- `uv build --out-dir dist`: built the source distribution and wheel.
- The wheel was installed in an isolated `uv run --no-project` environment. The registered Gymnasium environment reset and stepped with a 20-feature observation and six discrete actions. The recorder and training guard imported from that installed wheel.

The full verification environment includes Gymnasium 1.4.0, NumPy 2.5.4, Stable-Baselines3 2.9.0, and CPU PyTorch 2.14.1. The lockfile records the resolved dependencies. Default installation omits the optional RL extra.

## External connection outcome

A 15-second public Kraken WebSocket capture attempt received no usable book messages because the opening handshake timed out. The recorder wrote gap records and a failure manifest; no real-data replay result is claimed. Protocol tests use Kraken's published checksum example and local fixtures.

At that original slice, private account access, live orders, trader interface, durable live sessions, holdout evaluation and promotion were absent. The current paper implementation is recorded above; private live execution, holdout evidence and promotion remain incomplete.

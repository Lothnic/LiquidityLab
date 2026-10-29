# LiquidityLab trader UI

`apps/web` is a React and TypeScript operator desk for the existing authenticated
`internal/api` contract. It is intentionally a thin client: prices, balances,
fees, order state, and command receipts come from the server. It has no demo
fallback and does not invent market candles, P&L, or model explanations.

## Run locally

Use Node 26 and npm 11 (the versions used for this build). Start the Go API
service on `127.0.0.1:8080`, then run:

```sh
cd apps/web
npm ci
npm run dev
```

Open the Vite address printed by npm. The dev server binds to loopback and
proxies `/v1` and `/healthz` to `http://127.0.0.1:8080`. The browser uses
relative same-origin API paths; it does not accept an API URL from the user.
The local `liquiditylabd paper` daemon serves `npm run build`'s `dist/` assets
on the same origin as `/v1`; see [paper-server.md](paper-server.md) for the
explicit synthetic account, balances, source and credential configuration. It
preloads immutable assets, so restart it after a rebuild using the documented
create/resume commands. A recovered parent stays paused until explicit trader
resume; see [paper recovery](paper-recovery.md). Hosted deployment remains a
separate milestone.

Commands:

```sh
npm test
npm run build
```

`package-lock.json` pins all direct and transitive npm packages. The bundled
IBM Plex Mono and Cormorant Garamond font assets are served locally; the UI
makes no third-party font or analytics requests.

## Operator flow

The desk separates **Trade** from **Session history**. Trade follows three
visible steps: set limits, review the plan, then start trading. Labels use
everyday order language; internal parent/child identifiers live under account
details. The existing paper profile and market-source distinction remain.

1. Paste the configured operator bearer token into the password field. It is
   held only in component memory, never URL, local storage, or session storage.
2. Select one of the exact tenant/account scopes returned by `GET /v1/accounts`.
   The active selection is visible in the control strip.
3. Enter buy/sell side, Amount, Maximum buy price or Minimum sell price,
   Time to complete, and Maximum per order. Execution settings contains the
   scheduled/liquidity-aware method and cash/asset budgets. Financial inputs remain text through
   request construction; the UI does not round them through JavaScript numbers.
4. Choose **Review plan** and inspect the first proposed order, price and funds
   needed. If it is ready and the financial/control authority is
   unchanged, create a paused plan. Starting or resuming requires a separate
   command. A ready preview without a first child can still create a paused
   plan: the baseline may wait for a larger slice or an eligible price. This
   does not guarantee a later fill. Duration fixes an absolute deadline at creation; waiting to start
   or pausing does not extend it.
5. Supervise fills, fees, remaining quantity, current child status, account
   holds, and chronological journal metadata. Pause requests cancellation of
   the child. The UI keeps `cancel_requested` visible until the server reports
   known-final state.
6. Refresh Session history and select a retained session. Inspect exact totals,
   frozen benchmark estimates and stored event revisions, or export its report.
   Fill navigation renders at most 200 rows per page. Timeline export explicitly
   indicates whether its bounded event set is complete. See
   [session-history.md](session-history.md) for cursor and financial semantics.

Execution settings, balance breakdowns and technical activity use native
disclosures. Invalid fields inside execution settings reopen that disclosure
before the browser focuses the field. The current plan and cancellation warnings
stay visible; a running/paused plan collapses the new-plan composer. Free balances
and local reservation amounts are displayed separately. A pending action shows
its target account, distinguishes an in-flight request from an unknown outcome,
and retains the original command for an explicit **Retry same action**.

A configured Python policy shows its actual proposal/fallback status in the
current session. The deterministic baseline remains the default. Worker loss
uses the explicitly configured baseline or pause behavior; no model gains live
eligibility through this interface.

The prominent Paper badge describes the execution profile. A separate source
badge identifies the server's `data_source` (`KRAKEN PUBLIC`, `RECORDED`,
`SYNTHETIC FEED`, or `MANUAL`). A public Kraken market-data feed still means
paper execution; it does not mean an exchange order is being sent. Unknown
source values are labelled `SOURCE UNKNOWN`.

## Command safety and data freshness

The UI polls the account once per second after each completed request; it does
not start overlapping polling loops. Account-read requests use abort signals
and the account polling interval backs off to 30 seconds after read errors. The
last confirmed account view remains visible with a stale warning until a
refresh succeeds. Journal metadata advances in chronological pages of at most
100 entries when account revisions advance, and is bounded to 500 entries in
browser memory.

Each mutation carries one generated command ID and the currently displayed
expected durable revision and authority revision. Receipt-only journal updates
do not invalidate the authority token; fills, queue changes, account validity
and controls still do. The exact ID, revisions, and body stay in memory if a request
times out or returns an uncertain server error. The desk offers an explicit
retry that reuses that command; it never silently changes the revision or
creates a replacement ID. A `stale_revision` response proves the command did
not commit at that revision, so the UI refreshes and asks the operator to
review. Recovery, ownership, conflict, and generic server errors remain
uncertain and retain the original command for retry. A known validation or
preflight rejection clears it. Disconnect is disabled while an outcome is
unknown so the in-memory command cannot be discarded accidentally.

New plan creation is disabled unless profile is paper, account and market are
ready, a confirmed view is no older than ten seconds with no polling error, the clock is valid, and there is no active, paused, or unresolved parent
or child. Recovery and stale-market states are surfaced from the API, not
inferred from a local quote. The UI cannot force reconciliation or mark an
unknown cancellation complete.

## Financial presentation and limits

Balances, reservations, fill quantities, fill prices, and fees are rendered
from server decimal strings. Fill rows show the latest eight rows from the API
projection. Their time column is an elapsed offset (`T+...`), not a wall-clock
timestamp. The API itself includes at most the latest 2,048 fills; the server
reports total count and truncation. Average fill price and executed cost are
separate metrics. The **Cost vs. start price** value is the server's fill slippage against the
frozen arrival midpoint plus quote-valued fees; residual quantity is excluded.
It is not P&L, total cost, or policy reward. The UI labels paper fills as
simulated.

This view does not expose raw snapshots, private decision features, order-book
depth history, journal payloads, exchange credentials, or live execution
controls. The current API has no streaming endpoint, so snapshots and new
journal metadata are polled. Live promotion is unavailable; the persistent
Paper label and source label remain visible regardless of market-data source.

Every successful API payload is validated before it reaches the desk, including
scopes, nested account state, preview amounts and paginated journal cursors. A
malformed successful command receipt retains the command as outcome unknown.
Account/authentication generations and request sequences reject late responses
after selection changes; preview bindings retain an immutable exact plan and
policy and are invalidated by form edits.

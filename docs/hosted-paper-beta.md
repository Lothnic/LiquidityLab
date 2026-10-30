# Current milestone: hosted paper-trading beta

Selected 30 October 2026. Prepare a portable deployment first; select the hosting
provider and domain later. The full implementation roadmap remains open. Passing
this milestone establishes the paper beta only.

## Release scope

An invite-only beta for discretionary traders using the existing BTC/USD paper
desk. Each deployed daemon owns one explicit paper account. Start with a bounded
cohort of isolated instances; publish the supported cohort only after measuring
it. The beta includes a real trained RL policy running in the Python CPU worker,
with Go retaining all order, balance and risk authority. Scheduled and
liquidity-aware baselines remain available as explicit execution/fallback modes.
The demo policy is unvalidated and restricted to simulated execution.

The required trader journey is:

1. Sign in to the intended account over HTTPS.
2. Review the market-data source and simulated balances.
3. Set an amount, hard price limit, deadline and maximum order size.
4. Review the first proposed order, create a paused plan and start it explicitly.
5. Supervise fills, fees, remaining quantity and local reservations.
6. Pause, resume or cancel, with pending cancellation and uncertain actions shown.
7. Reopen session history and export the saved report.
8. Inspect the model's actual proposal, the action Go applied and any fallback.

Public Kraken prices can drive paper execution. Synthetic input remains an
explicitly labelled demo/smoke profile. Instrument rules, fees and fills retain
their actual assumption labels. No private exchange credentials are required.

## Acceptance checklist

### Trader workflow

- [ ] Desktop and mobile review of the simplified desk, including loading,
      disconnected, invalid-market, paused, partial-fill and cancellation states.
- [ ] Complete the trader journey against the packaged service and PostgreSQL.
      Exact amounts, command identity, account scopes and saved reports survive
      reconnects and restarts.
- [ ] Resolve unknown command outcomes by the original identity. Disable new
      price-sensitive actions on stale/invalid data while keeping inspection and
      cancellation available.
- [ ] A design user can complete the journey and explain the distinction between
      a paused plan, pending cancellation and a completed session.

### Portable packaging

- [ ] Reproducible frontend and Go service images/packages from a pinned source
      revision and locked dependencies, with documented configuration.
- [ ] An isolated PostgreSQL service or external-database configuration, persistent
      volumes and explicit migration procedure.
- [ ] Deliberate first-account creation and subsequent resume. No deployment
      restart silently replaces balances or creates a replacement account.
- [ ] Runtime-only credentials, restrictive filesystem permissions and a clear
      boundary between application ingress, database access and internal workers.
- [ ] Bounded recording storage, distinct capture identities on restart, and
      retention/rotation behavior which preserves referenced source material.
- [ ] A fresh environment can start, stop, resume and upgrade the same deployment
      using the supplied instructions.

### RL-assisted paper demo

- [ ] Build and train a real PPO bundle through the Go-backed environment. Retain
      its model hash, training settings, source-data identity and simulator
      assumptions. A short demo training run does not establish trading quality.
- [ ] Package the Python CPU worker and pinned bundle alongside the Go service,
      using a private local transport, bounded resources and no exchange secrets.
      Startup verifies the model hash and observation schema before account use.
- [ ] In paper mode, actual model proposals pass through Go's ordinary execution
      checks and simulated order/fill path. In shadow mode, clearly distinguish
      the proposal from the baseline action actually applied.
- [ ] The desk visibly shows paper/shadow mode, unvalidated training status,
      latest proposed/applied semantic actions, decision time and model identity.
      Waiting, HOLD and fallback states must be understandable without exposing
      internal worker errors or inventing model explanations.
- [ ] Demonstrate worker loss or an expired proposal, the configured pause or
      baseline fallback, and responsive trader controls. Recovery must retain
      the same account and pinned policy identity.
- [ ] Supply a repeatable guided demo from training through the packaged desk,
      history and report export. Label synthetic market inputs explicitly; do
      not claim measured AI savings or performance without a valid comparison.

### Secure hosted access

- [ ] A provider-independent HTTPS/reverse-proxy configuration; application and
      database ports are not directly exposed to the public network.
- [ ] Invite-only, account-scoped access with a documented credential provisioning,
      revocation and rotation procedure. Tokens never enter URLs, frontend builds
      or logs.
- [ ] Measured request/body/concurrency limits, server timeouts and bounded history
      reads. Replay traffic preserves capacity for trading controls.
- [ ] Public market-data egress works in the selected environment and feed loss
      produces visible readiness failure instead of fabricated prices.

### Operations and release evidence

- [ ] Distinguish process liveness, service availability and market/account trading
      readiness. Data gaps must not make stop controls unreachable.
- [ ] Logs, metrics and actionable alerts cover database/ownership failures, source
      gaps, storage limits, unresolved outcomes and failed recovery.
- [ ] Restore a backup into a separate paused deployment and verify balances,
      reservations, fill identities, command results and reports.
- [ ] Exercise process termination and restart under the declared beta workload.
      Accepted effects remain recoverable and original deadlines are retained.
- [ ] Complete a 24-hour soak within declared recording/storage bounds and publish
      the tested limits and recovery headroom for the initial cohort.
- [ ] CI validates pull requests and main-branch changes. Release checks configure
      real PostgreSQL and Python/replay integration paths; missing prerequisites
      cannot be mistaken for a complete integration pass.
- [ ] Retain the release revision, artifact digests, configuration, evidence,
      operator instructions and rollback decision. Observe a deployment smoke
      check in the chosen host before inviting users.

## Delivery order

1. Make the real RL proposal/action/fallback flow visible in the desk and prepare
   portable frontend, Go, Python worker, database and gateway artifacts.
2. Add startup/resume, migration, health, recording-retention and credential
   lifecycle procedures around the packaged service.
3. Run the packaged workflow, workload/restart, backup/restore and soak checks;
   correct failures and publish the initial supported limits.
4. Select the provider/domain, prepare a concrete deployment for approval, deploy,
   and complete the hosted smoke check and first design-user session.

The earlier 2–3 week planning allowance assumes focused engineering and timely
feedback. Re-estimate after the packaging and first end-to-end deployment checks;
it is not a committed launch date.

## Later milestones

The full roadmap still includes shared-account supervision/feed distribution,
actual-session counterfactual replay, immutable learned-policy evaluation and
promotion, private exchange execution/reconciliation and an independently
authorized live pilot. Conditional sharding follows measured need.

The draft replay UI remains uncommitted and is not part of the current shipped
desk. Existing verified baseline comparison CLI/API jobs remain available.
Statistically credible learned-policy evaluation/promotion, a Go training-default
switch and real-money execution retain their separate evidence and authorization
gates. The actual RL-assisted paper demo above is required for this beta.

## Current evidence

The Go execution/store, authenticated API, baseline paper desk and history are
implemented. The UI cleanup passes its 48 existing component checks and production
build. Post-cleanup browser/mobile sign-off remains open after browser access was
blocked. Earlier PostgreSQL race, conformance and process-recovery evidence is
recorded in [verification.md](verification.md); it does not validate deployment
packaging or the eventual hosting environment.

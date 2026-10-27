# Asynchronous paper policy runtime

The paper daemon uses the scheduled deterministic baseline by default. A local Python policy is an explicit paper or shadow opt-in. No configuration or worker manifest enables live transport or live policy promotion.

Start a compatible Python policy worker using the worker instructions in `src/liquiditylab/inference/worker.py`, then add these options to the usual `liquiditylabd paper` command:

```sh
--policy-target 127.0.0.1:50052 \
--policy-sha256 <exact-lowercase-bundle-sha256> \
--policy-allow-unvalidated \
--policy-profile paper \
--policy-fallback pause \
--policy-timeout 100ms
```

`--policy-target` accepts a literal loopback IP and port or `unix:/absolute/socket/path`. The daemon verifies policy identity, observation version, ordered field names, promotion status, and non-live eligibility before creating its new synthetic account. Required policy configuration errors abort startup. The explicit flags can also be supplied through `LIQUIDITYLAB_POLICY_TARGET`, `LIQUIDITYLAB_POLICY_SHA256`, `LIQUIDITYLAB_POLICY_ALLOW_UNVALIDATED=true`, and `LIQUIDITYLAB_POLICY_FALLBACK`. The profile and timeout remain explicit optional flags; paper and 100ms are their defaults. Policy response budgets must be positive and at most 200ms.

`--policy-fallback baseline` executes the parent session's pinned deterministic baseline on failure. `--policy-fallback pause` pauses the parent and cancels its child through the existing durable control path. Loss of readiness, an invalid book, or a terminal parent prevents execution even when baseline fallback is configured. `--policy-profile shadow` records valid worker proposals and executes the pinned baseline. Neither mode validates or promotes an unevaluated model.

## Authority and bounded work

The serialized actor freezes and persists one decision request before calling Python in a separate goroutine. There is at most one outstanding RPC per actor and at most one pending slot per parent. The response channel holds one result. A worker that ignores its deadline cannot block market ingress, fills, control commands, lease renewal, or fallback resolution. Until that worker actually returns, subsequent slots take their configured fallback without spawning more RPC goroutines. Production RPC requests have their own deadline and admission bound.

The worker request binds tenant, account, parent identity and revision, financial/control authority revision, policy hash, observation schema, original market snapshot, and expiry. Ownership generation is local durable audit metadata; it is not a field in the worker wire binding. Ownership checks fence every commit, and recovery invalidates old pending requests. Source-only receipts and automatic HOLD bookkeeping preserve financial/control authority. Fills, queue changes, reservations, controls, readiness changes, and owner recovery invalidate it. The actor drains accepted durable facts before checking a returned proposal. Compilation uses the latest valid book; an updated book snapshot alone does not invalidate a proposal.

A slot resolves once. A valid action or deterministic baseline compiles through the ordinary execution safeguards and records its outcome in the same transaction as any intent and reservation. Pause fallback and its audit commit together before child cancellation. Later responses produce an audit-only journal entry and cannot compile another action. An owner that loses its lease cannot commit a result or dispatch an intent. Recovery fences an old pending request as `fallback_pause` / `owner_restart`, pauses the session, and requires a fresh market snapshot and reconciliation.

The account snapshot pins its default `policy_bundle`, and every parent independently pins `policy_bundle`. Restart rejects a different hash, schema, worker promotion status, profile, fallback, or timeout while a retained session or financial obligation remains unsettled. A terminal account may adopt a new default bundle for its next session only after its child, pending policy request, intents, reservations, pending facts, and unresolved facts are settled and every observed fact has been processed. The terminal parent's original bundle remains pinned. Public baseline decision commands cannot bypass policy slot ownership when a policy is configured; ordinary explicit manual action/control commands remain revision checked.

## Durable projections

The existing account journal and its versioned snapshots provide the audit. `policy_pending` records the request, slot, original market snapshot, expiry, frozen observation, ownership epoch (`owner_generation`), financial authority revision, original journal revision, and bundle. `last_policy_decision` records its final `result`, safe reason code, applied semantic action, optional `proposed_action`, resolution time, worker promotion mode when present, and `applied_market_snapshot_id`. Results include `policy_action`, `fallback_baseline`, `shadow_baseline`, and `fallback_pause`. The ordinary snapshot reason also describes compiler guards such as HOLD or message budget.

`policy_late_outcome` records an ignored response separately, preserving the final decision. Original request and final snapshots remain discoverable in the durable journal, including after a new parent starts. These projections contain no training seed, transport address, credential, raw worker error, or secret. Observations and bundle hashes are bounded, explicit model provenance rather than live eligibility claims.

## Validation

Real PostgreSQL race tests cover stalled inference, cancel responsiveness, lease renewal, expiry, saturation, configured baseline/pause fallback, late response suppression, latest valid book acceptance, HOLD authority stability, fill/control/readiness staleness, schema/hash/live-mode rejection, pinned restart recovery, and daemon RPC wiring. The inference package separately runs the real Python worker interoperability test when its Python dependencies are available.

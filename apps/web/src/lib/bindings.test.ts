import { describe, expect, it } from 'vitest';
import { bindPreview, captureScope, formatElapsedNanoseconds, isLatestRequest, planFingerprint, previewMatches, requestMayClearIndicator, scopeMatches, snapshotIsFresh } from './bindings';
import type { PlanInput, Preview } from './api';

const plan: PlanInput = {
  symbol: 'BTC/USD', side: 'buy', quantity: '0.05000000', limit_price: '102.0100',
  deadline_ns: 900_000_000_000, max_child_quantity: '0.010000', quote_budget: '1000.000', base_balance: '1.00',
};
const response: Preview = {
  revision: 7, authority_revision: 5, profile: 'paper', observed_at: '2026-10-23T00:00:00Z',
  market: { valid: true, snapshot_id: 's1', instrument_revision: '1', best_bid: '100', best_ask: '101' },
  policy: 'scheduled', ready: true, reason: 'ready', spread: '1', visible_bids: '2', visible_asks: '3',
  intent: { side: 'buy', quantity: '0.01', price: '100', passive: true },
  reservation: { currency: 'USD', amount: '1' }, estimated_maker_fee: null, estimated_taker_fee: null,
  fee_source: 'configured', completion_guaranteed: false,
};

describe('UI request and preview bindings', () => {
  it('distinguishes two scopes even when their revisions match', () => {
    const accountA = captureScope('secret', 'tenant', 'A', 12);
    const accountB = { token: 'secret', tenant: 'tenant', account: 'B', generation: 13 };
    expect(scopeMatches(accountA, accountB)).toBe(false);
    expect(scopeMatches(accountA, { ...accountB, account: 'A', generation: 12 })).toBe(true);

    const bound = bindPreview({ tenant: 'tenant', account: 'A', revision: 7, authorityRevision: 5, plan, policy: 'scheduled', result: response });
    expect(previewMatches(bound, { tenant: 'tenant', account: 'B', authorityRevision: 5, plan, policy: 'scheduled' })).toBe(false);
  });

  it('keeps a preview valid across receipt-only revisions but rejects financial changes', () => {
    const bound = bindPreview({ tenant: 'tenant', account: 'A', revision: 7, authorityRevision: 5, plan, policy: 'scheduled', result: response });
    expect(bound.revision).toBe(7);
    expect(previewMatches(bound, { tenant: 'tenant', account: 'A', authorityRevision: 5, plan, policy: 'scheduled' })).toBe(true);
    expect(previewMatches(bound, { tenant: 'tenant', account: 'A', authorityRevision: 9, plan, policy: 'scheduled' })).toBe(false);
  });

  it('rejects an old refresh response after a newer same-scope request starts', () => {
    expect(isLatestRequest(1, 2)).toBe(false);
    expect(isLatestRequest(2, 2)).toBe(true);
  });

  it('lets the latest current-scope request clear the refresh indicator even when it is silent', () => {
    expect(requestMayClearIndicator(2, 2, true)).toBe(true);
    expect(requestMayClearIndicator(1, 2, true)).toBe(false);
    expect(requestMayClearIndicator(2, 2, false)).toBe(false);
  });

  it('requires a confirmed snapshot no older than ten seconds', () => {
    expect(snapshotIsFresh(10_000, 20_000)).toBe(true);
    expect(snapshotIsFresh(10_000, 20_001)).toBe(false);
    expect(snapshotIsFresh(null, 20_000)).toBe(false);
    expect(snapshotIsFresh(20_001, 20_000)).toBe(false);
  });

  it('formats fill offsets as elapsed time rather than a wall clock', () => {
    expect(formatElapsedNanoseconds(3_723_004_000_000)).toBe('T+01:02:03.004');
    expect(formatElapsedNanoseconds(9_000_000)).toBe('T+00:00:00.009');
  });

  it('binds exact plan and policy and retains decimal text for the submitted payload', () => {
    const bound = bindPreview({ tenant: 'tenant', account: 'A', revision: 7, authorityRevision: 5, plan, policy: 'scheduled', result: response });
    expect(bound.plan.quantity).toBe('0.05000000');
    expect(bound.plan.limit_price).toBe('102.0100');
    expect(bound.plan.quote_budget).toBe('1000.000');
    expect(previewMatches(bound, { tenant: 'tenant', account: 'A', authorityRevision: 5, plan, policy: 'scheduled' })).toBe(true);
    expect(previewMatches(bound, { tenant: 'tenant', account: 'A', authorityRevision: 5, plan: { ...plan, quantity: '0.05' }, policy: 'scheduled' })).toBe(false);
    expect(previewMatches(bound, { tenant: 'tenant', account: 'A', authorityRevision: 5, plan, policy: 'liquidity-aware' })).toBe(false);
    expect(planFingerprint(plan, 'scheduled')).not.toBe(planFingerprint({ ...plan, quote_budget: '1000' }, 'scheduled'));
  });
});

import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from './api';
import { history, type SessionReport, type SessionSummary } from './history';

afterEach(() => vi.unstubAllGlobals());
const summary: SessionSummary = {
  plan_id: 'plan-1', created_revision: 4, revision: 9, created_at: '2026-10-23T10:00:00Z', updated_at: '2026-10-23T10:01:00Z',
  profile: 'paper', data_source: 'synthetic', base_currency: 'BTC', quote_currency: 'USD', status: 'completed', side: 'buy', symbol: 'BTC/USD',
  quantity: '0.12000000000000000001', limit_price: '101.25', policy: 'scheduled',
};
const report: SessionReport = {
  ...summary, tenant_id: 'tenant-a', account_id: 'account-a', version: 'session-report-v1', as_of_revision: 9,
  plan: { id: 'plan-1', revision: 9, status: 'completed', started: false, start: '2026-10-23T10:00:00Z', deadline: '2026-10-23T10:10:00Z',
    spec: { side: 'buy', quantity: '0.12', limit_price: '101.25', deadline_ns: 1792749600000000000, max_child_quantity: '0.1', quote_budget: '12.15', base_balance: '0', symbol: 'BTC/USD' },
    policy: 'scheduled', filled: '0.1', remaining: '0.02000000000000000001', average_price: '101.2', arrival_mid: '101.15', benchmark_valid: true,
    executed_cost_quote: '123.4500000000000000001', fees_base: '0', fees_quote: '0.1' }, child: null, fills: [],
  completion_percent: '83.3333333333333333333333333333', known_final: true, recovery_required: false, deadline_result: 'completed',
  market: { valid: true, snapshot_id: 'snap', instrument_revision: 'i1', best_bid: '101.1', best_ask: '101.2' },
  residual_reference_mid: '101.15', residual_reference_revision: 9, residual_reference_kind: 'first-terminal',
  residual_opportunity_quote: '-0.0000000000000000001', marked_cost_estimate_quote: '123.4499999999999999999',
};

describe('revision-bound session history API', () => {
  it('validates the account page and enforces its descending creation cursor', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Response.json({ entries: [summary], as_of_revision: 11, next_before_revision: 4, limit: 20 })));
    const value = await history.sessions('secret', 'tenant-a', 'account-a');
    expect(value.entries[0].quantity).toBe('0.12000000000000000001');
    expect(String(vi.mocked(fetch).mock.calls[0][0])).toContain('before_revision=0');
    vi.stubGlobal('fetch', vi.fn(async () => Response.json({ entries: [{ ...summary, created_revision: 6 }], as_of_revision: 11, next_before_revision: 6, limit: 20 })));
    await expect(history.sessions('secret', 'tenant-a', 'account-a', 5)).rejects.toMatchObject({ status: 502, code: 'invalid_response' });
  });

  it('pins timeline and report requests to the selected event revision and retains exact money strings', async () => {
    const fetchMock = vi.fn(async (_input: RequestInfo | URL) => Response.json(report)); vi.stubGlobal('fetch', fetchMock);
    const got = await history.report('secret', 'tenant-a', 'account-a', 'plan-1', 9);
    expect(got.marked_cost_estimate_quote).toBe('123.4499999999999999999');
    expect(String(fetchMock.mock.calls[0][0])).toContain('as_of_revision=9');
    fetchMock.mockResolvedValueOnce(Response.json({ entries: [{ revision: 7, kind: 'fill', created_at: '2026-10-23T10:00:30Z' }], as_of_revision: 9, next_after_revision: 7, limit: 100 }));
    const page = await history.timeline('secret', 'tenant-a', 'account-a', 'plan-1', 0, 9);
    expect(page.entries[0].revision).toBe(7);
    expect(String(fetchMock.mock.calls[1][0])).toContain('as_of_revision=9');
  });

  it('rejects a report returned for a different account scope', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Response.json(report)));
    await expect(history.report('secret', 'other-tenant', 'account-a', 'plan-1')).rejects.toBeInstanceOf(ApiError);
  });

  it('rejects missing or malformed financial fields that the report view renders', async () => {
    for (const mutate of [
      (value: any) => { delete value.plan.filled; },
      (value: any) => { value.plan.remaining = 'NaN'; },
      (value: any) => { value.plan.average_price = null; },
      (value: any) => { value.plan.spec.quote_budget = '1e4'; },
      (value: any) => { value.plan.id = 'another-plan'; },
      (value: any) => { value.plan.revision = 10; },
    ]) {
      const malformed = structuredClone(report); mutate(malformed);
      vi.stubGlobal('fetch', vi.fn(async () => Response.json(malformed)));
      await expect(history.report('secret', 'tenant-a', 'account-a', 'plan-1')).rejects.toMatchObject({ status: 502, code: 'invalid_response' });
    }
  });
});

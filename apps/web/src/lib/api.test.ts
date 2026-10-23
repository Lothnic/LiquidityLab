import { afterEach, describe, expect, it, vi } from 'vitest';
import { api } from './api';

const summary = { tenant_id: 't', account_id: 'a', profile: 'paper', data_source: 'synthetic', revision: 7, authority_revision: 3, ready: true, recovery_required: false };
const accountDTO = {
  ...summary, base_currency: 'BTC', quote_currency: 'USD', clock_discontinuity: false,
  market: { valid: true, snapshot_id: 's', instrument_revision: 'i', best_bid: '10', best_ask: '11' },
  rules: { symbol: 'BTC/USD', tick_size: '0.01', lot_size: '0.001', min_quantity: '0.001', min_notional: '0.01' },
  fees: { maker: '0.001', taker: '0.002', currency: 'quote', source: 'synthetic-assumption' },
  balances: [{ currency: 'BTC', total: '2', free: '2', locked: '0' }], reserved: [{ currency: 'BTC', amount: '0' }],
  plan: null, child: null, fills: [], fill_count: 0, fills_truncated: false,
};

afterEach(() => vi.unstubAllGlobals());

describe('API success response validation', () => {
  it.each([
    ['truncated JSON receipt', '{'],
    ['empty receipt object', '{}'],
    ['unsafe revision', '{"revision":9007199254740992,"duplicate":false}'],
    ['missing duplicate flag', '{"revision":8}'],
  ])('classifies %s as an uncertain invalid response', async (_label, body) => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(body, { status: 200 })));
    await expect(api.command('token', 't', 'a', { id: 'cmd-1', expected_revision: 7, kind: 'pause' }))
      .rejects.toMatchObject({ status: 502, code: 'invalid_response' });
  });

  it('accepts a valid command receipt', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Response.json({ revision: 8, duplicate: false })));
    await expect(api.command('token', 't', 'a', { id: 'cmd-1', expected_revision: 7, kind: 'pause' }))
      .resolves.toEqual({ revision: 8, duplicate: false });
  });

  it('rejects an account DTO for a different scope or without critical status fields', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Response.json({
      tenant_id: 't', account_id: 'other', revision: 7, authority_revision: 3, ready: true, recovery_required: false,
      profile: 'paper', market: { valid: true },
    })));
    await expect(api.account('token', 't', 'a')).rejects.toMatchObject({ status: 502, code: 'invalid_response' });
  });

  it('validates every account summary in the accounts list', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Response.json({ accounts: [summary, { ...summary, revision: '7' }] })));
    await expect(api.accounts('token')).rejects.toMatchObject({ status: 502, code: 'invalid_response' });
    vi.stubGlobal('fetch', vi.fn(async () => Response.json({ accounts: [summary] })));
    await expect(api.accounts('token')).resolves.toEqual({ accounts: [summary] });
  });

  it.each([
    ['missing balances', (({ balances: _balances, ...rest }) => rest)(accountDTO)],
    ['non-decimal balance amount', { ...accountDTO, balances: [{ currency: 'BTC', total: 'NaN', free: '2', locked: '0' }] }],
    ['malformed plan spec', { ...accountDTO, plan: { id: 'p', revision: 1, status: 'active', started: true,
      start: '2026-01-01T00:00:00Z', deadline: '2026-01-01T00:01:00Z', spec: {}, policy: 'scheduled', filled: '0', remaining: '1',
      average_price: '0', arrival_mid: '0', benchmark_valid: false, executed_cost_quote: null, fees_base: '0', fees_quote: '0' } }],
    ['malformed child intent', { ...accountDTO, child: { id: 'c', intent: {}, remaining: '1', state: 'open', known_final: false, cancel_requested: false } }],
  ])('rejects account payload with %s', async (_label, payload) => {
    vi.stubGlobal('fetch', vi.fn(async () => Response.json(payload)));
    await expect(api.account('token', 't', 'a')).rejects.toMatchObject({ status: 502, code: 'invalid_response' });
  });

  it('accepts a complete account DTO with internal precision amounts', async () => {
    const payload = { ...accountDTO, balances: [{ currency: 'BTC', total: `0.${'1'.repeat(72)}`, free: '0', locked: '0' }] };
    vi.stubGlobal('fetch', vi.fn(async () => Response.json(payload)));
    await expect(api.account('token', 't', 'a')).resolves.toEqual(payload);
  });

  it.each([
    ['missing entries', { next_after_revision: 0, limit: 100 }],
    ['malformed entry timestamp', { entries: [{ revision: 1, kind: 'create', created_at: 'yesterday' }], next_after_revision: 1, limit: 100 }],
    ['non-advancing revision', { entries: [{ revision: 0, kind: 'create', created_at: '2026-01-01T00:00:00Z' }], next_after_revision: 0, limit: 100 }],
    ['inconsistent next cursor', { entries: [], next_after_revision: 1, limit: 100 }],
  ])('rejects journal payload with %s', async (_label, payload) => {
    vi.stubGlobal('fetch', vi.fn(async () => Response.json(payload)));
    await expect(api.journal('token', 't', 'a', 0)).rejects.toMatchObject({ status: 502, code: 'invalid_response' });
  });

  it('accepts a valid empty journal page', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Response.json({ entries: [], next_after_revision: 0, limit: 100 })));
    await expect(api.journal('token', 't', 'a', 0)).resolves.toEqual({ entries: [], next_after_revision: 0, limit: 100 });
  });

  it('rejects malformed preview readiness or revision fields', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Response.json({ revision: '7', ready: true, reason: 'ready', profile: 'paper', market: { valid: true }, intent: null })));
    await expect(api.preview('token', 't', 'a', {
      symbol: 'BTC/USD', side: 'buy', quantity: '1', limit_price: '10', deadline_ns: 1,
      max_child_quantity: '1', quote_budget: '10', base_balance: '1',
    }, 'scheduled')).rejects.toMatchObject({ status: 502, code: 'invalid_response' });
  });
});

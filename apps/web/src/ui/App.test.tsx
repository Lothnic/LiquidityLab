import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, type Account, type AccountSummary, type Preview } from '../lib/api';
import { App } from './App';

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>();
  return { ...actual, api: {
    ...actual.api,
    accounts: vi.fn(), account: vi.fn(), preview: vi.fn(), command: vi.fn(), journal: vi.fn(),
  } };
});

const accountSummary = (accountID: string): AccountSummary => ({
  tenant_id: 'tenant', account_id: accountID, profile: 'paper', data_source: 'synthetic',
  revision: 10, authority_revision: 4, ready: true, recovery_required: false,
});
const accountDTO = (accountID = 'account-a', revision = 10, authorityRevision = 4): Account => ({
  ...accountSummary(accountID), revision, authority_revision: authorityRevision,
  base_currency: 'BTC', quote_currency: 'USD', clock_discontinuity: false,
  market: { valid: true, snapshot_id: `snapshot-${revision}`, instrument_revision: 'rules-1', best_bid: '100', best_ask: '101' },
  rules: { symbol: 'BTC/USD', tick_size: '0.01', lot_size: '0.001', min_quantity: '0.001', min_notional: '0.01' },
  fees: { maker: '0.001', taker: '0.002', currency: 'quote', source: 'synthetic-assumption' },
  balances: [
    { currency: 'BTC', total: '2', free: '2', locked: '0' },
    { currency: 'USD', total: '5000', free: '5000', locked: '0' },
  ],
  reserved: [{ currency: 'BTC', amount: '0' }, { currency: 'USD', amount: '0' }],
  plan: null, child: null, fills: [], fill_count: 0, fills_truncated: false,
});
const previewDTO = (revision = 10, authorityRevision = 4): Preview => ({
  revision, authority_revision: authorityRevision, profile: 'paper', observed_at: '2026-01-01T00:00:00Z',
  market: { valid: true, snapshot_id: 'snapshot-10', instrument_revision: 'rules-1', best_bid: '100', best_ask: '101' },
  policy: 'scheduled', ready: true, reason: 'ready', spread: '1', visible_bids: '1', visible_asks: '1',
  intent: { side: 'buy', quantity: '0.01234567890123456789', price: '100.12345678901234567890', passive: true },
  reservation: { currency: 'USD', amount: '1.24' },
  estimated_maker_fee: { currency: 'USD', amount: '0.001' },
  estimated_taker_fee: { currency: 'USD', amount: '0.002' }, fee_source: 'synthetic-assumption', completion_guaranteed: false,
});

let root: Root;
let container: HTMLDivElement;
const flush = async () => { await Promise.resolve(); await Promise.resolve(); };
async function mount() {
  container = document.createElement('div');
  document.body.append(container);
  root = createRoot(container);
  await act(async () => { root.render(<App />); await flush(); });
}
async function unmount() {
  if (root) await act(async () => { root.unmount(); await flush(); });
  container?.remove();
}
function input(label: string) {
  const element = container.querySelector<HTMLInputElement>(`#${label}`);
  if (!element) throw new Error(`missing input ${label}`);
  return element;
}
function setInput(element: HTMLInputElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')?.set;
  setter?.call(element, value);
  element.dispatchEvent(new Event('input', { bubbles: true }));
}
function button(text: string) {
  const element = [...container.querySelectorAll('button')].find((candidate) => candidate.textContent?.includes(text));
  if (!element) throw new Error(`missing button ${text}`);
  return element;
}
async function connect(summaries = [accountSummary('account-a')]) {
  vi.mocked(api.accounts).mockResolvedValue({ accounts: summaries });
  await act(async () => {
    setInput(input('token'), 'operator-token');
    input('token').closest('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await flush();
  });
}
async function submitPreview() {
  await act(async () => {
    setInput(input('base-quantity'), '0.01234567890123456789');
    setInput(input('max-buy-price'), '100.12345678901234567890');
    setInput(input('max-child-size'), '0.00123456789012345678');
    setInput(input('quote-budget'), '2000.000000000000000001');
    setInput(input('base-budget'), '1.00000000000000000001');
    container.querySelector('.plan-form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await flush();
  });
}

beforeEach(() => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
  vi.mocked(api.accounts).mockReset();
  vi.mocked(api.account).mockReset().mockImplementation(async (_token, _tenant, accountID) => accountDTO(accountID));
  vi.mocked(api.preview).mockReset().mockResolvedValue(previewDTO());
  vi.mocked(api.command).mockReset().mockResolvedValue({ revision: 11, duplicate: false });
  vi.mocked(api.journal).mockReset().mockResolvedValue({ entries: [], next_after_revision: 0, limit: 100 });
});
afterEach(async () => { await unmount(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe('trader desk account and preview workflow', () => {
  it('previews and creates with exact decimal strings and retries an unknown command unchanged', async () => {
    vi.mocked(api.command).mockRejectedValueOnce(new Error('connection interrupted'))
      .mockResolvedValueOnce({ revision: 11, duplicate: true });
    await mount(); await connect(); await submitPreview();

    expect(api.preview).toHaveBeenCalledWith('operator-token', 'tenant', 'account-a', expect.objectContaining({
      quantity: '0.01234567890123456789', limit_price: '100.12345678901234567890',
      max_child_quantity: '0.00123456789012345678', quote_budget: '2000.000000000000000001',
      base_balance: '1.00000000000000000001',
    }), 'scheduled', expect.any(AbortSignal));
    expect(container.textContent).toContain('FIRST CHILD ESTIMATE');
    await act(async () => { button('Create paused plan').click(); await flush(); });
    expect(container.textContent).toContain('COMMAND OUTCOME UNKNOWN');

    const original = vi.mocked(api.command).mock.calls[0][3];
    expect(original).toMatchObject({ kind: 'create', expected_revision: 10, expected_authority_revision: 4,
      plan: { quantity: '0.01234567890123456789', limit_price: '100.12345678901234567890' } });
    await act(async () => { button('Retry unchanged command').click(); await flush(); });
    expect(api.command).toHaveBeenCalledTimes(2);
    expect(vi.mocked(api.command).mock.calls[1][3]).toEqual(original);
  });

  it('keeps a preview across a durable revision refresh with unchanged authority', async () => {
    await mount(); await connect(); await submitPreview();
    vi.mocked(api.account).mockResolvedValue(accountDTO('account-a', 11, 4));
    await act(async () => { button('REFRESH').click(); await flush(); });
    expect(container.textContent).toContain('FIRST CHILD ESTIMATE');
    expect(container.querySelector('.create-button')).not.toBeNull();
    await act(async () => { button('Create paused plan').click(); await flush(); });
    expect(vi.mocked(api.command).mock.calls[0][3]).toMatchObject({ expected_revision: 10, expected_authority_revision: 4 });
  });

  it('marks a preview stale after account authority changes', async () => {
    await mount(); await connect(); await submitPreview();
    vi.mocked(api.account).mockResolvedValue(accountDTO('account-a', 11, 5));
    await act(async () => { button('REFRESH').click(); await flush(); });
    expect(container.textContent).toContain('Account state changed. Run preview again.');
    expect(container.querySelector('.create-button')).toBeNull();
    expect(api.command).not.toHaveBeenCalled();
  });

  it('allows a ready paused plan when the current preview has no immediate child estimate', async () => {
    vi.mocked(api.preview).mockResolvedValue({ ...previewDTO(), reason: 'below_minimum_or_no_free_balance', intent: null,
      reservation: null, estimated_maker_fee: null, estimated_taker_fee: null });
    await mount(); await connect(); await submitPreview();
    expect(container.textContent).toContain('BELOW MINIMUM OR NO FREE BALANCE');
    expect(container.querySelector('.create-button')).not.toBeNull();
    await act(async () => { button('Create paused plan').click(); await flush(); });
    expect(vi.mocked(api.command).mock.calls[0][3]).toMatchObject({ kind: 'create', expected_revision: 10,
      expected_authority_revision: 4, plan: { quantity: '0.01234567890123456789', limit_price: '100.12345678901234567890' } });
  });

  it.each(['market_invalid', 'recovery_required'])('does not offer create for a not-ready %s preview', async (reason) => {
    vi.mocked(api.preview).mockResolvedValue({ ...previewDTO(), ready: false, reason });
    await mount(); await connect(); await submitPreview();
    expect(container.querySelector('.preview-card')).not.toBeNull();
    expect(container.querySelector('.create-button')).toBeNull();
    expect(api.command).not.toHaveBeenCalled();
  });

  it('removes a preview when any plan field changes', async () => {
    await mount(); await connect(); await submitPreview();
    expect(container.querySelector('.preview-card')).not.toBeNull();
    await act(async () => {
      setInput(input('base-quantity'), '0.02');
      await flush();
    });
    expect(container.querySelector('.preview-card')).toBeNull();
    expect(api.command).not.toHaveBeenCalled();
  });

  it('ignores an account response that arrives after switching scopes', async () => {
    let resolveA!: (value: Account) => void;
    vi.mocked(api.account).mockImplementation((_token, _tenant, accountID) => accountID === 'account-a'
      ? new Promise((resolve) => { resolveA = resolve; }) : Promise.resolve(accountDTO('account-b', 22, 8)));
    await mount(); await connect([accountSummary('account-a'), accountSummary('account-b')]);

    await act(async () => {
      const selector = container.querySelector<HTMLSelectElement>('select[aria-label="Account"]')!;
      selector.value = 'tenant/account-b'; selector.dispatchEvent(new Event('change', { bubbles: true }));
      await flush();
    });
    expect(container.textContent).toContain('#22');
    await act(async () => { resolveA(accountDTO('account-a', 10, 4)); await flush(); });
    expect(container.textContent).toContain('#22');
    expect(container.textContent).not.toContain('#10');
  });
});

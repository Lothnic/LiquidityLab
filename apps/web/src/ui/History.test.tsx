import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, describe, expect, it, vi } from 'vitest';
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
import { History } from './History';

const summary = (id: string) => ({ plan_id: id, created_revision: 4, revision: 9, created_at: '2026-10-23T10:00:00Z', updated_at: '2026-10-23T10:01:00Z', profile: 'paper', data_source: 'synthetic', base_currency: 'BTC', quote_currency: 'USD', status: 'completed', side: 'buy', symbol: 'BTC/USD', quantity: '0.1', limit_price: '101', policy: 'scheduled' });
const report = (id: string, revision = 9) => ({ ...summary(id), revision, tenant_id: 'tenant-a', account_id: 'account-a', version: 'session-report-v1', as_of_revision: revision,
  plan: { id, revision, status: 'completed', started: true, start: '2026-10-23T10:00:00Z', deadline: '2026-10-23T10:10:00Z', spec: { side: 'buy', quantity: '0.1', limit_price: '101', deadline_ns: 1792749600000000000, max_child_quantity: '0.1', quote_budget: '10.1', base_balance: '0', symbol: 'BTC/USD' }, policy: 'scheduled', filled: '0.1', remaining: '0', average_price: '101', arrival_mid: '100', benchmark_valid: true, executed_cost_quote: '10.1', fees_base: '0', fees_quote: '0' },
  child: null, fills: [], completion_percent: '100', known_final: true, recovery_required: false, deadline_result: 'completed', market: { valid: true, snapshot_id: 's', instrument_revision: 'i', best_bid: '100', best_ask: '101' }, residual_reference_mid: null, residual_reference_revision: 9, residual_reference_kind: 'none', residual_opportunity_quote: null, marked_cost_estimate_quote: null });
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>((r) => { resolve = r; }); return { promise, resolve }; }
let root: Root | undefined; let host: HTMLDivElement | undefined;
afterEach(async () => { if (root) await act(async () => root?.unmount()); root = undefined; host?.remove(); host = undefined; vi.unstubAllGlobals(); });

describe('History scope isolation', () => {
  it('reports a timed out history request clearly', async () => {
    host = document.createElement('div'); document.body.append(host); root = createRoot(host);
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new DOMException('Timed out', 'TimeoutError'))));
    await act(async () => { root?.render(<History token="secret" tenant="tenant-a" account="account-a" />); });
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
    expect(host.textContent).toContain('History unavailable: request timed out');
  });

  it('paginates more than 400 fills with at most 200 rendered rows', async () => {
    host = document.createElement('div'); document.body.append(host); root = createRoot(host);
    const manyFills = Array.from({ length: 450 }, (_, index) => ({ execution_id: `fill-${index}`, order_id: `order-${index}`, side: 'buy', price: '101', quantity: `0.${String(index).padStart(3, '0')}`, fee: '0', fee_currency: 'USD', time_ns: index, maker: true }));
    const fullReport = { ...report('plan-a'), fills: manyFills };
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes('/sessions?')) return Promise.resolve(Response.json({ entries: [summary('plan-a')], as_of_revision: 10, next_before_revision: 4, limit: 20 }));
      if (url.includes('/plan-a?')) return Promise.resolve(Response.json(fullReport));
      if (url.includes('/timeline?')) return Promise.resolve(Response.json({ entries: [], as_of_revision: 9, next_after_revision: 0, limit: 100 }));
      throw new Error(url);
    }));
    await act(async () => { root?.render(<History token="secret" tenant="tenant-a" account="account-a" />); });
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
    await act(async () => { host?.querySelector('.history-item')?.dispatchEvent(new MouseEvent('click', { bubbles: true })); });
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
    const rows = () => host?.querySelectorAll('.history-fill-table tbody tr') ?? [];
    expect(rows()).toHaveLength(200);
    expect(rows()[0].textContent).toContain('0.449');
    expect(host.querySelector('.history-fill-pagination')?.textContent).toContain('Showing 251–450 of 450 fills');
    await act(async () => { host?.querySelector('[aria-label="Show older fills"]')?.dispatchEvent(new MouseEvent('click', { bubbles: true })); });
    expect(rows()).toHaveLength(200);
    expect(rows()[0].textContent).toContain('0.249');
    expect(host.querySelector('.history-fill-pagination')?.textContent).toContain('Showing 51–250 of 450 fills');
    await act(async () => { host?.querySelector('[aria-label="Show older fills"]')?.dispatchEvent(new MouseEvent('click', { bubbles: true })); });
    expect(rows()).toHaveLength(50);
    expect(rows()[0].textContent).toContain('0.049');
    expect(host.querySelector('.history-fill-pagination')?.textContent).toContain('Showing 1–50 of 450 fills');
    await act(async () => { host?.querySelector('[aria-label="Show newer fills"]')?.dispatchEvent(new MouseEvent('click', { bubbles: true })); });
    expect(rows()).toHaveLength(200);
    expect(host.querySelector('.history-fill-pagination')?.textContent).toContain('Showing 51–250 of 450 fills');
    expect(fullReport.fills).toHaveLength(450);
  });

  it('keeps the latest report attribution while a focused event report is pending', async () => {
    host = document.createElement('div'); document.body.append(host); root = createRoot(host);
    const pending = deferred<Response>();
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes('/sessions?')) return Promise.resolve(Response.json({ entries: [summary('plan-a')], as_of_revision: 10, next_before_revision: 4, limit: 20 }));
      if (url.includes('/timeline?')) return Promise.resolve(Response.json({ entries: [{ revision: 7, kind: 'fill', created_at: '2026-10-23T10:00:30Z' }], as_of_revision: 9, next_after_revision: 7, limit: 100 }));
      if (url.includes('as_of_revision=7')) return pending.promise;
      if (url.includes('/plan-a?')) return Promise.resolve(Response.json(report('plan-a')));
      throw new Error(url);
    }));
    await act(async () => { root?.render(<History token="secret" tenant="tenant-a" account="account-a" />); });
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
    await act(async () => { host?.querySelector('.history-item')?.dispatchEvent(new MouseEvent('click', { bubbles: true })); });
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
    await act(async () => { host?.querySelector('.history-event')?.dispatchEvent(new MouseEvent('click', { bubbles: true })); });
    expect(host.textContent).toContain('Saved revision 9');
    expect(host.textContent).toContain('Loading stored report at r7');
    expect(host.textContent).not.toContain('Inspecting saved revision');
    await act(async () => { pending.resolve(Response.json(report('plan-a', 7))); await pending.promise; });
    expect(host.textContent).toContain('Inspecting saved revision');
  });

  it('ignores a late list response after the account scope changes', async () => {
    host = document.createElement('div'); document.body.append(host); root = createRoot(host);
    const first = deferred<Response>();
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL) => String(input).includes('/tenant-a/') ? first.promise : Promise.resolve(Response.json({ entries: [{ ...summary('plan-b'), symbol: 'ETH/USD' }], as_of_revision: 10, next_before_revision: 4, limit: 20 }))));
    await act(async () => { root?.render(<History token="secret" tenant="tenant-a" account="account-a" />); });
    await act(async () => { root?.render(<History token="secret" tenant="tenant-b" account="account-b" />); });
    await act(async () => { first.resolve(Response.json({ entries: [summary('stale-plan')], as_of_revision: 9, next_before_revision: 4, limit: 20 })); await first.promise; });
    expect(host.textContent).toContain('ETH/USD');
    expect(host.textContent).not.toContain('stale-plan');
  });

  it('ignores an old selected-plan report after selecting another plan', async () => {
    host = document.createElement('div'); document.body.append(host); root = createRoot(host);
    const old = deferred<Response>();
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes('/sessions?')) return Promise.resolve(Response.json({ entries: [summary('plan-a'), { ...summary('plan-b'), created_revision: 3 }], as_of_revision: 10, next_before_revision: 3, limit: 20 }));
      if (url.includes('/plan-a?')) return old.promise;
      if (url.includes('/plan-b?')) return Promise.resolve(Response.json(report('plan-b')));
      if (url.includes('/timeline?')) return Promise.resolve(Response.json({ entries: [], as_of_revision: 9, next_after_revision: 0, limit: 100 }));
      throw new Error(url);
    }));
    await act(async () => { root?.render(<History token="secret" tenant="tenant-a" account="account-a" />); });
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
    await act(async () => { host?.querySelectorAll('.history-item')[0].dispatchEvent(new MouseEvent('click', { bubbles: true })); });
    await act(async () => { host?.querySelectorAll('.history-item')[1].dispatchEvent(new MouseEvent('click', { bubbles: true })); });
    await act(async () => { old.resolve(Response.json(report('plan-a'))); await old.promise; });
    expect(host.querySelectorAll('.history-item')[1].classList.contains('selected')).toBe(true);
    expect(vi.mocked(fetch).mock.calls.some(([input]) => String(input).includes('/plan-b?'))).toBe(true);
    expect(host.querySelector('.history-report-title h3')?.textContent).toContain('BTC/USD');
  });

  it('ignores a late focused report after returning to the latest revision', async () => {
    host = document.createElement('div'); document.body.append(host); root = createRoot(host);
    const oldFocus = deferred<Response>();
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes('/sessions?')) return Promise.resolve(Response.json({ entries: [summary('plan-a')], as_of_revision: 10, next_before_revision: 4, limit: 20 }));
      if (url.includes('/timeline?')) return Promise.resolve(Response.json({ entries: [{ revision: 7, kind: 'fill', created_at: '2026-10-23T10:00:30Z' }], as_of_revision: 9, next_after_revision: 7, limit: 100 }));
      if (url.includes('as_of_revision=7')) return oldFocus.promise;
      if (url.includes('/plan-a?')) return Promise.resolve(Response.json(report('plan-a')));
      throw new Error(url);
    }));
    await act(async () => { root?.render(<History token="secret" tenant="tenant-a" account="account-a" />); });
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
    await act(async () => { host?.querySelector('.history-item')?.dispatchEvent(new MouseEvent('click', { bubbles: true })); });
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
    await act(async () => { host?.querySelector('.history-event')?.dispatchEvent(new MouseEvent('click', { bubbles: true })); });
    await act(async () => { host?.querySelectorAll('.load-more')[0]?.dispatchEvent(new MouseEvent('click', { bubbles: true })); });
    await act(async () => { oldFocus.resolve(Response.json(report('plan-a', 7))); await oldFocus.promise; });
    expect(host.textContent).toContain('revision 9');
    expect(host.textContent).not.toContain('revision 7');
  });

  it('invalidates a pending detail when refreshing the pinned session list', async () => {
    host = document.createElement('div'); document.body.append(host); root = createRoot(host);
    const oldReport = deferred<Response>();
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes('/sessions?')) return Promise.resolve(Response.json({ entries: [summary('plan-a')], as_of_revision: 10, next_before_revision: 4, limit: 20 }));
      if (url.includes('/plan-a?')) return oldReport.promise;
      throw new Error(url);
    }));
    await act(async () => { root?.render(<History token="secret" tenant="tenant-a" account="account-a" />); });
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
    await act(async () => { host?.querySelector('.history-item')?.dispatchEvent(new MouseEvent('click', { bubbles: true })); });
    await act(async () => { host?.querySelector('.history-actions button')?.dispatchEvent(new MouseEvent('click', { bubbles: true })); });
    await act(async () => { oldReport.resolve(Response.json(report('plan-a'))); await oldReport.promise; });
    expect(host.querySelector('.history-report')).toBeNull();
    expect(host.textContent).toContain('Select a row to review its fills, fees, completion status, and saved report.');
  });
});

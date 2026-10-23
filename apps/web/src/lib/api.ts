export type AccountSummary = {
  tenant_id: string;
  account_id: string;
  profile: 'paper' | 'live' | 'replay' | string;
  data_source: 'manual' | 'synthetic' | 'recorded' | 'kraken-public' | string;
  revision: number;
  authority_revision: number;
  ready: boolean;
  recovery_required: boolean;
  plan_status?: string;
};

export type Balance = { currency: string; total: string; free: string; locked: string };
export type Fill = {
  execution_id: string;
  order_id: string;
  side: string;
  price: string;
  quantity: string;
  fee: string;
  fee_currency: string;
  time_ns: number;
  maker: boolean;
};
export type Account = AccountSummary & {
  base_currency: string;
  quote_currency: string;
  clock_discontinuity: boolean;
  market: { valid: boolean; snapshot_id: string; instrument_revision: string; best_bid: string; best_ask: string };
  rules: { symbol: string; tick_size: string; lot_size: string; min_quantity: string; min_notional: string };
  fees: { maker: string; taker: string; currency: string; source: string };
  balances: Balance[];
  reserved: { currency: string; amount: string }[];
  plan: null | {
    id: string; revision: number; status: string; started: boolean; start: string; deadline: string;
    spec: { side: string; quantity: string; limit_price: string; deadline_ns: number; max_child_quantity: string; quote_budget: string; base_balance: string; symbol: string };
    policy: string; filled: string; remaining: string; average_price: string; arrival_mid: string;
    benchmark_valid: boolean; executed_cost_quote: string | null; fees_base: string; fees_quote: string;
  };
  child: null | { id: string; intent: { side: string; quantity: string; price: string; passive: boolean }; remaining: string; state: string; known_final: boolean; cancel_requested: boolean };
  fills: Fill[];
  fill_count: number;
  fills_truncated: boolean;
};
export type Preview = {
  revision: number; authority_revision: number; profile: string; observed_at: string; market: Account['market']; policy: string;
  ready: boolean; reason: string; spread: string; visible_bids: string; visible_asks: string;
  intent: null | { side: string; quantity: string; price: string; passive: boolean };
  reservation: null | { currency: string; amount: string };
  estimated_maker_fee: null | { currency: string; amount: string };
  estimated_taker_fee: null | { currency: string; amount: string };
  fee_source: string; completion_guaranteed: boolean;
};
export type PlanInput = {
  symbol: string; side: 'buy' | 'sell'; quantity: string; limit_price: string;
  deadline_ns: number; max_child_quantity: string; quote_budget: string; base_balance: string;
};
export type Command = {
  id: string; expected_revision: number; expected_authority_revision?: number; kind: 'create' | 'start' | 'pause' | 'resume' | 'cancel';
  plan_id?: string; plan?: PlanInput; policy?: string;
};
export type JournalEntry = { revision: number; kind: string; created_at: string };

export class ApiError extends Error {
  constructor(readonly status: number, readonly code: string) { super(code); }
}

async function request<T>(path: string, token: string, init: RequestInit = {}, signal?: AbortSignal): Promise<T> {
  const response = await fetch(path, {
    ...init,
    signal,
    headers: { Authorization: `Bearer ${token}`, ...(init.body ? { 'Content-Type': 'application/json' } : {}), ...init.headers },
  });
  let payload: unknown;
  try {
    payload = await response.json();
  } catch {
    if (response.ok) throw new ApiError(502, 'invalid_response');
    payload = {};
  }
  if (!response.ok) {
    const code = typeof payload === 'object' && payload !== null && 'error' in payload && typeof payload.error === 'string'
      ? payload.error : 'request_failed';
    throw new ApiError(response.status, code);
  }
  return payload as T;
}

function record(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
function validRevision(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
}
function validInt64Number(value: unknown): value is number {
  return typeof value === 'number' && Number.isInteger(value) && Math.abs(value) <= 9_223_372_036_854_775_807;
}
function validString(value: unknown): value is string { return typeof value === 'string'; }
function validBoolean(value: unknown): value is boolean { return typeof value === 'boolean'; }
// Go's Decimal marshals exact amounts as plain decimal strings. Internal values
// may be wider than accepted inputs (512 coefficient digits, scale 144).
function validDecimal(value: unknown): value is string {
  return typeof value === 'string' && value.length <= 659 && /^[+-]?\d+(?:\.\d{1,144})?$/.test(value) &&
    value.replace(/^[+-]?/, '').replace('.', '').length <= 512;
}
function validTime(value: unknown): value is string {
  return typeof value === 'string' && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)$/.test(value) && Number.isFinite(Date.parse(value));
}
function validSummary(value: unknown): boolean {
  return record(value) && validString(value.tenant_id) && validString(value.account_id) && validString(value.profile) &&
    validString(value.data_source) && validRevision(value.revision) && validRevision(value.authority_revision) && value.authority_revision <= value.revision && validBoolean(value.ready) &&
    validBoolean(value.recovery_required) && (value.plan_status === undefined || validString(value.plan_status));
}
function validMarket(value: unknown): boolean {
  return record(value) && validBoolean(value.valid) && validString(value.snapshot_id) &&
    validString(value.instrument_revision) && validDecimal(value.best_bid) && validDecimal(value.best_ask);
}
function validIntent(value: unknown): boolean {
  return record(value) && validString(value.side) && validDecimal(value.quantity) && validDecimal(value.price) && validBoolean(value.passive);
}
function validArray(value: unknown, item: (entry: unknown) => boolean): boolean {
  return Array.isArray(value) && value.every(item);
}
function validAccount(value: unknown, tenant?: string, account?: string): value is Account {
  if (!record(value) || !validSummary(value) || (tenant !== undefined && value.tenant_id !== tenant) || (account !== undefined && value.account_id !== account)) return false;
  return validString(value.base_currency) && validString(value.quote_currency) && validBoolean(value.clock_discontinuity) &&
    validMarket(value.market) &&
    record(value.rules) && validString(value.rules.symbol) && validDecimal(value.rules.tick_size) && validDecimal(value.rules.lot_size) &&
    validDecimal(value.rules.min_quantity) && validDecimal(value.rules.min_notional) &&
    record(value.fees) && validDecimal(value.fees.maker) && validDecimal(value.fees.taker) && validString(value.fees.currency) && validString(value.fees.source) &&
    validArray(value.balances, entry => record(entry) && validString(entry.currency) && validDecimal(entry.total) && validDecimal(entry.free) && validDecimal(entry.locked)) &&
    validArray(value.reserved, entry => record(entry) && validString(entry.currency) && validDecimal(entry.amount)) &&
    (value.plan === null || (record(value.plan) && validString(value.plan.id) && validRevision(value.plan.revision) &&
      validString(value.plan.status) && validBoolean(value.plan.started) && validTime(value.plan.start) && validTime(value.plan.deadline) &&
      record(value.plan.spec) && validString(value.plan.spec.side) && validDecimal(value.plan.spec.quantity) &&
      validDecimal(value.plan.spec.limit_price) && validInt64Number(value.plan.spec.deadline_ns) &&
      validDecimal(value.plan.spec.max_child_quantity) && validDecimal(value.plan.spec.quote_budget) &&
      validDecimal(value.plan.spec.base_balance) && validString(value.plan.spec.symbol) && validString(value.plan.policy) &&
      validDecimal(value.plan.filled) && validDecimal(value.plan.remaining) && validDecimal(value.plan.average_price) &&
      validDecimal(value.plan.arrival_mid) && validBoolean(value.plan.benchmark_valid) &&
      (value.plan.executed_cost_quote === null || validDecimal(value.plan.executed_cost_quote)) &&
      validDecimal(value.plan.fees_base) && validDecimal(value.plan.fees_quote))) &&
    (value.child === null || (record(value.child) && validString(value.child.id) && validIntent(value.child.intent) &&
      validDecimal(value.child.remaining) && validString(value.child.state) && validBoolean(value.child.known_final) && validBoolean(value.child.cancel_requested))) &&
    validArray(value.fills, entry => record(entry) && validString(entry.execution_id) && validString(entry.order_id) &&
      validString(entry.side) && validDecimal(entry.price) && validDecimal(entry.quantity) && validDecimal(entry.fee) &&
      validString(entry.fee_currency) && validInt64Number(entry.time_ns) && validBoolean(entry.maker)) &&
    validRevision(value.fill_count) && validBoolean(value.fills_truncated);
}
function invalidResponse(): never {
  throw new ApiError(502, 'invalid_response');
}

export const api = {
  accounts: async (token: string, signal?: AbortSignal) => {
    const value: unknown = await request('/v1/accounts', token, {}, signal);
    if (!record(value) || !validArray(value.accounts, validSummary)) invalidResponse();
    return value as { accounts: AccountSummary[] };
  },
  account: async (token: string, tenant: string, account: string, signal?: AbortSignal) => {
    const value: unknown = await request(`/v1/accounts/${encodeURIComponent(tenant)}/${encodeURIComponent(account)}`, token, {}, signal);
    if (!validAccount(value, tenant, account)) invalidResponse();
    return value as Account;
  },
  preview: async (token: string, tenant: string, account: string, plan: PlanInput, policy: string, signal?: AbortSignal) => {
    const value: unknown = await request(`/v1/accounts/${encodeURIComponent(tenant)}/${encodeURIComponent(account)}/preview`, token,
      { method: 'POST', body: JSON.stringify({ plan, policy }) }, signal);
    if (!record(value) || !validRevision(value.revision) || !validRevision(value.authority_revision) || typeof value.ready !== 'boolean' || typeof value.reason !== 'string' ||
      typeof value.profile !== 'string' || value.authority_revision > value.revision || !validMarket(value.market) ||
      !validTime(value.observed_at) || !validString(value.policy) || !validDecimal(value.spread) ||
      !validDecimal(value.visible_bids) || !validDecimal(value.visible_asks) || !validString(value.fee_source) ||
      !validBoolean(value.completion_guaranteed) || !(value.intent === null || validIntent(value.intent)) ||
      !(value.reservation === null || (record(value.reservation) && validString(value.reservation.currency) && validDecimal(value.reservation.amount))) ||
      !(value.estimated_maker_fee === null || (record(value.estimated_maker_fee) && validString(value.estimated_maker_fee.currency) && validDecimal(value.estimated_maker_fee.amount))) ||
      !(value.estimated_taker_fee === null || (record(value.estimated_taker_fee) && validString(value.estimated_taker_fee.currency) && validDecimal(value.estimated_taker_fee.amount)))) invalidResponse();
    return value as Preview;
  },
  command: async (token: string, tenant: string, account: string, command: Command, signal?: AbortSignal) => {
    const value: unknown = await request(`/v1/accounts/${encodeURIComponent(tenant)}/${encodeURIComponent(account)}/commands`, token,
      { method: 'POST', body: JSON.stringify(command) }, signal);
    if (!record(value) || !validRevision(value.revision) || typeof value.duplicate !== 'boolean') invalidResponse();
    return value as { revision: number; duplicate: boolean };
  },
  journal: async (token: string, tenant: string, account: string, after: number, signal?: AbortSignal) => {
    const value: unknown = await request(
      `/v1/accounts/${encodeURIComponent(tenant)}/${encodeURIComponent(account)}/journal?after_revision=${after}&limit=100`, token, {}, signal);
    if (!record(value) || !validArray(value.entries, entry => record(entry) && validRevision(entry.revision) && validString(entry.kind) && validTime(entry.created_at)) ||
      !validRevision(value.next_after_revision) || !validRevision(value.limit) ||
      (value.entries as JournalEntry[]).some((entry, index, entries) => entry.revision <= (index === 0 ? after : entries[index - 1].revision)) ||
      value.next_after_revision !== ((value.entries as JournalEntry[]).length ? (value.entries as JournalEntry[])[(value.entries as JournalEntry[]).length - 1].revision : after) || value.limit !== 100) invalidResponse();
    return value as { entries: JournalEntry[]; next_after_revision: number; limit: number };
  },
};

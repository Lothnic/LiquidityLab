import { ApiError, isPolicyRuntime, request, type Account, type Fill, type PolicyRuntime } from './api';
import { isTimestamp } from './time';

export type SessionSummary = {
  plan_id: string; created_revision: number; revision: number; created_at: string; updated_at: string;
  profile: string; data_source: string; base_currency: string; quote_currency: string; status: string;
  side: string; symbol: string; quantity: string; limit_price: string; policy: string; policy_sha256?: string;
};
export type SessionReport = SessionSummary & {
  policy_runtime?: PolicyRuntime;
  tenant_id: string; account_id: string;
  version: 'session-report-v1'; as_of_revision: number; plan: NonNullable<Account['plan']>;
  child: Record<string, unknown> | null; fills: Fill[]; completion_percent: string; known_final: boolean;
  recovery_required: boolean; deadline_result: string; market: { valid: boolean; snapshot_id: string; instrument_revision: string; best_bid: string; best_ask: string };
  residual_reference_mid: string | null; residual_reference_revision: number; residual_reference_kind: string;
  residual_opportunity_quote: string | null; marked_cost_estimate_quote: string | null;
};
export type SessionEvent = { revision: number; kind: string; created_at: string };
export type SessionPage = { entries: SessionSummary[]; as_of_revision: number; next_before_revision: number; limit: number };
export type TimelinePage = { entries: SessionEvent[]; as_of_revision: number; next_after_revision: number; limit: number };

const record = (x: unknown): x is Record<string, unknown> => typeof x === 'object' && x !== null && !Array.isArray(x);
const str = (x: unknown): x is string => typeof x === 'string';
const rev = (x: unknown): x is number => typeof x === 'number' && Number.isSafeInteger(x) && x >= 0;
const decimal = (x: unknown): x is string => typeof x === 'string' && x.length <= 659 && /^[+-]?\d+(?:\.\d{1,144})?$/.test(x) && x.replace(/^[+-]?/, '').replace('.', '').length <= 512;
const time = isTimestamp;
const identifier = (x: unknown): x is string => typeof x === 'string' && x.length > 0 && x.length <= 128 && x !== '.' && x !== '..' && /^[a-zA-Z0-9_.-]+$/.test(x);
const invalid = (): never => { throw new ApiError(502, 'invalid_response'); };
function summary(x: unknown): x is SessionSummary {
  return record(x) && identifier(x.plan_id) && rev(x.created_revision) && rev(x.revision) && x.created_revision <= x.revision &&
    time(x.created_at) && time(x.updated_at) && str(x.profile) && str(x.data_source) && str(x.base_currency) && str(x.quote_currency) &&
    str(x.status) && str(x.side) && str(x.symbol) && decimal(x.quantity) && decimal(x.limit_price) && str(x.policy) &&
    (x.policy_sha256 === undefined || (typeof x.policy_sha256 === 'string' && /^[a-f0-9]{64}$/.test(x.policy_sha256)));
}
function fill(x: unknown): x is Fill {
  return record(x) && str(x.execution_id) && str(x.order_id) && str(x.side) && decimal(x.price) && decimal(x.quantity) && decimal(x.fee) &&
    str(x.fee_currency) && typeof x.time_ns === 'number' && Number.isInteger(x.time_ns) && Math.abs(x.time_ns) <= 9_223_372_036_854_775_807 && typeof x.maker === 'boolean';
}
function pageValid(x: unknown, before: number, requestedLimit: number): x is SessionPage {
  if (!record(x)) return false;
  const r = x as Record<string, unknown>;
  if (!Array.isArray(r.entries) || !r.entries.every(summary) || !rev(r.as_of_revision) || !rev(r.next_before_revision) || r.limit !== requestedLimit || r.entries.length > requestedLimit) return false;
  const entries = r.entries as SessionSummary[];
  return entries.every((e, i) => e.created_revision <= (r.as_of_revision as number) && e.revision <= (r.as_of_revision as number) && (i === 0 || entries[i - 1].created_revision > e.created_revision)) &&
    r.next_before_revision === (entries.length ? entries[entries.length - 1].created_revision : before) &&
    entries.every(e => before === 0 || e.created_revision < before);
}
function reportValid(x: unknown, tenant: string, account: string, planId: string, asOf: number): x is SessionReport {
  if (!record(x) || !summary(x)) return false;
  const r = x as Record<string, unknown>;
  const m = r.market;
  const p = r.plan;
  const validPlan = record(p) && p.id === r.plan_id && rev(p.revision) && p.revision <= (r.as_of_revision as number) && str(p.status) && typeof p.started === 'boolean' &&
    time(p.start) && time(p.deadline) && record(p.spec) && str(p.spec.side) && decimal(p.spec.quantity) && decimal(p.spec.limit_price) &&
    typeof p.spec.deadline_ns === 'number' && Number.isInteger(p.spec.deadline_ns) && Math.abs(p.spec.deadline_ns) <= 9_223_372_036_854_775_807 &&
    decimal(p.spec.max_child_quantity) && decimal(p.spec.quote_budget) && decimal(p.spec.base_balance) && str(p.spec.symbol) && str(p.policy) &&
    decimal(p.filled) && decimal(p.remaining) && decimal(p.average_price) && decimal(p.arrival_mid) && typeof p.benchmark_valid === 'boolean' &&
    (p.executed_cost_quote === null || decimal(p.executed_cost_quote)) && decimal(p.fees_base) && decimal(p.fees_quote);
  const c = r.child;
  const validChild = c === null || (record(c) && identifier(c.id) && record(c.intent) && str(c.intent.side) && decimal(c.intent.quantity) &&
    decimal(c.intent.price) && typeof c.intent.passive === 'boolean' && decimal(c.remaining) && str(c.state) && typeof c.known_final === 'boolean' && typeof c.cancel_requested === 'boolean');
  return r.tenant_id === tenant && r.account_id === account && r.plan_id === planId && r.version === 'session-report-v1' && rev(r.as_of_revision) && (asOf === 0 || r.as_of_revision === asOf) &&
    validPlan && validChild && (r.policy_runtime === undefined || (isPolicyRuntime(r.policy_runtime) && r.policy_runtime.policy_sha256 === r.policy_sha256)) &&
    Array.isArray(r.fills) && r.fills.every(fill) &&
    decimal(r.completion_percent) && typeof r.known_final === 'boolean' && typeof r.recovery_required === 'boolean' && str(r.deadline_result) &&
    record(m) && typeof m.valid === 'boolean' && str(m.snapshot_id) && str(m.instrument_revision) && decimal(m.best_bid) && decimal(m.best_ask) &&
    (r.residual_reference_mid === null || decimal(r.residual_reference_mid)) && rev(r.residual_reference_revision) && str(r.residual_reference_kind) &&
    (r.residual_opportunity_quote === null || decimal(r.residual_opportunity_quote)) && (r.marked_cost_estimate_quote === null || decimal(r.marked_cost_estimate_quote));
}
function timelineValid(x: unknown, after: number, requestedAsOf: number, requestedLimit: number): x is TimelinePage {
  if (!record(x)) return false;
  const r = x as Record<string, unknown>;
  if (!Array.isArray(r.entries) || !rev(r.as_of_revision) || (requestedAsOf !== 0 && r.as_of_revision !== requestedAsOf) ||
    !rev(r.next_after_revision) || r.limit !== requestedLimit || r.entries.length > requestedLimit) return false;
  const entries = r.entries;
  if (!entries.every((e): e is SessionEvent => record(e) && rev(e.revision) && str(e.kind) && time(e.created_at))) return false;
  return entries.every((e, i) => e.revision > (i ? entries[i - 1].revision : after) && e.revision <= (r.as_of_revision as number)) &&
    r.next_after_revision === (entries.length ? entries[entries.length - 1].revision : after);
}
const base = (tenant: string, account: string) => `/v1/accounts/${encodeURIComponent(tenant)}/${encodeURIComponent(account)}/sessions`;
const timedSignal = (signal?: AbortSignal) => signal ? AbortSignal.any([signal, AbortSignal.timeout(5000)]) : AbortSignal.timeout(5000);

export const history = {
  sessions: async (token: string, tenant: string, account: string, before = 0, asOf = 0, limit = 20, signal?: AbortSignal) => {
    if (!Number.isSafeInteger(before) || before < 0 || !Number.isSafeInteger(asOf) || asOf < 0 || !Number.isInteger(limit) || limit < 1 || limit > 50) throw new ApiError(400, 'invalid_query');
    const q = new URLSearchParams({ before_revision: String(before), as_of_revision: String(asOf), limit: String(limit) });
    const value: unknown = await request<unknown>(`${base(tenant, account)}?${q}`, token, {}, timedSignal(signal));
    if (!pageValid(value, before, limit)) invalid();
    return value as SessionPage;
  },
  report: async (token: string, tenant: string, account: string, planId: string, asOf = 0, signal?: AbortSignal) => {
    if (!identifier(planId) || !Number.isSafeInteger(asOf) || asOf < 0) throw new ApiError(400, 'invalid_query');
    const value: unknown = await request<unknown>(`${base(tenant, account)}/${encodeURIComponent(planId)}?as_of_revision=${asOf}`, token, {}, timedSignal(signal));
    if (!reportValid(value, tenant, account, planId, asOf)) invalid();
    return value as SessionReport;
  },
  timeline: async (token: string, tenant: string, account: string, planId: string, after = 0, asOf = 0, limit = 100, signal?: AbortSignal) => {
    if (!identifier(planId) || !Number.isSafeInteger(after) || after < 0 || !Number.isSafeInteger(asOf) || asOf < 0 || !Number.isInteger(limit) || limit < 1 || limit > 200) throw new ApiError(400, 'invalid_query');
    const q = new URLSearchParams({ after_revision: String(after), as_of_revision: String(asOf), limit: String(limit) });
    const value: unknown = await request<unknown>(`${base(tenant, account)}/${encodeURIComponent(planId)}/timeline?${q}`, token, {}, timedSignal(signal));
    if (!timelineValid(value, after, asOf, limit)) invalid();
    return value as TimelinePage;
  },
};

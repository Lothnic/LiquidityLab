import type { PlanInput } from './api';

export type ScopeBinding = Readonly<{
  token: string;
  tenant: string;
  account: string;
  generation: number;
}>;

export function captureScope(token: string, tenant: string, account: string, generation: number): ScopeBinding {
  return Object.freeze({ token, tenant, account, generation });
}

export function scopeMatches(
  captured: ScopeBinding,
  current: { token: string; tenant: string; account: string; generation: number },
): boolean {
  return captured.token === current.token && captured.tenant === current.tenant &&
    captured.account === current.account && captured.generation === current.generation;
}

export function isLatestRequest(capturedSequence: number, currentSequence: number): boolean {
  return capturedSequence === currentSequence;
}

export function requestMayClearIndicator(capturedSequence: number, currentSequence: number, scopeIsCurrent: boolean): boolean {
  return isLatestRequest(capturedSequence, currentSequence) && scopeIsCurrent;
}

export function snapshotIsFresh(updatedAtMs: number | null, nowMs: number, maxAgeMs = 10_000): boolean {
  return updatedAtMs !== null && Number.isFinite(updatedAtMs) && Number.isFinite(nowMs) &&
    nowMs >= updatedAtMs && nowMs - updatedAtMs <= maxAgeMs;
}

export function formatElapsedNanoseconds(elapsedNs: number): string {
  const elapsedMs = Math.max(0, Math.floor(elapsedNs / 1_000_000));
  const hours = Math.floor(elapsedMs / 3_600_000);
  const minutes = Math.floor((elapsedMs % 3_600_000) / 60_000);
  const seconds = Math.floor((elapsedMs % 60_000) / 1_000);
  const milliseconds = elapsedMs % 1_000;
  return `T+${String(hours).padStart(2, '0')}:${String(minutes).padStart(2, '0')}:${String(seconds).padStart(2, '0')}.${String(milliseconds).padStart(3, '0')}`;
}

export type BoundPreview<T> = Readonly<{
  tenant: string;
  account: string;
  revision: number;
  authorityRevision: number;
  plan: Readonly<PlanInput>;
  policy: string;
  fingerprint: string;
  result: T;
}>;

export function planFingerprint(plan: PlanInput, policy: string): string {
  return JSON.stringify({ plan, policy });
}

export function bindPreview<T>(input: {
  tenant: string;
  account: string;
  revision: number;
  authorityRevision: number;
  plan: PlanInput;
  policy: string;
  result: T;
}): BoundPreview<T> {
  const plan = Object.freeze({ ...input.plan });
  return Object.freeze({
    tenant: input.tenant,
    account: input.account,
    revision: input.revision,
    authorityRevision: input.authorityRevision,
    plan,
    policy: input.policy,
    fingerprint: planFingerprint(plan, input.policy),
    result: input.result,
  });
}

export function previewMatches<T>(preview: BoundPreview<T>, input: {
  tenant: string;
  account: string;
  authorityRevision: number;
  plan: PlanInput;
  policy: string;
}): boolean {
  return preview.tenant === input.tenant && preview.account === input.account &&
    preview.authorityRevision === input.authorityRevision && preview.fingerprint === planFingerprint(input.plan, input.policy);
}

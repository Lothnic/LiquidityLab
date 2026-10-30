import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, describe, expect, it } from 'vitest';
import type { PolicyConfig, PolicyRuntime } from '../lib/api';
import { PolicyPanel } from './PolicyPanel';

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const config: PolicyConfig = {
  policy_sha256: 'a'.repeat(64), profile: 'paper', fallback: 'pause', promotion_status: 'unvalidated',
};
const runtime = (overrides: Partial<PolicyRuntime> = {}): PolicyRuntime => ({
  ...config, result: 'policy_action', reason: 'accepted', action: 4, proposed_action: 4,
  observation_snapshot_id: 'observed-snapshot', applied_snapshot_id: 'applied-snapshot',
  expires_at: '2026-10-30T10:00:00Z', resolved_at: '2026-10-30T09:59:00Z', ...overrides,
});

let root: Root | undefined;
let host: HTMLDivElement | undefined;
async function render(props: { config?: PolicyConfig; runtime?: PolicyRuntime; fresh: boolean }) {
  host = document.createElement('div'); document.body.append(host); root = createRoot(host);
  await act(async () => { root?.render(<PolicyPanel {...props} />); });
  return host;
}
afterEach(async () => {
  if (root) await act(async () => root?.unmount());
  root = undefined; host?.remove(); host = undefined;
});

describe('RL execution assistant panel', () => {
  it('shows configured, unvalidated model settings before a plan exists', async () => {
    const view = await render({ config, fresh: true });
    expect(view.textContent).toContain('Next plan: paper mode');
    expect(view.textContent).toContain('Unvalidated model');
    expect(view.textContent).toContain('Configured for the next plan');
    expect(view.textContent).toContain('Paper use only');
  });

  it('does not present the runtime action as applied while a proposal is pending', async () => {
    const view = await render({ runtime: runtime({ result: 'pending', action: 1, proposed_action: undefined, resolved_at: '0001-01-01T00:00:00Z' }), fresh: true });
    expect(view.textContent).toContain('Requesting a proposal');
    expect(view.textContent).toContain('Waiting for response');
    expect(view.textContent).toContain('No decision yet');
    expect(view.querySelectorAll('.policy-actions > div')[1].textContent).not.toContain('Wait');
  });

  it.each([
    [1, 'Wait'], [2, 'Cancel current order'], [3, 'Post small limit order'],
    [4, 'Post limit order'], [5, 'Cross spread with small limit order'], [6, 'Cross spread with limit order'],
  ] as const)('keeps semantic action %i distinct as proposed and applied labels', async (action, label) => {
    const view = await render({ runtime: runtime({ action, proposed_action: action }), fresh: true });
    const rows = [...view.querySelectorAll('.policy-actions > div')];
    expect(rows[0].textContent).toContain('Model proposal');
    expect(rows[0].textContent).toContain(label);
    expect(rows[1].textContent).toContain('Go decision');
    expect(rows[1].textContent).toContain(label);
  });

  it('shows a shadow proposal separately from the baseline action Go applied', async () => {
    const view = await render({ runtime: runtime({ profile: 'shadow', fallback: 'baseline', result: 'shadow_baseline',
      proposed_action: 3, action: 6 }), fresh: true });
    const rows = [...view.querySelectorAll('.policy-actions > div')];
    expect(view.textContent).toContain('The trained policy proposes actions; the baseline executes simulated orders.');
    expect(rows[0].textContent).toContain('Post small limit order');
    expect(rows[1].textContent).toContain('Cross spread with limit order');
    expect(rows[0].textContent).not.toContain('Cross spread with limit order');
    expect(rows[1].textContent).not.toContain('Post small limit order');
  });

  it('labels a cancelled fallback-pause decision as historical and not as the current plan state', async () => {
    const view = await render({ runtime: runtime({ result: 'fallback_pause', reason: 'control_cancel', action: 1, proposed_action: undefined }), fresh: true });
    expect(view.textContent).toContain('Model action not applied');
    expect(view.textContent).toContain('No model order applied');
    expect(view.textContent).toContain('Trader cancelled the plan');
    expect(view.textContent).not.toContain('Plan paused');
  });

  it('shows retained terminal runtime when there is no next-plan model default', async () => {
    const view = await render({ runtime: runtime({ result: 'fallback_pause', reason: 'control_cancel', action: 1, proposed_action: undefined }), fresh: true });
    expect(view.textContent).toContain('This plan: paper mode');
    expect(view.textContent).toContain('The next plan uses baseline execution without an RL model');
    expect(view.textContent).toContain('Recorded result');
    expect(view.textContent).toContain('fallback_pause');
  });

  it('marks the loaded decision snapshot stale', async () => {
    const view = await render({ runtime: runtime(), fresh: false });
    expect(view.textContent).toContain('Decision view is out of date');
    expect(view.textContent).toContain('last loaded snapshot');
  });

  it('shows when the current plan bundle differs from the next-plan default', async () => {
    const view = await render({ config: { ...config, policy_sha256: 'b'.repeat(64), profile: 'shadow', fallback: 'baseline' }, runtime: runtime(), fresh: true });
    expect(view.textContent).toContain('This plan: paper mode');
    expect(view.textContent).toContain('The next plan uses a separately configured model in shadow mode with baseline fallback');
    expect(view.textContent).toContain('Next plan model');
    expect(view.textContent).toContain('b'.repeat(64));
  });
});

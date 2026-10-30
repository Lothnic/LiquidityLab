import type { PolicyConfig, PolicyRuntime } from '../lib/api';
import { policyActionLabel } from '../lib/policy';
import './policy.css';

const outcomes: Record<PolicyRuntime['result'], { label: string; description: string }> = {
  waiting: { label: 'Waiting for the first decision', description: 'Go requests a model proposal once the plan is active and eligible.' },
  pending: { label: 'Requesting a proposal', description: 'Go is waiting for the model. A response must arrive before its deadline and pass the current execution checks.' },
  policy_action: { label: 'Model action accepted', description: 'Go recorded the model action after checking the current account and market. Execution constraints can still produce no new order; inspect orders and fills above.' },
  shadow_baseline: { label: 'Shadow proposal recorded', description: 'The model is observing. The selected baseline decides the paper orders.' },
  fallback_baseline: { label: 'Baseline fallback used', description: 'The model proposal could not be used. Go selected an action from the plan’s pinned baseline.' },
  fallback_pause: { label: 'Model action not applied', description: 'This decision placed no model order. A fallback, trader control or deadline stopped the request. Check the current plan status above.' },
};
const reasons: Record<string, string> = {
  accepted: 'Proposal accepted', shadow_policy: 'Shadow mode', worker_busy: 'Model worker is busy',
  incompatible_response: 'Response did not match the pinned model or request', invalid_response: 'Invalid model response',
  expired: 'Proposal deadline elapsed', worker_unavailable: 'Model worker unavailable',
  source_facts_pending: 'Market updates arrived while the proposal was being checked. Review the plan and resume when the account is ready.', stale_authority_or_expired: 'Account state changed or proposal expired',
  action_rejected: 'Execution checks rejected the action', owner_restart: 'Account owner restarted',
  control_pause: 'Trader paused the plan', control_cancel: 'Trader cancelled the plan',
  control_deadline: 'Plan deadline elapsed',
};

function decisionTime(value: string) {
  // Go's zero time means no decision was resolved, not a year-one UI timestamp.
  if (value.startsWith('0001-')) return null;
  return new Date(value).toLocaleString();
}

export function PolicyPanel({ config, runtime, fresh }: { config?: PolicyConfig; runtime?: PolicyRuntime; fresh: boolean }) {
  const policy = runtime ?? config;
  if (!policy) return null;
  const resolved = runtime && runtime.result !== 'waiting' && runtime.result !== 'pending';
  const outcome = runtime ? outcomes[runtime.result] : null;
  const nextDiffers = runtime && (!config || config.policy_sha256 !== runtime.policy_sha256 || config.profile !== runtime.profile || config.fallback !== runtime.fallback || config.promotion_status !== runtime.promotion_status);
  return <section className="panel policy-panel" aria-labelledby="policy-title">
    <div className="panel-heading"><h2 id="policy-title">RL execution assistant</h2><span className="subtle-tag">{runtime ? 'This plan' : 'Next plan'}: {policy.profile === 'shadow' ? 'shadow' : 'paper'} mode</span></div>
    <p className="policy-description">{policy.profile === 'shadow' ? 'The trained policy proposes actions; the baseline executes simulated orders.' : 'The trained policy proposes actions. Go applies your limits before placing simulated orders.'}</p>
    <p className="policy-validation">{policy.promotion_status === 'unvalidated' ? 'Unvalidated model' : `Model status: ${policy.promotion_status}`} · Paper use only. Trading performance has not been established.</p>
    {nextDiffers && <p className="policy-reason">The next plan uses {config ? `a separately configured model in ${config.profile} mode with ${config.fallback === 'pause' ? 'pause' : 'baseline'} fallback` : 'baseline execution without an RL model'}. This plan retains the model shown here.</p>}
    <div className="policy-outcome">
      <h3 className={!fresh || runtime?.result.startsWith('fallback_') ? 'text-warning' : ''}>{!fresh ? 'Decision view is out of date' : outcome?.label ?? 'Configured for the next plan'}</h3>
      <p>{!fresh ? 'Refresh the account to see the latest recorded decision. These values are from the last loaded snapshot.' : outcome?.description ?? 'Create and start a plan to see real model proposals and Go decisions here.'}</p>
    </div>
    {runtime && <dl className="policy-actions">
      <div><dt>Model proposal</dt><dd>{runtime.proposed_action ? policyActionLabel(runtime.proposed_action) : runtime.result === 'pending' ? 'Waiting for response' : 'No usable proposal recorded'}</dd></div>
      <div><dt>Go decision</dt><dd>{resolved ? runtime.result === 'fallback_pause' ? 'No model order applied' : policyActionLabel(runtime.action) : 'No decision yet'}</dd></div>
    </dl>}
    {runtime?.reason && <p className="policy-reason">{reasons[runtime.reason] ?? 'See the recorded reason in decision details.'}</p>}
    <div className="policy-footer"><span>On unavailable/invalid proposals: <strong>{policy.fallback === 'pause' ? 'Pause trading' : 'Use baseline when eligible'}</strong></span>{resolved && decisionTime(runtime.resolved_at) && <time dateTime={runtime.resolved_at}>Decided {decisionTime(runtime.resolved_at)}</time>}</div>
    <details className="technical-details policy-details"><summary>Model and decision details</summary><dl>
      <dt>Model SHA-256</dt><dd className="policy-digest">{policy.policy_sha256}</dd>
      <dt>Training status</dt><dd>{policy.promotion_status}</dd>
      {runtime && <><dt>Recorded result</dt><dd>{runtime.result}</dd><dt>Recorded reason</dt><dd>{runtime.reason || 'None yet'}</dd><dt>Observed market snapshot</dt><dd>{runtime.observation_snapshot_id || 'None yet'}</dd><dt>Applied market snapshot</dt><dd>{runtime.applied_snapshot_id || 'None yet'}</dd><dt>Proposal deadline</dt><dd>{decisionTime(runtime.expires_at) ?? 'None yet'}</dd></>}
      {nextDiffers && config && <><dt>Next plan model</dt><dd className="policy-digest">{config.policy_sha256}</dd><dt>Next plan mode / fallback</dt><dd>{config.profile} / {config.fallback}</dd></>}
    </dl><p className="panel-note">This is the latest recorded decision, not a complete timeline. A semantic action can produce no new order when execution constraints prevent it. Model identity and mode are pinned for each plan.</p></details>
  </section>;
}

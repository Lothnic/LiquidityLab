import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from 'react';
import {
  AlertTriangle, ArrowRight, ChevronDown,
  Command as CommandIcon, LoaderCircle, LockKeyhole,
  LogOut, RefreshCw, ShieldCheck, X, Wallet, Check,
} from 'lucide-react';
import { History } from './History';
import { PolicyPanel } from './PolicyPanel';
import { api, ApiError, type Account, type AccountSummary, type Command, type JournalEntry, type PlanInput, type Preview } from '../lib/api';
import { createPlanInput, holdCommand, retryBody, type PendingCommand } from '../lib/commands';
import { bindPreview, captureScope, formatElapsedNanoseconds, isLatestRequest, previewMatches, requestMayClearIndicator, scopeMatches, snapshotIsFresh, type BoundPreview, type ScopeBinding } from '../lib/bindings';

const POLL_BASE_MS = 1000;
const POLL_MAX_MS = 30000;
const REQUEST_TIMEOUT_MS = 5000;
const isoClock = (value?: string) => value ? new Date(value).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '—';
const formatDuration = (ns: number) => `${Number((ns / 60_000_000_000).toFixed(2))} min`;
const newID = (prefix: string) => `${prefix}-${crypto.randomUUID()}`;

type PlanForm = {
  side: 'buy' | 'sell'; quantity: string; limitPrice: string; durationMinutes: string;
  childCap: string; quoteBudget: string; baseBalance: string; policy: string;
};
const emptyPlan: PlanForm = { side: 'buy', quantity: '', limitPrice: '', durationMinutes: '15', childCap: '', quoteBudget: '', baseBalance: '', policy: 'scheduled' };

function balanceFor(account: Account | null, currency: string) {
  return account?.balances.find((row) => row.currency === currency);
}
function reservedFor(account: Account | null, currency: string) {
  return account?.reserved.find((row) => row.currency === currency)?.amount ?? '0';
}
function requestSignal(parent?: AbortSignal) {
  const timeout = AbortSignal.timeout(REQUEST_TIMEOUT_MS);
  return parent ? AbortSignal.any([parent, timeout]) : timeout;
}

export function App() {
  const previewHeading = useRef<HTMLHeadingElement>(null);
  const currentPlanHeading = useRef<HTMLHeadingElement>(null);
  const [section, setSection] = useState<'trade' | 'history'>('trade');
  const [tokenInput, setTokenInput] = useState('');
  const [token, setToken] = useState('');
  const [accounts, setAccounts] = useState<AccountSummary[]>([]);
  const [selection, setSelection] = useState('');
  const [account, setAccount] = useState<Account | null>(null);
  const [form, setForm] = useState<PlanForm>(emptyPlan);
  const [preview, setPreview] = useState<BoundPreview<Preview> | null>(null);
  const [journal, setJournal] = useState<JournalEntry[]>([]);
  const [journalMore, setJournalMore] = useState(false);
  const [pending, setPending] = useState<PendingCommand | null>(null);
  const [pendingTarget, setPendingTarget] = useState<{ tenant: string; account: string } | null>(null);
  const currentAccountRef = useRef<Account | null>(null);
  const [loading, setLoading] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [pollError, setPollError] = useState('');
  const [message, setMessage] = useState('');
  const [loginError, setLoginError] = useState('');
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null);
  const [freshnessTick, setFreshnessTick] = useState(0);
  const [previewBusy, setPreviewBusy] = useState(false);
  const [commandSending, setCommandSending] = useState(false);
  const [journalBusy, setJournalBusy] = useState(false);
  currentAccountRef.current = account;
  useEffect(() => {
    if (preview) {
      previewHeading.current?.focus({ preventScroll: true });
      previewHeading.current?.scrollIntoView?.({ block: 'nearest' });
    }
  }, [preview]);
  useEffect(() => {
    if (account?.plan?.id) {
      currentPlanHeading.current?.focus({ preventScroll: true });
      currentPlanHeading.current?.scrollIntoView?.({ block: 'nearest' });
    }
  }, [account?.plan?.id]);
  const pollDelay = useRef(POLL_BASE_MS);
  const commandBusy = useRef(false);
  const journalCursorRef = useRef(0);
  const journalBusyRef = useRef(false);
  const journalScopeRef = useRef('');
  const tokenRef = useRef('');
  const activeScopeRef = useRef('');
  const scopeGenerationRef = useRef(0);
  const accountRequestSeqRef = useRef(0);
  const journalRequestSeqRef = useRef(0);
  const previewRequestSeqRef = useRef(0);

  const accountPath = useMemo(() => {
    if (!selection) return null;
    const [tenant, accountName] = selection.split('/', 2);
    return tenant && accountName ? { tenant, account: accountName } : null;
  }, [selection]);
  const selectedSummary = accounts.find((item) => `${item.tenant_id}/${item.account_id}` === selection);
  const accountMatchesSelection = Boolean(accountPath && account && account.tenant_id === accountPath.tenant && account.account_id === accountPath.account);

  const currentBinding = (binding: ScopeBinding) => scopeMatches(binding, {
    token: tokenRef.current,
    tenant: accountPath?.tenant ?? '',
    account: accountPath?.account ?? '',
    generation: scopeGenerationRef.current,
  }) && activeScopeRef.current === `${binding.tenant}/${binding.account}`;
  const invalidatePreview = () => {
    previewRequestSeqRef.current += 1;
    setPreview(null);
    setPreviewBusy(false);
  };
  const beginScopeChange = (scope: string) => {
    scopeGenerationRef.current += 1;
    activeScopeRef.current = scope;
    accountRequestSeqRef.current += 1;
    journalRequestSeqRef.current += 1;
    journalBusyRef.current = false;
    setAccount(null);
    setJournalBusy(false);
    invalidatePreview();
  };
  const changePlanForm = (next: PlanForm | ((old: PlanForm) => PlanForm)) => {
    invalidatePreview();
    setForm(next);
  };

  const refreshAccount = useCallback(async (silent = false, signal?: AbortSignal) => {
    if (!token || !accountPath) return;
    const binding = captureScope(token, accountPath.tenant, accountPath.account, scopeGenerationRef.current);
    const requestSeq = ++accountRequestSeqRef.current;
    if (!silent) setRefreshing(true);
    try {
      const value = await api.account(token, accountPath.tenant, accountPath.account, requestSignal(signal));
      if (!currentBinding(binding) || !isLatestRequest(requestSeq, accountRequestSeqRef.current) ||
        value.tenant_id !== binding.tenant || value.account_id !== binding.account) return null;
      setAccount(value);
      setAccounts((items) => items.map((item) => item.tenant_id === value.tenant_id && item.account_id === value.account_id
        ? { ...item, revision: value.revision, authority_revision: value.authority_revision, ready: value.ready, recovery_required: value.recovery_required, plan_status: value.plan?.status }
        : item));
      setUpdatedAt(new Date());
      setPollError('');
      pollDelay.current = POLL_BASE_MS;
      return value;
    } catch (error) {
      if (!currentBinding(binding) || !isLatestRequest(requestSeq, accountRequestSeqRef.current)) return null;
      if (error instanceof DOMException && error.name === 'AbortError') return;
      const label = error instanceof ApiError ? error.code : 'connection unavailable';
      setPollError(label);
      pollDelay.current = Math.min(POLL_MAX_MS, Math.max(POLL_BASE_MS, pollDelay.current * 2));
      if (!silent) setMessage(`Refresh failed: ${label}`);
      return null;
    } finally {
      if (requestMayClearIndicator(requestSeq, accountRequestSeqRef.current, currentBinding(binding))) setRefreshing(false);
    }
  }, [token, accountPath]);

  const loadJournalPage = useCallback(async (reset = false, signal?: AbortSignal) => {
    if (!token || !accountPath || journalBusyRef.current) return;
    const scope = `${accountPath.tenant}/${accountPath.account}`;
    const binding = captureScope(token, accountPath.tenant, accountPath.account, scopeGenerationRef.current);
    const requestSeq = ++journalRequestSeqRef.current;
    journalBusyRef.current = true;
    setJournalBusy(true);
    try {
      const after = reset ? 0 : journalCursorRef.current;
      const page = await api.journal(token, accountPath.tenant, accountPath.account, after, requestSignal(signal));
      if (!currentBinding(binding) || journalScopeRef.current !== scope || !isLatestRequest(requestSeq, journalRequestSeqRef.current)) return;
      setJournal((old) => {
        const base = reset ? [] : old;
        const merged = [...base, ...page.entries];
        return merged.length > 500 ? merged.slice(-500) : merged;
      });
      journalCursorRef.current = page.next_after_revision;
      setJournalMore(page.entries.length === page.limit);
    } catch (error) {
      if (currentBinding(binding) && isLatestRequest(requestSeq, journalRequestSeqRef.current) &&
        !(error instanceof DOMException && error.name === 'AbortError') && journalScopeRef.current === scope) {
        setMessage(`Journal unavailable: ${error instanceof ApiError ? error.code : 'connection unavailable'}`);
      }
    } finally {
      if (isLatestRequest(requestSeq, journalRequestSeqRef.current)) {
        journalBusyRef.current = false;
        setJournalBusy(false);
      }
    }
  }, [token, accountPath]);

  useEffect(() => {
    setAccount(null);
    setPreview(null);
    setJournal([]);
    journalCursorRef.current = 0;
    journalScopeRef.current = accountPath ? `${accountPath.tenant}/${accountPath.account}` : '';
    setJournalMore(false);
    setMessage('');
    setUpdatedAt(null);
    pollDelay.current = POLL_BASE_MS;
    if (!token || !accountPath) return;
    const controller = new AbortController();
    void refreshAccount(false, controller.signal).then((value) => {
      if (value) void loadJournalPage(true, controller.signal);
    });
    return () => controller.abort();
    // A selection change resets account-scoped state; callbacks track the exact account key.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [token, accountPath?.tenant, accountPath?.account]);

  useEffect(() => {
    if (!token || !accountPath) return;
    let timer: ReturnType<typeof setTimeout>;
    let stopped = false;
    let activeRequest: AbortController | null = null;
    const tick = async () => {
      activeRequest = new AbortController();
      const signal = activeRequest.signal;
      const value = await refreshAccount(true, signal);
      if (value && value.revision > journalCursorRef.current) await loadJournalPage(false, signal);
      if (!stopped) timer = setTimeout(tick, pollDelay.current);
    };
    timer = setTimeout(tick, POLL_BASE_MS);
    return () => { stopped = true; clearTimeout(timer); activeRequest?.abort(); };
  }, [token, accountPath, refreshAccount, loadJournalPage]);

  useEffect(() => {
    if (!updatedAt) return;
    const staleAt = updatedAt.getTime() + 10_001;
    const timer = setTimeout(() => setFreshnessTick(Date.now()), Math.max(0, staleAt - Date.now()));
    return () => clearTimeout(timer);
  }, [updatedAt]);

  useEffect(() => {
    if (!account) return;
    setForm((old) => ({
      ...old,
      quoteBudget: old.quoteBudget || balanceFor(account, account.quote_currency)?.free || '',
      baseBalance: old.baseBalance || balanceFor(account, account.base_currency)?.free || '',
    }));
  }, [account?.account_id, account?.tenant_id]);

  const connect = async (event: FormEvent) => {
    event.preventDefault();
    setLoginError('');
    const candidate = tokenInput.trim();
    if (!candidate) return setLoginError('Enter the operator bearer token.');
    setLoading(true);
    try {
      const result = await api.accounts(candidate, requestSignal());
      if (!result.accounts.length) throw new Error('No accounts are granted to this token.');
      setAccounts(result.accounts);
      const first = result.accounts[0];
      tokenRef.current = candidate;
      beginScopeChange(`${first.tenant_id}/${first.account_id}`);
      setSelection(`${first.tenant_id}/${first.account_id}`);
      setToken(candidate);
      setTokenInput('');
    } catch (error) {
      setLoginError(error instanceof ApiError ? `Authentication or account lookup failed: ${error.code}` : error instanceof Error ? error.message : 'Connection failed.');
    } finally {
      setLoading(false);
    }
  };

  const logout = () => {
    tokenRef.current = '';
    beginScopeChange('');
    journalScopeRef.current = '';
    setToken(''); setTokenInput(''); setAccounts([]); setSelection('');
    setPending(null); setPendingTarget(null); setPreview(null); setJournal([]); setMessage(''); setPollError('');
  };

  const planPayload = (): PlanInput | null => {
    if (!account) return null;
    const duration = Number(form.durationMinutes);
    if (!Number.isFinite(duration) || duration <= 0 || duration > 1440) return null;
    return createPlanInput({
      symbol: account.rules.symbol, side: form.side, quantity: form.quantity, limit_price: form.limitPrice,
      deadline_ns: Math.round(duration * 60_000_000_000), max_child_quantity: form.childCap,
      quote_budget: form.quoteBudget, base_balance: form.baseBalance,
    });
  };

  const runPreview = async (event: FormEvent) => {
    event.preventDefault();
    if (!token || !accountPath || !account || !accountMatchesSelection || pending || pollError ||
      !snapshotIsFresh(updatedAt?.getTime() ?? null, Date.now())) return;
    const plan = planPayload();
    if (!plan || !plan.quantity || !plan.limit_price || !plan.max_child_quantity || !plan.quote_budget || !plan.base_balance) {
      setMessage('Complete each amount field with the exact decimal text you intend to submit.');
      return;
    }
    const submittedPlan = Object.freeze({ ...plan });
    const submittedPolicy = form.policy;
    const binding = captureScope(token, accountPath.tenant, accountPath.account, scopeGenerationRef.current);
    const requestSeq = ++previewRequestSeqRef.current;
    const authorityRevision = account.authority_revision;
    setPreviewBusy(true); setMessage('');
    try {
      const result = await api.preview(token, accountPath.tenant, accountPath.account, submittedPlan, submittedPolicy, requestSignal());
      if (!currentBinding(binding) || !isLatestRequest(requestSeq, previewRequestSeqRef.current) ||
        !currentAccountRef.current || currentAccountRef.current.tenant_id !== binding.tenant ||
        currentAccountRef.current.account_id !== binding.account) return;
      if (result.authority_revision !== authorityRevision || currentAccountRef.current.authority_revision !== authorityRevision) {
        setPreview(null);
        setMessage('Account state changed while preparing the preview. Refresh and review before previewing again.');
        return;
      }
      setPreview(bindPreview({ tenant: binding.tenant, account: binding.account, revision: result.revision, authorityRevision, plan: submittedPlan, policy: submittedPolicy, result }));
      if (!result.ready) setMessage(`Preview is not ready: ${result.reason}`);
    } catch (error) {
      if (currentBinding(binding) && isLatestRequest(requestSeq, previewRequestSeqRef.current)) {
        setPreview(null);
        setMessage(`Preview failed: ${error instanceof ApiError ? error.code : 'connection unavailable'}`);
      }
    } finally {
      if (currentBinding(binding) && isLatestRequest(requestSeq, previewRequestSeqRef.current)) setPreviewBusy(false);
    }
  };

  const sendCommand = async (command: Command, useExisting?: PendingCommand, targetOverride?: { tenant: string; account: string }) => {
    const target = targetOverride ?? accountPath;
    if (!token || !target || commandBusy.current || (pending !== null && !useExisting) ||
      (!useExisting && (!accountMatchesSelection || target.tenant !== accountPath?.tenant || target.account !== accountPath?.account))) return;
    const held = useExisting ?? holdCommand(command);
    commandBusy.current = true;
    setCommandSending(true);
    setPending(held);
    if (!useExisting) setPendingTarget(target);
    setMessage('');
    try {
      // The held object and serialized bytes are retained for unknown outcomes.
      // Revisions and IDs are never rewritten during retry.
      if (retryBody(held) !== JSON.stringify(held.command)) throw new Error('Command payload changed in memory.');
      const result = await api.command(token, target.tenant, target.account, held.command, requestSignal());
      setPending(null);
      setPendingTarget(null);
      setMessage(result.duplicate ? `Command already committed at revision ${result.revision}.` : `Command committed at revision ${result.revision}.`);
      invalidatePreview();
      if (accountPath?.tenant === target.tenant && accountPath.account === target.account) {
        await refreshAccount(false);
        await loadJournalPage(true);
      }
    } catch (error) {
      if (error instanceof ApiError && error.status === 409 && error.code === 'stale_revision') {
        setPending(null);
        setPendingTarget(null);
        setMessage('State changed before this command committed. Review the refreshed account before issuing a new command.');
        if (accountPath?.tenant === target.tenant && accountPath.account === target.account) await refreshAccount(false);
        return;
      }
      if (error instanceof ApiError && (error.status === 400 || error.status === 401 || error.status === 404 ||
        error.code === 'insufficient_funds' || error.code === 'expired')) {
        setPending(null);
        setPendingTarget(null);
        setMessage(`Command rejected: ${error.code}. Review the account before trying again.`);
        if (accountPath?.tenant === target.tenant && accountPath.account === target.account) await refreshAccount(false);
        return;
      }
      setMessage(`Outcome unknown. The original command is retained; retry it unchanged. ${error instanceof ApiError ? error.code : 'Connection interrupted.'}`);
    } finally { commandBusy.current = false; setCommandSending(false); }
  };

  const actionCommand = (kind: Command['kind']) => {
    if (!account || !accountPath || !accountMatchesSelection || account.profile !== 'paper') return;
    if ((kind === 'start' || kind === 'resume') && (pollError || !snapshotIsFresh(updatedAt?.getTime() ?? null, Date.now()))) return;
    void sendCommand({ id: newID(kind), expected_revision: account.revision, expected_authority_revision: account.authority_revision, kind });
  };

  const createPlan = () => {
    const currentPlan = planPayload();
    const bound = preview;
    if (!account || !accountPath || !accountMatchesSelection || pollError || !snapshotIsFresh(updatedAt?.getTime() ?? null, Date.now()) || !bound || !bound.result.ready ||
      !currentPlan || !previewMatches(bound, {
        tenant: accountPath.tenant, account: accountPath.account, authorityRevision: account.authority_revision,
        plan: currentPlan, policy: form.policy,
      })) return;
    void sendCommand({ id: newID('create'), expected_revision: bound.revision, expected_authority_revision: bound.authorityRevision, kind: 'create', plan_id: newID('plan'), plan: { ...bound.plan }, policy: bound.policy });
  };

  const retryPending = () => { if (pending && pendingTarget) void sendCommand(pending.command, pending, pendingTarget); };
  const snapshotFresh = !pollError && snapshotIsFresh(updatedAt?.getTime() ?? null, Math.max(Date.now(), freshnessTick));
  const snapshotStale = accountMatchesSelection && !snapshotFresh;
  const readyForNew = Boolean(snapshotFresh && accountMatchesSelection && account?.profile === 'paper' && account.ready && !account.recovery_required && !account.clock_discontinuity && account.market.valid && !['active', 'paused', 'cancel_requested'].includes(account.plan?.status ?? '') && !account.child);
  const lastUpdated = updatedAt ? `${isoClock(updatedAt.toISOString())} local` : 'waiting for first snapshot';
  const visibleJournal = journal.slice(-12).reverse();

  if (!token) return <Login tokenInput={tokenInput} setTokenInput={setTokenInput} onConnect={connect} loading={loading} error={loginError} />;

  const plan = account?.plan;
  const activePlan = Boolean(plan && ['active', 'paused', 'cancel_requested'].includes(plan.status));
  const step = activePlan ? 3 : preview ? 2 : 1;
  const dataReady = Boolean(accountMatchesSelection && snapshotFresh && account?.profile === 'paper' && account.ready && account.market.valid && !account.recovery_required && !account.clock_discontinuity);

  return (
    <main className="desk-shell">
      <a className="skip-link" href="#workspace">Skip to trading workspace</a>
      <header className="topbar">
        <a className="wordmark" href="#top" aria-label="LiquidityLab desk home"><span className="wordmark-symbol">L</span><span>LiquidityLab</span></a>
        <div className="topbar-right"><span className="paper-pill">Paper trading</span><span className="topbar-note">No real funds</span><button className="icon-button logout" type="button" onClick={logout} disabled={commandSending || Boolean(pending)} title={pending ? 'Resolve your last action before disconnecting.' : 'Disconnect token'} aria-label="Disconnect token"><LogOut size={17} /><span>Disconnect</span></button></div>
      </header>

      <section className="desk-intro" id="top">
        <div><h1>Trading desk</h1><p>Set your limits, review the plan, then start a paper trade.</p></div>
        <div className="intro-meta"><div className={`status-line ${dataReady ? 'text-good' : 'text-warning'}`}><span className={`status-led ${dataReady ? 'good' : 'bad'}`} />{dataReady ? 'Ready to trade' : !accountMatchesSelection ? 'Loading account…' : snapshotStale ? 'Waiting for fresh data' : 'Account needs attention'}</div><p>Updated {lastUpdated}</p></div>
      </section>

      <section className="control-strip" aria-label="Account and safety status">
        <label className="select-wrap"><span>Account</span><select aria-label="Account" value={selection} onChange={(e) => { const next = e.target.value; beginScopeChange(next); journalScopeRef.current = next; setSelection(next); setSection('trade'); changePlanForm(emptyPlan); }}>
          {accounts.map((item) => <option key={`${item.tenant_id}/${item.account_id}`} value={`${item.tenant_id}/${item.account_id}`}>{item.account_id} · {item.tenant_id}</option>)}
        </select><ChevronDown size={14} /></label>
        <div className="market-quote"><strong>{account?.rules.symbol ?? '—'}</strong><span>Bid <b>{account?.market.valid ? account.market.best_bid : '—'}</b></span><span>Ask <b>{account?.market.valid ? account.market.best_ask : '—'}</b></span></div>
        <span className={`source-pill source-${sourceSlug(account?.data_source ?? selectedSummary?.data_source)}`}>{sourceDescription(account?.data_source ?? selectedSummary?.data_source)}</span>
        <button type="button" className="refresh-button" onClick={() => void refreshAccount(false)} disabled={refreshing} aria-label="Refresh account"><RefreshCw size={16} className={refreshing ? 'spin' : ''} /> Refresh</button>
      </section>

      {(pollError || message || pending || snapshotStale) && <section className={`notice ${pending ? 'notice-amber' : pollError || snapshotStale ? 'notice-red' : ''}`} role="status">
        {pending || pollError || snapshotStale ? <AlertTriangle size={19} /> : <Check size={19} />}
        <div className="notice-copy"><strong>{pending ? commandSending ? 'Waiting for confirmation' : 'Your last action needs confirmation' : pollError || snapshotStale ? 'Account data is out of date' : 'Update'}</strong><span>{message || (pending ? commandSending ? 'Waiting for the server to confirm this action.' : 'We did not receive a final response. Retry the same action to check its result.' : pollError ? `Refresh failed (${pollError}). The last confirmed state is shown.` : 'New plans and starts are disabled until fresh data arrives.')}</span>{pending && <small>Action: {pending.command.kind} · Account: {pendingTarget?.tenant}/{pendingTarget?.account}</small>}</div>
        {pending && <button className="button button-outline compact" onClick={retryPending} disabled={loading || commandSending}>{commandSending ? 'Waiting for confirmation…' : 'Retry same action'}</button>}
        {message && !pending && <button className="notice-close" aria-label="Dismiss message" onClick={() => setMessage('')}><X size={18} /></button>}
      </section>}

      <nav className="workspace-nav" aria-label="Workspace">
        <button type="button" aria-current={section === 'trade' ? 'page' : undefined} onClick={() => setSection('trade')}>Trade</button>
        <button type="button" aria-current={section === 'history' ? 'page' : undefined} onClick={() => setSection('history')}>Session history</button>
      </nav>

      <div id="workspace" hidden={section !== 'trade'}>
        <ol className="workflow" aria-label="Create a trading plan">
          {['Set your limits', 'Review the plan', 'Start trading'].map((label, index) => <li key={label} aria-current={step === index + 1 ? 'step' : undefined} className={step > index + 1 ? 'complete' : ''}><span>{step > index + 1 ? <Check size={14} /> : index + 1}</span>{label}{index < 2 && <ArrowRight className="step-arrow" size={15} />}</li>)}
        </ol>
        <div className="workspace-grid">
          <aside className="ticket-column">
            <section className="panel ticket-panel" aria-labelledby="plan-title">
              <details className="plan-composer" open={!activePlan}>
              <summary><h2 id="plan-title">Create a trading plan</h2><ChevronDown size={18} /></summary>
              <p className="ticket-subtitle">Choose what to trade and the limits to follow.</p>
              {activePlan && <p className="panel-note form-blocked">Finish or cancel the current plan before creating another.</p>}
              <form onSubmit={runPreview} className="plan-form" onInvalidCapture={event => { const disclosure = (event.target as HTMLElement).closest('details'); if (disclosure) disclosure.open = true; }}>
                <div className="side-switch" role="group" aria-label="Order side"><button type="button" aria-pressed={form.side === 'buy'} className={form.side === 'buy' ? 'selected buy' : ''} onClick={() => changePlanForm({ ...form, side: 'buy' })}>Buy</button><button type="button" aria-pressed={form.side === 'sell'} className={form.side === 'sell' ? 'selected sell' : ''} onClick={() => changePlanForm({ ...form, side: 'sell' })}>Sell</button></div>
                <div className="field-grid"><Field id="base-quantity" label="Amount" suffix={account?.base_currency ?? 'BTC'} value={form.quantity} onChange={(quantity) => changePlanForm({ ...form, quantity })} placeholder="0.050" required /><Field id={form.side === 'buy' ? 'max-buy-price' : 'min-sell-price'} label={form.side === 'buy' ? 'Maximum buy price' : 'Minimum sell price'} suffix={account?.quote_currency ?? 'USD'} value={form.limitPrice} onChange={(limitPrice) => changePlanForm({ ...form, limitPrice })} placeholder="102.00" required /></div>
                <p className="field-help">The plan will never trade beyond your price limit.</p>
                <div className="field-grid"><Field id="duration" label="Time to complete" suffix="min" type="number" min="0.01" max="1440" step="any" value={form.durationMinutes} onChange={(durationMinutes) => changePlanForm({ ...form, durationMinutes })} placeholder="15" required /><Field id="max-child-size" label="Maximum per order" suffix={account?.base_currency ?? 'BTC'} value={form.childCap} onChange={(childCap) => changePlanForm({ ...form, childCap })} placeholder="0.010" required /></div>
                <p className="field-help">The amount is split into smaller orders. Some may remain unfilled.</p>
                <details className="settings-disclosure">
                  <summary>Execution settings <span>{form.policy === 'scheduled' ? 'Scheduled' : 'Liquidity-aware'}</span><ChevronDown size={16} /></summary>
                  <div className="settings-content">
                    <div className="field-block"><label className="field-label" htmlFor="policy">{account?.policy_config ? 'Baseline method' : 'Execution method'}</label><div className="select-control"><select id="policy" value={form.policy} onChange={(e) => changePlanForm({ ...form, policy: e.target.value })}><option value="scheduled">Scheduled</option><option value="liquidity-aware">Liquidity-aware</option></select><ChevronDown size={16} /></div><p className="field-help">{form.policy === 'scheduled' ? 'Schedules smaller orders across the time you set.' : 'Adjusts smaller orders to available market liquidity.'}</p></div>
                    <div className="budget-heading"><span>Funds available to this plan</span><button type="button" onClick={() => changePlanForm({ ...form, quoteBudget: balanceFor(account, account?.quote_currency ?? '')?.free ?? '', baseBalance: balanceFor(account, account?.base_currency ?? '')?.free ?? '' })}>Use available balances</button></div>
                    <div className="field-grid"><Field id="quote-budget" label="Cash budget" suffix={account?.quote_currency ?? 'USD'} value={form.quoteBudget} onChange={(quoteBudget) => changePlanForm({ ...form, quoteBudget })} placeholder="1000" required /><Field id="base-budget" label="Asset budget" suffix={account?.base_currency ?? 'BTC'} value={form.baseBalance} onChange={(baseBalance) => changePlanForm({ ...form, baseBalance })} placeholder="1" required /></div>
                    <p className="rules-caption">Price step {account?.rules.tick_size ?? '—'} · Quantity step {account?.rules.lot_size ?? '—'} · Minimum {account?.rules.min_quantity ?? '—'} {account?.base_currency ?? ''}</p>
                  </div>
                </details>
                <p className="creation-note">{account?.policy_config ? 'The preview uses the selected baseline. It is not a model prediction. Create a paused plan, then start it to request model proposals.' : 'You will review the first order before creating a paused plan.'} Its timer starts when you create it.</p>
                <button className="button button-lime preview-button" type="submit" disabled={previewBusy || commandSending || Boolean(pending) || !readyForNew}>{previewBusy ? <LoaderCircle className="spin" size={17} /> : <ShieldCheck size={17} />}{previewBusy ? 'Checking your plan…' : 'Review plan'}</button>
                {!readyForNew && <div className="disabled-reason">{!snapshotFresh ? 'Waiting for current account data. Refresh to try again.' : account?.profile !== 'paper' ? 'This desk supports paper trading only.' : account?.recovery_required || account?.clock_discontinuity ? 'The account needs recovery before a new plan can start.' : !account?.market.valid ? 'Waiting for valid market prices.' : 'Finish the current plan before creating a new one.'}</div>}
              </form>

              {preview && <div className="preview-card" aria-live="polite">
                <div className="preview-head"><h3 ref={previewHeading} tabIndex={-1}>{account?.policy_config ? 'Review baseline preview' : 'Review your first order'}</h3><span className={preview.result.ready && preview.result.intent ? 'text-good' : 'text-warning'}>{preview.result.ready && preview.result.intent ? 'Ready' : previewReason(preview.result.reason)}</span></div>
                <div className="preview-values"><div><small>{account?.policy_config ? 'Baseline order preview' : 'Proposed order'}</small><b>{preview.result.intent ? `${preview.result.intent.side === 'buy' ? 'Buy' : 'Sell'} ${preview.result.intent.quantity} ${account?.base_currency}` : 'No order yet'}</b></div><div><small>Limit price</small><b>{preview.result.intent?.price ?? '—'} {account?.quote_currency}</b></div></div>
                <div className="preview-values"><div><small>Bid–ask spread</small><b>{preview.result.spread}</b></div><div><small>Funds needed</small><b>{preview.result.reservation ? `${preview.result.reservation.amount} ${preview.result.reservation.currency}` : '—'}</b></div></div>
                <details className="preview-fee-details"><summary>Estimated fees</summary><p>Maker {preview.result.estimated_maker_fee ? `${preview.result.estimated_maker_fee.amount} ${preview.result.estimated_maker_fee.currency}` : '—'} · Taker {preview.result.estimated_taker_fee ? `${preview.result.estimated_taker_fee.amount} ${preview.result.estimated_taker_fee.currency}` : '—'}</p></details>
                {preview.result.ready && !preview.result.intent && <p className="panel-note">The plan can be created, but it must wait for an eligible first order. Completion is not guaranteed.</p>}
                <p className="creation-note">The plan starts paused. Choose Start trading after creating it. Prices may change before an order is placed.</p>
                {preview.result.ready && preview.authorityRevision === account?.authority_revision && preview.tenant === accountPath?.tenant && preview.account === accountPath?.account && <button className="button button-lime create-button" type="button" disabled={commandSending || Boolean(pending) || !accountMatchesSelection || !snapshotFresh} onClick={createPlan}>Create paused plan <ArrowRight size={17} /></button>}
                {preview.authorityRevision !== account?.authority_revision && <div className="preview-stale">Account state changed. Review the plan again.</div>}
              </div>}
              </details>
              <p className="paper-explanation"><LockKeyhole size={15} /> Orders and fills are simulated. Live trading is unavailable.</p>
            </section>
          </aside>

          <div className="main-column">
            <section className="panel active-panel" aria-labelledby="current-plan-title">
              <div className="panel-heading"><h2 id="current-plan-title" ref={currentPlanHeading} tabIndex={-1}>{plan ? `${plan.spec.side === 'buy' ? 'Buy' : 'Sell'} ${plan.spec.quantity} ${account?.base_currency}` : 'Current plan'}</h2><span className={`state-tag ${stateClass(plan?.status)}`}>{plan ? planStatus(plan.status, plan.started) : 'No plan yet'}</span></div>
              {account?.plan ? <>
                <div className="session-topline"><div className="session-asset"><strong>{account.plan.spec.symbol}</strong><small>{account.policy_runtime ? account.policy_runtime.profile === 'shadow' ? 'RL shadow · baseline execution' : 'RL-assisted execution' : account.plan.policy === 'scheduled' ? 'Scheduled execution' : 'Liquidity-aware execution'} · {formatDuration(account.plan.spec.deadline_ns)}</small></div><div className="deadline"><span>Ends at</span><strong>{isoClock(account.plan.deadline)}</strong></div></div>
                <div className="progress-block"><div className="progress-label"><span>Amount filled</span><strong>{account.plan.filled}<small> / {account.plan.spec.quantity} {account.base_currency}</small></strong></div><div className="progress-track" role="progressbar" aria-label="Filled quantity" aria-valuenow={ratio(account.plan.filled, account.plan.spec.quantity)} aria-valuemin={0} aria-valuemax={100}><div style={{ width: `${ratio(account.plan.filled, account.plan.spec.quantity)}%` }} /></div><div className="progress-foot"><span>{account.plan.remaining} {account.base_currency} remaining</span><span>{account.plan.spec.side === 'buy' ? 'Max' : 'Min'} price {account.plan.spec.limit_price} {account.quote_currency}</span></div></div>
                <div className="session-metrics">
                  <Metric label="Average fill price" value={account.plan.filled === '0' ? '—' : account.plan.average_price} suffix={account.quote_currency} />
                  <Metric label="Fees paid" value={`${account.plan.fees_quote} ${account.quote_currency}`} secondary={`${account.plan.fees_base} ${account.base_currency}`} />
                  <Metric label="Cost vs. start price" value={account.plan.executed_cost_quote ?? '—'} suffix={account.plan.benchmark_valid ? account.quote_currency : 'Not available yet'} help="Filled slippage against the starting midpoint plus quote-valued fees. Unfilled quantity is excluded. This is not profit or loss." />
                </div>
                <div className="child-order"><CommandIcon size={19} /><div className="child-details"><strong>{account.child ? 'Current order' : 'No open order'}</strong><span>{account.child ? `${account.child.intent.side === 'buy' ? 'Buy' : 'Sell'} ${account.child.intent.quantity} at ${account.child.intent.price} ${account.quote_currency}` : (['completed', 'cancelled', 'expired'].includes(account.plan.status) ? 'Execution has ended. Completed fills remain in the account.' : 'Waiting for the next order.')}</span>{account.child && <span>{account.child.remaining} {account.base_currency} remaining in this order</span>}</div>{account.child && <span className={`child-status ${account.child.known_final ? 'text-good' : 'text-warning'}`}>{account.child.known_final ? 'Final' : account.child.cancel_requested ? 'Cancellation pending' : 'Open'}</span>}</div>
                <div className="session-actions">
                  {account.plan.status === 'active' && <button className="button button-outline" disabled={commandSending || Boolean(pending)} onClick={() => actionCommand('pause')}>Pause trading</button>}
                  {account.plan.status === 'paused' && <button className="button button-lime" disabled={commandSending || Boolean(pending) || !snapshotFresh || !account.ready || !account.market.valid || account.recovery_required || account.clock_discontinuity || Boolean(account.child)} onClick={() => actionCommand(account.plan?.started ? 'resume' : 'start')}>{account.plan.started ? 'Resume trading' : 'Start trading'}<ArrowRight size={17} /></button>}
                  {['active', 'paused', 'cancel_requested'].includes(account.plan.status) && <button className="button button-danger" disabled={commandSending || Boolean(pending) || account.plan.status === 'cancel_requested'} onClick={() => actionCommand('cancel')}>{account.plan.status === 'cancel_requested' ? 'Cancelling plan…' : 'Cancel plan'}</button>}
                </div>
                {account.plan.status === 'paused' && account.child && <p className="panel-note text-warning">Wait for the current order to finish cancelling before resuming.</p>}
                {account.plan.status === 'paused' && !account.plan.started && <p className="panel-note">Your plan is ready. Start trading to begin placing simulated orders. The deadline is already counting down.</p>}
                {account.plan.status === 'cancel_requested' && <p className="panel-note text-warning">Cancellation is still pending. An order can fill until its final state is confirmed.</p>}
                {account.plan.status === 'active' && <p className="panel-note">Pausing stops new orders and requests cancellation of the current order.</p>}
              </> : <div className="empty-session"><CommandIcon size={28} /><h3>No plan running</h3><p>Enter an amount, price limit and time to complete. Review the first order before you start.</p><span>All activity stays in your paper account.</span></div>}
            </section>

            {accountMatchesSelection && <PolicyPanel config={account?.policy_config} runtime={account?.policy_runtime} fresh={snapshotFresh} />}

            <section className="panel balances-panel" aria-labelledby="balances-title">
              <div className="panel-heading"><h2 id="balances-title">Account balances</h2><Wallet size={19} /></div>
              <div className="available-balances">{account?.balances.map(balance => <div key={balance.currency}><span>{balance.currency}</span><strong>{balance.free}</strong><small>Free balance</small><small>Reserved locally: {reservedFor(account, balance.currency)}</small></div>)}</div>
              <details className="balance-details"><summary>Balance breakdown</summary><div className="balance-table-wrap"><table><thead><tr><th>Asset</th><th>Total</th><th>Free balance</th><th>Locked</th><th>Reserved locally</th></tr></thead><tbody>
                {account?.balances.map(balance => <tr key={balance.currency}><td>{balance.currency}</td><td>{balance.total}</td><td>{balance.free}</td><td>{balance.locked}</td><td>{reservedFor(account, balance.currency)}</td></tr>)}
              </tbody></table></div><p className="panel-note">Local reservations track order amounts not yet reflected in locked balances. Reflected holds are already included in locked balances.</p></details>
            </section>

            <section className="panel balances-panel fills-panel" aria-labelledby="fills-title">
              <div className="panel-heading"><h2 id="fills-title">Recent fills <span className="fill-count">{account?.fill_count ?? 0}</span></h2><span className="subtle-tag">Simulated</span></div>
              {account?.fills.length ? <><div className="balance-table-wrap"><table><thead><tr><th>Elapsed</th><th>Side / amount</th><th>Price</th><th>Fee</th><th>Liquidity</th></tr></thead><tbody>
                {[...account.fills].slice(-8).reverse().map(fill => <tr key={fill.execution_id}><td>{formatElapsedNanoseconds(fill.time_ns)}</td><td><span className={fill.side === 'buy' ? 'fill-buy' : 'fill-sell'}>{fill.side === 'buy' ? 'Buy' : 'Sell'}</span> {fill.quantity}</td><td>{fill.price}</td><td>{fill.fee} {fill.fee_currency === 'quote' ? account.quote_currency : fill.fee_currency === 'base' ? account.base_currency : fill.fee_currency}</td><td>{fill.maker ? 'Maker' : 'Taker'}</td></tr>)}
              </tbody></table></div><p className="panel-note">Latest {Math.min(8, account.fills.length)} of {account.fill_count} fills. View past sessions in Session history.</p>{account.fills_truncated && <p className="panel-note">The account API retains the latest 2,048 fill rows in this view.</p>}</> : <p className="empty-inline">When an order fills, its amount, price and fee will appear here.</p>}
            </section>

            <details className="technical-details account-technical"><summary>Account details and activity</summary><dl><dt>Account scope</dt><dd>{accountPath?.tenant}/{accountPath?.account}</dd><dt>Revision</dt><dd>#{account?.revision ?? '—'}</dd><dt>Market snapshot</dt><dd>{account?.market.snapshot_id || '—'}</dd><dt>Market data</dt><dd>{sourceDescription(account?.data_source)}</dd><dt>Plan ID</dt><dd>{plan?.id ?? 'None'}</dd></dl><h3>Recent activity</h3><div className="journal-list">{visibleJournal.map(entry => <div className="journal-row" key={entry.revision}><span className="journal-rev">{entry.revision}</span><span className="journal-kind">{entry.kind.replaceAll('_', ' ')}</span><time>{isoClock(entry.created_at)}</time></div>)}{!visibleJournal.length && <p className="empty-inline">No activity in the loaded page.</p>}</div>{journalMore && <button className="load-more" disabled={journalBusy} onClick={() => void loadJournalPage()}>{journalBusy ? 'Loading…' : 'Load older activity'}</button>}<p className="panel-note">Activity contains event labels and revisions. Internal payloads are not exposed.</p></details>
          </div>
        </div>
      </div>
      {section === 'history' && accountPath && <History key={`${accountPath.tenant}/${accountPath.account}`} token={token} tenant={accountPath.tenant} account={accountPath.account} />}
      <footer className="desk-footer"><span>LiquidityLab · Paper trading</span><span>Simulated orders. No real funds are traded.</span></footer>
    </main>
  );
}

function Login({ tokenInput, setTokenInput, onConnect, loading, error }: { tokenInput: string; setTokenInput: (s: string) => void; onConnect: (e: FormEvent) => void; loading: boolean; error: string }) {
  return <main className="login-shell"><div className="login-brand"><span className="wordmark-symbol">L</span><span>LiquidityLab</span></div><section className="login-card"><span className="paper-pill">Paper trading</span><h1>Connect to your trading desk</h1><p>Use the access token configured for your local paper server.</p><form onSubmit={onConnect}><label htmlFor="token">Access token</label><input id="token" type="password" autoComplete="off" value={tokenInput} onChange={e => setTokenInput(e.target.value)} placeholder="Paste your access token" required /><button className="button button-lime" type="submit" disabled={loading}>{loading ? <LoaderCircle className="spin" size={17} /> : <LockKeyhole size={17} />}{loading ? 'Connecting…' : 'Connect to desk'}</button></form>{error && <div className="login-error" role="alert">{error}</div>}<details className="login-help"><summary>Where do I get an access token?</summary><p>Use the value of LIQUIDITYLAB_API_TOKEN configured when your paper server was started. Ask the person running the server if you do not have it.</p></details><p className="login-foot">Your token stays in this tab and is cleared when you disconnect. Orders and funds are simulated.</p></section></main>;
}

function Field({ id, label, suffix, value, onChange, placeholder = '', required, type = 'text', min, max, step }: { id: string; label: string; suffix: string; value: string; onChange: (s: string) => void; placeholder?: string; required?: boolean; type?: string; min?: string; max?: string; step?: string }) {
  return <div className="field-block"><label className="field-label" htmlFor={id}>{label}</label><div className="input-shell"><input id={id} type={type} inputMode={type === 'text' ? 'decimal' : undefined} value={value} onChange={e => onChange(e.target.value)} placeholder={placeholder} required={required} min={min} max={max} step={step} /><span>{suffix}</span></div></div>;
}

function planStatus(status: string, started: boolean) {
  if (status === 'paused') return started ? 'Paused' : 'Ready to start';
  if (status === 'active') return 'Running';
  if (status === 'cancel_requested') return 'Cancellation pending';
  return status.charAt(0).toUpperCase() + status.slice(1).replaceAll('_', ' ');
}
function previewReason(reason: string) {
  const labels: Record<string, string> = { ready: 'Ready', market_invalid: 'Waiting for valid prices', recovery_required: 'Account recovery needed', below_minimum_or_no_free_balance: 'Waiting for an eligible order' };
  return labels[reason] ?? reason.replaceAll('_', ' ');
}
function sourceDescription(source?: string) {
  if (source === 'kraken-public') return 'Kraken market data';
  if (source === 'recorded') return 'Recorded market data';
  if (source === 'synthetic') return 'Simulated market data';
  if (source === 'manual') return 'Manual market data';
  return 'Market source unknown';
}

function Metric({ label, value, suffix, secondary, tone = '', help }: { label: string; value: string; suffix?: string; secondary?: string; tone?: string; help?: string }) {
  return <div className="metric"><span>{label}</span><strong className={tone}>{value}</strong>{suffix && <small>{suffix}</small>}{secondary && <small>{secondary}</small>}{help && <details className="metric-help"><summary>About this estimate</summary><p>{help}</p></details>}</div>;
}

function ratio(filled: string, quantity: string) {
  const a = Number(filled); const b = Number(quantity);
  return b > 0 && Number.isFinite(a / b) ? Math.max(0, Math.min(100, a / b * 100)) : 0;
}
function stateClass(status?: string) {
  if (status === 'active') return 'tag-active';
  if (status === 'completed') return 'tag-done';
  if (status === 'cancel_requested' || status === 'paused') return 'tag-wait';
  return '';
}
function sourceSlug(source?: string) {
  return source === 'kraken-public' ? 'kraken' : source === 'synthetic' ? 'synthetic' : source === 'recorded' ? 'recorded' : 'unknown';
}

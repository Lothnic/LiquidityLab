import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from 'react';
import {
  Activity, AlertTriangle, ArrowDownRight, ArrowUpRight, ChevronDown,
  CircleHelp, Command as CommandIcon, LoaderCircle, LockKeyhole,
  LogOut, RefreshCw, ShieldCheck, X,
} from 'lucide-react';
import { History } from './History';
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
  const [journalBusy, setJournalBusy] = useState(false);
  currentAccountRef.current = account;
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
    } finally { commandBusy.current = false; }
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

  return (
    <main className="desk-shell">
      <header className="topbar">
        <a className="wordmark" href="#top" aria-label="LiquidityLab desk home"><span className="wordmark-symbol">L</span><span>LIQUIDITY<span className="wordmark-light">LAB</span></span></a>
        <div className="topbar-center"><span className="live-dot" /> LOCAL CONTROL PLANE <span className="topbar-divider">/</span> {account?.rules.symbol ?? 'BTC/USD'}</div>
        <div className="topbar-right"><span className="paper-pill"><span /> PAPER ONLY</span><span className={`source-pill source-${sourceSlug(account?.data_source ?? selectedSummary?.data_source)}`}>{sourceLabel(account?.data_source ?? selectedSummary?.data_source)}</span><button className="icon-button logout" type="button" onClick={logout} disabled={Boolean(pending)} title={pending ? 'Resolve the retained command before disconnecting.' : 'Disconnect token'} aria-label="Disconnect token"><LogOut size={17} /><span>Disconnect</span></button></div>
      </header>

      <section className="desk-intro" id="top">
        <div><div className="eyebrow">EXECUTION / OPERATOR VIEW</div><h1>Trade with a <em>clear boundary.</em></h1></div>
        <div className="intro-meta"><div className="status-line"><span className={account?.ready && account.market.valid ? 'status-led good' : 'status-led bad'} />{account?.ready && account.market.valid ? 'ACCOUNT READY' : 'RECOVERY / DATA CHECK'}</div><p>Snapshot {account?.market.snapshot_id || '—'}<br />Updated {lastUpdated}</p></div>
      </section>

      <section className="control-strip" aria-label="Account and safety status">
        <label className="select-wrap"><span>ACCOUNT</span><select aria-label="Account" value={selection} onChange={(e) => { const next = e.target.value; beginScopeChange(next); journalScopeRef.current = next; setSelection(next); changePlanForm(emptyPlan); }}>
          {accounts.map((item) => <option key={`${item.tenant_id}/${item.account_id}`} value={`${item.tenant_id}/${item.account_id}`}>{item.tenant_id} / {item.account_id}</option>)}
        </select><ChevronDown size={14} /></label>
        <div className="strip-item"><span>PROFILE / SOURCE</span><strong>{account ? `${account.profile.toUpperCase()} · ${sourceLabel(account.data_source)}` : '—'}</strong></div>
        <div className="strip-item"><span>REVISION</span><strong>#{account?.revision ?? '—'}</strong></div>
        <div className="strip-item"><span>MARKET</span><strong className={account?.market.valid ? 'text-good' : 'text-warning'}>{account?.market.valid ? 'VALID' : 'INVALID'}</strong></div>
        <button type="button" className="refresh-button" onClick={() => void refreshAccount(false)} disabled={refreshing} aria-label="Refresh account"><RefreshCw size={15} className={refreshing ? 'spin' : ''} /> REFRESH</button>
      </section>

      {(pollError || message || pending || snapshotStale) && <section className={`notice ${pending ? 'notice-amber' : pollError || snapshotStale ? 'notice-red' : ''}`} role="status">
        {pending ? <AlertTriangle size={17} /> : pollError || snapshotStale ? <AlertTriangle size={17} /> : <Activity size={17} />}
        <div className="notice-copy"><strong>{pending ? 'COMMAND OUTCOME UNKNOWN' : pollError || snapshotStale ? 'STALE ACCOUNT VIEW' : 'DESK MESSAGE'}</strong><span>{message || (pending ? `Retained ${pending.command.kind} ${pending.command.id} for ${pendingTarget?.tenant}/${pendingTarget?.account} at revision ${pending.command.expected_revision}. Retry uses the same command.` : pollError ? `Polling failed (${pollError}); showing last confirmed state from ${lastUpdated}.` : snapshotStale ? `Last confirmed snapshot is over 10 seconds old. New orders and starts are disabled.` : '—')}</span></div>
        {pending && <button className="button button-dark compact" onClick={retryPending} disabled={loading}>Retry unchanged command</button>}
        {message && !pending && <button className="notice-close" aria-label="Dismiss message" onClick={() => setMessage('')}><X size={16} /></button>}
      </section>}

      <div className="workspace-grid">
        <div className="main-column">
          <section className="panel active-panel">
            <div className="panel-heading"><div><div className="eyebrow">CURRENT SESSION</div><h2>{account?.plan?.id ?? 'No active order'}</h2></div><span className={`state-tag ${stateClass(account?.plan?.status)}`}>{(account?.plan?.status ?? 'STANDBY').replaceAll('_', ' ').toUpperCase()}</span></div>
            {account?.plan ? <>
              <div className="session-topline"><div className="session-asset"><span className="asset-mark">₿</span><div><strong>{account.plan.spec.side.toUpperCase()} {account.plan.spec.symbol}</strong><small>{account.plan.policy} policy <i>·</i> {formatDuration(account.plan.spec.deadline_ns)}</small></div></div><div className="deadline"><span>DEADLINE</span><strong>{isoClock(account.plan.deadline)}</strong></div></div>
              {account.policy_runtime && <p className="panel-note">{account.policy_runtime.profile.toUpperCase()} MODEL · {account.policy_runtime.result.replaceAll('_', ' ')} · fallback {account.policy_runtime.fallback}{account.policy_runtime.reason ? ` · ${account.policy_runtime.reason.replaceAll('_', ' ')}` : ''}</p>}
              <div className="progress-block"><div className="progress-label"><span>BASE QUANTITY FILLED</span><strong>{account.plan.filled}<small> / {account.plan.spec.quantity} {account.base_currency}</small></strong></div><div className="progress-track" role="progressbar" aria-label="Filled quantity" aria-valuenow={ratio(account.plan.filled, account.plan.spec.quantity)} aria-valuemin={0} aria-valuemax={100}><div style={{ width: `${ratio(account.plan.filled, account.plan.spec.quantity)}%` }} /></div><div className="progress-foot"><span>{account.plan.remaining} {account.base_currency} remaining</span><span>{account.plan.spec.limit_price} hard {account.plan.spec.side === 'buy' ? 'ceiling' : 'floor'}</span></div></div>
              <div className="session-metrics">
                <Metric label="AVERAGE FILL" value={account.plan.filled === '0' ? '—' : account.plan.average_price} suffix={account.quote_currency} />
                <Metric label="EXECUTED COST" value={account.plan.executed_cost_quote ?? '—'} suffix={account.plan.benchmark_valid ? account.quote_currency : 'benchmark unavailable'} tone={account.plan.executed_cost_quote && Number(account.plan.executed_cost_quote) > 0 ? 'amber' : ''} help="Filled slippage against the frozen arrival midpoint plus quote-valued fees. Remaining quantity is excluded; this is not P&L." />
                <Metric label="FEES PAID" value={`${account.plan.fees_quote} ${account.quote_currency}`} secondary={`${account.plan.fees_base} ${account.base_currency}`} />
                <Metric label="PARENT SIZE" value={`${account.plan.spec.quantity} ${account.base_currency}`} secondary={account.plan.benchmark_valid ? `Arrival mid ${account.plan.arrival_mid}` : 'Arrival benchmark set on start'} />
              </div>
              <div className="child-order"><div className="child-icon"><CommandIcon size={17} /></div><div className="child-details"><strong>{account.child ? `${account.child.intent.side.toUpperCase()} LIMIT CHILD` : 'NO OPEN CHILD ORDER'}</strong><span>{account.child ? `${account.child.intent.quantity} @ ${account.child.intent.price} · ${account.child.state.replaceAll('_', ' ')}` : (['completed', 'cancelled', 'expired'].includes(account.plan.status) ? 'Execution has ended. Completed fills remain in the account.' : 'Waiting for next decision or final state')}</span></div>{account.child && <span className={`child-status ${account.child.known_final ? 'text-good' : 'text-warning'}`}>{account.child.known_final ? 'FINAL' : account.child.cancel_requested ? 'CANCEL PENDING' : 'OPEN'}</span>}</div>
              <div className="session-actions">
                {account.plan.status === 'active' && <button className="button button-outline" disabled={Boolean(pending)} onClick={() => actionCommand('pause')}>Pause & cancel child</button>}
                {account.plan.status === 'paused' && <button className="button button-lime" disabled={Boolean(pending) || !snapshotFresh || !account.ready || !account.market.valid || account.recovery_required || account.clock_discontinuity || Boolean(account.child)} onClick={() => actionCommand(account.plan?.started ? 'resume' : 'start')}>{account.plan.started ? 'Resume session' : 'Start session'}</button>}
                {account.plan.status === 'paused' && account.child && <span className="muted small">Wait for the child cancellation to reach a known final state before resuming.</span>}
                {['active', 'paused', 'cancel_requested'].includes(account.plan.status) && <button className="button button-danger" disabled={Boolean(pending) || account.plan.status === 'cancel_requested'} onClick={() => actionCommand('cancel')}>{account.plan.status === 'cancel_requested' ? 'Cancellation pending' : 'Cancel plan'}</button>}
                {account.plan.status === 'cancel_requested' && <span className="muted small">Awaiting known final child state</span>}
              </div>
            </> : <div className="empty-session"><span className="empty-rune">—</span><strong>Nothing in motion.</strong><p>Preview a bounded paper order to inspect the first proposed child before creating a paused plan.</p></div>}
          </section>

          <section className="panel balances-panel">
            <div className="panel-heading panel-heading-tight"><div><div className="eyebrow">ACCOUNT INVENTORY</div><h2>Balances & holds</h2></div><span className="subtle-tag">VENUE STYLE</span></div>
            <div className="balance-table-wrap"><table><thead><tr><th>ASSET</th><th>TOTAL</th><th>FREE</th><th>LOCKED</th><th>LOCAL RESERVED</th></tr></thead><tbody>
              {account?.balances.map((balance) => <tr key={balance.currency}><td><b className="currency-dot">{balance.currency.slice(0, 1)}</b>{balance.currency}</td><td>{balance.total}</td><td>{balance.free}</td><td>{balance.locked}</td><td>{reservedFor(account, balance.currency)}</td></tr>)}
              {!account?.balances.length && <tr><td colSpan={5} className="table-empty">Waiting for account projection.</td></tr>}
            </tbody></table></div>
            <p className="panel-note">Local reservations represent unreflected exposure. Reflected holds are already included in locked balances.</p>
          </section>

          <section className="panel balances-panel fills-panel">
            <div className="panel-heading panel-heading-tight"><div><div className="eyebrow">EXECUTION RECORD</div><h2>Paper fills <span className="fill-count">{account?.fill_count ?? 0}</span></h2></div><span className="subtle-tag">SIMULATED</span></div>
            <div className="balance-table-wrap"><table><thead><tr><th>ELAPSED</th><th>SIDE / QTY</th><th>PRICE</th><th>FEE</th><th>LIQUIDITY</th></tr></thead><tbody>
              {[...(account?.fills ?? [])].slice(-8).reverse().map((fill) => <tr key={fill.execution_id}><td>{formatElapsedNanoseconds(fill.time_ns)}</td><td><span className={fill.side === 'buy' ? 'fill-buy' : 'fill-sell'}>{fill.side.toUpperCase()}</span> {fill.quantity}</td><td>{fill.price}</td><td>{fill.fee} {fill.fee_currency === 'quote' ? account?.quote_currency : fill.fee_currency === 'base' ? account?.base_currency : fill.fee_currency}</td><td>{fill.maker ? 'MAKER' : 'TAKER'}</td></tr>)}
              {!account?.fills.length && <tr><td colSpan={5} className="table-empty">No simulated fills recorded.</td></tr>}
            </tbody></table></div>
            {account?.fills_truncated && <p className="panel-note">Showing the latest 8 of the latest 2,048 API fills. Full retained total: {account.fill_count}.</p>}
            {!account?.fills_truncated && account?.fills.length ? <p className="panel-note">Fill records and fees are simulated in the paper profile.</p> : null}
          </section>

          <section className="panel journal-panel">
            <div className="panel-heading panel-heading-tight"><div><div className="eyebrow">DURABLE AUDIT TRAIL</div><h2>Session journal</h2></div><span className="subtle-tag">METADATA ONLY</span></div>
            <div className="journal-list">
              {visibleJournal.map((entry) => <div className="journal-row" key={entry.revision}><span className="journal-rev">{String(entry.revision).padStart(4, '0')}</span><span className="journal-kind">{entry.kind.replaceAll('_', ' ')}</span><time>{isoClock(entry.created_at)}</time></div>)}
              {!visibleJournal.length && <div className="journal-empty">No journal entries in the loaded page.</div>}
            </div>
            {journalMore && <button className="load-more" disabled={journalBusy} onClick={() => void loadJournalPage()}>{journalBusy ? 'Loading…' : 'Load next journal page'} <ArrowDownRight size={14} /></button>}
            <p className="panel-note">Pages are chronological and bounded. Raw snapshots and journal detail are not exposed by this API.</p>
          </section>
          {accountPath && <History key={`${accountPath.tenant}/${accountPath.account}`} token={token} tenant={accountPath.tenant} account={accountPath.account} />}
        </div>

        <aside className="ticket-column">
          <section className="panel ticket-panel">
            <div className="ticket-title"><div className="eyebrow">NEW PARENT ORDER</div><span className="paper-pill"><span /> PAPER</span></div>
            <h2>Define the boundary.</h2>
            <div className="ticket-subtitle">A plan starts paused. Its deadline begins at creation and continues while paused.</div>
            <form onSubmit={runPreview} className="plan-form">
              <div className="side-switch" role="group" aria-label="Order side"><button type="button" aria-pressed={form.side === 'buy'} className={form.side === 'buy' ? 'selected buy' : ''} onClick={() => changePlanForm({ ...form, side: 'buy' })}><ArrowDownRight size={15} /> Buy</button><button type="button" aria-pressed={form.side === 'sell'} className={form.side === 'sell' ? 'selected sell' : ''} onClick={() => changePlanForm({ ...form, side: 'sell' })}><ArrowUpRight size={15} /> Sell</button></div>
              <div className="field-grid"><Field label="BASE QUANTITY" suffix={account?.base_currency ?? 'BTC'} value={form.quantity} onChange={(quantity) => changePlanForm({ ...form, quantity })} placeholder="0.050" required /><Field label={form.side === 'buy' ? 'MAX BUY PRICE' : 'MIN SELL PRICE'} suffix={account?.quote_currency ?? 'USD'} value={form.limitPrice} onChange={(limitPrice) => changePlanForm({ ...form, limitPrice })} placeholder="102.00" required /></div>
              <div className="field-grid"><Field label="DURATION" suffix="MIN" type="number" min="0.01" max="1440" step="any" value={form.durationMinutes} onChange={(durationMinutes) => changePlanForm({ ...form, durationMinutes })} placeholder="15" required /><Field label="MAX CHILD SIZE" suffix={account?.base_currency ?? 'BTC'} value={form.childCap} onChange={(childCap) => changePlanForm({ ...form, childCap })} placeholder="0.010" required /></div>
              <div className="field-block"><label className="field-label" htmlFor="policy">EXECUTION POLICY</label><div className="select-control"><select id="policy" value={form.policy} onChange={(e) => changePlanForm({ ...form, policy: e.target.value })}><option value="scheduled">Scheduled</option><option value="liquidity-aware">Liquidity-aware</option></select><ChevronDown size={15} /></div></div>
              <div className="budget-heading"><span>ACCOUNT LIMITS</span><button type="button" onClick={() => changePlanForm({ ...form, quoteBudget: balanceFor(account, account?.quote_currency ?? '')?.free ?? '', baseBalance: balanceFor(account, account?.base_currency ?? '')?.free ?? '' })}>Use free balances</button></div>
              <div className="field-grid"><Field label="QUOTE BUDGET" suffix={account?.quote_currency ?? 'USD'} value={form.quoteBudget} onChange={(quoteBudget) => changePlanForm({ ...form, quoteBudget })} placeholder="1000" required /><Field label="BASE BUDGET" suffix={account?.base_currency ?? 'BTC'} value={form.baseBalance} onChange={(baseBalance) => changePlanForm({ ...form, baseBalance })} placeholder="1" required /></div>
              <div className="rules-caption">Tick {account?.rules.tick_size ?? '—'} <i>·</i> Lot {account?.rules.lot_size ?? '—'} <i>·</i> Min {account?.rules.min_quantity ?? '—'} {account?.base_currency ?? ''}</div>
              <button className="button button-dark preview-button" type="submit" disabled={previewBusy || Boolean(pending) || !readyForNew}>{previewBusy ? <LoaderCircle className="spin" size={16} /> : <ShieldCheck size={16} />} Preview first child</button>
              {!readyForNew && <div className="disabled-reason">{!snapshotFresh ? 'A current account snapshot is required before preview.' : account?.profile !== 'paper' ? 'Only paper profile controls are available here.' : account?.recovery_required || account?.clock_discontinuity ? 'Recovery and operator reconciliation are required before a new plan.' : !account?.market.valid ? 'A valid market snapshot is required before preview.' : 'An active, paused, or unresolved plan blocks new creation.'}</div>}
            </form>

            {preview && <div className="preview-card" aria-live="polite">
              <div className="preview-head"><strong>FIRST CHILD ESTIMATE</strong><span className={preview.result.ready && preview.result.intent ? 'text-good' : 'text-warning'}>{preview.result.ready && preview.result.intent ? 'READY' : preview.result.reason.replaceAll('_', ' ').toUpperCase()}</span></div>
              <div className="preview-values"><div><small>PROPOSED</small><b>{preview.result.intent ? `${preview.result.intent.side.toUpperCase()} ${preview.result.intent.quantity}` : 'No order'}</b></div><div><small>LIMIT</small><b>{preview.result.intent?.price ?? '—'} {account?.quote_currency}</b></div></div>
              <div className="preview-values"><div><small>SPREAD</small><b>{preview.result.spread}</b></div><div><small>RESERVATION</small><b>{preview.result.reservation ? `${preview.result.reservation.amount} ${preview.result.reservation.currency}` : '—'}</b></div></div>
              <div className="preview-fees">Estimated fees · maker {preview.result.estimated_maker_fee ? `${preview.result.estimated_maker_fee.amount} ${preview.result.estimated_maker_fee.currency}` : '—'} · taker {preview.result.estimated_taker_fee ? `${preview.result.estimated_taker_fee.amount} ${preview.result.estimated_taker_fee.currency}` : '—'}</div>
              {preview.result.ready && !preview.result.intent && <p className="panel-note">No child can be placed at this first decision. A paused plan can still be created; execution may wait and completion is not guaranteed.</p>}
              <div className="preview-foot">Observed {isoClock(preview.result.observed_at)} <span>Completion not guaranteed</span></div>
              {preview.result.ready && preview.authorityRevision === account?.authority_revision && preview.tenant === accountPath?.tenant && preview.account === accountPath?.account && <button className="button button-lime create-button" type="button" disabled={Boolean(pending) || !accountMatchesSelection || !snapshotFresh} onClick={createPlan}>Create paused plan <ArrowUpRight size={16} /></button>}
              {preview.authorityRevision !== account?.authority_revision && <div className="preview-stale">Account state changed. Run preview again.</div>}
            </div>}
          </section>

          <section className="panel safeguards-panel"><div className="safeguard-heading"><LockKeyhole size={16} /><strong>GUARDRAILS</strong></div><ul><li>Hard price boundary is enforced by the runtime.</li><li>Cancel remains pending until child state is known final.</li><li>Paper fills are simulated and are not exchange executions.</li></ul><div className="safety-foot"><CircleHelp size={14} /> Live execution is unavailable.</div></section>
        </aside>
      </div>
      <footer className="desk-footer"><span>LIQUIDITYLAB <i>·</i> LOCAL TRADER CONSOLE</span><span>DATA {sourceLabel(account?.data_source).toUpperCase()} <i>·</i> PAPER EXECUTION <i>·</i> API REV {account?.revision ?? '—'}</span></footer>
    </main>
  );
}

function Login({ tokenInput, setTokenInput, onConnect, loading, error }: { tokenInput: string; setTokenInput: (s: string) => void; onConnect: (e: FormEvent) => void; loading: boolean; error: string }) {
  return <main className="login-shell"><div className="login-brand"><span className="wordmark-symbol">L</span><span>LIQUIDITY<span className="wordmark-light">LAB</span></span></div><section className="login-card"><div className="eyebrow">LOCAL OPERATOR ACCESS</div><h1>Open the <em>paper desk.</em></h1><p>Connect with a configured bearer token. Credentials stay in this tab's memory and are cleared when you disconnect.</p><form onSubmit={onConnect}><label htmlFor="token">OPERATOR TOKEN</label><input id="token" type="password" autoComplete="off" value={tokenInput} onChange={(e) => setTokenInput(e.target.value)} placeholder="Paste bearer token" required /><button className="button button-lime" type="submit" disabled={loading}>{loading ? <LoaderCircle className="spin" size={16} /> : <LockKeyhole size={16} />} Connect securely</button></form>{error && <div className="login-error" role="alert">{error}</div>}<div className="login-foot"><span className="status-led good" /> API token is never written to browser storage</div></section><div className="login-caption">SPOT EXECUTION <i>/</i> OBSERVABLE STATE <i>/</i> PAPER ONLY</div></main>;
}

function Field({ label, suffix, value, onChange, placeholder = '', required, type = 'text', min, max, step }: { label: string; suffix: string; value: string; onChange: (s: string) => void; placeholder?: string; required?: boolean; type?: string; min?: string; max?: string; step?: string }) {
  const id = label.toLowerCase().replaceAll(/[^a-z0-9]+/g, '-');
  return <div className="field-block"><label className="field-label" htmlFor={id}>{label}</label><div className="input-shell"><input id={id} type={type} inputMode={type === 'text' ? 'decimal' : undefined} value={value} onChange={(e) => onChange(e.target.value)} placeholder={placeholder} required={required} min={min} max={max} step={step} /><span>{suffix}</span></div></div>;
}

function Metric({ label, value, suffix, secondary, tone = '', help }: { label: string; value: string; suffix?: string; secondary?: string; tone?: string; help?: string }) {
  return <div className="metric"><span>{label}{help && <span className="help" title={help} aria-label={help}>ⓘ</span>}</span><strong className={tone}>{value}</strong>{suffix && <small>{suffix}</small>}{secondary && <small>{secondary}</small>}</div>;
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
function sourceLabel(source?: string) {
  if (source === 'kraken-public') return 'KRAKEN PUBLIC';
  if (source === 'recorded') return 'RECORDED';
  if (source === 'synthetic') return 'SYNTHETIC FEED';
  if (source === 'manual') return 'MANUAL';
  return 'SOURCE UNKNOWN';
}
function sourceSlug(source?: string) {
  return source === 'kraken-public' ? 'kraken' : source === 'synthetic' ? 'synthetic' : source === 'recorded' ? 'recorded' : 'unknown';
}

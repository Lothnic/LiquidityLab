import { useEffect, useRef, useState } from 'react';
import { Download, RefreshCw, ChevronDown, ChevronUp } from 'lucide-react';
import { ApiError } from '../lib/api';
import { history, type SessionEvent, type SessionReport, type SessionSummary } from '../lib/history';
import './history.css';

const PAGE_SIZE = 20;
const SESSION_CAP = 200;
const EVENT_CAP = 500;
const iso = (s: string) => new Date(s).toLocaleString();
type Props = { token: string; tenant: string; account: string };
type Scope = { token: string; tenant: string; account: string; key: string };

export function History({ token, tenant, account }: Props) {
  const [sessions, setSessions] = useState<SessionSummary[]>([]);
  const [sessionCursor, setSessionCursor] = useState<number | null>(null);
  const [sessionAsOf, setSessionAsOf] = useState(0);
  const [sessionPages, setSessionPages] = useState(0);
  const [selected, setSelected] = useState('');
  const [report, setReport] = useState<SessionReport | null>(null);
  const [events, setEvents] = useState<SessionEvent[]>([]);
  const [eventCursor, setEventCursor] = useState(0);
  const [eventPages, setEventPages] = useState(0);
  const [focusRevision, setFocusRevision] = useState<number | null>(null);
  const [focusReport, setFocusReport] = useState<SessionReport | null>(null);
  const [busy, setBusy] = useState(false);
  const [detailBusy, setDetailBusy] = useState(false);
  const [error, setError] = useState('');
  const generation = useRef(0);
  const current = useRef<Scope | null>(null);
  const controllers = useRef(new Set<AbortController>());
  const seq = useRef({ list: 0, detail: 0, events: 0, focus: 0 });
  const selectedPlan = useRef('');
  const scopeKey = `${tenant}\u0000${account}\u0000${token}`;

  useEffect(() => {
    const scope: Scope = { token, tenant, account, key: scopeKey };
    current.current = scope;
    const g = ++generation.current;
    for (const c of controllers.current) c.abort();
    controllers.current.clear();
    seq.current = { list: 0, detail: 0, events: 0, focus: 0 };
    selectedPlan.current = '';
    setSessions([]); setSessionCursor(null); setSessionAsOf(0); setSessionPages(0); setSelected(''); setReport(null); setEvents([]);
    setEventCursor(0); setEventPages(0); setFocusRevision(null); setFocusReport(null); setError(''); setBusy(false); setDetailBusy(false);
    if (!token || !tenant || !account) return () => { if (generation.current === g) generation.current++; };
    const ctl = new AbortController(); controllers.current.add(ctl);
    const requestSeq = ++seq.current.list;
    setBusy(true);
    void history.sessions(token, tenant, account, 0, 0, PAGE_SIZE, ctl.signal).then((page) => {
      if (generation.current !== g || current.current?.key !== scope.key || seq.current.list !== requestSeq) return;
      setSessions(page.entries); setSessionCursor(page.entries.length === PAGE_SIZE ? page.next_before_revision : null); setSessionAsOf(page.as_of_revision); setSessionPages(1);
    }).catch((e: unknown) => {
      if (generation.current === g && current.current?.key === scope.key && !(e instanceof DOMException && e.name === 'AbortError')) setError(errorLabel(e));
    }).finally(() => {
      controllers.current.delete(ctl);
      if (generation.current === g && current.current?.key === scope.key && seq.current.list === requestSeq) setBusy(false);
    });
    return () => {
      if (generation.current === g) generation.current++;
      ctl.abort(); controllers.current.delete(ctl);
      for (const c of controllers.current) c.abort(); controllers.current.clear();
    };
  }, [scopeKey, token, tenant, account]);

  const isCurrent = (key: string, g: number) => generation.current === g && current.current?.key === key;
  const scopedFetch = (signal?: AbortSignal) => {
    const ctl = new AbortController();
    controllers.current.add(ctl);
    const abort = () => ctl.abort();
    signal?.addEventListener('abort', abort, { once: true });
    return { ctl, done: () => { controllers.current.delete(ctl); signal?.removeEventListener('abort', abort); } };
  };

  async function loadSessions(reset: boolean) {
    const scope = current.current;
    if (!scope || !scope.token || busy) return;
    const g = generation.current; const requestSeq = ++seq.current.list;
    if (reset) {
      ++seq.current.detail; ++seq.current.events; ++seq.current.focus;
      selectedPlan.current = '';
      setDetailBusy(false);
    }
    const before = reset ? 0 : sessionCursor;
    if (before === null && !reset) return;
    const { ctl, done } = scopedFetch(); setBusy(true); setError('');
    try {
      const next = await history.sessions(scope.token, scope.tenant, scope.account, before ?? 0, reset ? 0 : sessionAsOf, PAGE_SIZE, ctl.signal);
      if (!isCurrent(scope.key, g) || seq.current.list !== requestSeq) return;
      setSessions((old) => reset ? next.entries : [...old, ...next.entries].slice(0, SESSION_CAP));
      setSessionCursor(next.entries.length === PAGE_SIZE ? next.next_before_revision : null); setSessionAsOf(next.as_of_revision); setSessionPages(reset ? 1 : sessionPages + 1);
      if (reset) { setSelected(''); setReport(null); setEvents([]); setEventPages(0); setFocusReport(null); setFocusRevision(null); }
    } catch (e) { if (isCurrent(scope.key, g) && !(e instanceof DOMException && e.name === 'AbortError')) setError(errorLabel(e)); }
    finally { done(); if (isCurrent(scope.key, g) && seq.current.list === requestSeq) setBusy(false); }
  }

  async function selectSession(id: string) {
    const scope = current.current; if (!scope) return;
    const item = sessions.find((s) => s.plan_id === id); if (!item) return;
    const g = generation.current; const requestSeq = ++seq.current.detail;
    ++seq.current.events; ++seq.current.focus;
    selectedPlan.current = id;
    setSelected(id); setReport(null); setEvents([]); setEventCursor(0); setEventPages(0); setFocusRevision(null); setFocusReport(null); setError(''); setDetailBusy(true);
    const { ctl, done } = scopedFetch();
    try {
      const value = await history.report(scope.token, scope.tenant, scope.account, id, 0, ctl.signal);
      if (!isCurrent(scope.key, g) || seq.current.detail !== requestSeq || selectedPlan.current !== id || value.plan_id !== id) return;
      setReport(value);
      const ep = await history.timeline(scope.token, scope.tenant, scope.account, id, 0, value.as_of_revision, 100, ctl.signal);
      if (!isCurrent(scope.key, g) || seq.current.detail !== requestSeq || selectedPlan.current !== id) return;
      setEvents(ep.entries); setEventCursor(ep.entries.length === 100 ? ep.next_after_revision : 0); setEventPages(1);
    } catch (e) { if (isCurrent(scope.key, g) && seq.current.detail === requestSeq && !(e instanceof DOMException && e.name === 'AbortError')) setError(errorLabel(e)); }
    finally { done(); if (isCurrent(scope.key, g) && seq.current.detail === requestSeq) setDetailBusy(false); }
  }

  async function loadEvents() {
    const scope = current.current; if (!scope || !report || selectedPlan.current !== report.plan_id || eventCursor === 0 && !events.length || events.length >= EVENT_CAP) return;
    const planId = report.plan_id;
    const g = generation.current; const requestSeq = ++seq.current.events;
    ++seq.current.focus; setFocusRevision(null); setFocusReport(null);
    const { ctl, done } = scopedFetch(); setDetailBusy(true); setError('');
    try {
      const page = await history.timeline(scope.token, scope.tenant, scope.account, report.plan_id, eventCursor, report.as_of_revision, 100, ctl.signal);
      if (!isCurrent(scope.key, g) || seq.current.events !== requestSeq || selectedPlan.current !== planId) return;
      setEvents((old) => [...old, ...page.entries].slice(0, EVENT_CAP)); setEventCursor(page.entries.length === 100 ? page.next_after_revision : 0); setEventPages(eventPages + 1);
    } catch (e) { if (isCurrent(scope.key, g) && seq.current.events === requestSeq && selectedPlan.current === planId && !(e instanceof DOMException && e.name === 'AbortError')) setError(errorLabel(e)); }
    finally { done(); if (isCurrent(scope.key, g) && seq.current.events === requestSeq) setDetailBusy(false); }
  }

  async function inspectEvent(event: SessionEvent) {
    const scope = current.current; if (!scope || !report || selectedPlan.current !== report.plan_id) return;
    const g = generation.current; const requestSeq = ++seq.current.focus; const { ctl, done } = scopedFetch(); setFocusRevision(event.revision); setFocusReport(null); setDetailBusy(true); setError('');
    try {
      const value = await history.report(scope.token, scope.tenant, scope.account, report.plan_id, event.revision, ctl.signal);
      if (!isCurrent(scope.key, g) || seq.current.focus !== requestSeq || selectedPlan.current !== report.plan_id || value.plan_id !== report.plan_id || value.as_of_revision !== event.revision) return;
      setFocusReport(value);
    } catch (e) { if (isCurrent(scope.key, g) && seq.current.focus === requestSeq && selectedPlan.current === report.plan_id && !(e instanceof DOMException && e.name === 'AbortError')) setError(errorLabel(e)); }
    finally { done(); if (isCurrent(scope.key, g) && seq.current.focus === requestSeq) setDetailBusy(false); }
  }

  const exportJSON = () => {
    if (!report) return;
    const blob = new Blob([JSON.stringify({ account: { tenant_id: tenant, account_id: account }, report, timeline: { events, as_of_revision: report.as_of_revision, loaded_count: events.length, complete: eventCursor === 0 && events.length < EVENT_CAP, bounded_at: EVENT_CAP } }, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob); const anchor = document.createElement('a');
    anchor.href = url; anchor.download = `session-${report.plan_id}.json`; anchor.click(); URL.revokeObjectURL(url);
  };
  const hasMoreSessions = sessionCursor !== null && sessionCursor > 0 && sessionPages * PAGE_SIZE >= sessions.length && sessions.length < SESSION_CAP;
  const hasMoreEvents = eventCursor > 0 && events.length < EVENT_CAP && eventPages * 100 >= events.length;

  return <section className="panel history-panel" aria-label="Session history">
    <div className="history-head"><div><h2>Session history</h2><p className="history-scope">Account {tenant} / {account}</p></div><div className="history-actions"><button className="button button-outline" type="button" disabled={busy || !token} onClick={() => void loadSessions(true)}><RefreshCw size={14} /> Refresh history</button>{report && <button className="button button-outline" type="button" onClick={exportJSON}><Download size={14} /> Export report</button>}</div></div>
    {error && <div role="alert" className="history-error">History unavailable: {error}</div>}
    <div className="history-layout">
      <div className="history-list" aria-label="Sessions">
        {busy && !sessions.length && <div className="journal-empty" role="status">Loading session history…</div>}
        {!sessions.length && !busy && !error && <div className="journal-empty"><strong>No sessions yet</strong><span>Completed and in-progress sessions for this account will appear here.</span></div>}
        {sessions.map((item) => <button type="button" className={`history-item ${selected === item.plan_id ? 'selected' : ''}`} key={item.plan_id} onClick={() => void selectSession(item.plan_id)}>
          <span className="history-item-top"><strong>{item.side.toUpperCase()} {item.symbol}</strong><small>r{item.revision}</small></span>
          <span className="history-item-meta">{item.status.replaceAll('_', ' ')} · {item.quantity} {item.base_currency} · {item.policy}</span>
          <time>{iso(item.created_at)}</time>
        </button>)}
        {hasMoreSessions && <button className="load-more" type="button" disabled={busy} onClick={() => void loadSessions(false)}>{busy ? 'Loading…' : 'Load older sessions'} <ChevronDown size={14}/></button>}
        {sessions.length >= SESSION_CAP && <p className="panel-note">Showing the bounded first {SESSION_CAP} sessions.</p>}
      </div>
      <div className="history-detail">
        {detailBusy && !report && <div className="journal-empty" role="status">Loading the selected session report…</div>}
        {!selected && <div className="journal-empty"><strong>Choose a session</strong><span>Select a row to review its fills, fees, completion status, and saved report.</span></div>}
        {report && <ReportView key={`${(focusReport ?? report).plan_id}:${(focusReport ?? report).as_of_revision}`} report={focusReport ?? report} provisional={Boolean(focusReport && focusRevision !== null)} />}
        {report && focusRevision !== null && !focusReport && <p className="panel-note" role="status">Loading stored report at r{focusRevision}… Latest retained report remains shown.</p>}
        {report && <>
          <details className="history-technical">
            <summary>Technical activity <ChevronDown size={16} /></summary>
            <div className="history-timeline-head"><div><h3>Activity timeline <small>Report at revision {report.as_of_revision}</small></h3></div></div>
            <p className="panel-note">Choose an event to view the report saved at that revision. Market data is not replayed.</p>
            <div className="history-events">{events.map((event) => <button type="button" key={event.revision} className={`history-event ${focusRevision === event.revision ? 'selected' : ''}`} onClick={() => void inspectEvent(event)}><span>Revision {event.revision}</span><strong>{event.kind.replaceAll('_', ' ')}</strong><time>{iso(event.created_at)}</time><ChevronDown size={13}/></button>)}</div>
            {hasMoreEvents && <button type="button" className="load-more" disabled={detailBusy} onClick={() => void loadEvents()}>{detailBusy ? 'Loading…' : 'Load more events'} <ChevronDown size={14}/></button>}
          {focusRevision !== null && <button type="button" className="load-more" onClick={() => { ++seq.current.focus; setFocusRevision(null); setFocusReport(null); setDetailBusy(false); }}><ChevronUp size={14}/> Return to latest report</button>}
            {events.length >= EVENT_CAP && <p className="panel-note">Showing the first {EVENT_CAP} events.</p>}
          </details>
        </>}
      </div>
    </div>
  </section>;
}

function errorLabel(error: unknown) {
  if (error instanceof DOMException && error.name === 'TimeoutError') return 'request timed out';
  return error instanceof ApiError ? error.code : 'connection unavailable';
}
function ReportView({ report, provisional }: { report: SessionReport; provisional: boolean }) {
  const [fillsPage, setFillsPage] = useState(0);
  const fillsPageSize = 200;
  const fillsEnd = Math.max(0, report.fills.length - fillsPage * fillsPageSize);
  const fillsStart = Math.max(0, fillsEnd - fillsPageSize);
  const visibleRows = report.fills.slice(fillsStart, fillsEnd).reverse();
  const pageCount = Math.ceil(report.fills.length / fillsPageSize);
  const costs = report.marked_cost_estimate_quote === null ? '—' : `${report.marked_cost_estimate_quote} ${report.quote_currency}`;
  const opportunity = report.residual_opportunity_quote === null ? '—' : `${report.residual_opportunity_quote} ${report.quote_currency}`;
  const feeCurrency = (currency: string) => currency === 'quote' ? report.quote_currency : currency === 'base' ? report.base_currency : currency;
  return <div className="history-report">
    <div className="history-report-title"><div><h3>{report.side.toUpperCase()} {report.symbol}</h3></div><span className={`state-tag ${report.known_final ? 'tag-done' : 'tag-wait'}`}>{report.known_final ? 'Final' : 'Provisional'}</span></div>
    <p className="history-status">{report.known_final ? 'Final state is known' : 'State is provisional'} · Saved revision {report.as_of_revision} · {report.deadline_result.replaceAll('-', ' ')} · {report.profile} / {report.data_source}{provisional ? ' · Inspecting saved revision' : ''}</p>
    <p className="history-status">{report.plan.started ? `Started ${iso(report.plan.start)}` : 'Not started'} · Deadline {iso(report.plan.deadline)}{report.policy_sha256 ? ` · Policy SHA-256 ${report.policy_sha256}` : ''}</p>
    {report.recovery_required && <p className="history-warning">Recovery or reconciliation is required before treating this record as final.</p>}
    <div className="history-metrics"><Metric label="Completion" value={`${report.completion_percent}%`} /><Metric label="Filled / size" value={`${report.plan.filled} / ${report.quantity}`} /><Metric label="Remaining" value={report.plan.remaining} /><Metric label="Average price" value={report.plan.average_price} unit={report.quote_currency} /><Metric label="Cost vs. start price" value={String(report.plan.executed_cost_quote ?? '—')} unit={report.quote_currency} /><Metric label="Fees paid" value={`${report.plan.fees_quote} ${report.quote_currency}`} /><Metric label="Base fees" value={`${report.plan.fees_base} ${report.base_currency}`} /></div>
    <div className="history-accounting"><div><span>Unfilled amount cost estimate</span><strong>{opportunity}</strong></div><div><span>Total marked cost estimate</span><strong>{costs}</strong></div></div>
    <p className="panel-note">These estimates compare fills and unfilled quantity with the starting price. They are not profit or loss. The unfilled amount uses the {report.residual_reference_kind.replaceAll('-', ' ')}{report.residual_reference_mid !== null ? ` midpoint ${report.residual_reference_mid}` : ''} recorded at revision {report.residual_reference_revision}. The reference is fixed at the first final state when available; otherwise it uses the observed report revision.</p>
    <div className="history-fills-title"><strong>Simulated fills <span className="fill-count">{report.fills.length}</span></strong></div>
    <div className="history-fill-table"><table><thead><tr><th>Side / quantity</th><th>Price</th><th>Fee</th><th>Liquidity</th></tr></thead><tbody>{visibleRows.map((fill) => <tr key={fill.execution_id}><td>{fill.side.toUpperCase()} {fill.quantity}</td><td>{fill.price}</td><td>{fill.fee} {feeCurrency(fill.fee_currency)}</td><td>{fill.maker ? 'Maker' : 'Taker'}</td></tr>)}{!visibleRows.length && <tr><td colSpan={4} className="table-empty">No simulated fills recorded.</td></tr>}</tbody></table></div>
    {report.fills.length > fillsPageSize && <div className="history-fill-pagination">
      <span className="panel-note">Showing {fillsStart + 1}–{fillsEnd} of {report.fills.length} fills · page {fillsPage + 1} of {pageCount}</span>
      <div>
        {fillsPage > 0 && <button type="button" className="load-more" aria-label="Show newer fills" onClick={() => setFillsPage(fillsPage - 1)}><ChevronUp size={14}/> Newer fills</button>}
        {fillsPage + 1 < pageCount && <button type="button" className="load-more" aria-label="Show older fills" onClick={() => setFillsPage(fillsPage + 1)}>Older fills <ChevronDown size={14}/></button>}
      </div>
    </div>}
  </div>;
}
function Metric({ label, value, unit }: { label: string; value: string; unit?: string }) { return <div><span>{label}</span><strong>{value}{value !== '—' && unit ? ` ${unit}` : ''}</strong></div>; }

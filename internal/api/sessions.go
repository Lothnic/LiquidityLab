package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

// SessionReader returns owned, revision-bound history. Raw snapshots never cross
// the HTTP interface; only curated financial and provenance projections do.
type SessionReader interface {
	Sessions(context.Context, pg.Key, int64, int64, int) (pg.SessionPage, error)
	Session(context.Context, pg.Key, string, int64) (pg.SessionSnapshot, error)
	SessionJournal(context.Context, pg.Key, string, int64, int64, int) ([]pg.JournalEntry, error)
}
type SessionSummary struct {
	PlanID          string    `json:"plan_id"`
	CreatedRevision int64     `json:"created_revision"`
	Revision        int64     `json:"revision"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Profile         string    `json:"profile"`
	DataSource      string    `json:"data_source"`
	BaseCurrency    string    `json:"base_currency"`
	QuoteCurrency   string    `json:"quote_currency"`
	Status          string    `json:"status"`
	Side            string    `json:"side"`
	Symbol          string    `json:"symbol"`
	Quantity        d.Decimal `json:"quantity"`
	LimitPrice      d.Decimal `json:"limit_price"`
	Policy          string    `json:"policy"`
	PolicySHA256    string    `json:"policy_sha256,omitempty"`
}
type SessionReport struct {
	SourceProvenance *SourceProvenanceDTO `json:"source_provenance,omitempty"`
	Rules            d.InstrumentRules    `json:"rules"`
	Fees             d.FeeConfig          `json:"fees"`
	Simulation       d.ReplayConfig       `json:"simulation"`
	PolicyRuntime    *PolicyRuntimeDTO    `json:"policy_runtime,omitempty"`
	TenantID         string               `json:"tenant_id"`
	AccountID        string               `json:"account_id"`
	Version          string               `json:"version"`
	SessionSummary
	AsOfRevision              int64      `json:"as_of_revision"`
	Plan                      *PlanDTO   `json:"plan"`
	Child                     *ChildDTO  `json:"child"`
	Fills                     []d.Fill   `json:"fills"`
	CompletionPercent         d.Decimal  `json:"completion_percent"`
	KnownFinal                bool       `json:"known_final"`
	RecoveryRequired          bool       `json:"recovery_required"`
	DeadlineResult            string     `json:"deadline_result"`
	Market                    MarketDTO  `json:"market"`
	ResidualReferenceMid      *d.Decimal `json:"residual_reference_mid"`
	ResidualReferenceRevision int64      `json:"residual_reference_revision"`
	ResidualReferenceKind     string     `json:"residual_reference_kind"`
	ResidualOpportunityQuote  *d.Decimal `json:"residual_opportunity_quote"`
	MarkedCostEstimateQuote   *d.Decimal `json:"marked_cost_estimate_quote"`
}

func sessionSummary(meta pg.SessionMeta, p *rt.Parent) SessionSummary {
	result := SessionSummary{PlanID: p.ID, CreatedRevision: meta.CreatedRevision, Revision: meta.Revision, CreatedAt: meta.CreatedAt, UpdatedAt: meta.UpdatedAt, Profile: meta.Profile, DataSource: meta.DataSource, BaseCurrency: meta.BaseCurrency, QuoteCurrency: meta.QuoteCurrency, Status: p.Status, Side: p.Spec.Side, Symbol: p.Spec.Symbol, Quantity: p.Spec.Quantity, LimitPrice: p.Spec.LimitPrice, Policy: p.Baseline}
	if p.PolicyBundle != nil {
		result.PolicySHA256 = p.PolicyBundle.PolicySHA256
	}
	return result
}
func buildSessionReport(key pg.Key, id string, saved pg.SessionSnapshot) (report SessionReport, err error) {
	defer func() {
		if recover() != nil {
			report = SessionReport{}
			err = rt.ErrInvalid
		}
	}()
	s, err := rt.DecodeSnapshot(saved.Snapshot)
	if err != nil || s.Parent == nil || s.Parent.ID != id || s.Parent.Spec.Validate() != nil {
		return SessionReport{}, rt.ErrInvalid
	}
	v := rt.View{Account: pg.Account{Key: key, Revision: saved.Revision, Market: pg.Market{Valid: s.Book.Valid}}, State: s}
	dto, err := project(v)
	if err != nil {
		return SessionReport{}, err
	}
	out := SessionReport{TenantID: key.TenantID, AccountID: key.AccountID, Version: "session-report-v1", SessionSummary: sessionSummary(pg.SessionMeta{CreatedRevision: saved.CreatedRevision, Revision: saved.Revision, CreatedAt: saved.CreatedAt, UpdatedAt: saved.UpdatedAt, Profile: string(s.Profile), DataSource: s.DataSource, BaseCurrency: s.BaseCurrency, QuoteCurrency: s.QuoteCurrency}, s.Parent), AsOfRevision: saved.AsOfRevision, Plan: dto.Plan, Child: dto.Child, Fills: append([]d.Fill{}, s.Fills...), Market: dto.Market, RecoveryRequired: s.RecoveryRequired || s.ClockDiscontinuity, DeadlineResult: "in-progress"}
	out.Rules = s.Rules
	out.Fees = s.Fees
	out.Simulation = s.Execution
	out.PolicyRuntime = policyProjection(s)
	out.SourceProvenance, err = sourceProvenance(s)
	if err != nil {
		return SessionReport{}, rt.ErrInvalid
	}
	out.CompletionPercent = dto.Plan.Filled.Mul(d.MustDecimal("100")).Quo(s.Parent.Spec.Quantity)
	final := s.Parent.Status == "completed" || s.Parent.Status == "cancelled" || s.Parent.Status == "expired"
	// Retained legacy snapshots can still contain accepted, unapplied facts.
	// Terminal parent status alone does not settle those financial outcomes.
	if len(s.PendingFacts) > 0 {
		out.RecoveryRequired = true
	}
	out.KnownFinal = final && s.Child == nil && !out.RecoveryRequired
	for _, fact := range s.UnresolvedFacts {
		if !fact.Resolved {
			out.KnownFinal = false
			out.RecoveryRequired = true
		}
	}
	if !s.Parent.Started {
		out.DeadlineResult = "not-started"
	} else if final {
		out.DeadlineResult = s.Parent.Status
	}
	reference := s
	out.ResidualReferenceRevision = saved.Revision
	out.ResidualReferenceKind = "observed-as-of"
	if len(saved.TerminalSnapshot) > 0 {
		reference, err = rt.DecodeSnapshot(saved.TerminalSnapshot)
		if err != nil || reference.Parent == nil || reference.Parent.ID != id {
			return SessionReport{}, rt.ErrInvalid
		}
		if _, err := sourceProvenance(reference); err != nil || !matchingSourceReference(s, reference) {
			return SessionReport{}, rt.ErrInvalid
		}
		out.ResidualReferenceRevision = saved.TerminalRevision
		out.ResidualReferenceKind = "first-terminal"
	}
	if dto.Plan.BenchmarkValid && reference.Book.Valid && reference.Book.Mid().Sign() > 0 {
		mid := reference.Book.Mid()
		sign := d.One
		if s.Parent.Spec.Side == "sell" {
			sign = sign.Neg()
		}
		residual := sign.Mul(dto.Plan.Remaining).Mul(mid.Sub(s.Parent.ArrivalMid))
		marked := dto.Plan.ExecutedCostQuote.Add(residual)
		out.ResidualReferenceMid = &mid
		out.ResidualOpportunityQuote = &residual
		out.MarkedCostEstimateQuote = &marked
	}
	return out, nil
}

// strictHistoryQuery rejects unknown/repeated keys and decimal/exponent cursors.
func strictHistoryQuery(raw string, defaults map[string]int64) (map[string]int64, error) {
	parsed, err := url.ParseQuery(raw)
	if err != nil {
		return nil, pg.ErrInvalid
	}
	for key, values := range parsed {
		if _, ok := defaults[key]; !ok || len(values) != 1 || values[0] == "" {
			return nil, pg.ErrInvalid
		}
		for _, c := range values[0] {
			if c < '0' || c > '9' {
				return nil, pg.ErrInvalid
			}
		}
		n, e := strconv.ParseInt(values[0], 10, 64)
		if e != nil {
			return nil, pg.ErrInvalid
		}
		defaults[key] = n
	}
	return defaults, nil
}
func (h *Handler) sessionsEndpoint(w http.ResponseWriter, r *http.Request, key pg.Key, parts []string) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if h.sessions == nil {
		fail(w, 503, "unavailable")
		return
	}
	if len(parts) == 5 {
		q, err := strictHistoryQuery(r.URL.RawQuery, map[string]int64{"before_revision": 0, "as_of_revision": 0, "limit": 20})
		if err != nil || q["limit"] < 1 || q["limit"] > 50 {
			fail(w, 400, "invalid_query")
			return
		}
		page, err := h.sessions.Sessions(r.Context(), key, q["before_revision"], q["as_of_revision"], int(q["limit"]))
		if err != nil {
			backendError(w, err)
			return
		}
		entries := []SessionSummary{}
		next := q["before_revision"]
		for _, e := range page.Entries {
			encoded, _ := json.Marshal(map[string]any{"version": rt.StateVersion, "profile": rt.ProfilePaper, "book": map[string]any{}, "parent": e.Parent})
			snapshot, decodeErr := rt.DecodeSnapshot(encoded)
			parent := snapshot.Parent
			if decodeErr != nil || parent == nil || parent.ID != e.PlanID || parent.Spec.Validate() != nil {
				fail(w, 503, "unavailable")
				return
			}
			entries = append(entries, sessionSummary(e, parent))
			next = e.CreatedRevision
		}
		writeJSON(w, 200, map[string]any{"entries": entries, "as_of_revision": page.AsOfRevision, "next_before_revision": next, "limit": q["limit"]})
		return
	}
	if !identifier(parts[5]) {
		fail(w, 404, "not_found")
		return
	}
	if len(parts) == 6 {
		q, err := strictHistoryQuery(r.URL.RawQuery, map[string]int64{"as_of_revision": 0})
		if err != nil {
			fail(w, 400, "invalid_query")
			return
		}
		saved, err := h.sessions.Session(r.Context(), key, parts[5], q["as_of_revision"])
		if err != nil {
			backendError(w, err)
			return
		}
		out, err := buildSessionReport(key, parts[5], saved)
		if err != nil {
			fail(w, 503, "unavailable")
			return
		}
		writeJSON(w, 200, out)
		return
	}
	if len(parts) != 7 || parts[6] != "timeline" {
		fail(w, 404, "not_found")
		return
	}
	q, err := strictHistoryQuery(r.URL.RawQuery, map[string]int64{"after_revision": 0, "as_of_revision": 0, "limit": 100})
	if err != nil || q["limit"] < 1 || q["limit"] > 200 {
		fail(w, 400, "invalid_query")
		return
	}
	// Pin the timeline to a report revision before requesting any event pages.
	saved, err := h.sessions.Session(r.Context(), key, parts[5], q["as_of_revision"])
	if err != nil {
		backendError(w, err)
		return
	}
	entries, err := h.sessions.SessionJournal(r.Context(), key, parts[5], q["after_revision"], saved.AsOfRevision, int(q["limit"]))
	if err != nil {
		backendError(w, err)
		return
	}
	next := q["after_revision"]
	if len(entries) > 0 {
		next = entries[len(entries)-1].Revision
	}
	writeJSON(w, 200, map[string]any{"entries": entries, "as_of_revision": saved.AsOfRevision, "next_after_revision": next, "limit": q["limit"]})
}

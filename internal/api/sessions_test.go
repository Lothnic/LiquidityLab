package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

type fakeSessionReader struct {
	page                                              pg.SessionPage
	pageErr                                           error
	saved                                             pg.SessionSnapshot
	sessionErr                                        error
	journal                                           []pg.JournalEntry
	journalErr                                        error
	listCalls, sessionCalls, journalCalls             int
	listKey, sessionKey, journalKey                   pg.Key
	before, listAsOf, sessionAsOf, after, journalAsOf int64
	limit, journalLimit                               int
	planID, journalPlanID                             string
}

func (f *fakeSessionReader) Sessions(_ context.Context, key pg.Key, before, asof int64, limit int) (pg.SessionPage, error) {
	f.listCalls++
	f.listKey = key
	f.before = before
	f.listAsOf = asof
	f.limit = limit
	return f.page, f.pageErr
}
func (f *fakeSessionReader) Session(_ context.Context, key pg.Key, id string, asof int64) (pg.SessionSnapshot, error) {
	f.sessionCalls++
	f.sessionKey = key
	f.planID = id
	f.sessionAsOf = asof
	if f.saved.AsOfRevision == 0 && asof > 0 {
		f.saved.AsOfRevision = asof
	}
	return f.saved, f.sessionErr
}
func (f *fakeSessionReader) SessionJournal(_ context.Context, key pg.Key, id string, after, asof int64, limit int) ([]pg.JournalEntry, error) {
	f.journalCalls++
	f.journalKey = key
	f.journalPlanID = id
	f.after = after
	f.journalAsOf = asof
	f.journalLimit = limit
	return f.journal, f.journalErr
}

func sessionsHandler(t *testing.T, reader *fakeSessionReader) *Handler {
	t.Helper()
	h, _, _ := setup(t)
	h.sessions = reader
	return h
}

func sessionState(t *testing.T, id, status, side string, arrival, mid string, fills []d.Fill) json.RawMessage {
	t.Helper()
	state := sampleView(keyA).State
	state.Version = rt.StateVersion
	state.Profile = rt.ProfilePaper
	state.DataSource = "historical-replay"
	state.BaseCurrency = "BTC"
	state.QuoteCurrency = "USD"
	book := x.NewBook()
	if err := book.Apply(d.MarketEvent{Kind: "snapshot", Bids: []d.Level{{Price: d.MustDecimal(mid).Sub(d.MustDecimal("0.5")), Quantity: d.One}}, Asks: []d.Level{{Price: d.MustDecimal(mid).Add(d.MustDecimal("0.5")), Quantity: d.One}}}); err != nil {
		t.Fatal(err)
	}
	state.Book = book
	plan := d.DefaultPlan()
	plan.Side = side
	plan.Quantity = d.One
	plan.LimitPrice = d.MustDecimal("110")
	if side == "sell" {
		plan.LimitPrice = d.MustDecimal("90")
	}
	plan.MaxChildQuantity = d.One
	plan.BaseBalance = d.MustDecimal("10")
	plan.QuoteBudget = d.MustDecimal("1000")
	state.Parent = &rt.Parent{ID: id, Revision: 1, Status: status, Started: true, Start: time.Unix(100, 0).UTC(), Deadline: time.Unix(200, 0).UTC(), Spec: plan, Baseline: x.BaselineScheduled, ArrivalMid: d.MustDecimal(arrival)}
	state.Fills = append([]d.Fill{}, fills...)
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func sessionRecord(t *testing.T, id string, revision, asof int64, status, side, arrival, mid string, fills []d.Fill) pg.SessionSnapshot {
	t.Helper()
	return pg.SessionSnapshot{AsOfRevision: asof, Revision: revision, CreatedRevision: 2, CreatedAt: time.Unix(10, 0).UTC(), UpdatedAt: time.Unix(20, 0).UTC(), Snapshot: sessionState(t, id, status, side, arrival, mid, fills)}
}

func TestSessionsEndpointsRequireScopeAndStrictQueries(t *testing.T) {
	f := &fakeSessionReader{page: pg.SessionPage{AsOfRevision: 8}}
	h := sessionsHandler(t, f)
	for _, tc := range []struct {
		method, path, token string
		status              int
	}{
		{"GET", pathA + "/sessions", tokenB, 404},
		{"POST", pathA + "/sessions", tokenA, 405},
		{"GET", pathA + "/sessions?unknown=1", tokenA, 400},
		{"GET", pathA + "/sessions?limit=1&limit=2", tokenA, 400},
		{"GET", pathA + "/sessions?before_revision=-1", tokenA, 400},
		{"GET", pathA + "/sessions?as_of_revision=9223372036854775808", tokenA, 400},
		{"GET", pathA + "/sessions/plan?as_of_revision=1&as_of_revision=2", tokenA, 400},
		{"GET", pathA + "/sessions/plan/timeline?after_revision=-1", tokenA, 400},
		{"GET", pathA + "/sessions/plan/timeline?limit=201", tokenA, 400},
		{"GET", pathA + "/sessions/plan/timeline?what=1", tokenA, 400},
	} {
		w := request(h, tc.method, tc.path, tc.token, "")
		if w.Code != tc.status {
			t.Fatalf("%s %s got %d want %d body=%s", tc.method, tc.path, w.Code, tc.status, w.Body)
		}
	}
	if f.listCalls+f.sessionCalls+f.journalCalls != 0 {
		t.Fatalf("invalid or unauthorized request reached reader: %+v", f)
	}
}

func TestSessionsListAndTimelinePropagateScopedCursorsAndAsOf(t *testing.T) {
	parent, err := json.Marshal(rt.Parent{ID: "plan-a", Status: "completed", Spec: d.DefaultPlan()})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSessionReader{page: pg.SessionPage{AsOfRevision: 44, Entries: []pg.SessionMeta{{PlanID: "plan-a", CreatedRevision: 9, Revision: 40, Parent: parent, Profile: "paper", DataSource: "replay"}}}, saved: sessionRecord(t, "plan-a", 40, 17, "completed", "buy", "100", "101", nil), journal: []pg.JournalEntry{{Revision: 13, Kind: "create", CreatedAt: time.Unix(1, 0)}}}
	h := sessionsHandler(t, f)
	w := request(h, "GET", pathA+"/sessions?before_revision=22&as_of_revision=44&limit=7", tokenA, "")
	if w.Code != 200 || f.listCalls != 1 || f.listKey != keyA || f.before != 22 || f.listAsOf != 44 || f.limit != 7 {
		t.Fatalf("list propagation status=%d reader=%+v body=%s", w.Code, f, w.Body)
	}
	if strings.Contains(w.Body.String(), "snapshot") || strings.Contains(w.Body.String(), "owner_id") {
		t.Fatalf("list leaked internal state: %s", w.Body)
	}
	w = request(h, "GET", pathA+"/sessions/plan-a/timeline?after_revision=4&as_of_revision=17&limit=5", tokenA, "")
	if w.Code != 200 || f.sessionCalls != 1 || f.journalCalls != 1 || f.sessionKey != keyA || f.planID != "plan-a" || f.sessionAsOf != 17 || f.journalKey != keyA || f.journalPlanID != "plan-a" || f.after != 4 || f.journalAsOf != 17 || f.journalLimit != 5 {
		t.Fatalf("timeline propagation status=%d reader=%+v body=%s", w.Code, f, w.Body)
	}
	for _, secret := range []string{"snapshot", "owner_id", "features", "balances"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("timeline leaked %s: %s", secret, w.Body)
		}
	}
}

func TestSessionDetailRedactsAndUsesHistoricalParent(t *testing.T) {
	f := &fakeSessionReader{saved: sessionRecord(t, "old-plan", 31, 31, "completed", "buy", "100", "101", nil)}
	h, authority, _ := setup(t)
	authority.view.State.Parent = &rt.Parent{ID: "current-new-plan", Status: "active", Spec: d.DefaultPlan()}
	h.sessions = f
	w := request(h, "GET", pathA+"/sessions/old-plan?as_of_revision=31", tokenA, "")
	if w.Code != 200 {
		t.Fatalf("historical report status=%d body=%s", w.Code, w.Body)
	}
	var report SessionReport
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.PlanID != "old-plan" || report.Revision != 31 || report.AsOfRevision != 31 || report.Status != "completed" {
		t.Fatalf("historical report was confused with current account: %+v", report)
	}
	for _, secret := range []string{"raw-snapshot", "private-owner", "owner_id", "last_observation", "balances", "features"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("session report leaked %s: %s", secret, w.Body)
		}
	}
}

func TestSessionDetailRejectsCorruptOrMismatchedStoredSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		saved pg.SessionSnapshot
	}{
		{name: "malformed", saved: pg.SessionSnapshot{AsOfRevision: 7, Snapshot: json.RawMessage(`{"Version":broken`)}},
		{name: "wrong parent", saved: pg.SessionSnapshot{AsOfRevision: 7, Snapshot: sessionState(t, "another-plan", "completed", "buy", "100", "101", nil)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSessionReader{saved: tc.saved}
			h := sessionsHandler(t, f)
			w := request(h, "GET", pathA+"/sessions/plan-a", tokenA, "")
			if w.Code != 503 {
				t.Fatalf("corrupt history status=%d body=%s", w.Code, w.Body)
			}
		})
	}
}

func TestSessionFinancialReportUsesFrozenTerminalReference(t *testing.T) {
	fills := []d.Fill{{ExecutionID: "fill-1", OrderID: "order-1", Side: "buy", Quantity: d.MustDecimal("0.5"), Price: d.MustDecimal("101"), Fee: d.MustDecimal("0.01"), FeeCurrency: "base", Time: 150, Maker: true}}
	saved := sessionRecord(t, "plan-buy", 8, 8, "completed", "buy", "100", "120", fills)
	saved.TerminalRevision = 4
	saved.TerminalAt = time.Unix(40, 0).UTC()
	saved.TerminalSnapshot = sessionState(t, "plan-buy", "completed", "buy", "100", "110", fills)
	f := &fakeSessionReader{saved: saved}
	h := sessionsHandler(t, f)
	w := request(h, "GET", pathA+"/sessions/plan-buy", tokenA, "")
	if w.Code != 200 {
		t.Fatalf("buy report status=%d body=%s", w.Code, w.Body)
	}
	var buy SessionReport
	if err := json.Unmarshal(w.Body.Bytes(), &buy); err != nil {
		t.Fatal(err)
	}
	if buy.ResidualReferenceKind != "first-terminal" || buy.ResidualReferenceRevision != 4 || buy.ResidualReferenceMid == nil || !buy.ResidualReferenceMid.Equal(d.MustDecimal("110")) || buy.ResidualOpportunityQuote == nil || !buy.ResidualOpportunityQuote.Equal(d.MustDecimal("5")) || buy.MarkedCostEstimateQuote == nil || !buy.MarkedCostEstimateQuote.Equal(d.MustDecimal("6.51")) {
		t.Fatalf("buy/base-fee frozen report wrong: %+v", buy)
	}
	if !buy.Market.Valid || !buy.Market.BestBid.Equal(d.MustDecimal("119.5")) {
		t.Fatalf("current report market should stay as-of snapshot while residual uses terminal book: %+v", buy.Market)
	}

	sellFill := d.Fill{ExecutionID: "fill-sell", OrderID: "order-sell", Side: "sell", Quantity: d.MustDecimal("0.5"), Price: d.MustDecimal("99"), Fee: d.MustDecimal("1"), FeeCurrency: "quote", Time: 150}
	sell := sessionRecord(t, "plan-sell", 8, 8, "cancelled", "sell", "100", "120", []d.Fill{sellFill})
	sell.TerminalRevision = 5
	sell.TerminalAt = time.Unix(50, 0).UTC()
	sell.TerminalSnapshot = sessionState(t, "plan-sell", "cancelled", "sell", "100", "90", []d.Fill{sellFill})
	f.saved = sell
	w = request(h, "GET", pathA+"/sessions/plan-sell", tokenA, "")
	if w.Code != 200 {
		t.Fatalf("sell report status=%d body=%s", w.Code, w.Body)
	}
	var sellReport SessionReport
	if err := json.Unmarshal(w.Body.Bytes(), &sellReport); err != nil {
		t.Fatal(err)
	}
	if sellReport.ResidualReferenceMid == nil || !sellReport.ResidualReferenceMid.Equal(d.MustDecimal("90")) || sellReport.ResidualOpportunityQuote == nil || !sellReport.ResidualOpportunityQuote.Equal(d.MustDecimal("5")) || sellReport.MarkedCostEstimateQuote == nil || !sellReport.MarkedCostEstimateQuote.Equal(d.MustDecimal("6.5")) {
		t.Fatalf("sell/quote-fee report wrong: %+v", sellReport)
	}
}

func TestSessionBackendErrorsAreSanitized(t *testing.T) {
	f := &fakeSessionReader{sessionErr: errors.New("secret database connection string")}
	h := sessionsHandler(t, f)
	w := request(h, "GET", pathA+"/sessions/plan", tokenA, "")
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "connection string") {
		t.Fatalf("backend error was not sanitized: %d %s", w.Code, w.Body)
	}
}

func TestSessionFinalityRequiresSettledFinancialState(t *testing.T) {
	for _, tc := range []struct {
		name            string
		mutate          func(*rt.State)
		final, recovery bool
	}{
		{"settled", func(*rt.State) {}, true, false},
		{"feed ended after settlement", func(s *rt.State) { s.Book.Valid = false }, true, false},
		{"pending legacy fact", func(s *rt.State) { s.PendingFacts = []rt.Fact{{ID: "accepted-fill"}} }, false, true},
		{"unresolved fact", func(s *rt.State) { s.UnresolvedFacts = []rt.Fact{{ID: "unknown-fill"}} }, false, true},
		{"resolved fact", func(s *rt.State) { s.UnresolvedFacts = []rt.Fact{{ID: "settled-fill", Resolved: true}} }, true, false},
		{"recovery flag", func(s *rt.State) { s.RecoveryRequired = true }, false, true},
		{"clock discontinuity", func(s *rt.State) { s.ClockDiscontinuity = true }, false, true},
		{"remaining child", func(s *rt.State) { s.Child = &rt.Child{ID: "child"} }, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := sessionRecord(t, "plan-final", 8, 8, "cancelled", "buy", "100", "101", nil)
			state, err := rt.DecodeSnapshot(saved.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(&state)
			saved.Snapshot, err = json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			report, err := buildSessionReport(keyA, "plan-final", saved)
			if err != nil {
				t.Fatal(err)
			}
			if report.KnownFinal != tc.final || report.RecoveryRequired != tc.recovery {
				t.Fatalf("final=%v recovery=%v want %v/%v", report.KnownFinal, report.RecoveryRequired, tc.final, tc.recovery)
			}
		})
	}
}

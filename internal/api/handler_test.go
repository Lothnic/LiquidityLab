package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

const tokenA = "test-token-a-012345678901234567890123456789"
const tokenB = "test-token-b-012345678901234567890123456789"

var keyA = pg.Key{TenantID: "tenant-a", AccountID: "account"}
var keyB = pg.Key{TenantID: "tenant-b", AccountID: "account"}

const pathA = "/v1/accounts/tenant-a/account"

type fakeAuthority struct {
	view    rt.View
	command rt.Command
	calls   int
	err     error
	onView  func(context.Context) (rt.View, error)
}

func (f *fakeAuthority) View(ctx context.Context) (rt.View, error) {
	if f.onView != nil {
		return f.onView(ctx)
	}
	return f.view, nil
}
func (f *fakeAuthority) Do(ctx context.Context, c rt.Command) (pg.Result, error) {
	f.calls++
	f.command = c
	return pg.Result{Revision: 8, Duplicate: true}, f.err
}

type fakeJournal struct {
	key   pg.Key
	after int64
	limit int
	calls int
	err   error
}

func (f *fakeJournal) Journal(ctx context.Context, k pg.Key, a int64, l int) ([]pg.JournalEntry, error) {
	f.calls++
	f.key = k
	f.after = a
	f.limit = l
	return []pg.JournalEntry{{Revision: a + 1, Kind: "cancel", CreatedAt: time.Unix(0, 0)}}, f.err
}
func sampleView(key pg.Key) rt.View {
	book := x.NewBook()
	_ = book.Apply(d.MarketEvent{Kind: "snapshot", Bids: []d.Level{{Price: d.MustDecimal("100"), Quantity: d.One}}, Asks: []d.Level{{Price: d.MustDecimal("101"), Quantity: d.One}}})
	return rt.View{Account: pg.Account{Key: key, Revision: 7, Ready: true, Market: pg.Market{Valid: true, SnapshotID: "snapshot"}, Snapshot: json.RawMessage(`{"secret":"raw-snapshot"}`), Ownership: pg.Ownership{OwnerID: "private-owner", Epoch: 99}, Balances: []pg.Balance{{Currency: "BTC", Total: d.One, Free: d.One}, {Currency: "USD", Total: d.MustDecimal("1000"), Free: d.MustDecimal("1000")}}}, State: rt.State{Profile: rt.ProfilePaper, Rules: d.DefaultRules(), Fees: d.DefaultFees(), Execution: d.DefaultConfig(), BaseCurrency: "BTC", QuoteCurrency: "USD", InstrumentRevision: "rules-1", Book: book, MarketID: "snapshot"}}
}
func setup(t *testing.T) (*Handler, *fakeAuthority, *fakeJournal) {
	t.Helper()
	a := &fakeAuthority{view: sampleView(keyA)}
	b := &fakeAuthority{view: sampleView(keyB)}
	j := &fakeJournal{}
	h, e := New(Config{Accounts: map[pg.Key]Authority{keyA: a, keyB: b}, Credentials: []Credential{{Token: tokenA, Scopes: []pg.Key{keyA}}, {Token: tokenB, Scopes: []pg.Key{keyB}}}, Journal: j})
	if e != nil {
		t.Fatal(e)
	}
	return h, a, j
}
func request(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func bodyJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func TestAuthorizationIsolationAndProjection(t *testing.T) {
	h, a, j := setup(t)
	for _, test := range []struct {
		path, token string
		status      int
	}{{"/healthz", "", 200}, {pathA, "", 401}, {pathA, "wrong", 401}, {pathA, tokenB, 404}, {"/v1/accounts/tenant-a/missing", tokenA, 404}, {pathA, tokenA, 200}} {
		w := request(h, "GET", test.path, test.token, "")
		if w.Code != test.status {
			t.Fatalf("%s status=%d body=%s", test.path, w.Code, w.Body)
		}
	}
	w := request(h, "GET", "/v1/accounts", tokenA, "")
	if strings.Contains(w.Body.String(), "tenant-b") || !strings.Contains(w.Body.String(), "tenant-a") {
		t.Fatal(w.Body)
	}
	a.view.State.Parent = &rt.Parent{ID: "parent", Status: "cancel_requested", Spec: d.DefaultPlan()}
	a.view.State.Child = &rt.Child{ID: "child", State: "cancel_requested", KnownFinal: false, Order: d.ChildOrder{Remaining: d.One}}
	a.view.State.LastObservation = []float32{123}
	a.view.State.PendingFacts = []rt.Fact{{Reason: "private-fact"}}
	w = request(h, "GET", pathA, tokenA, "")
	for _, secret := range []string{"raw-snapshot", "private-owner", "ownership", "lease_until", "last_observation", "private-fact", "pending_facts"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("leaked %s: %s", secret, w.Body)
		}
	}
	if !strings.Contains(w.Body.String(), `"cancel_requested":true`) || !strings.Contains(w.Body.String(), `"known_final":false`) {
		t.Fatal(w.Body)
	}
	w = request(h, "GET", pathA+"/journal?after_revision=3&limit=2", tokenB, "")
	if w.Code != 404 || j.calls != 0 {
		t.Fatalf("journal unauthorized: %d %d", w.Code, j.calls)
	}
	w = request(h, "GET", pathA+"/journal?after_revision=3&limit=2", tokenA, "")
	if w.Code != 200 || j.key != keyA || j.after != 3 || j.limit != 2 || !strings.Contains(w.Body.String(), `"next_after_revision":4`) {
		t.Fatal(w.Body)
	}
}
func TestCommandSurfaceAndReceipts(t *testing.T) {
	h, a, _ := setup(t)
	for _, kind := range []string{"deadline", "dispatch", "decision", "action", "reconcile", "unknown"} {
		w := request(h, "POST", pathA+"/commands", tokenA, fmt.Sprintf(`{"id":"cmd-1","expected_revision":7,"kind":%q}`, kind))
		if w.Code != 400 || a.calls != 0 {
			t.Fatalf("kind=%s status=%d calls=%d", kind, w.Code, a.calls)
		}
	}
	for _, kind := range []string{"start", "pause", "resume", "cancel"} {
		w := request(h, "POST", pathA+"/commands", tokenA, fmt.Sprintf(`{"id":"cmd-1","expected_revision":7,"kind":%q}`, kind))
		if w.Code != 200 || a.command.Kind != kind || a.command.ExpectedRevision != 7 || !strings.Contains(w.Body.String(), `"duplicate":true`) {
			t.Fatal(w.Body)
		}
	}
	p := d.DefaultPlan()
	payload := commandRequest{ID: "create-1", ExpectedRevision: new(int64(7)), Kind: "create", PlanID: "plan-1", Plan: &p, Policy: x.BaselineLiquidityAware}
	w := request(h, "POST", pathA+"/commands", tokenA, bodyJSON(payload))
	if w.Code != 200 || a.command.Plan == nil || !a.command.Plan.Quantity.Equal(p.Quantity) || a.command.Policy != x.BaselineLiquidityAware {
		t.Fatal(w.Body)
	}
	for _, body := range []string{`{"id":"cmd","kind":"cancel"}`, `{"id":"cmd","expected_revision":-1,"kind":"cancel"}`, `{"id":"runtime-internal","expected_revision":7,"kind":"cancel"}`, `{"id":"cmd","expected_revision":7,"kind":"cancel","plan_id":"other"}`, `{"id":"cmd","expected_revision":7,"kind":"create"}`, `{"id":"cmd","expected_revision":7,"kind":"cancel","deadline":"2030-01-01"}`, `{"id":"cmd","expected_revision":7,"kind":"cancel","action":1}`} {
		before := a.calls
		w := request(h, "POST", pathA+"/commands", tokenA, body)
		if w.Code != 400 || a.calls != before {
			t.Fatalf("invalid accepted: %s %s", body, w.Body)
		}
	}
}
func TestSafeErrorMappings(t *testing.T) {
	h, a, _ := setup(t)
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{{pg.ErrStaleRevision, 409, "stale_revision"}, {pg.ErrConflict, 409, "idempotency_conflict"}, {pg.ErrInsufficientFunds, 409, "insufficient_funds"}, {rt.ErrRecoveryRequired, 409, "recovery_required"}, {rt.ErrOutcomeUnknown, 504, "outcome_unknown"}, {rt.ErrMailboxFull, 503, "unavailable"}, {context.Canceled, 408, "request_cancelled"}, {errors.New("postgres password=TOP_SECRET"), 503, "unavailable"}} {
		a.err = test.err
		w := request(h, "POST", pathA+"/commands", tokenA, `{"id":"cmd","expected_revision":7,"kind":"cancel"}`)
		if w.Code != test.status || !strings.Contains(w.Body.String(), test.code) || strings.Contains(w.Body.String(), "TOP_SECRET") {
			t.Fatal(w.Code, w.Body)
		}
	}
}
func TestPreviewUsesExactSharedHelpersWithoutMutation(t *testing.T) {
	h, a, _ := setup(t)
	before := bodyJSON(a.view)
	plan := d.DefaultPlan()
	plan.Quantity = d.MustDecimal("0.100000000000000000000000000000000001")
	w := request(h, "POST", pathA+"/preview", tokenA, bodyJSON(previewRequest{Plan: &plan}))
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	var got PreviewDTO
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	expected, err := x.Compile(d.AggressiveNormal, plan, a.view.State.Rules, a.view.State.Fees, a.view.State.Execution, x.NewLedger(plan), a.view.State.Book, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, reserved, err := x.ReservationFor(*expected.Intent, a.view.State.Fees)
	if err != nil {
		t.Fatal(err)
	}
	if got.Intent == nil || !got.Intent.Equal(*expected.Intent) || got.Reservation.Amount.String() != reserved.String() || got.CompletionGuaranteed || got.Market.SnapshotID != "snapshot" {
		t.Fatalf("preview mismatch: %s", w.Body)
	}
	if before != bodyJSON(a.view) || a.calls != 0 {
		t.Fatal("preview mutated authority")
	}
	numberBody := strings.Replace(bodyJSON(previewRequest{Plan: &plan}), `"limit_price":"102"`, `"limit_price":102`, 1)
	w = request(h, "POST", pathA+"/preview", tokenA, numberBody)
	if w.Code != 400 || a.calls != 0 {
		t.Fatal(w.Body)
	}
	plan.QuoteBudget = d.MustDecimal("1001")
	w = request(h, "POST", pathA+"/preview", tokenA, bodyJSON(previewRequest{Plan: &plan}))
	if w.Code != 409 {
		t.Fatal(w.Body)
	}
	plan = d.DefaultPlan()
	a.view.State.Book.Valid = false
	w = request(h, "POST", pathA+"/preview", tokenA, bodyJSON(previewRequest{Plan: &plan}))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"intent":null`) {
		t.Fatal(w.Body)
	}
}
func TestRequestBoundsAndMethods(t *testing.T) {
	h, a, _ := setup(t)
	h.maxBody = 256
	bodies := []struct {
		body   string
		status int
	}{{strings.Repeat(" ", 257), 413}, {`{"id":"cmd","expected_revision":7,"kind":"cancel"} {}`, 400}, {`{"id":"cmd","expected_revision":7,"kind":"cancel","extra":true}`, 400}, {`null`, 400}, {`{`, 400}, {`{"id":"cmd","expected_revision":7,"kind":"cancel"}` + strings.Repeat(" ", 257), 413}}
	for _, tt := range bodies {
		w := request(h, "POST", pathA+"/commands", tokenA, tt.body)
		if w.Code != tt.status || a.calls != 0 {
			t.Fatalf("body status %d want %d: %s", w.Code, tt.status, w.Body)
		}
	}
	for _, path := range []string{pathA + "/journal?limit=201", pathA + "/journal?limit=0", pathA + "/journal?after_revision=-1", pathA + "/journal?limit=1&limit=2", pathA + "/journal?unknown=1", pathA + "/journal?limit=%zz", pathA + "?limit=1"} {
		w := request(h, "GET", path, tokenA, "")
		if w.Code != 400 {
			t.Fatal(path, w.Code, w.Body)
		}
	}
	for _, tt := range []struct{ method, path, allow string }{{"POST", pathA, "GET"}, {"GET", pathA + "/commands", "POST"}, {"DELETE", "/healthz", "GET"}, {"POST", pathA + "/journal", "GET"}} {
		w := request(h, tt.method, tt.path, tokenA, "")
		if w.Code != 405 || w.Header().Get("Allow") != tt.allow {
			t.Fatal(w.Code, w.Header())
		}
	}
	r := httptest.NewRequest("POST", pathA+"/commands", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+tokenA)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatal(w.Code)
	}
}
func TestCancellationAdmissionAndRegistryCopy(t *testing.T) {
	a := &fakeAuthority{view: sampleView(keyA)}
	entered := make(chan struct{})
	var once sync.Once
	a.onView = func(ctx context.Context) (rt.View, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return rt.View{}, ctx.Err()
	}
	accounts := map[pg.Key]Authority{keyA: a}
	scopes := []pg.Key{keyA}
	h, err := New(Config{Accounts: accounts, Credentials: []Credential{{Token: tokenA, Scopes: scopes}}, Journal: &fakeJournal{}, MaxConcurrent: 1, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	delete(accounts, keyA)
	scopes[0] = keyB
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", pathA, nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+tokenA)
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(first, r); close(done) }()
	<-entered
	w := request(h, "GET", pathA, tokenA, "")
	if w.Code != 503 || w.Header().Get("Retry-After") != "1" {
		t.Fatal(w.Code)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request ignored cancellation")
	}
	if first.Code != 408 {
		t.Fatal(first.Code)
	}
}
func TestCredentialValidationAndDTOIdentity(t *testing.T) {
	h, a, _ := setup(t)
	a.view.Account.Key = keyB
	for _, route := range []string{pathA, "/v1/accounts", pathA + "/preview"} {
		method, body := "GET", ""
		if strings.HasSuffix(route, "preview") {
			method = "POST"
			p := d.DefaultPlan()
			body = bodyJSON(previewRequest{Plan: &p})
		}
		w := request(h, method, route, tokenA, body)
		if w.Code != 503 {
			t.Fatal(route, w.Code)
		}
	}
	base := Config{Accounts: map[pg.Key]Authority{keyA: &fakeAuthority{}}, Credentials: []Credential{{Token: tokenA, Scopes: []pg.Key{keyA}}}, Journal: &fakeJournal{}}
	for _, mutate := range []func(*Config){func(c *Config) { c.Credentials[0].Token = "short" }, func(c *Config) { c.Credentials = append(c.Credentials, c.Credentials[0]) }, func(c *Config) { c.Credentials[0].Scopes = []pg.Key{keyB} }, func(c *Config) { c.MaxBodyBytes = 1 }, func(c *Config) { c.RequestTimeout = time.Minute }, func(c *Config) { c.MaxConcurrent = -1 }} {
		c := base
		c.Credentials = append([]Credential{}, base.Credentials...)
		mutate(&c)
		if _, err := New(c); err == nil {
			t.Fatal("accepted bad config")
		}
	}
	if reflect.DeepEqual(h.credentials[0].digest, [32]byte{}) {
		t.Fatal("missing digest")
	}
}

func TestPreviewReservationsAndExistingParent(t *testing.T) {
	h, a, _ := setup(t)
	plan := d.DefaultPlan()
	plan.QuoteBudget = d.MustDecimal("900")
	a.view.Account.Reservations = []pg.Reservation{{Currency: "USD", Amount: d.MustDecimal("101"), State: "held", Reflected: false}}
	w := request(h, "POST", pathA+"/preview", tokenA, bodyJSON(previewRequest{Plan: &plan}))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "insufficient_funds") {
		t.Fatal(w.Code, w.Body)
	}
	a.view.Account.Reservations[0].Reflected = true
	w = request(h, "POST", pathA+"/preview", tokenA, bodyJSON(previewRequest{Plan: &plan}))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	a.view.Account.Reservations[0].Reflected = false
	a.view.Account.Reservations[0].State = "released"
	w = request(h, "POST", pathA+"/preview", tokenA, bodyJSON(previewRequest{Plan: &plan}))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	for _, status := range []string{"active", "paused", "cancel_requested"} {
		a.view.State.Parent = &rt.Parent{ID: "existing", Status: status}
		w = request(h, "POST", pathA+"/preview", tokenA, bodyJSON(previewRequest{Plan: &plan}))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"ready":false`) || !strings.Contains(w.Body.String(), `"reason":"creation_unavailable"`) || !strings.Contains(w.Body.String(), `"intent":null`) {
			t.Fatal(w.Code, w.Body)
		}
	}
	if a.calls != 0 {
		t.Fatal("preview mutated account")
	}
}
func TestExecutedCostBenchmarkAndMissingBenchmark(t *testing.T) {
	h, a, _ := setup(t)
	plan := d.DefaultPlan()
	a.view.State.Parent = &rt.Parent{ID: "parent", Spec: plan, ArrivalMid: d.MustDecimal("100")}
	a.view.State.Fills = []d.Fill{{ExecutionID: "exec-1", OrderID: "child-1", Side: "buy", Quantity: d.MustDecimal(".05"), Price: d.MustDecimal("101"), Fee: d.MustDecimal(".0101"), FeeCurrency: "quote"}}
	w := request(h, "GET", pathA, tokenA, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var got AccountDTO
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Plan.BenchmarkValid || got.Plan.ExecutedCostQuote == nil || got.Plan.ExecutedCostQuote.String() != "0.0601" {
		t.Fatal(w.Body)
	}
	a.view.State.Parent.ArrivalMid = d.Zero
	w = request(h, "GET", pathA, tokenA, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"benchmark_valid":false`) || !strings.Contains(w.Body.String(), `"executed_cost_quote":null`) {
		t.Fatal(w.Code, w.Body)
	}
}

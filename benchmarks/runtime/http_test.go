package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	ar "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

type pollingBackend struct {
	key  pg.Key
	view ar.View
}

func (b *pollingBackend) View(context.Context) (ar.View, error) { return b.view, nil }
func (b *pollingBackend) Do(context.Context, ar.Command) (pg.Result, error) {
	return pg.Result{}, errors.New("read-only benchmark fixture")
}
func (b *pollingBackend) Journal(context.Context, pg.Key, int64, int) ([]pg.JournalEntry, error) {
	return nil, nil
}
func (b *pollingBackend) Sessions(_ context.Context, key pg.Key, _, _ int64, _ int) (pg.SessionPage, error) {
	if key != b.key {
		return pg.SessionPage{}, pg.ErrNotFound
	}
	parent, _ := json.Marshal(b.view.State.Parent)
	return pg.SessionPage{AsOfRevision: 3, Entries: []pg.SessionMeta{{PlanID: "benchmark-plan", CreatedRevision: 2, Revision: 3, Parent: parent, Profile: "paper", DataSource: "synthetic"}}}, nil
}
func (b *pollingBackend) Session(_ context.Context, key pg.Key, id string, _ int64) (pg.SessionSnapshot, error) {
	if key != b.key || id != "benchmark-plan" {
		return pg.SessionSnapshot{}, pg.ErrNotFound
	}
	raw, _ := json.Marshal(b.view.State)
	return pg.SessionSnapshot{AsOfRevision: 3, Revision: 3, CreatedRevision: 2, Snapshot: raw}, nil
}
func (b *pollingBackend) SessionJournal(context.Context, pg.Key, string, int64, int64, int) ([]pg.JournalEntry, error) {
	return nil, nil
}
func newPollingBackend(t *testing.T) *pollingBackend {
	t.Helper()
	key := pg.Key{TenantID: "bench-tenant", AccountID: "bench-account"}
	book := x.NewBook()
	if err := book.Apply(d.MarketEvent{Kind: "snapshot", Bids: []d.Level{{Price: d.MustDecimal("99"), Quantity: d.One}}, Asks: []d.Level{{Price: d.MustDecimal("101"), Quantity: d.One}}}); err != nil {
		t.Fatal(err)
	}
	return &pollingBackend{key: key, view: ar.View{Account: pg.Account{Key: key, Revision: 3, Ready: true, Balances: []pg.Balance{{Currency: "BTC", Total: d.One, Free: d.One}, {Currency: "USD", Total: d.MustDecimal("1000"), Free: d.MustDecimal("1000")}}}, State: ar.State{Version: ar.StateVersion, Profile: ar.ProfilePaper, DataSource: "synthetic", Rules: d.DefaultRules(), Fees: d.DefaultFees(), Execution: d.DefaultConfig(), BaseCurrency: "BTC", QuoteCurrency: "USD", Book: book, Parent: &ar.Parent{ID: "benchmark-plan", Revision: 2, Status: "active", Started: true, Spec: d.DefaultPlan(), Start: time.Now(), Deadline: time.Now().Add(time.Minute), ArrivalMid: d.MustDecimal("100")}}}}
}

func TestHTTPPollingUsesScopedProductRoutesAndConsumesBodies(t *testing.T) {
	c := testConfig()
	c.ReadMode = "http"
	backend := newPollingBackend(t)
	p, err := startHTTPPoller(c, backend.key, backend, backend, backend)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, route := range []string{"state", "sessions", "report"} {
		out, err := p.Read(context.Background(), input{ReadRoute: route})
		if err != nil || out.Status != 200 || out.Bytes == 0 {
			t.Fatalf("route %s out=%+v err=%v", route, out, err)
		}
	}
	for _, test := range []struct {
		path, token string
		status      int
	}{{p.accountPath, "", 401}, {p.accountPath, "wrong", 401}, {"/v1/accounts/another-tenant/another-account", p.token, 404}} {
		req, _ := http.NewRequest(http.MethodGet, p.baseURL+test.path, nil)
		if test.token != "" {
			req.Header.Set("Authorization", "Bearer "+test.token)
		}
		response, err := p.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != test.status {
			t.Fatalf("scope/auth status=%d want=%d", response.StatusCode, test.status)
		}
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	default:
		t.Fatal("HTTP server did not stop")
	}
}

func TestHTTPResponseBoundsAndCompleteConsumption(t *testing.T) {
	for _, kind := range []string{"delayed_body", "oversize", "bad_json", "busy", "gateway_timeout"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("missing credential")
				}
				switch kind {
				case "delayed_body":
					_, _ = io.WriteString(w, `{"value":"`)
					w.(http.Flusher).Flush()
					time.Sleep(15 * time.Millisecond)
					_, _ = io.WriteString(w, `complete"}`)
				case "oversize":
					_, _ = io.WriteString(w, strings.Repeat("x", int(maxHTTPResponseBytes)+1))
				case "bad_json":
					_, _ = io.WriteString(w, "invalid")
				case "busy":
					w.WriteHeader(503)
					_, _ = io.WriteString(w, `{"error":"busy"}`)
				case "gateway_timeout":
					w.WriteHeader(http.StatusGatewayTimeout)
					_, _ = io.WriteString(w, `{"error":"outcome_unknown"}`)
				}
			}))
			defer srv.Close()
			p := &httpPoller{client: srv.Client(), baseURL: srv.URL, accountPath: "/state", token: "test-token"}
			start := time.Now()
			out, err := p.Read(context.Background(), input{ReadRoute: "state"})
			switch kind {
			case "delayed_body":
				if err != nil || out.Bytes != 20 || time.Since(start) < 15*time.Millisecond {
					t.Fatalf("body not consumed: out=%+v err=%v", out, err)
				}
			case "busy":
				if !errors.Is(err, errReadRejected) {
					t.Fatalf("busy not rejected: %v", err)
				}
			case "gateway_timeout":
				if out.Status != http.StatusGatewayTimeout || out.Bytes == 0 || !errors.Is(err, context.DeadlineExceeded) || classify(err) != "outcome_unknown" {
					t.Fatalf("gateway timeout lost unresolved outcome: status=%d bytes=%d err=%v", out.Status, out.Bytes, err)
				}
			default:
				if err == nil {
					t.Fatal("invalid response accepted")
				}
			}
		})
	}
}

func TestHTTPMixIsExplicitAndBounded(t *testing.T) {
	c := testConfig()
	c.ReadMode = "http"
	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		counts[c.readRoute(i)]++
	}
	if counts["state"] != 60 || counts["sessions"] != 20 || counts["report"] != 20 {
		t.Fatalf("mix=%v", counts)
	}
	c.HTTPReportPercent = 30
	if c.validate() == nil {
		t.Fatal("non-100 mix accepted")
	}
}

func TestHTTPTimeoutNeverCountsAsCompleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"unfinished":"`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 15 * time.Millisecond
	p := &httpPoller{client: client, baseURL: srv.URL, accountPath: "/state", token: "test"}
	_, err := p.Read(context.Background(), input{ReadRoute: "state"})
	if !errors.Is(err, context.DeadlineExceeded) || classify(err) != "outcome_unknown" {
		t.Fatalf("timeout was not unresolved: %v", err)
	}
}

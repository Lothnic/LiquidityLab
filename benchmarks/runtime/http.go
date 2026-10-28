package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/liquiditylab/liquiditylab/internal/api"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	ar "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

const maxHTTPResponseBytes int64 = 1 << 20

var errReadRejected = errors.New("HTTP polling admission rejected")

type readResult struct {
	Status int
	Bytes  int64
}

// Product projections can contain derived decimal values wider than public
// command inputs (for example a repeating average price). Keep input amounts
// on the normal domain decoder and use its internal envelope for these outputs.
type responseDecimal struct{ d.Decimal }

func (v *responseDecimal) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return errors.New("invalid response decimal")
	}
	value, err := d.ParseInternalDecimal(text)
	if err != nil || value.String() != text {
		return errors.New("invalid response decimal")
	}
	v.Decimal = value
	return nil
}

type pollingPlanDTO struct {
	api.PlanDTO
	AveragePrice      responseDecimal  `json:"average_price"`
	ExecutedCostQuote *responseDecimal `json:"executed_cost_quote"`
}
type readFunc func(context.Context, input) (readResult, error)
type httpPoller struct {
	server                      *http.Server
	listener                    net.Listener
	client                      *http.Client
	transport                   *http.Transport
	baseURL, token, accountPath string
	done                        chan struct{}
	once                        sync.Once
	closeErr                    error
	key                         pg.Key
	plan                        d.Plan
	baseline, base, quote       string
}

func startHTTPPoller(c config, key pg.Key, authority api.Authority, journal api.JournalReader, sessions api.SessionReader) (*httpPoller, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.HTTPTimeout)
	view, err := authority.View(ctx)
	cancel()
	if err != nil || view.Account.Key != key || view.State.Parent == nil || view.State.Parent.ID != "benchmark-plan" || view.State.Parent.Spec.Validate() != nil {
		return nil, errors.New("HTTP polling account binding failed")
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, errors.New("HTTP credential generation failed")
	}
	token := hex.EncodeToString(secret[:])
	handler, err := api.New(api.Config{Accounts: map[pg.Key]api.Authority{key: authority}, Credentials: []api.Credential{{Token: token, Scopes: []pg.Key{key}}}, Journal: journal, Sessions: sessions, MaxConcurrent: c.InFlight, RequestTimeout: c.HTTPTimeout})
	if err != nil {
		return nil, errors.New("HTTP polling API configuration failed")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("HTTP polling listener failed")
	}
	transport := &http.Transport{Proxy: nil, MaxConnsPerHost: c.InFlight, MaxIdleConns: c.InFlight, MaxIdleConnsPerHost: c.InFlight, IdleConnTimeout: 5 * time.Second, ResponseHeaderTimeout: c.HTTPTimeout, DisableCompression: true}
	p := &httpPoller{listener: listener, transport: transport, token: token, baseURL: "http://" + listener.Addr().String(), accountPath: "/v1/accounts/" + key.TenantID + "/" + key.AccountID, done: make(chan struct{})}
	p.key, p.plan, p.baseline = key, view.State.Parent.Spec, view.State.Parent.Baseline
	p.base, p.quote = view.State.BaseCurrency, view.State.QuoteCurrency
	p.client = &http.Client{Transport: transport, Timeout: c.HTTPTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	p.server = &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8 << 10}
	go func() { defer close(p.done); _ = p.server.Serve(listener) }()
	return p, nil
}

func (p *httpPoller) Read(ctx context.Context, in input) (readResult, error) {
	path := p.accountPath
	switch in.ReadRoute {
	case "state":
	case "sessions":
		path += "/sessions?limit=1"
	case "report":
		path += "/sessions/benchmark-plan"
	default:
		return readResult{}, errors.New("invalid HTTP read route")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path, nil)
	if err != nil {
		return readResult{}, errors.New("HTTP polling request construction failed")
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	response, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return readResult{}, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return readResult{}, context.DeadlineExceeded
		}
		return readResult{}, errors.New("HTTP polling request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxHTTPResponseBytes+1))
	out := readResult{Status: response.StatusCode, Bytes: int64(len(data))}
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return out, context.DeadlineExceeded
		}
		return out, errors.New("HTTP response consumption failed")
	}
	if int64(len(data)) > maxHTTPResponseBytes {
		return out, errors.New("HTTP response exceeded bounded size")
	}
	// Timing includes full body consumption and JSON syntax validation. No body,
	// bearer token, endpoint address, or raw network error is retained in reports.
	var object map[string]json.RawMessage
	if err = json.Unmarshal(data, &object); err != nil || object == nil {
		return out, errors.New("HTTP response JSON invalid")
	}
	if response.StatusCode == http.StatusServiceUnavailable {
		return out, errReadRejected
	}
	if response.StatusCode == http.StatusGatewayTimeout {
		return out, context.DeadlineExceeded
	}
	if response.StatusCode != http.StatusOK {
		return out, errors.New("HTTP polling status failed")
	}
	if err := p.validateProjection(in.ReadRoute, data); err != nil {
		return out, err
	}
	return out, nil
}

// Validate the product DTOs against the account and immutable parent captured
// before warmup. Bodies remain bounded and never enter reports or error text.
func (p *httpPoller) validateProjection(route string, data []byte) error {
	invalid := errors.New("HTTP polling projection identity or contract mismatch")
	decode := func(target any) error {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		return decoder.Decode(target)
	}
	validRevision := func(revision, asOf int64) bool { return revision > 0 && revision <= asOf }
	validPlan := func(plan *pollingPlanDTO) bool {
		if plan == nil || plan.ID != "benchmark-plan" || plan.Revision <= 0 || plan.Spec.Validate() != nil || !samePollingPlan(plan.Spec, p.plan) {
			return false
		}
		if plan.Policy != p.baseline || x.ValidateBaselinePolicy(plan.Policy) != nil || !validPollingStatus(plan.Status) {
			return false
		}
		return plan.Filled.Sign() >= 0 && plan.Remaining.Sign() >= 0 && plan.Filled.Add(plan.Remaining).Equal(plan.Spec.Quantity) && (plan.Status != "completed" || plan.Remaining.IsZero())
	}
	validSummary := func(summary api.SessionSummary, asOf int64) bool {
		if summary.PlanID != "benchmark-plan" || summary.Profile != string(ar.ProfilePaper) || summary.DataSource != "synthetic" || summary.BaseCurrency != p.base || summary.QuoteCurrency != p.quote {
			return false
		}
		if !validRevision(summary.CreatedRevision, summary.Revision) || !validRevision(summary.Revision, asOf) || !validPollingStatus(summary.Status) {
			return false
		}
		return summary.Side == p.plan.Side && summary.Symbol == p.plan.Symbol && summary.Quantity.Equal(p.plan.Quantity) && summary.LimitPrice.Equal(p.plan.LimitPrice) && summary.Policy == p.baseline
	}
	switch route {
	case "state":
		var dto struct {
			api.AccountDTO
			Plan *pollingPlanDTO `json:"plan"`
		}
		if decode(&dto) != nil || dto.TenantID != p.key.TenantID || dto.AccountID != p.key.AccountID || dto.Profile != ar.ProfilePaper || dto.DataSource != "synthetic" {
			return invalid
		}
		if !validRevision(dto.AuthorityRevision, dto.Revision) || dto.BaseCurrency != p.base || dto.QuoteCurrency != p.quote || dto.Rules.Validate() != nil || dto.Rules.Symbol != p.plan.Symbol || dto.Fees.Validate() != nil {
			return invalid
		}
		if !validPlan(dto.Plan) || dto.PlanStatus != dto.Plan.Status || len(dto.Balances) > 128 || len(dto.Reserved) > 128 || len(dto.Fills) > 2048 || dto.FillCount < len(dto.Fills) {
			return invalid
		}
	case "sessions":
		var dto struct {
			Entries []api.SessionSummary `json:"entries"`
			AsOf    int64                `json:"as_of_revision"`
			Next    int64                `json:"next_before_revision"`
			Limit   int                  `json:"limit"`
		}
		if decode(&dto) != nil || dto.Limit != 1 || len(dto.Entries) != 1 || !validSummary(dto.Entries[0], dto.AsOf) || dto.Next != dto.Entries[0].CreatedRevision {
			return invalid
		}
	case "report":
		var dto struct {
			api.SessionReport
			Plan                     *pollingPlanDTO  `json:"plan"`
			CompletionPercent        responseDecimal  `json:"completion_percent"`
			ResidualReferenceMid     *responseDecimal `json:"residual_reference_mid"`
			ResidualOpportunityQuote *responseDecimal `json:"residual_opportunity_quote"`
			MarkedCostEstimateQuote  *responseDecimal `json:"marked_cost_estimate_quote"`
		}
		if decode(&dto) != nil || dto.TenantID != p.key.TenantID || dto.AccountID != p.key.AccountID || dto.Version != "session-report-v1" {
			return invalid
		}
		if !validSummary(dto.SessionSummary, dto.AsOfRevision) || !validPlan(dto.Plan) || dto.Plan.Status != dto.Status || dto.Rules.Validate() != nil || dto.Rules.Symbol != p.plan.Symbol || dto.Fees.Validate() != nil || dto.Simulation.Validate() != nil {
			return invalid
		}
		if len(dto.Fills) > 4096 || !dto.CompletionPercent.Equal(dto.Plan.Filled.Mul(d.MustDecimal("100")).Quo(p.plan.Quantity)) {
			return invalid
		}
	default:
		return invalid
	}
	return nil
}

func samePollingPlan(a, b d.Plan) bool {
	return a.Side == b.Side && a.Symbol == b.Symbol && a.Deadline == b.Deadline && a.Quantity.Equal(b.Quantity) && a.LimitPrice.Equal(b.LimitPrice) && a.MaxChildQuantity.Equal(b.MaxChildQuantity) && a.QuoteBudget.Equal(b.QuoteBudget) && a.BaseBalance.Equal(b.BaseBalance)
}

func validPollingStatus(status string) bool {
	switch status {
	case "active", "paused", "cancel_requested", "completed", "cancelled", "expired":
		return true
	}
	return false
}

func (p *httpPoller) Close() error {
	if p == nil {
		return nil
	}
	p.once.Do(func() {
		p.transport.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		p.closeErr = p.server.Shutdown(ctx)
		if p.closeErr != nil {
			_ = p.server.Close()
		}
		<-p.done
	})
	return p.closeErr
}

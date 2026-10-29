package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/liquiditylab/liquiditylab/internal/api"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

const (
	loadRecoveryRate       = 20
	loadRecoverySlots      = 4
	loadRecoveryTimeout    = 750 * time.Millisecond
	loadRecoveryBodyLimit  = 1 << 20
	loadRecoveryDuration   = 10 * time.Second
	loadRecoveryPollPeriod = 20 * time.Millisecond
)

type loadRecoverySample struct {
	Role       string `json:"role"`
	Route      string `json:"route"`
	Index      int    `json:"index"`
	IntendedNS int64  `json:"intended_ns"`
	AttemptNS  int64  `json:"attempt_ns"`
	AdmittedNS *int64 `json:"admitted_ns"`
	CallNS     *int64 `json:"call_ns"`
	EndNS      int64  `json:"end_ns"`
	Status     int    `json:"http_status"`
	Bytes      int    `json:"response_bytes"`
	Outcome    string `json:"outcome"`
	CrashPhase string `json:"phase"`
}

type loadRecoveryCounts struct {
	Offered   int            `json:"offered"`
	Admitted  int            `json:"admitted"`
	Outcomes  map[string]int `json:"outcomes"`
	ReadRoute map[string]int `json:"offered_routes"`
}

type loadRecoveryEvents struct {
	KillIssuedNS      int64 `json:"sigkill_issued_ns"`
	ReapedNS          int64 `json:"sigkill_reaped_ns"`
	LaunchNS          int64 `json:"cold_launch_started_ns"`
	ListenerNS        int64 `json:"listener_observed_ns"`
	ReadyNS           int64 `json:"first_ready_paused_observed_ns"`
	ProofEndNS        int64 `json:"paused_proof_finished_ns"`
	LoadDrainedNS     int64 `json:"read_schedules_and_calls_drained_ns"`
	ProcessesReapedNS int64 `json:"graceful_processes_reaped_ns"`
	OutageNS          int64 `json:"kill_to_first_ready_paused_ns"`
	LaunchReadyNS     int64 `json:"cold_launch_to_first_ready_paused_ns"`
}

type loadRecoveryFact struct {
	Sequence int64  `json:"sequence"`
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	SHA256   string `json:"payload_sha256"`
}

type loadRecoveryAuditBoundary struct {
	Phase         string `json:"phase"`
	ProcessReaped bool   `json:"process_reaped"`
	KnownFinal    bool   `json:"known_final"`
}

const (
	loadRecoveryPausedBoundary   = "recovered_ready_paused"
	loadRecoveryShutdownBoundary = "after_reaped_known_final_shutdown"
)

type loadRecoveryAudit struct {
	Boundary           loadRecoveryAuditBoundary `json:"boundary"`
	Ready              bool                      `json:"account_ready"`
	MarketValid        bool                      `json:"market_valid"`
	BookValid          bool                      `json:"book_valid"`
	ClockDiscontinuity bool                      `json:"clock_discontinuity"`
	RecoveryRequired   bool                      `json:"recovery_required"`
	Reason             string                    `json:"reason"`
	MarketID           string                    `json:"market_id"`

	Valid        bool         `json:"valid"`
	Issues       []string     `json:"issues"`
	Status       string       `json:"status"`
	Epoch        int64        `json:"ownership_epoch"`
	Observed     int64        `json:"observed_sequence"`
	Processed    int64        `json:"processed_sequence"`
	Balances     []pg.Balance `json:"exact_balances"`
	Accepted     []d.Fill     `json:"accepted_fills"`
	Filled       d.Decimal    `json:"filled_quantity"`
	Remaining    d.Decimal    `json:"remaining_quantity"`
	FeesQuote    d.Decimal    `json:"fees_quote"`
	PinsRetained bool         `json:"original_pins_retained"`
}

type loadRecoveryReport struct {
	Backlog         *loadRecoveryBacklog            `json:"harness_authored_backlog,omitempty"`
	Issues          []string                        `json:"issues"`
	Version         string                          `json:"report_version"`
	Valid           bool                            `json:"valid"`
	Status          string                          `json:"status"`
	StartedUTC      time.Time                       `json:"schedule_origin_utc"`
	Binary          map[string]any                  `json:"ordinary_daemon_binary"`
	Workload        map[string]any                  `json:"workload"`
	Accounts        map[string]pg.Key               `json:"accounts"`
	Initial         []pg.Balance                    `json:"initial_balances_per_account"`
	Events          loadRecoveryEvents              `json:"events"`
	CrashEpoch      int64                           `json:"ownership_epoch_after_sigkill"`
	CrashObserved   int64                           `json:"observed_sequence_after_sigkill"`
	CrashProcessed  int64                           `json:"processed_sequence_after_sigkill"`
	OriginalPlans   map[string]*api.PlanDTO         `json:"original_plans"`
	CrashDebt       int64                           `json:"observed_debt_after_sigkill"`
	CrashFacts      []loadRecoveryFact              `json:"accepted_observations_after_sigkill"`
	Recovered       loadRecoveryAudit               `json:"victim_recovered_paused"`
	Final           map[string]loadRecoveryAudit    `json:"final_audits"`
	Counts          map[string]loadRecoveryCounts   `json:"counts"`
	Samples         map[string][]loadRecoverySample `json:"samples"`
	StartRetry      bool                            `json:"original_start_retry_duplicate"`
	KnownFinal      map[string]bool                 `json:"graceful_known_final"`
	Reaped          map[string]bool                 `json:"processes_reaped"`
	PollersDrained  bool                            `json:"all_pollers_drained"`
	BystanderBefore int64                           `json:"bystander_observed_sequence_at_crash"`
	BystanderAfter  int64                           `json:"bystander_observed_sequence_after_load"`
	Limitations     []string                        `json:"measurement_boundaries"`
}

// The URL is process-local control state and never enters retained evidence.
type loadRecoveryTarget struct{ url atomic.Value }

func (v *loadRecoveryTarget) set(url string) { v.url.Store(url) }
func (v *loadRecoveryTarget) get() string {
	if value := v.url.Load(); value != nil {
		return value.(string)
	}
	return ""
}

type loadRecoveryPoller struct {
	samples   []loadRecoverySample
	done      chan struct{}
	cancel    context.CancelFunc
	transport *http.Transport
}

func startLoadRecoveryPoller(parent context.Context, origin time.Time, duration time.Duration, role string, target *loadRecoveryTarget, key pg.Key, expected *api.PlanDTO) *loadRecoveryPoller {
	ctx, cancel := context.WithCancel(parent)
	transport := &http.Transport{Proxy: nil, MaxConnsPerHost: loadRecoverySlots, MaxIdleConns: loadRecoverySlots, MaxIdleConnsPerHost: loadRecoverySlots, ResponseHeaderTimeout: loadRecoveryTimeout, DisableCompression: true}
	client := &http.Client{Transport: transport, Timeout: loadRecoveryTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	count := int(duration / (time.Second / loadRecoveryRate))
	p := &loadRecoveryPoller{samples: make([]loadRecoverySample, count), done: make(chan struct{}), cancel: cancel, transport: transport}
	go func() {
		defer close(p.done)
		var calls sync.WaitGroup
		slots := make(chan struct{}, loadRecoverySlots)
		for index := range p.samples {
			s := &p.samples[index]
			s.Role, s.Index = role, index
			s.Route = []string{"state", "state", "sessions", "state", "report"}[index%5]
			s.IntendedNS = int64(index) * int64(time.Second/loadRecoveryRate)
			if wait := time.Duration(s.IntendedNS) - time.Since(origin); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-ctx.Done():
				}
				timer.Stop()
			}
			s.AttemptNS = time.Since(origin).Nanoseconds()
			s.EndNS = s.AttemptNS
			if ctx.Err() != nil {
				s.Outcome = "not_attempted_cancelled"
				continue
			}
			url := target.get()
			if url == "" {
				s.Outcome = "endpoint_unbound"
				continue
			}
			select {
			case slots <- struct{}{}:
				admitted := time.Since(origin).Nanoseconds()
				s.AdmittedNS = &admitted
				calls.Add(1)
				go func(s *loadRecoverySample, url string) {
					defer calls.Done()
					defer func() { <-slots }()
					call := time.Since(origin).Nanoseconds()
					s.CallNS = &call
					loadRecoveryRead(ctx, client, url, key, expected, s)
					s.EndNS = time.Since(origin).Nanoseconds()
				}(s, url)
			default:
				s.Outcome = "runner_rejected"
			}
		}
		if wait := duration - time.Since(origin); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
			timer.Stop()
		}
		calls.Wait()
		transport.CloseIdleConnections()
	}()
	return p
}

func (p *loadRecoveryPoller) close() {
	p.cancel()
	<-p.done
	p.transport.CloseIdleConnections()
}

func loadRecoveryRead(ctx context.Context, client *http.Client, url string, key pg.Key, expected *api.PlanDTO, s *loadRecoverySample) {
	path := "/v1/accounts/" + key.TenantID + "/" + key.AccountID
	switch s.Route {
	case "sessions":
		path += "/sessions?limit=1"
	case "report":
		path += "/sessions/" + expected.ID
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+path, nil)
	if err != nil {
		s.Outcome = "request_failed"
		return
	}
	req.Header.Set("Authorization", "Bearer "+daemonTestToken)
	response, err := client.Do(req)
	if err != nil {
		s.Outcome = "transport_failed"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			s.Outcome = "response_unresolved"
		}
		return
	}
	defer response.Body.Close()
	s.Status = response.StatusCode
	body, err := io.ReadAll(io.LimitReader(response.Body, loadRecoveryBodyLimit+1))
	s.Bytes = len(body)
	if err != nil {
		s.Outcome = "response_unresolved"
		return
	}
	if len(body) > loadRecoveryBodyLimit {
		s.Outcome = "response_too_large"
		return
	}
	if s.Status != http.StatusOK {
		s.Outcome = "http_failed"
		if s.Status == http.StatusServiceUnavailable {
			s.Outcome = "http_rejected"
		}
		return
	}
	if !loadRecoveryProjection(body, s.Route, key, expected) {
		s.Outcome = "projection_failed"
		return
	}
	s.Outcome = "completed"
}

func loadRecoveryProjection(body []byte, route string, key pg.Key, expected *api.PlanDTO) bool {
	if expected == nil {
		return false
	}
	decode := func(value any) error {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(value); err != nil {
			return err
		}
		if decoder.Decode(new(any)) != io.EOF {
			return errors.New("trailing response data")
		}
		return nil
	}
	planOK := func(plan *api.PlanDTO) bool {
		return plan != nil && plan.ID == expected.ID && plan.Revision > 0 && reflect.DeepEqual(plan.Spec, expected.Spec) && plan.Policy == expected.Policy && plan.Start.Equal(expected.Start) && plan.Deadline.Equal(expected.Deadline) && plan.ArrivalMid.Equal(expected.ArrivalMid) && plan.Filled.Sign() >= 0 && plan.Remaining.Sign() > 0 && plan.Started && (plan.Status == "active" || plan.Status == "paused") && plan.Filled.Add(plan.Remaining).Equal(plan.Spec.Quantity)
	}
	summaryOK := func(summary api.SessionSummary, asOf int64) bool {
		return summary.PlanID == expected.ID && summary.CreatedRevision > 0 && summary.CreatedRevision <= summary.Revision && summary.Revision <= asOf && summary.Profile == "paper" && summary.DataSource == "synthetic" && summary.Side == expected.Spec.Side && summary.Symbol == expected.Spec.Symbol && summary.Quantity.Equal(expected.Spec.Quantity) && summary.LimitPrice.Equal(expected.Spec.LimitPrice) && summary.Policy == expected.Policy
	}
	switch route {
	case "state":
		var value api.AccountDTO
		return decode(&value) == nil && value.TenantID == key.TenantID && value.AccountID == key.AccountID && value.Profile == rt.ProfilePaper && value.DataSource == "synthetic" && value.AuthorityRevision > 0 && value.AuthorityRevision <= value.Revision && planOK(value.Plan) && value.PlanStatus == value.Plan.Status && len(value.Fills) <= 2048 && value.FillCount >= len(value.Fills)
	case "sessions":
		var value struct {
			Entries []api.SessionSummary `json:"entries"`
			AsOf    int64                `json:"as_of_revision"`
			Next    int64                `json:"next_before_revision"`
			Limit   int                  `json:"limit"`
		}
		return decode(&value) == nil && value.Limit == 1 && len(value.Entries) == 1 && summaryOK(value.Entries[0], value.AsOf) && value.Next == value.Entries[0].CreatedRevision
	case "report":
		var value api.SessionReport
		return decode(&value) == nil && value.TenantID == key.TenantID && value.AccountID == key.AccountID && value.Version == "session-report-v1" && summaryOK(value.SessionSummary, value.AsOfRevision) && planOK(value.Plan) && value.Status == value.Plan.Status && len(value.Fills) <= 4096
	}
	return false
}

func loadRecoveryFacts(t *testing.T, dsn string, opt paperServerOptions) []loadRecoveryFact {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("accepted observation audit connection unavailable")
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, "SELECT sequence,observation_id,kind,payload FROM "+pgx.Identifier{opt.schema, "observations"}.Sanitize()+" WHERE tenant_id=$1 AND account_id=$2 ORDER BY sequence LIMIT 4097", paperDaemonTenant, opt.account)
	if err != nil {
		t.Fatal("accepted observation audit unavailable")
	}
	defer rows.Close()
	facts := []loadRecoveryFact{}
	for rows.Next() {
		var fact loadRecoveryFact
		var payload []byte
		if rows.Scan(&fact.Sequence, &fact.ID, &fact.Kind, &payload) != nil {
			t.Fatal("accepted observation row unavailable")
		}
		sum := sha256.Sum256(payload)
		fact.SHA256 = hex.EncodeToString(sum[:])
		facts = append(facts, fact)
	}
	if rows.Err() != nil || len(facts) > 4096 {
		t.Fatal("accepted observation audit exceeded bound or failed")
	}
	return facts
}

// Compute cash flows from independently accepted venue observations, then compare
// both the reducer snapshot and durable execution rows. No execution ledger is used.
func auditLoadRecovery(t *testing.T, dsn string, opt paperServerOptions, account pg.Account, original rt.State, boundary loadRecoveryAuditBoundary) loadRecoveryAudit {
	t.Helper()
	out := loadRecoveryAudit{Boundary: boundary, Ready: account.Ready, MarketValid: account.Market.Valid, Valid: true, Issues: []string{}, Epoch: account.Ownership.Epoch, Observed: account.ObservedSequence, Processed: account.ProcessedSequence, Balances: account.Balances, Accepted: []d.Fill{}}
	fail := func(issue string) { out.Valid = false; out.Issues = append(out.Issues, issue) }
	state, err := rt.DecodeSnapshot(account.Snapshot)
	if err != nil || state.Parent == nil || original.Parent == nil {
		fail("parent snapshot unavailable")
		return out
	}
	out.Status, out.RecoveryRequired, out.Reason, out.MarketID = state.Parent.Status, state.RecoveryRequired, loadRecoverySafeReason(state.Reason), state.MarketID
	out.BookValid = state.Book != nil && state.Book.Valid
	out.ClockDiscontinuity = state.ClockDiscontinuity
	out.PinsRetained = state.Parent.ID == original.Parent.ID && state.Parent.Start.Equal(original.Parent.Start) && state.Parent.Deadline.Equal(original.Parent.Deadline) && state.Parent.ArrivalMid.Equal(original.Parent.ArrivalMid) && reflect.DeepEqual(state.Parent.Spec, original.Parent.Spec) && state.Parent.Baseline == original.Parent.Baseline && reflect.DeepEqual(state.Parent.PolicyBundle, original.Parent.PolicyBundle) && reflect.DeepEqual(state.PolicyBundle, original.PolicyBundle) && reflect.DeepEqual(state.Rules, original.Rules) && reflect.DeepEqual(state.Fees, original.Fees) && reflect.DeepEqual(state.Execution, original.Execution) && state.BaseCurrency == original.BaseCurrency && state.QuoteCurrency == original.QuoteCurrency && state.InstrumentRevision == original.InstrumentRevision && state.DataSource == original.DataSource && state.Profile == original.Profile
	if !out.PinsRetained {
		fail("original execution pins changed")
	}
	accepted := loadRecoveryAcceptedFills(t, dsn, opt)
	base, quote := opt.initialBase, opt.initialQuote
	for _, fill := range accepted {
		if fill.Validate() != nil || fill.Side != "buy" || fill.FeeCurrency != "quote" || fill.Price.Cmp(state.Parent.Spec.LimitPrice) > 0 {
			fail("accepted fill violates fixture contract")
			continue
		}
		out.Accepted = append(out.Accepted, fill)
		out.Filled = out.Filled.Add(fill.Quantity)
		out.FeesQuote = out.FeesQuote.Add(fill.Fee)
		base = base.Add(fill.Quantity)
		quote = quote.Sub(fill.Quantity.Mul(fill.Price)).Sub(fill.Fee)
	}
	sort.Slice(out.Accepted, func(i, j int) bool { return out.Accepted[i].ExecutionID < out.Accepted[j].ExecutionID })
	out.Remaining = state.Parent.Spec.Quantity.Sub(out.Filled)
	if out.Remaining.Sign() <= 0 || !state.Parent.Started {
		fail("fixture lost its started residual parent")
	}
	seen := map[string]bool{}
	for _, fill := range state.Fills {
		prior, ok := accepted[fill.ExecutionID]
		if !ok || !prior.Equal(fill) || seen[fill.ExecutionID] {
			fail("snapshot lost repeated or invented accepted fill")
		}
		seen[fill.ExecutionID] = true
	}
	if len(seen) != len(accepted) || len(account.Balances) != 2 {
		fail("accepted fill or currency set differs")
	}
	seenCurrency := map[string]bool{}
	for _, balance := range account.Balances {
		want := base
		if balance.Currency == "USD" {
			want = quote
		} else if balance.Currency != "BTC" {
			fail("unexpected account currency")
		}
		if seenCurrency[balance.Currency] || !balance.Total.Equal(want) || !balance.Free.Equal(want) || !balance.Locked.IsZero() || balance.Total.Sign() < 0 {
			fail("cash flow or unlocked balance mismatch")
		}
		seenCurrency[balance.Currency] = true
	}
	for _, issue := range loadRecoveryBoundaryIssues(account, state, boundary) {
		fail(issue)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("execution audit connection unavailable")
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, "SELECT detail FROM "+pgx.Identifier{opt.schema, "executions"}.Sanitize()+" WHERE tenant_id=$1 AND account_id=$2 AND venue='paper' ORDER BY execution_id LIMIT 4097", paperDaemonTenant, opt.account)
	if err != nil {
		t.Fatal("execution audit unavailable")
	}
	defer rows.Close()
	executions := map[string]bool{}
	for rows.Next() {
		var raw []byte
		var fill d.Fill
		if rows.Scan(&raw) != nil || json.Unmarshal(raw, &fill) != nil {
			fail("execution row malformed")
			continue
		}
		prior, ok := accepted[fill.ExecutionID]
		if !ok || !prior.Equal(fill) || executions[fill.ExecutionID] {
			fail("durable execution differs from accepted observation")
		}
		executions[fill.ExecutionID] = true
	}
	if rows.Err() != nil || len(executions) != len(accepted) || len(executions) > 4096 {
		fail("durable execution set differs or exceeds bound")
	}
	return out
}

func loadRecoveryBinary(t *testing.T, binary string) map[string]any {
	t.Helper()
	f, err := os.Open(binary)
	if err != nil {
		t.Fatal("ordinary daemon identity unavailable")
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(f, (512<<20)+1))
	if err != nil || n > 512<<20 {
		t.Fatal("ordinary daemon checksum unavailable or exceeds bound")
	}
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		t.Fatal("ordinary daemon build identity unavailable")
	}
	if info.Path != "github.com/liquiditylab/liquiditylab/cmd/liquiditylabd" {
		t.Fatal("ordinary daemon package identity differs")
	}
	settings := map[string]string{}
	for _, item := range info.Settings {
		if item.Key == "-race" && item.Value != "false" || item.Key == "-tags" && item.Value != "" {
			t.Fatal("ordinary daemon has race or tag instrumentation")
		}
		switch item.Key {
		case "vcs", "vcs.revision", "vcs.time", "vcs.modified", "GOOS", "GOARCH", "CGO_ENABLED", "-race", "-tags", "-buildmode", "-compiler":
			settings[item.Key] = item.Value
		}
	}
	return map[string]any{"sha256": hex.EncodeToString(hash.Sum(nil)), "bytes": n, "go_version": info.GoVersion, "package": info.Path, "module": info.Main.Path, "module_version": info.Main.Version, "build_settings": settings, "fault_hooks": false, "race_instrumented": false}
}

func stopLoadRecoveryProcess(t *testing.T, process *paperTestProcess) bool {
	t.Helper()
	if process.command.Process.Signal(syscall.SIGTERM) != nil {
		t.Fatal("ordinary daemon SIGTERM failed")
	}
	select {
	case err := <-process.done:
		process.waited = true
		if err != nil {
			t.Fatal("ordinary daemon graceful shutdown failed")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("ordinary daemon graceful shutdown exceeded bound")
	}
	log, err := os.ReadFile(process.logPath)
	return err == nil && strings.Contains(string(log), "known_final=true")
}

func TestPaperProcessLoadRecoveryWithBystanderDemand(t *testing.T) {
	runPaperProcessLoadRecovery(t, false)
}

func runPaperProcessLoadRecovery(t *testing.T, backlog bool) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("ordinary SIGKILL recovery requires Unix")
	}
	dsn := os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set explicit development LIQUIDITYLAB_TEST_POSTGRES_DSN")
	}
	outputEnv := "LIQUIDITYLAB_RECOVERY_LOAD_OUTPUT"
	if backlog {
		outputEnv = "LIQUIDITYLAB_BACKLOG_RECOVERY_OUTPUT"
	}
	output := os.Getenv(outputEnv)
	if output == "" {
		output = filepath.Join(t.TempDir(), "load-recovery.json")
	}
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal("recovery evidence requires a fresh writable output file")
	}
	processes := map[string]*paperTestProcess{}
	report := loadRecoveryReport{Version: "paper-process-load-recovery-v1", Status: "incomplete", Final: map[string]loadRecoveryAudit{}, Counts: map[string]loadRecoveryCounts{}, Samples: map[string][]loadRecoverySample{}, KnownFinal: map[string]bool{}, Reaped: map[string]bool{}, Workload: map[string]any{
		"source": "ordinary daemon fixed 2 Hz synthetic trades and initial snapshot per process", "accounts": 2, "processes": 2, "read_rate_per_account_hz": loadRecoveryRate, "read_slots_per_account": loadRecoverySlots, "read_timeout_ns": int64(loadRecoveryTimeout), "response_limit_bytes": loadRecoveryBodyLimit, "schedule_duration_ns": int64(loadRecoveryDuration), "route_cycle": []string{"state", "state", "sessions", "state", "report"}, "daemon_pool_max_per_process": 16, "configured_daemon_pool_total": 32, "audit_store_pool_max": 2, "audit_sql_connections_at_once_max": 1, "configured_max_connections_including_observers": 35, "ready_poll_interval_ns": int64(loadRecoveryPollPeriod), "paused_proof_minimum_ns": int64(1100 * time.Millisecond), "financial_checker": "accepted-observation-cashflows-and-executions-v1", "http_checker": "scoped-original-parent-v1", "account_schema_retention": "fresh randomized development schema retained; never reset or dropped",
	}, Limitations: []string{
		"One account recovers under a second ordinary process/account's modest synthetic and HTTP database demand; no shared actor/pool, 50 Hz source, private adapter, headroom, capacity or SLO claim.",
		"Each ordinary synthetic source emits trades every 500 ms without an independent offered source schedule. HTTP offers alone use an independent continuous 20 Hz schedule per account, including downtime.",
		"SIGKILL timing is ordinary wall-clock timing. Observed recovery debt may be zero; zero debt does not prove backlog recovery. Existing tagged crash-boundary checks are separate evidence.",
		"Recovery intervals include process launch, listener discovery, HTTP work and ready polling. First-ready observation is an upper bound, with 20 ms poll waits plus request duration; no exchange or storage recovery inference.",
		"Restart uses a fresh synthetic source session and snapshot. Only accepted database-backed paper outcomes are reconstructed; synthetic events during process downtime were never accepted and are not replayed.",
		"Build and initial account/plan setup precede the continuous schedule. The ordinary daemon binary is untagged and not race-instrumented; the Go test harness can independently run with the race detector.",
	}}
	if backlog {
		configureLoadRecoveryBacklogReport(&report)
	}
	t.Cleanup(func() {
		defer file.Close()
		for role, process := range processes {
			report.Reaped[role] = process.waited
		}
		if t.Failed() {
			report.Valid, report.Status = false, "failed"
		}
		if report.Status == "complete" {
			report.Issues = loadRecoveryReportIssues(report)
			if len(report.Issues) != 0 {
				report.Valid, report.Status = false, "failed"
				t.Error("recovery evidence failed completeness validation")
			}
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil || len(data) > 4<<20 {
			t.Error("recovery evidence encoding failed or exceeded 4 MiB")
			return
		}
		if _, err := file.Write(append(data, '\n')); err != nil || file.Sync() != nil {
			t.Error("recovery evidence persistence failed")
		}
	})
	// Build outside the measured schedule. No fault tag or special child mode.
	binary := filepath.Join(t.TempDir(), "liquiditylabd-ordinary")
	buildCtx, buildCancel := context.WithTimeout(context.Background(), time.Minute)
	build := exec.CommandContext(buildCtx, "go", "build", "-buildvcs=true", "-tags=", "-race=false", "-o", binary, ".")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GOFLAGS=") {
			build.Env = append(build.Env, entry)
		}
	}
	build.Env = append(build.Env, "GOFLAGS=")
	buildErr := build.Run()
	buildCancel()
	if buildErr != nil {
		t.Fatal("build ordinary daemon failed")
	}
	report.Binary = loadRecoveryBinary(t, binary)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal("fresh schema identity unavailable")
	}
	opt := daemonOptions(t)
	opt.schema, opt.account = "ll_load_recovery_"+hex.EncodeToString(suffix[:]), "victim"
	bystanderOpt := opt
	bystanderOpt.account = "bystander"
	report.Accounts = map[string]pg.Key{"victim": {TenantID: paperDaemonTenant, AccountID: opt.account}, "bystander": {TenantID: paperDaemonTenant, AccountID: bystanderOpt.account}}
	report.Workload["retained_schema"] = opt.schema
	report.Initial = []pg.Balance{{Currency: "BTC", Total: opt.initialBase, Free: opt.initialBase}, {Currency: "USD", Total: opt.initialQuote, Free: opt.initialQuote}}
	operationStart := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	victimDSN, victimApplication := dsn, ""
	if backlog {
		victimApplication = "ll_backlog_victim_" + hex.EncodeToString(suffix[:])
		victimDSN = loadRecoveryApplicationDSN(t, dsn, victimApplication)
	}
	victim := startLoadRecoveryProcess(t, binary, victimDSN, opt, "create")
	processes["victim_sigkill"] = victim
	bystander := startLoadRecoveryProcess(t, binary, dsn, bystanderOpt, "create")
	processes["bystander"] = bystander
	setupPlan := func(process *paperTestProcess, options paperServerOptions, id string) (*api.PlanDTO, map[string]any) {
		ready := waitLoadRecoveryAccount(t, process, options, func(a api.AccountDTO) bool { return a.Ready })
		plan := d.DefaultPlan()
		plan.Quantity, plan.MaxChildQuantity = d.MustDecimal("0.05"), d.MustDecimal("0.01")
		plan.QuoteBudget, plan.BaseBalance, plan.Deadline = options.initialQuote, options.initialBase, 120*d.Second
		create := map[string]any{"id": id + "-create", "kind": "create", "plan_id": id, "plan": plan, "expected_revision": ready.Revision, "expected_authority_revision": ready.AuthorityRevision}
		if backlog && process == victim {
			create["policy"] = x.BaselineLiquidityAware
		}
		loadRecoveryRequest(t, process, options, "POST", "/commands", create)
		paused := loadRecoveryAccount(t, process, options)
		start := map[string]any{"id": id + "-start", "kind": "start", "expected_revision": paused.Revision, "expected_authority_revision": paused.AuthorityRevision}
		loadRecoveryRequest(t, process, options, "POST", "/commands", start)
		active := waitLoadRecoveryAccount(t, process, options, func(a api.AccountDTO) bool {
			if backlog && process == victim {
				return a.Plan != nil && a.Plan.Status == "active" && a.Plan.Remaining.Sign() > 0 && a.Child != nil && a.Child.State == "open" && a.Child.Intent.Passive
			}
			return a.Plan != nil && a.Plan.Status == "active" && a.Plan.Remaining.Sign() > 0 && a.FillCount > 0
		})
		return active.Plan, start
	}
	victimPlan, originalStart := setupPlan(victim, opt, "recovery-load-plan")
	bystanderPlan, _ := setupPlan(bystander, bystanderOpt, "bystander-load-plan")
	report.OriginalPlans = map[string]*api.PlanDTO{"victim": victimPlan, "bystander": bystanderPlan}
	store, err := pg.OpenWithOptions(ctx, dsn, opt.schema, pg.Options{MaxConnections: 2})
	if err != nil {
		t.Fatal("recovery audit store unavailable")
	}
	defer store.Close()
	victimInitial, err := store.GetAccount(ctx, report.Accounts["victim"])
	if err != nil {
		t.Fatal("initial victim account unavailable")
	}
	victimPins, err := rt.DecodeSnapshot(victimInitial.Snapshot)
	if err != nil {
		t.Fatal("initial victim pins unavailable")
	}
	bystanderInitial, err := store.GetAccount(ctx, report.Accounts["bystander"])
	if err != nil {
		t.Fatal("initial bystander account unavailable")
	}
	bystanderPins, err := rt.DecodeSnapshot(bystanderInitial.Snapshot)
	if err != nil {
		t.Fatal("initial bystander pins unavailable")
	}
	if backlog {
		// The explicit fixture setup is outside the read schedule and recovery clocks.
		report.Backlog = freezeLoadRecoveryBacklog(t, ctx, victim, store, dsn, victimApplication, report.Accounts["victim"])
	}
	origin := time.Now()
	report.StartedUTC = origin.UTC()
	victimTarget, bystanderTarget := &loadRecoveryTarget{}, &loadRecoveryTarget{}
	victimTarget.set(victim.url)
	bystanderTarget.set(bystander.url)
	victimPoller := startLoadRecoveryPoller(ctx, origin, loadRecoveryDuration, "victim", victimTarget, report.Accounts["victim"], victimPlan)
	bystanderPoller := startLoadRecoveryPoller(ctx, origin, loadRecoveryDuration, "bystander", bystanderTarget, report.Accounts["bystander"], bystanderPlan)
	defer func() {
		victimPoller.close()
		bystanderPoller.close()
		report.PollersDrained = true
		if report.Events.LoadDrainedNS == 0 {
			report.Events.LoadDrainedNS = time.Since(origin).Nanoseconds()
		}
		for role, poller := range map[string]*loadRecoveryPoller{"victim": victimPoller, "bystander": bystanderPoller} {
			report.Samples[role], report.Counts[role] = loadRecoverySummarize(poller.samples, report.Events)
		}
	}()
	// The two fixed schedules continue through kill, reap, restart and proof.
	timer := time.NewTimer(3 * time.Second)
	if backlog {
		timer.Stop()
		continueLoadRecoveryBacklog(t, victim, report.Backlog)
		waitLoadRecoveryBacklogPrefix(t, ctx, store, report.Accounts["victim"], report.Backlog)
	} else {
		select {
		case <-timer.C:
		case <-ctx.Done():
			t.Fatal("pre-crash load interval exceeded bound")
		}
	}
	bystanderAtCrash, err := store.GetAccount(ctx, report.Accounts["bystander"])
	if err != nil {
		t.Fatal("bystander crash boundary unavailable")
	}
	report.BystanderBefore = bystanderAtCrash.ObservedSequence
	report.Events.KillIssuedNS = time.Since(origin).Nanoseconds()
	victimTarget.set("")
	killPaperTestProcess(t, victim)
	report.Events.ReapedNS = time.Since(origin).Nanoseconds()
	report.Reaped["victim_sigkill"] = true
	var crashed pg.Account
	var crashExecutions []d.Fill
	if backlog {
		crashCtx, crashCancel := context.WithTimeout(ctx, 2*time.Second)
		crashed, crashExecutions, err = loadRecoveryCrashObservation(crashCtx, store, dsn, opt.schema, report.Accounts["victim"])
		crashCancel()
	} else {
		crashed, err = store.GetAccount(ctx, report.Accounts["victim"])
	}
	if err != nil {
		t.Fatal("crashed account/execution observation unavailable or exceeded bounded account-lock barrier")
	}
	report.CrashEpoch, report.CrashObserved, report.CrashProcessed = crashed.Ownership.Epoch, crashed.ObservedSequence, crashed.ProcessedSequence
	report.CrashDebt = crashed.ObservedSequence - crashed.ProcessedSequence
	if backlog {
		checkLoadRecoveryBacklogCrash(t, crashed, crashExecutions, report.Backlog)
	}
	report.CrashFacts = loadRecoveryFacts(t, dsn, opt)
	acceptedBefore := loadRecoveryAcceptedFills(t, dsn, opt)
	if len(acceptedBefore) == 0 {
		t.Fatal("crash lacked accepted financial facts")
	}
	// Keep an explicit short unbound interval so its offered reads are retained.
	timer.Reset(250 * time.Millisecond)
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal("restart boundary exceeded bound")
	}
	report.Events.LaunchNS = time.Since(origin).Nanoseconds()
	restarted := startLoadRecoveryProcess(t, binary, victimDSN, opt, "resume")
	processes["victim_resumed"] = restarted
	report.Events.ListenerNS = time.Since(origin).Nanoseconds()
	victimTarget.set(restarted.url)
	recovered := waitLoadRecoveryAccount(t, restarted, opt, func(a api.AccountDTO) bool { return a.Ready && a.Plan != nil && a.Plan.Status == "paused" })
	report.Events.ReadyNS = time.Since(origin).Nanoseconds()
	report.Events.OutageNS = report.Events.ReadyNS - report.Events.KillIssuedNS
	report.Events.LaunchReadyNS = report.Events.ReadyNS - report.Events.LaunchNS
	retained, err := store.GetAccount(ctx, report.Accounts["victim"])
	if err != nil {
		t.Fatal("recovered durable account unavailable")
	}
	report.Recovered = auditLoadRecovery(t, dsn, opt, retained, victimPins, loadRecoveryAuditBoundary{Phase: loadRecoveryPausedBoundary})
	if !report.Recovered.Valid || retained.Ownership.Epoch != crashed.Ownership.Epoch+1 || !reflect.DeepEqual(acceptedBefore, loadRecoveryAcceptedFills(t, dsn, opt)) || !loadRecoveryExactProjection(recovered, report.Recovered) {
		t.Fatal("recovered accepted finances, epoch, pins or residual disagree")
	}
	afterFacts := loadRecoveryFacts(t, dsn, opt)
	if len(afterFacts) < len(report.CrashFacts) || !reflect.DeepEqual(report.CrashFacts, afterFacts[:len(report.CrashFacts)]) {
		t.Fatal("restart changed or lost accepted source observations")
	}
	var receipt pg.Result
	if json.Unmarshal(loadRecoveryRequest(t, restarted, opt, "POST", "/commands", originalStart), &receipt) != nil || !receipt.Duplicate {
		t.Fatal("exact original start retry was not duplicate")
	}
	report.StartRetry = true
	proofUntil := time.Now().Add(1100 * time.Millisecond)
	for time.Now().Before(proofUntil) {
		current := loadRecoveryAccount(t, restarted, opt)
		if current.Plan == nil || current.Plan.Status != "paused" || !current.Plan.Filled.Equal(recovered.Plan.Filled) || !current.Plan.Remaining.Equal(recovered.Plan.Remaining) || current.FillCount != recovered.FillCount {
			t.Fatal("recovered parent traded without explicit resume")
		}
		time.Sleep(loadRecoveryPollPeriod)
	}
	report.Events.ProofEndNS = time.Since(origin).Nanoseconds()
	<-victimPoller.done
	<-bystanderPoller.done
	report.PollersDrained = true
	report.Events.LoadDrainedNS = time.Since(origin).Nanoseconds()
	for role, poller := range map[string]*loadRecoveryPoller{"victim": victimPoller, "bystander": bystanderPoller} {
		report.Samples[role], report.Counts[role] = loadRecoverySummarize(poller.samples, report.Events)
		for _, sample := range poller.samples {
			if !loadRecoveryReadExpected(role, sample, report.Events) {
				t.Errorf("%s unexpected read outcome: %s", role, sample.Outcome)
			}
		}
	}
	if report.Counts["victim"].Outcomes["endpoint_unbound"] == 0 {
		t.Error("outage offers were not retained")
	}
	bystanderAfter, err := store.GetAccount(ctx, report.Accounts["bystander"])
	if err != nil {
		t.Fatal("bystander load outcome unavailable")
	}
	report.BystanderAfter = bystanderAfter.ObservedSequence
	if bystanderAfter.ObservedSequence <= bystanderAtCrash.ObservedSequence || bystanderAfter.Ownership != bystanderInitial.Ownership {
		t.Error("bystander did not preserve ownership and advance durable source demand")
	}
	report.KnownFinal["victim"] = stopLoadRecoveryProcess(t, restarted)
	report.Reaped["victim_resumed"] = restarted.waited
	report.KnownFinal["bystander"] = stopLoadRecoveryProcess(t, bystander)
	report.Reaped["bystander"] = bystander.waited
	report.Events.ProcessesReapedNS = time.Since(origin).Nanoseconds()
	for role, options := range map[string]paperServerOptions{"victim": opt, "bystander": bystanderOpt} {
		final, err := store.GetAccount(ctx, report.Accounts[role])
		if err != nil {
			t.Fatal("final durable account unavailable")
		}
		pins, process := victimPins, restarted
		if role == "bystander" {
			pins, process = bystanderPins, bystander
		}
		audit := auditLoadRecovery(t, dsn, options, final, pins, loadRecoveryAuditBoundary{Phase: loadRecoveryShutdownBoundary, ProcessReaped: process.waited, KnownFinal: report.KnownFinal[role]})
		report.Final[role] = audit
		if !audit.Valid || audit.Status != "cancelled" || !report.KnownFinal[role] {
			t.Errorf("%s final accounting or graceful finality failed: issues=%v status=%s known_final=%t ready=%t book_valid=%t recovery_required=%t reason=%s", role, audit.Issues, audit.Status, report.KnownFinal[role], audit.Ready, audit.BookValid, audit.RecoveryRequired, loadRecoverySafeReason(audit.Reason))
		}
	}
	if !reflect.DeepEqual(acceptedBefore, loadRecoveryAcceptedFills(t, dsn, opt)) {
		t.Error("victim accepted fills changed after paused recovery")
	}
	if time.Since(operationStart) > 30*time.Second || report.Events.ProofEndNS >= int64(loadRecoveryDuration) {
		t.Error("process recovery/proof exceeded bounded load interval")
	}
	report.Valid, report.Status = !t.Failed(), "complete"
	if !report.Valid {
		report.Status = "failed"
	}
	label := "ordinary SIGKILL recovery"
	if backlog {
		label = "ordinary SIGKILL recovery of a harness-authored 128-fill accepted backlog"
	}
	t.Logf("%s under 2 Hz synthetic and 20 Hz/account HTTP bystander demand: debt=%d, outage=%s, cold-launch-to-ready=%s, victim=%v, bystander=%v; evidence=%s", label, report.CrashDebt, time.Duration(report.Events.OutageNS), time.Duration(report.Events.LaunchReadyNS), report.Counts["victim"].Outcomes, report.Counts["bystander"].Outcomes, filepath.Base(output))
}

func TestPaperLoadRecoveryOpenLoopRetainsUnboundOffers(t *testing.T) {
	origin := time.Now()
	poller := startLoadRecoveryPoller(context.Background(), origin, 100*time.Millisecond, "victim", &loadRecoveryTarget{}, pg.Key{}, nil)
	defer poller.close()
	<-poller.done
	if len(poller.samples) != 2 {
		t.Fatal("offered schedule changed with absent endpoint")
	}
	for index, sample := range poller.samples {
		if sample.IntendedNS != int64(index)*int64(50*time.Millisecond) || sample.Outcome != "endpoint_unbound" || sample.AdmittedNS != nil || sample.CallNS != nil || sample.EndNS < sample.AttemptNS {
			t.Fatalf("unbound offered slot was hidden or counted as completed: %+v", sample)
		}
	}
}

// All operation errors are fixed classifications. URLs, DSNs, tokens, raw
// responses and operating-system network messages never enter evidence or test output.
func startLoadRecoveryProcess(t *testing.T, binary, dsn string, opt paperServerOptions, mode string) *paperTestProcess {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "ordinary-daemon.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal("ordinary daemon log unavailable")
	}
	args := []string{"paper", mode, "--listen", "127.0.0.1:0", "--schema", opt.schema, "--account", opt.account, "--source", "synthetic", "--web-dir", opt.webDir, "--duration", "1m"}
	if mode == "create" {
		args = append(args, "--initial-base", opt.initialBase.String(), "--initial-quote", opt.initialQuote.String())
	}
	command := exec.Command(binary, args...)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "LIQUIDITYLAB_POLICY_") && !strings.HasPrefix(entry, "LIQUIDITYLAB_DATABASE_URL=") && !strings.HasPrefix(entry, "LIQUIDITYLAB_API_TOKEN=") && !strings.HasPrefix(entry, "LIQUIDITYLAB_TEST_FAULT_POINT=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "LIQUIDITYLAB_DATABASE_URL="+dsn, "LIQUIDITYLAB_API_TOKEN="+daemonTestToken)
	command.Stderr = log
	if err := command.Start(); err != nil {
		log.Close()
		t.Fatal("ordinary daemon launch failed")
	}
	log.Close()
	p := &paperTestProcess{command: command, done: make(chan error, 1), logPath: logPath}
	go func() { p.done <- command.Wait() }()
	t.Cleanup(func() {
		if !p.waited {
			_ = p.command.Process.Kill()
			select {
			case <-p.done:
				p.waited = true
			case <-time.After(5 * time.Second):
				t.Error("ordinary daemon cleanup did not reap child")
			}
		}
	})
	pattern := regexp.MustCompile(`PAPER server (http://127\.0\.0\.1:[0-9]+) `)
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		data, _ := os.ReadFile(logPath)
		if match := pattern.FindSubmatch(data); match != nil {
			p.url = string(match[1])
			return p
		}
		select {
		case <-p.done:
			p.waited = true
			t.Fatal("ordinary daemon exited before listener discovery")
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("ordinary daemon listener discovery exceeded bound")
	return nil
}

func loadRecoveryRequest(t *testing.T, p *paperTestProcess, opt paperServerOptions, method, suffix string, body any) []byte {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal("control request encoding failed")
	}
	request, err := http.NewRequest(method, p.url+"/v1/accounts/"+paperDaemonTenant+"/"+opt.account+suffix, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal("control request construction failed")
	}
	request.Header.Set("Authorization", "Bearer "+daemonTestToken)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: loadRecoveryTimeout, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("control response unresolved")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, loadRecoveryBodyLimit+1))
	if err != nil || len(data) > loadRecoveryBodyLimit || response.StatusCode != http.StatusOK {
		t.Fatal("control response invalid or unavailable")
	}
	return data
}

func loadRecoveryAccount(t *testing.T, p *paperTestProcess, opt paperServerOptions) api.AccountDTO {
	t.Helper()
	var account api.AccountDTO
	if json.Unmarshal(loadRecoveryRequest(t, p, opt, http.MethodGet, "", nil), &account) != nil {
		t.Fatal("control account projection malformed")
	}
	return account
}

func waitLoadRecoveryAccount(t *testing.T, p *paperTestProcess, opt paperServerOptions, condition func(api.AccountDTO) bool) api.AccountDTO {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		account := loadRecoveryAccount(t, p, opt)
		if condition(account) {
			return account
		}
		time.Sleep(loadRecoveryPollPeriod)
	}
	t.Fatal("ordinary daemon account condition exceeded bound")
	return api.AccountDTO{}
}

func loadRecoveryAcceptedFills(t *testing.T, dsn string, opt paperServerOptions) map[string]d.Fill {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("accepted financial observation audit connection unavailable")
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, "SELECT payload FROM "+pgx.Identifier{opt.schema, "observations"}.Sanitize()+" WHERE tenant_id=$1 AND account_id=$2 ORDER BY sequence LIMIT 4097", paperDaemonTenant, opt.account)
	if err != nil {
		t.Fatal("accepted financial observation audit unavailable")
	}
	defer rows.Close()
	fills := map[string]d.Fill{}
	count := 0
	for rows.Next() {
		count++
		var raw []byte
		var fact struct {
			Outcome struct {
				Fills []d.Fill `json:"fills"`
			} `json:"outcome"`
			Fills []d.Fill `json:"fills"`
		}
		if rows.Scan(&raw) != nil || len(raw) > loadRecoveryBodyLimit || json.Unmarshal(raw, &fact) != nil {
			t.Fatal("accepted financial observation malformed or exceeds bound")
		}
		for _, fill := range append(fact.Fills, fact.Outcome.Fills...) {
			if prior, ok := fills[fill.ExecutionID]; ok && !prior.Equal(fill) {
				t.Fatal("accepted financial observation conflicts")
			}
			fills[fill.ExecutionID] = fill
		}
	}
	if rows.Err() != nil || count > 4096 || len(fills) > 2048 {
		t.Fatal("accepted financial observation audit failed or exceeded bound")
	}
	return fills
}

func loadRecoveryExactProjection(value api.AccountDTO, audit loadRecoveryAudit) bool {
	if value.Plan == nil || !audit.Valid || value.Plan.Status != "paused" || !value.Plan.Filled.Equal(audit.Filled) || !value.Plan.Remaining.Equal(audit.Remaining) || !value.Plan.FeesQuote.Equal(audit.FeesQuote) || !value.Plan.FeesBase.IsZero() || value.FillCount != len(audit.Accepted) || value.FillsTruncated || value.Child != nil || !reflect.DeepEqual(value.Balances, audit.Balances) {
		return false
	}
	for _, hold := range value.Reserved {
		if !hold.Amount.IsZero() {
			return false
		}
	}
	fills := append([]d.Fill{}, value.Fills...)
	sort.Slice(fills, func(i, j int) bool { return fills[i].ExecutionID < fills[j].ExecutionID })
	return reflect.DeepEqual(fills, audit.Accepted)
}

func loadRecoverySummarize(samples []loadRecoverySample, events loadRecoveryEvents) ([]loadRecoverySample, loadRecoveryCounts) {
	counts := loadRecoveryCounts{Outcomes: map[string]int{}, ReadRoute: map[string]int{}}
	for index := range samples {
		sample := &samples[index]
		sample.CrashPhase = "before_crash"
		if events.KillIssuedNS > 0 && sample.IntendedNS >= events.KillIssuedNS {
			sample.CrashPhase = "outage"
		}
		if events.ReadyNS > 0 && sample.IntendedNS >= events.ReadyNS {
			sample.CrashPhase = "after_ready"
		}
		counts.Offered++
		counts.ReadRoute[sample.Route]++
		counts.Outcomes[sample.Outcome]++
		if sample.AdmittedNS != nil {
			counts.Admitted++
		}
	}
	return samples, counts
}

func loadRecoveryReportIssues(report loadRecoveryReport) []string {
	issues := loadRecoveryBacklogIssues(report)
	checksum, _ := report.Binary["sha256"].(string)
	decoded, err := hex.DecodeString(checksum)
	if err != nil || len(decoded) != sha256.Size || report.Binary["fault_hooks"] != false || report.Binary["race_instrumented"] != false {
		issues = append(issues, "ordinary daemon identity incomplete")
	}
	if !report.PollersDrained || !report.StartRetry || !report.Recovered.Valid || report.Recovered.Status != "paused" || report.Recovered.Boundary.Phase != loadRecoveryPausedBoundary || !report.Recovered.Ready || !report.Recovered.BookValid || !report.Recovered.MarketValid || report.Recovered.RecoveryRequired || report.Recovered.ClockDiscontinuity || report.CrashDebt < 0 || report.CrashDebt != report.CrashObserved-report.CrashProcessed || report.Recovered.Epoch != report.CrashEpoch+1 || len(report.CrashFacts) == 0 {
		issues = append(issues, "recovery evidence incomplete")
	}
	if report.Events.KillIssuedNS <= 0 || report.Events.ReapedNS < report.Events.KillIssuedNS || report.Events.LaunchNS < report.Events.ReapedNS || report.Events.ListenerNS < report.Events.LaunchNS || report.Events.ReadyNS < report.Events.ListenerNS || report.Events.ProofEndNS-report.Events.ReadyNS < int64(1100*time.Millisecond) || report.Events.ProofEndNS >= int64(loadRecoveryDuration) || report.Events.LoadDrainedNS < int64(loadRecoveryDuration) || report.Events.ProcessesReapedNS < report.Events.LoadDrainedNS {
		issues = append(issues, "recovery event order or duration invalid")
	}
	if !report.Reaped["victim_sigkill"] || !report.Reaped["victim_resumed"] || !report.Reaped["bystander"] || report.BystanderAfter <= report.BystanderBefore {
		issues = append(issues, "process cleanup or bystander progress incomplete")
	}
	for _, role := range []string{"victim", "bystander"} {
		_, counts := loadRecoverySummarize(append([]loadRecoverySample{}, report.Samples[role]...), report.Events)
		if len(report.Samples[role]) != int(loadRecoveryDuration/(time.Second/loadRecoveryRate)) || !reflect.DeepEqual(counts, report.Counts[role]) {
			issues = append(issues, role+" offered accounting incomplete")
		}
		for index, sample := range report.Samples[role] {
			if !loadRecoverySampleOutcomeValid(sample) {
				issues = append(issues, role+" read outcome inconsistent")
				break
			}
			if sample.Index != index || sample.Role != role || sample.IntendedNS != int64(index)*int64(time.Second/loadRecoveryRate) || sample.AttemptNS < sample.IntendedNS || sample.EndNS < sample.AttemptNS || sample.Outcome == "" || sample.Outcome == "not_attempted_cancelled" || (sample.AdmittedNS == nil) != (sample.CallNS == nil) || (sample.AdmittedNS != nil && (*sample.AdmittedNS < sample.AttemptNS || *sample.CallNS < *sample.AdmittedNS || sample.EndNS < *sample.CallNS)) {
				issues = append(issues, role+" offered sample invalid")
				break
			}
			if !loadRecoveryReadExpected(role, sample, report.Events) {
				issues = append(issues, role+" unexpected read outcome")
				break
			}
		}
		final := report.Final[role]
		if final.RecoveryRequired && (final.Reason != "market_invalid" || final.MarketID != "web-source-ended" || final.Ready || final.MarketValid || final.BookValid) {
			issues = append(issues, role+" final audit retains unexplained recovery requirement")
		}
		if !report.KnownFinal[role] || !report.Final[role].Valid || report.Final[role].Status != "cancelled" || report.Final[role].Boundary.Phase != loadRecoveryShutdownBoundary || !report.Final[role].Boundary.ProcessReaped || !report.Final[role].Boundary.KnownFinal || report.Final[role].ClockDiscontinuity {
			issues = append(issues, role+" final audit incomplete")
		}
	}
	if report.Counts["victim"].Outcomes["endpoint_unbound"] == 0 {
		issues = append(issues, "victim outage offers missing")
	}
	return issues
}

// Only loss of endpoint/transport/response or explicit service unavailability
// can be attributed to the victim outage. A successful HTTP response with bad
// identity, malformed JSON or an excessive body is invalid in every phase.
func loadRecoveryReadExpected(role string, sample loadRecoverySample, events loadRecoveryEvents) bool {
	if !loadRecoverySampleOutcomeValid(sample) {
		return false
	}
	if sample.Outcome == "completed" {
		return true
	}
	if role != "victim" || events.KillIssuedNS <= 0 || events.ReadyNS <= 0 || sample.AttemptNS >= events.ReadyNS || sample.EndNS < events.KillIssuedNS {
		return false
	}
	switch sample.Outcome {
	case "endpoint_unbound", "transport_failed", "response_unresolved":
		return true
	case "http_rejected":
		return sample.Status == http.StatusServiceUnavailable
	}
	return false
}

func loadRecoverySampleOutcomeValid(sample loadRecoverySample) bool {
	admitted := sample.AdmittedNS != nil && sample.CallNS != nil
	switch sample.Outcome {
	case "completed":
		return admitted && sample.Status == http.StatusOK && sample.Bytes > 0 && sample.Bytes <= loadRecoveryBodyLimit
	case "endpoint_unbound", "runner_rejected", "not_attempted_cancelled":
		return !admitted && sample.Status == 0 && sample.Bytes == 0
	case "request_failed", "transport_failed", "response_unresolved", "response_too_large", "http_failed", "http_rejected", "projection_failed":
		return admitted
	}
	return false
}

func TestPaperLoadRecoveryAccountingRetainsEveryOutcome(t *testing.T) {
	admitted, call := int64(2), int64(3)
	samples := []loadRecoverySample{
		{Outcome: "completed", AdmittedNS: &admitted, CallNS: &call, Status: 200, Bytes: 10},
		{IntendedNS: 50, Outcome: "endpoint_unbound"},
		{IntendedNS: 100, Outcome: "runner_rejected"},
		{IntendedNS: 150, Outcome: "response_unresolved", AdmittedNS: &admitted, CallNS: &call},
		{IntendedNS: 200, Outcome: "transport_failed", AdmittedNS: &admitted, CallNS: &call},
	}
	retained, counts := loadRecoverySummarize(samples, loadRecoveryEvents{KillIssuedNS: 40, ReadyNS: 180})
	if counts.Offered != 5 || counts.Admitted != 3 || len(counts.Outcomes) != 5 || retained[0].CrashPhase != "before_crash" || retained[1].CrashPhase != "outage" || retained[4].CrashPhase != "after_ready" {
		t.Fatal("rejected, unbound or unresolved offers disappeared from accounting")
	}
	for _, sample := range retained {
		if !loadRecoverySampleOutcomeValid(sample) {
			t.Fatal("retained outcome classification is inconsistent")
		}
	}
	for _, sample := range []loadRecoverySample{
		{Outcome: "completed", Status: 200, Bytes: 10},
		{Outcome: "completed", AdmittedNS: &admitted, CallNS: &call, Status: 503, Bytes: 10},
		{Outcome: "endpoint_unbound", AdmittedNS: &admitted, CallNS: &call},
		{Outcome: "invented"},
	} {
		if loadRecoverySampleOutcomeValid(sample) {
			t.Fatal("invalid or falsely completed outcome admitted")
		}
	}
	_, unfinished := loadRecoverySummarize(retained, loadRecoveryEvents{})
	if unfinished.Offered != counts.Offered || retained[4].CrashPhase != "before_crash" {
		t.Fatal("unset recovery boundary fabricated an after-ready phase")
	}
}

func TestPaperLoadRecoveryProjectionRejectsWrongIdentityAndResidualCompletion(t *testing.T) {
	key := pg.Key{TenantID: "tenant", AccountID: "victim"}
	plan := api.PlanDTO{ID: "original", Revision: 1, Status: "paused", Started: true, Spec: d.DefaultPlan(), Policy: "scheduled", Start: time.Unix(100, 0).UTC(), Deadline: time.Unix(220, 0).UTC(), ArrivalMid: d.MustDecimal("100")}
	plan.Filled = d.MustDecimal("0.01")
	plan.Remaining = plan.Spec.Quantity.Sub(plan.Filled)
	state := api.AccountDTO{AccountSummary: api.AccountSummary{TenantID: key.TenantID, AccountID: key.AccountID, Profile: rt.ProfilePaper, DataSource: "synthetic", Revision: 2, AuthorityRevision: 1, PlanStatus: "paused"}, Plan: &plan}
	check := func(value api.AccountDTO) bool {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal("projection fixture encoding failed")
		}
		return loadRecoveryProjection(raw, "state", key, &plan)
	}
	if !check(state) {
		t.Fatal("scoped original-parent fixture rejected")
	}
	wrong := state
	wrong.AccountID = "bystander"
	if check(wrong) {
		t.Fatal("another account projection admitted")
	}
	wrong = state
	changed := plan
	changed.ID = "replacement"
	wrong.Plan = &changed
	if check(wrong) {
		t.Fatal("replacement parent projection admitted")
	}
	changed = plan
	changed.Status = "completed"
	wrong.PlanStatus = "completed"
	if check(wrong) {
		t.Fatal("completed parent with residual admitted")
	}
	if loadRecoveryProjection([]byte(`{"tenant_id":"tenant"}`), "state", key, &plan) || loadRecoveryProjection([]byte(`null`), "report", key, &plan) {
		t.Fatal("incomplete projection admitted")
	}
}

func TestPaperLoadRecoveryReportRejectsMissingOrFailedEvidence(t *testing.T) {
	// Load a wholly synthetic complete report so this guard has no database or
	// filesystem dependency and can exercise each required evidence boundary.
	report := loadRecoveryReport{Binary: map[string]any{"sha256": strings.Repeat("01", sha256.Size), "fault_hooks": false, "race_instrumented": false}, PollersDrained: true, StartRetry: true, CrashEpoch: 1, Recovered: loadRecoveryAudit{Valid: true, Status: "paused", Epoch: 2, Ready: true, BookValid: true, MarketValid: true, Boundary: loadRecoveryAuditBoundary{Phase: loadRecoveryPausedBoundary}}, CrashFacts: []loadRecoveryFact{{Sequence: 1}}, Counts: map[string]loadRecoveryCounts{}, Samples: map[string][]loadRecoverySample{}, KnownFinal: map[string]bool{"victim": true, "bystander": true}, Reaped: map[string]bool{"victim_sigkill": true, "victim_resumed": true, "bystander": true}, Final: map[string]loadRecoveryAudit{"victim": {Valid: true, Status: "cancelled", Boundary: loadRecoveryAuditBoundary{Phase: loadRecoveryShutdownBoundary, ProcessReaped: true, KnownFinal: true}}, "bystander": {Valid: true, Status: "cancelled", Boundary: loadRecoveryAuditBoundary{Phase: loadRecoveryShutdownBoundary, ProcessReaped: true, KnownFinal: true}}}, BystanderBefore: 1, BystanderAfter: 2, Events: loadRecoveryEvents{KillIssuedNS: int64(3 * time.Second), ReapedNS: int64(3 * time.Second), LaunchNS: int64(3250 * time.Millisecond), ListenerNS: int64(3290 * time.Millisecond), ReadyNS: int64(3300 * time.Millisecond), ProofEndNS: int64(4400 * time.Millisecond), LoadDrainedNS: int64(loadRecoveryDuration), ProcessesReapedNS: int64(loadRecoveryDuration + time.Millisecond)}}
	for _, role := range []string{"victim", "bystander"} {
		samples := make([]loadRecoverySample, int(loadRecoveryDuration/(time.Second/loadRecoveryRate)))
		for index := range samples {
			now := int64(index) * int64(time.Second/loadRecoveryRate)
			admitted, call := now, now
			samples[index] = loadRecoverySample{Role: role, Route: "state", Index: index, IntendedNS: now, AttemptNS: now, AdmittedNS: &admitted, CallNS: &call, EndNS: now + 1, Outcome: "completed", Status: 200, Bytes: 10}
			if role == "victim" && now >= report.Events.KillIssuedNS && now < report.Events.ReadyNS {
				samples[index].AdmittedNS, samples[index].CallNS = nil, nil
				samples[index].Outcome, samples[index].Status, samples[index].Bytes = "endpoint_unbound", 0, 0
			}
		}
		report.Samples[role], report.Counts[role] = loadRecoverySummarize(samples, report.Events)
	}
	if issues := loadRecoveryReportIssues(report); len(issues) != 0 {
		t.Fatalf("complete evidence rejected: %v", issues)
	}
	for _, outcome := range []string{"projection_failed", "response_too_large", "request_failed", "http_failed", "runner_rejected"} {
		t.Run("outage_does_not_allow_"+outcome, func(t *testing.T) {
			altered := report
			altered.Samples = map[string][]loadRecoverySample{"victim": append([]loadRecoverySample{}, report.Samples["victim"]...), "bystander": report.Samples["bystander"]}
			altered.Counts = map[string]loadRecoveryCounts{"bystander": report.Counts["bystander"]}
			sample := &altered.Samples["victim"][61] // 3.05 s, inside the observed outage.
			admitted, call := sample.AttemptNS, sample.AttemptNS
			sample.AdmittedNS, sample.CallNS = &admitted, &call
			sample.Outcome, sample.Status, sample.Bytes = outcome, http.StatusOK, 10
			switch outcome {
			case "response_too_large":
				sample.Bytes = loadRecoveryBodyLimit + 1
			case "request_failed":
				sample.Status, sample.Bytes = 0, 0
			case "http_failed":
				sample.Status = http.StatusInternalServerError
			case "runner_rejected":
				sample.AdmittedNS, sample.CallNS = nil, nil
				sample.Status, sample.Bytes = 0, 0
			}
			// These were all admitted by the former blanket crash-overlap rule.
			if sample.AttemptNS >= altered.Events.ReadyNS || sample.EndNS < altered.Events.KillIssuedNS || !loadRecoverySampleOutcomeValid(*sample) {
				t.Fatal("regression fixture does not exercise the former outage exception")
			}
			altered.Samples["victim"], altered.Counts["victim"] = loadRecoverySummarize(altered.Samples["victim"], altered.Events)
			if loadRecoveryReadExpected("victim", *sample, altered.Events) || len(loadRecoveryReportIssues(altered)) == 0 {
				t.Fatal("corrupt response or unexpected runner failure remained valid during outage")
			}
		})
	}
	for _, mutate := range []func(*loadRecoveryReport){
		func(r *loadRecoveryReport) { r.PollersDrained = false },
		func(r *loadRecoveryReport) { r.StartRetry = false },
		func(r *loadRecoveryReport) { r.Recovered.Epoch = 1 },
		func(r *loadRecoveryReport) { r.Recovered.Valid = false },
		func(r *loadRecoveryReport) { r.Recovered.Ready = false },
		func(r *loadRecoveryReport) { r.Recovered.BookValid = false },
		func(r *loadRecoveryReport) { r.Recovered.RecoveryRequired = true },
		func(r *loadRecoveryReport) { r.Recovered.Boundary.Phase = loadRecoveryShutdownBoundary },
		func(r *loadRecoveryReport) { r.Binary = nil },
		func(r *loadRecoveryReport) { r.Counts = nil },
		func(r *loadRecoveryReport) { r.Samples = nil },
		func(r *loadRecoveryReport) { r.Final = nil },
		func(r *loadRecoveryReport) { r.KnownFinal = nil },
		func(r *loadRecoveryReport) { r.Reaped = nil },
		func(r *loadRecoveryReport) { r.Events.ReadyNS = 0 },
		func(r *loadRecoveryReport) { r.Events.ProofEndNS = r.Events.ReadyNS },
	} {
		altered := report
		mutate(&altered)
		if len(loadRecoveryReportIssues(altered)) == 0 {
			t.Fatal("incomplete or failed evidence remained valid")
		}
	}
}

func TestPaperLoadRecoveryOutageAllowsOnlyExplicitAvailabilityFailures(t *testing.T) {
	events := loadRecoveryEvents{KillIssuedNS: 10, ReadyNS: 30}
	admitted, call := int64(15), int64(16)
	for _, sample := range []loadRecoverySample{
		{AttemptNS: 15, EndNS: 20, Outcome: "endpoint_unbound"},
		{AttemptNS: 15, EndNS: 20, Outcome: "transport_failed", AdmittedNS: &admitted, CallNS: &call},
		{AttemptNS: 15, EndNS: 20, Outcome: "response_unresolved", AdmittedNS: &admitted, CallNS: &call},
		{AttemptNS: 15, EndNS: 20, Outcome: "http_rejected", Status: http.StatusServiceUnavailable, Bytes: 10, AdmittedNS: &admitted, CallNS: &call},
	} {
		if !loadRecoveryReadExpected("victim", sample, events) {
			t.Fatalf("explicit victim availability failure rejected: %s", sample.Outcome)
		}
		if loadRecoveryReadExpected("bystander", sample, events) {
			t.Fatal("victim outage concealed a bystander failure")
		}
		before := sample
		before.AttemptNS, before.EndNS = 1, 9
		if loadRecoveryReadExpected("victim", before, events) {
			t.Fatal("outage exception concealed a pre-crash failure")
		}
		after := sample
		after.AttemptNS, after.EndNS = 31, 32
		if loadRecoveryReadExpected("victim", after, events) {
			t.Fatal("outage exception concealed an after-ready failure")
		}
	}
	rejection := loadRecoverySample{AttemptNS: 15, EndNS: 20, Outcome: "http_rejected", Status: http.StatusOK, Bytes: 10, AdmittedNS: &admitted, CallNS: &call}
	if loadRecoveryReadExpected("victim", rejection, events) {
		t.Fatal("availability classification concealed a successful HTTP status")
	}
}

func loadRecoverySafeReason(reason string) string {
	switch reason {
	case "market_invalid", "paper_reconciliation_asserted", "hold", "":
		return reason
	}
	return "other"
}

func loadRecoveryBoundaryIssues(account pg.Account, state rt.State, boundary loadRecoveryAuditBoundary) []string {
	issues := []string{}
	if state.Child != nil || len(account.Intents) != 0 || len(account.Reservations) != 0 || account.ObservedSequence != account.ProcessedSequence || len(state.PendingFacts) != 0 || len(state.UnresolvedFacts) != 0 || state.PolicyPending != nil || state.ClockDiscontinuity {
		issues = append(issues, "unresolved financial or source obligation")
	}
	if state.Parent == nil || !state.Parent.Started || state.Book == nil {
		return append(issues, "audit parent or book unavailable")
	}
	switch boundary.Phase {
	case loadRecoveryPausedBoundary:
		if state.Parent.Status != "paused" || !account.Ready || !account.Market.Valid || !state.Book.Valid || state.RecoveryRequired {
			issues = append(issues, "recovered paused account is not ready with a valid source")
		}
	case loadRecoveryShutdownBoundary:
		if !boundary.ProcessReaped || !boundary.KnownFinal || state.Parent.Status != "cancelled" {
			issues = append(issues, "shutdown boundary lacks reaped known-final cancelled parent")
		}
		// Stopping the public source can persist readiness invalidation before
		// final cancellation. This exact flag describes stopped source readiness;
		// financial obligations above must still be completely settled.
		stoppedSource := state.Reason == "market_invalid" && state.MarketID == "web-source-ended" && !account.Ready && !account.Market.Valid && !state.Book.Valid
		if state.RecoveryRequired && !stoppedSource {
			issues = append(issues, "shutdown retains unexplained recovery requirement")
		}
	default:
		issues = append(issues, "audit boundary unknown")
	}
	return issues
}

func TestPaperLoadRecoveryAuditSeparatesStoppedSourceReadinessFromFinality(t *testing.T) {
	account := pg.Account{Ready: true, Market: pg.Market{Valid: true}, ObservedSequence: 25, ProcessedSequence: 25}
	state := rt.State{Parent: &rt.Parent{Status: "paused", Started: true}, Book: &x.Book{Valid: true}}
	paused := loadRecoveryAuditBoundary{Phase: loadRecoveryPausedBoundary}
	shutdown := loadRecoveryAuditBoundary{Phase: loadRecoveryShutdownBoundary, ProcessReaped: true, KnownFinal: true}
	if issues := loadRecoveryBoundaryIssues(account, state, paused); len(issues) != 0 {
		t.Fatalf("ready paused recovery rejected: %v", issues)
	}
	// Reproduce the persisted source-ended transition before shutdown's cancel.
	account.Ready, account.Market.Valid = false, false
	state.Book.Valid, state.RecoveryRequired = false, true
	state.Reason, state.MarketID = "market_invalid", "web-source-ended"
	if len(loadRecoveryBoundaryIssues(account, state, paused)) == 0 {
		t.Fatal("source-ended readiness was admitted as recovered ready paused")
	}
	if len(loadRecoveryBoundaryIssues(account, state, shutdown)) == 0 {
		t.Fatal("nonterminal source-ended parent was admitted as final")
	}
	state.Parent.Status = "cancelled"
	if issues := loadRecoveryBoundaryIssues(account, state, shutdown); len(issues) != 0 {
		t.Fatalf("reaped known-final cancelled parent with only source-ended unreadiness rejected: %v", issues)
	}
	for _, mutate := range []func(*pg.Account, *rt.State, *loadRecoveryAuditBoundary){
		func(_ *pg.Account, s *rt.State, _ *loadRecoveryAuditBoundary) { s.Reason = "transport_outcome_unknown" },
		func(_ *pg.Account, s *rt.State, _ *loadRecoveryAuditBoundary) { s.MarketID = "unexplained-gap" },
		func(a *pg.Account, _ *rt.State, _ *loadRecoveryAuditBoundary) { a.Ready = true },
		func(a *pg.Account, _ *rt.State, _ *loadRecoveryAuditBoundary) { a.Market.Valid = true },
		func(_ *pg.Account, s *rt.State, _ *loadRecoveryAuditBoundary) { s.Book = &x.Book{Valid: true} },
		func(a *pg.Account, _ *rt.State, _ *loadRecoveryAuditBoundary) { a.ObservedSequence++ },
		func(a *pg.Account, _ *rt.State, _ *loadRecoveryAuditBoundary) { a.Intents = []pg.Intent{{}} },
		func(a *pg.Account, _ *rt.State, _ *loadRecoveryAuditBoundary) { a.Reservations = []pg.Reservation{{}} },
		func(_ *pg.Account, s *rt.State, _ *loadRecoveryAuditBoundary) { s.Child = &rt.Child{} },
		func(_ *pg.Account, s *rt.State, _ *loadRecoveryAuditBoundary) { s.PendingFacts = []rt.Fact{{}} },
		func(_ *pg.Account, s *rt.State, _ *loadRecoveryAuditBoundary) { s.UnresolvedFacts = []rt.Fact{{}} },
		func(_ *pg.Account, s *rt.State, _ *loadRecoveryAuditBoundary) { s.ClockDiscontinuity = true },
		func(_ *pg.Account, s *rt.State, _ *loadRecoveryAuditBoundary) { s.PolicyPending = &rt.PolicyDecision{} },
		func(_ *pg.Account, _ *rt.State, b *loadRecoveryAuditBoundary) { b.ProcessReaped = false },
		func(_ *pg.Account, _ *rt.State, b *loadRecoveryAuditBoundary) { b.KnownFinal = false },
	} {
		alteredAccount, alteredState, alteredBoundary := account, state, shutdown
		mutate(&alteredAccount, &alteredState, &alteredBoundary)
		if len(loadRecoveryBoundaryIssues(alteredAccount, alteredState, alteredBoundary)) == 0 {
			t.Fatal("unexplained recovery flag, financial debt or unestablished finality was admitted")
		}
	}
}

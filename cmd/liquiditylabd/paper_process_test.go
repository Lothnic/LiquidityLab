package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/liquiditylab/liquiditylab/internal/api"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

type paperTestProcess struct {
	command      *exec.Cmd
	done         chan error
	logPath, url string
	waited       bool
}

func startPaperTestProcess(t *testing.T, binary, dsn string, opt paperServerOptions, mode string, gates ...*paperTestGate) *paperTestProcess {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"paper", mode, "--listen", "127.0.0.1:0", "--schema", opt.schema, "--account", opt.account, "--source", "synthetic", "--web-dir", opt.webDir, "--duration", "1m"}
	if mode == "create" {
		args = append(args, "--initial-base", opt.initialBase.String(), "--initial-quote", opt.initialQuote.String())
	}
	if opt.policyTarget != "" {
		args = append(args, "--policy-target", opt.policyTarget, "--policy-sha256", opt.policyHash, "--policy-profile", "paper", "--policy-fallback", "pause", "--policy-timeout", "100ms", "--policy-allow-unvalidated")
	}
	command := exec.Command(binary, args...)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "LIQUIDITYLAB_POLICY_") && !strings.HasPrefix(e, "LIQUIDITYLAB_DATABASE_URL=") && !strings.HasPrefix(e, "LIQUIDITYLAB_API_TOKEN=") && !strings.HasPrefix(e, "LIQUIDITYLAB_TEST_FAULT_POINT=") {
			command.Env = append(command.Env, e)
		}
	}
	command.Env = append(command.Env, "LIQUIDITYLAB_DATABASE_URL="+dsn, "LIQUIDITYLAB_API_TOKEN="+daemonTestToken)
	if len(gates) > 0 {
		gate := gates[0]
		command.Env = append(command.Env, "LIQUIDITYLAB_TEST_FAULT_POINT="+gate.point)
		command.ExtraFiles = []*os.File{gate.childEvents, gate.childRelease}
	}
	command.Stderr = log
	if err = command.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	log.Close()
	if len(gates) > 0 {
		gates[0].childEvents.Close()
		gates[0].childRelease.Close()
	}
	p := &paperTestProcess{command: command, done: make(chan error, 1), logPath: logPath}
	go func() { p.done <- command.Wait() }()
	t.Cleanup(func() {
		if !p.waited {
			_ = p.command.Process.Kill()
			select {
			case <-p.done:
				p.waited = true
			case <-time.After(5 * time.Second):
				t.Error("child did not exit")
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
		case err := <-p.done:
			p.waited = true
			t.Fatalf("daemon startup failed: %v\n%s", err, data)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("paper child did not publish its bound address")
	return nil
}

func paperProcessRequest(t *testing.T, p *paperTestProcess, opt paperServerOptions, method, suffix string, body any) []byte {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	url := p.url + "/v1/accounts/" + paperDaemonTenant + "/" + opt.account + suffix
	request, err := http.NewRequest(method, url, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+daemonTestToken)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("process %s %s: %d %s", method, suffix, response.StatusCode, data)
	}
	return data
}

func paperProcessAccount(t *testing.T, p *paperTestProcess, opt paperServerOptions) api.AccountDTO {
	t.Helper()
	var account api.AccountDTO
	if err := json.Unmarshal(paperProcessRequest(t, p, opt, "GET", "", nil), &account); err != nil {
		t.Fatal(err)
	}
	return account
}

func waitPaperProcessAccount(t *testing.T, p *paperTestProcess, opt paperServerOptions, condition func(api.AccountDTO) bool) api.AccountDTO {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		a := paperProcessAccount(t, p, opt)
		if condition(a) {
			return a
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("paper process condition not reached")
	return api.AccountDTO{}
}

func TestPaperProcessSIGKILLResumeKeepsAcceptedEconomicsAndRequiresResume(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGKILL process evidence requires Unix")
	}
	opt := daemonOptions(t)
	dsn := daemonPostgres(t, &opt)
	binary := filepath.Join(t.TempDir(), "liquiditylabd")
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-race", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build process binary: %v\n%s", err, output)
	}
	first := startPaperTestProcess(t, binary, dsn, opt, "create")
	ready := waitPaperProcessAccount(t, first, opt, func(a api.AccountDTO) bool { return a.Ready })
	plan := d.DefaultPlan()
	plan.Quantity = d.MustDecimal("0.05")
	plan.MaxChildQuantity = d.MustDecimal("0.01")
	plan.QuoteBudget = d.MustDecimal("20")
	plan.BaseBalance = d.Zero
	plan.Deadline = 30 * d.Second
	paperProcessRequest(t, first, opt, "POST", "/commands", map[string]any{"id": "process-create", "kind": "create", "expected_revision": ready.Revision, "expected_authority_revision": ready.AuthorityRevision, "plan_id": "process-plan", "plan": plan})
	paused := paperProcessAccount(t, first, opt)
	start := map[string]any{"id": "process-start", "kind": "start", "expected_revision": paused.Revision, "expected_authority_revision": paused.AuthorityRevision}
	paperProcessRequest(t, first, opt, "POST", "/commands", start)
	waitPaperProcessAccount(t, first, opt, func(a api.AccountDTO) bool {
		return a.FillCount > 0 && a.Plan.Status == "active" && a.Plan.Remaining.Sign() > 0
	})
	if err := first.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-first.done:
		first.waited = true
		if err == nil || first.command.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			t.Fatalf("child was not killed at OS boundary: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("killed process was not reaped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := pg.Open(ctx, dsn, opt.schema)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := pg.Key{TenantID: paperDaemonTenant, AccountID: opt.account}
	crashed, err := store.GetAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := rt.DecodeSnapshot(crashed.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	// Accepted simulated fills live in durable observations even if SIGKILL
	// interrupted their reducer. Independently reconstruct the expected economics.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, "SELECT payload FROM "+pgx.Identifier{opt.schema, "observations"}.Sanitize()+" WHERE tenant_id=$1 AND account_id=$2 ORDER BY sequence", key.TenantID, key.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	fills := map[string]d.Fill{}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var fact struct {
			Outcome struct {
				Fills []d.Fill `json:"fills"`
			} `json:"outcome"`
			Fills []d.Fill `json:"fills"`
		}
		if err = json.Unmarshal(raw, &fact); err != nil {
			t.Fatal(err)
		}
		for _, fill := range append(fact.Fills, fact.Outcome.Fills...) {
			if prior, ok := fills[fill.ExecutionID]; ok && !prior.Equal(fill) {
				t.Fatal("conflicting durable fill")
			}
			fills[fill.ExecutionID] = fill
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	quantity, notional, fees := d.Zero, d.Zero, d.Zero
	for _, fill := range fills {
		quantity = quantity.Add(fill.Quantity)
		notional = notional.Add(fill.Price.Mul(fill.Quantity))
		fees = fees.Add(fill.Fee)
	}
	second := startPaperTestProcess(t, binary, dsn, opt, "resume")
	recovered := waitPaperProcessAccount(t, second, opt, func(a api.AccountDTO) bool { return a.Ready })
	if recovered.Plan == nil || recovered.Plan.Status != "paused" || !recovered.Plan.Deadline.Equal(checkpoint.Parent.Deadline) || !recovered.Plan.ArrivalMid.Equal(checkpoint.Parent.ArrivalMid) || !recovered.Plan.Filled.Equal(quantity) || !recovered.Plan.FeesQuote.Equal(fees) || recovered.FillCount != len(fills) || recovered.Child != nil {
		t.Fatalf("resume lost or repeated accepted economics: %+v", recovered)
	}
	for _, balance := range recovered.Balances {
		if balance.Currency == "BTC" && !balance.Total.Equal(quantity) || balance.Currency == "USD" && !balance.Total.Equal(d.MustDecimal("20").Sub(notional).Sub(fees)) {
			t.Fatalf("recovered balance mismatch: %+v", balance)
		}
	}
	for _, fill := range recovered.Fills {
		prior, ok := fills[fill.ExecutionID]
		if !ok || !prior.Equal(fill) {
			t.Fatal("recovery invented a fill")
		}
	}
	for _, hold := range recovered.Reserved {
		if !hold.Amount.IsZero() {
			t.Fatal("paper recovery retained unexplained hold")
		}
	}
	retained, err := store.GetAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Ownership.Epoch != crashed.Ownership.Epoch+1 || len(retained.Intents) != 0 || len(retained.Reservations) != 0 || retained.ObservedSequence != retained.ProcessedSequence {
		t.Fatal("recovered ownership or financial obligations disagree")
	}
	var receipt struct {
		Duplicate bool `json:"duplicate"`
	}
	if err = json.Unmarshal(paperProcessRequest(t, second, opt, "POST", "/commands", start), &receipt); err != nil || !receipt.Duplicate {
		t.Fatal("original start retry was not idempotent")
	}
	until := time.Now().Add(1100 * time.Millisecond)
	for time.Now().Before(until) {
		a := paperProcessAccount(t, second, opt)
		if a.Plan.Status != "paused" || !a.Plan.Filled.Equal(quantity) {
			t.Fatal("recovery or retry restarted execution")
		}
		time.Sleep(30 * time.Millisecond)
	}
	if err = second.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-second.done:
		second.waited = true
		if err != nil {
			t.Fatalf("resumed shutdown failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("resumed shutdown did not finish")
	}
	log, _ := os.ReadFile(second.logPath)
	if !strings.Contains(string(log), "known_final=true") {
		t.Fatalf("shutdown did not establish finality: %s", log)
	}
	t.Logf("SIGKILL/reap/resume preserved %d accepted fills, quantity=%s, fees=%s; recovered paused, original command retry duplicate, known-final shutdown", len(fills), quantity, fees)
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	pb "github.com/liquiditylab/liquiditylab/gen/go/liquiditylab/v1"
	"github.com/liquiditylab/liquiditylab/internal/api"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
	"google.golang.org/grpc"
)

type paperTestGate struct {
	point                              string
	events                             chan string
	release, childEvents, childRelease *os.File
}

func newPaperTestGate(t *testing.T, point string) *paperTestGate {
	t.Helper()
	events, childEvents, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	childRelease, release, err := os.Pipe()
	if err != nil {
		events.Close()
		childEvents.Close()
		t.Fatal(err)
	}
	gate := &paperTestGate{point: point, events: make(chan string, 16), release: release, childEvents: childEvents, childRelease: childRelease}
	go func() {
		defer close(gate.events)
		scanner := bufio.NewScanner(events)
		for scanner.Scan() {
			gate.events <- scanner.Text()
		}
	}()
	t.Cleanup(func() { events.Close(); release.Close(); childEvents.Close(); childRelease.Close() })
	return gate
}

func waitPaperGate(t *testing.T, gate *paperTestGate, expected string) {
	t.Helper()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case event, ok := <-gate.events:
			if !ok {
				t.Fatalf("fault controller closed before %s", expected)
			}
			if event == expected {
				return
			}
		case <-timeout.C:
			t.Fatalf("process did not reach checkpoint %s", expected)
		}
	}
}

func pausePaperProcessAtGate(t *testing.T, p *paperTestProcess, opt paperServerOptions, gate *paperTestGate) {
	t.Helper()
	type reply struct {
		status int
		data   []byte
		err    error
	}
	for attempt := 0; attempt < 16; attempt++ {
		account := paperProcessAccount(t, p, opt)
		data, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("fault-pause-%d", attempt), "kind": "pause", "expected_revision": account.Revision, "expected_authority_revision": account.AuthorityRevision})
		request, _ := http.NewRequest("POST", p.url+"/v1/accounts/"+paperDaemonTenant+"/"+opt.account+"/commands", bytes.NewReader(data))
		request.Header.Set("Authorization", "Bearer "+daemonTestToken)
		request.Header.Set("Content-Type", "application/json")
		responseDone := make(chan reply, 1)
		go func() {
			response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
			if err != nil {
				responseDone <- reply{err: err}
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
			responseDone <- reply{response.StatusCode, body, err}
		}()
		timeout := time.NewTimer(6 * time.Second)
		retry := false
		for !retry {
			select {
			case event, ok := <-gate.events:
				if !ok {
					timeout.Stop()
					t.Fatal("cancel checkpoint process exited")
				}
				if event == "reached:"+gate.point {
					timeout.Stop()
					return
				}
			case result := <-responseDone:
				timeout.Stop()
				if result.err == nil && result.status == http.StatusConflict && bytes.Contains(result.data, []byte("stale_revision")) {
					retry = true
					continue
				}
				t.Fatalf("pause returned without checkpoint: status=%d body=%s err=%v", result.status, result.data, result.err)
			case <-timeout.C:
				t.Fatal("pause did not reach cancellation checkpoint")
			}
		}
	}
	t.Fatal("pause could not acquire current financial authority")
}

func killPaperTestProcess(t *testing.T, p *paperTestProcess) {
	t.Helper()
	if err := p.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.done:
		p.waited = true
		if err == nil || p.command.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
			t.Fatalf("expected SIGKILL: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGKILL child did not reap")
	}
}

// This independent check reads the simulated venue's accepted source facts,
// including any effect not yet reduced when the process died.
func acceptedPaperFills(t *testing.T, dsn string, opt paperServerOptions) map[string]d.Fill {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, "SELECT payload FROM "+pgx.Identifier{opt.schema, "observations"}.Sanitize()+" WHERE tenant_id=$1 AND account_id=$2 ORDER BY sequence", paperDaemonTenant, opt.account)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
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
				t.Fatal("contradictory source fill identity")
			}
			fills[fill.ExecutionID] = fill
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return fills
}

func assertPaperRecoveredEconomics(t *testing.T, p *paperTestProcess, dsn string, opt paperServerOptions, before pg.Account, accepted map[string]d.Fill) api.AccountDTO {
	t.Helper()
	checkpoint, err := rt.DecodeSnapshot(before.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	account := waitPaperProcessAccount(t, p, opt, func(a api.AccountDTO) bool { return a.Ready })
	quantity, notional, fees := d.Zero, d.Zero, d.Zero
	for _, fill := range accepted {
		if fill.FeeCurrency != "quote" {
			t.Fatal("fixture expects quote fees")
		}
		quantity = quantity.Add(fill.Quantity)
		notional = notional.Add(fill.Price.Mul(fill.Quantity))
		fees = fees.Add(fill.Fee)
	}
	if account.Plan == nil || account.Plan.Status != "paused" || !account.Plan.Deadline.Equal(checkpoint.Parent.Deadline) || !account.Plan.Start.Equal(checkpoint.Parent.Start) || !account.Plan.ArrivalMid.Equal(checkpoint.Parent.ArrivalMid) || !account.Plan.Filled.Equal(quantity) || !account.Plan.FeesQuote.Equal(fees) || account.FillCount != len(accepted) || account.Child != nil {
		t.Fatalf("unsafe recovered report: %+v", account)
	}
	for _, fill := range account.Fills {
		prior, ok := accepted[fill.ExecutionID]
		if !ok || !prior.Equal(fill) {
			t.Fatal("lost or invented recovered fill")
		}
	}
	for _, balance := range account.Balances {
		if balance.Currency == "BTC" && !balance.Total.Equal(opt.initialBase.Add(quantity)) || balance.Currency == "USD" && !balance.Total.Equal(opt.initialQuote.Sub(notional).Sub(fees)) {
			t.Fatalf("recovered balance mismatch: %+v", balance)
		}
	}
	for _, hold := range account.Reserved {
		if !hold.Amount.IsZero() {
			t.Fatal("recovered paper hold remains")
		}
	}
	return account
}

func TestPaperProcessCrashBoundaryMatrixAndSuspendedSender(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process/control-pipe harness")
	}
	if os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN") == "" {
		t.Skip("LIQUIDITYLAB_TEST_POSTGRES_DSN is not set")
	}
	binary := filepath.Join(t.TempDir(), "liquiditylabd-faults")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-race", "-tags", "liquiditylab_faults", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build checkpoint binary: %v\n%s", err, output)
	}
	for _, tc := range []struct {
		name, point                            string
		passive, pause, policy, large, suspend bool
	}{
		{name: "intent", point: "intent_committed"},
		{name: "authorized-before-send", point: "dispatch_authorized"},
		{name: "accepted-before-reduction", point: "submit_effect_committed"},
		{name: "fill-before-cursor", point: "fill_committed"},
		{name: "partial-child-fill-before-cursor", point: "fill_committed", large: true, passive: true},
		{name: "resting-acknowledged", point: "resting_ack_committed", passive: true},
		{name: "cancel-accepted-before-reduction", point: "cancel_effect_committed", passive: true, pause: true},
		{name: "ordered-trade-before-reduction", point: "trade_fact_committed", passive: true},
		{name: "pending-policy", point: "policy_pending", policy: true},
		{name: "suspended-old-sender", point: "dispatch_authorized", suspend: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := daemonOptions(t)
			dsn := daemonPostgres(t, &opt)
			if tc.large {
				opt.initialQuote = d.MustDecimal("4000")
			}
			if tc.policy {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				worker := grpc.NewServer()
				pb.RegisterPolicyServiceServer(worker, &daemonPolicyServer{hash: strings.Repeat("a", 64)})
				go worker.Serve(listener)
				t.Cleanup(worker.Stop)
				opt.policyTarget = listener.Addr().String()
				opt.policyHash = strings.Repeat("a", 64)
			}
			gate := newPaperTestGate(t, tc.point)
			old := startPaperTestProcess(t, binary, dsn, opt, "create", gate)
			ready := waitPaperProcessAccount(t, old, opt, func(a api.AccountDTO) bool { return a.Ready })
			plan := d.DefaultPlan()
			plan.Quantity = d.MustDecimal("0.05")
			plan.MaxChildQuantity = d.MustDecimal("0.01")
			plan.QuoteBudget = opt.initialQuote
			plan.BaseBalance = opt.initialBase
			plan.Deadline = 30 * d.Second
			if tc.large {
				plan.Quantity = d.MustDecimal("30")
				plan.MaxChildQuantity = d.MustDecimal("5")
				plan.Deadline = 120 * d.Second
			}
			policy := "scheduled"
			if tc.passive {
				policy = "liquidity-aware"
			}
			paperProcessRequest(t, old, opt, "POST", "/commands", map[string]any{"id": "fault-create", "kind": "create", "expected_revision": ready.Revision, "expected_authority_revision": ready.AuthorityRevision, "plan_id": "fault-plan", "plan": plan, "policy": policy})
			paused := paperProcessAccount(t, old, opt)
			paperProcessRequest(t, old, opt, "POST", "/commands", map[string]any{"id": "fault-start", "kind": "start", "expected_revision": paused.Revision, "expected_authority_revision": paused.AuthorityRevision})
			if tc.pause {
				waitPaperProcessAccount(t, old, opt, func(a api.AccountDTO) bool { return a.Child != nil && a.Child.State == "open" })
				pausePaperProcessAtGate(t, old, opt, gate)
			} else {
				waitPaperGate(t, gate, "reached:"+tc.point)
			}
			if !tc.suspend {
				killPaperTestProcess(t, old)
			}
			store, err := pg.Open(context.Background(), dsn, opt.schema)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			key := pg.Key{TenantID: paperDaemonTenant, AccountID: opt.account}
			before, err := store.GetAccount(context.Background(), key)
			if err != nil {
				t.Fatal(err)
			}
			accepted := acceptedPaperFills(t, dsn, opt)
			if tc.large {
				state, err := rt.DecodeSnapshot(before.Snapshot)
				if err != nil || state.Child == nil || state.Child.Order.Remaining.Sign() <= 0 || len(state.Fills) != 1 {
					t.Fatal("checkpoint did not contain a partial child fill")
				}
			}
			recoveryStarted := time.Now()
			next := startPaperTestProcess(t, binary, dsn, opt, "resume")
			recovered := assertPaperRecoveredEconomics(t, next, dsn, opt, before, accepted)
			recoveryElapsed := time.Since(recoveryStarted)
			retained, err := store.GetAccount(context.Background(), key)
			if err != nil {
				t.Fatal(err)
			}
			if retained.Ownership.Epoch != before.Ownership.Epoch+1 || len(retained.Intents) != 0 || len(retained.Reservations) != 0 || retained.ObservedSequence != retained.ProcessedSequence {
				t.Fatal("recovery left an unknown obligation")
			}
			if tc.policy {
				state, err := rt.DecodeSnapshot(retained.Snapshot)
				if err != nil || state.PolicyPending != nil || state.LastPolicyDecision == nil || state.LastPolicyDecision.Reason != "owner_restart" {
					t.Fatal("old pending policy was not fenced and audited")
				}
			}
			if tc.suspend {
				if _, err = gate.release.Write([]byte{1}); err != nil {
					t.Fatal(err)
				}
				waitPaperGate(t, gate, "handoff_returned")
				after := acceptedPaperFills(t, dsn, opt)
				if len(after) != len(accepted) {
					t.Fatal("resumed old sender appended a financial effect")
				}
				var oldEffects int
				conn, err := pgx.Connect(context.Background(), dsn)
				if err != nil {
					t.Fatal(err)
				}
				err = conn.QueryRow(context.Background(), "SELECT count(*) FROM "+pgx.Identifier{opt.schema, "observations"}.Sanitize()+" WHERE kind='transport' AND observation_id LIKE 'paper-submit-%'").Scan(&oldEffects)
				conn.Close(context.Background())
				if err != nil || oldEffects != 0 {
					t.Fatal("old sender submitted after takeover")
				}
				killPaperTestProcess(t, old)
			}
			if current := paperProcessAccount(t, next, opt); current.Plan.Status != "paused" || !current.Plan.Filled.Equal(recovered.Plan.Filled) {
				t.Fatal("recovery began trading without explicit resume")
			}
			if err = next.command.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-next.done:
				next.waited = true
				if err != nil {
					log, _ := os.ReadFile(next.logPath)
					t.Fatalf("resumed process shutdown: %v\n%s", err, log)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("resumed process did not stop")
			}
			log, _ := os.ReadFile(next.logPath)
			if !strings.Contains(string(log), "known_final=true") {
				t.Fatal("shutdown was not known-final")
			}
			t.Logf("%s: epoch %d -> %d, %d accepted fills, recovery=%s debt=%d->0, exact economics preserved, paused recovery and known-final shutdown", tc.point, before.Ownership.Epoch, retained.Ownership.Epoch, len(accepted), recoveryElapsed, before.ObservedSequence-before.ProcessedSequence)
		})
	}
}

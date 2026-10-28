package main

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"github.com/liquiditylab/liquiditylab/internal/marketdata"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func TestPaperCreateResumeOptionsPreserveBalanceAuthority(t *testing.T) {
	for _, name := range []string{"LIQUIDITYLAB_POLICY_TARGET", "LIQUIDITYLAB_POLICY_SHA256", "LIQUIDITYLAB_POLICY_FALLBACK", "LIQUIDITYLAB_POLICY_ALLOW_UNVALIDATED"} {
		t.Setenv(name, "")
	}
	common := []string{"--schema", "ll_options", "--account", "existing", "--source", "synthetic", "--web-dir", "."}
	for _, tc := range []struct {
		name          string
		args          []string
		resume, valid bool
	}{
		{"explicit create", append([]string{"create", "--initial-base", "0", "--initial-quote", "20"}, common...), false, true},
		{"legacy create", append([]string{"--initial-base", "0", "--initial-quote", "20"}, common...), false, true},
		{"resume", append([]string{"resume"}, common...), true, true},
		{"resume rejects balances", append([]string{"resume", "--initial-quote", "20"}, common...), true, false},
		{"resume rejects empty balance flag", append([]string{"resume", "--initial-base="}, common...), true, false},
		{"create requires balances", append([]string{"create"}, common...), false, false},
		{"unknown mode", append([]string{"reset"}, common...), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt, err := parsePaperOptions(tc.args)
			if (err == nil) != tc.valid || err == nil && opt.resume != tc.resume {
				t.Fatalf("resume=%v err=%v", opt.resume, err)
			}
		})
	}
}

func resumeTestSource(session string) paperSource {
	return func(ctx context.Context, deliver func(context.Context, marketdata.Event) error) error {
		event := daemonSnapshot()
		event.ReceiptSessionID = session
		if err := deliver(ctx, event); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
}

func TestPaperDaemonResumeRetainsFillsBalancesAndDeadlinePaused(t *testing.T) {
	opt := daemonOptions(t)
	dsn := daemonPostgres(t, &opt)
	static, err := paperStatic(opt.webDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stopSource := context.WithCancel(context.Background())
	defer stopSource()
	old, err := newPaperDaemon(ctx, opt, dsn, daemonTestToken, static, io.Discard, resumeTestSource("before-stop"))
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = old.Close()
		}
	}()
	ready := daemonWait(t, old, func(v rt.View) bool { return v.Account.Ready })
	plan := d.DefaultPlan()
	plan.Quantity = d.MustDecimal("0.05")
	plan.MaxChildQuantity = d.MustDecimal("0.01")
	plan.QuoteBudget = d.MustDecimal("20")
	plan.BaseBalance = d.Zero
	plan.Deadline = 60 * d.Second
	op, cancel := paperServerOperation()
	defer cancel()
	created, err := old.actor.Do(op, rt.Command{ID: "resume-create", Kind: "create", ExpectedRevision: ready.Account.Revision, PlanID: "resume-plan", Plan: &plan})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.actor.Do(op, rt.Command{ID: "resume-start", Kind: "start", ExpectedRevision: created.Revision}); err != nil {
		t.Fatal(err)
	}
	daemonWait(t, old, func(v rt.View) bool { return len(v.State.Fills) > 0 && v.State.Parent.Status == "active" })
	// Stop the source and actor without the daemon's graceful parent cancellation.
	// The separate OS harness exercises actual process death.
	stopSource()
	<-old.sourceDone
	if err = old.actor.Close(op); err != nil {
		t.Fatal(err)
	}
	before, err := old.store.GetAccount(op, old.key)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := rt.DecodeSnapshot(before.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	old.store.Close()
	stopped = true
	opt.resume = true
	opt.initialBase = d.Zero
	opt.initialQuote = d.Zero
	next := daemonNewTest(t, opt, dsn, resumeTestSource("after-resume"))
	after := daemonWait(t, next, func(v rt.View) bool { return v.Account.Ready })
	if after.State.Parent.Status != "paused" || !after.State.Parent.Deadline.Equal(saved.Parent.Deadline) || !after.State.Parent.Start.Equal(saved.Parent.Start) || !reflect.DeepEqual(before.Balances, after.Account.Balances) || !reflect.DeepEqual(saved.Fills, after.State.Fills) || after.Account.Ownership.Epoch != before.Ownership.Epoch+1 || len(after.Account.Intents) != 0 || len(after.Account.Reservations) != 0 {
		t.Fatalf("resume changed retained economics or resumed trading: before=%+v after=%+v", before, after)
	}
	until := time.Now().Add(1100 * time.Millisecond)
	for time.Now().Before(until) {
		view := daemonView(t, next)
		if view.State.Parent.Status != "paused" || !reflect.DeepEqual(view.State.Fills, after.State.Fills) {
			t.Fatal("recovered plan traded without explicit resume")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPaperResumeMissingAccountNeverCreatesIt(t *testing.T) {
	opt := daemonOptions(t)
	opt.resume = true
	dsn := daemonPostgres(t, &opt)
	static, err := paperStatic(opt.webDir)
	if err != nil {
		t.Fatal(err)
	}
	if daemon, err := newPaperDaemon(context.Background(), opt, dsn, daemonTestToken, static, io.Discard, resumeTestSource("missing")); err == nil || daemon != nil {
		t.Fatal("missing account was created on resume")
	}
	store, err := pg.Open(context.Background(), dsn, opt.schema)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.GetAccount(context.Background(), pg.Key{TenantID: paperDaemonTenant, AccountID: opt.account}); !errors.Is(err, pg.ErrNotFound) {
		t.Fatalf("resume changed absent identity: %v", err)
	}
}

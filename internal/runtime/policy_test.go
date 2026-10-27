package runtime

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/liquiditylab/liquiditylab/gen/go/liquiditylab/v1"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"github.com/liquiditylab/liquiditylab/internal/inference"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
	"google.golang.org/protobuf/proto"
)

type controlledPolicy struct {
	calls    chan *pb.DecisionRequest
	release  chan d.Action
	count    atomic.Int64
	fail     error
	mutate   func(*inference.Proposal)
	returned chan struct{}
}

func (w *controlledPolicy) Propose(_ context.Context, req *pb.DecisionRequest) (inference.Proposal, error) {
	defer func() {
		if w.returned != nil {
			select {
			case w.returned <- struct{}{}:
			default:
			}
		}
	}()
	w.count.Add(1)
	w.calls <- proto.Clone(req).(*pb.DecisionRequest)
	action := <-w.release // Deliberately ignores deadlines to exercise actor isolation.
	if w.fail != nil {
		return inference.Proposal{}, w.fail
	}
	proposal := inference.Proposal{Binding: proto.Clone(req.Binding).(*pb.DecisionBinding), Action: action, Mode: "unvalidated"}
	if w.mutate != nil {
		w.mutate(&proposal)
	}
	return proposal, nil
}
func newControlledPolicy() *controlledPolicy {
	return &controlledPolicy{calls: make(chan *pb.DecisionRequest, 10), release: make(chan d.Action, 10)}
}
func policyTestActor(t *testing.T, w PolicyWorker, fallback string, timeout time.Duration, customize ...func(*Config)) (*Actor, *pg.Store, pg.Key) {
	t.Helper()
	store, key, _ := actorTestDB(t)
	cfg := DefaultConfig(store, key)
	cfg.AutoExecute = true
	cfg.Lease = 120 * time.Millisecond
	cfg.OwnerID = "policy-test-owner"
	cfg.Execution.DecisionInterval = int64(30 * time.Millisecond)
	cfg.Execution.MinMessageInterval = 0
	cfg.Execution.MinRestingAge = 0
	cfg.Policy = &PolicyConfig{Worker: w, PolicySHA256: strings.Repeat("a", 64), Profile: "paper", AllowUnvalidated: true, Fallback: fallback, Timeout: timeout}
	for _, change := range customize {
		change(&cfg)
	}
	ctx, cancel := actorTestContext(t)
	actor, err := New(ctx, cfg)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := actorTestContext(t)
		defer cancel()
		if err := actor.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	actorTestMarket(t, actor, "policy-book")
	actorTestReconcile(t, actor, "policy-reconcile")
	return actor, store, key
}
func policyTestStart(t *testing.T, a *Actor) {
	t.Helper()
	plan := actorTestPlan()
	plan.Quantity = d.MustDecimal("0.2")
	plan.MaxChildQuantity = d.MustDecimal("0.1")
	actorTestDo(t, a, Command{ID: "policy-create", Kind: "create", PlanID: "policy-plan", Plan: plan})
	actorTestDo(t, a, Command{ID: "policy-start", Kind: "start"})
}
func policyTestCall(t *testing.T, w *controlledPolicy) *pb.DecisionRequest {
	t.Helper()
	select {
	case request := <-w.calls:
		return request
	case <-time.After(time.Second):
		t.Fatal("no policy request")
		return nil
	}
}

func TestPolicyStalledWorkerDoesNotBlockCancelOrLeaseAndLateCannotAct(t *testing.T) {
	w := newControlledPolicy()
	a, store, key := policyTestActor(t, w, "pause", 100*time.Millisecond)
	policyTestStart(t, a)
	request := policyTestCall(t, w)
	before := actorTestView(t, a)
	started := time.Now()
	actorTestDo(t, a, authorityCommand(before, "policy-cancel", "cancel"))
	if time.Since(started) > 90*time.Millisecond {
		t.Fatal("stalled inference delayed cancellation")
	}
	resolved := actorTestWaitView(t, a, time.Second, func(v View) bool { return v.State.PolicyPending == nil && v.State.LastPolicyDecision != nil })
	if resolved.State.LastPolicyDecision.Result != "fallback_pause" || resolved.State.DecisionCount != 1 || len(resolved.State.Fills) != 0 || resolved.State.Parent.Status != "cancelled" {
		t.Fatalf("unexpected cancellation outcome: %+v", resolved.State)
	}
	time.Sleep(180 * time.Millisecond)
	current := actorTestView(t, a)
	if !current.Account.LeaseUntil.After(time.Now()) {
		t.Fatal("worker prevented ownership renewal")
	}
	w.release <- d.AggressiveNormal
	late := actorTestWaitView(t, a, time.Second, func(v View) bool { return v.State.PolicyLateOutcome != nil })
	if late.State.PolicyLateOutcome.RequestID != request.Binding.RequestId || late.State.DecisionCount != 1 || late.State.Child != nil || len(late.State.Fills) != 0 || late.State.LastPolicyDecision.Result != "fallback_pause" {
		t.Fatalf("late response changed resolved slot: %+v", late.State)
	}
	journal, err := store.Journal(context.Background(), key, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	requests, resolutions, lateEntries := 0, 0, 0
	for _, entry := range journal {
		switch entry.Kind {
		case "policy_request":
			requests++
		case "policy_fallback", "cancel":
			resolutions++
		case "policy_late":
			lateEntries++
		}
	}
	if requests != 1 || resolutions != 1 || lateEntries != 1 {
		t.Fatalf("durable outcomes requests=%d resolutions=%d late=%d", requests, resolutions, lateEntries)
	}
}
func TestPolicyExpiryAndSaturationExecuteOneBaselinePerSlot(t *testing.T) {
	w := newControlledPolicy()
	a, _, _ := policyTestActor(t, w, "baseline", 45*time.Millisecond)
	policyTestStart(t, a)
	policyTestCall(t, w)
	after := actorTestWaitView(t, a, time.Second, func(v View) bool {
		return v.State.DecisionCount >= 3 && v.State.LastPolicyDecision != nil && v.State.LastPolicyDecision.Reason == "worker_busy"
	})
	if w.count.Load() != 1 || after.State.LastPolicyDecision.Result != "fallback_baseline" || len(after.State.Fills) != 1 || after.State.PolicyPending != nil {
		t.Fatalf("saturated worker spawned duplicate calls or missed fallback: calls=%d state=%+v", w.count.Load(), after.State)
	}
	actorTestAutoPause(t, a, "policy-saturation-pause")
	before := actorTestView(t, a)
	w.release <- d.AggressiveNormal
	late := actorTestWaitView(t, a, time.Second, func(v View) bool { return v.State.PolicyLateOutcome != nil })
	if late.State.DecisionCount != before.State.DecisionCount || len(late.State.Fills) != len(before.State.Fills) {
		t.Fatal("late outcome acted twice")
	}
}
func TestPolicyValidProposalUsesLatestBookAndPreservesHoldAuthority(t *testing.T) {
	w := newControlledPolicy()
	a, _, _ := policyTestActor(t, w, "pause", 150*time.Millisecond)
	policyTestStart(t, a)
	request := policyTestCall(t, w)
	initial := actorTestView(t, a)
	actorTestMarket(t, a, "policy-newer-book")
	// A source-only trade advances the journal but preserves financial authority.
	if err := actorTestMarketFromSession(t, a, actorTestTrade("policy-source-trade", "0.01", 2, time.Now().UnixNano()), "policy-source-market", "policy-source-session"); err != nil {
		t.Fatal(err)
	}
	w.release <- d.Hold
	applied := actorTestWaitView(t, a, time.Second, func(v View) bool {
		return v.State.LastPolicyDecision != nil && v.State.LastPolicyDecision.RequestID == request.Binding.RequestId
	})
	if applied.State.LastPolicyDecision.Result != "policy_action" || applied.State.LastPolicyDecision.AppliedMarketSnapshotID != "policy-source-market" || applied.AuthorityRevision() != initial.AuthorityRevision() || applied.State.LastPolicyDecision.Bundle.PolicySHA256 != strings.Repeat("a", 64) {
		t.Fatalf("proposal was not safely rebound to latest book: %+v", applied.State.LastPolicyDecision)
	}
	actorTestAutoPause(t, a, "policy-valid-pause")
	// Release a possible next request so no mock goroutine survives cleanup.
	w.release <- d.Hold
}
func TestPolicyControlRevisionAndReadinessInvalidateOutstandingProposal(t *testing.T) {
	for _, effect := range []string{"pause_resume", "gap"} {
		t.Run(effect, func(t *testing.T) {
			w := newControlledPolicy()
			a, _, _ := policyTestActor(t, w, "pause", 150*time.Millisecond)
			policyTestStart(t, a)
			policyTestCall(t, w)
			switch effect {
			case "pause_resume":
				actorTestAutoPause(t, a, "policy-control-pause")
				actorTestDo(t, a, Command{ID: "policy-control-resume", Kind: "resume"})
			case "gap":
				ctx, cancel := actorTestContext(t)
				err := a.Market(ctx, d.MarketEvent{Kind: "gap", Symbol: "BTC/USD", Invalid: true}, "policy-gap")
				cancel()
				if err != nil {
					t.Fatal(err)
				}
			}
			w.release <- d.AggressiveNormal
			resolved := actorTestWaitView(t, a, time.Second, func(v View) bool {
				if effect == "pause_resume" {
					return v.State.PolicyLateOutcome != nil
				}
				return v.State.PolicyPending == nil && v.State.LastPolicyDecision != nil
			})
			if resolved.State.LastPolicyDecision.Result != "fallback_pause" || effect == "gap" && resolved.State.Parent.Status != "paused" || resolved.State.Child != nil || len(resolved.State.Fills) != 0 {
				t.Fatalf("stale proposal acted: %+v", resolved.State)
			}
			if effect == "pause_resume" {
				actorTestAutoPause(t, a, "policy-test-final-pause")
				w.release <- d.Hold
			}
		})
	}
}
func TestPolicyBundlePinnedAcrossRestartAndPendingRequestFenced(t *testing.T) {
	w := newControlledPolicy()
	w.returned = make(chan struct{}, 1)
	a, store, key := policyTestActor(t, w, "baseline", 150*time.Millisecond)
	policyTestStart(t, a)
	policyTestCall(t, w)
	before := actorTestView(t, a)
	ctx, cancel := actorTestContext(t)
	defer cancel()
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := a.config
	cfg.OwnerID = "policy-restarted-owner"
	cfg.RecoveryProof = pg.RecoveryProof{PreviousSenderIsolated: true, Reconciled: true, Reference: "isolated-old-policy-owner"}
	copyPolicy := *cfg.Policy
	cfg.Policy = &copyPolicy
	cfg.Policy.PolicySHA256 = strings.Repeat("b", 64)
	if _, err := New(ctx, cfg); !errors.Is(err, ErrInvalid) {
		t.Fatalf("changed bundle accepted on restart: %v", err)
	}
	cfg.Policy.PolicySHA256 = strings.Repeat("a", 64)
	restarted, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close(ctx)
	after := actorTestView(t, restarted)
	if after.State.PolicyPending != nil || after.State.Parent.Status != "paused" || after.State.LastPolicyDecision.Reason != "owner_restart" || after.State.DecisionCount != before.State.PolicyPending.Slot || after.State.Parent.PolicyBundle.PolicySHA256 != strings.Repeat("a", 64) {
		t.Fatalf("restart did not fence pinned request: %+v", after.State)
	}
	w.release <- d.AggressiveNormal
	select {
	case <-w.returned:
	case <-ctx.Done():
		t.Fatal("old worker did not return after restart")
	}
	policyTestAssertNoRecoveredMutation(t, store, key, after.Account)
}

func policyTestAssertNoRecoveredMutation(t *testing.T, store *pg.Store, key pg.Key, expected pg.Account) {
	t.Helper()
	ctx, cancel := actorTestContext(t)
	defer cancel()
	current, err := store.GetAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	// View clones compact raw JSON; PostgreSQL jsonb reads include whitespace.
	// Hash their canonical marshaling so formatting cannot mimic a mutation.
	if current.Revision != expected.Revision || current.Ownership != expected.Ownership || current.Ready != expected.Ready || inputDigest(current.Snapshot) != inputDigest(expected.Snapshot) || inputDigest(current.Balances) != inputDigest(expected.Balances) || len(current.Intents) != 0 || len(current.Reservations) != 0 || current.ObservedSequence != expected.ObservedSequence || current.ProcessedSequence != expected.ProcessedSequence {
		t.Fatalf("old policy result mutated recovered account: revision=%d want=%d owner=%+v want=%+v snapshot_changed=%t balances_changed=%t intents=%d reservations=%d", current.Revision, expected.Revision, current.Ownership, expected.Ownership, inputDigest(current.Snapshot) != inputDigest(expected.Snapshot), inputDigest(current.Balances) != inputDigest(expected.Balances), len(current.Intents), len(current.Reservations))
	}
	entries, err := store.Journal(ctx, key, expected.Revision, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("old policy result committed journal entries after recovery: %+v", entries)
	}
}

func TestPolicyRunningPreviousOwnerCannotCommitReturnedProposalAfterTakeover(t *testing.T) {
	w := newControlledPolicy()
	w.returned = make(chan struct{}, 1)
	old, store, key := policyTestActor(t, w, "pause", 200*time.Millisecond, func(c *Config) { c.Lease = 2 * time.Second })
	policyTestStart(t, old)
	request := policyTestCall(t, w)
	before := actorTestView(t, old)
	cfg := old.config
	cfg.OwnerID, cfg.AutoExecute = "policy-takeover-owner", false
	// This deliberately leaves the old actor alive. The proof is mocked solely
	// to exercise the database ownership fence; it establishes no OS isolation.
	cfg.RecoveryProof = pg.RecoveryProof{PreviousSenderIsolated: true, Reconciled: true, Reference: "mocked-db-fence-only-not-os-isolation"}
	ctx, cancel := actorTestContext(t)
	defer cancel()
	recovered, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close(ctx)
	after := actorTestView(t, recovered)
	decision := after.State.LastPolicyDecision
	if after.Account.Ownership.Epoch != before.Account.Ownership.Epoch+1 || after.Account.Ready || after.State.Book.Valid || after.State.Parent.Status != "paused" || after.State.PolicyPending != nil || decision == nil || decision.RequestID != request.Binding.RequestId || decision.Result != "fallback_pause" || decision.Reason != "owner_restart" || decision.OwnerGeneration != before.Account.Ownership.Epoch || after.State.DecisionCount != before.State.PolicyPending.Slot {
		t.Fatalf("takeover did not durably fence pending inference: %+v", after)
	}
	w.release <- d.AggressiveNormal
	select {
	case <-w.returned:
	case <-ctx.Done():
		t.Fatal("previous owner's worker did not return")
	}
	// Both loops remain active while the returned proposal is available. The
	// recovered account must stay paused throughout the old request's validity.
	actorTestAssertStable(t, recovered, 250*time.Millisecond, func(v View) bool {
		return v.Account.Revision == after.Account.Revision && v.State.Parent.Status == "paused" && v.State.PolicyPending == nil && v.State.Child == nil && len(v.State.Fills) == 0
	})
	if _, err := old.View(ctx); !errors.Is(err, pg.ErrOwnership) {
		t.Fatalf("previous actor retained account ownership after takeover: %v", err)
	}
	if err := old.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// Close synchronizes access to the old actor's private state. A false busy
	// flag proves its loop consumed the response instead of merely abandoning it.
	if old.policyBusy {
		t.Fatal("previous owner never consumed its returned proposal")
	}
	policyTestAssertNoRecoveredMutation(t, store, key, after.Account)
}
func TestPolicyConfigurationRejectsImplicitFallbackAndLive(t *testing.T) {
	worker := newControlledPolicy()
	for _, cfg := range []PolicyConfig{{Worker: worker, PolicySHA256: strings.Repeat("a", 64), Profile: "live", AllowUnvalidated: true, Fallback: "pause", Timeout: time.Millisecond}, {Worker: worker, PolicySHA256: strings.Repeat("a", 64), Profile: "paper", AllowUnvalidated: true, Timeout: time.Millisecond}, {Worker: worker, PolicySHA256: strings.Repeat("a", 64), Profile: "paper", AllowUnvalidated: true, Fallback: "baseline", Timeout: 201 * time.Millisecond}} {
		if _, err := policyBundle(&cfg); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid config accepted: %+v", cfg)
		}
	}
}

func TestPolicyFillInvalidatesOutstandingProposal(t *testing.T) {
	w := newControlledPolicy()
	a, _, _ := policyTestActor(t, w, "pause", 150*time.Millisecond)
	policyTestStart(t, a)
	policyTestCall(t, w)
	w.release <- d.PassiveNormal
	policyTestCall(t, w)
	resting := actorTestWaitView(t, a, time.Second, func(v View) bool { return v.State.Child != nil && v.State.Child.State == "open" })
	child := resting.State.Child
	fill := d.Fill{ExecutionID: "policy-observed-fill", OrderID: child.ID, Side: "buy", Quantity: d.MustDecimal("0.01"), Price: child.Order.Intent.Price, Fee: child.Order.Intent.Price.Mul(d.MustDecimal("0.00001")), FeeCurrency: "quote", Time: 0, Maker: true}
	ctx, cancel := actorTestContext(t)
	err := a.ObserveFill(ctx, "policy-fill-event", child.ID, fill)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	afterFill := actorTestView(t, a)
	if afterFill.AuthorityRevision() == resting.AuthorityRevision() {
		t.Fatal("fill did not change financial authority")
	}
	w.release <- d.AggressiveNormal
	resolved := actorTestWaitView(t, a, time.Second, func(v View) bool { return v.State.PolicyPending == nil && v.State.Parent.Status == "paused" })
	if len(resolved.State.Fills) != 1 || resolved.State.LastPolicyDecision.Result != "fallback_pause" || resolved.State.LastPolicyDecision.Reason != "stale_authority_or_expired" {
		t.Fatalf("stale fill response acted: %+v", resolved.State.LastPolicyDecision)
	}
}
func TestPolicyActorRejectsChangedHashSchemaAndLiveMode(t *testing.T) {
	for _, effect := range []string{"hash", "schema", "live_mode", "wrong_mode", "worker_busy"} {
		t.Run(effect, func(t *testing.T) {
			w := newControlledPolicy()
			w.mutate = func(p *inference.Proposal) {
				switch effect {
				case "hash":
					p.Binding.PolicySha256 = strings.Repeat("b", 64)
				case "schema":
					p.Binding.ObservationSchemaSha256 = strings.Repeat("b", 64)
				case "live_mode":
					p.Mode = "live"
				case "wrong_mode":
					p.Mode = "arbitrary"
				}
			}
			if effect == "worker_busy" {
				w.fail = inference.ErrBusy
			}
			a, _, _ := policyTestActor(t, w, "pause", 150*time.Millisecond)
			policyTestStart(t, a)
			policyTestCall(t, w)
			w.release <- d.AggressiveNormal
			resolved := actorTestWaitView(t, a, time.Second, func(v View) bool { return v.State.PolicyPending == nil && v.State.LastPolicyDecision != nil })
			if resolved.State.LastPolicyDecision.Result != "fallback_pause" || resolved.State.Child != nil || len(resolved.State.Fills) != 0 {
				t.Fatalf("incompatible response acted: %+v", resolved.State)
			}
		})
	}
}

func TestPolicyExpiryUsesDedicatedTimerWithoutLeasePolling(t *testing.T) {
	w := newControlledPolicy()
	a, _, _ := policyTestActor(t, w, "pause", 40*time.Millisecond, func(cfg *Config) {
		cfg.Lease = 600 * time.Millisecond
		cfg.Execution.DecisionInterval = int64(200 * time.Millisecond)
	})
	policyTestStart(t, a)
	policyTestCall(t, w)
	before := actorTestView(t, a)
	started := time.Now()
	after := actorTestWaitView(t, a, 150*time.Millisecond, func(v View) bool { return v.State.LastPolicyDecision != nil })
	if after.State.LastPolicyDecision.Result != "fallback_pause" || time.Since(started) > 120*time.Millisecond || !after.Account.LeaseUntil.Equal(before.Account.LeaseUntil) {
		t.Fatalf("expiry waited for full maintenance or renewed lease: before=%s after=%s result=%+v", before.Account.LeaseUntil, after.Account.LeaseUntil, after.State.LastPolicyDecision)
	}
	w.release <- d.Hold
}
func TestPolicyShadowRecordsProposalAndExecutesPinnedBaseline(t *testing.T) {
	w := newControlledPolicy()
	a, _, _ := policyTestActor(t, w, "pause", 150*time.Millisecond, func(cfg *Config) { cfg.Policy.Profile = "shadow" })
	policyTestStart(t, a)
	policyTestCall(t, w)
	w.release <- d.Hold
	after := actorTestWaitView(t, a, time.Second, func(v View) bool { return len(v.State.Fills) == 1 && v.State.LastPolicyDecision != nil })
	decision := after.State.LastPolicyDecision
	if decision.Result != "shadow_baseline" || decision.ProposedAction == nil || *decision.ProposedAction != d.Hold || decision.Action != d.AggressiveNormal || decision.Bundle.Profile != "shadow" {
		t.Fatalf("shadow failed to retain proposal/applied baseline: %+v", decision)
	}
	actorTestAutoPause(t, a, "policy-shadow-pause")
	w.release <- d.Hold
}

func TestPolicyBundleReplacementRequiresSettledSession(t *testing.T) {
	for _, status := range []string{"completed", "cancelled", "expired"} {
		if !canReplacePolicyBundle(State{Parent: &Parent{Status: status}}, pg.Account{}) {
			t.Fatalf("settled %s parent rejected", status)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*State, *pg.Account)
	}{
		{"active", func(s *State, _ *pg.Account) { s.Parent.Status = "active" }},
		{"paused", func(s *State, _ *pg.Account) { s.Parent.Status = "paused" }},
		{"cancelling", func(s *State, _ *pg.Account) { s.Parent.Status = "cancel_requested" }},
		{"child", func(s *State, _ *pg.Account) { s.Child = &Child{} }},
		{"policy slot", func(s *State, _ *pg.Account) { s.PolicyPending = &PolicyDecision{} }},
		{"pending facts", func(s *State, _ *pg.Account) { s.PendingFacts = []Fact{{}} }},
		{"unresolved facts", func(s *State, _ *pg.Account) { s.UnresolvedFacts = []Fact{{}} }},
		{"intents", func(_ *State, a *pg.Account) { a.Intents = []pg.Intent{{}} }},
		{"reservations", func(_ *State, a *pg.Account) { a.Reservations = []pg.Reservation{{}} }},
		{"inbox", func(_ *State, a *pg.Account) { a.ObservedSequence = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, account := State{Parent: &Parent{Status: "cancelled"}}, pg.Account{}
			tc.edit(&state, &account)
			if canReplacePolicyBundle(state, account) {
				t.Fatal("unsettled session accepted a new bundle")
			}
		})
	}
	if !canReplacePolicyBundle(State{UnresolvedFacts: []Fact{{Resolved: true}}}, pg.Account{}) {
		t.Fatal("resolved historical facts blocked an otherwise clean account")
	}
}

func TestPolicyNewSessionCanPinNewBundleAfterSettledRestart(t *testing.T) {
	a, _, _ := policyTestActor(t, newControlledPolicy(), "pause", 150*time.Millisecond)
	plan := actorTestPlan()
	actorTestDo(t, a, Command{ID: "old-policy-create", Kind: "create", PlanID: "old-policy-plan", Plan: plan})
	actorTestDo(t, a, Command{ID: "old-policy-cancel", Kind: "cancel"})
	ctx, cancel := actorTestContext(t)
	defer cancel()
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := a.config
	copied := *cfg.Policy
	cfg.Policy, cfg.OwnerID, cfg.AutoExecute = &copied, "next-policy-owner", false
	cfg.Policy.PolicySHA256 = strings.Repeat("b", 64)
	cfg.RecoveryProof = pg.RecoveryProof{PreviousSenderIsolated: true, Reconciled: true, Reference: "settled-policy-upgrade"}
	restarted, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close(ctx)
	before := actorTestView(t, restarted)
	if before.State.PolicyBundle.PolicySHA256 != strings.Repeat("b", 64) || before.State.Parent.PolicyBundle.PolicySHA256 != strings.Repeat("a", 64) {
		t.Fatalf("upgrade rewrote historical parent or missed new default: %+v", before.State.Parent)
	}
	actorTestDo(t, restarted, Command{ID: "new-policy-create", Kind: "create", PlanID: "new-policy-plan", Plan: plan})
	after := actorTestView(t, restarted)
	if after.State.Parent.PolicyBundle.PolicySHA256 != strings.Repeat("b", 64) {
		t.Fatal("new session did not pin new bundle")
	}
	for _, row := range after.Account.Plans {
		if row.ID == "old-policy-plan" {
			var old Parent
			if decodeInternal(row.Detail, &old) != nil || old.PolicyBundle.PolicySHA256 != strings.Repeat("a", 64) {
				t.Fatal("durable historical parent bundle changed")
			}
		}
	}
}

func TestPolicyLateOutcomeRemainsAuditableWithUnresolvedFacts(t *testing.T) {
	w := newControlledPolicy()
	a, _, _ := policyTestActor(t, w, "pause", 150*time.Millisecond)
	policyTestStart(t, a)
	request := policyTestCall(t, w)
	actorTestAutoPause(t, a, "late-audit-pause")
	ctx, cancel := actorTestContext(t)
	defer cancel()
	fill := d.Fill{ExecutionID: "unexpected-late-fill", OrderID: "missing-child", Side: "buy", Quantity: d.MustDecimal("0.01"), Price: d.MustDecimal("99"), FeeCurrency: "quote", Maker: true}
	if err := a.ObserveFill(ctx, "unresolved-before-late", fill.OrderID, fill); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("unexpected fact did not require recovery: %v", err)
	}
	w.release <- d.AggressiveNormal
	after := actorTestWaitView(t, a, time.Second, func(v View) bool { return v.State.PolicyLateOutcome != nil })
	if after.State.PolicyLateOutcome.RequestID != request.Binding.RequestId || after.State.LastPolicyDecision.Result != "fallback_pause" || after.State.Parent.Status != "paused" || after.State.Child != nil || len(after.State.Fills) != 0 || !after.State.RecoveryRequired {
		t.Fatalf("late audit changed financial state or was lost: %+v", after.State)
	}
}

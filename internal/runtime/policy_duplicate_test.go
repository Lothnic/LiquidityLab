package runtime

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	pb "github.com/liquiditylab/liquiditylab/gen/go/liquiditylab/v1"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"github.com/liquiditylab/liquiditylab/internal/inference"
	"github.com/liquiditylab/liquiditylab/internal/telemetry"
	"google.golang.org/protobuf/proto"
)

func TestPolicyRepeatedAcceptedProposalCannotEnqueueTwice(t *testing.T) {
	w := newControlledPolicy()
	w.returned = make(chan struct{}, 1)
	recorder := telemetry.New()
	a, store, key := policyTestActor(t, w, "pause", 150*time.Millisecond, func(c *Config) {
		c.AutoExecute = false
		c.Lease = time.Minute
		c.Telemetry = recorder
	})
	policyTestStart(t, a)
	ctx := context.Background()
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// Drive the stopped owner serially to repeat the exact worker result without
	// racing the loop or allowing another cadence slot to obscure the assertion.
	frozen := time.Now()
	a.now = func() time.Time { return frozen }
	defer a.stopPolicyTimer()
	if err := a.startPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	request := policyTestCall(t, w)
	result := policyResult{audit: *a.state.PolicyPending, proposal: inference.Proposal{Binding: proto.Clone(request.Binding).(*pb.DecisionBinding), Action: d.PassiveNormal, Mode: "unvalidated"}}
	// Return the mock worker as well. Its channel result has no running actor;
	// the exact result above is delivered explicitly below.
	w.release <- d.PassiveNormal
	select {
	case <-w.returned:
	case <-time.After(time.Second):
		t.Fatal("controlled worker did not return")
	}
	initialAuthority := a.state.AuthorityRevision
	// Repeated public book updates cannot perpetually invalidate one otherwise
	// current proposal. Compile its action against the final available book.
	for i := 0; i < 100; i++ {
		err := a.market(ctx, marketInput{Event: d.MarketEvent{Kind: "snapshot", Symbol: "BTC/USD", Bids: []d.Level{{Price: d.MustDecimal("99.50"), Quantity: d.MustDecimal("2")}}, Asks: []d.Level{{Price: d.MustDecimal("100.50"), Quantity: d.MustDecimal("2")}}}, SnapshotID: fmt.Sprintf("duplicate-book-%03d", i)})
		if err != nil {
			t.Fatal(err)
		}
	}
	if a.state.AuthorityRevision != initialAuthority {
		t.Fatal("book-only updates changed financial authority")
	}
	a.receivePolicy(result)
	first, err := cloneState(a.state)
	if err != nil || first.PolicyPending != nil || first.LastPolicyDecision == nil || first.LastPolicyDecision.Result != "policy_action" || first.LastPolicyDecision.AppliedMarketSnapshotID != "duplicate-book-099" || first.Child == nil || first.Child.State != "queued" || !first.Child.Order.Intent.Price.Equal(d.MustDecimal("99.50")) || first.DecisionCount != 1 || len(a.account.Intents) != 1 || len(a.account.Reservations) != 1 {
		t.Fatalf("initial proposal not applied once against latest book: state=%+v account=%+v err=%v", first, a.account, err)
	}
	firstAccount := a.account
	previousRevision := firstAccount.Revision
	for duplicate := 0; duplicate < 3; duplicate++ {
		a.receivePolicy(result)
		account, err := store.GetAccount(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		saved, err := DecodeSnapshot(account.Snapshot)
		if err != nil || account.Revision != previousRevision+1 || inputDigest(saved) != inputDigest(a.state) {
			t.Fatalf("duplicate %d late audit was not durably committed: revision=%d previous=%d err=%v", duplicate, account.Revision, previousRevision, err)
		}
		previousRevision = account.Revision
		if a.state.DecisionCount != 1 || a.state.AuthorityRevision != first.AuthorityRevision || !reflect.DeepEqual(a.state.LastPolicyDecision, first.LastPolicyDecision) || !reflect.DeepEqual(a.state.Child, first.Child) || !reflect.DeepEqual(a.state.Fills, first.Fills) || !reflect.DeepEqual(account.Balances, firstAccount.Balances) || !reflect.DeepEqual(account.Intents, firstAccount.Intents) || !reflect.DeepEqual(account.Reservations, firstAccount.Reservations) || account.ObservedSequence != firstAccount.ObservedSequence || account.ProcessedSequence != firstAccount.ProcessedSequence || a.state.PolicyLateOutcome == nil || a.state.PolicyLateOutcome.RequestID != request.Binding.RequestId || a.state.PolicyLateOutcome.Result != "late_response" || a.state.PolicyLateOutcome.Action != d.Hold {
			t.Fatalf("duplicate %d changed financial/action authority: state=%+v account=%+v", duplicate, a.state, account)
		}
	}
	metrics := recorder.Snapshot()
	if metrics.Counters[telemetry.PolicyDecisions].Value != 1 || metrics.Counters[telemetry.PolicyFallbacks].Value != 0 {
		t.Fatal("duplicate proposal counted another policy resolution or fallback")
	}
}

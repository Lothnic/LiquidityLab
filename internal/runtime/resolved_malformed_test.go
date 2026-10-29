package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func TestActorResolvedMalformedSourceContinuesThroughFollowingValidObservation(t *testing.T) {
	for _, tc := range []struct{ name, kind, payload, reason string }{
		{"fill", "fill", `[]`, "malformed_fill_fact"},
		{"trade", paperTradeKind, `{"id":"wrong-source-identity"}`, "malformed_paper_trade_fact"},
		{"transport", paperTransportKind, `{}`, "invalid_transport_fact"},
		{"unsupported", "future_account_fact", `{"unknown":true}`, "unsupported_account_fact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, key, _, actor := actorTestReady(t, 0)
			actorTestCreateAndStart(t, actor, "resolved-malformed-plan")
			queued := actorTestQueue(t, actor, d.PassiveNormal, "resolved-malformed-queue")
			childID := queued.State.Child.ID
			if err := actorTestDispatch(t, actor, childID, "resolved-malformed-dispatch"); err != nil {
				t.Fatal(err)
			}
			fill := d.Fill{ExecutionID: "resolved-malformed-execution", OrderID: childID, Side: "buy", Quantity: d.MustDecimal("0.025"), Price: d.MustDecimal("99"), Fee: d.MustDecimal("0.002475"), FeeCurrency: "quote", Maker: true, Time: 1}
			ctx, cancel := actorTestContext(t)
			err := actor.ObserveFill(ctx, "resolved-malformed-original-fill", childID, fill)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			actorTestDo(t, actor, Command{ID: "resolved-malformed-cancel-child", Kind: "action", Action: d.CancelChild})
			before := actorTestWaitView(t, actor, 2*time.Second, func(v View) bool { return v.State.Child == nil && len(v.Account.Reservations) == 0 })
			bad := pg.Observation{ID: "resolved-malformed-source", Kind: tc.kind, Payload: json.RawMessage(tc.payload)}
			var badSequence int64
			if tc.kind == paperTradeKind || tc.kind == paperTransportKind {
				badSequence, err = store.RecordPaperObservation(context.Background(), key, actor.owner, bad)
			} else {
				badSequence, err = store.RecordObservation(context.Background(), key, bad)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := recoveryTestDrain(t, actor); !errors.Is(err, ErrRecoveryRequired) {
				t.Fatalf("malformed source not quarantined: %v", err)
			}
			quarantined := actorTestView(t, actor)
			if quarantined.Account.ProcessedSequence != before.Account.ProcessedSequence || quarantined.Account.ObservedSequence != badSequence || len(quarantined.State.UnresolvedFacts) != 1 || quarantined.State.UnresolvedFacts[0].ID != bad.ID || quarantined.State.UnresolvedFacts[0].Reason != tc.reason || quarantined.State.UnresolvedFacts[0].Resolved {
				t.Fatalf("incorrect quarantine cursor/audit: %+v", quarantined)
			}
			// A valid later receipt repeats an execution already applied before
			// quarantine. It must be consumed without a second economic effect.
			following := Fact{ID: "resolved-malformed-following-valid", IntentID: childID, Fill: fill}
			followingSequence, err := store.RecordObservation(context.Background(), key, pg.Observation{ID: following.ID, Kind: "fill", Payload: jsonBytes(following)})
			if err != nil || followingSequence != badSequence+1 {
				t.Fatalf("following sequence=%d err=%v", followingSequence, err)
			}
			actorTestDo(t, actor, Command{ID: "resolved-malformed-local-cancel", Kind: "cancel"})
			pending := actorTestView(t, actor)
			if pending.State.Parent.Status != "cancel_requested" || pending.Account.Ready || pending.Account.ProcessedSequence != before.Account.ProcessedSequence {
				t.Fatalf("premature finality over unresolved source debt: %+v", pending)
			}
			ctx, cancel = actorTestContext(t)
			_, err = actor.Reconcile(ctx, Reconciliation{ID: "resolved-malformed-reconciliation", ExpectedRevision: pending.Account.Revision, Evidence: "operator discarded malformed source and retained known execution", NoExternalOrders: true, Balances: pending.Account.Balances, ResolvedFactIDs: []string{bad.ID}, AbortParent: true})
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			if err := recoveryTestDrain(t, actor); err != nil {
				t.Fatalf("resolved source blocked following valid receipt: %v", err)
			}
			final := actorTestWaitView(t, actor, 2*time.Second, func(v View) bool {
				return v.Account.ProcessedSequence == followingSequence && v.State.Parent.Status == "cancelled"
			})
			if final.Account.ObservedSequence != followingSequence || !final.Account.Ready || final.State.RecoveryRequired || final.State.Child != nil || len(final.Account.Intents) != 0 || len(final.Account.Reservations) != 0 || len(final.State.Fills) != 1 || !final.State.Fills[0].Equal(fill) || len(final.State.UnresolvedFacts) != 1 || !final.State.UnresolvedFacts[0].Resolved || final.State.UnresolvedFacts[0].Reason != tc.reason || !findBalance(final.Account.Balances, "BTC").Total.Equal(d.MustDecimal("1.025")) || !findBalance(final.Account.Balances, "USD").Total.Equal(d.MustDecimal("997.522525")) {
				t.Fatalf("resolved source changed cursor/finality/economics: %+v", final)
			}
			// Re-enter the reducer after completion to detect re-quarantine or a
			// stranded background block, and retain the original raw source audit.
			if err := recoveryTestDrain(t, actor); err != nil {
				t.Fatal(err)
			}
			after := actorTestView(t, actor)
			if after.Account.Revision != final.Account.Revision || after.Account.ProcessedSequence != followingSequence || len(after.State.UnresolvedFacts) != 1 || !after.State.UnresolvedFacts[0].Resolved {
				t.Fatalf("resolved source re-entered quarantine: %+v", after)
			}
			raw, err := store.GetObservation(context.Background(), key, bad.ID)
			if err != nil || raw.Kind != bad.Kind || inputDigest(json.RawMessage(raw.Payload)) != inputDigest(json.RawMessage(bad.Payload)) {
				t.Fatalf("raw source changed: observation=%+v err=%v", raw, err)
			}
		})
	}
}

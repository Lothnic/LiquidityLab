package runtime

import (
	"context"
	"errors"
	"fmt"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	"github.com/liquiditylab/liquiditylab/internal/faultpoint"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

// A complete source outcome is durable before any individual fill is applied.
// Each fill commits with its unique identity. The observation cursor advances
// only after the whole source outcome is incorporated, so restart can resume it.
// drainFacts performs one cooperative quantum. A pending result is scheduling
// state, never a successful acknowledgement that the inbox has been consumed.
func (a *Actor) drainFacts(ctx context.Context) error {
	progress, err := a.reduceFactsQuantum(ctx, a.factBudget())
	if err != nil {
		return err
	}
	if progress.More {
		return errFactsPending
	}
	return nil
}

func (a *Actor) applyPaperTrade(ctx context.Context, observation pg.Observation, fact paperTradeFact) error {
	if a.resolvedFact(observation.ID) {
		return a.consumeOnly(ctx, observation)
	}
	next, err := cloneState(a.state)
	if err != nil {
		return err
	}
	change := pg.Change{ConsumeObservationThrough: observation.Sequence}
	if fact.IntentID != "" {
		if next.Child == nil || next.Child.ID != fact.IntentID {
			return a.unresolved(ctx, observation, Fact{ID: observation.ID, IntentID: fact.IntentID, Reason: "unexpected_paper_trade_order"})
		}
		next.Child.Order.QueueAhead = fact.QueueAhead
		if next.Child.Order.Remaining.IsZero() {
			next.Child = nil
		}
	}
	if next.Parent != nil && next.Parent.Started {
		quantity := fact.Event.Quantity
		if fact.Event.Side == "sell" {
			quantity = quantity.Neg()
		}
		next.TradeFlow = next.TradeFlow.Add(quantity)
	}
	id := fmt.Sprintf("observation-%d-trade-complete", observation.Sequence)
	a.stamp(&next, id, fact.InputDigest)
	// Trade flow and source cursor updates alone do not change account authority.
	// Queue movement and child/parent completion still invalidate old commands.
	sourceOnly := change.Plan == nil && inputDigest(next.Child) == inputDigest(a.state.Child)
	_, err = a.persistRevision(ctx, next, pg.Command{ID: id, Kind: "paper_trade", ExpectedRevision: a.account.Revision}, change, sourceOnly)
	return err
}

func (a *Actor) resolvedFact(id string) bool {
	for _, fact := range a.state.UnresolvedFacts {
		if fact.ID == id && fact.Resolved {
			return true
		}
	}
	return false
}

func (a *Actor) consumeOnly(ctx context.Context, observation pg.Observation) error {
	next, err := cloneState(a.state)
	if err != nil {
		return err
	}
	id := fmt.Sprintf("observation-%d-complete", observation.Sequence)
	a.stamp(&next, id, inputDigest(observation.Payload))
	_, err = a.persistRevision(ctx, next, pg.Command{ID: id, Kind: "observation_consumed", ExpectedRevision: a.account.Revision}, pg.Change{ConsumeObservationThrough: observation.Sequence}, true)
	return err
}

func (a *Actor) unresolved(ctx context.Context, observation pg.Observation, fact Fact) error {
	if fact.ParentID == "" && a.state.Parent != nil {
		fact.ParentID = a.state.Parent.ID
	}
	for _, prior := range a.state.UnresolvedFacts {
		if prior.ID == fact.ID {
			if prior.Resolved {
				return a.consumeOnly(ctx, observation)
			}
			return ErrRecoveryRequired
		}
	}
	if len(a.state.UnresolvedFacts) >= a.config.MaxFacts {
		return ErrHistoryLimit
	}
	next, err := cloneState(a.state)
	if err != nil {
		return err
	}
	next.UnresolvedFacts = append(next.UnresolvedFacts, fact)
	next.RecoveryRequired = true
	next.Reason = nonempty(fact.Reason, "unresolved_account_fact")
	ready := false
	id := fmt.Sprintf("observation-%d-unresolved", observation.Sequence)
	a.stamp(&next, id, inputDigest(fact))
	_, err = a.persist(ctx, next, pg.Command{ID: id, Kind: "unresolved_fact", ExpectedRevision: a.account.Revision}, pg.Change{Ready: &ready})
	if err != nil {
		return err
	}
	return ErrRecoveryRequired
}

func (a *Actor) applyFill(ctx context.Context, observation pg.Observation, fact Fact, index int, consume bool) error {
	if a.resolvedFact(fact.ID) {
		if consume {
			return a.consumeOnly(ctx, observation)
		}
		return nil
	}
	for _, prior := range a.state.Fills {
		if prior.ExecutionID == fact.Fill.ExecutionID {
			if !prior.Equal(fact.Fill) {
				fact.Reason = "conflicting_execution_identity"
				return a.unresolved(ctx, observation, fact)
			}
			if consume {
				return a.consumeOnly(ctx, observation)
			}
			return nil
		}
	}
	// Old-session duplicate fills may arrive after a new parent has started.
	prior, err := a.config.Store.GetExecution(ctx, a.config.Key, "paper", fact.Fill.ExecutionID)
	if err == nil {
		var saved d.Fill
		if decodeInternal(prior.Detail, &saved) != nil || prior.IntentID != fact.IntentID || !saved.Equal(fact.Fill) {
			fact.Reason = "conflicting_execution_identity"
			return a.unresolved(ctx, observation, fact)
		}
		if consume {
			return a.consumeOnly(ctx, observation)
		}
		return nil
	}
	if !errors.Is(err, pg.ErrNotFound) {
		return err
	}
	if a.state.Child == nil || a.state.Parent == nil || a.state.Child.ID != fact.IntentID || fact.Fill.OrderID != fact.IntentID {
		fact.Reason = "unexpected_execution_order"
		return a.unresolved(ctx, observation, fact)
	}
	if len(a.state.Fills) >= a.config.MaxFills {
		return ErrHistoryLimit
	}
	if fact.Fill.Quantity.Cmp(a.state.Child.Order.Remaining) > 0 {
		fact.Reason = "fill_exceeds_child_remaining"
		return a.unresolved(ctx, observation, fact)
	}
	next, err := cloneState(a.state)
	if err != nil {
		return err
	}
	ledger, err := a.ledger(next)
	if err != nil {
		return err
	}
	oldBase, oldQuote := ledger.Base, ledger.Quote
	if _, err = ledger.Apply(fact.Fill); err != nil {
		fact.Reason = "execution_requires_reconciliation: " + err.Error()
		return a.unresolved(ctx, observation, fact)
	}
	next.Fills = append(next.Fills, fact.Fill)
	next.Child.Order.Remaining = next.Child.Order.Remaining.Sub(fact.Fill.Quantity)
	if !next.Child.Order.Intent.Passive {
		side := "asks"
		if fact.Fill.Side == "sell" {
			side = "bids"
		}
		next.Book.Consume(side, fact.Fill.Price, fact.Fill.Quantity)
	}
	balances := append([]pg.Balance(nil), a.account.Balances...)
	baseFound, quoteFound := false, false
	for i := range balances {
		delta := d.Zero
		if balances[i].Currency == next.BaseCurrency {
			delta = ledger.Base.Sub(oldBase)
			baseFound = true
		}
		if balances[i].Currency == next.QuoteCurrency {
			delta = ledger.Quote.Sub(oldQuote)
			quoteFound = true
		}
		balances[i].Total = balances[i].Total.Add(delta)
		balances[i].Free = balances[i].Free.Add(delta)
	}
	if !baseFound || !quoteFound {
		fact.Reason = "missing_account_currency"
		return a.unresolved(ctx, observation, fact)
	}
	change := pg.Change{Balances: balances, Execution: &pg.Execution{ID: fact.Fill.ExecutionID, IntentID: fact.IntentID, Venue: "paper", Detail: jsonBytes(fact.Fill)}}
	if consume {
		change.ConsumeObservationThrough = observation.Sequence
	}
	if next.Child.Order.Remaining.Sign() > 0 {
		intent := next.Child.Order.Intent
		intent.Quantity = next.Child.Order.Remaining
		kind, remaining, err := x.ReservationFor(intent, next.Fees)
		if err != nil {
			return err
		}
		currency := next.QuoteCurrency
		if kind == "base" {
			currency = next.BaseCurrency
		}
		change.ReservationUpdates = []pg.ReservationAdjustment{{IntentID: fact.IntentID, Currency: currency, Remaining: remaining}}
	} else {
		// The complete submitted quantity is known filled, so its hold is final.
		change.Resolutions = []pg.Resolution{{IntentID: fact.IntentID, State: "final", KnownFinal: true, Evidence: jsonBytes(map[string]string{"reason": "complete_fill", "profile": "paper"})}}
		if consume {
			next.Child = nil
		}
	}
	id := fmt.Sprintf("observation-%d-fill-%d", observation.Sequence, index)
	a.stamp(&next, id, inputDigest(fact))
	_, err = a.persist(ctx, next, pg.Command{ID: id, Kind: "execution", ExpectedRevision: a.account.Revision}, change)
	if err == nil {
		faultpoint.Hit("fill_committed")
	}
	return err
}

func (a *Actor) applyTransport(ctx context.Context, observation pg.Observation, fact transportFact) error {
	if a.resolvedFact(observation.ID) {
		return a.consumeOnly(ctx, observation)
	}
	if a.state.Child == nil || a.state.Child.ID != fact.IntentID {
		prior, err := a.config.Store.GetIntent(ctx, a.config.Key, fact.IntentID)
		if err != nil {
			return err
		}
		if prior.State == "final" {
			next, err := cloneState(a.state)
			if err != nil {
				return err
			}
			if _, err := a.config.Store.LookupCommand(ctx, a.config.Key, fact.OperationID); err == nil {
				return a.consumeOnly(ctx, observation)
			} else if !errors.Is(err, pg.ErrNotFound) {
				return err
			}
			a.stamp(&next, fact.OperationID, fact.InputDigest)
			_, err = a.persist(ctx, next, pg.Command{ID: fact.OperationID, Kind: "transport_outcome", ExpectedRevision: a.account.Revision}, pg.Change{ConsumeObservationThrough: observation.Sequence})
			return err
		}
		return a.unresolved(ctx, observation, Fact{ID: observation.ID, IntentID: fact.IntentID, Reason: "unexpected_transport_order"})
	}
	next, err := cloneState(a.state)
	if err != nil {
		return err
	}
	outcome := fact.Outcome
	if outcome.Handoff == RejectedBeforeHandoff && !outcome.KnownFinal {
		return a.unresolved(ctx, observation, Fact{ID: observation.ID, Reason: "invalid_transport_finality"})
	}
	resolution := pg.Resolution{IntentID: fact.IntentID, VenueOrderID: outcome.VenueOrderID, Evidence: observation.Payload}
	if outcome.KnownFinal {
		resolution.State = "final"
		resolution.KnownFinal = true
		next.Child = nil
	} else if outcome.Accepted {
		resolution.State = "acknowledged"
		// A control may have committed between this observation's fill prefix and
		// trailer. Its older submission acknowledgement cannot undo cancellation.
		if next.Child.State != "cancel_requested" {
			next.Child.State = "open"
			next.Child.Order.State = "open"
		}
		next.Child.Order.QueueAhead = outcome.QueueAhead
	} else {
		resolution.State = "unknown"
		if next.Child.State != "cancel_requested" {
			next.Child.State = "unknown"
		}
		next.Child.Order.State = "cancel_requested"
		next.RecoveryRequired = true
		next.Reason = "transport_outcome_unknown"
	}
	change := pg.Change{ConsumeObservationThrough: observation.Sequence, Resolutions: []pg.Resolution{resolution}}
	// A full fill may already have finalized the outbox in its own atomic fill
	// transition; do not attempt a second final resolution during cursor commit.
	for _, intent := range a.account.Intents {
		if intent.ID == fact.IntentID && intent.State == "final" {
			change.Resolutions = nil
		}
	}
	if outcome.KnownFinal && a.state.Child.Order.Remaining.IsZero() {
		change.Resolutions = nil
	}
	if next.Parent.Revision != a.state.Parent.Revision {
		change.Plan = parentRow(next.Parent)
	}
	if next.RecoveryRequired {
		ready := false
		change.Ready = &ready
	}
	a.stamp(&next, fact.OperationID, fact.InputDigest)
	_, err = a.persist(ctx, next, pg.Command{ID: fact.OperationID, Kind: "transport_outcome", ExpectedRevision: a.account.Revision}, change)
	if err == nil && outcome.Accepted && !outcome.KnownFinal {
		faultpoint.Hit("resting_ack_committed")
	}
	return err
}

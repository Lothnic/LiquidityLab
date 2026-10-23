package runtime

import (
	"context"
	"fmt"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func parentRow(p *Parent) *pg.Plan {
	if p == nil {
		return nil
	}
	return &pg.Plan{ID: p.ID, Revision: p.Revision, Status: p.Status, Deadline: p.Deadline, Detail: jsonBytes(p)}
}
func (a *Actor) elapsed(state State) int64 {
	if state.Parent == nil || !state.Parent.Started {
		return 0
	}
	now := a.now()
	if a.elapsedPlan != state.Parent.ID {
		a.elapsedPlan = state.Parent.ID
		a.elapsedAt = now
		a.elapsedBase = max(0, now.Sub(state.Parent.Start).Nanoseconds())
	}
	return max(0, a.elapsedBase+now.Sub(a.elapsedAt).Nanoseconds())
}
func (a *Actor) command(ctx context.Context, c Command) (pg.Result, error) {
	if c.ID == "" || len(c.ID) > 128 || c.ExpectedRevision < 0 {
		return pg.Result{}, ErrInvalid
	}
	digest := inputDigest(c)
	if result, exists, err := a.duplicate(ctx, c.ID, digest); exists || err != nil {
		return result, err
	}
	if c.ExpectedRevision != a.account.Revision {
		return pg.Result{}, pg.ErrStaleRevision
	}
	if !a.account.LeaseUntil.After(a.now()) {
		a.state.RecoveryRequired = true
		a.state.Reason = "ownership_lease_lost"
		a.account.Ready = false
		return pg.Result{}, pg.ErrOwnership
	}
	if c.Kind == "dispatch" {
		return a.dispatch(ctx, c, digest)
	}
	next, err := cloneState(a.state)
	if err != nil {
		return pg.Result{}, err
	}
	a.stamp(&next, c.ID, digest)
	change := pg.Change{}
	cancelAfter := false
	switch c.Kind {
	case "create":
		if c.Plan == nil || c.PlanID == "" || len(c.PlanID) > 128 || next.Parent != nil && (next.Parent.Status == "active" || next.Parent.Status == "paused" || next.Parent.Status == "cancel_requested") || next.Child != nil {
			return pg.Result{}, ErrInvalid
		}
		if err := c.Plan.Validate(); err != nil {
			return pg.Result{}, err
		}
		if c.Plan.Symbol != next.Rules.Symbol {
			return pg.Result{}, ErrInvalid
		}
		deadline := c.Deadline
		if deadline.IsZero() {
			deadline = a.now().Add(time.Duration(c.Plan.Deadline))
		}
		deadline = deadline.UTC().Truncate(time.Microsecond)
		if !deadline.After(a.now()) || deadline.Sub(a.now()) > 24*time.Hour {
			return pg.Result{}, ErrInvalid
		}
		p := *c.Plan
		for _, balance := range a.account.Balances {
			if balance.Currency == next.BaseCurrency && p.BaseBalance.Cmp(balance.Free) > 0 {
				return pg.Result{}, pg.ErrInsufficientFunds
			}
			if balance.Currency == next.QuoteCurrency && p.QuoteBudget.Cmp(balance.Free) > 0 {
				return pg.Result{}, pg.ErrInsufficientFunds
			}
		}
		next.Parent = &Parent{ID: c.PlanID, Revision: 1, Status: "paused", Deadline: deadline, Spec: p}
		next.Fills = nil
		next.HasMessage = false
		next.LastMessage = 0
		next.TradeFlow = d.Zero
		next.DecisionMids = nil
		next.ReferenceMid = d.Zero
		change.Plan = parentRow(next.Parent)
	case "start", "resume":
		if next.Parent == nil || next.Parent.Status != "paused" || next.Child != nil || !a.account.Ready || !next.Book.Valid || next.RecoveryRequired || a.overflow.Load() {
			return pg.Result{}, ErrRecoveryRequired
		}
		if !next.Parent.Deadline.After(a.now()) {
			return pg.Result{}, pg.ErrExpired
		}
		if c.Kind == "start" && next.Parent.Started || c.Kind == "resume" && !next.Parent.Started {
			return pg.Result{}, ErrInvalid
		}
		if !next.Parent.Started {
			next.Parent.Start = a.now().UTC()
			next.Parent.Started = true
			next.Parent.Spec.Deadline = next.Parent.Deadline.Sub(next.Parent.Start).Nanoseconds()
		}
		next.Parent.Status = "active"
		next.Parent.Revision++
		change.Plan = parentRow(next.Parent)
	case "pause", "cancel", "deadline":
		if next.Parent == nil || next.Parent.Status == "completed" || next.Parent.Status == "cancelled" || next.Parent.Status == "expired" {
			return pg.Result{}, ErrInvalid
		}
		if c.Kind == "deadline" && a.now().Before(next.Parent.Deadline) {
			return pg.Result{}, ErrInvalid
		}
		if next.Child != nil && next.Child.State == "queued" {
			change.ObsoleteQueued = []string{next.Child.ID}
			next.Child = nil
		}
		next.Parent.Status = "paused"
		if c.Kind != "pause" {
			next.Parent.Status = "cancel_requested"
			if next.Child == nil {
				next.Parent.Status = "cancelled"
				if c.Kind == "deadline" {
					next.Parent.Status = "expired"
				}
			}
		}
		next.Parent.Revision++
		change.Plan = parentRow(next.Parent)
		cancelAfter = next.Child != nil
	case "action":
		if next.Parent == nil || next.Parent.Status != "active" || !next.Parent.Started || !a.account.Ready || !next.Book.Valid || next.RecoveryRequired || a.overflow.Load() {
			return pg.Result{}, ErrRecoveryRequired
		}
		ledger, err := a.ledger(next)
		if err != nil {
			return pg.Result{}, err
		}
		var child *d.ChildOrder
		if next.Child != nil {
			child = &next.Child.Order
		}
		compiled, err := x.Compile(c.Action, next.Parent.Spec, next.Rules, next.Fees, next.Execution, ledger, next.Book, child, a.elapsed(next))
		if err != nil {
			return pg.Result{}, err
		}
		if (compiled.Cancel || compiled.Intent != nil) && next.HasMessage && a.elapsed(next)-next.LastMessage < next.Execution.MinMessageInterval {
			next.Reason = "message_budget"
			break
		}
		next.Reason = compiled.Reason
		if compiled.Cancel {
			if next.Child.State == "queued" {
				change.ObsoleteQueued = []string{next.Child.ID}
				next.Child = nil
			} else {
				cancelAfter = true
			}
			next.LastMessage = a.elapsed(next)
			next.HasMessage = true
		}
		if compiled.Intent != nil {
			kind, reservation, err := x.ReservationFor(*compiled.Intent, next.Fees)
			if err != nil {
				return pg.Result{}, err
			}
			currency := next.QuoteCurrency
			if kind == "base" {
				currency = next.BaseCurrency
			}
			id := randomID("child-")
			next.Child = &Child{ID: id, Action: c.Action, State: "queued", Order: d.ChildOrder{OrderID: id, Intent: *compiled.Intent, SubmittedAt: a.elapsed(next), ArrivalAt: a.elapsed(next), Remaining: compiled.Intent.Quantity, State: "submitting"}}
			change.Enqueue = []pg.Intent{{ID: id, PlanID: next.Parent.ID, PlanRevision: next.Parent.Revision, ExpiresAt: next.Parent.Deadline, InstrumentRevision: next.InstrumentRevision, Payload: jsonBytes(compiled.Intent)}}
			change.Reservations = []pg.Reservation{{IntentID: id, Currency: currency, Amount: reservation}}
			next.LastMessage = a.elapsed(next)
			next.HasMessage = true
		}
	default:
		return pg.Result{}, ErrInvalid
	}
	result, err := a.persist(ctx, next, pg.Command{ID: c.ID, Kind: c.Kind, ExpectedRevision: c.ExpectedRevision}, change)
	if err != nil {
		return result, err
	}
	if cancelAfter {
		if err = a.cancelChild(ctx); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (a *Actor) dispatch(ctx context.Context, c Command, digest string) (pg.Result, error) {
	if a.state.Child == nil || a.state.Child.State != "queued" || c.IntentID != a.state.Child.ID || a.state.Parent == nil || a.state.Parent.Status != "active" || a.state.RecoveryRequired || !a.account.Ready || !a.state.Book.Valid || a.overflow.Load() {
		return pg.Result{}, ErrRecoveryRequired
	}
	ledger, err := a.ledger(a.state)
	if err != nil {
		return pg.Result{}, err
	}
	compiled, err := x.Compile(a.state.Child.Action, a.state.Parent.Spec, a.state.Rules, a.state.Fees, a.state.Execution, ledger, a.state.Book, nil, a.elapsed(a.state))
	if err != nil {
		return pg.Result{}, err
	}
	if compiled.Intent == nil || !compiled.Intent.Equal(a.state.Child.Order.Intent) {
		next, err := cloneState(a.state)
		if err != nil {
			return pg.Result{}, err
		}
		id := next.Child.ID
		next.Child = nil
		next.Reason = "queued_intent_obsolete_after_recompile"
		a.stamp(&next, c.ID, digest)
		return a.persist(ctx, next, pg.Command{ID: c.ID, Kind: c.Kind, ExpectedRevision: c.ExpectedRevision}, pg.Change{ObsoleteQueued: []string{id}})
	}
	// Persist the latest market identity without changing the queued intent. The
	// actor loop remains its sole owner through authorization and physical handoff.
	if a.account.Market.SnapshotID != a.state.MarketID {
		next, err := cloneState(a.state)
		if err != nil {
			return pg.Result{}, err
		}
		if _, err = a.persist(ctx, next, pg.Command{ID: randomID("dispatch-market-"), Kind: "dispatch_market", ExpectedRevision: a.account.Revision}, pg.Change{}); err != nil {
			return pg.Result{}, err
		}
	}
	child := a.state.Child
	authorized, err := a.config.Store.AuthorizeDispatch(ctx, pg.Authorization{Key: a.config.Key, Ownership: a.owner, IntentID: child.ID, ExpectedRevision: a.account.Revision, PlanRevision: a.state.Parent.Revision, SnapshotID: a.state.MarketID, InstrumentRevision: a.state.InstrumentRevision, AttemptID: randomID("attempt-")})
	if err != nil {
		if account, readErr := a.config.Store.GetAccount(ctx, a.config.Key); readErr == nil {
			a.account = account
		}
		return pg.Result{}, err
	}
	a.account.Revision = authorized.Revision
	child.State = "attempted"
	a.state.Child = child
	// No database transaction is held here. Any pause/cancel waits behind this
	// bounded handoff; the transport must honor the context deadline.
	budget := min(time.Second, time.Until(a.state.Parent.Deadline), time.Until(a.account.LeaseUntil))
	if budget <= 0 {
		return a.acceptOutcome(ctx, TransportResult{Handoff: RejectedBeforeHandoff, KnownFinal: true, Reason: "expired_before_handoff"}, c.ID, digest)
	}
	handoffCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	bookState, err := cloneState(a.state)
	if err != nil {
		return pg.Result{}, err
	}
	outcome, transportErr := a.transport.Handoff(handoffCtx, Submission{ID: child.ID, Intent: child.Order.Intent, Book: bookState.Book, Fees: a.state.Fees, Now: a.elapsed(a.state), OperationID: c.ID, InputDigest: digest})
	if transportErr != nil {
		a.state.RecoveryRequired = true
		a.state.Reason = "transport_outcome_unknown"
		a.account.Ready = false
		return pg.Result{}, fmt.Errorf("%w: %v", ErrOutcomeUnknown, transportErr)
	}
	_ = outcome // authoritative outcome is the durable source observation.
	if err := a.drainFacts(ctx); err != nil {
		return pg.Result{}, err
	}
	record, err := a.config.Store.LookupCommand(ctx, a.config.Key, c.ID)
	return record.Result, err
}
func (a *Actor) cancelChild(ctx context.Context) error {
	if a.state.Child == nil {
		return nil
	}
	if a.state.Child.State == "queued" {
		return ErrInvalid
	}
	cancelCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err := a.transport.Cancel(cancelCtx, a.state.Child.ID)
	if err != nil {
		a.state.RecoveryRequired = true
		a.state.Reason = "cancel_outcome_unknown"
		a.account.Ready = false
		return err
	}
	return a.drainFacts(ctx)
}

// acceptOutcome is used only for a proven pre-handoff rejection. Normal paper
// source outcomes are already in the durable observation inbox before return.
func (a *Actor) acceptOutcome(ctx context.Context, outcome TransportResult, id, digest string) (pg.Result, error) {
	if a.state.Child == nil {
		return pg.Result{}, ErrInvalid
	}
	fact := transportFact{IntentID: a.state.Child.ID, OperationID: id, InputDigest: digest, Outcome: outcome}
	if _, err := a.config.Store.RecordObservation(ctx, a.config.Key, pg.Observation{ID: "transport-" + id, Kind: paperTransportKind, Payload: jsonBytes(fact)}); err != nil {
		return pg.Result{}, err
	}
	if err := a.drainFacts(ctx); err != nil {
		return pg.Result{}, err
	}
	record, err := a.config.Store.LookupCommand(ctx, a.config.Key, id)
	return record.Result, err
}
func (a *Actor) reconcile(ctx context.Context, r Reconciliation) (pg.Result, error) {
	if r.ID == "" || r.Evidence == "" || len(r.Evidence) > 128 || !r.NoExternalOrders {
		return pg.Result{}, ErrInvalid
	}
	digest := inputDigest(r)
	if result, exists, err := a.duplicate(ctx, r.ID, digest); exists || err != nil {
		return result, err
	}
	if r.ExpectedRevision != a.account.Revision {
		return pg.Result{}, pg.ErrStaleRevision
	}
	if !a.state.Book.Valid {
		return pg.Result{}, ErrRecoveryRequired
	}
	next, err := cloneState(a.state)
	if err != nil {
		return pg.Result{}, err
	}
	resolved := map[string]bool{}
	for _, id := range r.ResolvedFactIDs {
		resolved[id] = true
	}
	for i := range next.UnresolvedFacts {
		if resolved[next.UnresolvedFacts[i].ID] {
			next.UnresolvedFacts[i].Resolved = true
		}
		if !next.UnresolvedFacts[i].Resolved {
			return pg.Result{}, ErrRecoveryRequired
		}
	}
	// Reconciled balances alone cannot repair a parent execution ledger that
	// rejected a contradictory fill. End that obligation before another send.
	if (len(next.UnresolvedFacts) > 0 || next.ClockDiscontinuity) && next.Parent != nil && (next.Parent.Status == "active" || next.Parent.Status == "paused" || next.Parent.Status == "cancel_requested") && !r.AbortParent {
		return pg.Result{}, ErrRecoveryRequired
	}
	if next.Child != nil {
		final := false
		for _, resolution := range r.Resolutions {
			if resolution.IntentID == next.Child.ID && resolution.State == "final" && resolution.KnownFinal {
				final = true
			}
		}
		if !final {
			return pg.Result{}, ErrRecoveryRequired
		}
		next.Child = nil
	}
	next.RecoveryRequired = false
	next.ClockDiscontinuity = false
	next.Reason = "paper_reconciliation_asserted"
	a.stamp(&next, r.ID, digest)
	ready := true
	change := pg.Change{Ready: &ready, Balances: r.Balances, Resolutions: r.Resolutions}
	if r.AbortParent && next.Parent != nil && (next.Parent.Status == "active" || next.Parent.Status == "paused" || next.Parent.Status == "cancel_requested") {
		next.Parent.Status = "cancelled"
		next.Parent.Revision++
		change.Plan = parentRow(next.Parent)
		next.Reason = "reconciled_parent_aborted"
	}
	result, err := a.persist(ctx, next, pg.Command{ID: r.ID, Kind: "reconcile", ExpectedRevision: r.ExpectedRevision}, change)
	if err == nil {
		a.overflow.Store(false)
	}
	return result, err
}

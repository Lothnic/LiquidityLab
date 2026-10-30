package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
	"github.com/liquiditylab/liquiditylab/internal/telemetry"
)

const (
	operationPrimary = iota
	operationAwaitFacts
	operationAwaitCommand
)

// One ordinary request retains its place while yielding to controls/maintenance.
// Later market inputs stay in the bounded FIFO; they cannot overtake it.
type actorOperation struct {
	request        request
	activeElapsed  time.Duration
	stage          int
	target         int64
	commandID      string
	marketPrepared bool
}

func (a *Actor) loop() {
	defer close(a.done)
	defer func() {
		if a.policyCancel != nil {
			a.policyCancel()
		}
	}()
	interval := min(a.config.Lease/3, time.Second)
	if a.config.AutoExecute {
		interval = min(interval, time.Duration(a.config.Execution.DecisionInterval))
	}
	interval = max(interval, time.Millisecond)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	if a.config.Policy != nil {
		a.policyTimer = time.NewTimer(time.Hour)
		a.policyTimer.Stop()
		defer a.policyTimer.Stop()
	}
	stopping := false
	for {
		// At most one control and one reduction quantum per cycle: continuous
		// control ingress cannot starve accepted facts, nor can a backlog hide controls.
		select {
		case <-ticker.C:
			a.maintenance()
		default:
		}
		select {
		case r := <-a.control:
			a.handle(r)
		default:
		}
		select {
		case r := <-a.views:
			a.handle(r)
		default:
		}
		select {
		case <-a.policyExpiry:
			a.handlePolicyExpiry()
		default:
		}
		a.progressPolicyResult()
		select {
		case result := <-a.policyResults:
			a.receivePolicy(result)
		default:
		}
		policyFactRevision, policyOrdinaryPending := a.account.Revision, a.pending
		a.progressCancellation()
		if a.pending != nil {
			a.handleOperation(a.pending)
		} else {
			select {
			case <-a.factWake:
				a.factTurn()
			default:
			}
		}
		a.finishControl()
		// A returned proposal gets its next eligibility check only after the
		// next cycle's controls and expiry. The wake also bounds continuation
		// without taking another fact quantum inside policy handling.
		if a.policyDeferred != nil && !a.factBlocked && !a.needsReload && (a.account.Revision != policyFactRevision || policyOrdinaryPending != nil && a.pending == nil) {
			a.wakeFacts()
		}
		if !stopping {
			select {
			case <-a.stop:
				stopping = true
			default:
			}
		}
		if stopping && a.pending == nil && len(a.control)+len(a.work)+len(a.facts)+len(a.views) == 0 && (a.factBlocked || a.account.ObservedSequence == a.account.ProcessedSequence) {
			return
		}
		if a.pending != nil {
			// A blocked quantum is reported to this waiter on its next turn;
			// continuation never depends on a new external notification.
			continue
		}
		stop := a.stop
		if stopping {
			stop = nil
		}
		select {
		case r := <-a.control:
			a.handle(r)
		case r := <-a.views:
			a.handle(r)
		case r := <-a.facts:
			a.handle(r)
		case r := <-a.work:
			a.handle(r)
		case <-a.factWake:
			a.factTurn()
		case result := <-a.policyResults:
			a.receivePolicy(result)
		case <-a.policyExpiry:
			a.handlePolicyExpiry()
		case <-ticker.C:
			a.maintenance()
		case <-stop:
			stopping = true
		}
	}
}

func (a *Actor) factTurn() {
	if a.factBlocked {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer func() {
		if recover() != nil {
			a.state.RecoveryRequired, a.account.Ready, a.factBlocked, a.needsReload = true, false, true, true
		}
	}()
	progress, err := a.reduceFactsQuantum(ctx, a.factBudget())
	if err == nil && progress.More {
		a.wakeFacts()
	}
}

func (a *Actor) handle(req request) {
	if !req.enqueuedAt.IsZero() {
		a.config.Telemetry.Adjust(telemetry.ActorQueued, -1)
		a.observeElapsed(telemetry.ActorQueueWaitNS, req.enqueuedAt)
	}
	a.handleOperation(&actorOperation{request: req})
}

func (a *Actor) handleOperation(op *actorOperation) {
	req := op.request
	result := response{}
	turnStarted := a.metricStart()
	a.config.Telemetry.Adjust(telemetry.ActorHandling, 1)
	previous := a.currentOperation
	a.currentOperation = op
	defer func() {
		a.currentOperation = previous
		a.config.Telemetry.Adjust(telemetry.ActorHandling, -1)
		if !turnStarted.IsZero() {
			op.activeElapsed += max(0, time.Since(turnStarted))
			a.observeElapsed(telemetry.ActorHandleNS, turnStarted)
		}
		if panicValue := recover(); panicValue != nil {
			a.state.RecoveryRequired, a.account.Ready, a.factBlocked, a.needsReload = true, false, true, true
			result.err = fmt.Errorf("%w: runtime panic contained", ErrRecoveryRequired)
		}
		if errors.Is(result.err, errFactsPending) {
			a.pending = op
			return
		}
		if a.pending == op {
			a.pending = nil
		}
		if result.err == nil {
			a.config.Telemetry.Add(telemetry.ActorCompleted, 1)
		} else {
			a.config.Telemetry.Add(telemetry.ActorFailed, 1)
		}
		if req.kind == "market" {
			a.config.Telemetry.Add(telemetry.MarketEvents, 1)
			if a.config.Telemetry != nil {
				a.config.Telemetry.Observe(telemetry.PublicMarketApplyNS, uint64(op.activeElapsed.Nanoseconds()))
			}
			if result.err != nil {
				a.config.Telemetry.Add(telemetry.MarketErrors, 1)
			}
		}
		req.reply <- result
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if a.needsReload {
		if result.err = a.refreshOwned(ctx); result.err != nil {
			return
		}
	}
	control := req.kind == "command" && isControl(req.input.(Command).Kind)
	if err := a.checkClock(ctx); err != nil && req.kind != "view" && req.kind != "market" && req.kind != "reconcile" && !control {
		result.err = err
		return
	}
	if op.stage == operationAwaitCommand {
		if a.factBlocked {
			result.err = ErrRecoveryRequired
			return
		}
		_, result.err = a.reduceFactsQuantum(ctx, a.factBudget())
		if result.err != nil {
			return
		}
		record, err := a.config.Store.LookupCommand(ctx, a.config.Key, op.commandID)
		if err == nil {
			result.result = record.Result
			return
		}
		if errors.Is(err, pg.ErrNotFound) && a.account.ObservedSequence > a.account.ProcessedSequence {
			result.err = errFactsPending
			return
		}
		result.err = err
		return
	}
	if op.stage == operationAwaitFacts || req.kind == "facts" {
		if result.err = a.awaitFacts(ctx, op); result.err != nil {
			return
		}
		return
	}
	if req.kind != "view" && req.kind != "market" && req.kind != "reconcile" && !control {
		if result.err = a.awaitFacts(ctx, op); result.err != nil {
			return
		}
	}
	switch req.kind {
	case "command":
		result.result, result.err = a.command(ctx, req.input.(Command))
	case "market":
		result.err = a.market(ctx, req.input.(marketInput))
	case "recover_paper":
		result.err = a.recoverPaper(ctx)
		if errors.Is(result.err, errFactsPending) {
			op.target = 0
		}
	case "reconcile":
		result.result, result.err = a.reconcile(ctx, req.input.(Reconciliation))
		if result.err == nil {
			a.factBlocked = false
			a.factCursor = nil
			a.wakeFacts()
		}
	case "view":
		state, err := cloneState(a.state)
		if err != nil {
			result.err = err
			return
		}
		current, err := a.config.Store.GetAccount(ctx, a.config.Key)
		if err != nil {
			result.err = err
			return
		}
		if current.Ownership != a.owner {
			result.err = pg.ErrOwnership
			return
		}
		if !current.LeaseUntil.After(a.now()) || current.ObservedSequence != current.ProcessedSequence {
			current.Ready = false
		}
		var account pg.Account
		if result.err = decodeInternal(jsonBytes(current), &account); result.err != nil {
			return
		}
		result.view = View{Account: account, State: state}
	default:
		result.err = ErrInvalid
	}
}

func isControl(kind string) bool { return kind == "pause" || kind == "cancel" || kind == "deadline" }

func (a *Actor) awaitFacts(ctx context.Context, op *actorOperation) error {
	if a.factBlocked {
		return ErrRecoveryRequired
	}
	if op.target == 0 {
		if err := a.refreshOwned(ctx); err != nil {
			return err
		}
		op.target = a.account.ObservedSequence
		if sequence, ok := op.request.input.(int64); op.request.kind == "facts" && ok {
			op.target = sequence
		}
	}
	if a.account.ProcessedSequence >= op.target {
		return nil
	}
	if _, err := a.reduceFactsThrough(ctx, a.factBudget(), op.target); err != nil {
		return err
	}
	if a.account.ProcessedSequence < op.target {
		return errFactsPending
	}
	if a.account.ObservedSequence > a.account.ProcessedSequence {
		a.wakeFacts()
	}
	return nil
}

func (a *Actor) waitCommand(id string) error {
	if op := a.currentOperation; op != nil {
		op.stage, op.commandID = operationAwaitCommand, id
	}
	a.wakeFacts()
	return errFactsPending
}

func (a *Actor) progressCancellation() {
	child := a.state.Child
	if child == nil || (child.State != "queued" && child.State != "cancel_requested") || a.needsReload || a.factTime().Before(a.cancelRetryAt) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// The store may have kept a queued hold through the last source trailer.
	// An unsent order has no venue cancellation outcome: dispose it atomically
	// only once all accepted facts have been reduced.
	intent, err := a.config.Store.GetIntent(ctx, a.config.Key, child.ID)
	if err != nil {
		return
	}
	if intent.State == "queued" || intent.State == "obsolete" {
		invalid := child.State == "cancel_requested" || a.state.Parent == nil || a.state.Parent.Status != "active" || a.state.Parent.Revision != intent.PlanRevision || !a.account.Ready || !a.state.Book.Valid || a.state.RecoveryRequired || a.overflow.Load()
		if !invalid || a.account.ObservedSequence != a.account.ProcessedSequence {
			return
		}
		next, err := cloneState(a.state)
		if err != nil {
			return
		}
		next.Child = nil
		id := fmt.Sprintf("runtime-queued-final-%d", a.account.Revision)
		a.stamp(&next, id, "unsent-caught-up")
		change := pg.Change{RequireCaughtUp: true}
		if intent.State == "queued" {
			change.ObsoleteQueued = []string{child.ID}
		}
		_, _ = a.persist(ctx, next, pg.Command{ID: id, Kind: "queued_final", ExpectedRevision: a.account.Revision}, change)
		return
	}
	if child.State != "cancel_requested" || a.cancelRecorded == child.ID {
		return
	}
	if a.cancelChild(ctx) == nil {
		a.cancelRecorded = child.ID
	} else {
		a.cancelRetryAt = a.factTime().Add(100 * time.Millisecond)
	}
}

// A local cancellation receipt never implies finality while accepted source debt
// or child/hold obligations remain. The caught-up guard also covers append races.
func (a *Actor) finishControl() {
	p := a.state.Parent
	if a.needsReload || a.factBlocked || p == nil || (p.Status != "active" && p.Status != "paused" && p.Status != "cancel_requested") || a.state.Child != nil || len(a.state.PendingFacts) != 0 || len(a.account.Intents) != 0 || len(a.account.Reservations) != 0 || a.account.ObservedSequence != a.account.ProcessedSequence {
		return
	}
	for _, fact := range a.state.UnresolvedFacts {
		if !fact.Resolved {
			return
		}
	}
	status := ""
	ledger, err := a.ledger(a.state)
	if err != nil {
		return
	}
	if ledger.Remaining().IsZero() {
		status = "completed"
	} else if p.Status == "cancel_requested" {
		status = "cancelled"
		if !a.now().Before(p.Deadline) {
			status = "expired"
		}
	}
	if status == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	next, err := cloneState(a.state)
	if err != nil {
		return
	}
	next.Parent.Status = status
	next.Parent.Revision++
	id := fmt.Sprintf("runtime-control-final-%s-%d", p.ID, p.Revision)
	a.stamp(&next, id, "known-final-caught-up")
	_, _ = a.persist(ctx, next, pg.Command{ID: id, Kind: "control_final", ExpectedRevision: a.account.Revision}, pg.Change{RequireCaughtUp: true, Plan: parentRow(next.Parent)})
}

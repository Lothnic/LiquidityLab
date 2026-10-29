package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

var errFactsPending = errors.New("accepted source facts await a later actor turn")

type factBudget struct {
	MaxSteps   int
	MaxElapsed time.Duration
}

type factProgress struct {
	ProcessedSequence int64
	Steps             int
	More              bool
}

// Only the position within an immutable observation is process local. Every
// completed fill has its own durable identity; the observation cursor advances
// only with its trailer, so losing this cache merely rechecks an applied prefix.
type factCursor struct {
	observation pg.Observation
	trade       *paperTradeFact
	transport   *transportFact
	fill        *Fact
	invalid     *Fact
	nextFill    int
}

func (a *Actor) factBudget() factBudget {
	steps, elapsed := a.config.FactQuantumSteps, a.config.FactQuantumTime
	if steps == 0 {
		steps = 4
	}
	if elapsed == 0 {
		elapsed = 5 * time.Millisecond
	}
	return factBudget{MaxSteps: steps, MaxElapsed: elapsed}
}

func (a *Actor) factTime() time.Time {
	if a.factClock != nil {
		return a.factClock()
	}
	return time.Now()
}

func (a *Actor) wakeFacts() {
	if a.factWake == nil {
		return
	}
	select {
	case a.factWake <- struct{}{}:
	default:
	}
}

func (a *Actor) refreshOwned(ctx context.Context) error {
	quarantinedSource := false
	account, err := a.config.Store.GetAccount(ctx, a.config.Key)
	if err != nil {
		return err
	}
	if account.Ownership != a.owner {
		return pg.ErrOwnership
	}
	if a.needsReload {
		prior := a.state
		state, err := DecodeSnapshot(account.Snapshot)
		if err != nil {
			return err
		}
		if err := validateSourceState(state); err != nil {
			return err
		}
		if !reflect.DeepEqual(state.SealedSource, a.config.SealedSource) {
			return ErrInvalid
		}
		if restoreSourceAfterReload(&state, prior) {
			account.Ready = false
			quarantinedSource = true
		}
		a.state = state
		a.factCursor = nil
		a.needsReload = false
		// A trade request can be parked across a control's uncertain commit.
		// Its preparation only changed the market identity (trade Apply is a
		// book no-op); preserve that accepted FIFO position after reloading all
		// financial state from the durable snapshot.
		if op := a.pending; op != nil && op.marketPrepared && op.request.kind == "market" {
			input := op.request.input.(marketInput)
			a.state.MarketID = input.SnapshotID
			acceptSourceReceipt(&a.state, input)
		}
	}
	a.account = account
	if quarantinedSource {
		return a.invalidateMarket(ctx, "sealed_source_snapshot_required_after_reload")
	}
	return nil
}

func (a *Actor) loadFactCursor(ctx context.Context) error {
	observations, err := a.config.Store.PendingObservations(ctx, a.config.Key, 1)
	if err != nil {
		return err
	}
	if len(observations) != 1 || observations[0].Sequence != a.account.ProcessedSequence+1 {
		return ErrRecoveryRequired
	}
	observation := observations[0]
	cursor := &factCursor{observation: observation}
	switch observation.Kind {
	case paperTradeKind:
		var fact paperTradeFact
		if err := decodeInternal(observation.Payload, &fact); err != nil || fact.ID != observation.ID || len(fact.Fills) > 1 {
			cursor.invalid = &Fact{ID: observation.ID, Reason: "malformed_paper_trade_fact"}
		} else {
			cursor.trade = &fact
		}
	case "fill":
		var fact Fact
		if err := decodeInternal(observation.Payload, &fact); err != nil {
			cursor.invalid = &Fact{ID: observation.ID, Reason: "malformed_fill_fact"}
		} else {
			cursor.fill = &fact
		}
	case paperTransportKind:
		var fact transportFact
		if err := decodeInternal(observation.Payload, &fact); err != nil || len(fact.Outcome.Fills) > 512 || fact.OperationID == "" || fact.InputDigest == "" {
			cursor.invalid = &Fact{ID: observation.ID, IntentID: fact.IntentID, Reason: "invalid_transport_fact"}
		} else {
			cursor.transport = &fact
		}
	default:
		cursor.invalid = &Fact{ID: observation.ID, Reason: "unsupported_account_fact"}
	}
	a.factCursor = cursor
	return nil
}

func (a *Actor) reduceFactsQuantum(ctx context.Context, budget factBudget) (progress factProgress, err error) {
	return a.reduceFactsThrough(ctx, budget, 0)
}

func (a *Actor) reduceFactsThrough(ctx context.Context, budget factBudget, target int64) (progress factProgress, err error) {
	if budget.MaxSteps < 1 || budget.MaxElapsed <= 0 {
		return progress, ErrInvalid
	}
	defer func() {
		progress.ProcessedSequence = a.account.ProcessedSequence
		progress.More = a.account.ObservedSequence > a.account.ProcessedSequence
		if errors.Is(err, ErrRecoveryRequired) || errors.Is(err, ErrHistoryLimit) {
			a.factBlocked = true
		}
	}()
	if err = a.refreshOwned(ctx); err != nil {
		return progress, err
	}
	started := a.factTime()
	for a.account.ProcessedSequence < a.account.ObservedSequence {
		if target > 0 && a.account.ProcessedSequence >= target {
			break
		}
		if progress.Steps >= budget.MaxSteps || progress.Steps > 0 && a.factTime().Sub(started) >= budget.MaxElapsed {
			break
		}
		if err = ctx.Err(); err != nil {
			return progress, err
		}
		if a.factCursor == nil || a.factCursor.observation.Sequence <= a.account.ProcessedSequence {
			if err = a.loadFactCursor(ctx); err != nil {
				return progress, err
			}
		}
		cursor := a.factCursor
		observation := cursor.observation
		if observation.Sequence != a.account.ProcessedSequence+1 {
			return progress, ErrRecoveryRequired
		}
		progress.Steps++ // Duplicate/resolved substeps also consume the quantum.
		switch {
		case cursor.invalid != nil:
			err = a.unresolved(ctx, observation, *cursor.invalid)
		case cursor.fill != nil:
			err = a.applyFill(ctx, observation, *cursor.fill, 0, true)
		case cursor.trade != nil:
			fact := cursor.trade
			if cursor.nextFill < len(fact.Fills) {
				i := cursor.nextFill
				err = a.applyFill(ctx, observation, Fact{ID: fmt.Sprintf("%s/fill/%d", fact.ID, i), IntentID: fact.IntentID, Fill: fact.Fills[i]}, i, false)
				if err == nil {
					cursor.nextFill++
				}
			} else {
				err = a.applyPaperTrade(ctx, observation, *fact)
			}
		case cursor.transport != nil:
			fact := cursor.transport
			if cursor.nextFill < len(fact.Outcome.Fills) {
				i := cursor.nextFill
				err = a.applyFill(ctx, observation, Fact{ID: fmt.Sprintf("%s/fill/%d", observation.ID, i), IntentID: fact.IntentID, Fill: fact.Outcome.Fills[i]}, i, false)
				if err == nil {
					cursor.nextFill++
				}
			} else {
				err = a.applyTransport(ctx, observation, *fact)
			}
		}
		if err != nil {
			return progress, err
		}
		if a.account.ProcessedSequence >= observation.Sequence {
			a.factCursor = nil
		}
	}
	return progress, nil
}

package runtime

import (
	"context"

	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

func (a *Actor) recoverPaper(ctx context.Context) error {
	account, err := a.config.Store.GetAccount(ctx, a.config.Key)
	if err != nil {
		return err
	}
	if account.Ownership != a.owner {
		return pg.ErrOwnership
	}
	transport, ok := a.transport.(*paperTransport)
	if !ok || transport.owner != a.owner || account.PaperTransportContract != pg.PaperTransportContract || a.config.Profile != ProfilePaper {
		return ErrProfile
	}
	a.account = account
	if a.state.Parent != nil && a.state.Parent.Status == "active" {
		return ErrInvalid
	}
	// The ordinary source reducer already drained a fixed watermark in handle.
	// Quarantined facts and legacy pending payloads need explicit inspection.
	if len(a.state.PendingFacts) != 0 {
		return ErrRecoveryRequired
	}
	for _, fact := range a.state.UnresolvedFacts {
		if !fact.Resolved {
			return ErrRecoveryRequired
		}
	}
	if a.state.Child != nil {
		// Takeover obsoletes unsent intents. Remaining children were attempted or
		// acknowledged: persist cancellation, never submit them a second time.
		if err := a.cancelChild(ctx); err != nil {
			return err
		}
		return errFactsPending
	}
	account, err = a.config.Store.GetAccount(ctx, a.config.Key)
	if err != nil {
		return err
	}
	if account.Ownership != a.owner {
		return pg.ErrOwnership
	}
	a.account = account
	if a.state.Child != nil || len(account.Intents) != 0 || len(account.Reservations) != 0 || account.ObservedSequence != account.ProcessedSequence || !a.state.Book.Valid {
		return ErrRecoveryRequired
	}
	if account.Ready && !a.state.RecoveryRequired && !a.state.ClockDiscontinuity {
		return nil
	}
	// All simulated effects are now durable/reduced and the old epoch cannot
	// append any. NoExternalOrders follows from that paper contract; it is not
	// supplied by the daemon or asserted about an external account.
	_, err = a.reconcile(ctx, Reconciliation{
		ID: randomID("runtime-paper-recovery-"), ExpectedRevision: account.Revision,
		Evidence:         "postgres-paper-v1 owned effects drained; no pending intents or holds; fresh book; retained balances",
		NoExternalOrders: true, Balances: account.Balances,
	})
	return err
}

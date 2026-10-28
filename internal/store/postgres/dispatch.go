package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuthorizeDispatch is the durable transport handoff boundary. It commits an
// attempted state before returning the intent; the caller performs I/O afterward.
// A lost reply, process crash, or transport error is potentially in flight, never
// permission to call this method again and send the same intent a second time.
func (s *Store) AuthorizeDispatch(ctx context.Context, request Authorization) (Dispatch, error) {
	if !validKey(request.Key) || !validID(request.IntentID) || !validID(request.AttemptID) || !validID(request.SnapshotID) || !validID(request.InstrumentRevision) || request.ExpectedRevision < 0 || request.PlanRevision <= 0 {
		return Dispatch{}, ErrInvalid
	}
	tx, err := s.begin(ctx, pgx.TxOptions{})
	if err != nil {
		return Dispatch{}, err
	}
	defer tx.Rollback(ctx)
	a, err := lockAccount(ctx, tx, request.Key)
	if err != nil {
		return Dispatch{}, err
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return Dispatch{}, err
	}
	if err = checkOwner(a, request.Ownership, now); err != nil {
		return Dispatch{}, err
	}
	i := Intent{ID: request.IntentID}
	err = tx.QueryRow(ctx, `SELECT plan_id,plan_revision,expires_at,instrument_revision,payload,state,attempt_id,venue_order_id,authorization_revision,authorization_snapshot FROM outbox WHERE tenant_id=$1 AND account_id=$2 AND intent_id=$3`, request.Key.TenantID, request.Key.AccountID, request.IntentID).Scan(&i.PlanID, &i.PlanRevision, &i.ExpiresAt, &i.InstrumentRevision, &i.Payload, &i.State, &i.AttemptID, &i.VenueOrderID, &i.AuthorizationRevision, &i.AuthorizationSnapshot)
	if err != nil {
		return Dispatch{}, normalized(err)
	}
	if i.State != "queued" {
		return Dispatch{}, ErrConflict
	}
	var revision int64
	var status string
	var deadline time.Time
	err = tx.QueryRow(ctx, "SELECT revision,status,deadline FROM plans WHERE tenant_id=$1 AND account_id=$2 AND plan_id=$3", request.Key.TenantID, request.Key.AccountID, i.PlanID).Scan(&revision, &status, &deadline)
	if err != nil {
		return Dispatch{}, normalized(err)
	}
	if !deadline.After(now) || !i.ExpiresAt.After(now) {
		if err = obsoleteQueued(ctx, tx, request.Key, i.PlanID, "deadline or intent expired"); err != nil {
			return Dispatch{}, err
		}
		if _, err = tx.Exec(ctx, "UPDATE accounts SET revision=revision+1 WHERE tenant_id=$1 AND account_id=$2", request.Key.TenantID, request.Key.AccountID); err != nil {
			return Dispatch{}, err
		}
		if err = journal(ctx, tx, request.Key, a.Revision+1, "dispatch_expired", request); err != nil {
			return Dispatch{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return Dispatch{}, err
		}
		return Dispatch{}, ErrExpired
	}
	if !a.Ready || !a.Market.Valid || status != "active" || a.ObservedSequence != a.ProcessedSequence {
		return Dispatch{}, ErrNotReady
	}
	if a.Revision != request.ExpectedRevision || revision != request.PlanRevision || i.PlanRevision != revision || i.InstrumentRevision != a.Market.InstrumentRevision || request.InstrumentRevision != a.Market.InstrumentRevision || request.SnapshotID != a.Market.SnapshotID {
		return Dispatch{}, ErrStaleRevision
	}
	if err = checkFunds(ctx, tx, request.Key); err != nil {
		return Dispatch{}, err
	}
	var reserved bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM reservations WHERE tenant_id=$1 AND account_id=$2 AND intent_id=$3 AND state='held')", request.Key.TenantID, request.Key.AccountID, i.ID).Scan(&reserved); err != nil {
		return Dispatch{}, err
	}
	if !reserved {
		return Dispatch{}, ErrInsufficientFunds
	}
	if a.Revision == int64(^uint64(0)>>1) {
		return Dispatch{}, ErrInvalid
	}
	result, err := tx.Exec(ctx, `UPDATE outbox o SET state='attempted',attempt_id=$4,authorization_revision=$5,authorization_snapshot=$6,attempted_at=clock_timestamp()
 FROM accounts a,plans p WHERE o.tenant_id=$1 AND o.account_id=$2 AND o.intent_id=$3 AND o.state='queued'
 AND a.tenant_id=o.tenant_id AND a.account_id=o.account_id AND a.lease_until>clock_timestamp()
 AND p.tenant_id=o.tenant_id AND p.account_id=o.account_id AND p.plan_id=o.plan_id
 AND p.deadline>clock_timestamp() AND o.expires_at>clock_timestamp()`, request.Key.TenantID, request.Key.AccountID, i.ID, request.AttemptID, a.Revision+1, a.Market.SnapshotID)
	if err != nil {
		return Dispatch{}, normalized(err)
	}
	if result.RowsAffected() != 1 {
		return Dispatch{}, ErrExpired
	}
	if _, err = tx.Exec(ctx, "UPDATE accounts SET revision=revision+1 WHERE tenant_id=$1 AND account_id=$2", request.Key.TenantID, request.Key.AccountID); err != nil {
		return Dispatch{}, err
	}
	if err = journal(ctx, tx, request.Key, a.Revision+1, "dispatch_attempt", request); err != nil {
		return Dispatch{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Dispatch{}, err
	}
	i.State = "attempted"
	i.AttemptID = request.AttemptID
	i.AuthorizationRevision = a.Revision + 1
	i.AuthorizationSnapshot = a.Market.SnapshotID
	return Dispatch{Intent: i, Revision: a.Revision + 1}, nil
}

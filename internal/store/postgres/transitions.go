package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

func obsoleteQueued(ctx context.Context, tx pgx.Tx, key Key, planID, reason string) error {
	_, err := tx.Exec(ctx, `WITH obsolete AS (
 UPDATE outbox SET state='obsolete',resolution_evidence=jsonb_build_object('reason',$4::text)
 WHERE tenant_id=$1 AND account_id=$2 AND state='queued' AND ($3::text='' OR plan_id=$3)
 RETURNING intent_id)
 UPDATE reservations SET state='released' WHERE tenant_id=$1 AND account_id=$2 AND state='held'
 AND intent_id IN (SELECT intent_id FROM obsolete)`, key.TenantID, key.AccountID, planID, reason)
	return err
}
func (s *Store) ClaimOwnership(ctx context.Context, key Key, owner string, lease time.Duration, proof RecoveryProof) (Ownership, error) {
	if !validKey(key) || !validID(owner) || lease <= 0 || lease > 5*time.Minute {
		return Ownership{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ownership{}, err
	}
	defer tx.Rollback(ctx)
	a, err := lockAccount(ctx, tx, key)
	if err != nil {
		return Ownership{}, err
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return Ownership{}, err
	}
	if a.Ownership.OwnerID != "" {
		if a.Ownership.OwnerID == owner && a.LeaseUntil.After(now) {
			return a.Ownership, nil
		}
		if !proof.PreviousSenderIsolated || !proof.Reconciled || !validID(proof.Reference) {
			return Ownership{}, ErrIsolationRequired
		}
	}
	if a.Ownership.Epoch == math.MaxInt64 || a.Revision == math.MaxInt64 {
		return Ownership{}, ErrInvalid
	}
	ownership := Ownership{OwnerID: owner, Epoch: a.Ownership.Epoch + 1}
	if _, err = tx.Exec(ctx, `UPDATE accounts SET owner_id=$3,ownership_epoch=$4,lease_until=$5,ready=false,market_valid=false,revision=revision+1 WHERE tenant_id=$1 AND account_id=$2`, key.TenantID, key.AccountID, owner, ownership.Epoch, now.Add(lease)); err != nil {
		return Ownership{}, err
	}
	if err = obsoleteQueued(ctx, tx, key, "", "ownership change"); err != nil {
		return Ownership{}, err
	}
	if err = journal(ctx, tx, key, a.Revision+1, "ownership", struct {
		Ownership Ownership
		Proof     RecoveryProof
	}{ownership, proof}); err != nil {
		return Ownership{}, err
	}
	return ownership, tx.Commit(ctx)
}
func (s *Store) RenewOwnership(ctx context.Context, key Key, ownership Ownership, lease time.Duration) error {
	if !validKey(key) || lease <= 0 || lease > 5*time.Minute {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	a, err := lockAccount(ctx, tx, key)
	if err != nil {
		return err
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return err
	}
	if err = checkOwner(a, ownership, now); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE accounts SET lease_until=$3 WHERE tenant_id=$1 AND account_id=$2", key.TenantID, key.AccountID, now.Add(lease))
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func planStatus(s string) bool {
	switch s {
	case "active", "paused", "cancel_requested", "completed", "cancelled", "expired":
		return true
	}
	return false
}
func finalPlan(s string) bool { return s == "completed" || s == "cancelled" || s == "expired" }
func validateChange(c Change) error {
	if !validKey(c.Key) || !validID(c.Command.ID) || !validID(c.Command.Kind) || c.Command.ExpectedRevision < 0 || !balancesValid(c.Balances) {
		return ErrInvalid
	}
	if len(c.Snapshot) > 0 && !validJSON(c.Snapshot) {
		return ErrInvalid
	}
	if len(c.Enqueue) > 64 || len(c.Reservations) > 128 || len(c.Resolutions) > 64 || len(c.ReservationUpdates) > 32 {
		return ErrInvalid
	}
	if c.Market != nil && (!validID(c.Market.SnapshotID) || !validID(c.Market.InstrumentRevision)) {
		return ErrInvalid
	}
	if p := c.Plan; p != nil {
		if !validID(p.ID) || p.Revision <= 0 || !planStatus(p.Status) || p.Deadline.IsZero() || !validJSON(p.Detail) {
			return ErrInvalid
		}
	}
	intents := map[string]bool{}
	for _, i := range c.Enqueue {
		if !validID(i.ID) || intents[i.ID] || !validID(i.PlanID) || i.PlanRevision <= 0 || i.ExpiresAt.IsZero() || !validID(i.InstrumentRevision) || !validJSON(i.Payload) || (i.State != "" && i.State != "queued") || i.AttemptID != "" || i.VenueOrderID != "" || i.AuthorizationRevision != 0 || i.AuthorizationSnapshot != "" {
			return ErrInvalid
		}
		intents[i.ID] = true
	}
	reservations := map[string]bool{}
	reserved := map[string]bool{}
	for _, r := range c.Reservations {
		identity := r.IntentID + "\x00" + r.Currency
		if !intents[r.IntentID] || !currency.MatchString(r.Currency) || r.Amount.Sign() <= 0 || r.Reflected || (r.State != "" && r.State != "held") || reservations[identity] {
			return ErrInvalid
		}
		reservations[identity] = true
		reserved[r.IntentID] = true
	}
	for id := range intents {
		if !reserved[id] {
			return ErrInvalid
		}
	}
	resolved := map[string]bool{}
	for _, r := range c.Resolutions {
		if !validID(r.IntentID) || resolved[r.IntentID] || intents[r.IntentID] || !validJSON(r.Evidence) || (r.State != "acknowledged" && r.State != "unknown" && r.State != "final") || r.State == "final" && !r.KnownFinal || r.State != "final" && r.KnownFinal || len(r.VenueOrderID) > 128 {
			return ErrInvalid
		}
		resolved[r.IntentID] = true
	}
	if e := c.Execution; e != nil {
		if !validID(e.ID) || !validID(e.IntentID) || !validID(e.Venue) || !validJSON(e.Detail) || len(c.Balances) == 0 || len(c.Snapshot) == 0 {
			return ErrInvalid
		}
	}
	adjustments := map[string]bool{}
	for _, r := range c.ReservationUpdates {
		identity := r.IntentID + "\x00" + r.Currency
		if c.Execution == nil || r.IntentID != c.Execution.IntentID || !currency.MatchString(r.Currency) || r.Remaining.Sign() <= 0 || adjustments[identity] {
			return ErrInvalid
		}
		adjustments[identity] = true
	}
	return nil
}

func existingCommand(ctx context.Context, tx pgx.Tx, key Key, cmd Command, digest [32]byte) (Result, bool, error) {
	var saved []byte
	var body []byte
	err := tx.QueryRow(ctx, "SELECT digest,result FROM commands WHERE tenant_id=$1 AND account_id=$2 AND command_id=$3", key.TenantID, key.AccountID, cmd.ID).Scan(&saved, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if !bytes.Equal(saved, digest[:]) {
		return Result{}, true, ErrConflict
	}
	var result Result
	if err = json.Unmarshal(body, &result); err != nil {
		return Result{}, true, err
	}
	result.Duplicate = true
	return result, true, nil
}
func saveCommand(ctx context.Context, tx pgx.Tx, key Key, cmd Command, digest [32]byte, result Result) error {
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO commands(tenant_id,account_id,command_id,kind,digest,expected_revision,result) VALUES($1,$2,$3,$4,$5,$6,$7)", key.TenantID, key.AccountID, cmd.ID, cmd.Kind, digest[:], cmd.ExpectedRevision, body)
	return normalized(err)
}
func checkFunds(ctx context.Context, tx pgx.Tx, key Key) error {
	var currencies int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM balances WHERE tenant_id=$1 AND account_id=$2", key.TenantID, key.AccountID).Scan(&currencies); err != nil {
		return err
	}
	if currencies > 32 {
		return ErrInvalid
	}
	var bad bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM balances b LEFT JOIN reservations r ON r.tenant_id=b.tenant_id AND r.account_id=b.account_id AND r.currency=b.currency AND r.state='held' WHERE b.tenant_id=$1 AND b.account_id=$2 GROUP BY b.currency,b.free,b.locked HAVING coalesce(sum(r.amount) FILTER (WHERE NOT r.reflected),0)>b.free OR coalesce(sum(r.amount) FILTER (WHERE r.reflected),0)>b.locked)`, key.TenantID, key.AccountID).Scan(&bad)
	if err != nil {
		return err
	}
	if bad {
		return ErrInsufficientFunds
	}
	return nil
}
func applyPlan(ctx context.Context, tx pgx.Tx, key Key, p Plan, now time.Time) error {
	var revision int64
	var oldStatus string
	err := tx.QueryRow(ctx, "SELECT revision,status FROM plans WHERE tenant_id=$1 AND account_id=$2 AND plan_id=$3", key.TenantID, key.AccountID, p.ID).Scan(&revision, &oldStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		if p.Revision != 1 {
			return ErrStaleRevision
		}
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM outbox WHERE tenant_id=$1 AND account_id=$2 AND state IN ('attempted','unknown','acknowledged'))`, key.TenantID, key.AccountID).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return ErrNotReady
		}
	} else if err != nil {
		return err
	} else if revision == math.MaxInt64 || p.Revision != revision+1 || finalPlan(oldStatus) {
		return ErrStaleRevision
	}
	if p.Status == "active" && !p.Deadline.After(now) {
		return ErrExpired
	}
	if finalPlan(p.Status) {
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM outbox WHERE tenant_id=$1 AND account_id=$2 AND plan_id=$3 AND state IN ('attempted','unknown','acknowledged'))`, key.TenantID, key.AccountID, p.ID).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return ErrNotReady
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO plans(tenant_id,account_id,plan_id,revision,status,deadline,detail) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(tenant_id,account_id,plan_id) DO UPDATE SET revision=excluded.revision,status=excluded.status,deadline=excluded.deadline,detail=excluded.detail`, key.TenantID, key.AccountID, p.ID, p.Revision, p.Status, p.Deadline, p.Detail)
	if err != nil {
		return normalized(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO plan_revisions(tenant_id,account_id,plan_id,revision,status,deadline,detail) VALUES($1,$2,$3,$4,$5,$6,$7)`, key.TenantID, key.AccountID, p.ID, p.Revision, p.Status, p.Deadline, p.Detail); err != nil {
		return err
	}
	return obsoleteQueued(ctx, tx, key, p.ID, "plan revision changed")
}
func resolve(ctx context.Context, tx pgx.Tx, key Key, r Resolution) error {
	var state string
	err := tx.QueryRow(ctx, "SELECT state FROM outbox WHERE tenant_id=$1 AND account_id=$2 AND intent_id=$3", key.TenantID, key.AccountID, r.IntentID).Scan(&state)
	if err != nil {
		return normalized(err)
	}
	if state != "attempted" && state != "unknown" && state != "acknowledged" {
		return ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE outbox SET state=$4,venue_order_id=CASE WHEN $5::text='' THEN venue_order_id ELSE $5 END,resolution_evidence=$6 WHERE tenant_id=$1 AND account_id=$2 AND intent_id=$3`, key.TenantID, key.AccountID, r.IntentID, r.State, r.VenueOrderID, r.Evidence); err != nil {
		return err
	}
	if r.State == "final" {
		_, err = tx.Exec(ctx, "UPDATE reservations SET state='released' WHERE tenant_id=$1 AND account_id=$2 AND intent_id=$3", key.TenantID, key.AccountID, r.IntentID)
	} else if r.Reflected != nil {
		_, err = tx.Exec(ctx, "UPDATE reservations SET reflected=$4 WHERE tenant_id=$1 AND account_id=$2 AND intent_id=$3 AND state='held'", key.TenantID, key.AccountID, r.IntentID, *r.Reflected)
	}
	return err
}

// Apply commits projection, reservation, command, execution, journal, and outbox
// changes together under the account row lock. A failed transition rolls back all.
func (s *Store) Apply(ctx context.Context, c Change) (Result, error) {
	if err := validateChange(c); err != nil {
		return Result{}, err
	}
	body, err := json.Marshal(c)
	if err != nil {
		return Result{}, err
	}
	if len(body) > 2<<20 {
		return Result{}, ErrInvalid
	}
	digest := sha256.Sum256(body)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	a, err := lockAccount(ctx, tx, c.Key)
	if err != nil {
		return Result{}, err
	}
	if result, exists, err := existingCommand(ctx, tx, c.Key, c.Command, digest); exists || err != nil {
		return result, err
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return Result{}, err
	}
	if err = checkOwner(a, c.Ownership, now); err != nil {
		return Result{}, err
	}
	if e := c.Execution; e != nil {
		var saved, encoded []byte
		executionBody, _ := json.Marshal(e)
		executionDigest := sha256.Sum256(executionBody)
		err = tx.QueryRow(ctx, "SELECT digest,result FROM executions WHERE tenant_id=$1 AND account_id=$2 AND venue=$3 AND execution_id=$4", c.Key.TenantID, c.Key.AccountID, e.Venue, e.ID).Scan(&saved, &encoded)
		if err == nil {
			if !bytes.Equal(saved, executionDigest[:]) {
				return Result{}, ErrConflict
			}
			var result Result
			if err = json.Unmarshal(encoded, &result); err != nil {
				return Result{}, err
			}
			result.Duplicate = true
			if err = saveCommand(ctx, tx, c.Key, c.Command, digest, result); err != nil {
				return Result{}, err
			}
			return result, tx.Commit(ctx)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Result{}, err
		}
	}
	if c.Command.ExpectedRevision != a.Revision {
		return Result{}, ErrStaleRevision
	}
	if a.Revision == math.MaxInt64 {
		return Result{}, ErrInvalid
	}
	for _, r := range c.Resolutions {
		if err = resolve(ctx, tx, c.Key, r); err != nil {
			return Result{}, err
		}
	}
	if err = putBalances(ctx, tx, c.Key, c.Balances); err != nil {
		return Result{}, err
	}
	if c.Plan != nil {
		if err = applyPlan(ctx, tx, c.Key, *c.Plan, now); err != nil {
			return Result{}, err
		}
	}
	if c.Ready != nil {
		a.Ready = *c.Ready
	}
	if c.Market != nil {
		if !c.Market.Valid || a.Market.InstrumentRevision != c.Market.InstrumentRevision {
			if err = obsoleteQueued(ctx, tx, c.Key, "", "market invalid or metadata revision changed"); err != nil {
				return Result{}, err
			}
		}
		a.Market = *c.Market
	}
	if !a.Ready {
		if err = obsoleteQueued(ctx, tx, c.Key, "", "account not ready"); err != nil {
			return Result{}, err
		}
	}
	if len(c.Snapshot) > 0 {
		a.Snapshot = c.Snapshot
	}
	if len(c.Enqueue) > 0 && (!a.Ready || !a.Market.Valid) {
		return Result{}, ErrNotReady
	}
	for _, i := range c.Enqueue {
		var planRevision int64
		var status string
		var deadline time.Time
		err = tx.QueryRow(ctx, "SELECT revision,status,deadline FROM plans WHERE tenant_id=$1 AND account_id=$2 AND plan_id=$3", c.Key.TenantID, c.Key.AccountID, i.PlanID).Scan(&planRevision, &status, &deadline)
		if err != nil {
			return Result{}, normalized(err)
		}
		if planRevision != i.PlanRevision {
			return Result{}, ErrStaleRevision
		}
		if status != "active" {
			return Result{}, ErrNotReady
		}
		expires := i.ExpiresAt.UTC().Truncate(time.Microsecond)
		if !deadline.After(now) || !expires.After(now) || expires.After(deadline) {
			return Result{}, ErrExpired
		}
		if i.InstrumentRevision != a.Market.InstrumentRevision {
			return Result{}, ErrStaleRevision
		}
		_, err = tx.Exec(ctx, `INSERT INTO outbox(tenant_id,account_id,intent_id,plan_id,plan_revision,instrument_revision,expires_at,payload,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'queued')`, c.Key.TenantID, c.Key.AccountID, i.ID, i.PlanID, i.PlanRevision, i.InstrumentRevision, expires, i.Payload)
		if err != nil {
			return Result{}, normalized(err)
		}
	}
	for _, r := range c.Reservations {
		_, err = tx.Exec(ctx, `INSERT INTO reservations(tenant_id,account_id,intent_id,currency,amount,reflected,state) VALUES($1,$2,$3,$4,$5::numeric,false,'held')`, c.Key.TenantID, c.Key.AccountID, r.IntentID, r.Currency, r.Amount.String())
		if err != nil {
			return Result{}, normalized(err)
		}
	}
	for _, r := range c.ReservationUpdates {
		updated, updateErr := tx.Exec(ctx, `UPDATE reservations SET amount=$5::numeric WHERE tenant_id=$1 AND account_id=$2 AND intent_id=$3 AND currency=$4 AND state='held' AND amount >= $5::numeric`, c.Key.TenantID, c.Key.AccountID, r.IntentID, r.Currency, r.Remaining.String())
		if updateErr != nil {
			return Result{}, normalized(updateErr)
		}
		if updated.RowsAffected() != 1 {
			return Result{}, ErrInvalid
		}
	}
	var pendingCount int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE tenant_id=$1 AND account_id=$2 AND state IN ('queued','attempted','unknown','acknowledged')`, c.Key.TenantID, c.Key.AccountID).Scan(&pendingCount); err != nil {
		return Result{}, err
	}
	if pendingCount > 64 {
		return Result{}, ErrInvalid
	}
	if err = checkFunds(ctx, tx, c.Key); err != nil {
		return Result{}, err
	}
	result := Result{Revision: a.Revision + 1}
	if e := c.Execution; e != nil {
		var state string
		if err = tx.QueryRow(ctx, "SELECT state FROM outbox WHERE tenant_id=$1 AND account_id=$2 AND intent_id=$3", c.Key.TenantID, c.Key.AccountID, e.IntentID).Scan(&state); err != nil {
			return Result{}, normalized(err)
		}
		if state != "attempted" && state != "unknown" && state != "acknowledged" && state != "final" {
			return Result{}, ErrInvalid
		}
		executionBody, _ := json.Marshal(e)
		executionDigest := sha256.Sum256(executionBody)
		encoded, _ := json.Marshal(result)
		_, err = tx.Exec(ctx, "INSERT INTO executions(tenant_id,account_id,venue,execution_id,intent_id,digest,detail,result) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", c.Key.TenantID, c.Key.AccountID, e.Venue, e.ID, e.IntentID, executionDigest[:], e.Detail, encoded)
		if err != nil {
			return Result{}, normalized(err)
		}
	}
	_, err = tx.Exec(ctx, `UPDATE accounts SET revision=$3,snapshot=$4,ready=$5,market_valid=$6,market_snapshot_id=$7,instrument_revision=$8 WHERE tenant_id=$1 AND account_id=$2`, c.Key.TenantID, c.Key.AccountID, result.Revision, a.Snapshot, a.Ready, a.Market.Valid, a.Market.SnapshotID, a.Market.InstrumentRevision)
	if err != nil {
		return Result{}, err
	}
	if err = journal(ctx, tx, c.Key, result.Revision, c.Command.Kind, c); err != nil {
		return Result{}, err
	}
	if err = saveCommand(ctx, tx, c.Key, c.Command, digest, result); err != nil {
		return Result{}, err
	}
	return result, tx.Commit(ctx)
}

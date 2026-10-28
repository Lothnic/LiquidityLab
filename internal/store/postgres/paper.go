package postgres

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// PaperTransportContract identifies a simulated venue whose only lasting effect
// is an owned PostgreSQL observation. It must never identify an exchange sender.
const PaperTransportContract = "postgres-paper-v1"

func paperObservationKind(kind string) bool {
	return kind == "transport" || kind == "paper_trade"
}

// ClaimPaperOwnership atomically fences the previous simulated sender and
// returns the complete post-claim account. ObservedSequence is the recovery
// watermark: all paper effects committed before the claim are at or below it.
// Readiness stays false until the caller reduces and reconciles those effects.
// This fences only the database effect of the local paper venue, not external I/O.
func (s *Store) ClaimPaperOwnership(ctx context.Context, key Key, owner string, lease time.Duration) (Account, error) {
	if !validKey(key) || !validID(owner) || lease <= 0 || lease > 5*time.Minute {
		return Account{}, ErrInvalid
	}
	tx, err := s.begin(ctx, pgx.TxOptions{})
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback(ctx)
	a, err := lockAccount(ctx, tx, key)
	if err != nil {
		return Account{}, err
	}
	if a.PaperTransportContract != "" && a.PaperTransportContract != PaperTransportContract {
		return Account{}, ErrInvalid
	}
	var identity struct {
		Profile string `json:"profile"`
	}
	virgin := string(a.Snapshot) == "{}" && a.Revision == 0 && a.Ownership == (Ownership{}) && a.LeaseUntil.IsZero() &&
		!a.Ready && !a.Market.Valid && a.ObservedSequence == 0 && a.ProcessedSequence == 0 && a.PendingObservationBytes == 0 && a.PaperTransportContract == ""
	// A previous paper claim can have committed before the runtime wrote its
	// first snapshot. Its contract is already durable evidence of the venue mode.
	paperBootstrap := string(a.Snapshot) == "{}" && a.PaperTransportContract == PaperTransportContract
	if err = json.Unmarshal(a.Snapshot, &identity); err != nil || identity.Profile != "paper" && !virgin && !paperBootstrap {
		return Account{}, ErrInvalid
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return Account{}, err
	}
	if a.PaperTransportContract == PaperTransportContract && a.Ownership.OwnerID == owner && a.LeaseUntil.After(now) {
		claimed, err := readAccount(ctx, tx, key)
		if err != nil {
			return Account{}, err
		}
		return claimed, tx.Commit(ctx)
	}
	if a.Ownership.Epoch == math.MaxInt64 || a.Revision == math.MaxInt64 {
		return Account{}, ErrInvalid
	}
	ownership := Ownership{OwnerID: owner, Epoch: a.Ownership.Epoch + 1}
	if _, err = tx.Exec(ctx, `UPDATE accounts SET paper_transport_contract=$3,owner_id=$4,ownership_epoch=$5,lease_until=$6,ready=false,market_valid=false,revision=revision+1 WHERE tenant_id=$1 AND account_id=$2`, key.TenantID, key.AccountID, PaperTransportContract, owner, ownership.Epoch, now.Add(lease)); err != nil {
		return Account{}, err
	}
	if err = obsoleteQueued(ctx, tx, key, "", "paper ownership change"); err != nil {
		return Account{}, err
	}
	if err = journal(ctx, tx, key, a.Revision+1, "paper_ownership", struct {
		Profile           string    `json:"profile"`
		TransportContract string    `json:"transport_contract"`
		PreviousOwnership Ownership `json:"previous_ownership"`
		Ownership         Ownership `json:"ownership"`
		RecoveryWatermark int64     `json:"recovery_watermark"`
		Fence             string    `json:"fence"`
		Reconciliation    string    `json:"reconciliation"`
	}{"paper", PaperTransportContract, a.Ownership, ownership, a.ObservedSequence, "database-only simulated venue effects; previous epoch cannot append transport or paper_trade observations; no external sender isolation asserted", "required after takeover"}); err != nil {
		return Account{}, err
	}
	claimed, err := readAccount(ctx, tx, key)
	if err != nil {
		return Account{}, err
	}
	return claimed, tx.Commit(ctx)
}

// RecordPaperObservation persists a simulated transport or matching effect only
// for the current owner and lease. External financial facts use RecordObservation
// independently of execution ownership and must not use these paper effect kinds.
func (s *Store) RecordPaperObservation(ctx context.Context, key Key, ownership Ownership, observation Observation) (int64, error) {
	if !paperObservationKind(observation.Kind) {
		return 0, ErrInvalid
	}
	return s.recordObservation(ctx, key, &ownership, observation)
}

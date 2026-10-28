package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/liquiditylab/liquiditylab/internal/telemetry"
)

// RecordObservation persists an observed account fact without execution authority.
// The ingress caller is trusted/authenticated by the runtime, never a model. An
// event ID is stable across retries; reusing it for changed facts is a conflict.
func (s *Store) RecordObservation(ctx context.Context, key Key, observation Observation) (int64, error) {
	return s.recordObservation(ctx, key, nil, observation)
}

func (s *Store) recordObservation(ctx context.Context, key Key, ownership *Ownership, observation Observation) (int64, error) {
	if !validKey(key) || !validID(observation.ID) || !validID(observation.Kind) || !validJSON(observation.Payload) {
		return 0, ErrInvalid
	}
	// Canonical JSON prevents harmless whitespace/key ordering differences on retry.
	var value any
	decoder := json.NewDecoder(bytes.NewReader(observation.Payload))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return 0, ErrInvalid
	}
	canonical, err := json.Marshal(struct {
		Kind    string
		Payload any
	}{observation.Kind, value})
	if err != nil {
		return 0, ErrInvalid
	}
	digest := sha256.Sum256(canonical)
	tx, err := s.begin(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	a, err := lockAccount(ctx, tx, key)
	if err != nil {
		return 0, err
	}
	if ownership != nil {
		if a.PaperTransportContract != PaperTransportContract {
			return 0, ErrInvalid
		}
		now, err := databaseNow(ctx, tx)
		if err != nil {
			return 0, err
		}
		if err = checkOwner(a, *ownership, now); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `SELECT set_config('liquiditylab.paper_owner_id',$1,true),set_config('liquiditylab.paper_owner_epoch',$2,true)`, ownership.OwnerID, strconv.FormatInt(ownership.Epoch, 10)); err != nil {
			return 0, err
		}
	} else if a.PaperTransportContract != "" && paperObservationKind(observation.Kind) {
		return 0, ErrOwnership
	}
	var prior []byte
	var sequence int64
	err = tx.QueryRow(ctx, "SELECT sequence,digest FROM observations WHERE tenant_id=$1 AND account_id=$2 AND observation_id=$3", key.TenantID, key.AccountID, observation.ID).Scan(&sequence, &prior)
	if err == nil {
		if !bytes.Equal(prior, digest[:]) {
			return 0, ErrConflict
		}
		s.telemetry.Add(telemetry.DBObservationDuplicates, 1)
		return sequence, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if a.ObservedSequence == math.MaxInt64 || a.ObservedSequence-a.ProcessedSequence >= 4096 || int64(len(observation.Payload)) > 16<<20-a.PendingObservationBytes {
		return 0, ErrInboxFull
	}
	sequence = a.ObservedSequence + 1
	txRecorder(tx).Observe(telemetry.ObservationBytes, uint64(len(observation.Payload)))
	if _, err = tx.Exec(ctx, "INSERT INTO observations(tenant_id,account_id,sequence,observation_id,kind,payload,payload_bytes,digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", key.TenantID, key.AccountID, sequence, observation.ID, observation.Kind, observation.Payload, len(observation.Payload), digest[:]); err != nil {
		return 0, normalized(err)
	}
	if _, err = tx.Exec(ctx, "UPDATE accounts SET observed_sequence=$3,pending_observation_bytes=pending_observation_bytes+$4 WHERE tenant_id=$1 AND account_id=$2", key.TenantID, key.AccountID, sequence, len(observation.Payload)); err != nil {
		return 0, err
	}
	countOnCommit(tx, telemetry.DBObservationWrites)
	return sequence, tx.Commit(ctx)
}

func (s *Store) PendingObservations(ctx context.Context, key Key, limit int) ([]Observation, error) {
	if !validKey(key) || limit < 1 || limit > 4096 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT o.sequence,o.observation_id,o.kind,o.payload,o.received_at FROM observations o JOIN accounts a USING(tenant_id,account_id)
 WHERE o.tenant_id=$1 AND o.account_id=$2 AND o.sequence>a.processed_sequence ORDER BY o.sequence LIMIT $3`, key.TenantID, key.AccountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Observation{}
	for rows.Next() {
		var item Observation
		if err := rows.Scan(&item.Sequence, &item.ID, &item.Kind, &item.Payload, &item.ReceivedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) GetObservation(ctx context.Context, key Key, id string) (Observation, error) {
	if !validKey(key) || !validID(id) {
		return Observation{}, ErrInvalid
	}
	var item Observation
	err := s.pool.QueryRow(ctx, `SELECT sequence,observation_id,kind,payload,received_at FROM observations WHERE tenant_id=$1 AND account_id=$2 AND observation_id=$3`, key.TenantID, key.AccountID, id).Scan(&item.Sequence, &item.ID, &item.Kind, &item.Payload, &item.ReceivedAt)
	return item, normalized(err)
}

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// SessionMeta contains only session identity and display metadata. Snapshot
// bodies are fetched separately by Session.
type SessionMeta struct {
	PlanID                                           string
	CreatedRevision                                  int64
	Revision                                         int64
	CreatedAt, UpdatedAt                             time.Time
	Parent                                           json.RawMessage
	Profile, DataSource, BaseCurrency, QuoteCurrency string
}

type SessionPage struct {
	AsOfRevision int64
	Entries      []SessionMeta
}

type SessionSnapshot struct {
	AsOfRevision, Revision, CreatedRevision int64
	CreatedAt, UpdatedAt                    time.Time
	Snapshot                                json.RawMessage
	TerminalRevision                        int64
	TerminalAt                              time.Time
	TerminalSnapshot                        json.RawMessage
}

// Sessions returns at most limit sessions created by the requested revision,
// newest creation first. beforeRevision is an exclusive creation-revision
// cursor; zero starts at the newest session.
func (s *Store) Sessions(ctx context.Context, key Key, beforeRevision, asOfRevision int64, limit int) (SessionPage, error) {
	page := SessionPage{Entries: []SessionMeta{}}
	if !validKey(key) || beforeRevision < 0 || asOfRevision < 0 || limit < 1 || limit > 50 {
		return page, ErrInvalid
	}
	tx, err := s.begin(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return page, err
	}
	defer tx.Rollback(ctx)
	var current int64
	if err = tx.QueryRow(ctx, `SELECT revision FROM accounts WHERE tenant_id=$1 AND account_id=$2`, key.TenantID, key.AccountID).Scan(&current); err != nil {
		return page, normalized(err)
	}
	if asOfRevision == 0 {
		asOfRevision = current
	}
	if asOfRevision > current {
		return page, ErrInvalid
	}
	page.AsOfRevision = asOfRevision
	rows, err := tx.Query(ctx, `WITH created AS (
  SELECT revision AS created_revision, created_at AS created_at,
         detail->'snapshot'->'parent' AS parent,
         detail->'snapshot'->'parent'->>'id' AS plan_id,
         coalesce(detail->'snapshot'->>'profile','') AS profile,
         coalesce(detail->'snapshot'->>'data_source','') AS data_source,
         coalesce(detail->'snapshot'->>'base_currency','') AS base_currency,
         coalesce(detail->'snapshot'->>'quote_currency','') AS quote_currency
  FROM account_journal
  WHERE tenant_id=$1 AND account_id=$2 AND kind='create' AND revision <= $3
    AND ($4=0 OR revision < $4)
    AND coalesce(detail->'snapshot'->'parent'->>'id','') <> ''
  ORDER BY revision DESC LIMIT $5
), latest AS (
  SELECT c.created_revision,j.revision,j.created_at AS updated_at,
         j.detail->'snapshot'->'parent' AS parent,
         coalesce(j.detail->'snapshot'->>'profile','') AS profile,
         coalesce(j.detail->'snapshot'->>'data_source','') AS data_source,
         coalesce(j.detail->'snapshot'->>'base_currency','') AS base_currency,
         coalesce(j.detail->'snapshot'->>'quote_currency','') AS quote_currency,
         c.plan_id,c.created_at
  FROM created c
  JOIN LATERAL (
    SELECT revision, created_at, detail FROM account_journal
    WHERE tenant_id=$1 AND account_id=$2 AND revision <= $3
      AND detail->'snapshot'->'parent'->>'id'=c.plan_id
    ORDER BY revision DESC LIMIT 1
  ) j ON true
)
SELECT plan_id,created_revision,revision,created_at,updated_at,parent,
       profile,data_source,base_currency,quote_currency
FROM latest ORDER BY created_revision DESC LIMIT $5`, key.TenantID, key.AccountID, asOfRevision, beforeRevision, limit)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var e SessionMeta
		if err = rows.Scan(&e.PlanID, &e.CreatedRevision, &e.Revision, &e.CreatedAt, &e.UpdatedAt, &e.Parent, &e.Profile, &e.DataSource, &e.BaseCurrency, &e.QuoteCurrency); err != nil {
			return page, err
		}
		if len(e.Parent) == 0 || len(e.Parent) > 16<<10 || !json.Valid(e.Parent) {
			return page, fmt.Errorf("%w: session parent exceeds bound or is invalid", ErrInvalid)
		}
		page.Entries = append(page.Entries, e)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	return page, tx.Commit(ctx)
}

// Session returns the latest snapshot for planID visible at asOfRevision.
func (s *Store) Session(ctx context.Context, key Key, planID string, asOfRevision int64) (SessionSnapshot, error) {
	value := SessionSnapshot{}
	if !validKey(key) || !validID(planID) || asOfRevision < 0 {
		return value, ErrInvalid
	}
	tx, err := s.begin(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return value, err
	}
	defer tx.Rollback(ctx)
	var current int64
	if err = tx.QueryRow(ctx, `SELECT revision FROM accounts WHERE tenant_id=$1 AND account_id=$2`, key.TenantID, key.AccountID).Scan(&current); err != nil {
		return value, normalized(err)
	}
	if asOfRevision == 0 {
		asOfRevision = current
	}
	if asOfRevision > current {
		return value, ErrInvalid
	}
	value.AsOfRevision = asOfRevision
	var parent json.RawMessage
	err = tx.QueryRow(ctx, `WITH created AS (
 SELECT revision AS created_revision, created_at AS created_at
 FROM account_journal
 WHERE tenant_id=$1 AND account_id=$2 AND kind='create' AND revision <= $4
   AND detail->'snapshot'->'parent'->>'id'=$3
 ORDER BY revision DESC LIMIT 1
), latest AS (
 SELECT c.created_revision,c.created_at,j.revision,j.created_at AS updated_at,j.detail->'snapshot' AS snapshot
 FROM created c
 JOIN LATERAL (
   SELECT revision,created_at,detail FROM account_journal
   WHERE tenant_id=$1 AND account_id=$2 AND revision <= $4
     AND detail->'snapshot'->'parent'->>'id'=$3
   ORDER BY revision DESC LIMIT 1
 ) j ON true
)
SELECT created_revision,revision,created_at,updated_at,snapshot,snapshot->'parent' FROM latest`, key.TenantID, key.AccountID, planID, asOfRevision).Scan(&value.CreatedRevision, &value.Revision, &value.CreatedAt, &value.UpdatedAt, &value.Snapshot, &parent)
	if err != nil {
		return SessionSnapshot{}, normalized(err)
	}
	if len(value.Snapshot) == 0 || len(value.Snapshot) > 1<<20 || !json.Valid(value.Snapshot) {
		return SessionSnapshot{}, fmt.Errorf("%w: session snapshot exceeds bound or is invalid", ErrInvalid)
	}
	if len(parent) == 0 || len(parent) > 16<<10 || !json.Valid(parent) {
		return SessionSnapshot{}, fmt.Errorf("%w: session parent exceeds bound or is invalid", ErrInvalid)
	}
	var terminalParent json.RawMessage
	err = tx.QueryRow(ctx, `SELECT revision,created_at,detail->'snapshot',detail->'snapshot'->'parent'
FROM account_journal
WHERE tenant_id=$1 AND account_id=$2 AND revision<=$4
  AND detail->'snapshot'->'parent'->>'id'=$3
  AND detail->'snapshot'->'parent'->>'status' IN ('completed','cancelled','expired')
ORDER BY revision ASC LIMIT 1`, key.TenantID, key.AccountID, planID, asOfRevision).Scan(&value.TerminalRevision, &value.TerminalAt, &value.TerminalSnapshot, &terminalParent)
	if err != nil && err != pgx.ErrNoRows {
		return SessionSnapshot{}, err
	}
	if err == nil {
		if len(value.TerminalSnapshot) == 0 || len(value.TerminalSnapshot) > 1<<20 || !json.Valid(value.TerminalSnapshot) {
			return SessionSnapshot{}, fmt.Errorf("%w: terminal session snapshot exceeds bound or is invalid", ErrInvalid)
		}
		if len(terminalParent) == 0 || len(terminalParent) > 16<<10 || !json.Valid(terminalParent) {
			return SessionSnapshot{}, fmt.Errorf("%w: terminal session parent exceeds bound or is invalid", ErrInvalid)
		}
	} else {
		value.TerminalRevision = 0
		value.TerminalAt = time.Time{}
		value.TerminalSnapshot = nil
	}
	if err = tx.Commit(ctx); err != nil {
		return SessionSnapshot{}, err
	}
	return value, nil
}

// SessionJournal returns bounded, ascending journal metadata for one session.
// The detail column is deliberately excluded to avoid exposing snapshots or
// other reducer payloads through the event timeline.
func (s *Store) SessionJournal(ctx context.Context, key Key, planID string, afterRevision, asOfRevision int64, limit int) ([]JournalEntry, error) {
	if !validKey(key) || !validID(planID) || afterRevision < 0 || asOfRevision < 0 || limit < 1 || limit > 200 {
		return nil, ErrInvalid
	}
	tx, err := s.begin(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var current int64
	if err = tx.QueryRow(ctx, `SELECT revision FROM accounts WHERE tenant_id=$1 AND account_id=$2`, key.TenantID, key.AccountID).Scan(&current); err != nil {
		return nil, normalized(err)
	}
	if asOfRevision == 0 {
		asOfRevision = current
	}
	if asOfRevision > current {
		return nil, ErrInvalid
	}
	var exists bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM account_journal WHERE tenant_id=$1 AND account_id=$2 AND kind='create' AND revision <= $4 AND detail->'snapshot'->'parent'->>'id'=$3)`, key.TenantID, key.AccountID, planID, asOfRevision).Scan(&exists)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := tx.Query(ctx, `SELECT revision,kind,created_at FROM account_journal
WHERE tenant_id=$1 AND account_id=$2 AND revision>$4 AND revision<=$5
  AND detail->'snapshot'->'parent'->>'id'=$3
ORDER BY revision ASC LIMIT $6`, key.TenantID, key.AccountID, planID, afterRevision, asOfRevision, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]JournalEntry, 0, limit)
	for rows.Next() {
		var entry JournalEntry
		if err = rows.Scan(&entry.Revision, &entry.Kind, &entry.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return entries, nil
}

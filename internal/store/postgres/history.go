package postgres

import (
	"context"
	"time"
)

// JournalEntry is a bounded account-history projection. It intentionally omits
// the journal detail payload, which can contain large reducer snapshots.
type JournalEntry struct {
	Revision  int64     `json:"revision"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
}

// Journal returns account journal metadata strictly after afterRevision, in
// ascending revision order. It never reads the journal detail column.
func (s *Store) Journal(ctx context.Context, key Key, afterRevision int64, limit int) ([]JournalEntry, error) {
	if !validKey(key) || afterRevision < 0 || limit < 1 || limit > 1000 {
		return nil, ErrInvalid
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE tenant_id=$1 AND account_id=$2)`, key.TenantID, key.AccountID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT revision,kind,created_at
FROM account_journal
WHERE tenant_id=$1 AND account_id=$2 AND revision>$3
ORDER BY revision ASC
LIMIT $4`, key.TenantID, key.AccountID, afterRevision, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]JournalEntry, 0, limit)
	for rows.Next() {
		var entry JournalEntry
		if err := rows.Scan(&entry.Revision, &entry.Kind, &entry.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

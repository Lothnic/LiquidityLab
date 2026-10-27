package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"github.com/liquiditylab/liquiditylab/migrations"
)

type Store struct{ pool *pgxpool.Pool }

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
var currency = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,23}$`)

func validID(s string) bool            { return len(s) > 0 && len(s) <= 128 }
func validKey(k Key) bool              { return validID(k.TenantID) && validID(k.AccountID) }
func validJSON(v json.RawMessage) bool { return len(v) > 0 && len(v) <= 1<<20 && json.Valid(v) }
func normalized(err error) error {
	var e *pgconn.PgError
	if errors.As(err, &e) {
		if e.ConstraintName == "ll_paper_observation_owner" {
			return ErrOwnership
		}
		switch e.Code {
		case "23505":
			return fmt.Errorf("%w: durable uniqueness constraint", ErrConflict)
		case "23503", "23514", "22003":
			return fmt.Errorf("%w: durable constraint violation", ErrInvalid)
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// Open binds a bounded pool to an explicitly named schema. It creates that schema
// if needed; it never drops or resets existing schemas. Call Migrate before use.
func Open(ctx context.Context, dsn, schema string) (*Store, error) {
	if !identifier.MatchString(schema) {
		return nil, fmt.Errorf("%w: schema identifier", ErrInvalid)
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	config.MaxConns = 16
	config.MinConns = 0
	config.MaxConnIdleTime = 5 * time.Minute
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	config.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET search_path TO "+pgx.Identifier{schema}.Sanitize()+", pg_catalog")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize()); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}
func (s *Store) Close() { s.pool.Close() }

func (s *Store) GetExecution(ctx context.Context, key Key, venue, id string) (Execution, error) {
	if !validKey(key) || !validID(venue) || !validID(id) {
		return Execution{}, ErrInvalid
	}
	value := Execution{ID: id, Venue: venue}
	err := s.pool.QueryRow(ctx, "SELECT intent_id,detail FROM executions WHERE tenant_id=$1 AND account_id=$2 AND venue=$3 AND execution_id=$4", key.TenantID, key.AccountID, venue, id).Scan(&value.IntentID, &value.Detail)
	return value, normalized(err)
}

func (s *Store) GetIntent(ctx context.Context, key Key, id string) (Intent, error) {
	if !validKey(key) || !validID(id) {
		return Intent{}, ErrInvalid
	}
	value := Intent{ID: id}
	err := s.pool.QueryRow(ctx, `SELECT plan_id,plan_revision,expires_at,instrument_revision,payload,state,attempt_id,venue_order_id,authorization_revision,authorization_snapshot FROM outbox WHERE tenant_id=$1 AND account_id=$2 AND intent_id=$3`, key.TenantID, key.AccountID, id).Scan(&value.PlanID, &value.PlanRevision, &value.ExpiresAt, &value.InstrumentRevision, &value.Payload, &value.State, &value.AttemptID, &value.VenueOrderID, &value.AuthorizationRevision, &value.AuthorizationSnapshot)
	return value, normalized(err)
}

// LookupCommand returns the immutable result and reducer snapshot of a prior
// command. It never restores that historical snapshot over current account state.
func (s *Store) LookupCommand(ctx context.Context, key Key, commandID string) (CommandRecord, error) {
	if !validKey(key) || !validID(commandID) {
		return CommandRecord{}, ErrInvalid
	}
	var record CommandRecord
	var result []byte
	err := s.pool.QueryRow(ctx, `SELECT c.kind,c.expected_revision,c.result,j.detail->'snapshot'
 FROM commands c LEFT JOIN account_journal j ON j.tenant_id=c.tenant_id AND j.account_id=c.account_id
 AND j.revision=(c.result->>'revision')::bigint
 WHERE c.tenant_id=$1 AND c.account_id=$2 AND c.command_id=$3`, key.TenantID, key.AccountID, commandID).Scan(&record.Kind, &record.ExpectedRevision, &result, &record.Snapshot)
	if err != nil {
		return CommandRecord{}, normalized(err)
	}
	if err := json.Unmarshal(result, &record.Result); err != nil {
		return CommandRecord{}, err
	}
	record.Result.Duplicate = true
	return record, nil
}
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema(), 904215))"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version integer PRIMARY KEY,name text NOT NULL,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`); err != nil {
		return err
	}
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		return err
	}
	names := []string{}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	known := map[int]bool{}
	for _, name := range names {
		parts := strings.SplitN(name, "_", 2)
		version, parseErr := strconv.Atoi(parts[0])
		if parseErr != nil || version <= 0 || known[version] {
			return ErrSchema
		}
		known[version] = true
		body, err := migrations.Files.ReadFile(name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		checksum := hex.EncodeToString(sum[:])
		var savedName, savedChecksum string
		err = tx.QueryRow(ctx, "SELECT name,checksum FROM schema_migrations WHERE version=$1", version).Scan(&savedName, &savedChecksum)
		if err == nil {
			if savedName != name || savedChecksum != checksum {
				return fmt.Errorf("%w: migration %d", ErrSchema, version)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err = tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version,name,checksum) VALUES($1,$2,$3)", version, name, checksum); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return err
	}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			rows.Close()
			return err
		}
		if !known[version] {
			rows.Close()
			return fmt.Errorf("%w: unsupported migration version", ErrSchema)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func balancesValid(values []Balance) (valid bool) {
	defer func() {
		if recover() != nil {
			valid = false
		}
	}()
	if len(values) > 32 {
		return false
	}
	seen := map[string]bool{}
	for _, b := range values {
		if !currency.MatchString(b.Currency) || seen[b.Currency] || d.Min(b.Total, b.Free, b.Locked).Sign() < 0 || !b.Total.Equal(b.Free.Add(b.Locked)) {
			return false
		}
		seen[b.Currency] = true
	}
	return true
}
func putBalances(ctx context.Context, tx pgx.Tx, key Key, values []Balance) error {
	for _, b := range values {
		_, err := tx.Exec(ctx, `INSERT INTO balances(tenant_id,account_id,currency,total,free,locked) VALUES($1,$2,$3,$4::numeric,$5::numeric,$6::numeric) ON CONFLICT(tenant_id,account_id,currency) DO UPDATE SET total=excluded.total,free=excluded.free,locked=excluded.locked`, key.TenantID, key.AccountID, b.Currency, b.Total.String(), b.Free.String(), b.Locked.String())
		if err != nil {
			return normalized(err)
		}
	}
	return nil
}
func (s *Store) CreateAccount(ctx context.Context, key Key, balances []Balance, snapshot json.RawMessage) (Account, error) {
	if !validKey(key) || !balancesValid(balances) || len(balances) == 0 || !validJSON(snapshot) {
		return Account{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "INSERT INTO accounts(tenant_id,account_id,snapshot) VALUES($1,$2,$3)", key.TenantID, key.AccountID, snapshot); err != nil {
		return Account{}, normalized(err)
	}
	if err = putBalances(ctx, tx, key, balances); err != nil {
		return Account{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Account{}, err
	}
	return s.GetAccount(ctx, key)
}

func lockAccount(ctx context.Context, tx pgx.Tx, key Key) (Account, error) {
	a := Account{Key: key}
	var lease *time.Time
	err := tx.QueryRow(ctx, `SELECT revision,owner_id,ownership_epoch,lease_until,ready,market_valid,market_snapshot_id,instrument_revision,snapshot,observed_sequence,processed_sequence,pending_observation_bytes,paper_transport_contract FROM accounts WHERE tenant_id=$1 AND account_id=$2 FOR UPDATE`, key.TenantID, key.AccountID).Scan(&a.Revision, &a.Ownership.OwnerID, &a.Ownership.Epoch, &lease, &a.Ready, &a.Market.Valid, &a.Market.SnapshotID, &a.Market.InstrumentRevision, &a.Snapshot, &a.ObservedSequence, &a.ProcessedSequence, &a.PendingObservationBytes, &a.PaperTransportContract)
	if lease != nil {
		a.LeaseUntil = *lease
	}
	return a, normalized(err)
}
func databaseNow(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now)
	return now, err
}
func checkOwner(a Account, o Ownership, now time.Time) error {
	if !validID(o.OwnerID) || o.Epoch <= 0 || a.Ownership != o || !a.LeaseUntil.After(now) {
		return ErrOwnership
	}
	return nil
}
func journal(ctx context.Context, tx pgx.Tx, key Key, revision int64, kind string, detail any) error {
	body, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO account_journal(tenant_id,account_id,revision,kind,detail) VALUES($1,$2,$3,$4,$5)", key.TenantID, key.AccountID, revision, kind, body)
	return err
}
func (s *Store) GetAccount(ctx context.Context, key Key) (Account, error) {
	if !validKey(key) {
		return Account{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback(ctx)
	a, err := readAccount(ctx, tx, key)
	if err != nil {
		return Account{}, err
	}
	return a, tx.Commit(ctx)
}

// readAccount reads a complete projection in the caller's transaction. A caller
// making a transition must hold the account lock before using the projection.
func readAccount(ctx context.Context, tx pgx.Tx, key Key) (Account, error) {
	a := Account{Key: key}
	var lease *time.Time
	err := tx.QueryRow(ctx, `SELECT revision,owner_id,ownership_epoch,lease_until,ready,market_valid,market_snapshot_id,instrument_revision,snapshot,observed_sequence,processed_sequence,pending_observation_bytes,paper_transport_contract FROM accounts WHERE tenant_id=$1 AND account_id=$2`, key.TenantID, key.AccountID).Scan(&a.Revision, &a.Ownership.OwnerID, &a.Ownership.Epoch, &lease, &a.Ready, &a.Market.Valid, &a.Market.SnapshotID, &a.Market.InstrumentRevision, &a.Snapshot, &a.ObservedSequence, &a.ProcessedSequence, &a.PendingObservationBytes, &a.PaperTransportContract)
	if err != nil {
		return a, normalized(err)
	}
	if lease != nil {
		a.LeaseUntil = *lease
	}
	rows, err := tx.Query(ctx, "SELECT currency,trim_scale(total)::text,trim_scale(free)::text,trim_scale(locked)::text FROM balances WHERE tenant_id=$1 AND account_id=$2 ORDER BY currency", key.TenantID, key.AccountID)
	if err != nil {
		return a, err
	}
	for rows.Next() {
		var b Balance
		var total, free, locked string
		if err = rows.Scan(&b.Currency, &total, &free, &locked); err != nil {
			rows.Close()
			return a, err
		}
		b.Total, err = d.ParseInternalDecimal(total)
		if err == nil {
			b.Free, err = d.ParseInternalDecimal(free)
		}
		if err == nil {
			b.Locked, err = d.ParseInternalDecimal(locked)
		}
		if err != nil {
			rows.Close()
			return a, err
		}
		a.Balances = append(a.Balances, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return a, err
	}
	rows, err = tx.Query(ctx, `SELECT plan_id,revision,status,deadline,detail FROM plans WHERE tenant_id=$1 AND account_id=$2 AND status IN ('active','paused','cancel_requested') ORDER BY plan_id`, key.TenantID, key.AccountID)
	if err != nil {
		return a, err
	}
	for rows.Next() {
		var p Plan
		if err = rows.Scan(&p.ID, &p.Revision, &p.Status, &p.Deadline, &p.Detail); err != nil {
			rows.Close()
			return a, err
		}
		a.Plans = append(a.Plans, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return a, err
	}
	rows, err = tx.Query(ctx, `SELECT intent_id,currency,trim_scale(amount)::text,reflected,state FROM reservations WHERE tenant_id=$1 AND account_id=$2 AND state='held' ORDER BY intent_id,currency`, key.TenantID, key.AccountID)
	if err != nil {
		return a, err
	}
	for rows.Next() {
		var r Reservation
		var amount string
		if err = rows.Scan(&r.IntentID, &r.Currency, &amount, &r.Reflected, &r.State); err != nil {
			rows.Close()
			return a, err
		}
		r.Amount, err = d.ParseInternalDecimal(amount)
		if err != nil {
			rows.Close()
			return a, err
		}
		a.Reservations = append(a.Reservations, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return a, err
	}
	rows, err = tx.Query(ctx, `SELECT intent_id,plan_id,plan_revision,expires_at,instrument_revision,payload,state,attempt_id,venue_order_id,authorization_revision,authorization_snapshot FROM outbox WHERE tenant_id=$1 AND account_id=$2 AND state IN ('queued','attempted','unknown','acknowledged') ORDER BY intent_id`, key.TenantID, key.AccountID)
	if err != nil {
		return a, err
	}
	for rows.Next() {
		var i Intent
		if err = rows.Scan(&i.ID, &i.PlanID, &i.PlanRevision, &i.ExpiresAt, &i.InstrumentRevision, &i.Payload, &i.State, &i.AttemptID, &i.VenueOrderID, &i.AuthorizationRevision, &i.AuthorizationSnapshot); err != nil {
			rows.Close()
			return a, err
		}
		a.Intents = append(a.Intents, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return a, err
	}
	return a, nil
}

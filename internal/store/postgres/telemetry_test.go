package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/liquiditylab/liquiditylab/internal/telemetry"
)

func telemetryTestStore(t *testing.T, options Options) *Store {
	t.Helper()
	dsn := os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set LIQUIDITYLAB_TEST_POSTGRES_DSN for isolated PostgreSQL telemetry checks")
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "ll_telemetry_" + hex.EncodeToString(suffix[:])
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := OpenWithOptions(ctx, dsn, schema, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.pool.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
		s.Close()
	})
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func counterDelta(before, after telemetry.Snapshot, id telemetry.Counter) uint64 {
	return after.Counters[id].Value - before.Counters[id].Value
}

func distributionCount(before, after telemetry.Snapshot, id telemetry.Distribution) uint64 {
	return after.Distributions[id].Count - before.Distributions[id].Count
}

func telemetryDeltaSummary(before, after telemetry.Snapshot) string {
	return fmt.Sprintf("tx(started=%d committed=%d rolled_back=%d commit_errors=%d rollback_errors=%d in_flight=%d) writes(journal=%d observation=%d duplicates=%d) acquire_errors=%d samples(begin=%d transaction=%d commit=%d rollback=%d lock=%d acquire=%d) bytes(snapshot=%d journal=%d observation=%d)",
		counterDelta(before, after, telemetry.DBTransactionsStarted), counterDelta(before, after, telemetry.DBTransactionsCommitted),
		counterDelta(before, after, telemetry.DBTransactionsRolledBack), counterDelta(before, after, telemetry.DBCommitErrors),
		counterDelta(before, after, telemetry.DBRollbackErrors), after.Gauges[telemetry.DBTransactionsInFlight].Value,
		counterDelta(before, after, telemetry.DBDurableTransitions), counterDelta(before, after, telemetry.DBObservationWrites),
		counterDelta(before, after, telemetry.DBObservationDuplicates), counterDelta(before, after, telemetry.DBAcquireErrors),
		distributionCount(before, after, telemetry.DBBeginNS), distributionCount(before, after, telemetry.DBTransactionNS),
		distributionCount(before, after, telemetry.DBCommitNS), distributionCount(before, after, telemetry.DBRollbackNS),
		distributionCount(before, after, telemetry.DBAccountLockNS), distributionCount(before, after, telemetry.DBAcquireNS),
		after.Distributions[telemetry.SnapshotBytes].Sum-before.Distributions[telemetry.SnapshotBytes].Sum,
		after.Distributions[telemetry.JournalBytes].Sum-before.Distributions[telemetry.JournalBytes].Sum,
		after.Distributions[telemetry.ObservationBytes].Sum-before.Distributions[telemetry.ObservationBytes].Sum)
}

// pgxpool releases broken connections through puddle's asynchronous destructor.
// Until that goroutine removes the resource, Stat can still report it acquired.
// A successful query through this one-slot pool proves reuse; then inspect the
// quiescent statistics rather than treating an intermediate snapshot as a leak.
func assertTelemetryPoolReusable(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	connection, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("pool did not become reusable after completion: err=%v pool=%+v", err, s.PoolStats())
	}
	var one int
	err = connection.QueryRow(ctx, `SELECT 1`).Scan(&one)
	connection.Release()
	if err != nil || one != 1 {
		t.Fatalf("reacquired pool connection is not usable: result=%d err=%v pool=%+v", one, err, s.PoolStats())
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		stats := s.PoolStats()
		if stats.AcquiredConnections == 0 && stats.ConstructingConnections == 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("pool did not settle after completed reuse: pool=%+v", stats)
		case <-ticker.C:
		}
	}
}

func TestStoreTelemetryPoolBoundsAndCanceledAcquire(t *testing.T) {
	for _, maximum := range []int32{-1, 65} {
		if _, err := OpenWithOptions(context.Background(), "unused", "valid_schema", Options{MaxConnections: maximum}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("pool bound %d accepted or misreported: %v", maximum, err)
		}
	}
	defaults := telemetryTestStore(t, Options{})
	if defaults.PoolStats().MaxConnections != 16 {
		t.Fatalf("default pool cap changed: %+v", defaults.PoolStats())
	}
	r := telemetry.New()
	s := telemetryTestStore(t, Options{Telemetry: r, MaxConnections: 1})
	connection, err := s.pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	before := r.Snapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	_, err = s.GetAccount(ctx, Key{"unreachable", "pool-full"})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded acquisition ignored cancellation: %v", err)
	}
	after := r.Snapshot()
	if counterDelta(before, after, telemetry.DBAcquireErrors) != 1 || distributionCount(before, after, telemetry.DBAcquireNS) != 1 ||
		counterDelta(before, after, telemetry.DBTransactionsStarted) != 0 || after.Gauges[telemetry.DBTransactionsInFlight].Value != 0 {
		t.Fatalf("failed acquire became a started transaction: %s pool=%+v", telemetryDeltaSummary(before, after), s.PoolStats())
	}
	stats := s.PoolStats()
	if stats.MaxConnections != 1 || stats.TotalConnections > 1 || stats.AcquiredConnections != 1 || stats.CanceledAcquireCount == 0 {
		t.Fatalf("pool cap/cancellation statistics incorrect: %+v", stats)
	}
	connection.Release()
	// Direct pool queries also acquire through the tracer, not just s.begin.
	before = r.Snapshot()
	if _, err := s.GetObservation(context.Background(), Key{"missing", "account"}, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	after = r.Snapshot()
	if distributionCount(before, after, telemetry.DBAcquireNS) != 1 || counterDelta(before, after, telemetry.DBQueries) != 1 {
		t.Fatalf("nontransactional reads escaped pool/query metrics: %s pool=%+v", telemetryDeltaSummary(before, after), s.PoolStats())
	}
}

func TestStoreTelemetryCommitFailureDoesNotAssertRollback(t *testing.T) {
	r := telemetry.New()
	s := telemetryTestStore(t, Options{Telemetry: r, MaxConnections: 1})
	ctx := context.Background()
	before := r.Snapshot()
	tx, err := s.begin(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SELECT 1/0`); err == nil {
		t.Fatal("expected real PostgreSQL transaction failure")
	}
	countOnCommit(tx, telemetry.DBDurableTransitions)
	countOnCommit(tx, telemetry.DBObservationWrites)
	if err = tx.Commit(ctx); !errors.Is(err, pgx.ErrTxCommitRollback) {
		t.Fatalf("failed commit lost its rollback status error: %v", err)
	}
	if err = tx.Rollback(ctx); !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("cleanup after failed commit was not closed: %v", err)
	}
	assertTelemetryPoolReusable(t, s)
	after := r.Snapshot()
	if counterDelta(before, after, telemetry.DBTransactionsStarted) != 1 || counterDelta(before, after, telemetry.DBCommitErrors) != 1 ||
		counterDelta(before, after, telemetry.DBTransactionsCommitted) != 0 || counterDelta(before, after, telemetry.DBTransactionsRolledBack) != 0 ||
		counterDelta(before, after, telemetry.DBDurableTransitions) != 0 || counterDelta(before, after, telemetry.DBObservationWrites) != 0 ||
		distributionCount(before, after, telemetry.DBCommitNS) != 1 || distributionCount(before, after, telemetry.DBRollbackNS) != 0 ||
		distributionCount(before, after, telemetry.DBTransactionNS) != 1 || after.Gauges[telemetry.DBTransactionsInFlight].Value != 0 || s.PoolStats().AcquiredConnections != 0 {
		t.Fatalf("failed commit was counted as durable or rolled back twice: %s pool=%+v", telemetryDeltaSummary(before, after), s.PoolStats())
	}
	// The same one-connection pool remains usable after the commit error.
	tx, err = s.begin(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatal(err)
	}
	after = r.Snapshot()
	if counterDelta(before, after, telemetry.DBTransactionsCommitted) != 1 || distributionCount(before, after, telemetry.DBTransactionNS) != 2 {
		t.Fatalf("successful commit/cleanup counted more than once: %s pool=%+v", telemetryDeltaSummary(before, after), s.PoolStats())
	}
}

func TestStoreTelemetryRollbackFailureAndBeginFailureReleasePool(t *testing.T) {
	r := telemetry.New()
	s := telemetryTestStore(t, Options{Telemetry: r, MaxConnections: 1})
	ctx := context.Background()
	before := r.Snapshot()
	if _, err := s.begin(ctx, pgx.TxOptions{IsoLevel: "invalid-isolation-level"}); err == nil {
		t.Fatal("invalid BEGIN unexpectedly succeeded")
	}
	assertTelemetryPoolReusable(t, s)
	after := r.Snapshot()
	if counterDelta(before, after, telemetry.DBTransactionsStarted) != 0 || distributionCount(before, after, telemetry.DBBeginNS) != 1 ||
		after.Gauges[telemetry.DBTransactionsInFlight].Value != 0 || s.PoolStats().AcquiredConnections != 0 {
		t.Fatalf("BEGIN error leaked transaction admission or pool slot: %s pool=%+v", telemetryDeltaSummary(before, after), s.PoolStats())
	}
	tx, err := s.begin(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err = tx.Rollback(canceled); err == nil {
		t.Fatal("cancelled rollback did not report its failure")
	}
	if err = tx.Rollback(ctx); !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatal(err)
	}
	assertTelemetryPoolReusable(t, s)
	after = r.Snapshot()
	if counterDelta(before, after, telemetry.DBRollbackErrors) != 1 || counterDelta(before, after, telemetry.DBTransactionsRolledBack) != 0 ||
		distributionCount(before, after, telemetry.DBRollbackNS) != 1 || after.Gauges[telemetry.DBTransactionsInFlight].Value != 0 || s.PoolStats().AcquiredConnections != 0 {
		t.Fatalf("rollback failure was asserted successful or leaked: %s pool=%+v", telemetryDeltaSummary(before, after), s.PoolStats())
	}
	tx, err = s.begin(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	after = r.Snapshot()
	if counterDelta(before, after, telemetry.DBTransactionsRolledBack) != 1 || distributionCount(before, after, telemetry.DBRollbackNS) != 2 {
		t.Fatalf("explicit successful rollback was not counted once: %s pool=%+v", telemetryDeltaSummary(before, after), s.PoolStats())
	}
}

func TestStoreTelemetryPayloadAttemptsAndAcknowledgedWrites(t *testing.T) {
	r := telemetry.New()
	s := telemetryTestStore(t, Options{Telemetry: r})
	ctx := context.Background()
	key, owner, account := initialized(t, s)
	before := r.Snapshot()
	change := Change{Key: key, Ownership: owner, Command: Command{"measured-change", "snapshot", account.Revision}, Snapshot: raw(`{"private":"snapshot-secret"}`)}
	if _, err := s.Apply(ctx, change); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	after := r.Snapshot()
	if distributionCount(before, after, telemetry.SnapshotBytes) != 1 || after.Distributions[telemetry.SnapshotBytes].Sum-before.Distributions[telemetry.SnapshotBytes].Sum != uint64(len(change.Snapshot)) ||
		distributionCount(before, after, telemetry.JournalBytes) != 1 || after.Distributions[telemetry.JournalBytes].Sum-before.Distributions[telemetry.JournalBytes].Sum != uint64(len(encoded)) ||
		counterDelta(before, after, telemetry.DBDurableTransitions) != 1 || distributionCount(before, after, telemetry.DBAccountLockNS) != 1 {
		t.Fatalf("transition payload/counter measurement incorrect: %s pool=%+v", telemetryDeltaSummary(before, after), s.PoolStats())
	}
	// A duplicate command acquires and rolls back a read transaction, and never
	// attempts to rewrite the checkpoint or journal.
	before = after
	if duplicate, err := s.Apply(ctx, change); err != nil || !duplicate.Duplicate {
		t.Fatalf("identical measured transition retry changed behavior: %+v %v", duplicate, err)
	}
	after = r.Snapshot()
	if counterDelta(before, after, telemetry.DBDurableTransitions) != 0 || distributionCount(before, after, telemetry.SnapshotBytes) != 0 || distributionCount(before, after, telemetry.JournalBytes) != 0 {
		t.Fatalf("duplicate transition was treated as another write: %s pool=%+v", telemetryDeltaSummary(before, after), s.PoolStats())
	}
	observation := Observation{ID: "measured-observation", Kind: "fill", Payload: raw(`{"private":"observation-secret"}`)}
	before = after
	if _, err := s.RecordObservation(ctx, key, observation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordObservation(ctx, key, observation); err != nil {
		t.Fatal(err)
	}
	after = r.Snapshot()
	if counterDelta(before, after, telemetry.DBObservationWrites) != 1 || counterDelta(before, after, telemetry.DBObservationDuplicates) != 1 ||
		distributionCount(before, after, telemetry.ObservationBytes) != 1 || after.Distributions[telemetry.ObservationBytes].Sum-before.Distributions[telemetry.ObservationBytes].Sum != uint64(len(observation.Payload)) {
		t.Fatalf("observation write/duplicate accounting incorrect: %s pool=%+v", telemetryDeltaSummary(before, after), s.PoolStats())
	}
	// Attempted bytes still measure work when the containing transaction rolls
	// back. Durable-transition counters do not claim that the row was retained.
	before = after
	tx, err := s.begin(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal(ctx, tx, key, 9000, "rolled-back", map[string]string{"private": "journal-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	after = r.Snapshot()
	if counterDelta(before, after, telemetry.DBDurableTransitions) != 0 || distributionCount(before, after, telemetry.JournalBytes) != 1 {
		t.Fatalf("rolled-back journal attempt was described as durable: %s pool=%+v", telemetryDeltaSummary(before, after), s.PoolStats())
	}
	serialized, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{key.TenantID, key.AccountID, "snapshot-secret", "observation-secret", "journal-secret", "INSERT INTO", "SELECT ", "postgres://"} {
		if strings.Contains(string(serialized), forbidden) {
			t.Fatalf("telemetry retained identity, SQL or payload %q", forbidden)
		}
	}
}

func TestStoreTelemetryAccountLockIsFullRoundtrip(t *testing.T) {
	r := telemetry.New()
	s := telemetryTestStore(t, Options{Telemetry: r, MaxConnections: 3})
	key, _, _ := initialized(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	holder, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := lockAccount(ctx, holder, key); err != nil {
		t.Fatal(err)
	}
	waiter, err := s.begin(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Rollback(ctx)
	before := r.Snapshot()
	done := make(chan error, 1)
	go func() {
		_, err := lockAccount(ctx, waiter, key)
		done <- err
	}()
	// Observe PostgreSQL's actual blocked query rather than relying on an
	// arbitrary sleep to assume that the second connection reached the lock.
	var blocked bool
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')`, waiter.Conn().PgConn().PID()).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !blocked {
		t.Fatal("waiter did not reach the account row lock")
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := waiter.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	after := r.Snapshot()
	if distributionCount(before, after, telemetry.DBAccountLockNS) != 1 || after.Distributions[telemetry.DBAccountLockNS].Sum <= before.Distributions[telemetry.DBAccountLockNS].Sum {
		t.Fatalf("account FOR UPDATE roundtrip not measured: %s pool=%+v", telemetryDeltaSummary(before, after), s.PoolStats())
	}
	if s.PoolStats().AcquiredConnections != 0 {
		t.Fatal("account lock measurement leaked a pool connection")
	}
}

func TestStoreTelemetryConcurrentTransactionsShareRecorder(t *testing.T) {
	r := telemetry.New()
	s := telemetryTestStore(t, Options{Telemetry: r, MaxConnections: 4})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	before := r.Snapshot()
	errors := make(chan error, 12)
	for i := 0; i < cap(errors); i++ {
		go func(index int) {
			_, err := s.CreateAccount(ctx, Key{"telemetry-concurrent", fmt.Sprint(index)}, []Balance{{"USD", amount("1"), amount("1"), amount("0")}}, raw(`{}`))
			errors <- err
		}(i)
	}
	for i := 0; i < cap(errors); i++ {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	after := r.Snapshot()
	if counterDelta(before, after, telemetry.DBTransactionsStarted) != 24 || counterDelta(before, after, telemetry.DBTransactionsCommitted) != 24 ||
		distributionCount(before, after, telemetry.SnapshotBytes) != 12 || after.Gauges[telemetry.DBTransactionsInFlight].Value != 0 ||
		s.PoolStats().MaxConnections != 4 || s.PoolStats().TotalConnections > 4 || s.PoolStats().AcquiredConnections != 0 {
		t.Fatalf("concurrent metrics lost completions or exceeded pool bound: %s pool=%+v", telemetryDeltaSummary(before, after), s.PoolStats())
	}
}

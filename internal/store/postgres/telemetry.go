package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/liquiditylab/liquiditylab/internal/telemetry"
)

type Options struct {
	Telemetry      *telemetry.Recorder
	MaxConnections int32
}

// PoolStatistics is a point-in-time pool snapshot. AcquireDurationNS covers
// successful acquisitions; DBAcquireNS also includes failed/cancelled attempts.
// Acquisition includes connection construction/ping and is not pure queue wait.
type PoolStatistics struct {
	MaxConnections          int32 `json:"max_connections"`
	TotalConnections        int32 `json:"total_connections"`
	AcquiredConnections     int32 `json:"acquired_connections"`
	IdleConnections         int32 `json:"idle_connections"`
	ConstructingConnections int32 `json:"constructing_connections"`
	AcquireCount            int64 `json:"acquire_count"`
	CanceledAcquireCount    int64 `json:"canceled_acquire_count"`
	EmptyAcquireCount       int64 `json:"empty_acquire_count"`
	AcquireDurationNS       int64 `json:"acquire_duration_ns"`
	EmptyAcquireWaitNS      int64 `json:"empty_acquire_wait_ns"`
}

func (s *Store) PoolStats() PoolStatistics {
	stat := s.pool.Stat()
	return PoolStatistics{
		MaxConnections: stat.MaxConns(), TotalConnections: stat.TotalConns(),
		AcquiredConnections: stat.AcquiredConns(), IdleConnections: stat.IdleConns(),
		ConstructingConnections: stat.ConstructingConns(), AcquireCount: stat.AcquireCount(),
		CanceledAcquireCount: stat.CanceledAcquireCount(), EmptyAcquireCount: stat.EmptyAcquireCount(),
		AcquireDurationNS: int64(stat.AcquireDuration()), EmptyAcquireWaitNS: int64(stat.EmptyAcquireWaitTime()),
	}
}

func elapsedNS(start time.Time) uint64 {
	return uint64(max(0, time.Since(start)))
}

type acquireStartKey struct{}

// poolTracer records fixed metric IDs only. SQL, arguments, errors and account
// identities are never retained in the recorder. pgx invokes acquisition tracing
// for direct pool queries too, not just the explicit transaction helper.
type poolTracer struct{ recorder *telemetry.Recorder }

func (t *poolTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return context.WithValue(ctx, acquireStartKey{}, time.Now())
}

func (t *poolTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	if started, ok := ctx.Value(acquireStartKey{}).(time.Time); ok {
		t.recorder.Observe(telemetry.DBAcquireNS, elapsedNS(started))
	}
	if data.Err != nil {
		t.recorder.Add(telemetry.DBAcquireErrors, 1)
	}
}

func (t *poolTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.recorder.Add(telemetry.DBQueries, 1)
	return ctx
}

func (t *poolTracer) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if data.Err != nil {
		t.recorder.Add(telemetry.DBQueryErrors, 1)
	}
}

// begin keeps pool acquisition separate from the BEGIN round trip. The returned
// wrapper owns the acquired connection until one explicit completion attempt.
func (s *Store) begin(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	if s.telemetry == nil {
		return s.pool.BeginTx(ctx, options)
	}
	connection, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	tx, err := connection.BeginTx(ctx, options)
	s.telemetry.Observe(telemetry.DBBeginNS, elapsedNS(started))
	if err != nil {
		connection.Release()
		return nil, err
	}
	s.telemetry.Add(telemetry.DBTransactionsStarted, 1)
	s.telemetry.Adjust(telemetry.DBTransactionsInFlight, 1)
	return &measuredTx{Tx: tx, recorder: s.telemetry, release: connection.Release, started: time.Now()}, nil
}

type measuredTx struct {
	pgx.Tx
	recorder *telemetry.Recorder
	release  func()
	started  time.Time
	finished bool

	durableTransitions uint64
	observationWrites  uint64
}

// Commit records the explicit commit attempt even if the server reports a
// rollback status or the outcome is unknown. Deferred Rollback must not turn a
// failed commit into an asserted rollback. Pool release discards non-idle/broken
// connections according to pgxpool's connection contract.
func (t *measuredTx) Commit(ctx context.Context) error {
	if t.finished {
		return pgx.ErrTxClosed
	}
	t.finished = true
	defer t.complete()
	started := time.Now()
	err := t.Tx.Commit(ctx)
	t.recorder.Observe(telemetry.DBCommitNS, elapsedNS(started))
	if err != nil {
		t.recorder.Add(telemetry.DBCommitErrors, 1)
	} else {
		t.recorder.Add(telemetry.DBTransactionsCommitted, 1)
		t.recorder.Add(telemetry.DBDurableTransitions, t.durableTransitions)
		t.recorder.Add(telemetry.DBObservationWrites, t.observationWrites)
	}
	return err
}

func (t *measuredTx) Rollback(ctx context.Context) error {
	if t.finished {
		return pgx.ErrTxClosed
	}
	t.finished = true
	defer t.complete()
	started := time.Now()
	err := t.Tx.Rollback(ctx)
	t.recorder.Observe(telemetry.DBRollbackNS, elapsedNS(started))
	if err != nil {
		t.recorder.Add(telemetry.DBRollbackErrors, 1)
	} else {
		t.recorder.Add(telemetry.DBTransactionsRolledBack, 1)
	}
	return err
}

func (t *measuredTx) complete() {
	t.recorder.Observe(telemetry.DBTransactionNS, elapsedNS(t.started))
	t.recorder.Adjust(telemetry.DBTransactionsInFlight, -1)
	t.release()
}

func txRecorder(tx pgx.Tx) *telemetry.Recorder {
	if measured, ok := tx.(*measuredTx); ok {
		return measured.recorder
	}
	return nil
}

// These counters acknowledge committed journal transitions and observation
// inserts only. Payload byte distributions instead measure attempted SQL writes.
func countOnCommit(tx pgx.Tx, metric telemetry.Counter) {
	if measured, ok := tx.(*measuredTx); ok {
		switch metric {
		case telemetry.DBDurableTransitions:
			measured.durableTransitions++
		case telemetry.DBObservationWrites:
			measured.observationWrites++
		}
	}
}

package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var replayInput = raw(`{"schema":"test-replay-v1","estimate_only":true}`)
var replayResult = raw(`{"schema":"test-result-v1","estimate_only":true,"cost":"0.01"}`)

func enqueueReplay(t *testing.T, s *Store, key Key, id string) ReplayJob {
	t.Helper()
	job, duplicate, err := s.EnqueueReplay(context.Background(), key, id, replayInput, ReplayAdmission{PerAccount: 16, Global: 32})
	if err != nil || duplicate || job.Status != "queued" || !bytes.Equal(job.Input, replayInput) {
		t.Fatalf("enqueue: job=%+v duplicate=%v err=%v", job, duplicate, err)
	}
	return job
}

func expireReplay(t *testing.T, s *Store, key Key, id string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), "UPDATE replay_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND account_id=$2 AND job_id=$3 AND status='running'", key.TenantID, key.AccountID, id); err != nil {
		t.Fatal(err)
	}
}

func TestReplayJobsExactIdentityAndFinancialIsolation(t *testing.T) {
	s := testStore(t)
	key, _, before := initialized(t, s)
	ctx := context.Background()
	first := enqueueReplay(t, s, key, "job-1")
	sum := sha256.Sum256(replayInput)
	if first.InputSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("input hash differs")
	}
	duplicate, seen, err := s.EnqueueReplay(ctx, key, first.ID, replayInput, ReplayAdmission{1, 1})
	if err != nil || !seen || duplicate.Sequence != first.Sequence || !duplicate.CreatedAt.Equal(first.CreatedAt) {
		t.Fatal("accepted retry did not precede full-capacity check", err)
	}
	if _, _, err := s.EnqueueReplay(ctx, key, first.ID, raw(`{"changed":true}`), ReplayAdmission{1, 1}); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting identity accepted", err)
	}
	claimed, err := s.ClaimReplay(ctx, "worker-1", time.Minute)
	if err != nil || claimed.ID != first.ID || claimed.Status != "running" || claimed.Attempts != 1 || claimed.LeaseUntil == nil {
		t.Fatal("claim failed", err)
	}
	finished, err := s.FinishReplay(ctx, key, first.ID, "worker-1", 1, replayResult, "")
	if err != nil || finished.Status != "succeeded" || !bytes.Equal(finished.Result, replayResult) || finished.LeaseUntil != nil {
		t.Fatal("finish failed", err)
	}
	if retry, err := s.FinishReplay(ctx, key, first.ID, "worker-1", 1, replayResult, ""); err != nil || retry.UpdatedAt != finished.UpdatedAt {
		t.Fatal("exact terminal finish retry mutated result", err)
	}
	if _, err := s.FinishReplay(ctx, key, first.ID, "worker-1", 1, raw(`{"different":true}`), ""); !errors.Is(err, ErrConflict) {
		t.Fatal("terminal result replaced", err)
	}
	if cancelled, err := s.CancelReplay(ctx, key, first.ID); err != nil || cancelled.Status != "succeeded" || !bytes.Equal(cancelled.Result, replayResult) {
		t.Fatal("cancel overwrote completed result", err)
	}
	latest, err := s.Replay(ctx, key, first.ID)
	if err != nil || latest.InputSHA256 != first.InputSHA256 || latest.ResultSHA256 == "" {
		t.Fatal("job history lost hash", err)
	}
	page, err := s.Replays(ctx, key, 0, 1)
	if err != nil || len(page) != 1 || len(page[0].Input) != 0 || len(page[0].Result) != 0 || page[0].ResultSHA256 != latest.ResultSHA256 {
		t.Fatal("metadata list exposed payloads or lost hashes", err)
	}
	if page, err := s.Replays(ctx, key, first.Sequence, 1); err != nil || len(page) != 0 {
		t.Fatal("exclusive sequence pagination broken", err)
	}
	after := mustAccount(t, s, key)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("analytical job mutated financial account authority")
	}
	if _, err := s.Replay(ctx, Key{"other-tenant", key.AccountID}, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-tenant job visible", err)
	}
}

func TestReplayJobAttemptFencesExpiredAndCancelledWorkers(t *testing.T) {
	s := testStore(t)
	key, _, _ := initialized(t, s)
	ctx := context.Background()
	enqueueReplay(t, s, key, "fenced")
	first, err := s.ClaimReplay(ctx, "same-worker-id", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	expireReplay(t, s, key, first.ID)
	if _, err := s.FinishReplay(ctx, key, first.ID, first.WorkerID, first.Attempts, replayResult, ""); !errors.Is(err, ErrOwnership) {
		t.Fatal("expired worker published", err)
	}
	second, err := s.ClaimReplay(ctx, "same-worker-id", time.Minute)
	if err != nil || second.Attempts != 2 {
		t.Fatal("expired job not reclaimed", err)
	}
	if _, err := s.FinishReplay(ctx, key, first.ID, first.WorkerID, first.Attempts, replayResult, ""); !errors.Is(err, ErrOwnership) {
		t.Fatal("same worker name bypassed attempt fence", err)
	}
	cancelled, err := s.CancelReplay(ctx, key, second.ID)
	if err != nil || cancelled.Status != "cancelled" {
		t.Fatal("cancel failed", err)
	}
	if _, err := s.FinishReplay(ctx, key, second.ID, second.WorkerID, second.Attempts, replayResult, ""); !errors.Is(err, ErrOwnership) {
		t.Fatal("cancelled worker published", err)
	}
	if retry, err := s.CancelReplay(ctx, key, second.ID); err != nil || !retry.UpdatedAt.Equal(cancelled.UpdatedAt) {
		t.Fatal("cancel retry changed terminal job", err)
	}
	if _, err := s.ClaimReplay(ctx, "worker-3", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatal("terminal job was claimed", err)
	}
	enqueueReplay(t, s, key, "queued-cancel")
	if job, err := s.CancelReplay(ctx, key, "queued-cancel"); err != nil || job.Attempts != 0 || job.Status != "cancelled" {
		t.Fatal("queued cancellation failed", err)
	}
}

func TestReplayJobLostWorkerAttemptBoundAndFailureIdentity(t *testing.T) {
	s := testStore(t)
	key, _, _ := initialized(t, s)
	ctx := context.Background()
	enqueueReplay(t, s, key, "lost")
	for attempt := 1; attempt <= ReplayMaxAttempts; attempt++ {
		job, err := s.ClaimReplay(ctx, fmt.Sprintf("worker-%d", attempt), time.Minute)
		if err != nil || job.Status != "running" || job.Attempts != attempt {
			t.Fatal("claim generation mismatch", err)
		}
		expireReplay(t, s, key, job.ID)
	}
	job, err := s.ClaimReplay(ctx, "replacement", time.Minute)
	if err != nil || job.Status != "failed" || job.ErrorCode != "worker_lost" || job.Attempts != 3 {
		t.Fatal("exhausted job remained pending", err)
	}
	if _, err := s.ClaimReplay(ctx, "replacement", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatal("exhausted job reclaimed", err)
	}
	enqueueReplay(t, s, key, "failure")
	job, err = s.ClaimReplay(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := s.FinishReplay(ctx, key, job.ID, "worker", job.Attempts, nil, "archive_unavailable")
	if err != nil || failed.Status != "failed" || failed.ErrorCode != "archive_unavailable" || len(failed.Result) != 0 {
		t.Fatal("fixed failure not retained", err)
	}
	if retry, err := s.FinishReplay(ctx, key, job.ID, "worker", job.Attempts, nil, "archive_unavailable"); err != nil || !retry.UpdatedAt.Equal(failed.UpdatedAt) {
		t.Fatal("fixed failure retry not idempotent", err)
	}
	if _, err := s.FinishReplay(ctx, key, job.ID, "worker", job.Attempts, nil, "execution_failed"); !errors.Is(err, ErrConflict) {
		t.Fatal("terminal failure changed", err)
	}
}

func TestReplayAdmissionIsAtomicAcrossConcurrentRequests(t *testing.T) {
	s := testStore(t)
	key, _, _ := initialized(t, s)
	ctx := context.Background()
	var mu sync.Mutex
	success, busy := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := s.EnqueueReplay(ctx, key, fmt.Sprintf("job-%02d", i), replayInput, ReplayAdmission{4, 6})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				success++
			} else if errors.Is(err, ErrReplayBusy) {
				busy++
			} else {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if success != 4 || busy != 8 {
		t.Fatalf("pending capacity over/under-admitted: accepted=%d busy=%d", success, busy)
	}
	other := Key{key.TenantID, "other-account"}
	if _, err := s.CreateAccount(ctx, other, []Balance{{Currency: "USD", Total: amount("1"), Free: amount("1")}}, raw(`{}`)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, _, err := s.EnqueueReplay(ctx, other, fmt.Sprintf("other-%d", i), replayInput, ReplayAdmission{4, 6}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.EnqueueReplay(ctx, other, "over-global", replayInput, ReplayAdmission{4, 6}); !errors.Is(err, ErrReplayBusy) {
		t.Fatal("global queue cap exceeded", err)
	}
	page, err := s.Replays(ctx, key, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelReplay(ctx, key, page[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnqueueReplay(ctx, other, "after-cancel", replayInput, ReplayAdmission{4, 6}); err != nil {
		t.Fatal("cancel did not free pending capacity", err)
	}
}

func TestReplayAdmissionOverridesRepeatableReadDefault(t *testing.T) {
	s := testStore(t)
	key, _, _ := initialized(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	capacity := ReplayAdmission{PerAccount: 2, Global: 2}
	// Establish the singleton before contenders take their snapshots. Otherwise
	// a concurrent first insertion can fail at RR and conceal the stale count.
	if _, _, err := s.EnqueueReplay(ctx, key, "seed", replayInput, capacity); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelReplay(ctx, key, "seed"); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := s.pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	const contenders = 6
	config := s.pool.Config()
	config.MaxConns = contenders + 2 // every waiter, the lock holder, and observer
	config.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	application := "replay_rr_" + schema
	config.ConnConfig.RuntimeParams["application_name"] = application
	// Config retains the source pool's AfterConnect schema selection.
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var isolation string
	if err := pool.QueryRow(ctx, "SHOW transaction_isolation").Scan(&isolation); err != nil || isolation != "repeatable read" {
		t.Fatalf("connection isolation=%q err=%v", isolation, err)
	}
	queue := &Store{pool: pool}
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema(), 904216))"); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, contenders)
	var workers sync.WaitGroup
	for i := 0; i < contenders; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			_, _, err := queue.EnqueueReplay(ctx, key, fmt.Sprintf("rr-%d", i), replayInput, capacity)
			results <- err
		}(i)
	}
	defer func() {
		cancel()
		workers.Wait()
	}()
	// Wait for actual advisory lock contention, not elapsed time. With the old
	// inherited RR transactions, all snapshots now predate every contender's
	// insert, so releasing this gate deterministically exercises stale counts.
	gate, stopGate := context.WithTimeout(ctx, 5*time.Second)
	defer stopGate()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		if err := pool.QueryRow(gate, "SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND wait_event='advisory'", application).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == contenders {
			break
		}
		select {
		case <-gate.Done():
			t.Fatalf("only %d/%d enqueue requests reached the advisory gate", waiting, contenders)
		case <-ticker.C:
		}
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	accepted, busy := 0, 0
	for i := 0; i < contenders; i++ {
		select {
		case err := <-results:
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, ErrReplayBusy):
				busy++
			default:
				t.Errorf("enqueue failed instead of observing current admission capacity: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("enqueue did not leave the advisory gate", ctx.Err())
		}
	}
	if accepted != capacity.Global || busy != contenders-capacity.Global {
		t.Fatalf("RR connection default bypassed capacity: accepted=%d busy=%d limit=%d", accepted, busy, capacity.Global)
	}
}

func TestReplayClaimsAreExclusiveAndSkipLocked(t *testing.T) {
	s := testStore(t)
	key, _, _ := initialized(t, s)
	ctx := context.Background()
	first := enqueueReplay(t, s, key, "first")
	enqueueReplay(t, s, key, "second")
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT sequence FROM replay_jobs WHERE sequence=$1 FOR UPDATE", first.Sequence); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimReplay(ctx, "worker", time.Minute)
	if err != nil || job.ID != "second" {
		t.Fatal("locked oldest job blocked unrelated replay", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	job, err = s.ClaimReplay(ctx, "worker-2", time.Minute)
	if err != nil || job.ID != "first" {
		t.Fatal("unlocked first job not available", err)
	}
	if _, err := s.ClaimReplay(ctx, "worker-3", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatal("running job concurrently claimed", err)
	}
}

func TestReplayStoreRejectsInvalidInputsAndSensitiveFailureText(t *testing.T) {
	s := testStore(t)
	key, _, _ := initialized(t, s)
	ctx := context.Background()
	for _, input := range [][]byte{nil, []byte("null"), []byte("[]"), []byte("{}{}"), bytes.Repeat([]byte("x"), ReplayMaxInputBytes+1)} {
		if _, _, err := s.EnqueueReplay(ctx, key, "bad", input, ReplayAdmission{4, 8}); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid replay input accepted", err)
		}
	}
	for _, cap := range []ReplayAdmission{{0, 1}, {1, 0}, {2, 1}, {65, 100}, {1, 1025}} {
		if _, _, err := s.EnqueueReplay(ctx, key, "bad", replayInput, cap); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid queue cap", err)
		}
	}
	for _, lease := range []time.Duration{0, time.Second - 1, time.Minute + 1} {
		if _, err := s.ClaimReplay(ctx, "worker", lease); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid lease", err)
		}
	}
	enqueueReplay(t, s, key, "sensitive")
	job, err := s.ClaimReplay(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishReplay(ctx, key, job.ID, "worker", job.Attempts, nil, "password=secret"); !errors.Is(err, ErrInvalid) {
		t.Fatal("arbitrary failure text persisted", err)
	}
	if _, err := s.FinishReplay(ctx, key, job.ID, "worker", job.Attempts, replayResult, "execution_failed"); !errors.Is(err, ErrInvalid) {
		t.Fatal("failure accepted a result", err)
	}
	if _, err := s.FinishReplay(ctx, key, job.ID, "worker", job.Attempts, bytes.Repeat([]byte("x"), ReplayMaxResultBytes+1), ""); !errors.Is(err, ErrInvalid) {
		t.Fatal("oversized result accepted", err)
	}
	if _, err := s.Replays(ctx, key, 0, 51); !errors.Is(err, ErrInvalid) {
		t.Fatal("unbounded history accepted", err)
	}
}

func TestReplayNamespaceAdmissionCannotBeSilentlyRaised(t *testing.T) {
	s := testStore(t)
	key, _, _ := initialized(t, s)
	ctx := context.Background()
	if _, _, err := s.EnqueueReplay(ctx, key, "original", replayInput, ReplayAdmission{1, 2}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnqueueReplay(ctx, key, "wider-process", replayInput, ReplayAdmission{64, 1024}); !errors.Is(err, ErrReplayConfig) {
		t.Fatal("another process silently raised durable limits", err)
	}
	if _, duplicate, err := s.EnqueueReplay(ctx, key, "original", replayInput, ReplayAdmission{64, 1024}); err != nil || !duplicate {
		t.Fatal("configuration mismatch obscured accepted command identity", err)
	}
}

func TestReplayJobSurvivesIndependentWorkerPoolRestart(t *testing.T) {
	s := testStore(t)
	key, _, _ := initialized(t, s)
	ctx := context.Background()
	enqueueReplay(t, s, key, "restart")
	var schema string
	if err := s.pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	worker, err := OpenWithOptions(ctx, os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN"), schema, Options{MaxConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	first, err := worker.ClaimReplay(ctx, "worker-before-restart", time.Minute)
	worker.Close()
	if err != nil {
		t.Fatal(err)
	}
	expireReplay(t, s, key, first.ID)
	restarted, err := OpenWithOptions(ctx, os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN"), schema, Options{MaxConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	second, err := restarted.ClaimReplay(ctx, "worker-after-restart", time.Minute)
	if err != nil || second.Attempts != 2 || !bytes.Equal(second.Input, first.Input) || second.InputSHA256 != first.InputSHA256 {
		t.Fatal("new worker pool changed durable request", err)
	}
	if _, err := restarted.FinishReplay(ctx, key, second.ID, second.WorkerID, second.Attempts, replayResult, ""); err != nil {
		t.Fatal(err)
	}
	final, err := s.Replay(ctx, key, second.ID)
	if err != nil || final.Status != "succeeded" || !bytes.Equal(final.Result, replayResult) {
		t.Fatal("requester did not discover independently committed result", err)
	}
}

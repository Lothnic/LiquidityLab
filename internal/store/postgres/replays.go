package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const ReplayMaxInputBytes = 64 << 10
const ReplayMaxResultBytes = 16 << 20
const ReplayMaxAttempts = 3

var ErrReplayBusy = errors.New("bounded replay pending capacity exhausted")
var ErrReplayConfig = errors.New("replay admission differs from durable namespace configuration")

// ReplayJob is an analytical job, never a financial command or venue intent.
// Input and result retain byte-exact JSON and hashes. The service constructing
// Input must validate its versioned semantic schema before enqueueing it.
type ReplayJob struct {
	Key
	ID           string          `json:"id"`
	Sequence     int64           `json:"sequence"`
	Input        json.RawMessage `json:"input,omitempty"`
	InputSHA256  string          `json:"input_sha256"`
	Status       string          `json:"status"`
	Attempts     int             `json:"attempts"`
	WorkerID     string          `json:"-"`
	LeaseUntil   *time.Time      `json:"-"`
	Result       json.RawMessage `json:"result,omitempty"`
	ResultSHA256 string          `json:"result_sha256,omitempty"`
	ErrorCode    string          `json:"error_code,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

type ReplayAdmission struct {
	PerAccount int
	Global     int
}

const replayColumns = `tenant_id,account_id,job_id,sequence,input_json,input_digest,status,attempts,worker_id,lease_until,result_json,result_digest,error_code,created_at,updated_at`

type replayScanner interface{ Scan(...any) error }

func scanReplay(row replayScanner) (ReplayJob, error) {
	var job ReplayJob
	var input, result []byte
	err := row.Scan(&job.TenantID, &job.AccountID, &job.ID, &job.Sequence, &job.Input, &input, &job.Status, &job.Attempts, &job.WorkerID, &job.LeaseUntil, &job.Result, &result, &job.ErrorCode, &job.CreatedAt, &job.UpdatedAt)
	if err != nil {
		return ReplayJob{}, normalized(err)
	}
	job.InputSHA256, job.ResultSHA256 = hex.EncodeToString(input), hex.EncodeToString(result)
	return job, nil
}

func replayJSON(raw json.RawMessage, limit int) bool {
	b := bytes.TrimSpace(raw)
	return len(raw) > 0 && len(raw) <= limit && len(b) > 0 && b[0] == '{' && json.Valid(b)
}

// EnqueueReplay serializes admission across this schema. Identity lookup comes
// before capacity checks: retrying accepted work never consumes another slot.
// This lock is independent of account ownership/financial transition locks.
func (s *Store) EnqueueReplay(ctx context.Context, key Key, id string, input json.RawMessage, capacity ReplayAdmission) (ReplayJob, bool, error) {
	if !validKey(key) || !validID(id) || !replayJSON(input, ReplayMaxInputBytes) || capacity.PerAccount < 1 || capacity.PerAccount > 64 || capacity.Global < capacity.PerAccount || capacity.Global > 1024 {
		return ReplayJob{}, false, ErrInvalid
	}
	// The post-advisory-lock count must see preceding accepted enqueues even
	// when a deployment configures Repeatable Read as its connection default.
	tx, err := s.begin(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ReplayJob{}, false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema(), 904216))"); err != nil {
		return ReplayJob{}, false, err
	}
	existing, err := scanReplay(tx.QueryRow(ctx, "SELECT "+replayColumns+" FROM replay_jobs WHERE tenant_id=$1 AND account_id=$2 AND job_id=$3", key.TenantID, key.AccountID, id))
	if err == nil {
		sum := sha256.Sum256(input)
		if existing.InputSHA256 != hex.EncodeToString(sum[:]) || !bytes.Equal(existing.Input, input) {
			return ReplayJob{}, false, ErrConflict
		}
		return existing, true, tx.Commit(ctx)
	}
	if !errors.Is(err, ErrNotFound) {
		return ReplayJob{}, false, err
	}
	// Pin one policy per namespace. Another process cannot silently raise the
	// global or per-account bound by enqueueing with a different configuration.
	if _, err = tx.Exec(ctx, "INSERT INTO replay_admission(singleton,per_account,global_limit) VALUES(true,$1,$2) ON CONFLICT DO NOTHING", capacity.PerAccount, capacity.Global); err != nil {
		return ReplayJob{}, false, err
	}
	var configured ReplayAdmission
	if err = tx.QueryRow(ctx, "SELECT per_account,global_limit FROM replay_admission WHERE singleton=true").Scan(&configured.PerAccount, &configured.Global); err != nil {
		return ReplayJob{}, false, err
	}
	if configured != capacity {
		return ReplayJob{}, false, ErrReplayConfig
	}
	var global, account int
	if err = tx.QueryRow(ctx, "SELECT count(*),count(*) FILTER (WHERE tenant_id=$1 AND account_id=$2) FROM replay_jobs WHERE status IN ('queued','running')", key.TenantID, key.AccountID).Scan(&global, &account); err != nil {
		return ReplayJob{}, false, err
	}
	if global >= capacity.Global || account >= capacity.PerAccount {
		return ReplayJob{}, false, ErrReplayBusy
	}
	sum := sha256.Sum256(input)
	job, err := scanReplay(tx.QueryRow(ctx, "INSERT INTO replay_jobs(tenant_id,account_id,job_id,input_json,input_digest,status) VALUES($1,$2,$3,$4,$5,'queued') RETURNING "+replayColumns, key.TenantID, key.AccountID, id, []byte(input), sum[:]))
	if err != nil {
		return ReplayJob{}, false, err
	}
	return job, false, tx.Commit(ctx)
}

// ClaimReplay takes one oldest available job without waiting behind another
// worker's row lock. Expired third attempts become failed/worker_lost and are
// returned as terminal jobs; the caller must execute only status=running jobs.
// All lease comparisons use database time. Attempts fence results even when
// worker IDs are reused by a restarted process.
func (s *Store) ClaimReplay(ctx context.Context, worker string, lease time.Duration) (ReplayJob, error) {
	if !validID(worker) || lease < time.Second || lease > time.Minute {
		return ReplayJob{}, ErrInvalid
	}
	tx, err := s.begin(ctx, pgx.TxOptions{})
	if err != nil {
		return ReplayJob{}, err
	}
	defer tx.Rollback(ctx)
	job, err := scanReplay(tx.QueryRow(ctx, "SELECT "+replayColumns+" FROM replay_jobs WHERE status='queued' OR (status='running' AND lease_until<=clock_timestamp()) ORDER BY sequence FOR UPDATE SKIP LOCKED LIMIT 1"))
	if err != nil {
		return ReplayJob{}, err
	}
	if job.Attempts >= ReplayMaxAttempts {
		job, err = scanReplay(tx.QueryRow(ctx, "UPDATE replay_jobs SET status='failed',lease_until=NULL,error_code='worker_lost',updated_at=clock_timestamp() WHERE tenant_id=$1 AND account_id=$2 AND job_id=$3 RETURNING "+replayColumns, job.TenantID, job.AccountID, job.ID))
	} else {
		job, err = scanReplay(tx.QueryRow(ctx, "UPDATE replay_jobs SET status='running',attempts=attempts+1,worker_id=$4,lease_until=clock_timestamp()+$5::bigint*interval '1 microsecond',updated_at=clock_timestamp() WHERE tenant_id=$1 AND account_id=$2 AND job_id=$3 RETURNING "+replayColumns, job.TenantID, job.AccountID, job.ID, worker, lease.Microseconds()))
	}
	if err != nil {
		return ReplayJob{}, err
	}
	return job, tx.Commit(ctx)
}

func replayErrorCode(code string) bool {
	switch code {
	case "input_invalid", "archive_unavailable", "execution_failed", "deadline_exceeded", "worker_lost":
		return true
	}
	return false
}

// FinishReplay commits one result or one fixed failure code for the current
// unexpired attempt. An exact retry of an already committed terminal result is
// idempotent even after lease expiry; a different result never replaces it.
func (s *Store) FinishReplay(ctx context.Context, key Key, id, worker string, attempt int, result json.RawMessage, errorCode string) (ReplayJob, error) {
	if !validKey(key) || !validID(id) || !validID(worker) || attempt < 1 || attempt > ReplayMaxAttempts {
		return ReplayJob{}, ErrInvalid
	}
	success := errorCode == ""
	if success && !replayJSON(result, ReplayMaxResultBytes) || !success && (!replayErrorCode(errorCode) || len(result) != 0) {
		return ReplayJob{}, ErrInvalid
	}
	tx, err := s.begin(ctx, pgx.TxOptions{})
	if err != nil {
		return ReplayJob{}, err
	}
	defer tx.Rollback(ctx)
	job, err := scanReplay(tx.QueryRow(ctx, "SELECT "+replayColumns+" FROM replay_jobs WHERE tenant_id=$1 AND account_id=$2 AND job_id=$3 FOR UPDATE", key.TenantID, key.AccountID, id))
	if err != nil {
		return ReplayJob{}, err
	}
	if job.WorkerID != worker || job.Attempts != attempt {
		return ReplayJob{}, ErrOwnership
	}
	status := "failed"
	if success {
		status = "succeeded"
	}
	if job.Status == status {
		if !bytes.Equal(job.Result, result) || job.ErrorCode != errorCode {
			return ReplayJob{}, ErrConflict
		}
		return job, tx.Commit(ctx)
	}
	if job.Status != "running" {
		return ReplayJob{}, ErrOwnership
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return ReplayJob{}, err
	}
	if job.LeaseUntil == nil || !job.LeaseUntil.After(now) {
		return ReplayJob{}, ErrOwnership
	}
	var storedResult, storedDigest []byte
	if success {
		sum := sha256.Sum256(result)
		storedResult, storedDigest = []byte(result), sum[:]
	}
	job, err = scanReplay(tx.QueryRow(ctx, "UPDATE replay_jobs SET status=$4,lease_until=NULL,result_json=$5,result_digest=$6,error_code=$7,updated_at=clock_timestamp() WHERE tenant_id=$1 AND account_id=$2 AND job_id=$3 RETURNING "+replayColumns, key.TenantID, key.AccountID, id, status, storedResult, storedDigest, errorCode))
	if err != nil {
		return ReplayJob{}, err
	}
	return job, tx.Commit(ctx)
}

// CancelReplay makes accepted cancellation immediately authoritative for the
// job; a still-running calculation cannot publish afterward. Terminal results
// remain immutable. Repeated cancellation returns the existing terminal state.
func (s *Store) CancelReplay(ctx context.Context, key Key, id string) (ReplayJob, error) {
	if !validKey(key) || !validID(id) {
		return ReplayJob{}, ErrInvalid
	}
	tx, err := s.begin(ctx, pgx.TxOptions{})
	if err != nil {
		return ReplayJob{}, err
	}
	defer tx.Rollback(ctx)
	job, err := scanReplay(tx.QueryRow(ctx, "SELECT "+replayColumns+" FROM replay_jobs WHERE tenant_id=$1 AND account_id=$2 AND job_id=$3 FOR UPDATE", key.TenantID, key.AccountID, id))
	if err != nil {
		return ReplayJob{}, err
	}
	if job.Status == "queued" || job.Status == "running" {
		job, err = scanReplay(tx.QueryRow(ctx, "UPDATE replay_jobs SET status='cancelled',lease_until=NULL,updated_at=clock_timestamp() WHERE tenant_id=$1 AND account_id=$2 AND job_id=$3 RETURNING "+replayColumns, key.TenantID, key.AccountID, id))
		if err != nil {
			return ReplayJob{}, err
		}
	}
	return job, tx.Commit(ctx)
}

func (s *Store) Replay(ctx context.Context, key Key, id string) (ReplayJob, error) {
	if !validKey(key) || !validID(id) {
		return ReplayJob{}, ErrInvalid
	}
	return scanReplay(s.pool.QueryRow(ctx, "SELECT "+replayColumns+" FROM replay_jobs WHERE tenant_id=$1 AND account_id=$2 AND job_id=$3", key.TenantID, key.AccountID, id))
}

// Replays lists bounded metadata only. Neither potentially large result bodies
// nor source/spec input bodies are selected by this query. Sequence is a replay
// history cursor and never an account financial revision.
func (s *Store) Replays(ctx context.Context, key Key, before int64, limit int) ([]ReplayJob, error) {
	if !validKey(key) || before < 0 || limit < 1 || limit > 50 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, "SELECT tenant_id,account_id,job_id,sequence,NULL::bytea,input_digest,status,attempts,worker_id,lease_until,NULL::bytea,result_digest,error_code,created_at,updated_at FROM replay_jobs WHERE tenant_id=$1 AND account_id=$2 AND ($3::bigint=0 OR sequence<$3) ORDER BY sequence DESC LIMIT $4", key.TenantID, key.AccountID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []ReplayJob{}
	for rows.Next() {
		job, err := scanReplay(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

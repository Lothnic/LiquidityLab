// Package replayjobs runs durable, account-scoped historical comparisons outside
// financial actors. All scenarios are estimates on an operator-selected archive.
package replayjobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/liquiditylab/liquiditylab/internal/historicalreplay"
	"github.com/liquiditylab/liquiditylab/internal/marketarchive"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

const InputSchema = "liquiditylab.replay-job.input.v1"

type Input struct {
	Schema    string                `json:"schema"`
	ArchiveID string                `json:"archive_id"`
	Spec      historicalreplay.Spec `json:"spec"`
}

// Queue must use a separate bounded connection pool from financial execution.
// Its attempt leases authorize only publication of analytical results.
type Queue interface {
	EnqueueReplay(context.Context, pg.Key, string, json.RawMessage, pg.ReplayAdmission) (pg.ReplayJob, bool, error)
	ClaimReplay(context.Context, string, time.Duration) (pg.ReplayJob, error)
	FinishReplay(context.Context, pg.Key, string, string, int, json.RawMessage, string) (pg.ReplayJob, error)
	CancelReplay(context.Context, pg.Key, string) (pg.ReplayJob, error)
	Replay(context.Context, pg.Key, string) (pg.ReplayJob, error)
	Replays(context.Context, pg.Key, int64, int) ([]pg.ReplayJob, error)
}

type Config struct {
	Queue       Queue
	ArchiveRoot string
	WorkerID    string
	Admission   pg.ReplayAdmission
	Workers     int
	JobTimeout  time.Duration
}

type Service struct {
	c       Config
	running atomic.Bool
	execute func(context.Context, Input) (json.RawMessage, string)
}

func identifier(s string) bool {
	if len(s) < 1 || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func New(c Config) (*Service, error) {
	if c.Workers == 0 {
		c.Workers = 1
	}
	if c.JobTimeout == 0 {
		c.JobTimeout = 20 * time.Second
	}
	if c.Admission == (pg.ReplayAdmission{}) {
		c.Admission = pg.ReplayAdmission{PerAccount: 4, Global: 16}
	}
	if c.Queue == nil || !filepath.IsAbs(c.ArchiveRoot) || !identifier(c.WorkerID) || c.Workers < 1 || c.Workers > 4 || c.JobTimeout < time.Millisecond || c.JobTimeout > 30*time.Second || c.Admission.PerAccount < 1 || c.Admission.PerAccount > 64 || c.Admission.Global < c.Admission.PerAccount || c.Admission.Global > 1024 {
		return nil, pg.ErrInvalid
	}
	s := &Service{c: c}
	s.execute = s.compare
	return s, nil
}

func (s *Service) Enqueue(ctx context.Context, key pg.Key, id, archiveID string, raw json.RawMessage) (pg.ReplayJob, bool, error) {
	if !identifier(key.TenantID) || !identifier(key.AccountID) || !identifier(id) || !marketarchive.ValidID(archiveID) {
		return pg.ReplayJob{}, false, pg.ErrInvalid
	}
	spec, err := historicalreplay.ParseSpec(raw)
	if err != nil {
		return pg.ReplayJob{}, false, pg.ErrInvalid
	}
	// Identity is the resolved specification, independent of insignificant JSON
	// whitespace. The accepted canonical bytes never change on a retry.
	input, err := json.Marshal(Input{Schema: InputSchema, ArchiveID: archiveID, Spec: spec})
	if err != nil || len(input) > pg.ReplayMaxInputBytes {
		return pg.ReplayJob{}, false, pg.ErrInvalid
	}
	return s.c.Queue.EnqueueReplay(ctx, key, id, input, s.c.Admission)
}

func (s *Service) Get(ctx context.Context, key pg.Key, id string) (pg.ReplayJob, error) {
	return s.c.Queue.Replay(ctx, key, id)
}

func (s *Service) List(ctx context.Context, key pg.Key, before int64, limit int) ([]pg.ReplayJob, error) {
	return s.c.Queue.Replays(ctx, key, before, limit)
}

func (s *Service) Cancel(ctx context.Context, key pg.Key, id string) (pg.ReplayJob, error) {
	return s.c.Queue.CancelReplay(ctx, key, id)
}

func parseInput(raw json.RawMessage) (Input, error) {
	var wire struct {
		Schema    string          `json:"schema"`
		ArchiveID string          `json:"archive_id"`
		Spec      json.RawMessage `json:"spec"`
	}
	if len(raw) == 0 || len(raw) > pg.ReplayMaxInputBytes {
		return Input{}, pg.ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&wire); err != nil {
		return Input{}, pg.ErrInvalid
	}
	if d.Decode(new(any)) != io.EOF || wire.Schema != InputSchema || !marketarchive.ValidID(wire.ArchiveID) {
		return Input{}, pg.ErrInvalid
	}
	spec, err := historicalreplay.ParseSpec(wire.Spec)
	if err != nil {
		return Input{}, pg.ErrInvalid
	}
	input := Input{Schema: wire.Schema, ArchiveID: wire.ArchiveID, Spec: spec}
	canonical, err := json.Marshal(input)
	if err != nil || !bytes.Equal(raw, canonical) {
		return Input{}, pg.ErrInvalid
	}
	return input, nil
}

func (s *Service) compare(ctx context.Context, input Input) (json.RawMessage, string) {
	report, err := historicalreplay.Compare(ctx, s.c.ArchiveRoot, input.ArchiveID, input.Spec)
	if err != nil {
		if errors.Is(err, historicalreplay.ErrArchiveUnavailable) {
			return nil, "archive_unavailable"
		}
		return nil, "execution_failed"
	}
	b, err := historicalreplay.Encode(report)
	if err != nil {
		return nil, "execution_failed"
	}
	return b, ""
}

// Run owns at most Workers calculations in this process and joins them before
// returning. Cancellation stops calculations cooperatively; unfinished attempts
// remain leased for safe recovery by another worker. There is no account lease,
// order submission, or account command on this path. Database errors back off;
// they never cause a second calculation for the same local slot.
func (s *Service) Run(ctx context.Context) error {
	if ctx == nil || !s.running.CompareAndSwap(false, true) {
		return pg.ErrInvalid
	}
	defer s.running.Store(false)
	done := make(chan struct{}, s.c.Workers)
	for i := 0; i < s.c.Workers; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			s.loop(ctx)
		}()
	}
	for i := 0; i < s.c.Workers; i++ {
		<-done
	}
	return nil
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Service) loop(ctx context.Context) {
	for ctx.Err() == nil {
		op, cancel := context.WithTimeout(ctx, 2*time.Second)
		job, err := s.c.Queue.ClaimReplay(op, s.c.WorkerID, time.Minute)
		cancel()
		if err != nil {
			delay := time.Second
			if errors.Is(err, pg.ErrNotFound) {
				delay = 100 * time.Millisecond
			}
			if !wait(ctx, delay) {
				return
			}
			continue
		}
		if job.Status == "running" {
			s.process(ctx, job)
		}
	}
}

func (s *Service) process(ctx context.Context, job pg.ReplayJob) {
	work, cancel := context.WithTimeout(ctx, s.c.JobTimeout)
	defer cancel()
	// Remote cancellation/ownership changes cancel a calculation within a
	// polling interval plus a bounded database call. DB publication fencing is
	// authoritative even if the computation is between cancellation checks.
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		for wait(work, 250*time.Millisecond) {
			op, stop := context.WithTimeout(work, time.Second)
			current, err := s.c.Queue.Replay(op, job.Key, job.ID)
			stop()
			if err != nil || current.Status != "running" || current.Attempts != job.Attempts || current.WorkerID != job.WorkerID {
				cancel()
				return
			}
		}
	}()
	input, err := parseInput(job.Input)
	digest := sha256.Sum256(job.Input)
	if job.InputSHA256 != hex.EncodeToString(digest[:]) {
		err = pg.ErrInvalid
	}
	var result json.RawMessage
	code := "input_invalid"
	if err == nil {
		result, code = s.execute(work, input)
	}
	workErr := work.Err()
	cancel()
	<-watchDone
	if ctx.Err() != nil || errors.Is(workErr, context.Canceled) {
		return // shutdown/remote cancellation leaves durable state authoritative
	}
	if errors.Is(workErr, context.DeadlineExceeded) {
		result, code = nil, "deadline_exceeded"
	}
	// Only fixed failure codes can enter storage. Never retain a filesystem,
	// SQL, or worker error string in an account-visible failure response.
	switch code {
	case "", "input_invalid", "archive_unavailable", "execution_failed", "deadline_exceeded":
	default:
		result, code = nil, "execution_failed"
	}
	for attempt := 0; attempt < 2 && ctx.Err() == nil; attempt++ {
		op, stop := context.WithTimeout(ctx, 2*time.Second)
		_, err := s.c.Queue.FinishReplay(op, job.Key, job.ID, job.WorkerID, job.Attempts, result, code)
		stop()
		if err == nil || errors.Is(err, pg.ErrOwnership) || errors.Is(err, pg.ErrConflict) || errors.Is(err, pg.ErrInvalid) || errors.Is(err, pg.ErrNotFound) {
			return
		}
	}
}

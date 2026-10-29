package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	dbActivityContract     = "scoped-postgres-activity-v1"
	dbActivityMaxTicks     = 10000
	dbActivityMaxRows      = 50000
	dbActivityMaxBackends  = 128
	dbActivityMaxBytes     = 8 << 20
	dbActivityQueryTimeout = 200 * time.Millisecond
)

// Activity sampling diagnoses observed backend states, not elapsed service or
// CPU time. No query text, client addresses, credentials or arbitrary labels are
// selected. A generated application name scopes this benchmark's store only.
type dbActivityRow struct {
	PID                        int32
	Started                    time.Time
	Postmaster                 time.Time
	State, WaitType, WaitEvent *string
}
type dbActivityObservation struct {
	Backend  int    `json:"backend"`
	Category string `json:"category"`
}
type dbActivitySample struct {
	Index        int                     `json:"index"`
	IntendedNS   int64                   `json:"intended_ns"`
	AttemptNS    int64                   `json:"attempt_ns"`
	EndNS        int64                   `json:"end_ns"`
	Outcome      string                  `json:"outcome"`
	Observations []dbActivityObservation `json:"observations,omitempty"`
}
type dbActivityBackend struct {
	PID            int32     `json:"pid"`
	Started        time.Time `json:"backend_started_utc"`
	FirstNS        int64     `json:"first_observed_ns"`
	LastNS         int64     `json:"last_observed_ns"`
	Observations   int       `json:"observations"`
	Disappearances int       `json:"disappearances"`
}
type dbActivityReport struct {
	Contract            string              `json:"contract"`
	Status              string              `json:"status"`
	Complete            bool                `json:"complete"`
	Stopped             bool                `json:"stopped"`
	Started             time.Time           `json:"schedule_origin_utc"`
	EndedNS             int64               `json:"ended_ns"`
	StopRequestedNS     int64               `json:"stop_requested_ns"`
	IntervalNS          int64               `json:"interval_ns"`
	QueryTimeoutNS      int64               `json:"query_timeout_ns"`
	Postmaster          time.Time           `json:"postmaster_started_utc"`
	MaxStoreConnections int                 `json:"max_store_connections"`
	ObserverConnections int                 `json:"observer_connections"`
	Outcomes            map[string]int      `json:"sample_outcomes"`
	Categories          map[string]int      `json:"backend_observation_categories"`
	ObservationCount    int                 `json:"backend_observations"`
	Backends            []dbActivityBackend `json:"backends"`
	Samples             []dbActivitySample  `json:"samples"`
	Issues              []string            `json:"issues"`
	Limits              []string            `json:"interpretation_limits"`
}

func validateDBActivity(interval, duration time.Duration, pool int) error {
	if interval == 0 {
		return nil
	}
	if interval < 10*time.Millisecond || interval > time.Second || pool < 1 || pool > 64 || duration < 0 || duration > 4*time.Minute {
		return errors.New("database activity interval must be 10ms..1s with a bounded workload")
	}
	// Reserve a second of sampling capacity for the ordinary accepted-call drain.
	// An unexpectedly longer drain hits runtime caps and is explicitly incomplete.
	ticks := int64((duration+time.Second-1)/interval) + 1
	if ticks > dbActivityMaxTicks || ticks*int64(pool) > dbActivityMaxRows {
		return errors.New("database activity schedule exceeds 10000 ticks or 50000 backend observations including one-second drain allowance")
	}
	return nil
}

type dbActivityQuery func(context.Context) ([]dbActivityRow, error)
type dbActivity struct {
	report     *dbActivityReport
	query      dbActivityQuery
	interval   time.Duration
	origin     time.Time
	stop       chan struct{}
	done       chan struct{}
	once       sync.Once
	stopNS     atomic.Int64
	identities map[string]int
	present    map[int]bool
}

// prepareDBActivity uses the existing observer outside the measurement window.
// Start transfers exclusive use of that connection to the sampler until Stop
// returns. Ordinary SQL/financial observers must not use it concurrently.
func prepareDBActivity(ctx context.Context, conn *pgx.Conn, tag string, interval time.Duration, pool int) (*dbActivity, error) {
	if interval == 0 {
		return nil, nil
	}
	if !validDBActivityTag(tag) {
		return nil, errors.New("invalid database activity scope")
	}
	var enabled bool
	var postmaster time.Time
	if err := conn.QueryRow(ctx, `SELECT current_setting('track_activities')::boolean, pg_postmaster_start_time()`).Scan(&enabled, &postmaster); err != nil || !enabled {
		return nil, errors.New("database activity observation unavailable")
	}
	query := func(ctx context.Context) ([]dbActivityRow, error) {
		rows, err := conn.Query(ctx, `SELECT pid,backend_start,pg_postmaster_start_time(),state,wait_event_type,wait_event
FROM pg_stat_activity
WHERE application_name=$1 AND datname=current_database() AND usename=current_user
AND backend_type='client backend' AND pid<>pg_backend_pid()
ORDER BY pid LIMIT 65`, tag)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := make([]dbActivityRow, 0, pool)
		for rows.Next() {
			var row dbActivityRow
			if err := rows.Scan(&row.PID, &row.Started, &row.Postmaster, &row.State, &row.WaitType, &row.WaitEvent); err != nil {
				return nil, err
			}
			out = append(out, row)
		}
		return out, rows.Err()
	}
	s := newDBActivity(query, interval, pool, postmaster)
	return s, nil
}

func newDBActivity(query dbActivityQuery, interval time.Duration, pool int, postmaster time.Time) *dbActivity {
	s := &dbActivity{query: query, interval: interval, stop: make(chan struct{}), done: make(chan struct{}), identities: map[string]int{}, present: map[int]bool{}, report: &dbActivityReport{
		Contract: dbActivityContract, Status: "prepared", IntervalNS: int64(interval), QueryTimeoutNS: int64(dbActivityQueryTimeout), Postmaster: postmaster,
		MaxStoreConnections: pool, ObserverConnections: 1, Outcomes: map[string]int{}, Categories: map[string]int{}, Issues: []string{}, Backends: []dbActivityBackend{}, Samples: []dbActivitySample{},
		Limits: []string{
			"One existing observer connection; exact generated store application tag, same database/role, client backends only. No SQL text or client addresses selected.",
			"PID plus backend-start identifies a lifetime; connections existing entirely between samples are unseen. Missing metadata or empty samples make this diagnostic incomplete.",
			"Counts are backend-observation denominators, not request counts or elapsed wait/CPU durations. Active with no reported wait is unclassified; idle ClientRead is not contention.",
			"Snapshots can miss brief waits and different backends can wait concurrently. Query execution and sampling perturb the workload. Compare repeated observer-off/on runs.",
			"Sampling covers its own declared clock from immediately before offered measurement through accepted-call drain; setup/warmup and cleanup are excluded. No cumulative IO/WAL counters or server CPU are measured.",
			"Attempt/end and backend first/last observation offsets are client query bounds, not exact server transition timestamps. WAL IO identifies WalSync/WalWrite samples only; LWLock WALWrite remains a lightweight-lock category.",
			"10000 sample ticks, 50000 backend observations, 128 lifetime identities and 8 MiB serialized diagnostic cap; a cap or missed/failed sample makes evidence incomplete.",
		},
	}}
	s.stopNS.Store(-1)
	return s
}

func (s *dbActivity) issue(code string) {
	for _, v := range s.report.Issues {
		if v == code {
			return
		}
	}
	s.report.Issues = append(s.report.Issues, code)
}

func dbActivityCategory(row dbActivityRow) string {
	if row.State == nil {
		return "metadata_missing"
	}
	switch *row.State {
	case "idle":
		return "idle"
	case "idle in transaction", "idle in transaction (aborted)":
		return "idle_in_transaction"
	case "disabled":
		return "metadata_missing"
	case "active":
		if row.WaitType == nil {
			return "active_unclassified"
		}
		switch *row.WaitType {
		case "IO":
			if row.WaitEvent != nil && (*row.WaitEvent == "WalSync" || *row.WaitEvent == "WalWrite") {
				return "active_wal_io"
			}
			return "active_other_io"
		case "Lock":
			return "active_lock"
		case "LWLock":
			return "active_lwlock"
		case "Client":
			return "active_client"
		case "IPC":
			return "active_ipc"
		case "Timeout":
			return "active_timeout"
		case "BufferPin":
			return "active_buffer_pin"
		default:
			return "active_other_wait"
		}
	default:
		return "state_other"
	}
}

func (s *dbActivity) recordRows(sample *dbActivitySample, rows []dbActivityRow) bool {
	if len(rows) > s.report.MaxStoreConnections || s.report.ObservationCount+len(rows) > dbActivityMaxRows {
		sample.Outcome = "observation_cap"
		s.issue("observation_cap")
		return false
	}
	keys := make([]string, len(rows))
	unique := map[string]bool{}
	newCount := 0
	for i, row := range rows {
		if row.PID <= 0 || row.Started.IsZero() || !row.Postmaster.Equal(s.report.Postmaster) {
			sample.Outcome = "identity_unavailable"
			s.issue("identity_unavailable")
			return false
		}
		keys[i] = strconv.FormatInt(int64(row.PID), 10) + "/" + row.Started.UTC().Format(time.RFC3339Nano)
		if unique[keys[i]] {
			sample.Outcome = "identity_duplicate"
			s.issue("identity_duplicate")
			return false
		}
		unique[keys[i]] = true
		if _, ok := s.identities[keys[i]]; !ok {
			newCount++
		}
	}
	if len(s.identities)+newCount > dbActivityMaxBackends {
		sample.Outcome = "identity_cap"
		s.issue("identity_cap")
		return false
	}
	present := map[int]bool{}
	for i, row := range rows {
		index, ok := s.identities[keys[i]]
		if !ok {
			index = len(s.report.Backends)
			s.identities[keys[i]] = index
			s.report.Backends = append(s.report.Backends, dbActivityBackend{PID: row.PID, Started: row.Started, FirstNS: sample.AttemptNS})
		}
		backend := &s.report.Backends[index]
		backend.LastNS = sample.EndNS
		backend.Observations++
		present[index] = true
		category := dbActivityCategory(row)
		if category == "metadata_missing" || category == "state_other" {
			s.issue("metadata_unavailable")
		}
		sample.Observations = append(sample.Observations, dbActivityObservation{Backend: index, Category: category})
		s.report.Categories[category]++
		s.report.ObservationCount++
	}
	for index := range s.present {
		if !present[index] {
			s.report.Backends[index].Disappearances++
		}
	}
	s.present = present
	if len(rows) == 0 {
		sample.Outcome = "empty"
		s.issue("backend_missing")
	}
	return true
}

func (s *dbActivity) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.origin = time.Now()
	s.report.Started = s.origin.UTC()
	s.report.Status = "running"
	go s.run(ctx)
}

func (s *dbActivity) run(ctx context.Context) {
	defer close(s.done)
	defer func() { s.report.EndedNS = time.Since(s.origin).Nanoseconds(); s.report.Stopped = true }()
	var previousCallStart, previousCallEnd int64 = -1, -1
	for index := 0; index < dbActivityMaxTicks; index++ {
		intended := int64(index) * int64(s.interval)
		if cutoff := s.stopNS.Load(); cutoff >= 0 && intended >= cutoff {
			return
		}
		if wait := time.Duration(intended) - time.Since(s.origin); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-s.stop:
				timer.Stop()
				if cutoff := s.stopNS.Load(); intended >= cutoff {
					return
				}
			case <-ctx.Done():
				timer.Stop()
				s.issue("parent_cancelled")
				return
			}
		}
		select {
		case <-ctx.Done():
			s.issue("parent_cancelled")
			return
		default:
		}
		sample := dbActivitySample{Index: index, IntendedNS: intended, AttemptNS: time.Since(s.origin).Nanoseconds(), Outcome: "completed"}
		if previousCallStart <= intended && previousCallEnd > intended {
			sample.Outcome = "query_busy"
			sample.EndNS = sample.AttemptNS
			s.issue("missed_sample")
		} else if sample.AttemptNS-intended >= int64(s.interval) {
			sample.Outcome = "generator_late"
			sample.EndNS = sample.AttemptNS
			s.issue("missed_sample")
		} else if s.stopNS.Load() >= 0 {
			sample.Outcome = "stopped_before_query"
			sample.EndNS = sample.AttemptNS
			s.issue("missed_sample")
		} else {
			queryCtx, cancel := context.WithTimeout(ctx, dbActivityQueryTimeout)
			rows, err := s.query(queryCtx)
			cancel()
			sample.EndNS = time.Since(s.origin).Nanoseconds()
			previousCallStart, previousCallEnd = sample.AttemptNS, sample.EndNS
			if err != nil {
				sample.Outcome = "query_failed"
				if errors.Is(err, context.DeadlineExceeded) {
					sample.Outcome = "query_timeout"
				}
				s.issue("query_failed")
			} else if !s.recordRows(&sample, rows) {
				s.report.Samples = append(s.report.Samples, sample)
				s.report.Outcomes[sample.Outcome]++
				return
			}
		}
		s.report.Samples = append(s.report.Samples, sample)
		s.report.Outcomes[sample.Outcome]++
		if s.report.ObservationCount >= dbActivityMaxRows {
			s.issue("observation_cap")
			return
		}
	}
	s.issue("tick_cap")
}

// Stop lets an accepted query finish within its 200ms deadline and joins the
// sampler before returning the report/observer to the caller. It is idempotent.
func (s *dbActivity) Stop() *dbActivityReport {
	if s == nil {
		return nil
	}
	s.once.Do(func() { s.stopNS.Store(time.Since(s.origin).Nanoseconds()); close(s.stop) })
	<-s.done
	s.report.StopRequestedNS = s.stopNS.Load()
	if len(s.report.Samples) == 0 || s.report.ObservationCount == 0 {
		s.issue("no_observations")
	}
	s.report.Complete = len(s.report.Issues) == 0
	s.report.Status = "complete"
	if !s.report.Complete {
		s.report.Status = "incomplete"
	}
	data, err := json.MarshalIndent(s.report, "", "  ")
	if err != nil || len(data) > dbActivityMaxBytes {
		s.issue("serialized_cap")
		s.report.Status = "incomplete"
		s.report.Complete = false
		// Keep aggregate counts and identities, explicitly disclosing raw truncation.
		s.report.Samples = nil
	}
	return s.report
}

// Keep diagnostic tags restricted even when this private helper is used by
// an integration test; they are parameters, never interpolated SQL.
func validDBActivityTag(tag string) bool {
	return len(tag) > 0 && len(tag) <= 63 && strings.IndexFunc(tag, func(r rune) bool { return r < 33 || r > 126 }) < 0
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func activityString(s string) *string { return &s }
func activityRow(pid int32, start time.Time) dbActivityRow {
	return dbActivityRow{PID: pid, Started: start, Postmaster: time.Unix(1, 0), State: activityString("active")}
}

func TestDBActivityBoundsAndDisabledPath(t *testing.T) {
	if s, err := prepareDBActivity(context.Background(), nil, "", 0, 0); s != nil || err != nil {
		t.Fatal("disabled observation used a connection")
	}
	var disabled *dbActivity
	disabled.Start(context.Background())
	if disabled.Stop() != nil {
		t.Fatal("disabled observation emitted a report")
	}
	for _, tc := range []struct {
		interval, duration time.Duration
		pool               int
	}{{-1, time.Second, 1}, {time.Millisecond, time.Second, 1}, {2 * time.Second, time.Second, 1}, {10 * time.Millisecond, 4 * time.Minute, 1}, {10 * time.Millisecond, 10 * time.Second, 64}, {20 * time.Millisecond, time.Second, 65}} {
		if validateDBActivity(tc.interval, tc.duration, tc.pool) == nil {
			t.Fatal("unbounded activity configuration accepted")
		}
	}
	if validateDBActivity(20*time.Millisecond, 20*time.Second, 4) != nil {
		t.Fatal("ordinary diagnostic configuration rejected")
	}
	for _, tag := range []string{"", strings.Repeat("x", 64), "space in tag", "line\nfeed"} {
		if validDBActivityTag(tag) {
			t.Fatal("invalid scope tag admitted")
		}
	}
}

func TestDBActivityFixedCategoriesDoNotInferCPU(t *testing.T) {
	for _, tc := range []struct{ state, kind, event, want string }{{"active", "", "", "active_unclassified"}, {"idle", "Client", "ClientRead", "idle"}, {"active", "Lock", "transactionid", "active_lock"}, {"active", "IO", "WalSync", "active_wal_io"}, {"active", "IO", "WalWrite", "active_wal_io"}, {"active", "LWLock", "WALWrite", "active_lwlock"}, {"active", "IO", "DataFileRead", "active_other_io"}, {"active", "extension-private-text", "secret", "active_other_wait"}, {"disabled", "", "", "metadata_missing"}} {
		row := activityRow(1, time.Unix(2, 0))
		row.State = activityString(tc.state)
		if tc.kind != "" {
			row.WaitType = activityString(tc.kind)
			row.WaitEvent = activityString(tc.event)
		}
		if got := dbActivityCategory(row); got != tc.want {
			t.Fatalf("category=%s want=%s", got, tc.want)
		}
	}
}

func TestDBActivityLifetimeIdentityMissingMetadataAndCaps(t *testing.T) {
	s := newDBActivity(nil, 20*time.Millisecond, 2, time.Unix(1, 0))
	probe := func(rows ...dbActivityRow) dbActivitySample {
		p := dbActivitySample{Outcome: "completed", AttemptNS: 10, EndNS: 20}
		s.recordRows(&p, rows)
		return p
	}
	probe(activityRow(10, time.Unix(2, 0)))
	probe(activityRow(10, time.Unix(3, 0))) // reused PID is a new lifetime
	if len(s.report.Backends) != 2 || s.report.Backends[0].Disappearances != 1 || s.report.Backends[1].Observations != 1 {
		t.Fatal("PID reuse merged backend lifetimes")
	}
	if p := probe(); p.Outcome != "empty" || len(s.report.Issues) == 0 {
		t.Fatal("empty sample concealed missing coverage")
	}
	row := activityRow(11, time.Unix(3, 0))
	row.State = nil
	probe(row)
	if s.report.Categories["metadata_missing"] != 1 {
		t.Fatal("missing state concealed")
	}
	if p := probe(row, row); p.Outcome != "identity_duplicate" {
		t.Fatal("duplicate identity accepted")
	}
	row.Postmaster = time.Unix(4, 0)
	if p := probe(row); p.Outcome != "identity_unavailable" {
		t.Fatal("postmaster restart merged")
	}
	row = activityRow(12, time.Unix(3, 0))
	s.report.ObservationCount = dbActivityMaxRows
	if p := probe(row); p.Outcome != "observation_cap" {
		t.Fatal("observation cap exceeded")
	}
	s.report.ObservationCount = 0
	for i := len(s.identities); i < dbActivityMaxBackends; i++ {
		s.identities[strconv.Itoa(i)] = i
	}
	if p := probe(row); p.Outcome != "identity_cap" {
		t.Fatal("lifetime cap exceeded")
	}
}

func TestDBActivityBusyTimeoutAndStopAreExplicit(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	s := newDBActivity(func(ctx context.Context) ([]dbActivityRow, error) {
		calls.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-time.After(35 * time.Millisecond):
			return []dbActivityRow{activityRow(1, time.Unix(2, 0))}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, 10*time.Millisecond, 1, time.Unix(1, 0))
	s.Start(context.Background())
	<-entered
	time.Sleep(95 * time.Millisecond)
	r := s.Stop()
	if r.Complete || !r.Stopped || r.Outcomes["query_busy"] == 0 || calls.Load() > 4 || r.EndedNS > int64(time.Second) {
		t.Fatal("query latency hid offered samples or stop failed to join")
	}
	if len(r.Samples) != int((r.StopRequestedNS+r.IntervalNS-1)/r.IntervalNS) {
		t.Fatal("stop concealed ticks overlapping the final accepted query")
	}
	if s.Stop() != r {
		t.Fatal("stop not idempotent")
	}
	for i, p := range r.Samples {
		if p.Index != i || p.IntendedNS != int64(i)*int64(10*time.Millisecond) || p.AttemptNS < p.IntendedNS || p.EndNS < p.AttemptNS {
			t.Fatal("sample timing provenance incorrect")
		}
	}
	deadlineEntered := make(chan struct{})
	deadline := newDBActivity(func(ctx context.Context) ([]dbActivityRow, error) {
		close(deadlineEntered)
		<-ctx.Done()
		return nil, ctx.Err()
	}, 10*time.Millisecond, 1, time.Unix(1, 0))
	deadline.Start(context.Background())
	<-deadlineEntered
	if r := deadline.Stop(); r.Complete || !r.Stopped || r.Outcomes["query_timeout"] != 1 {
		t.Fatal("accepted query deadline hidden by stop")
	}
}

func TestDBActivityTickCapPreservesMissedSlots(t *testing.T) {
	// A deliberately impossible private test interval reaches the cap without
	// issuing catch-up queries; public configuration rejects this interval.
	s := newDBActivity(func(context.Context) ([]dbActivityRow, error) { return nil, nil }, time.Nanosecond, 1, time.Unix(1, 0))
	s.Start(context.Background())
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		t.Fatal("sampler did not stop at tick cap")
	}
	r := s.Stop()
	if r.Complete || len(r.Samples) != dbActivityMaxTicks || r.Outcomes["generator_late"] == 0 || !strings.Contains(strings.Join(r.Issues, ","), "tick_cap") {
		t.Fatal("tick cap concealed skipped offers")
	}
}

func TestDBActivityFailureSanitizationAndSerializedCap(t *testing.T) {
	s := newDBActivity(func(context.Context) ([]dbActivityRow, error) {
		return nil, errors.New("postgres://credential-secret SELECT private_sql")
	}, 10*time.Millisecond, 1, time.Unix(1, 0))
	s.Start(context.Background())
	time.Sleep(15 * time.Millisecond)
	r := s.Stop()
	raw, _ := json.Marshal(r)
	if r.Complete || r.Outcomes["query_failed"] == 0 || strings.Contains(string(raw), "credential-secret") || strings.Contains(string(raw), "private_sql") {
		t.Fatal("failed sample looked complete or leaked source error")
	}
	s.report.Samples = make([]dbActivitySample, dbActivityMaxTicks)
	for i := range s.report.Samples {
		s.report.Samples[i].Observations = make([]dbActivityObservation, 10)
	}
	if r := s.Stop(); r.Complete || r.Samples != nil || !strings.Contains(strings.Join(r.Issues, ","), "serialized_cap") {
		t.Fatal("oversized diagnostic not explicitly truncated")
	}
}

func TestDBActivityPostgresObservesOnlyOwnedLockWait(t *testing.T) {
	dsn := os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set explicit development PostgreSQL DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tag := "llbench-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	connect := func(name string) *pgx.Conn {
		t.Helper()
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatal("parse development DSN")
		}
		cfg.RuntimeParams["application_name"] = name
		c, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatal("connect development database")
		}
		t.Cleanup(func() { c.Close(context.Background()) })
		return c
	}
	holder, waiter, other, observer := connect(tag), connect(tag), connect(tag+"-other"), connect(tag+"-observer")
	// Advisory locks create a controlled owned waiter without schema mutation.
	lock := time.Now().UnixNano()
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock($1)", lock); err != nil {
		t.Fatal(err)
	}
	defer holder.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lock)
	done := make(chan error, 1)
	go func() { _, err := waiter.Exec(ctx, "SELECT pg_advisory_lock($1)", lock); done <- err }()
	s, err := prepareDBActivity(ctx, observer, tag, 20*time.Millisecond, 2)
	if err != nil {
		t.Fatal(err)
	}
	s.Start(ctx)
	defer s.Stop()
	var blocked bool
	until := time.Now().Add(2 * time.Second)
	// This probe uses an unrelated connection, never the sampler-owned observer.
	for time.Now().Before(until) {
		if err := other.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')", waiter.PgConn().PID()).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !blocked {
		t.Fatal("owned waiter never reached server lock")
	}
	time.Sleep(65 * time.Millisecond)
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", lock); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := waiter.Exec(ctx, "SELECT pg_advisory_unlock($1)", lock); err != nil {
		t.Fatal(err)
	}
	time.Sleep(65 * time.Millisecond)
	r := s.Stop()
	if !r.Complete || r.Categories["active_lock"] == 0 || len(r.Backends) != 2 || !r.Stopped {
		t.Fatalf("scoped lock observation incomplete: issues=%v categories=%v backends=%d", r.Issues, r.Categories, len(r.Backends))
	}
	for _, b := range r.Backends {
		if uint32(b.PID) != holder.PgConn().PID() && uint32(b.PID) != waiter.PgConn().PID() {
			t.Fatal("foreign or observer backend was included")
		}
	}
	last := r.Samples[len(r.Samples)-1]
	for _, o := range last.Observations {
		if o.Category == "active_lock" {
			t.Fatal("released lock still observed")
		}
	}
	if _, err := observer.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal("stop did not return observer connection for ordinary audit use")
	}
}

func TestDBActivityPostgresWorkloadOptIn(t *testing.T) {
	if os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN") == "" {
		t.Skip("set explicit development PostgreSQL DSN")
	}
	c := testConfig()
	c.Schema = "ll_bench_activity_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	c.Output = filepath.Join(t.TempDir(), "report.json")
	c.MarketRate, c.ReadRate = 10, 0
	c.Duration = 300 * time.Millisecond
	c.Queue = 64
	c.DBActivityInterval = 20 * time.Millisecond
	if err := execute(c); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(c.Output)
	if err != nil {
		t.Fatal(err)
	}
	var r report
	if err = json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if !r.Valid || r.DBActivity == nil || !r.DBActivity.Complete || r.DBActivity.Contract != dbActivityContract || r.Validation["postgres_activity"] != dbActivityContract || r.DBActivity.ObserverConnections != 1 || r.DBActivity.ObservationCount == 0 {
		t.Fatal("opt-in observation missing or incomplete")
	}
	if r.DBActivity.Started.After(r.Measurement.Started) || r.DBActivity.Started.Add(time.Duration(r.DBActivity.EndedNS)).Before(r.Measurement.Started.Add(time.Duration(r.Measurement.ElapsedNS))) {
		t.Fatal("observation schedule does not enclose offered measurement and drain")
	}
	// Retain this fresh development schema/account; no reset or drop.
}

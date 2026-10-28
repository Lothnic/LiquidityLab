package main

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	ar "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
	"github.com/liquiditylab/liquiditylab/internal/telemetry"
)

var limitations = []string{
	"One synthetic paper account and one instrument; no exchange, React render, recovery restart, or supported-capacity claim. HTTP polling and inference are explicit optional workload variants.",
	"Input identities and values are seed deterministic. Wall-clock decisions, admission, source/order binding, and resulting economics can vary between repetitions.",
	"Open-loop offered scheduling is independent of completions. The ordered source worker is serial; actor call durations are service-call components with queue/persistence work, not a separate closed-loop maximum-throughput run.",
	"runner_admitted_ns is bounded runner queue/slot admission; actor admission happens between service_call_started_ns and service_call_returned_ns and has no exact per-call public timestamp. A successful return acknowledges completion; rejected and uncertain returns never count as completed.",
	"All sample offsets use the phase's local monotonic start. generator_attempt_ns minus intended_ns is generator lag; completed minus intended is offered-to-response latency; completed minus call started is service call latency; HTTP read calls include network/API/body/JSON work, while market calls remain actor methods.",
	"Warmup drains before the measurement schedule starts. Telemetry snapshots are cumulative before/after measurement (including drain); subtract counts, sums and buckets for interval data. Snapshot min/max and quantiles are cumulative and cannot be subtracted.",
	"Database WAL delta is cluster-wide and may contain concurrent activity. Database size and schema relation bytes include PostgreSQL storage behavior and retained prior benchmark accounts; relation statistics may lag.",
	"Source loss or any failed source call invalidates the workload even when subsequent calls complete. No lost fact is silently treated as harmless coalescing.",
	"Resource deltas include the generator, JSON manifest hashing, observer queries, runner samples, and actor. RSS/heap changes are endpoint differences; peak RSS is process lifetime high-water mark; GC CPU fraction is cumulative.",
	"No restart/restore recovery duration, recording write lag, private-event age, or pure lock-only duration is measured. An explicit separate Go block profile records cumulative completed Go synchronization blocking; it does not measure PostgreSQL fsync or external worker blocking. Inference RPC latency is measured only for an explicitly selected worker. Source debt is the final durable observed-minus-processed cursor count; local cleanup latency is not exchange cancellation latency.",
}

type dbState struct {
	WALLSN        string `json:"wal_lsn"`
	SchemaBytes   int64  `json:"schema_relation_bytes"`
	DatabaseBytes int64  `json:"database_bytes"`
	Available     bool   `json:"available"`
}
type report struct {
	Validation         map[string]string  `json:"validation_contracts"`
	Policy             policyReport       `json:"policy_worker"`
	Profile            profileReport      `json:"profiling"`
	QuiescentTelemetry telemetry.Snapshot `json:"telemetry_after_actor_shutdown"`
	QuiescentPool      pg.PoolStatistics  `json:"pool_after_actor_shutdown"`
	FinancialAudit     financialAudit     `json:"financial_conservation"`
	Failure            string             `json:"failure,omitempty"`
	Version            string             `json:"report_version"`
	Config             config             `json:"workload"`
	Started            time.Time          `json:"started_utc"`
	Status             string             `json:"status"`
	Valid              bool               `json:"valid"`
	Host               map[string]any     `json:"host"`
	Database           map[string]any     `json:"database"`
	Account            pg.Key             `json:"account"`
	InitialBalances    []pg.Balance       `json:"initial_balances"`
	ActorConfig        map[string]any     `json:"actor_configuration"`
	Plan               d.Plan             `json:"plan"`
	Warmup             phase              `json:"warmup"`
	Measurement        phase              `json:"measurement"`
	BeforeTelemetry    telemetry.Snapshot `json:"telemetry_before_measurement"`
	AfterTelemetry     telemetry.Snapshot `json:"telemetry_after_measurement"`
	BeforePool         pg.PoolStatistics  `json:"pool_before_measurement"`
	AfterPool          pg.PoolStatistics  `json:"pool_after_measurement"`
	BeforeResources    resources          `json:"resources_before_measurement"`
	AfterResources     resources          `json:"resources_after_measurement"`
	ResourceDelta      map[string]any     `json:"resource_deltas"`
	BeforeDB           dbState            `json:"database_before_measurement"`
	AfterDB            dbState            `json:"database_after_measurement"`
	ClusterWALBytes    string             `json:"cluster_wide_wal_delta_bytes,omitempty"`
	Final              ar.View            `json:"exact_final_view"`
	SourceDebt         int64              `json:"final_source_recovery_debt_count"`
	Cleanup            string             `json:"cleanup"`
	ControlCleanupNS   int64              `json:"cleanup_control_ack_ns"`
	Limitations        []string           `json:"measurement_boundaries"`
}

func sortInputs(items []input) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].IntendedNS < items[j].IntendedNS })
}
func hostInfo() map[string]any {
	git := func(args ...string) string {
		b, e := exec.Command("git", args...).Output()
		if e != nil {
			return "unavailable"
		}
		return strings.TrimSpace(string(b))
	}
	revision := git("rev-parse", "HEAD")
	dirtyOutput := git("status", "--porcelain")
	var dirty any
	if dirtyOutput != "unavailable" {
		dirty = dirtyOutput != ""
	}
	launchStatus := "available"
	if revision == "unavailable" {
		launchStatus = "unavailable"
	}
	h := map[string]any{"binary_build": currentBuildIdentity(), "executable": currentExecutableIdentity(), "launch_worktree_status": launchStatus, "go_version": runtime.Version(), "os": runtime.GOOS, "architecture": runtime.GOARCH, "logical_cpus": runtime.NumCPU(), "gomaxprocs": runtime.GOMAXPROCS(0), "launch_worktree_revision": revision, "launch_worktree_dirty": dirty, "engine_version": d.EnvironmentVersion, "state_version": ar.StateVersion, "report_source": "benchmarks/runtime", "process_page_bytes": os.Getpagesize()}
	if b, e := os.ReadFile("/proc/cpuinfo"); e == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "model name") {
				h["cpu_model"] = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
				break
			}
		}
	}
	if b, e := os.ReadFile("/proc/meminfo"); e == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				h["host_memory_total"] = strings.TrimSpace(strings.TrimPrefix(line, "MemTotal:"))
				break
			}
		}
	}
	for _, path := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/cpu.max"} {
		if b, e := os.ReadFile(path); e == nil {
			h[path] = strings.TrimSpace(string(b))
		}
	}
	return h
}
func cfgInfo(c ar.Config) map[string]any {
	bundle := "none; Go baseline without inference"
	if c.Policy != nil {
		bundle = c.Policy.PolicySHA256
	}
	return map[string]any{"profile": c.Profile, "data_source": c.DataSource, "accounts": 1, "instruments": 1, "instrument_revision": c.InstrumentRevision, "rules": c.Rules, "fees": c.Fees, "execution": c.Execution, "baseline": c.Baseline, "scheduled_slice_interval_ns": x.BaselineSliceInterval, "bundle_identity": bundle, "auto_execute": c.AutoExecute, "mailbox_capacity": c.MailboxCapacity, "control_capacity": c.ControlCapacity, "fact_capacity": c.FactCapacity, "max_fills": c.MaxFills, "max_facts": c.MaxFacts, "lease_ns": int64(c.Lease)}
}
func resourceDelta(a, b resources) map[string]any {
	return map[string]any{"user_cpu_us": b.UserCPUUS - a.UserCPUUS, "system_cpu_us": b.SystemCPUUS - a.SystemCPUUS, "total_alloc_bytes": b.TotalAlloc - a.TotalAlloc, "mallocs": b.Mallocs - a.Mallocs, "gc_cycles": b.NumGC - a.NumGC, "gc_pause_ns": b.GCPauseNS - a.GCPauseNS, "rss_endpoint_bytes": b.RSS - a.RSS, "heap_alloc_endpoint_bytes": int64(b.HeapAlloc) - int64(a.HeapAlloc), "elapsed_ns": b.Wall.Sub(a.Wall).Nanoseconds()}
}

func databaseInfo(ctx context.Context, conn *pgx.Conn, schema string) (map[string]any, error) {
	info := map[string]any{"observer_connections": 1, "storage_attribution": "user supplied description; no device durability claims"}
	for _, name := range []string{"server_version", "server_encoding", "fsync", "synchronous_commit", "full_page_writes", "wal_level", "max_connections", "shared_buffers", "effective_cache_size", "checkpoint_timeout", "max_wal_size", "track_io_timing"} {
		var value string
		if e := conn.QueryRow(ctx, "SELECT current_setting($1)", name).Scan(&value); e != nil {
			return nil, e
		}
		info[name] = value
	}
	rows, e := conn.Query(ctx, "SELECT version,name,checksum FROM "+pgx.Identifier{schema, "schema_migrations"}.Sanitize()+" ORDER BY version")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	versions := []map[string]any{}
	for rows.Next() {
		var version int
		var name, checksum string
		if e = rows.Scan(&version, &name, &checksum); e != nil {
			return nil, e
		}
		versions = append(versions, map[string]any{"version": version, "name": name, "sha256": checksum})
	}
	info["migrations"] = versions
	return info, rows.Err()
}
func dbSnapshot(ctx context.Context, conn *pgx.Conn, schema string) dbState {
	var out dbState
	e := conn.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text, pg_database_size(current_database()), COALESCE((SELECT sum(pg_total_relation_size(c.oid)) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind IN ('r','m')),0)::bigint`, schema).Scan(&out.WALLSN, &out.DatabaseBytes, &out.SchemaBytes)
	out.Available = e == nil
	return out
}

type latency struct {
	Count int   `json:"count"`
	P50   int64 `json:"p50_ns"`
	P95   int64 `json:"p95_ns"`
	P99   int64 `json:"p99_ns"`
	Max   int64 `json:"max_ns"`
}

func latencies(samples []sample) map[string]latency {
	values := map[string][]int64{}
	for _, prefix := range []string{"", "market_", "view_"} {
		for _, name := range []string{"generator_lag", "runner_queue_wait", "service_call_success", "offered_to_success", "service_call_all_returns"} {
			values[prefix+name] = []int64{}
		}
	}
	for _, s := range samples {
		for _, prefix := range []string{"", s.Kind + "_"} {
			values[prefix+"generator_lag"] = append(values[prefix+"generator_lag"], s.AttemptNS-s.IntendedNS)
			if s.CallNS != nil && s.AdmittedNS != nil {
				values[prefix+"runner_queue_wait"] = append(values[prefix+"runner_queue_wait"], *s.CallNS-*s.AdmittedNS)
			}
			if s.CompletedNS != nil && s.CallNS != nil {
				values[prefix+"service_call_all_returns"] = append(values[prefix+"service_call_all_returns"], *s.CompletedNS-*s.CallNS)
			}
			if s.Outcome == "completed" && s.CompletedNS != nil && s.CallNS != nil {
				values[prefix+"service_call_success"] = append(values[prefix+"service_call_success"], *s.CompletedNS-*s.CallNS)
				values[prefix+"offered_to_success"] = append(values[prefix+"offered_to_success"], *s.CompletedNS-s.IntendedNS)
			}
		}
	}
	out := map[string]latency{}
	for name, v := range values {
		sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		l := latency{Count: len(v)}
		if len(v) > 0 {
			q := func(p int) int64 { return v[(len(v)*p+99)/100-1] }
			l.P50 = q(50)
			l.P95 = q(95)
			l.P99 = q(99)
			l.Max = v[len(v)-1]
		}
		out[name] = l
	}
	return out
}

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

const loadRecoveryBacklogFills = 128
const loadRecoveryBacklogVersion = "paper-process-accepted-backlog-recovery-v1"
const loadRecoveryBacklogProvenance = "harness-authored simulated transport outcome; not emitted by the 2 Hz synthetic source"

type loadRecoveryBacklog struct {
	Provenance          string       `json:"provenance"`
	SetupStartedUTC     time.Time    `json:"fixture_setup_started_utc"`
	StoppedUTC          time.Time    `json:"victim_stopped_confirmed_utc"`
	CommittedUTC        time.Time    `json:"batch_commit_observed_utc"`
	ContinuedUTC        time.Time    `json:"victim_sigcont_issued_utc"`
	PrefixObservedUTC   time.Time    `json:"partial_prefix_observed_utc"`
	StopAttempts        int          `json:"bounded_stop_attempts"`
	IdleBackends        int          `json:"victim_backends_without_open_transaction"`
	LeaseRemainingNS    int64        `json:"lease_remaining_at_fixture_ns"`
	Owner               pg.Ownership `json:"authoring_ownership"`
	ObservationID       string       `json:"observation_id"`
	Sequence            int64        `json:"observation_sequence"`
	PayloadSHA256       string       `json:"stored_payload_sha256"`
	OperationID         string       `json:"operation_id"`
	InputDigest         string       `json:"input_digest"`
	ChildID             string       `json:"child_id"`
	RemainingBefore     d.Decimal    `json:"child_remaining_before"`
	Quantity            d.Decimal    `json:"batch_quantity"`
	Fills               []d.Fill     `json:"exact_batch_fills"`
	ObservedPrefix      int          `json:"batch_fill_prefix_before_sigkill"`
	CrashPrefix         int          `json:"batch_fill_prefix_after_reap"`
	CrashExecutions     int          `json:"batch_execution_rows_after_reap"`
	CrashHeldAmount     d.Decimal    `json:"held_reservation_after_reap"`
	CrashChildRemaining d.Decimal    `json:"child_remaining_after_reap"`
}

func TestPaperProcessAcceptedBacklogRecoveryWithBystanderDemand(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("accepted-backlog fixture confirms SIGSTOP through Linux /proc")
	}
	runPaperProcessLoadRecovery(t, true)
}

func configureLoadRecoveryBacklogReport(report *loadRecoveryReport) {
	report.Version = loadRecoveryBacklogVersion
	report.Workload["accepted_backlog_fixture"] = loadRecoveryBacklogProvenance
	report.Workload["backlog_fill_count"] = loadRecoveryBacklogFills
	report.Workload["fixture_setup_outside_schedule"] = true
	report.Workload["prefix_observer_poll_interval_ns"] = int64(time.Millisecond)
	report.Limitations[2] = "Before the measured read schedule, the harness briefly SIGSTOPs the victim and authors one owned simulated transport outcome containing 128 partial fills. SIGCONT begins the schedule; SIGKILL follows observed partial reduction. A successful result requires nonzero accepted debt and a strict partial batch after reap. This is an explicit fixture, not an ordinary 2 Hz source backlog or a source-rate/capacity claim."
	report.Limitations = append(report.Limitations, "Fixture setup/suspension and batch persistence precede the HTTP schedule and are excluded from kill/reap/launch/recovery intervals. One bounded 1 ms durable-state observer watches the batch prefix while both read schedules and the bystander source continue; observation timing is not a reducer throughput measurement.")
}

func loadRecoveryApplicationDSN(t *testing.T, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("accepted-backlog fixture requires an explicit PostgreSQL URL")
	}
	query := u.Query()
	query.Set("application_name", name)
	u.RawQuery = query.Encode()
	return u.String()
}

func loadRecoveryStopped(pid int) bool {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "State:") {
			fields := strings.Fields(line)
			return len(fields) >= 2 && (fields[1] == "T" || fields[1] == "t")
		}
	}
	return false
}

// This fixture never changes an account projection, balance, reservation, or
// outbox. Its sole authored financial input uses the production owned inbox API.
func freezeLoadRecoveryBacklog(t *testing.T, parent context.Context, victim *paperTestProcess, store *pg.Store, dsn, application string, key pg.Key) *loadRecoveryBacklog {
	t.Helper()
	fixture := &loadRecoveryBacklog{Provenance: loadRecoveryBacklogProvenance, SetupStartedUTC: time.Now().UTC()}
	conn, err := pgx.Connect(parent, dsn)
	if err != nil {
		t.Fatal("fixture transaction observer unavailable")
	}
	defer conn.Close(context.Background())
	// Also registered on failure paths after return: a stopped victim is always
	// continued before the process helper's eventual kill/reap cleanup.
	t.Cleanup(func() { _ = victim.command.Process.Signal(syscall.SIGCONT) })
	frozen := false
	defer func() {
		if !frozen {
			_ = victim.command.Process.Signal(syscall.SIGCONT)
		}
	}()
	for attempt := 1; attempt <= 40; attempt++ {
		fixture.StopAttempts = attempt
		if err := victim.command.Process.Signal(syscall.SIGSTOP); err != nil {
			t.Fatal("fixture SIGSTOP failed")
		}
		until := time.Now().Add(250 * time.Millisecond)
		for !loadRecoveryStopped(victim.command.Process.Pid) && time.Now().Before(until) {
			time.Sleep(time.Millisecond)
		}
		if !loadRecoveryStopped(victim.command.Process.Pid) {
			t.Fatal("fixture stopped-state confirmation exceeded bound")
		}
		fixture.StoppedUTC = time.Now().UTC()
		ctx, cancel := context.WithTimeout(parent, 500*time.Millisecond)
		var total, open int
		var now time.Time
		err = conn.QueryRow(ctx, "SELECT count(*),count(*) FILTER (WHERE xact_start IS NOT NULL),clock_timestamp() FROM pg_stat_activity WHERE application_name=$1", application).Scan(&total, &open, &now)
		var account pg.Account
		if err == nil && total > 0 && open == 0 {
			account, err = store.GetAccount(ctx, key)
		}
		var state rt.State
		if err == nil && total > 0 && open == 0 {
			state, err = rt.DecodeSnapshot(account.Snapshot)
		}
		stable := err == nil && total > 0 && open == 0 && account.LeaseUntil.Sub(now) > 10*time.Second && account.Ready && account.Market.Valid && account.ObservedSequence == account.ProcessedSequence && state.Parent != nil && state.Parent.Status == "active" && state.Parent.Baseline == x.BaselineLiquidityAware && state.Child != nil && state.Child.State == "open" && state.Child.Order.State == "open" && state.Child.Order.Intent.Passive && state.Child.Order.Remaining.Sign() > 0 && !state.RecoveryRequired && len(account.Intents) == 1 && account.Intents[0].ID == state.Child.ID && account.Intents[0].State == "acknowledged" && len(account.Reservations) == 1 && account.Reservations[0].IntentID == state.Child.ID
		if !stable {
			cancel()
			if err := victim.command.Process.Signal(syscall.SIGCONT); err != nil {
				t.Fatal("fixture retry SIGCONT failed")
			}
			select {
			case <-time.After(10 * time.Millisecond):
			case <-parent.Done():
				t.Fatal("fixture preparation exceeded operation deadline")
			}
			continue
		}
		fixture.IdleBackends, fixture.LeaseRemainingNS, fixture.Owner = total, account.LeaseUntil.Sub(now).Nanoseconds(), account.Ownership
		fixture.ChildID, fixture.RemainingBefore = state.Child.ID, state.Child.Order.Remaining
		fixture.ObservationID = "paper-harness-backlog-" + strings.TrimPrefix(application, "ll_backlog_victim_")
		fixture.OperationID = "runtime-harness-backlog-" + strings.TrimPrefix(application, "ll_backlog_victim_")
		quantity := fixture.RemainingBefore.Quo(d.MustDecimal("256"))
		fee, feeErr := x.FeeFor(quantity, state.Child.Order.Intent.Price, state.Fees.Maker, state.Fees.Currency)
		if feeErr != nil || quantity.Sign() <= 0 {
			cancel()
			t.Fatal("fixture exact maker fill construction failed")
		}
		for index := 0; index < loadRecoveryBacklogFills; index++ {
			fill := d.Fill{ExecutionID: fmt.Sprintf("%s-%03d", fixture.ObservationID, index), OrderID: fixture.ChildID, Side: state.Child.Order.Intent.Side, Quantity: quantity, Price: state.Child.Order.Intent.Price, Fee: fee, FeeCurrency: state.Fees.Currency, Maker: true, Time: max(0, now.Sub(state.Parent.Start).Nanoseconds())}
			if fill.Validate() != nil {
				cancel()
				t.Fatal("fixture maker fill violates retained execution contract")
			}
			fixture.Fills = append(fixture.Fills, fill)
			fixture.Quantity = fixture.Quantity.Add(quantity)
		}
		input, marshalErr := json.Marshal(fixture.Fills)
		if marshalErr != nil || !fixture.Quantity.Mul(d.MustDecimal("2")).Equal(fixture.RemainingBefore) {
			cancel()
			t.Fatal("fixture batch is not exactly half of retained child remaining")
		}
		digest := sha256.Sum256(input)
		fixture.InputDigest = hex.EncodeToString(digest[:])
		fact := struct {
			IntentID    string             `json:"intent_id"`
			OperationID string             `json:"operation_id"`
			InputDigest string             `json:"input_digest"`
			Outcome     rt.TransportResult `json:"outcome"`
		}{fixture.ChildID, fixture.OperationID, fixture.InputDigest, rt.TransportResult{Handoff: rt.PotentiallyInFlight, Accepted: true, KnownFinal: false, VenueOrderID: account.Intents[0].VenueOrderID, Reason: "harness_authored_partial_fill_batch", Fills: fixture.Fills, QueueAhead: state.Child.Order.QueueAhead}}
		payload, marshalErr := json.Marshal(fact)
		if marshalErr != nil {
			cancel()
			t.Fatal("fixture outcome encoding failed")
		}
		sequence, writeErr := store.RecordPaperObservation(ctx, key, account.Ownership, pg.Observation{ID: fixture.ObservationID, Kind: "transport", Payload: payload})
		if writeErr != nil {
			cancel()
			t.Fatal("fixture owned batch append failed or exceeded bound")
		}
		stored, readErr := store.GetObservation(ctx, key, fixture.ObservationID)
		cancel()
		if readErr != nil || stored.Sequence != sequence || sequence != account.ObservedSequence+1 || stored.Kind != "transport" {
			t.Fatal("fixture durable batch identity unavailable")
		}
		var decoded struct {
			IntentID    string             `json:"intent_id"`
			OperationID string             `json:"operation_id"`
			InputDigest string             `json:"input_digest"`
			Outcome     rt.TransportResult `json:"outcome"`
		}
		if json.Unmarshal(stored.Payload, &decoded) != nil || decoded.IntentID != fixture.ChildID || decoded.OperationID != fixture.OperationID || decoded.InputDigest != fixture.InputDigest || !reflect.DeepEqual(decoded.Outcome.Fills, fixture.Fills) {
			t.Fatal("fixture durable outcome differs from authored batch")
		}
		storedHash := sha256.Sum256(stored.Payload)
		fixture.Sequence, fixture.PayloadSHA256 = sequence, hex.EncodeToString(storedHash[:])
		fixture.CommittedUTC = time.Now().UTC()
		frozen = true
		return fixture
	}
	t.Fatal("fixture could not freeze an idle, caught-up open passive child within 40 attempts")
	return nil
}

func continueLoadRecoveryBacklog(t *testing.T, victim *paperTestProcess, fixture *loadRecoveryBacklog) {
	t.Helper()
	if fixture == nil || !loadRecoveryStopped(victim.command.Process.Pid) {
		t.Fatal("fixture lost its stopped process before measured schedule")
	}
	fixture.ContinuedUTC = time.Now().UTC()
	if err := victim.command.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal("fixture schedule SIGCONT failed")
	}
}

func loadRecoveryBatchPrefix(fills, batch []d.Fill) (int, bool) {
	seen := map[string]d.Fill{}
	for _, fill := range fills {
		if _, exists := seen[fill.ExecutionID]; exists {
			return 0, false
		}
		seen[fill.ExecutionID] = fill
	}
	prefix, gap := 0, false
	for _, expected := range batch {
		fill, exists := seen[expected.ExecutionID]
		if !exists {
			gap = true
			continue
		}
		if gap || !fill.Equal(expected) {
			return 0, false
		}
		prefix++
	}
	return prefix, true
}

func waitLoadRecoveryBacklogPrefix(t *testing.T, parent context.Context, store *pg.Store, key pg.Key, fixture *loadRecoveryBacklog) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		account, err := store.GetAccount(ctx, key)
		if err != nil {
			t.Fatal("fixture prefix durable snapshot unavailable")
		}
		state, err := rt.DecodeSnapshot(account.Snapshot)
		if err != nil {
			t.Fatal("fixture prefix snapshot malformed")
		}
		prefix, valid := loadRecoveryBatchPrefix(state.Fills, fixture.Fills)
		if !valid || prefix == len(fixture.Fills) || account.ProcessedSequence >= fixture.Sequence {
			t.Fatal("fixture missed a strict partial batch before crash; no zero-debt pass permitted")
		}
		if prefix > 0 {
			fixture.ObservedPrefix, fixture.PrefixObservedUTC = prefix, time.Now().UTC()
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("fixture did not observe partial batch reduction within two seconds")
}

func checkLoadRecoveryBacklogCrash(t *testing.T, dsn string, opt paperServerOptions, account pg.Account, fixture *loadRecoveryBacklog) {
	t.Helper()
	state, err := rt.DecodeSnapshot(account.Snapshot)
	if err != nil {
		t.Fatal("fixture crash snapshot malformed")
	}
	prefix, valid := loadRecoveryBatchPrefix(state.Fills, fixture.Fills)
	fixture.CrashPrefix = prefix
	if !valid || prefix < fixture.ObservedPrefix || prefix <= 0 || prefix >= len(fixture.Fills) || account.ProcessedSequence >= fixture.Sequence || account.ObservedSequence < fixture.Sequence || state.Child == nil || state.Child.ID != fixture.ChildID {
		t.Fatal("reaped crash did not retain an exact strict partial batch with accepted debt")
	}
	fixture.CrashChildRemaining = state.Child.Order.Remaining
	applied := d.Zero
	for _, fill := range fixture.Fills[:prefix] {
		applied = applied.Add(fill.Quantity)
	}
	if !state.Child.Order.Remaining.Equal(fixture.RemainingBefore.Sub(applied)) || len(account.Reservations) != 1 || account.Reservations[0].IntentID != fixture.ChildID || account.Reservations[0].Amount.Sign() <= 0 || len(account.Intents) != 1 || account.Intents[0].ID != fixture.ChildID || account.Intents[0].State != "acknowledged" {
		t.Fatal("reaped partial batch lost its exact remaining child or held obligation")
	}
	fixture.CrashHeldAmount = account.Reservations[0].Amount
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("fixture crash execution observer unavailable")
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, "SELECT detail FROM "+pgx.Identifier{opt.schema, "executions"}.Sanitize()+" WHERE tenant_id=$1 AND account_id=$2 AND venue='paper' ORDER BY execution_id LIMIT 4097", paperDaemonTenant, opt.account)
	if err != nil {
		t.Fatal("fixture crash execution rows unavailable")
	}
	defer rows.Close()
	fills := []d.Fill{}
	for rows.Next() {
		var payload []byte
		var fill d.Fill
		if rows.Scan(&payload) != nil || json.Unmarshal(payload, &fill) != nil {
			t.Fatal("fixture crash execution row malformed")
		}
		fills = append(fills, fill)
	}
	count, exact := loadRecoveryBatchPrefix(fills, fixture.Fills)
	fixture.CrashExecutions = count
	if rows.Err() != nil || len(fills) > 4096 || !exact || count != prefix {
		t.Fatal("fixture crash durable execution prefix differs from snapshot")
	}
}

func loadRecoveryBacklogIssues(report loadRecoveryReport) []string {
	if report.Backlog == nil {
		if report.Version == loadRecoveryBacklogVersion {
			return []string{"required authored backlog fixture missing"}
		}
		return []string{}
	}
	b := report.Backlog
	issues := []string{}
	fail := func(issue string) { issues = append(issues, issue) }
	if report.Version != loadRecoveryBacklogVersion || b.Provenance != loadRecoveryBacklogProvenance || b.StopAttempts < 1 || b.StopAttempts > 40 || b.IdleBackends < 1 || b.LeaseRemainingNS <= int64(10*time.Second) || b.Owner.OwnerID == "" || b.Owner.Epoch != report.CrashEpoch {
		fail("backlog fixture provenance or stable ownership incomplete")
	}
	if b.SetupStartedUTC.IsZero() || b.StoppedUTC.Before(b.SetupStartedUTC) || b.CommittedUTC.Before(b.StoppedUTC) || report.StartedUTC.Before(b.CommittedUTC) || b.ContinuedUTC.Before(report.StartedUTC) || b.PrefixObservedUTC.Before(b.ContinuedUTC) || b.PrefixObservedUTC.After(report.StartedUTC.Add(time.Duration(report.Events.KillIssuedNS))) {
		fail("backlog fixture setup or crash event order invalid")
	}
	if b.ObservationID == "" || b.OperationID == "" || b.ChildID == "" || b.Sequence <= 0 || b.InputDigest == "" || len(b.Fills) != loadRecoveryBacklogFills || b.Quantity.Sign() <= 0 || !b.Quantity.Mul(d.MustDecimal("2")).Equal(b.RemainingBefore) {
		fail("backlog authored batch identity or quantity incomplete")
	}
	hash, err := hex.DecodeString(b.PayloadSHA256)
	if err != nil || len(hash) != sha256.Size {
		fail("backlog stored payload hash invalid")
	}
	found := false
	for _, fact := range report.CrashFacts {
		if fact.ID == b.ObservationID && fact.Sequence == b.Sequence && fact.Kind == "transport" && fact.SHA256 == b.PayloadSHA256 {
			found = true
		}
	}
	if !found {
		fail("backlog batch absent from raw accepted observation manifest")
	}
	if report.CrashDebt <= 0 || report.CrashProcessed >= b.Sequence || report.CrashObserved < b.Sequence || b.ObservedPrefix <= 0 || b.ObservedPrefix >= len(b.Fills) || b.CrashPrefix < b.ObservedPrefix || b.CrashPrefix >= len(b.Fills) || b.CrashExecutions != b.CrashPrefix || b.CrashHeldAmount.Sign() <= 0 {
		fail("backlog crash lacks a strict exact partial prefix and held obligation")
	}
	seen := map[string]bool{}
	quantity, applied := d.Zero, d.Zero
	for index, fill := range b.Fills {
		if fill.Validate() != nil || fill.OrderID != b.ChildID || seen[fill.ExecutionID] || !fill.Maker {
			fail("backlog batch fill identities or contract invalid")
		}
		seen[fill.ExecutionID] = true
		quantity = quantity.Add(fill.Quantity)
		if index < b.CrashPrefix {
			applied = applied.Add(fill.Quantity)
		}
	}
	if !quantity.Equal(b.Quantity) || !b.CrashChildRemaining.Equal(b.RemainingBefore.Sub(applied)) {
		fail("backlog batch or crash remaining quantity inconsistent")
	}
	input, err := json.Marshal(b.Fills)
	if err != nil {
		fail("backlog exact fills cannot be encoded")
	} else {
		digest := sha256.Sum256(input)
		if b.InputDigest != hex.EncodeToString(digest[:]) {
			fail("backlog authored input digest differs")
		}
	}
	for _, audit := range []loadRecoveryAudit{report.Recovered, report.Final["victim"]} {
		prefix, exact := loadRecoveryBatchPrefix(audit.Accepted, b.Fills)
		if !exact || prefix != len(b.Fills) {
			fail("backlog accepted batch is not preserved in recovered and final cashflows")
		}
	}
	return issues
}

func TestPaperLoadRecoveryBacklogReportRequiresAcceptedPartialBatch(t *testing.T) {
	origin := time.Unix(1000, 0).UTC()
	fixture := &loadRecoveryBacklog{Provenance: loadRecoveryBacklogProvenance, SetupStartedUTC: origin.Add(-time.Second), StoppedUTC: origin.Add(-500 * time.Millisecond), CommittedUTC: origin.Add(-time.Millisecond), ContinuedUTC: origin, PrefixObservedUTC: origin.Add(time.Millisecond), StopAttempts: 1, IdleBackends: 2, LeaseRemainingNS: int64(50 * time.Second), Owner: pg.Ownership{OwnerID: "fixture-owner", Epoch: 1}, ObservationID: "fixture-batch", OperationID: "fixture-operation", ChildID: "fixture-child", Sequence: 5, PayloadSHA256: strings.Repeat("01", sha256.Size), RemainingBefore: d.MustDecimal("0.01"), ObservedPrefix: 2, CrashPrefix: 3, CrashExecutions: 3, CrashHeldAmount: d.MustDecimal("0.9")}
	for index := 0; index < loadRecoveryBacklogFills; index++ {
		fill := d.Fill{ExecutionID: fmt.Sprintf("fixture-fill-%03d", index), OrderID: fixture.ChildID, Side: "buy", Quantity: fixture.RemainingBefore.Quo(d.MustDecimal("256")), Price: d.MustDecimal("99.99"), FeeCurrency: "quote", Maker: true}
		fixture.Fills = append(fixture.Fills, fill)
		fixture.Quantity = fixture.Quantity.Add(fill.Quantity)
	}
	fixture.CrashChildRemaining = fixture.RemainingBefore.Sub(fixture.Fills[0].Quantity.Mul(d.MustDecimal("3")))
	input, _ := json.Marshal(fixture.Fills)
	digest := sha256.Sum256(input)
	fixture.InputDigest = hex.EncodeToString(digest[:])
	report := loadRecoveryReport{Version: loadRecoveryBacklogVersion, StartedUTC: origin, Backlog: fixture, CrashEpoch: 1, CrashObserved: 5, CrashProcessed: 4, CrashDebt: 1, CrashFacts: []loadRecoveryFact{{ID: fixture.ObservationID, Kind: "transport", Sequence: 5, SHA256: fixture.PayloadSHA256}}, Events: loadRecoveryEvents{KillIssuedNS: int64(2 * time.Millisecond)}, Recovered: loadRecoveryAudit{Accepted: fixture.Fills}, Final: map[string]loadRecoveryAudit{"victim": {Accepted: fixture.Fills}}}
	if issues := loadRecoveryBacklogIssues(report); len(issues) != 0 {
		t.Fatalf("complete explicit backlog evidence rejected: %v", issues)
	}
	for name, mutate := range map[string]func(*loadRecoveryReport){
		"missing_fixture":              func(r *loadRecoveryReport) { r.Backlog = nil },
		"zero_debt":                    func(r *loadRecoveryReport) { r.CrashDebt = 0 },
		"cursor_consumed_batch":        func(r *loadRecoveryReport) { r.CrashProcessed = r.Backlog.Sequence },
		"batch_not_accepted":           func(r *loadRecoveryReport) { r.CrashObserved = r.Backlog.Sequence - 1 },
		"no_prefix":                    func(r *loadRecoveryReport) { r.Backlog.ObservedPrefix = 0 },
		"fully_applied_after_reap":     func(r *loadRecoveryReport) { r.Backlog.CrashPrefix = loadRecoveryBacklogFills },
		"execution_projection_differs": func(r *loadRecoveryReport) { r.Backlog.CrashExecutions++ },
		"released_hold":                func(r *loadRecoveryReport) { r.Backlog.CrashHeldAmount = d.Zero },
		"wrong_remaining":              func(r *loadRecoveryReport) { r.Backlog.CrashChildRemaining = d.Zero },
		"raw_manifest_missing":         func(r *loadRecoveryReport) { r.CrashFacts = nil },
		"wrong_payload_hash":           func(r *loadRecoveryReport) { r.Backlog.PayloadSHA256 = strings.Repeat("02", sha256.Size) },
		"missing_operation":            func(r *loadRecoveryReport) { r.Backlog.OperationID = "" },
		"wrong_owner_epoch":            func(r *loadRecoveryReport) { r.Backlog.Owner.Epoch++ },
		"lease_too_short":              func(r *loadRecoveryReport) { r.Backlog.LeaseRemainingNS = int64(time.Second) },
		"setup_inside_read_schedule":   func(r *loadRecoveryReport) { r.Backlog.CommittedUTC = r.StartedUTC.Add(time.Second) },
		"fixture_not_labeled":          func(r *loadRecoveryReport) { r.Backlog.Provenance = "synthetic 2 Hz source" },
		"identical_fill_duplicate":     func(r *loadRecoveryReport) { r.Backlog.Fills[1] = r.Backlog.Fills[0] },
		"recovered_missing_batch_fill": func(r *loadRecoveryReport) { r.Recovered.Accepted = r.Recovered.Accepted[1:] },
		"final_missing_batch_fill":     func(r *loadRecoveryReport) { r.Final["victim"] = loadRecoveryAudit{Accepted: r.Backlog.Fills[:127]} },
	} {
		t.Run(name, func(t *testing.T) {
			altered := report
			copyFixture := *fixture
			copyFixture.Fills = append([]d.Fill{}, fixture.Fills...)
			altered.Backlog = &copyFixture
			altered.Final = map[string]loadRecoveryAudit{"victim": report.Final["victim"]}
			mutate(&altered)
			if len(loadRecoveryBacklogIssues(altered)) == 0 {
				t.Fatal("incomplete or non-backlog evidence remained valid")
			}
		})
	}
	if prefix, valid := loadRecoveryBatchPrefix([]d.Fill{fixture.Fills[1]}, fixture.Fills); valid || prefix != 0 {
		t.Fatal("noncontiguous applied prefix was accepted")
	}
}

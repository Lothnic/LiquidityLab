package replayjobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"github.com/liquiditylab/liquiditylab/internal/historicalreplay"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

type finishCall struct {
	key        pg.Key
	id, worker string
	attempt    int
	result     json.RawMessage
	code       string
}
type memoryQueue struct {
	mu                      sync.Mutex
	jobs                    map[string]pg.ReplayJob
	order                   []string
	admission               pg.ReplayAdmission
	enqueues                int
	claims                  int
	claimErrors             []error
	claimResults            []pg.ReplayJob
	readError               error
	finishErrors            []error
	commitBeforeFinishError bool
	finishes                []finishCall
}

func newMemoryQueue() *memoryQueue           { return &memoryQueue{jobs: map[string]pg.ReplayJob{}} }
func memoryKey(key pg.Key, id string) string { return key.TenantID + "/" + key.AccountID + "/" + id }
func cloneJob(job pg.ReplayJob) pg.ReplayJob {
	job.Input = append(json.RawMessage(nil), job.Input...)
	job.Result = append(json.RawMessage(nil), job.Result...)
	return job
}
func testHash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func (q *memoryQueue) EnqueueReplay(ctx context.Context, key pg.Key, id string, input json.RawMessage, admission pg.ReplayAdmission) (pg.ReplayJob, bool, error) {
	if err := ctx.Err(); err != nil {
		return pg.ReplayJob{}, false, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enqueues++
	q.admission = admission
	k := memoryKey(key, id)
	if prior, ok := q.jobs[k]; ok {
		if !bytes.Equal(prior.Input, input) {
			return pg.ReplayJob{}, false, pg.ErrConflict
		}
		return cloneJob(prior), true, nil
	}
	job := pg.ReplayJob{Key: key, ID: id, Sequence: int64(len(q.order) + 1), Input: append(json.RawMessage(nil), input...), InputSHA256: testHash(input), Status: "queued"}
	q.jobs[k] = job
	q.order = append(q.order, k)
	return cloneJob(job), false, nil
}
func (q *memoryQueue) ClaimReplay(ctx context.Context, worker string, lease time.Duration) (pg.ReplayJob, error) {
	if err := ctx.Err(); err != nil {
		return pg.ReplayJob{}, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.claims++
	if len(q.claimResults) > 0 {
		job := q.claimResults[0]
		q.claimResults = q.claimResults[1:]
		return cloneJob(job), nil
	}
	if len(q.claimErrors) > 0 {
		err := q.claimErrors[0]
		q.claimErrors = q.claimErrors[1:]
		return pg.ReplayJob{}, err
	}
	for _, key := range q.order {
		job := q.jobs[key]
		if job.Status != "queued" {
			continue
		}
		job.Status = "running"
		job.WorkerID = worker
		job.Attempts++
		until := time.Now().Add(lease)
		job.LeaseUntil = &until
		q.jobs[key] = job
		return cloneJob(job), nil
	}
	return pg.ReplayJob{}, pg.ErrNotFound
}
func (q *memoryQueue) FinishReplay(ctx context.Context, key pg.Key, id, worker string, attempt int, result json.RawMessage, code string) (pg.ReplayJob, error) {
	if err := ctx.Err(); err != nil {
		return pg.ReplayJob{}, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.finishes = append(q.finishes, finishCall{key, id, worker, attempt, append(json.RawMessage(nil), result...), code})
	k := memoryKey(key, id)
	job, ok := q.jobs[k]
	if !ok {
		return pg.ReplayJob{}, pg.ErrNotFound
	}
	if job.WorkerID != worker || job.Attempts != attempt {
		return pg.ReplayJob{}, pg.ErrOwnership
	}
	status := "succeeded"
	if code != "" {
		status = "failed"
	}
	if job.Status == status {
		if !bytes.Equal(job.Result, result) || job.ErrorCode != code {
			return pg.ReplayJob{}, pg.ErrConflict
		}
		return cloneJob(job), nil
	}
	if job.Status != "running" {
		return pg.ReplayJob{}, pg.ErrOwnership
	}
	var injected error
	if len(q.finishErrors) > 0 {
		injected = q.finishErrors[0]
		q.finishErrors = q.finishErrors[1:]
	}
	if injected != nil && !q.commitBeforeFinishError {
		return pg.ReplayJob{}, injected
	}
	job.Status = status
	job.Result = append(json.RawMessage(nil), result...)
	job.ErrorCode = code
	job.LeaseUntil = nil
	if code == "" {
		job.ResultSHA256 = testHash(result)
	}
	q.jobs[k] = job
	return cloneJob(job), injected
}
func (q *memoryQueue) CancelReplay(ctx context.Context, key pg.Key, id string) (pg.ReplayJob, error) {
	if err := ctx.Err(); err != nil {
		return pg.ReplayJob{}, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	k := memoryKey(key, id)
	job, ok := q.jobs[k]
	if !ok {
		return pg.ReplayJob{}, pg.ErrNotFound
	}
	if job.Status == "queued" || job.Status == "running" {
		job.Status = "cancelled"
		job.LeaseUntil = nil
		q.jobs[k] = job
	}
	return cloneJob(job), nil
}
func (q *memoryQueue) Replay(ctx context.Context, key pg.Key, id string) (pg.ReplayJob, error) {
	if err := ctx.Err(); err != nil {
		return pg.ReplayJob{}, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.readError != nil {
		return pg.ReplayJob{}, q.readError
	}
	job, ok := q.jobs[memoryKey(key, id)]
	if !ok {
		return pg.ReplayJob{}, pg.ErrNotFound
	}
	return cloneJob(job), nil
}
func (q *memoryQueue) Replays(ctx context.Context, key pg.Key, before int64, limit int) ([]pg.ReplayJob, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []pg.ReplayJob
	for i := len(q.order) - 1; i >= 0 && len(out) < limit; i-- {
		job := q.jobs[q.order[i]]
		if job.Key != key || before > 0 && job.Sequence >= before {
			continue
		}
		job.Input = nil
		job.Result = nil
		out = append(out, job)
	}
	return out, nil
}

func serviceSpec() historicalreplay.Spec {
	p := d.DefaultPlan()
	p.Quantity = d.MustDecimal(".001")
	p.MaxChildQuantity = p.Quantity
	p.Deadline = d.Second
	p.LimitPrice = d.MustDecimal("90000")
	c := d.DefaultConfig()
	c.DecisionInterval = d.Second / 10
	c.SettlementAllowance = d.Second / 5
	c.SubmissionLatency = 0
	c.CancelLatency = 0
	c.MinMessageInterval = 0
	c.MinRestingAge = 0
	return historicalreplay.Spec{Schema: historicalreplay.SpecSchema, Rules: d.DefaultRules(), Fees: d.DefaultFees(), Simulation: c, ValuationReference: "deadline-midpoint", Reference: historicalreplay.Scenario{Label: "reference", Plan: p, Policy: "scheduled"}, Alternatives: []historicalreplay.Scenario{{Label: "alternative", Plan: p, Policy: "liquidity-aware"}}}
}
func specBytes(t *testing.T) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(serviceSpec())
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func testService(t *testing.T, q *memoryQueue) *Service {
	t.Helper()
	s, err := New(Config{Queue: q, ArchiveRoot: t.TempDir(), WorkerID: "test-worker", JobTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var serviceKey = pg.Key{TenantID: "test-tenant", AccountID: "test-account"}
var serviceArchiveID = strings.Repeat("a", 64)

func queuedServiceJob(t *testing.T, s *Service, id string) pg.ReplayJob {
	t.Helper()
	job, seen, err := s.Enqueue(context.Background(), serviceKey, id, serviceArchiveID, specBytes(t))
	if err != nil || seen {
		t.Fatalf("enqueue: %v %v", seen, err)
	}
	return job
}
func claimedServiceJob(t *testing.T, s *Service, q *memoryQueue, id string) pg.ReplayJob {
	t.Helper()
	queuedServiceJob(t, s, id)
	job, err := q.ClaimReplay(context.Background(), s.c.WorkerID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return job
}
func awaitTest(t *testing.T, description string, fn func() bool) {
	t.Helper()
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		if fn() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("timed out: " + description)
		case <-ticker.C:
		}
	}
}

func TestServiceEnqueueCanonicalExplicitSpecAndValidation(t *testing.T) {
	q := newMemoryQueue()
	s := testService(t, q)
	raw := specBytes(t)
	first, seen, err := s.Enqueue(context.Background(), serviceKey, "canonical", serviceArchiveID, raw)
	if err != nil || seen {
		t.Fatal(err)
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, raw, "", "    "); err != nil {
		t.Fatal(err)
	}
	second, seen, err := s.Enqueue(context.Background(), serviceKey, "canonical", serviceArchiveID, append(indented.Bytes(), '\n'))
	if err != nil || !seen || !bytes.Equal(first.Input, second.Input) || first.InputSHA256 != second.InputSHA256 {
		t.Fatal("insignificant spec whitespace changed identity", err)
	}
	input, err := parseInput(first.Input)
	if err != nil || input.ArchiveID != serviceArchiveID || input.Schema != InputSchema || !reflect.DeepEqual(input.Spec, serviceSpec()) {
		t.Fatal("resolved spec was not retained", err)
	}
	if q.admission != (pg.ReplayAdmission{PerAccount: 4, Global: 16}) {
		t.Fatal("default admission was not forwarded")
	}
	before := q.enqueues
	for name, change := range map[string]func(pg.Key, string, string, json.RawMessage) (pg.Key, string, string, json.RawMessage){
		"unsafe account": func(k pg.Key, id, a string, b json.RawMessage) (pg.Key, string, string, json.RawMessage) {
			k.AccountID = "../account"
			return k, id, a, b
		},
		"unsafe id": func(k pg.Key, id, a string, b json.RawMessage) (pg.Key, string, string, json.RawMessage) {
			return k, "job/path", a, b
		},
		"uppercase archive": func(k pg.Key, id, a string, b json.RawMessage) (pg.Key, string, string, json.RawMessage) {
			return k, id, strings.ToUpper(a), b
		},
		"missing explicit fees": func(k pg.Key, id, a string, b json.RawMessage) (pg.Key, string, string, json.RawMessage) {
			var m map[string]json.RawMessage
			_ = json.Unmarshal(b, &m)
			delete(m, "fees")
			b, _ = json.Marshal(m)
			return k, id, a, b
		},
		"malformed raw": func(k pg.Key, id, a string, b json.RawMessage) (pg.Key, string, string, json.RawMessage) {
			return k, id, a, json.RawMessage(`{"secret":"/private/raw"`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			k, id, a, b := change(serviceKey, "invalid", serviceArchiveID, raw)
			if _, _, err := s.Enqueue(context.Background(), k, id, a, b); !errors.Is(err, pg.ErrInvalid) {
				t.Fatalf("invalid enqueue accepted: %v", err)
			}
		})
	}
	if q.enqueues != before {
		t.Fatal("invalid input reached durable admission")
	}
	changed := serviceSpec()
	changed.Alternatives[0].Plan.LimitPrice = d.MustDecimal("90001")
	changedBytes, _ := json.Marshal(changed)
	if _, _, err := s.Enqueue(context.Background(), serviceKey, "canonical", serviceArchiveID, changedBytes); !errors.Is(err, pg.ErrConflict) {
		t.Fatal("changed resolved spec reused identity", err)
	}
	list, err := s.List(context.Background(), serviceKey, 0, 10)
	if err != nil || len(list) != 1 || list[0].Input != nil || list[0].Result != nil {
		t.Fatal("metadata list mismatch", err)
	}
	if _, err := s.Cancel(context.Background(), serviceKey, "canonical"); err != nil {
		t.Fatal(err)
	}
	job, err := s.Get(context.Background(), serviceKey, "canonical")
	if err != nil || job.Status != "cancelled" {
		t.Fatal(err)
	}
}

func TestServiceConfigBoundsAndDefaults(t *testing.T) {
	base := Config{Queue: newMemoryQueue(), ArchiveRoot: t.TempDir(), WorkerID: "worker"}
	s, err := New(base)
	if err != nil || s.c.Workers != 1 || s.c.JobTimeout != 20*time.Second {
		t.Fatal("defaults invalid", err)
	}
	for name, alter := range map[string]func(*Config){
		"nil queue": func(c *Config) { c.Queue = nil }, "relative root": func(c *Config) { c.ArchiveRoot = "relative" }, "worker id": func(c *Config) { c.WorkerID = "bad/worker" }, "too many workers": func(c *Config) { c.Workers = 5 }, "negative workers": func(c *Config) { c.Workers = -1 }, "short timeout": func(c *Config) { c.JobTimeout = time.Nanosecond }, "long timeout": func(c *Config) { c.JobTimeout = 31 * time.Second }, "account cap": func(c *Config) { c.Admission = pg.ReplayAdmission{PerAccount: 65, Global: 100} }, "global cap": func(c *Config) { c.Admission = pg.ReplayAdmission{PerAccount: 4, Global: 3} },
	} {
		t.Run(name, func(t *testing.T) {
			c := base
			alter(&c)
			if _, err := New(c); !errors.Is(err, pg.ErrInvalid) {
				t.Fatal("invalid config accepted", err)
			}
		})
	}
	if err := s.Run(nil); !errors.Is(err, pg.ErrInvalid) {
		t.Fatal("nil Run context accepted")
	}
}

func TestServiceRejectsMalformedAndHashMismatchedStoredInputs(t *testing.T) {
	for _, kind := range []string{"hash", "malformed", "noncanonical", "unknown field", "spec"} {
		t.Run(kind, func(t *testing.T) {
			q := newMemoryQueue()
			s := testService(t, q)
			job := claimedServiceJob(t, s, q, "invalid")
			switch kind {
			case "hash":
				job.InputSHA256 = strings.Repeat("0", 64)
			case "malformed":
				job.Input = json.RawMessage("{")
			case "noncanonical":
				job.Input = append(job.Input, '\n')
			case "unknown field":
				job.Input = append(append(json.RawMessage(nil), job.Input[:len(job.Input)-1]...), []byte(`,"secret":"/private/input"}`)...)
			case "spec":
				job.Input = json.RawMessage(`{"schema":"` + InputSchema + `","archive_id":"` + serviceArchiveID + `","spec":{}}`)
			}
			if kind != "hash" {
				job.InputSHA256 = testHash(job.Input)
			}
			calls := 0
			s.execute = func(context.Context, Input) (json.RawMessage, string) { calls++; return json.RawMessage(`{}`), "" }
			s.process(context.Background(), job)
			got, _ := q.Replay(context.Background(), job.Key, job.ID)
			if calls != 0 || got.Status != "failed" || got.ErrorCode != "input_invalid" || len(got.Result) != 0 {
				t.Fatalf("stored invalid input executed: calls=%d job=%+v", calls, got)
			}
		})
	}
}

func TestServiceWorkerStatusCodesAndFinishRetry(t *testing.T) {
	for _, code := range []string{"", "archive_unavailable", "execution_failed", "secret SQL password=/private/raw"} {
		t.Run(code, func(t *testing.T) {
			q := newMemoryQueue()
			s := testService(t, q)
			job := claimedServiceJob(t, s, q, "fixed")
			s.execute = func(context.Context, Input) (json.RawMessage, string) {
				if code != "" {
					return nil, code
				}
				return json.RawMessage(`{"estimate_only":true}`), ""
			}
			s.process(context.Background(), job)
			got, _ := q.Replay(context.Background(), job.Key, job.ID)
			want := code
			if strings.Contains(code, "secret") {
				want = "execution_failed"
			}
			if got.ErrorCode != want {
				t.Fatalf("unbounded failure code entered storage: %q", got.ErrorCode)
			}
			if code == "" && (got.Status != "succeeded" || got.ResultSHA256 != testHash(got.Result)) {
				t.Fatal("success result hash missing")
			}
			if strings.Contains(string(got.Result)+got.ErrorCode, "private") {
				t.Fatal("sensitive raw text retained")
			}
		})
	}
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint("uncertain=", uncertain), func(t *testing.T) {
			q := newMemoryQueue()
			s := testService(t, q)
			job := claimedServiceJob(t, s, q, "retry")
			q.finishErrors = []error{errors.New("temporary SQL failure")}
			q.commitBeforeFinishError = uncertain
			calls := 0
			s.execute = func(context.Context, Input) (json.RawMessage, string) {
				calls++
				return json.RawMessage(`{"fixed":"result"}`), ""
			}
			s.process(context.Background(), job)
			got, _ := q.Replay(context.Background(), job.Key, job.ID)
			if got.Status != "succeeded" || calls != 1 || len(q.finishes) != 2 || !bytes.Equal(q.finishes[0].result, q.finishes[1].result) {
				t.Fatal("finish retry recalculated or changed frozen result")
			}
		})
	}
	q := newMemoryQueue()
	s := testService(t, q)
	job := claimedServiceJob(t, s, q, "fenced")
	q.finishErrors = []error{pg.ErrOwnership}
	s.execute = func(context.Context, Input) (json.RawMessage, string) { return json.RawMessage(`{}`), "" }
	s.process(context.Background(), job)
	if len(q.finishes) != 1 {
		t.Fatal("permanent attempt fence was retried")
	}
}

func TestServiceDeadlineShutdownAndRemoteFencing(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		q := newMemoryQueue()
		s := testService(t, q)
		s.c.JobTimeout = 10 * time.Millisecond
		job := claimedServiceJob(t, s, q, "timeout")
		s.execute = func(ctx context.Context, _ Input) (json.RawMessage, string) {
			<-ctx.Done()
			return nil, "private timeout error"
		}
		s.process(context.Background(), job)
		got, _ := q.Replay(context.Background(), job.Key, job.ID)
		if got.ErrorCode != "deadline_exceeded" || got.Status != "failed" {
			t.Fatalf("deadline lost: %+v", got)
		}
	})
	for _, change := range []string{"cancel", "attempt", "worker", "read error", "shutdown"} {
		t.Run(change, func(t *testing.T) {
			q := newMemoryQueue()
			s := testService(t, q)
			job := claimedServiceJob(t, s, q, "remote")
			started := make(chan struct{})
			stopped := make(chan struct{})
			finished := make(chan struct{})
			s.execute = func(ctx context.Context, _ Input) (json.RawMessage, string) {
				close(started)
				<-ctx.Done()
				close(stopped)
				return nil, "execution_failed"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { s.process(ctx, job); close(finished) }()
			<-started
			if change == "shutdown" {
				cancel()
			} else {
				q.mu.Lock()
				current := q.jobs[memoryKey(job.Key, job.ID)]
				switch change {
				case "cancel":
					current.Status = "cancelled"
				case "attempt":
					current.Attempts++
				case "worker":
					current.WorkerID = "new-worker"
				case "read error":
					q.readError = errors.New("temporary DB error secret")
				}
				q.jobs[memoryKey(job.Key, job.ID)] = current
				q.mu.Unlock()
			}
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("calculation did not observe cancellation")
			}
			select {
			case <-stopped:
			default:
				t.Fatal("process returned before calculation stopped")
			}
			q.mu.Lock()
			count := len(q.finishes)
			q.mu.Unlock()
			if count != 0 {
				t.Fatal("cancelled or replaced attempt published")
			}
		})
	}
}

func TestServiceRunBoundsParallelismDuplicateRunAndShutdownJoin(t *testing.T) {
	q := newMemoryQueue()
	s := testService(t, q)
	s.c.Workers = 2
	for i := 0; i < 6; i++ {
		queuedServiceJob(t, s, fmt.Sprintf("job-%d", i))
	}
	var active, peak, total atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{}, 6)
	s.execute = func(ctx context.Context, _ Input) (json.RawMessage, string) {
		now := active.Add(1)
		for old := peak.Load(); now > old && !peak.CompareAndSwap(old, now); old = peak.Load() {
		}
		total.Add(1)
		entered <- struct{}{}
		defer active.Add(-1)
		select {
		case <-release:
			return json.RawMessage(`{"estimate_only":true}`), ""
		case <-ctx.Done():
			return nil, "execution_failed"
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			cancel()
			t.Fatal("workers did not start")
		}
	}
	if err := s.Run(context.Background()); !errors.Is(err, pg.ErrInvalid) {
		cancel()
		t.Fatal("duplicate Run accepted")
	}
	if active.Load() != 2 || total.Load() != 2 {
		cancel()
		t.Fatal("more than configured calculations entered")
	}
	close(release)
	awaitTest(t, "all calculations complete", func() bool { q.mu.Lock(); defer q.mu.Unlock(); return len(q.finishes) == 6 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not join workers")
	}
	if peak.Load() > 2 || active.Load() != 0 || total.Load() != 6 {
		t.Fatal("worker calculation bound or join failed")
	}
	stopped, cancelStopped := context.WithCancel(context.Background())
	cancelStopped()
	if err := s.Run(stopped); err != nil {
		t.Fatal("service remained stuck running after shutdown", err)
	}
}

func TestServiceDefaultExecutionMissingArchiveAndClaimBackoff(t *testing.T) {
	q := newMemoryQueue()
	s := testService(t, q)
	job := claimedServiceJob(t, s, q, "missing")
	s.process(context.Background(), job)
	got, _ := q.Replay(context.Background(), job.Key, job.ID)
	if got.Status != "failed" || got.ErrorCode != "archive_unavailable" || len(got.Result) != 0 {
		t.Fatalf("default execution did not sanitize missing archive: %+v", got)
	}
	q = newMemoryQueue()
	s = testService(t, q)
	q.claimErrors = []error{errors.New("private SQL endpoint/password")}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	awaitTest(t, "first failed claim", func() bool { q.mu.Lock(); defer q.mu.Unlock(); return q.claims == 1 })
	time.Sleep(20 * time.Millisecond)
	q.mu.Lock()
	claims := q.claims
	q.mu.Unlock()
	cancel()
	<-done
	if claims != 1 {
		t.Fatal("transient database claim failure was busy-looped")
	}
}

func TestServiceTerminalClaimsAreSkippedAndFinishRetriesAreBounded(t *testing.T) {
	t.Run("terminal claim", func(t *testing.T) {
		q := newMemoryQueue()
		s := testService(t, q)
		q.claimResults = []pg.ReplayJob{{Key: serviceKey, ID: "worker-lost", Status: "failed", ErrorCode: "worker_lost"}}
		var executed atomic.Int32
		s.execute = func(context.Context, Input) (json.RawMessage, string) {
			executed.Add(1)
			return json.RawMessage(`{}`), ""
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()
		awaitTest(t, "terminal claim skipped", func() bool { q.mu.Lock(); defer q.mu.Unlock(); return q.claims >= 2 })
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if executed.Load() != 0 || len(q.finishes) != 0 {
			t.Fatal("terminal expired attempt was executed")
		}
	})
	t.Run("transient finish exhaustion", func(t *testing.T) {
		q := newMemoryQueue()
		s := testService(t, q)
		job := claimedServiceJob(t, s, q, "transient")
		q.finishErrors = []error{errors.New("database unavailable private DSN"), errors.New("database unavailable private DSN")}
		calls := 0
		s.execute = func(context.Context, Input) (json.RawMessage, string) {
			calls++
			return json.RawMessage(`{"estimate_only":true}`), ""
		}
		s.process(context.Background(), job)
		got, _ := q.Replay(context.Background(), job.Key, job.ID)
		if calls != 1 || len(q.finishes) != 2 || got.Status != "running" || got.LeaseUntil == nil || got.ErrorCode != "" || len(got.Result) != 0 {
			t.Fatal("transient finish exhaustion changed frozen lease or reran calculation")
		}
	})
}

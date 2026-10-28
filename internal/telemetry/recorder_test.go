package telemetry

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"
)

func TestNilReceiverAndInvalidIDsAreNoops(t *testing.T) {
	var nilRecorder *Recorder
	nilRecorder.Add(ActorAdmitted, 1)
	nilRecorder.Set(ActorQueued, 1)
	nilRecorder.Adjust(ActorQueued, 1)
	nilRecorder.Observe(ActorHandleNS, 42)
	if got := nilRecorder.Snapshot(); got.SchemaVersion != SchemaVersion || len(got.Counters) != 0 || len(got.Gauges) != 0 || len(got.Distributions) != 0 {
		t.Fatalf("nil snapshot = %#v", got)
	}
	r := New()
	r.Add(CounterCount, 1)
	r.Set(GaugeCount, 1)
	r.Adjust(GaugeCount, 1)
	r.Observe(DistributionCount, 1)
	s := r.Snapshot()
	if s.Counters[ActorAdmitted].Value != 0 || s.Gauges[ActorQueued].Value != 0 || s.Distributions[ActorQueueWaitNS].Count != 0 {
		t.Fatalf("invalid IDs changed state: %#v", s)
	}
}

func TestBucketsUnitsAndQuantileUpperBoundEstimates(t *testing.T) {
	r := New()
	r.Observe(ActorHandleNS, 1_000)
	r.Observe(ActorHandleNS, 1_001)
	r.Observe(ActorHandleNS, 5_000)
	r.Observe(SnapshotBytes, 64)
	r.Observe(SnapshotBytes, 65)
	r.Observe(ActorQueueDepth, 0)
	r.Observe(ActorQueueDepth, 1)
	s := r.Snapshot()
	d := s.Distributions[ActorHandleNS]
	if d.Unit != "ns" || d.Count != 3 || d.Sum != 7_001 || d.Min == nil || *d.Min != 1_000 || d.Max == nil || *d.Max != 5_000 {
		t.Fatalf("duration stats = %#v", d)
	}
	if d.Buckets[0].Count != 1 || d.Buckets[1].Count != 2 || d.P50UpperBound == nil || *d.P50UpperBound != 5_000 || d.P95UpperBound == nil || *d.P95UpperBound != 5_000 || d.P99UpperBound == nil || *d.P99UpperBound != 5_000 {
		t.Fatalf("duration histogram/estimates = %#v", d)
	}
	bytes := s.Distributions[SnapshotBytes]
	if bytes.Unit != "bytes" || bytes.Buckets[0].Count != 1 || bytes.Buckets[1].Count != 1 {
		t.Fatalf("byte histogram = %#v", bytes)
	}
	queue := s.Distributions[ActorQueueDepth]
	if queue.Unit != "items" || queue.Buckets[0].Count != 1 || queue.Buckets[1].Count != 1 {
		t.Fatalf("queue histogram = %#v", queue)
	}
}

func TestRequestedDurationBoundsAreExactAndInclusive(t *testing.T) {
	r := New()
	for _, value := range []uint64{150_000_000, 150_000_001, 200_000_000, 200_000_001} {
		r.Observe(DBAccountLockNS, value)
	}
	d := r.Snapshot().Distributions[DBAccountLockNS]
	countsByBound := make(map[uint64]uint64)
	for _, bucket := range d.Buckets {
		if bucket.UpperBound != nil {
			countsByBound[*bucket.UpperBound] = bucket.Count
		}
	}
	if countsByBound[150_000_000] != 1 || countsByBound[200_000_000] != 2 || countsByBound[250_000_000] != 1 {
		t.Fatalf("target boundaries not inclusive/exact: %#v", countsByBound)
	}
	if d.Name != "db_account_for_update_roundtrip_ns" {
		t.Fatalf("DBAccountLockNS name = %q", d.Name)
	}
}

func TestZeroValueRecorderIsUsable(t *testing.T) {
	var r Recorder
	r.Add(ActorAdmitted, 2)
	r.Set(ActorQueued, 3)
	r.Adjust(ActorQueued, -1)
	r.Observe(SnapshotBytes, 65)
	s := r.Snapshot()
	if s.SchemaVersion != SchemaVersion || s.Counters[ActorAdmitted].Value != 2 || s.Gauges[ActorQueued].Value != 2 {
		t.Fatalf("zero-value recorder state = %#v", s)
	}
	d := s.Distributions[SnapshotBytes]
	if d.Count != 1 || d.Unit != "bytes" || len(d.Buckets) != len(byteBounds)+1 || d.Buckets[1].Count != 1 {
		t.Fatalf("zero-value distribution shape/state = %#v", d)
	}
}

func TestEmptyQuantilesAndOverflowAreUnavailable(t *testing.T) {
	r := New()
	empty := r.Snapshot().Distributions[ActorQueueWaitNS]
	if empty.Min != nil || empty.Max != nil || empty.P50UpperBound != nil || empty.P95UpperBound != nil || empty.P99UpperBound != nil {
		t.Fatalf("empty distribution has values: %#v", empty)
	}
	r.Observe(ActorQueueWaitNS, math.MaxUint64)
	full := r.Snapshot().Distributions[ActorQueueWaitNS]
	if full.Buckets[len(full.Buckets)-1].Count != 1 || !full.Buckets[len(full.Buckets)-1].Overflow || full.P50UpperBound != nil || full.P95UpperBound != nil || full.P99UpperBound != nil {
		t.Fatalf("overflow-bucket quantile must be unavailable: %#v", full)
	}
	r.Observe(DecisionCommitNS, math.MaxUint64)
	r.Observe(DecisionCommitNS, 1)
	withSumOverflow := r.Snapshot().Distributions[DecisionCommitNS]
	if withSumOverflow.Sum != math.MaxUint64 || !withSumOverflow.SumOverflow || !withSumOverflow.Overflow {
		t.Fatalf("sum did not saturate: %#v", withSumOverflow)
	}
	if withSumOverflow.P95UpperBound != nil {
		t.Fatalf("quantile in overflow bucket should be unavailable: %#v", withSumOverflow)
	}
	r.Add(ActorRejected, math.MaxUint64)
	r.Add(ActorRejected, 1)
	if got := r.Snapshot().Counters[ActorRejected]; got.Value != math.MaxUint64 || !got.Overflow {
		t.Fatalf("counter did not saturate: %#v", got)
	}
}

func TestGaugeAdjustClampsAndSaturates(t *testing.T) {
	r := New()
	r.Adjust(ActorQueued, -1)
	if got := r.Snapshot().Gauges[ActorQueued]; got.Value != 0 || !got.Overflow {
		t.Fatalf("negative adjustment = %#v", got)
	}
	r.Set(ActorQueued, math.MaxInt64)
	r.Adjust(ActorQueued, 1)
	if got := r.Snapshot().Gauges[ActorQueued]; got.Value != math.MaxInt64 || !got.Overflow {
		t.Fatalf("positive overflow = %#v", got)
	}
	r.Set(ActorQueued, 5)
	r.Adjust(ActorQueued, -3)
	if got := r.Snapshot().Gauges[ActorQueued]; got.Value != 2 {
		t.Fatalf("ordinary adjustment = %#v", got)
	}
}

func TestConcurrentRecordingAndSnapshot(t *testing.T) {
	r := New()
	const workers, each = 8, 1_000
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				r.Add(ActorCompleted, 1)
				r.Observe(ActorHandleNS, uint64(j))
				r.Adjust(ActorHandling, 1)
				r.Adjust(ActorHandling, -1)
				_ = r.Snapshot()
			}
		}()
	}
	wg.Wait()
	s := r.Snapshot()
	if got := s.Counters[ActorCompleted].Value; got != workers*each {
		t.Fatalf("counter = %d, want %d", got, workers*each)
	}
	if got := s.Distributions[ActorHandleNS].Count; got != workers*each {
		t.Fatalf("distribution count = %d, want %d", got, workers*each)
	}
	if got := s.Gauges[ActorHandling].Value; got != 0 {
		t.Fatalf("gauge = %d, want 0", got)
	}
}

func TestSnapshotJSONIsFixedSchemaAndContainsNoIdentifiers(t *testing.T) {
	r := New()
	r.Add(DBQueries, 4)
	r.Observe(DBAccountLockNS, 25_000)
	b, err := json.Marshal(r.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, key := range []string{`"schema_version":"telemetry-v1"`, `"counters"`, `"gauges"`, `"distributions"`, `"name":"db_account_for_update_roundtrip_ns"`, `"unit":"ns"`, `"quantile_method":"bucket_upper_bound"`, `"p50_upper_bound":25000`} {
		if !strings.Contains(text, key) {
			t.Errorf("JSON missing %s: %s", key, text[:min(len(text), 500)])
		}
	}
	for _, forbidden := range []string{"tenant_id", "account_id", "plan_id", "infinity", "NaN"} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
			t.Errorf("JSON contains forbidden dynamic/invalid value %q", forbidden)
		}
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 4 {
		t.Fatalf("unexpected top-level schema keys: %v", decoded)
	}
}

// Package telemetry provides a bounded, dependency-free in-process metrics recorder.
package telemetry

import (
	"encoding/json"
	"math"
	"math/bits"
	"sync"
)

const SchemaVersion = "telemetry-v1"

type Counter uint16

const (
	ActorAdmitted Counter = iota
	ActorRejected
	ActorCompleted
	ActorFailed
	DBTransactionsStarted
	DBTransactionsCommitted
	DBTransactionsRolledBack
	DBCommitErrors
	DBRollbackErrors
	DBAcquireErrors
	DBQueries
	DBQueryErrors
	DBDurableTransitions
	DBObservationWrites
	DBObservationDuplicates
	InferenceRequests
	InferenceRejected
	InferenceRPCs
	InferenceErrors
	MarketEvents
	MarketErrors
	PolicyDecisions
	PolicyFallbacks
	CaptureFrames
	CaptureInputBytes
	CapturePersistAttempts
	CapturePersistErrors
	CaptureDeliveryAttempts
	CaptureDeliveryErrors
	PublicIngressRequests
	PublicIngressErrors
	PublicReceiptOriginMissing
	CounterCount
)

type Gauge uint16

const (
	ActorQueued Gauge = iota
	ActorHandling
	DBTransactionsInFlight
	InferenceInFlight
	GaugeCount
)

type Distribution uint16

const (
	ActorQueueWaitNS Distribution = iota
	ActorHandleNS
	ControlAckNS
	PublicMarketApplyNS
	InferenceRPCNS
	DecisionCommitNS
	DBAcquireNS
	DBBeginNS
	DBTransactionNS
	DBCommitNS
	DBRollbackNS
	DBAccountLockNS
	SnapshotBytes
	ObservationBytes
	JournalBytes
	ActorQueueDepth
	CapturePersistNS
	CaptureReadToDurableNS
	CaptureReceiptToConsumerNS
	PublicIngressNS
	PublicReceiptToBookNS
	DistributionCount
)

var (
	durationBounds = [...]uint64{1_000, 5_000, 10_000, 25_000, 50_000, 100_000, 250_000, 500_000, 1_000_000, 2_500_000, 5_000_000, 10_000_000, 25_000_000, 50_000_000, 100_000_000, 150_000_000, 200_000_000, 250_000_000, 500_000_000, 1_000_000_000, 2_500_000_000, 5_000_000_000, 10_000_000_000}
	byteBounds     = [...]uint64{64, 128, 256, 512, 1_024, 2_048, 4_096, 8_192, 16_384, 32_768, 65_536, 131_072, 262_144, 524_288, 1_048_576, 2_097_152}
	queueBounds    = [...]uint64{0, 1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1_024, 4_096, 8_192}
)

type CounterSnapshot struct {
	Name     string `json:"name"`
	Unit     string `json:"unit"`
	Value    uint64 `json:"value"`
	Overflow bool   `json:"overflow"`
}

type GaugeSnapshot struct {
	Name     string `json:"name"`
	Unit     string `json:"unit"`
	Value    int64  `json:"value"`
	Overflow bool   `json:"overflow"`
}

type BucketSnapshot struct {
	UpperBound *uint64 `json:"upper_bound,omitempty"`
	Overflow   bool    `json:"overflow,omitempty"`
	Count      uint64  `json:"count"`
}

type DistributionSnapshot struct {
	Name           string           `json:"name"`
	Unit           string           `json:"unit"`
	Count          uint64           `json:"count"`
	CountOverflow  bool             `json:"count_overflow"`
	Sum            uint64           `json:"sum"`
	SumOverflow    bool             `json:"sum_overflow"`
	Min            *uint64          `json:"min"`
	Max            *uint64          `json:"max"`
	Buckets        []BucketSnapshot `json:"buckets"`
	P50UpperBound  *uint64          `json:"p50_upper_bound"`
	P95UpperBound  *uint64          `json:"p95_upper_bound"`
	P99UpperBound  *uint64          `json:"p99_upper_bound"`
	QuantileMethod string           `json:"quantile_method"`
	Overflow       bool             `json:"overflow"`
}

type Snapshot struct {
	SchemaVersion string                 `json:"schema_version"`
	Counters      []CounterSnapshot      `json:"counters"`
	Gauges        []GaugeSnapshot        `json:"gauges"`
	Distributions []DistributionSnapshot `json:"distributions"`
}

type distributionState struct {
	count, sum, min, max uint64
	countOverflow        bool
	sumOverflow          bool
	seen                 bool
	buckets              []uint64
}

type Recorder struct {
	mu              sync.Mutex
	counters        [CounterCount]uint64
	counterOverflow [CounterCount]bool
	gauges          [GaugeCount]int64
	gaugeOverflow   [GaugeCount]bool
	distributions   [DistributionCount]distributionState
}

func New() *Recorder {
	r := &Recorder{}
	for id := Distribution(0); id < DistributionCount; id++ {
		r.distributions[id].buckets = make([]uint64, len(boundsFor(id))+1)
	}
	return r
}

func (r *Recorder) Add(id Counter, delta uint64) {
	if r == nil || id >= CounterCount {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	value, carry := bits.Add64(r.counters[id], delta, 0)
	if carry != 0 {
		r.counters[id] = math.MaxUint64
		r.counterOverflow[id] = true
		return
	}
	r.counters[id] = value
}

func (r *Recorder) Set(id Gauge, value int64) {
	if r == nil || id >= GaugeCount {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if value < 0 {
		r.gauges[id] = 0
		r.gaugeOverflow[id] = true
		return
	}
	r.gauges[id] = value
}

// Adjust changes an aggregation gauge. Negative results clamp to zero and mark
// overflow; positive results above int64 max saturate and mark overflow.
func (r *Recorder) Adjust(id Gauge, delta int64) {
	if r == nil || id >= GaugeCount {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.gauges[id]
	if delta < 0 {
		magnitude := uint64(-(delta + 1)) + 1 // also handles MinInt64
		if magnitude > uint64(current) {
			r.gauges[id] = 0
			r.gaugeOverflow[id] = true
			return
		}
		r.gauges[id] = current - int64(magnitude)
		return
	}
	if delta > math.MaxInt64-current {
		r.gauges[id] = math.MaxInt64
		r.gaugeOverflow[id] = true
		return
	}
	r.gauges[id] = current + delta
}

func (r *Recorder) Observe(id Distribution, value uint64) {
	if r == nil || id >= DistributionCount {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &r.distributions[id]
	if s.buckets == nil {
		s.buckets = make([]uint64, len(boundsFor(id))+1)
	}
	if s.count == math.MaxUint64 {
		s.countOverflow = true
	} else {
		s.count++
	}
	if math.MaxUint64-s.sum < value {
		s.sum = math.MaxUint64
		s.sumOverflow = true
	} else {
		s.sum += value
	}
	if !s.seen || value < s.min {
		s.min = value
	}
	if !s.seen || value > s.max {
		s.max = value
	}
	s.seen = true
	idx := bucketIndex(boundsFor(id), value)
	if s.buckets[idx] == math.MaxUint64 {
		s.countOverflow = true
	} else {
		s.buckets[idx]++
	}
}

func (r *Recorder) Snapshot() Snapshot {
	out := Snapshot{SchemaVersion: SchemaVersion, Counters: make([]CounterSnapshot, 0, CounterCount), Gauges: make([]GaugeSnapshot, 0, GaugeCount), Distributions: make([]DistributionSnapshot, 0, DistributionCount)}
	if r == nil {
		return out
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := Counter(0); id < CounterCount; id++ {
		unit := "count"
		if id == CaptureInputBytes {
			unit = "bytes"
		}
		out.Counters = append(out.Counters, CounterSnapshot{Name: counterNames[id], Unit: unit, Value: r.counters[id], Overflow: r.counterOverflow[id]})
	}
	for id := Gauge(0); id < GaugeCount; id++ {
		out.Gauges = append(out.Gauges, GaugeSnapshot{Name: gaugeNames[id], Unit: "items", Value: r.gauges[id], Overflow: r.gaugeOverflow[id]})
	}
	for id := Distribution(0); id < DistributionCount; id++ {
		s := r.distributions[id]
		bounds := boundsFor(id)
		counts := s.buckets
		if counts == nil {
			counts = make([]uint64, len(bounds)+1)
		}
		buckets := make([]BucketSnapshot, len(counts))
		for i, count := range counts {
			if i < len(bounds) {
				bound := bounds[i]
				buckets[i] = BucketSnapshot{UpperBound: &bound, Count: count}
			} else {
				buckets[i] = BucketSnapshot{Overflow: true, Count: count}
			}
		}
		d := DistributionSnapshot{Name: distributionNames[id], Unit: unitFor(id), Count: s.count, CountOverflow: s.countOverflow, Sum: s.sum, SumOverflow: s.sumOverflow, Buckets: buckets, QuantileMethod: "bucket_upper_bound", Overflow: s.countOverflow || s.sumOverflow}
		if s.seen {
			min, max := s.min, s.max
			d.Min, d.Max = &min, &max
			d.P50UpperBound = quantileUpperBound(counts, bounds, s.count, s.countOverflow, 50)
			d.P95UpperBound = quantileUpperBound(counts, bounds, s.count, s.countOverflow, 95)
			d.P99UpperBound = quantileUpperBound(counts, bounds, s.count, s.countOverflow, 99)
		}
		out.Distributions = append(out.Distributions, d)
	}
	return out
}

// MarshalJSON is implemented to keep Snapshot values directly encodable while
// ensuring recorder state is represented only by its bounded fixed fields.
func (s Snapshot) MarshalJSON() ([]byte, error) {
	type wire Snapshot
	return json.Marshal(wire(s))
}

func quantileUpperBound(counts, bounds []uint64, total uint64, countOverflow bool, percentile uint64) *uint64 {
	if total == 0 || countOverflow {
		return nil
	}
	// ceil(total * percentile / 100), without multiplication overflow.
	rank := total/100*percentile + (total%100*percentile+99)/100
	var seen uint64
	for i, n := range counts {
		seen += n
		if seen >= rank {
			if i >= len(bounds) {
				return nil
			}
			bound := bounds[i]
			return &bound
		}
	}
	return nil
}

func bucketIndex(bounds []uint64, value uint64) int {
	for i, bound := range bounds {
		if value <= bound {
			return i
		}
	}
	return len(bounds)
}

func boundsFor(id Distribution) []uint64 {
	switch id {
	case SnapshotBytes, ObservationBytes, JournalBytes:
		return byteBounds[:]
	case ActorQueueDepth:
		return queueBounds[:]
	default:
		return durationBounds[:]
	}
}

func unitFor(id Distribution) string {
	switch id {
	case SnapshotBytes, ObservationBytes, JournalBytes:
		return "bytes"
	case ActorQueueDepth:
		return "items"
	default:
		return "ns"
	}
}

var counterNames = [...]string{"actor_admitted", "actor_rejected", "actor_completed", "actor_failed", "db_transactions_started", "db_transactions_committed", "db_transactions_rolled_back", "db_commit_errors", "db_rollback_errors", "db_acquire_errors", "db_queries", "db_query_errors", "db_durable_transitions", "db_observation_writes", "db_observation_duplicates", "inference_requests", "inference_rejected", "inference_rpcs", "inference_errors", "market_events", "market_errors", "policy_decisions", "policy_fallbacks", "capture_frames", "capture_input_bytes", "capture_persist_attempts", "capture_persist_errors", "capture_delivery_attempts", "capture_delivery_errors", "public_ingress_requests", "public_ingress_errors", "public_receipt_origin_missing"}
var gaugeNames = [...]string{"actor_queued", "actor_handling", "db_transactions_in_flight", "inference_in_flight"}
var distributionNames = [...]string{"actor_queue_wait_ns", "actor_handle_ns", "control_ack_ns", "public_market_apply_ns", "inference_rpc_ns", "decision_commit_ns", "db_acquire_ns", "db_begin_ns", "db_transaction_ns", "db_commit_ns", "db_rollback_ns", "db_account_for_update_roundtrip_ns", "db_snapshot_write_attempt_bytes", "db_observation_write_attempt_bytes", "db_journal_write_attempt_bytes", "actor_queue_depth", "capture_persist_ns", "capture_read_to_durable_ns", "capture_receipt_to_consumer_ns", "public_ingress_ns", "public_receipt_to_book_ns"}

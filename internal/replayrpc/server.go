// Package replayrpc exposes bounded, local, in-memory replay batches over gRPC.
package replayrpc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	pb "github.com/liquiditylab/liquiditylab/gen/go/liquiditylab/v1"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"github.com/liquiditylab/liquiditylab/internal/replay"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Limits bound both admission and each retained resource. Immutable tape is shared;
// charging its serialized size per environment leaves headroom for engine state.
type Limits struct {
	MaxBatches, MaxEnvironments, MaxEnvironmentsPerBatch     int
	MaxEvents, MaxLevels, MaxLevelsPerEvent                  int
	MaxMessageBytes, MaxReportBytes, MaxRetainedEpisodeBytes int
	MaxConcurrentRPCs, MaxSteps, MaxHistoryItems             int
	MaxCachedOperations, MaxCachedOperationBytes             int
	MaxCreateCache, MaxCloseTombstones                       int
	MaxCreateIdentities, MaxOperationsPerBatch               int
	IdleTTL                                                  time.Duration
}

func DefaultLimits() Limits {
	return Limits{
		MaxBatches: 16, MaxEnvironments: 128, MaxEnvironmentsPerBatch: 32,
		MaxEvents: 20000, MaxLevels: 100000, MaxLevelsPerEvent: 512,
		MaxMessageBytes: 8 << 20, MaxReportBytes: 4 << 20, MaxRetainedEpisodeBytes: 128 << 20,
		MaxConcurrentRPCs: 32, MaxSteps: 10000, MaxHistoryItems: 20000,
		MaxCachedOperations: 8, MaxCachedOperationBytes: (8 << 20) + 512,
		MaxCreateCache: 128, MaxCloseTombstones: 128,
		MaxCreateIdentities: 4096, MaxOperationsPerBatch: 10000,
		IdleTTL: 15 * time.Minute,
	}
}

type cachedOperation struct {
	sequence          uint64
	requestID, method string
	digest            [32]byte
	result            proto.Message
	err               error
	bytes             int
}
type batch struct {
	gate                             chan struct{}
	id                               string
	environments                     map[string]*replay.Engine
	next                             uint64
	poisoned, closed                 bool
	cache                            []cachedOperation
	cacheBytes, retainedBytes, count int
	lastUsed                         time.Time
	identities                       map[string]cachedOperation
}

func (b *batch) lock(ctx context.Context) error {
	select {
	case b.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-b.gate
			return status.FromContextError(err).Err()
		}
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}
func (b *batch) unlock() { b.lastUsed = time.Now(); <-b.gate }

type createEntry struct {
	digest  [32]byte
	done    chan struct{}
	result  *pb.CreateBatchResponse
	err     error
	batchID string
}

type Server struct {
	pb.UnimplementedReplayServiceServer
	limits                          Limits
	instance                        string
	admission                       chan struct{}
	mu                              sync.Mutex
	batches                         map[string]*batch
	closed                          map[string]*batch
	closedOrder                     []string
	creates                         map[string]*createEntry
	createIdentities                map[string][32]byte
	createOrder                     []string
	environmentCount, retainedBytes int
	stop, stopped                   chan struct{}
	stopOnce                        sync.Once
}

func New(limits Limits) (*Server, error) {
	values := []int{limits.MaxBatches, limits.MaxEnvironments, limits.MaxEnvironmentsPerBatch, limits.MaxEvents, limits.MaxLevels, limits.MaxLevelsPerEvent, limits.MaxMessageBytes, limits.MaxReportBytes, limits.MaxRetainedEpisodeBytes, limits.MaxConcurrentRPCs, limits.MaxSteps, limits.MaxHistoryItems, limits.MaxCachedOperations, limits.MaxCachedOperationBytes, limits.MaxCreateCache, limits.MaxCloseTombstones, limits.MaxCreateIdentities, limits.MaxOperationsPerBatch}
	for _, v := range values {
		if v <= 0 {
			return nil, fmt.Errorf("all replay resource limits must be positive")
		}
	}
	if limits.IdleTTL <= 0 {
		return nil, fmt.Errorf("idle TTL must be positive")
	}
	if limits.MaxCreateCache < limits.MaxBatches || limits.MaxMessageBytes > math.MaxInt32 || limits.MaxCachedOperationBytes < limits.MaxMessageBytes+512 || limits.MaxCreateIdentities < limits.MaxCreateCache {
		return nil, fmt.Errorf("inconsistent replay resource limits")
	}
	instance, err := randomID("engine")
	if err != nil {
		return nil, err
	}
	s := &Server{limits: limits, instance: instance, admission: make(chan struct{}, limits.MaxConcurrentRPCs), batches: map[string]*batch{}, closed: map[string]*batch{}, creates: map[string]*createEntry{}, createIdentities: map[string][32]byte{}, stop: make(chan struct{}), stopped: make(chan struct{})}
	go s.sweep()
	return s, nil
}

// Shutdown stops idle reclamation after the gRPC server has drained its calls.
func (s *Server) Shutdown() { s.stopOnce.Do(func() { close(s.stop) }); <-s.stopped }
func (s *Server) sweep() {
	defer close(s.stopped)
	interval := min(s.limits.IdleTTL/2, 30*time.Second)
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-ticker.C:
			s.reap(now)
		}
	}
}
func (s *Server) reap(now time.Time) {
	s.mu.Lock()
	batches := make([]*batch, 0, len(s.batches))
	for _, b := range s.batches {
		batches = append(batches, b)
	}
	s.mu.Unlock()
	for _, b := range batches {
		select {
		case b.gate <- struct{}{}:
		default:
			continue
		}
		if now.Sub(b.lastUsed) >= s.limits.IdleTTL {
			s.mu.Lock()
			if s.batches[b.id] == b {
				b.closed = true
				b.environments = nil
				delete(s.batches, b.id)
				s.environmentCount -= b.count
				s.retainedBytes -= b.retainedBytes
			}
			s.mu.Unlock()
		}
		<-b.gate
	}
}
func (s *Server) InstanceID() string { return s.instance }
func randomID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(b[:]), nil
}
func (s *Server) admit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	select {
	case s.admission <- struct{}{}:
		return nil
	default:
		return status.Error(codes.ResourceExhausted, "replay RPC concurrency limit reached")
	}
}
func (s *Server) release()   { <-s.admission }
func validID(id string) bool { return len(id) > 0 && len(id) <= 128 }
func (s *Server) fingerprint(request proto.Message) ([32]byte, error) {
	if request == nil || !request.ProtoReflect().IsValid() {
		return [32]byte{}, status.Error(codes.InvalidArgument, "missing request")
	}
	if proto.Size(request) > s.limits.MaxMessageBytes {
		return [32]byte{}, status.Error(codes.ResourceExhausted, "request exceeds message limit")
	}
	b, err := (proto.MarshalOptions{Deterministic: true}).Marshal(request)
	if err != nil {
		return [32]byte{}, status.Error(codes.InvalidArgument, "invalid protobuf request")
	}
	return sha256.Sum256(b), nil
}

func (s *Server) CreateBatch(ctx context.Context, req *pb.CreateBatchRequest) (response *pb.CreateBatchResponse, err error) {
	if err = s.admit(ctx); err != nil {
		return nil, err
	}
	defer s.release()
	digest, err := s.fingerprint(req)
	if err != nil {
		return nil, err
	}
	if !validID(req.RequestId) || req.Count == 0 || uint64(req.Count) > uint64(s.limits.MaxEnvironmentsPerBatch) {
		return nil, status.Error(codes.InvalidArgument, "request identity or environment count invalid")
	}
	s.mu.Lock()
	if existing := s.creates[req.RequestId]; existing != nil {
		s.mu.Unlock()
		if digest != existing.digest {
			return nil, status.Error(codes.AlreadyExists, "request ID reused with different CreateBatch payload")
		}
		select {
		case <-existing.done:
			if existing.result == nil {
				return nil, existing.err
			}
			return proto.Clone(existing.result).(*pb.CreateBatchResponse), existing.err
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	if prior, exists := s.createIdentities[req.RequestId]; exists {
		s.mu.Unlock()
		if prior != digest {
			return nil, status.Error(codes.AlreadyExists, "CreateBatch identity reused with different payload")
		}
		return nil, status.Error(codes.FailedPrecondition, "CreateBatch response expired; create a new identity")
	}
	if len(s.createIdentities) >= s.limits.MaxCreateIdentities {
		s.mu.Unlock()
		return nil, status.Error(codes.ResourceExhausted, "process create identity limit reached; restart the replay process")
	}
	// In-progress and active create records remain pinned. Finished records whose
	// batches were closed are the only candidates for bounded retry-cache eviction.
	for len(s.creates) >= s.limits.MaxCreateCache {
		removed := false
		for i, id := range s.createOrder {
			entry := s.creates[id]
			select {
			case <-entry.done:
				if s.batches[entry.batchID] == nil {
					delete(s.creates, id)
					s.createOrder = append(s.createOrder[:i], s.createOrder[i+1:]...)
					removed = true
				}
			default:
			}
			if removed {
				break
			}
		}
		if !removed {
			s.mu.Unlock()
			return nil, status.Error(codes.ResourceExhausted, "create identity cache is full")
		}
	}
	retained := proto.Size(req.GetEpisode())
	if retained == 0 || retained > s.limits.MaxRetainedEpisodeBytes/int(req.Count) || len(s.batches) >= s.limits.MaxBatches || int(req.Count) > s.limits.MaxEnvironments-s.environmentCount || retained*int(req.Count) > s.limits.MaxRetainedEpisodeBytes-s.retainedBytes {
		s.mu.Unlock()
		return nil, status.Error(codes.ResourceExhausted, "batch, environment, or retained episode limit reached")
	}
	batchID, randomErr := randomID(s.instance + "-batch")
	if randomErr != nil {
		s.mu.Unlock()
		return nil, status.Error(codes.Internal, "cannot allocate batch identity")
	}
	b := &batch{gate: make(chan struct{}, 1), id: batchID, next: 1, environments: map[string]*replay.Engine{}, count: int(req.Count), retainedBytes: retained * int(req.Count), lastUsed: time.Now(), identities: map[string]cachedOperation{}}
	b.gate <- struct{}{}
	entry := &createEntry{digest: digest, done: make(chan struct{}), batchID: batchID}
	s.creates[req.RequestId] = entry
	s.createIdentities[req.RequestId] = digest
	s.createOrder = append(s.createOrder, req.RequestId)
	s.batches[batchID] = b
	s.environmentCount += b.count
	s.retainedBytes += b.retainedBytes
	s.mu.Unlock()
	defer func() {
		if recovered := recover(); recovered != nil {
			response = nil
			err = status.Error(codes.InvalidArgument, "episode arithmetic exceeds engine envelope")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			delete(s.batches, batchID)
			s.environmentCount -= b.count
			s.retainedBytes -= b.retainedBytes
		}
		entry.err = err
		if response != nil {
			entry.result = proto.Clone(response).(*pb.CreateBatchResponse)
		}
		close(entry.done)
		b.unlock()
	}()
	events, plan, rules, fees, config, parseErr := episode(req.Episode, s.limits)
	if parseErr != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid episode: %v", parseErr)
	}
	tape, tapeErr := replay.NewTape(events)
	if tapeErr != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tape: %v", tapeErr)
	}
	response = &pb.CreateBatchResponse{BatchId: batchID, EngineVersion: d.EnvironmentVersion, ObservationVersion: replay.ObservationVersion, EngineInstanceId: s.instance, NextSequence: 1}
	for range int(req.Count) {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		engine, newErr := replay.NewWithTape(tape, plan, rules, fees, config)
		if newErr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid episode: %v", newErr)
		}
		id, idErr := randomID("env")
		if idErr != nil {
			return nil, status.Error(codes.Internal, "cannot allocate environment identity")
		}
		b.environments[id] = engine
		response.EnvironmentIds = append(response.EnvironmentIds, id)
	}
	if proto.Size(response) > s.limits.MaxMessageBytes {
		return nil, status.Error(codes.ResourceExhausted, "create response exceeds message limit")
	}
	return response, nil
}

func (s *Server) find(id string, allowClosed bool) (*batch, error) {
	if !validID(id) {
		return nil, status.Error(codes.InvalidArgument, "invalid batch ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.batches[id]; b != nil {
		return b, nil
	}
	if allowClosed {
		if b := s.closed[id]; b != nil {
			return b, nil
		}
	}
	return nil, status.Error(codes.NotFound, "unknown batch; engine restart and expired handles require a new batch")
}

func (s *Server) prior(b *batch, method, id string, sequence uint64, digest [32]byte) (proto.Message, error, bool) {
	if !validID(id) || sequence == 0 {
		return nil, status.Error(codes.InvalidArgument, "request ID and positive sequence required"), true
	}
	for _, cached := range b.cache {
		if cached.sequence == sequence || cached.requestID == id {
			if cached.sequence != sequence || cached.requestID != id || cached.method != method || cached.digest != digest {
				return nil, status.Error(codes.AlreadyExists, "operation identity reused with different request"), true
			}
			if cached.result != nil {
				return proto.Clone(cached.result), cached.err, true
			}
			return nil, cached.err, true
		}
	}
	if previous, exists := b.identities[id]; exists {
		if previous.sequence != sequence || previous.method != method || previous.digest != digest {
			return nil, status.Error(codes.AlreadyExists, "request ID reused after response eviction"), true
		}
		return nil, status.Error(codes.FailedPrecondition, "operation response expired; operation was already applied"), true
	}
	if sequence != b.next {
		return nil, status.Errorf(codes.FailedPrecondition, "sequence is stale or out of order; expected %d; expired results cannot be replayed", b.next), true
	}
	if sequence == math.MaxUint64 {
		return nil, status.Error(codes.FailedPrecondition, "sequence exhausted; create a new batch"), true
	}
	if b.closed {
		return nil, status.Error(codes.FailedPrecondition, "batch is closed"), true
	}
	if b.poisoned && method != "close" {
		return nil, status.Error(codes.FailedPrecondition, "batch invalidated by engine failure; close and recreate"), true
	}
	if method != "close" && len(b.identities) >= s.limits.MaxOperationsPerBatch {
		return nil, status.Error(codes.ResourceExhausted, "batch operation identity limit reached; close and recreate"), true
	}
	return nil, nil, false
}

func (s *Server) remember(b *batch, method, id string, sequence uint64, digest [32]byte, result proto.Message, err error) {
	item := cachedOperation{sequence: sequence, requestID: id, method: method, digest: digest, err: err, bytes: 256 + len(id)}
	if result != nil {
		item.result = proto.Clone(result)
		item.bytes += proto.Size(result)
	}
	b.cache = append(b.cache, item)
	b.identities[id] = cachedOperation{sequence: sequence, requestID: id, method: method, digest: digest}
	b.cacheBytes += item.bytes
	b.next++
	for len(b.cache) > s.limits.MaxCachedOperations || b.cacheBytes > s.limits.MaxCachedOperationBytes {
		b.cacheBytes -= b.cache[0].bytes
		b.cache[0] = cachedOperation{}
		b.cache = b.cache[1:]
	}
}

// mutate deliberately completes once admitted to the engine. Transport cancellation
// may hide the response but never interrupts half a batch or causes a second advance.
func (s *Server) mutate(b *batch, method, id string, sequence uint64, digest [32]byte, operation func() (*pb.BatchTransitions, error)) (response *pb.BatchTransitions, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			response = nil
			err = status.Error(codes.Internal, "engine panic; batch invalidated")
		}
		if err != nil {
			b.poisoned = true
			response = nil
		}
		s.remember(b, method, id, sequence, digest, response, err)
	}()
	response, err = operation()
	if response != nil && proto.Size(response) > s.limits.MaxMessageBytes {
		response = nil
		err = status.Error(codes.ResourceExhausted, "transition response exceeds message limit; batch invalidated")
	}
	return response, err
}

func (s *Server) ResetBatch(ctx context.Context, req *pb.ResetBatchRequest) (*pb.BatchTransitions, error) {
	if err := s.admit(ctx); err != nil {
		return nil, err
	}
	defer s.release()
	digest, err := s.fingerprint(req)
	if err != nil {
		return nil, err
	}
	b, err := s.find(req.BatchId, false)
	if err != nil {
		return nil, err
	}
	if err = b.lock(ctx); err != nil {
		return nil, err
	}
	defer b.unlock()
	if prior, err, ok := s.prior(b, "reset", req.RequestId, req.Sequence, digest); ok {
		if prior == nil {
			return nil, err
		}
		return prior.(*pb.BatchTransitions), err
	}
	if len(req.Resets) == 0 || len(req.Resets) > b.count {
		return nil, status.Error(codes.InvalidArgument, "invalid reset count")
	}
	seen := map[string]bool{}
	for _, reset := range req.Resets {
		if reset == nil || seen[reset.EnvironmentId] || b.environments[reset.EnvironmentId] == nil {
			return nil, status.Error(codes.InvalidArgument, "unknown or duplicate environment handle")
		}
		seen[reset.EnvironmentId] = true
	}
	return s.mutate(b, "reset", req.RequestId, req.Sequence, digest, func() (*pb.BatchTransitions, error) {
		out := &pb.BatchTransitions{BatchId: b.id, Sequence: req.Sequence}
		// Seed is an accepted deterministic identifier. The current fill model has
		// no randomness; different seeds therefore yield identical trajectories.
		for _, reset := range req.Resets {
			r, err := b.environments[reset.EnvironmentId].Reset()
			if err != nil {
				return nil, status.Errorf(codes.Internal, "engine reset failed: %v", err)
			}
			t, err := transition(reset.EnvironmentId, r)
			if err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			out.Transitions = append(out.Transitions, t)
		}
		return out, nil
	})
}

func (s *Server) StepBatch(ctx context.Context, req *pb.StepBatchRequest) (*pb.BatchTransitions, error) {
	if err := s.admit(ctx); err != nil {
		return nil, err
	}
	defer s.release()
	digest, err := s.fingerprint(req)
	if err != nil {
		return nil, err
	}
	b, err := s.find(req.BatchId, false)
	if err != nil {
		return nil, err
	}
	if err = b.lock(ctx); err != nil {
		return nil, err
	}
	defer b.unlock()
	if prior, err, ok := s.prior(b, "step", req.RequestId, req.Sequence, digest); ok {
		if prior == nil {
			return nil, err
		}
		return prior.(*pb.BatchTransitions), err
	}
	if len(req.Actions) == 0 || len(req.Actions) > b.count {
		return nil, status.Error(codes.InvalidArgument, "invalid action count")
	}
	seen := map[string]bool{}
	for _, action := range req.Actions {
		if action == nil || seen[action.EnvironmentId] || b.environments[action.EnvironmentId] == nil {
			return nil, status.Error(codes.InvalidArgument, "unknown or duplicate environment handle")
		}
		seen[action.EnvironmentId] = true
		if err := d.Action(action.Action).Validate(); err != nil {
			return nil, status.Error(codes.InvalidArgument, "unknown semantic action; zero is invalid")
		}
		e := b.environments[action.EnvironmentId]
		if e.Terminated || e.Truncated {
			return nil, status.Error(codes.FailedPrecondition, "episode ended; reset before stepping")
		}
		if len(e.Decisions) >= s.limits.MaxSteps {
			return nil, status.Error(codes.ResourceExhausted, "episode step limit reached; reset before stepping")
		}
	}
	return s.mutate(b, "step", req.RequestId, req.Sequence, digest, func() (*pb.BatchTransitions, error) {
		out := &pb.BatchTransitions{BatchId: b.id, Sequence: req.Sequence}
		for _, action := range req.Actions {
			e := b.environments[action.EnvironmentId]
			r, err := e.Step(d.Action(action.Action))
			if err != nil {
				return nil, status.Errorf(codes.Internal, "engine step failed: %v", err)
			}
			if len(e.Ledger.Fills)+len(e.Orders)+len(e.Decisions) > s.limits.MaxHistoryItems {
				return nil, status.Error(codes.ResourceExhausted, "episode history limit exceeded; batch invalidated")
			}
			t, err := transition(action.EnvironmentId, r)
			if err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			out.Transitions = append(out.Transitions, t)
		}
		return out, nil
	})
}

func (s *Server) GetReports(ctx context.Context, req *pb.GetReportsRequest) (response *pb.GetReportsResponse, err error) {
	if err = s.admit(ctx); err != nil {
		return nil, err
	}
	defer s.release()
	if _, err = s.fingerprint(req); err != nil {
		return nil, err
	}
	b, err := s.find(req.BatchId, false)
	if err != nil {
		return nil, err
	}
	if err = b.lock(ctx); err != nil {
		return nil, err
	}
	defer b.unlock()
	if b.closed || b.poisoned {
		return nil, status.Error(codes.FailedPrecondition, "batch is closed or invalidated")
	}
	if len(req.EnvironmentIds) == 0 || len(req.EnvironmentIds) > b.count {
		return nil, status.Error(codes.InvalidArgument, "report handles required")
	}
	seen := map[string]bool{}
	for _, id := range req.EnvironmentIds {
		if seen[id] || b.environments[id] == nil {
			return nil, status.Error(codes.InvalidArgument, "unknown or duplicate environment handle")
		}
		seen[id] = true
		if e := b.environments[id]; !e.Terminated && !e.Truncated {
			return nil, status.Error(codes.FailedPrecondition, "final report requires an ended episode")
		}
	}
	defer func() {
		if recover() != nil {
			b.poisoned = true
			response = nil
			err = status.Error(codes.Internal, "report panic; batch invalidated")
		}
	}()
	response = &pb.GetReportsResponse{}
	for _, id := range req.EnvironmentIds {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		e := b.environments[id]
		report := e.Report()
		detail, jsonErr := json.Marshal(report)
		if jsonErr != nil {
			return nil, status.Error(codes.Internal, "report serialization failed")
		}
		if len(detail) > s.limits.MaxReportBytes {
			return nil, status.Error(codes.ResourceExhausted, "report exceeds report byte limit")
		}
		cost, ok := report["marked_cost_quote"].(d.Decimal)
		if !ok {
			b.poisoned = true
			return nil, status.Error(codes.Internal, "invalid report cost type")
		}
		wire := &pb.ExecutionReport{EconomicScoreValid: e.Terminated && !e.Truncated, Status: e.Status, FilledQuantity: wireDecimal(e.Ledger.Filled), RemainingQuantity: wireDecimal(e.Ledger.Remaining()), MarkedCostQuote: wireDecimal(cost), DetailJson: detail}
		for _, fill := range e.Ledger.Fills {
			feeCurrency := pb.FeeCurrency_FEE_QUOTE
			if fill.FeeCurrency == "base" {
				feeCurrency = pb.FeeCurrency_FEE_BASE
			}
			fillSide := pb.Side_SIDE_BUY
			if fill.Side == "sell" {
				fillSide = pb.Side_SIDE_SELL
			}
			wire.Fills = append(wire.Fills, &pb.Fill{ExecutionId: fill.ExecutionID, OrderId: fill.OrderID, Side: fillSide, Quantity: wireDecimal(fill.Quantity), Price: wireDecimal(fill.Price), Fee: wireDecimal(fill.Fee), FeeCurrency: feeCurrency, ReceiptTimeNs: fill.Time, Maker: fill.Maker})
		}
		response.Reports = append(response.Reports, wire)
		if proto.Size(response) > s.limits.MaxMessageBytes {
			return nil, status.Error(codes.ResourceExhausted, "reports exceed response limit; request fewer environments")
		}
	}
	return response, nil
}

func (s *Server) CloseBatch(ctx context.Context, req *pb.CloseBatchRequest) (*pb.CloseBatchResponse, error) {
	if err := s.admit(ctx); err != nil {
		return nil, err
	}
	defer s.release()
	digest, err := s.fingerprint(req)
	if err != nil {
		return nil, err
	}
	b, err := s.find(req.BatchId, true)
	if err != nil {
		return nil, err
	}
	if err = b.lock(ctx); err != nil {
		return nil, err
	}
	defer b.unlock()
	if prior, err, ok := s.prior(b, "close", req.RequestId, req.Sequence, digest); ok {
		if prior == nil {
			return nil, err
		}
		return prior.(*pb.CloseBatchResponse), err
	}
	response := &pb.CloseBatchResponse{Closed: true, Sequence: req.Sequence}
	s.remember(b, "close", req.RequestId, req.Sequence, digest, response, nil)
	b.closed = true
	b.environments = nil
	// A close tombstone retains only the close outcome, releasing step histories.
	last := b.cache[len(b.cache)-1]
	b.cache = []cachedOperation{last}
	b.cacheBytes = last.bytes
	b.identities = map[string]cachedOperation{req.RequestId: {sequence: req.Sequence, requestID: req.RequestId, method: "close", digest: digest}}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.batches, b.id)
	s.environmentCount -= b.count
	s.retainedBytes -= b.retainedBytes
	s.closed[b.id] = b
	s.closedOrder = append(s.closedOrder, b.id)
	for len(s.closedOrder) > s.limits.MaxCloseTombstones {
		delete(s.closed, s.closedOrder[0])
		s.closedOrder = s.closedOrder[1:]
	}
	return response, nil
}

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"sync"
	"time"

	pb "github.com/liquiditylab/liquiditylab/gen/go/liquiditylab/v1"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	"github.com/liquiditylab/liquiditylab/internal/inference"
	ar "github.com/liquiditylab/liquiditylab/internal/runtime"
	"github.com/liquiditylab/liquiditylab/internal/telemetry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func mockPolicySHA256() string {
	sum := sha256.Sum256([]byte("LiquidityLab-controlled-HOLD-unvalidated-v1"))
	return hex.EncodeToString(sum[:])
}

type workerStats struct {
	Requests    uint64 `json:"requests"`
	Succeeded   uint64 `json:"succeeded"`
	Failed      uint64 `json:"failed"`
	Rejected    uint64 `json:"rejected"`
	Expired     uint64 `json:"expired"`
	InFlight    int    `json:"in_flight"`
	MaxInFlight int    `json:"max_in_flight"`
}
type policyReport struct {
	Kind            string      `json:"kind"`
	SHA256          string      `json:"policy_sha256,omitempty"`
	PromotionStatus string      `json:"verified_promotion_status,omitempty"`
	Profile         string      `json:"profile,omitempty"`
	Fallback        string      `json:"fallback,omitempty"`
	TimeoutNS       int64       `json:"timeout_ns,omitempty"`
	MockDelayNS     int64       `json:"mock_delay_ns,omitempty"`
	MockAction      string      `json:"mock_action,omitempty"`
	Before          workerStats `json:"client_before_measurement"`
	After           workerStats `json:"client_after_measurement"`
	Quiescent       workerStats `json:"client_after_shutdown"`
	Quiescence      string      `json:"client_quiescence"`
	Caveat          string      `json:"caveat"`
}

// trackedWorker closes client admission under the same mutex that registers
// calls. It prevents Add/Wait races, including an actor goroutine that starts
// Propose only after actor.Close has returned.
type trackedWorker struct {
	client  *inference.Client
	mu      sync.Mutex
	closing bool
	idle    chan struct{}
	stats   workerStats
}

func newTrackedWorker(client *inference.Client) *trackedWorker {
	idle := make(chan struct{})
	close(idle)
	return &trackedWorker{client: client, idle: idle}
}
func (w *trackedWorker) Propose(ctx context.Context, req *pb.DecisionRequest) (inference.Proposal, error) {
	w.mu.Lock()
	if w.closing {
		w.mu.Unlock()
		return inference.Proposal{}, inference.ErrBusy
	}
	if w.stats.InFlight == 0 {
		w.idle = make(chan struct{})
	}
	w.stats.InFlight++
	w.stats.Requests++
	w.stats.MaxInFlight = max(w.stats.MaxInFlight, w.stats.InFlight)
	w.mu.Unlock()
	proposal, err := w.client.Propose(ctx, req)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.InFlight--
	if err == nil {
		w.stats.Succeeded++
	} else {
		w.stats.Failed++
		if errors.Is(err, inference.ErrBusy) {
			w.stats.Rejected++
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, inference.ErrStale) {
			w.stats.Expired++
		}
	}
	if w.stats.InFlight == 0 {
		close(w.idle)
	}
	return proposal, err
}
func (w *trackedWorker) Snapshot() workerStats {
	if w == nil {
		return workerStats{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
}
func (w *trackedWorker) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	w.closing = true
	idle := w.idle
	w.mu.Unlock()
	_ = w.client.Close()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-idle:
		return nil
	case <-timer.C:
		return errors.New("inference client calls did not quiesce")
	}
}

type policyHarness struct {
	worker   *trackedWorker
	mock     *mockPolicyHarness
	report   policyReport
	once     sync.Once
	closeErr error
}

func startPolicy(ctx context.Context, c config, rec *telemetry.Recorder) (*policyHarness, error) {
	h := &policyHarness{report: policyReport{Kind: "none", Caveat: "Go baseline; no inference configured"}}
	if c.PolicyTarget == "" && c.MockPolicyDelay < 0 {
		return h, nil
	}
	target, hash := c.PolicyTarget, c.PolicySHA256
	h.report = policyReport{Kind: "external_local_worker", Profile: c.PolicyProfile, Fallback: c.PolicyFallback, TimeoutNS: int64(c.PolicyTimeout), Caveat: "Explicit unvalidated local worker; model quality, external worker CPU/memory, and external server idleness are unmeasured. Quiescence covers this process's RPC client only."}
	if c.MockPolicyDelay >= 0 {
		mock, err := startMockPolicy(c.MockPolicyDelay)
		if err != nil {
			return nil, err
		}
		h.mock = mock
		target = mock.listener.Addr().String()
		hash = mockPolicySHA256()
		h.report.Kind = "controlled_local_grpc"
		h.report.MockDelayNS = int64(c.MockPolicyDelay)
		h.report.MockAction = "HOLD"
		h.report.Caveat = "Synthetic unvalidated HOLD fixture; honors cancellation and admits one Predict at a time. No policy quality claim. Paper timeout fallback can execute the pinned Go baseline."
	}
	client, err := inference.New(ctx, inference.Config{Target: target, PolicySHA256: hash, Profile: c.PolicyProfile, AllowUnvalidated: true, Timeout: c.PolicyTimeout, Telemetry: rec})
	if err != nil {
		if h.mock != nil {
			h.mock.Close()
		}
		return nil, errors.New("connect and verify pinned policy worker failed")
	}
	h.worker = newTrackedWorker(client)
	h.report.SHA256 = hash
	h.report.PromotionStatus = client.PromotionStatus()
	return h, nil
}
func (h *policyHarness) Config(c config) *ar.PolicyConfig {
	if h == nil || h.worker == nil {
		return nil
	}
	return &ar.PolicyConfig{Worker: h.worker, PolicySHA256: h.report.SHA256, Profile: c.PolicyProfile, ExpectedMode: h.report.PromotionStatus, AllowUnvalidated: true, Fallback: c.PolicyFallback, Timeout: c.PolicyTimeout}
}
func (h *policyHarness) Close() error {
	if h == nil {
		return nil
	}
	h.once.Do(func() {
		h.closeErr = h.worker.Close()
		if h.mock != nil {
			h.mock.Close()
		}
	})
	return h.closeErr
}

type mockPolicyService struct {
	pb.UnimplementedPolicyServiceServer
	delay time.Duration
	slot  chan struct{}
}

func (s *mockPolicyService) GetPolicyInfo(_ context.Context, req *pb.PolicyInfoRequest) (*pb.PolicyInfoResponse, error) {
	if req == nil || req.PolicySha256 != mockPolicySHA256() {
		return nil, status.Error(codes.InvalidArgument, "policy identity mismatch")
	}
	return &pb.PolicyInfoResponse{PolicySha256: mockPolicySHA256(), ObservationVersion: x.ObservationVersion, FieldNames: append([]string(nil), x.ObservationFields...), PromotionStatus: "unvalidated", LiveEligible: false}, nil
}
func (s *mockPolicyService) Predict(ctx context.Context, req *pb.DecisionRequest) (*pb.DecisionResponse, error) {
	if req == nil || req.Binding == nil || req.Binding.PolicySha256 != mockPolicySHA256() {
		return nil, status.Error(codes.InvalidArgument, "invalid binding")
	}
	select {
	case s.slot <- struct{}{}:
		defer func() { <-s.slot }()
	default:
		return nil, status.Error(codes.ResourceExhausted, "controlled worker busy")
	}
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return &pb.DecisionResponse{Binding: proto.Clone(req.Binding).(*pb.DecisionBinding), Action: pb.Action(d.Hold), Mode: "unvalidated"}, nil
}

type mockPolicyHarness struct {
	listener net.Listener
	server   *grpc.Server
	done     chan struct{}
	once     sync.Once
}

func startMockPolicy(delay time.Duration) (*mockPolicyHarness, error) {
	if delay < 0 || delay > 5*time.Second {
		return nil, errors.New("controlled worker delay invalid")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("controlled policy listener failed")
	}
	h := &mockPolicyHarness{listener: listener, server: grpc.NewServer(grpc.MaxRecvMsgSize(8192), grpc.MaxSendMsgSize(4096), grpc.MaxConcurrentStreams(2)), done: make(chan struct{})}
	pb.RegisterPolicyServiceServer(h.server, &mockPolicyService{delay: delay, slot: make(chan struct{}, 1)})
	go func() { defer close(h.done); _ = h.server.Serve(listener) }()
	return h, nil
}
func (h *mockPolicyHarness) Close() {
	h.once.Do(func() { h.server.Stop(); _ = h.listener.Close(); <-h.done })
}

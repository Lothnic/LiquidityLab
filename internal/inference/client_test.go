package inference

import (
	"context"
	"errors"
	"math"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/liquiditylab/liquiditylab/gen/go/liquiditylab/v1"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const testPolicyHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type testPolicyService struct {
	pb.UnimplementedPolicyServiceServer
	info    func(context.Context, *pb.PolicyInfoRequest) (*pb.PolicyInfoResponse, error)
	predict func(context.Context, *pb.DecisionRequest) (*pb.DecisionResponse, error)
}

func (s *testPolicyService) GetPolicyInfo(ctx context.Context, req *pb.PolicyInfoRequest) (*pb.PolicyInfoResponse, error) {
	if s.info != nil {
		return s.info(ctx, req)
	}
	return testPolicyInfo(), nil
}

func (s *testPolicyService) Predict(ctx context.Context, req *pb.DecisionRequest) (*pb.DecisionResponse, error) {
	if s.predict != nil {
		return s.predict(ctx, req)
	}
	return &pb.DecisionResponse{Binding: req.Binding, Action: pb.Action(d.PassiveNormal), Mode: "unvalidated"}, nil
}

func testPolicyInfo() *pb.PolicyInfoResponse {
	return &pb.PolicyInfoResponse{PolicySha256: testPolicyHash, ObservationVersion: x.ObservationVersion, FieldNames: append([]string(nil), x.ObservationFields...), PromotionStatus: "unvalidated"}
}

func serveTestPolicy(t *testing.T, service *testPolicyService) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterPolicyServiceServer(server, service)
	go server.Serve(listener)
	t.Cleanup(func() { server.Stop(); listener.Close() })
	return listener.Addr().String()
}

func testClient(t *testing.T, service *testPolicyService, timeout time.Duration) *Client {
	t.Helper()
	client, err := New(context.Background(), Config{Target: serveTestPolicy(t, service), PolicySHA256: testPolicyHash, Profile: "paper", AllowUnvalidated: true, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func testRequest() *pb.DecisionRequest {
	return &pb.DecisionRequest{
		Binding: &pb.DecisionBinding{
			RequestId: "request-1", TenantId: "tenant", AccountId: "account", SessionId: "session",
			SessionRevision: 3, AccountRevision: 7, PolicySha256: testPolicyHash,
			ObservationVersion: x.ObservationVersion, ObservationSchemaSha256: SchemaSHA256(),
			MarketSnapshotId: "market-1", ExpiresAtUnixNs: time.Now().Add(150 * time.Millisecond).UnixNano(),
		},
		Observation: &pb.Observation{SchemaVersion: x.ObservationVersion, FieldNames: append([]string(nil), x.ObservationFields...), Features: make([]float32, x.ObservationSize)},
	}
}

func TestSchemaSHA256MatchesPythonContract(t *testing.T) {
	if got := SchemaSHA256(); got != "96e4088967693e4b7e7e5653c13e8128f0507a09d6255acf88eea3e05107441a" {
		t.Fatalf("Python/Go schema hash mismatch: %s", got)
	}
}

func TestLocalTarget(t *testing.T) {
	for _, target := range []string{"127.0.0.1:32123", "[::1]:32123", "unix:/tmp/liquiditylab-policy.sock"} {
		if _, err := localTarget(target); err != nil {
			t.Errorf("valid local target %q: %v", target, err)
		}
	}
	for _, target := range []string{"", "localhost:32123", "192.0.2.1:32123", "0.0.0.0:32123", "[::]:32123", "127.0.0.1", "127.0.0.1:", "127.0.0.1:http", "127.0.0.1:-1", "127.0.0.1:65536", "127.0.0.1:99999999999999999999", "https://127.0.0.1:32123", "unix:relative.sock", "unix:/", "unix:/tmp/\x00socket", "unix:/" + strings.Repeat("s", 104)} {
		if _, err := localTarget(target); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid target %q accepted: %v", target, err)
		}
	}
}

func TestNewRequiresExplicitLocalPaperOrShadowConfiguration(t *testing.T) {
	target := serveTestPolicy(t, &testPolicyService{})
	for _, profile := range []string{"paper", "shadow"} {
		client, err := New(context.Background(), Config{Target: target, PolicySHA256: testPolicyHash, Profile: profile, AllowUnvalidated: true})
		if err != nil {
			t.Fatalf("explicit %s configuration rejected: %v", profile, err)
		}
		client.Close()
	}
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"missing opt-in", func(c *Config) { c.AllowUnvalidated = false }},
		{"live", func(c *Config) { c.Profile = "live" }},
		{"replay", func(c *Config) { c.Profile = "replay" }},
		{"missing profile", func(c *Config) { c.Profile = "" }},
		{"missing hash", func(c *Config) { c.PolicySHA256 = "" }},
		{"uppercase hash", func(c *Config) { c.PolicySHA256 = strings.ToUpper(testPolicyHash) }},
		{"short hash", func(c *Config) { c.PolicySHA256 = "aabb" }},
		{"negative timeout", func(c *Config) { c.Timeout = -time.Nanosecond }},
		{"excess timeout", func(c *Config) { c.Timeout = MaxValidity + time.Nanosecond }},
		{"remote endpoint", func(c *Config) { c.Target = "192.0.2.1:32123" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := Config{Target: target, PolicySHA256: testPolicyHash, Profile: "paper", AllowUnvalidated: true}
			tc.change(&config)
			client, err := New(context.Background(), config)
			if client != nil {
				client.Close()
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("expected invalid config, got %v", err)
			}
		})
	}
}

func TestNewRejectsIncompatiblePolicyInfo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*pb.PolicyInfoResponse)
	}{
		{"policy hash", func(i *pb.PolicyInfoResponse) { i.PolicySha256 = strings.Repeat("b", 64) }},
		{"schema version", func(i *pb.PolicyInfoResponse) { i.ObservationVersion = "other" }},
		{"schema order", func(i *pb.PolicyInfoResponse) { i.FieldNames[0], i.FieldNames[1] = i.FieldNames[1], i.FieldNames[0] }},
		{"schema length", func(i *pb.PolicyInfoResponse) { i.FieldNames = i.FieldNames[1:] }},
		{"live eligible", func(i *pb.PolicyInfoResponse) { i.LiveEligible = true }},
		{"live status", func(i *pb.PolicyInfoResponse) { i.PromotionStatus = "live" }},
		{"missing status", func(i *pb.PolicyInfoResponse) { i.PromotionStatus = "" }},
		{"oversized status", func(i *pb.PolicyInfoResponse) { i.PromotionStatus = strings.Repeat("s", 65) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := serveTestPolicy(t, &testPolicyService{info: func(_ context.Context, req *pb.PolicyInfoRequest) (*pb.PolicyInfoResponse, error) {
				if req.PolicySha256 != testPolicyHash {
					t.Error("handshake did not request pinned policy")
				}
				info := testPolicyInfo()
				tc.change(info)
				return info, nil
			}})
			client, err := New(context.Background(), Config{Target: target, PolicySHA256: testPolicyHash, Profile: "paper", AllowUnvalidated: true})
			if client != nil {
				client.Close()
			}
			if !errors.Is(err, ErrIncompatible) {
				t.Fatalf("expected incompatible info, got %v", err)
			}
		})
	}
}

func TestProposeExactBindingSemanticActionsAndDeadline(t *testing.T) {
	for action := d.Hold; action <= d.AggressiveNormal; action++ {
		t.Run(string(rune('0'+action)), func(t *testing.T) {
			client := testClient(t, &testPolicyService{predict: func(ctx context.Context, req *pb.DecisionRequest) (*pb.DecisionResponse, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 110*time.Millisecond || deadline.UnixNano() > req.Binding.ExpiresAtUnixNs {
					t.Error("RPC lacks bounded deadline within request expiry")
				}
				return &pb.DecisionResponse{Binding: req.Binding, Action: pb.Action(action), Mode: "unvalidated"}, nil
			}}, 0)
			request := testRequest()
			before := proto.Clone(request)
			proposal, err := client.Propose(context.Background(), request)
			if err != nil || proposal.Action != action || proposal.Mode != "unvalidated" || !proto.Equal(proposal.Binding, request.Binding) || !proto.Equal(request, before) {
				t.Fatalf("proposal mismatch or caller input changed: %+v %v", proposal, err)
			}
			request.Binding.RequestId = "mutated-after-return"
			if proposal.Binding.RequestId != "request-1" {
				t.Fatal("proposal binding aliases caller input")
			}
		})
	}
}

func TestProposeRejectsMalformedAndNonfiniteInputsBeforeRPC(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, &testPolicyService{predict: func(context.Context, *pb.DecisionRequest) (*pb.DecisionResponse, error) {
		calls.Add(1)
		return nil, status.Error(codes.Internal, "unexpected RPC")
	}}, 0)
	for _, tc := range []struct {
		name   string
		change func(*pb.DecisionRequest)
	}{
		{"missing binding", func(r *pb.DecisionRequest) { r.Binding = nil }},
		{"missing observation", func(r *pb.DecisionRequest) { r.Observation = nil }},
		{"missing request id", func(r *pb.DecisionRequest) { r.Binding.RequestId = "" }},
		{"oversize id", func(r *pb.DecisionRequest) { r.Binding.AccountId = strings.Repeat("a", 257) }},
		{"session revision", func(r *pb.DecisionRequest) { r.Binding.SessionRevision = 0 }},
		{"account revision", func(r *pb.DecisionRequest) { r.Binding.AccountRevision = 0 }},
		{"policy hash", func(r *pb.DecisionRequest) { r.Binding.PolicySha256 = strings.Repeat("b", 64) }},
		{"binding version", func(r *pb.DecisionRequest) { r.Binding.ObservationVersion = "other" }},
		{"schema hash", func(r *pb.DecisionRequest) { r.Binding.ObservationSchemaSha256 = strings.Repeat("b", 64) }},
		{"observation version", func(r *pb.DecisionRequest) { r.Observation.SchemaVersion = "other" }},
		{"field order", func(r *pb.DecisionRequest) {
			r.Observation.FieldNames[0], r.Observation.FieldNames[1] = r.Observation.FieldNames[1], r.Observation.FieldNames[0]
		}},
		{"feature count", func(r *pb.DecisionRequest) { r.Observation.Features = r.Observation.Features[1:] }},
		{"NaN", func(r *pb.DecisionRequest) { r.Observation.Features[0] = float32(math.NaN()) }},
		{"positive infinity", func(r *pb.DecisionRequest) { r.Observation.Features[0] = float32(math.Inf(1)) }},
		{"negative infinity", func(r *pb.DecisionRequest) { r.Observation.Features[0] = float32(math.Inf(-1)) }},
		{"message size", func(r *pb.DecisionRequest) { r.Observation.Features = make([]float32, 9000) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := testRequest()
			tc.change(req)
			if _, err := client.Propose(context.Background(), req); !errors.Is(err, ErrInvalid) {
				t.Fatalf("malformed request accepted: %v", err)
			}
		})
	}
	if _, err := client.Propose(context.Background(), nil); !errors.Is(err, ErrInvalid) || calls.Load() != 0 {
		t.Fatalf("invalid inputs reached inference: calls=%d err=%v", calls.Load(), err)
	}
}

func TestProposeRejectsResponseBindingActionAndModeChanges(t *testing.T) {
	changes := map[string]func(*pb.DecisionResponse){
		"missing binding":  func(r *pb.DecisionResponse) { r.Binding = nil },
		"request":          func(r *pb.DecisionResponse) { r.Binding.RequestId += "-other" },
		"tenant":           func(r *pb.DecisionResponse) { r.Binding.TenantId += "-other" },
		"account":          func(r *pb.DecisionResponse) { r.Binding.AccountId += "-other" },
		"session":          func(r *pb.DecisionResponse) { r.Binding.SessionId += "-other" },
		"plan revision":    func(r *pb.DecisionResponse) { r.Binding.SessionRevision++ },
		"account revision": func(r *pb.DecisionResponse) { r.Binding.AccountRevision++ },
		"policy":           func(r *pb.DecisionResponse) { r.Binding.PolicySha256 = strings.Repeat("b", 64) },
		"schema version":   func(r *pb.DecisionResponse) { r.Binding.ObservationVersion = "other" },
		"schema hash":      func(r *pb.DecisionResponse) { r.Binding.ObservationSchemaSha256 = strings.Repeat("b", 64) },
		"snapshot":         func(r *pb.DecisionResponse) { r.Binding.MarketSnapshotId += "-other" },
		"expiry":           func(r *pb.DecisionResponse) { r.Binding.ExpiresAtUnixNs++ },
		"zero action":      func(r *pb.DecisionResponse) { r.Action = pb.Action(0) },
		"unknown action":   func(r *pb.DecisionResponse) { r.Action = pb.Action(99) },
		"mode":             func(r *pb.DecisionResponse) { r.Mode = "live" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			client := testClient(t, &testPolicyService{predict: func(_ context.Context, req *pb.DecisionRequest) (*pb.DecisionResponse, error) {
				response := &pb.DecisionResponse{Binding: req.Binding, Action: pb.Action(d.Hold), Mode: "unvalidated"}
				change(response)
				return response, nil
			}}, 0)
			if _, err := client.Propose(context.Background(), testRequest()); !errors.Is(err, ErrIncompatible) {
				t.Fatalf("changed response accepted: %v", err)
			}
		})
	}
}

func TestProposeExpiryDeadlinesAndCancellation(t *testing.T) {
	client := testClient(t, &testPolicyService{}, 0)
	for _, expiry := range []time.Time{time.Now().Add(-time.Second), time.Now().Add(MaxValidity + time.Second)} {
		request := testRequest()
		request.Binding.ExpiresAtUnixNs = expiry.UnixNano()
		if _, err := client.Propose(context.Background(), request); !errors.Is(err, ErrStale) {
			t.Fatalf("invalid validity window accepted: %v", err)
		}
	}
	slow := testClient(t, &testPolicyService{predict: func(ctx context.Context, _ *pb.DecisionRequest) (*pb.DecisionResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}, 20*time.Millisecond)
	start := time.Now()
	if _, err := slow.Propose(context.Background(), testRequest()); !errors.Is(err, context.DeadlineExceeded) || status.Code(err) != codes.DeadlineExceeded || time.Since(start) > 150*time.Millisecond {
		t.Fatalf("inference timeout did not bound slow worker: %v duration=%v", err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Propose(ctx, testRequest()); !errors.Is(err, context.Canceled) || status.Code(err) != codes.Canceled {
		t.Fatalf("cancelled context reached successful proposal: %v", err)
	}
	// The client must shorten its own timeout to the proposal's expiry.
	expiring := testRequest()
	expiring.Binding.ExpiresAtUnixNs = time.Now().Add(10 * time.Millisecond).UnixNano()
	if _, err := slow.Propose(context.Background(), expiring); status.Code(err) != codes.DeadlineExceeded && !errors.Is(err, ErrStale) {
		t.Fatalf("expired slow proposal accepted: %v", err)
	}
}

func TestProposeClassifiesWorkerStatusWithoutLosingTransportCause(t *testing.T) {
	for _, tc := range []struct {
		name  string
		code  codes.Code
		cause error
	}{
		{"busy", codes.ResourceExhausted, ErrBusy},
		{"deadline", codes.DeadlineExceeded, context.DeadlineExceeded},
		{"cancelled", codes.Canceled, context.Canceled},
		{"invalid", codes.InvalidArgument, ErrInvalid},
		{"unavailable", codes.Unavailable, nil},
		{"internal", codes.Internal, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client := testClient(t, &testPolicyService{predict: func(_ context.Context, req *pb.DecisionRequest) (*pb.DecisionResponse, error) {
				if calls.Add(1) == 1 {
					return nil, status.Error(tc.code, "worker failure")
				}
				return &pb.DecisionResponse{Binding: req.Binding, Action: pb.Action(d.Hold), Mode: "unvalidated"}, nil
			}}, 0)
			proposal, err := client.Propose(context.Background(), testRequest())
			if err == nil || status.Code(err) != tc.code || tc.cause != nil && !errors.Is(err, tc.cause) || proposal.Binding != nil {
				t.Fatalf("worker status lost classification or returned a proposal: proposal=%+v err=%v", proposal, err)
			}
			if tc.cause == nil && (errors.Is(err, ErrBusy) || errors.Is(err, ErrInvalid) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
				t.Fatalf("unavailable worker was assigned an unrelated cause: %v", err)
			}
			if _, err := client.Propose(context.Background(), testRequest()); err != nil || calls.Load() != 2 {
				t.Fatalf("worker error retained admission slot: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestProposeInFlightAdmissionFailsFast(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	client := testClient(t, &testPolicyService{predict: func(ctx context.Context, req *pb.DecisionRequest) (*pb.DecisionResponse, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return &pb.DecisionResponse{Binding: req.Binding, Action: pb.Action(d.Hold), Mode: "unvalidated"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}, 150*time.Millisecond)
	finished := make(chan error, 1)
	go func() { _, err := client.Propose(context.Background(), testRequest()); finished <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first inference did not reach worker")
	}
	start := time.Now()
	_, err := client.Propose(context.Background(), testRequest())
	close(release)
	if !errors.Is(err, ErrBusy) || time.Since(start) > 50*time.Millisecond || calls.Load() != 1 {
		t.Fatalf("overload did not fail immediately: err=%v duration=%v calls=%d", err, time.Since(start), calls.Load())
	}
	if err := <-finished; err != nil {
		t.Fatalf("first admitted proposal failed: %v", err)
	}
}

func TestProposalValidateCurrentAllowsNewMarketButRejectsStateChanges(t *testing.T) {
	proposal, err := testClient(t, &testPolicyService{}, 0).Propose(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	base := Current{TenantID: "tenant", AccountID: "account", SessionID: "session", SessionRevision: 3, AccountRevision: 7, PolicySHA256: testPolicyHash, Active: true, Ready: true, MarketValid: true}
	// Current deliberately has no snapshot equality field: compilation uses the
	// newest valid market while the original proposal retains its audit binding.
	if err := proposal.ValidateCurrent(base, time.Now()); err != nil || proposal.Binding.MarketSnapshotId != "market-1" {
		t.Fatalf("unchanged authority/new market eligibility rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Current)
	}{
		{"tenant", func(c *Current) { c.TenantID = "other" }},
		{"account", func(c *Current) { c.AccountID = "other" }},
		{"parent", func(c *Current) { c.SessionID = "other" }},
		{"plan revision", func(c *Current) { c.SessionRevision++ }},
		{"account revision", func(c *Current) { c.AccountRevision++ }},
		{"policy", func(c *Current) { c.PolicySHA256 = strings.Repeat("b", 64) }},
		{"paused", func(c *Current) { c.Active = false }},
		{"not ready", func(c *Current) { c.Ready = false }},
		{"invalid market", func(c *Current) { c.MarketValid = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := base
			tc.change(&current)
			if err := proposal.ValidateCurrent(current, time.Now()); !errors.Is(err, ErrStale) {
				t.Fatalf("stale current state accepted: %v", err)
			}
		})
	}
	if err := proposal.ValidateCurrent(base, time.Unix(0, proposal.Binding.ExpiresAtUnixNs)); !errors.Is(err, ErrStale) {
		t.Fatal("expiry boundary proposal accepted")
	}
}

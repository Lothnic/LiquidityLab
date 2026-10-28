package inference

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "github.com/liquiditylab/liquiditylab/gen/go/liquiditylab/v1"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"github.com/liquiditylab/liquiditylab/internal/telemetry"
)

func telemetryClient(t *testing.T, service *testPolicyService, recorder *telemetry.Recorder, timeout time.Duration) *Client {
	t.Helper()
	client, err := New(context.Background(), Config{
		Target: serveTestPolicy(t, service), PolicySHA256: testPolicyHash, Profile: "paper",
		AllowUnvalidated: true, Timeout: timeout, Telemetry: recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestTelemetrySuccessAndValidationRejection(t *testing.T) {
	recorder := telemetry.New()
	client := telemetryClient(t, &testPolicyService{}, recorder, 100*time.Millisecond)
	if _, err := client.Propose(context.Background(), testRequest()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Propose(context.Background(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid request error = %v", err)
	}
	s := recorder.Snapshot()
	if s.Counters[telemetry.InferenceRequests].Value != 2 || s.Counters[telemetry.InferenceRejected].Value != 1 || s.Counters[telemetry.InferenceRPCs].Value != 1 || s.Counters[telemetry.InferenceErrors].Value != 0 {
		t.Fatalf("request/RPC counters = %+v", s.Counters)
	}
	if s.Gauges[telemetry.InferenceInFlight].Value != 0 || s.Distributions[telemetry.InferenceRPCNS].Count != 1 {
		t.Fatalf("success/rejection slot or duration telemetry = gauge %+v distribution %+v", s.Gauges[telemetry.InferenceInFlight], s.Distributions[telemetry.InferenceRPCNS])
	}
}

func TestTelemetryIncompatibleResponseCountsRPCError(t *testing.T) {
	recorder := telemetry.New()
	client := telemetryClient(t, &testPolicyService{predict: func(_ context.Context, req *pb.DecisionRequest) (*pb.DecisionResponse, error) {
		return &pb.DecisionResponse{Binding: req.Binding, Action: pb.Action(d.Hold), Mode: "wrong-mode"}, nil
	}}, recorder, 100*time.Millisecond)
	if _, err := client.Propose(context.Background(), testRequest()); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("incompatible response error = %v", err)
	}
	s := recorder.Snapshot()
	if s.Counters[telemetry.InferenceRequests].Value != 1 || s.Counters[telemetry.InferenceRejected].Value != 0 || s.Counters[telemetry.InferenceRPCs].Value != 1 || s.Counters[telemetry.InferenceErrors].Value != 1 {
		t.Fatalf("incompatible response counters = %+v", s.Counters)
	}
	if s.Gauges[telemetry.InferenceInFlight].Value != 0 || s.Distributions[telemetry.InferenceRPCNS].Count != 1 {
		t.Fatalf("incompatible response slot/duration = %+v / %+v", s.Gauges[telemetry.InferenceInFlight], s.Distributions[telemetry.InferenceRPCNS])
	}
}

func TestTelemetryBusyAdmissionDoesNotOwnInflightSlot(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	recorder := telemetry.New()
	client := telemetryClient(t, &testPolicyService{predict: func(ctx context.Context, req *pb.DecisionRequest) (*pb.DecisionResponse, error) {
		close(started)
		select {
		case <-release:
			return &pb.DecisionResponse{Binding: req.Binding, Action: pb.Action(d.Hold), Mode: "unvalidated"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}, recorder, 150*time.Millisecond)
	first := make(chan error, 1)
	go func() { _, err := client.Propose(context.Background(), testRequest()); first <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first RPC did not reach service")
	}
	blocked := recorder.Snapshot()
	if blocked.Gauges[telemetry.InferenceInFlight].Value != 1 || blocked.Counters[telemetry.InferenceRPCs].Value != 1 {
		t.Fatalf("in-flight snapshot = gauge %+v RPC counter %+v", blocked.Gauges[telemetry.InferenceInFlight], blocked.Counters[telemetry.InferenceRPCs])
	}
	if _, err := client.Propose(context.Background(), testRequest()); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent admission error = %v", err)
	}
	busy := recorder.Snapshot()
	if busy.Gauges[telemetry.InferenceInFlight].Value != 1 || busy.Counters[telemetry.InferenceRequests].Value != 2 || busy.Counters[telemetry.InferenceRejected].Value != 1 || busy.Counters[telemetry.InferenceRPCs].Value != 1 {
		t.Fatalf("busy request telemetry = %+v %+v", busy.Gauges[telemetry.InferenceInFlight], busy.Counters)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first RPC failed: %v", err)
	}
	after := recorder.Snapshot()
	if after.Gauges[telemetry.InferenceInFlight].Value != 0 || after.Distributions[telemetry.InferenceRPCNS].Count != 1 || after.Counters[telemetry.InferenceErrors].Value != 0 {
		t.Fatalf("post-RPC telemetry = %+v %+v %+v", after.Gauges[telemetry.InferenceInFlight], after.Distributions[telemetry.InferenceRPCNS], after.Counters)
	}
}

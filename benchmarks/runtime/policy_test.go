package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/liquiditylab/liquiditylab/gen/go/liquiditylab/v1"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	"github.com/liquiditylab/liquiditylab/internal/inference"
	"github.com/liquiditylab/liquiditylab/internal/telemetry"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func controlledRequest() *pb.DecisionRequest {
	return &pb.DecisionRequest{Binding: &pb.DecisionBinding{RequestId: "request-1", TenantId: "tenant", AccountId: "account", SessionId: "session", SessionRevision: 1, AccountRevision: 1, PolicySha256: mockPolicySHA256(), ObservationVersion: x.ObservationVersion, ObservationSchemaSha256: inference.SchemaSHA256(), MarketSnapshotId: "snapshot-1", ExpiresAtUnixNs: time.Now().Add(150 * time.Millisecond).UnixNano()}, Observation: &pb.Observation{SchemaVersion: x.ObservationVersion, FieldNames: append([]string(nil), x.ObservationFields...), Features: make([]float32, x.ObservationSize)}}
}

func TestControlledWorkerVerifiedDeadlineAndClientQuiescence(t *testing.T) {
	for _, delay := range []time.Duration{0, 100 * time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			c := testConfig()
			c.MockPolicyDelay = delay
			c.PolicyTimeout = 20 * time.Millisecond
			rec := telemetry.New()
			h, err := startPolicy(context.Background(), c, rec)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			proposal, err := h.worker.Propose(context.Background(), controlledRequest())
			if delay == 0 {
				if err != nil || proposal.Action != d.Hold || proposal.Mode != "unvalidated" {
					t.Fatalf("controlled proposal=%+v err=%v", proposal, err)
				}
			} else if err == nil {
				t.Fatal("delayed worker escaped deadline")
			}
			if err = h.Close(); err != nil {
				t.Fatal(err)
			}
			if h.worker.Snapshot().InFlight != 0 {
				t.Fatal("client did not drain")
			}
			for _, g := range rec.Snapshot().Gauges {
				if g.Value != 0 {
					t.Fatalf("gauge remained active: %+v", g)
				}
			}
			if _, err = h.worker.Propose(context.Background(), controlledRequest()); !errors.Is(err, inference.ErrBusy) {
				t.Fatalf("closed client admitted request: %v", err)
			}
		})
	}
}

func TestControlledWorkerSingleFlightAndContextCancellation(t *testing.T) {
	s := &mockPolicyService{delay: time.Second, slot: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.Predict(ctx, controlledRequest()); done <- err }()
	deadline := time.Now().Add(time.Second)
	for len(s.slot) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(s.slot) != 1 {
		t.Fatal("first request never entered worker")
	}
	if _, err := s.Predict(context.Background(), controlledRequest()); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("concurrent call=%v", err)
	}
	cancel()
	select {
	case err := <-done:
		if status.Code(err) != codes.Canceled {
			t.Fatalf("cancellation=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("controlled worker ignored cancellation")
	}
	if len(s.slot) != 0 {
		t.Fatal("worker slot retained")
	}
}

func TestPolicyCloseWaitsForRegisteredClientCall(t *testing.T) {
	c := testConfig()
	c.MockPolicyDelay = 5 * time.Second
	c.PolicyTimeout = 200 * time.Millisecond
	rec := telemetry.New()
	h, err := startPolicy(context.Background(), c, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	done := make(chan error, 1)
	go func() { _, err := h.worker.Propose(context.Background(), controlledRequest()); done <- err }()
	deadline := time.Now().Add(time.Second)
	for h.worker.Snapshot().InFlight == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.worker.Snapshot().InFlight == 0 {
		t.Fatal("client call never registered")
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("closed client invented a successful proposal")
		}
	case <-time.After(time.Second):
		t.Fatal("client call outlived cleanup")
	}
	if h.worker.Snapshot().InFlight != 0 {
		t.Fatal("client remained active after close")
	}
	for _, gauge := range rec.Snapshot().Gauges {
		if gauge.Value != 0 {
			t.Fatalf("gauge raced cleanup: %+v", gauge)
		}
	}
}

func TestPolicySelectorsAndBounds(t *testing.T) {
	for name, change := range map[string]func(*config){"mutually exclusive": func(c *config) { c.MockPolicyDelay = 0; c.PolicyTarget = "127.0.0.1:1" }, "missing hash": func(c *config) { c.PolicyTarget = "127.0.0.1:1" }, "live": func(c *config) { c.PolicyProfile = "live" }, "timeout": func(c *config) { c.PolicyTimeout = time.Second }, "delay": func(c *config) { c.MockPolicyDelay = 6 * time.Second }, "fallback": func(c *config) { c.PolicyFallback = "send" }} {
		t.Run(name, func(t *testing.T) {
			c := testConfig()
			change(&c)
			if c.validate() == nil {
				t.Fatal("invalid policy configuration accepted")
			}
		})
	}
}

func TestWorkerVerificationPrecedesDatabaseUse(t *testing.T) {
	mock, err := startMockPolicy(0)
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	c := testConfig()
	c.Output = filepath.Join(t.TempDir(), "report.json")
	c.PolicyTarget = mock.listener.Addr().String()
	c.PolicySHA256 = strings.Repeat("a", 64)
	c.DSNEnv = "LIQUIDITYLAB_BENCH_INVALID_TEST_DSN"
	t.Setenv(c.DSNEnv, "invalid-development-DSN-no-account-can-be-created")
	err = execute(c)
	if err == nil || !strings.Contains(err.Error(), "verify pinned policy") {
		t.Fatalf("worker was not verified before DB: %v", err)
	}
	b, err := os.ReadFile(c.Output)
	if err != nil {
		t.Fatal(err)
	}
	var r report
	if err = json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if r.Account.AccountID != "" || r.Valid {
		t.Fatal("failed verification reached account creation")
	}
	if strings.Contains(string(b), c.PolicyTarget) || strings.Contains(string(b), os.Getenv(c.DSNEnv)) {
		t.Fatal("target or DSN leaked")
	}
}

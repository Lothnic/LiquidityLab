package main

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/liquiditylab/liquiditylab/gen/go/liquiditylab/v1"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	rt "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type daemonPolicyServer struct {
	pb.UnimplementedPolicyServiceServer
	hash  string
	mode  string
	calls atomic.Int64
}

func (s *daemonPolicyServer) GetPolicyInfo(context.Context, *pb.PolicyInfoRequest) (*pb.PolicyInfoResponse, error) {
	mode := s.mode
	if mode == "" {
		mode = "unvalidated"
	}
	return &pb.PolicyInfoResponse{PolicySha256: s.hash, ObservationVersion: x.ObservationVersion, FieldNames: x.ObservationFields, PromotionStatus: mode}, nil
}
func (s *daemonPolicyServer) Predict(_ context.Context, request *pb.DecisionRequest) (*pb.DecisionResponse, error) {
	s.calls.Add(1)
	return &pb.DecisionResponse{Binding: proto.Clone(request.Binding).(*pb.DecisionBinding), Action: pb.Action(d.Hold), Mode: "unvalidated"}, nil
}
func TestPaperDaemonOptInPolicyActuallyRunsThroughRPCAndActor(t *testing.T) {
	opt := daemonOptions(t)
	dsn := daemonPostgres(t, &opt)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	worker := &daemonPolicyServer{hash: strings.Repeat("a", 64)}
	pb.RegisterPolicyServiceServer(server, worker)
	go server.Serve(listener)
	defer server.Stop()
	opt.policyTarget = listener.Addr().String()
	opt.policyHash = worker.hash
	opt.policyFallback = "pause"
	opt.policyProfile = "paper"
	opt.policyAllowUnvalidated = true
	opt.policyTimeout = 100 * time.Millisecond
	if err := opt.validate(); err != nil {
		t.Fatal(err)
	}
	daemon := daemonNewTest(t, opt, dsn, syntheticPaperSource)
	ready := daemonWait(t, daemon, func(v rt.View) bool { return v.Account.Ready })
	plan := d.DefaultPlan()
	plan.Quantity = d.MustDecimal("0.02")
	plan.MaxChildQuantity = d.MustDecimal("0.01")
	plan.QuoteBudget = d.MustDecimal("10")
	plan.BaseBalance = d.Zero
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	created, err := daemon.actor.Do(ctx, rt.Command{ID: "daemon-policy-create", Kind: "create", ExpectedRevision: ready.Account.Revision, PlanID: "daemon-policy-plan", Plan: &plan})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.actor.Do(ctx, rt.Command{ID: "daemon-policy-start", Kind: "start", ExpectedRevision: created.Revision}); err != nil {
		t.Fatal(err)
	}
	after := daemonWait(t, daemon, func(v rt.View) bool { return v.State.LastPolicyDecision != nil })
	if worker.calls.Load() == 0 || after.State.LastPolicyDecision.Result != "policy_action" || after.State.LastPolicyDecision.Action != d.Hold || after.State.Parent.PolicyBundle.PolicySHA256 != worker.hash || len(after.State.Fills) != 0 {
		t.Fatalf("daemon did not use configured policy: calls=%d decision=%+v", worker.calls.Load(), after.State.LastPolicyDecision)
	}
}
func TestPaperPolicyOptionsRequireCompleteExplicitOptIn(t *testing.T) {
	valid := daemonOptions(t)
	valid.policyTarget = "127.0.0.1:8888"
	valid.policyHash = strings.Repeat("a", 64)
	valid.policyFallback = "pause"
	valid.policyProfile = "paper"
	valid.policyAllowUnvalidated = true
	valid.policyTimeout = 100 * time.Millisecond
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	for _, effect := range []string{"hash", "allow", "fallback", "live", "timeout", "target"} {
		t.Run(effect, func(t *testing.T) {
			opt := valid
			switch effect {
			case "hash":
				opt.policyHash = ""
			case "allow":
				opt.policyAllowUnvalidated = false
			case "fallback":
				opt.policyFallback = ""
			case "live":
				opt.policyProfile = "live"
			case "timeout":
				opt.policyTimeout = 201 * time.Millisecond
			case "target":
				opt.policyTarget = ""
			}
			if err := opt.validate(); err == nil {
				t.Fatal("incomplete policy opt-in accepted")
			}
		})
	}
}

func TestPaperDaemonRejectsLiveManifestBeforeCreatingAccount(t *testing.T) {
	opt := daemonOptions(t)
	dsn := daemonPostgres(t, &opt)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	worker := &daemonPolicyServer{hash: strings.Repeat("a", 64), mode: "live"}
	pb.RegisterPolicyServiceServer(server, worker)
	go server.Serve(listener)
	defer server.Stop()
	opt.policyTarget, opt.policyHash = listener.Addr().String(), worker.hash
	opt.policyFallback, opt.policyProfile = "pause", "paper"
	opt.policyAllowUnvalidated, opt.policyTimeout = true, 100*time.Millisecond
	static, err := paperStatic(opt.webDir)
	if err != nil {
		t.Fatal(err)
	}
	if daemon, err := newPaperDaemon(context.Background(), opt, dsn, daemonTestToken, static, io.Discard, syntheticPaperSource); err == nil || daemon != nil {
		t.Fatalf("live manifest accepted: daemon=%v err=%v", daemon, err)
	}
	store, err := pg.Open(context.Background(), dsn, opt.schema)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.GetAccount(context.Background(), pg.Key{TenantID: paperDaemonTenant, AccountID: opt.account}); !errors.Is(err, pg.ErrNotFound) {
		t.Fatalf("rejected manifest created an account: %v", err)
	}
}

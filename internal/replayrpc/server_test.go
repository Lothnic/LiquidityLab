package replayrpc

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/liquiditylab/liquiditylab/gen/go/liquiditylab/v1"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

func dec(s string) *pb.Decimal { return wireDecimal(d.MustDecimal(s)) }
func fixture() *pb.EpisodeSpec {
	zero := int64(0)
	interval := d.Second
	settle := d.Second
	lambda := float64(0)
	return &pb.EpisodeSpec{
		Plan:   &pb.Plan{Side: pb.Side_SIDE_BUY, Quantity: dec(".1"), LimitPrice: dec("102"), DeadlineNs: 2 * d.Second, MaxChildQuantity: dec(".1"), QuoteBudget: dec("1000"), BaseBalance: dec("1"), Symbol: "BTC/USD"},
		Rules:  &pb.InstrumentRules{Symbol: "BTC/USD", TickSize: dec(".01"), LotSize: dec(".001"), MinQuantity: dec(".001"), MinNotional: dec(".01")},
		Fees:   &pb.FeeConfig{Maker: dec(".001"), Taker: dec(".002"), Currency: pb.FeeCurrency_FEE_QUOTE},
		Config: &pb.ReplayConfig{DecisionIntervalNs: &interval, SubmissionLatencyNs: &zero, CancelLatencyNs: &zero, SettlementAllowanceNs: &settle, LambdaMiss: &lambda, MaxParticipation: dec("1"), MinRestingAgeNs: &zero, MinMessageIntervalNs: &zero},
		Events: []*pb.MarketEvent{{Kind: pb.EventKind_EVENT_SNAPSHOT, Bids: []*pb.PriceLevel{{Price: dec("99"), Quantity: dec("1")}}, Asks: []*pb.PriceLevel{{Price: dec("101"), Quantity: dec("1")}}}, {Kind: pb.EventKind_EVENT_BOOK, ReceiptTimeNs: 3 * d.Second}},
	}
}
func newTestServer(t *testing.T, limits Limits) *Server {
	t.Helper()
	s, err := New(limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	return s
}
func create(t *testing.T, s *Server, id string, count uint32) *pb.CreateBatchResponse {
	t.Helper()
	r, err := s.CreateBatch(context.Background(), &pb.CreateBatchRequest{RequestId: id, Count: count, Episode: fixture()})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func stepRequest(c *pb.CreateBatchResponse, id string, seq uint64, action pb.Action) *pb.StepBatchRequest {
	return &pb.StepBatchRequest{BatchId: c.BatchId, RequestId: id, Sequence: seq, Actions: []*pb.EnvironmentAction{{EnvironmentId: c.EnvironmentIds[0], Action: action}}}
}
func wantCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("got %v, want %v", err, code)
	}
}

func TestSequencedBatchIsolationAndReports(t *testing.T) {
	s := newTestServer(t, DefaultLimits())
	c := create(t, s, "create", 2)
	if c.EngineInstanceId != s.InstanceID() || c.NextSequence != 1 {
		t.Fatal(c)
	}
	again := create(t, s, "create", 2)
	if !proto.Equal(c, again) {
		t.Fatal("create retry changed handles")
	}
	_, err := s.CreateBatch(context.Background(), &pb.CreateBatchRequest{RequestId: "create", Count: 1, Episode: fixture()})
	wantCode(t, err, codes.AlreadyExists)
	reset := &pb.ResetBatchRequest{BatchId: c.BatchId, RequestId: "reset", Sequence: 1, Resets: []*pb.EnvironmentReset{{EnvironmentId: c.EnvironmentIds[0], Seed: 42}, {EnvironmentId: c.EnvironmentIds[1], Seed: 43}}}
	r, err := s.ResetBatch(context.Background(), reset)
	if err != nil || len(r.Transitions) != 2 {
		t.Fatalf("reset: %v %v", r, err)
	}
	r2, err := s.ResetBatch(context.Background(), reset)
	if err != nil || !proto.Equal(r, r2) {
		t.Fatal("reset retry changed result", err)
	}
	req := stepRequest(c, "step", 2, pb.Action_ACTION_HOLD)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.StepBatch(context.Background(), req)
			if err != nil {
				t.Error(err)
				return
			}
			if r.Transitions[0].ReceiptTimeNs != d.Second {
				t.Error("duplicate advanced twice")
			}
		}()
	}
	wg.Wait()
	b, _ := s.find(c.BatchId, false)
	if b.environments[c.EnvironmentIds[1]].Now != 0 {
		t.Fatal("environments share mutable state")
	}
	if b.environments[c.EnvironmentIds[0]].Config.SubmissionLatency != 0 || b.environments[c.EnvironmentIds[0]].Config.LambdaMiss != 0 {
		t.Fatal("explicit optional zero replaced by defaults")
	}
	_, err = s.StepBatch(context.Background(), stepRequest(c, "step", 2, pb.Action_ACTION_AGGRESSIVE_NORMAL))
	wantCode(t, err, codes.AlreadyExists)
	_, err = s.StepBatch(context.Background(), stepRequest(c, "zero", 3, pb.Action_ACTION_UNSPECIFIED))
	wantCode(t, err, codes.InvalidArgument)
	duplicate := stepRequest(c, "duplicates", 3, pb.Action_ACTION_HOLD)
	duplicate.Actions = append(duplicate.Actions, duplicate.Actions[0])
	_, err = s.StepBatch(context.Background(), duplicate)
	wantCode(t, err, codes.InvalidArgument)
	_, err = s.GetReports(context.Background(), &pb.GetReportsRequest{BatchId: c.BatchId, EnvironmentIds: c.EnvironmentIds})
	wantCode(t, err, codes.FailedPrecondition)
	terminal, err := s.StepBatch(context.Background(), stepRequest(c, "terminal", 3, pb.Action_ACTION_HOLD))
	if err != nil || !terminal.Transitions[0].EconomicScoreValid {
		t.Fatalf("terminal: %v %v", terminal, err)
	}
	reports, err := s.GetReports(context.Background(), &pb.GetReportsRequest{BatchId: c.BatchId, EnvironmentIds: []string{c.EnvironmentIds[0]}})
	if err != nil {
		t.Fatal(err)
	}
	var detail map[string]any
	if err := json.Unmarshal(reports.Reports[0].DetailJson, &detail); err != nil {
		t.Fatal(err)
	}
	if reports.Reports[0].Status != "deadline_partial" || detail["remaining_quantity"] != "0.1" {
		t.Fatal(reports)
	}
	closeReq := &pb.CloseBatchRequest{BatchId: c.BatchId, RequestId: "close", Sequence: 4}
	closed, err := s.CloseBatch(context.Background(), closeReq)
	if err != nil || !closed.Closed {
		t.Fatal(closed, err)
	}
	closedAgain, err := s.CloseBatch(context.Background(), closeReq)
	if err != nil || !proto.Equal(closed, closedAgain) {
		t.Fatal("close retry", err)
	}
	_, err = s.CloseBatch(context.Background(), &pb.CloseBatchRequest{BatchId: c.BatchId, RequestId: "changed-close", Sequence: 4})
	wantCode(t, err, codes.AlreadyExists)
	if s.environmentCount != 0 || s.retainedBytes != 0 {
		t.Fatal("close retained engine resources")
	}
}

func TestCancellationBeforeMutationAndLostResponse(t *testing.T) {
	s := newTestServer(t, DefaultLimits())
	c := create(t, s, "cancel-create", 1)
	b, _ := s.find(c.BatchId, false)
	if err := b.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err := s.StepBatch(ctx, stepRequest(c, "waiting", 1, pb.Action_ACTION_HOLD))
	wantCode(t, err, codes.DeadlineExceeded)
	b.unlock()
	if b.next != 1 || b.environments[c.EnvironmentIds[0]].Now != 0 {
		t.Fatal("cancelled queued operation advanced engine")
	}
	var lost atomic.Bool
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		r, err := handler(ctx, req)
		if info.FullMethod == pb.ReplayService_StepBatch_FullMethodName && err == nil && !lost.Swap(true) {
			return nil, status.Error(codes.DeadlineExceeded, "simulated deadline after mutation")
		}
		return r, err
	}))
	pb.RegisterReplayServiceServer(server, s)
	listener := bufconn.Listen(1 << 20)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///replay", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	client := pb.NewReplayServiceClient(conn)
	req := stepRequest(c, "lost", 1, pb.Action_ACTION_HOLD)
	_, err = client.StepBatch(context.Background(), req)
	wantCode(t, err, codes.DeadlineExceeded)
	r, err := client.StepBatch(context.Background(), req)
	if err != nil || r.Transitions[0].ReceiptTimeNs != d.Second {
		t.Fatalf("retry: %v %v", r, err)
	}
	if len(b.environments[c.EnvironmentIds[0]].Decisions) != 1 {
		t.Fatal("lost response retry advanced twice")
	}
}

func TestBoundedCachesIdentityAndRestart(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxBatches = 1
	limits.MaxCreateCache = 1
	limits.MaxCreateIdentities = 3
	limits.MaxCachedOperations = 1
	limits.MaxCloseTombstones = 1
	s := newTestServer(t, limits)
	c := create(t, s, "first", 1)
	_, err := s.StepBatch(context.Background(), stepRequest(c, "first-step", 1, pb.Action_ACTION_HOLD))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ResetBatch(context.Background(), &pb.ResetBatchRequest{BatchId: c.BatchId, RequestId: "reset", Sequence: 2, Resets: []*pb.EnvironmentReset{{EnvironmentId: c.EnvironmentIds[0]}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.StepBatch(context.Background(), stepRequest(c, "first-step", 1, pb.Action_ACTION_HOLD))
	wantCode(t, err, codes.FailedPrecondition)
	_, err = s.StepBatch(context.Background(), stepRequest(c, "first-step", 3, pb.Action_ACTION_HOLD))
	wantCode(t, err, codes.AlreadyExists)
	_, err = s.CloseBatch(context.Background(), &pb.CloseBatchRequest{BatchId: c.BatchId, RequestId: "close", Sequence: 3})
	if err != nil {
		t.Fatal(err)
	}
	create(t, s, "second", 1)
	_, err = s.CreateBatch(context.Background(), &pb.CreateBatchRequest{RequestId: "first", Count: 1, Episode: fixture()})
	wantCode(t, err, codes.FailedPrecondition)
	s2 := newTestServer(t, DefaultLimits())
	if s2.InstanceID() == s.InstanceID() {
		t.Fatal("restart reused engine identity")
	}
	_, err = s2.StepBatch(context.Background(), stepRequest(c, "after-restart", 4, pb.Action_ACTION_HOLD))
	wantCode(t, err, codes.NotFound)
}

func TestWireBoundsAndOptionalDefaults(t *testing.T) {
	for _, v := range []*pb.Decimal{{Coefficient: "+1"}, {Coefficient: "01"}, {Coefficient: "-0"}, {Coefficient: "100", Scale: 2}, {Coefficient: "1", Scale: 37}, {Coefficient: "", Scale: 0}} {
		if _, err := decimal(v, true); err == nil {
			t.Errorf("accepted %v", v)
		}
	}
	s := newTestServer(t, DefaultLimits())
	bad := fixture()
	bad.Plan.Quantity = &pb.Decimal{Coefficient: "01"}
	_, err := s.CreateBatch(context.Background(), &pb.CreateBatchRequest{RequestId: "bad", Count: 1, Episode: bad})
	wantCode(t, err, codes.InvalidArgument)
	if s.environmentCount != 0 {
		t.Fatal("invalid create leaked admission")
	}
	_, err = s.CreateBatch(context.Background(), &pb.CreateBatchRequest{RequestId: "too-many", Count: 33, Episode: fixture()})
	wantCode(t, err, codes.InvalidArgument)
	_, err = s.StepBatch(context.Background(), nil)
	wantCode(t, err, codes.InvalidArgument)
	c := fixture()
	c.Config = nil
	c.Plan.DeadlineNs = d.Second
	c.Events[1].ReceiptTimeNs = 31 * d.Second
	_, _, _, _, config, err := episode(c, DefaultLimits())
	if err != nil || config.SubmissionLatency != 50_000_000 || config.LambdaMiss != 100 {
		t.Fatal(config, err)
	}
}

func TestPartialFailurePoisonsWholeBatch(t *testing.T) {
	s := newTestServer(t, DefaultLimits())
	c := create(t, s, "poison", 2)
	b, _ := s.find(c.BatchId, false)
	// Inject a broken engine invariant in the second environment after admission.
	b.environments[c.EnvironmentIds[1]].Plan.Quantity = d.Zero
	req := stepRequest(c, "partial", 1, pb.Action_ACTION_HOLD)
	req.Actions = append(req.Actions, &pb.EnvironmentAction{EnvironmentId: c.EnvironmentIds[1], Action: pb.Action_ACTION_HOLD})
	_, err := s.StepBatch(context.Background(), req)
	wantCode(t, err, codes.Internal)
	if !b.poisoned || b.environments[c.EnvironmentIds[0]].Now != d.Second {
		t.Fatal("partial mutation not contained")
	}
	_, err = s.StepBatch(context.Background(), req)
	wantCode(t, err, codes.Internal)
	_, err = s.ResetBatch(context.Background(), &pb.ResetBatchRequest{BatchId: c.BatchId, RequestId: "reset-poison", Sequence: 2, Resets: []*pb.EnvironmentReset{{EnvironmentId: c.EnvironmentIds[0]}}})
	wantCode(t, err, codes.FailedPrecondition)
	_, err = s.CloseBatch(context.Background(), &pb.CloseBatchRequest{BatchId: c.BatchId, RequestId: "close-poison", Sequence: 2})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIdleReaperSkipsActiveMutation(t *testing.T) {
	s := newTestServer(t, DefaultLimits())
	c := create(t, s, "reap", 1)
	b, _ := s.find(c.BatchId, false)
	if err := b.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.reap(time.Now().Add(2 * s.limits.IdleTTL))
	if s.environmentCount != 1 {
		t.Fatal("reaped locked batch")
	}
	b.unlock()
	s.reap(time.Now().Add(2 * s.limits.IdleTTL))
	if s.environmentCount != 0 {
		t.Fatal("idle batch not reclaimed")
	}
	_, err := s.find(c.BatchId, false)
	wantCode(t, err, codes.NotFound)
}

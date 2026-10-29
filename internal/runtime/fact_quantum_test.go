package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
	"github.com/liquiditylab/liquiditylab/internal/telemetry"
)

// Freeze a real attempted child and 512 independent partial fills, without
// running its reducer. Their sum is 0.0256 BTC at 99, with 0.0025344 quote fee.
func quantumTestBatch(t *testing.T) (*Actor, pg.Observation) {
	t.Helper()
	store, key, _, a := actorTestReady(t, 0)
	plan := actorTestPlan()
	plan.Deadline = int64(3 * time.Minute)
	plan.Quantity = d.MustDecimal("0.6") // Twelve scheduled slices of 0.05.
	actorTestDo(t, a, Command{ID: "quantum-create", Kind: "create", PlanID: "quantum-plan", Plan: plan})
	actorTestDo(t, a, Command{ID: "quantum-start", Kind: "start"})
	queued := actorTestQueue(t, a, d.PassiveNormal, "quantum-queue")
	ctx := context.Background()
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	child := queued.State.Child
	_, err := store.AuthorizeDispatch(ctx, pg.Authorization{Key: key, Ownership: a.owner, IntentID: child.ID, ExpectedRevision: a.account.Revision, PlanRevision: a.state.Parent.Revision, SnapshotID: a.state.MarketID, InstrumentRevision: a.state.InstrumentRevision, AttemptID: "quantum-attempt"})
	if err != nil {
		t.Fatal(err)
	}
	form := transportFact{IntentID: child.ID, OperationID: "quantum-submit", InputDigest: "quantum-input", Intent: &child.Order.Intent, Outcome: TransportResult{Handoff: PotentiallyInFlight, Accepted: true, QueueAhead: d.Zero}}
	for i := 0; i < 512; i++ {
		form.Outcome.Fills = append(form.Outcome.Fills, d.Fill{ExecutionID: fmt.Sprintf("quantum-fill-%03d", i), OrderID: child.ID, Side: "buy", Quantity: d.MustDecimal("0.00005"), Price: d.MustDecimal("99"), Fee: d.MustDecimal("0.00000495"), FeeCurrency: "quote", Maker: true, Time: int64(i)})
	}
	id := "paper-submit-" + child.ID
	_, err = store.RecordPaperObservation(ctx, key, a.owner, pg.Observation{ID: id, Kind: paperTransportKind, Payload: jsonBytes(form)})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := store.GetObservation(ctx, key, id)
	if err != nil {
		t.Fatal(err)
	}
	return a, observation
}

func TestFactQuantumBoundsCountAndElapsedInsideTransport(t *testing.T) {
	a, observation := quantumTestBatch(t)
	ctx := context.Background()
	progress, err := a.reduceFactsQuantum(ctx, factBudget{MaxSteps: 2, MaxElapsed: time.Hour})
	if err != nil || progress.Steps != 2 || !progress.More || progress.ProcessedSequence >= observation.Sequence || len(a.state.Fills) != 2 {
		t.Fatalf("count quantum progress=%+v fills=%d err=%v", progress, len(a.state.Fills), err)
	}
	if a.state.Child == nil || !a.state.Child.Order.Remaining.Equal(d.MustDecimal("0.0499")) || len(a.account.Reservations) != 1 || !findBalance(a.account.Balances, "BTC").Total.Equal(d.MustDecimal("1.0001")) || !findBalance(a.account.Balances, "USD").Total.Equal(d.MustDecimal("999.9900901")) {
		t.Fatalf("partial atomic projection child=%+v account=%+v", a.state.Child, a.account)
	}
	clock := time.Unix(0, 0)
	a.factClock = func() time.Time { clock = clock.Add(10 * time.Millisecond); return clock }
	progress, err = a.reduceFactsQuantum(ctx, factBudget{MaxSteps: 512, MaxElapsed: time.Millisecond})
	if err != nil || progress.Steps != 1 || len(a.state.Fills) != 3 || !progress.More {
		t.Fatalf("elapsed quantum progress=%+v fills=%d err=%v", progress, len(a.state.Fills), err)
	}
	a.factClock = nil
	// Simulate loss of all process-local progress after an uncertain commit.
	a.needsReload = true
	a.state.Fills = nil
	a.factCursor = nil
	progress, err = a.reduceFactsQuantum(ctx, factBudget{MaxSteps: 2, MaxElapsed: time.Hour})
	if err != nil || progress.Steps != 2 || len(a.state.Fills) != 3 || a.factCursor.nextFill != 2 {
		t.Fatalf("durable duplicate prefix not bounded/reloaded: progress=%+v fills=%d cursor=%+v err=%v", progress, len(a.state.Fills), a.factCursor, err)
	}
	// A true new owner starts paused, reconstructs the applied prefix and settles
	// the same immutable source, with no synthetic persistent subcursor.
	recovered := recoveryTestNew(t, a.config.Store, a.config.Key, "quantum-recovered-owner")
	actorTestMarket(t, recovered, "quantum-recovered-book")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := recovered.RecoverPaper(ctx); err != nil {
		t.Fatal(err)
	}
	v := actorTestView(t, recovered)
	if len(v.State.Fills) != 512 || v.State.Child != nil || len(v.Account.Reservations) != 0 || v.State.Parent.Status != "paused" || !v.Account.Ready || v.Account.ObservedSequence != v.Account.ProcessedSequence || !findBalance(v.Account.Balances, "BTC").Total.Equal(d.MustDecimal("1.0256")) || !findBalance(v.Account.Balances, "USD").Total.Equal(d.MustDecimal("997.4630656")) {
		t.Fatalf("resumed prefix changed economics or finality: fills=%d parent=%+v child=%+v account=%+v", len(v.State.Fills), v.State.Parent, v.State.Child, v.Account)
	}
}

func quantumTestRequest(kind string, input any) request {
	return request{kind: kind, input: input, ctx: context.Background(), reply: make(chan response, 1)}
}

func quantumTestResponse(t *testing.T, r request) response {
	t.Helper()
	select {
	case result := <-r.reply:
		return result
	case <-time.After(60 * time.Second):
		t.Fatal("actor continuation did not reply")
		return response{}
	}
}

func TestFactQuantumControlAckPrecedesPrefixCompletionAndMarketsStayOrdered(t *testing.T) {
	a, source := quantumTestBatch(t)
	ctx := context.Background()
	if _, err := a.reduceFactsQuantum(ctx, factBudget{MaxSteps: 2, MaxElapsed: time.Hour}); err != nil {
		t.Fatal(err)
	}
	// Start a pending ordinary market operation after its preceding watermark was
	// captured. Queue controls/read before resuming, at an exact fill checkpoint.
	first := quantumTestRequest("market", marketInput{Event: actorTestTrade("quantum-first-trade", "0.01", 1, 1), SnapshotID: "quantum-first-market", SessionID: "quantum-session"})
	second := quantumTestRequest("market", marketInput{Event: actorTestTrade("quantum-second-trade", "0.01", 2, 2), SnapshotID: "quantum-second-market", SessionID: "quantum-session"})
	control := quantumTestRequest("command", Command{ID: "quantum-local-cancel", Kind: "cancel", ExpectedRevision: a.account.Revision})
	read := quantumTestRequest("view", nil)
	a.pending = &actorOperation{request: first, stage: operationPrimary, target: source.Sequence}
	a.control <- control
	a.views <- read
	a.work <- second
	a.config.FactQuantumSteps = 1
	// Resume only the stopped owner loop, without a takeover or state rewrite.
	a.stop, a.done = make(chan struct{}), make(chan struct{})
	a.once = sync.Once{}
	a.closing = false
	go a.loop()
	ack := quantumTestResponse(t, control)
	if ack.err != nil || ack.result.Revision == 0 {
		t.Fatalf("local control receipt=%+v", ack)
	}
	v := quantumTestResponse(t, read)
	if v.err != nil || len(v.view.State.Fills) != 2 || v.view.State.Parent.Status != "cancel_requested" || v.view.State.Child == nil || v.view.State.Child.State != "cancel_requested" || len(v.view.Account.Reservations) != 1 || v.view.Account.Ready || v.view.Account.ProcessedSequence >= source.Sequence {
		t.Fatalf("control/read crossed unfinished financial prefix: %+v", v)
	}
	select {
	case result := <-first.reply:
		t.Fatalf("market falsely acknowledged before its observation: %+v", result)
	default:
	}
	if result := quantumTestResponse(t, first); result.err != nil {
		t.Fatal(result.err)
	}
	if result := quantumTestResponse(t, second); result.err != nil {
		t.Fatal(result.err)
	}
	end := actorTestView(t, a)
	if len(end.State.Fills) != 512 || end.State.Child != nil || len(end.Account.Reservations) != 0 || end.State.Parent.Status != "cancelled" || end.Account.ProcessedSequence != end.Account.ObservedSequence || !findBalance(end.Account.Balances, "BTC").Total.Equal(d.MustDecimal("1.0256")) || !findBalance(end.Account.Balances, "USD").Total.Equal(d.MustDecimal("997.4630656")) {
		t.Fatalf("cancel dropped frozen fills or failed known final: %+v", end)
	}
	firstID, _ := paperTradeIdentity("quantum-session", first.input.(marketInput).Event)
	secondID, _ := paperTradeIdentity("quantum-session", second.input.(marketInput).Event)
	firstFact, err := a.config.Store.GetObservation(ctx, a.config.Key, firstID)
	if err != nil {
		t.Fatal(err)
	}
	secondFact, err := a.config.Store.GetObservation(ctx, a.config.Key, secondID)
	if err != nil || secondFact.Sequence != firstFact.Sequence+1 || firstFact.Sequence <= source.Sequence {
		t.Fatalf("market FIFO source sequence first=%+v second=%+v err=%v", firstFact, secondFact, err)
	}
	var frozen paperTradeFact
	if decodeInternal(firstFact.Payload, &frozen) != nil || frozen.IntentID != "" || len(frozen.Fills) != 0 {
		t.Fatalf("new trade froze before cancellation debt settled: %+v", frozen)
	}
	// Original receipt identity survives later financial revisions.
	retry, err := a.Do(ctx, control.input.(Command))
	if err != nil || !retry.Duplicate || retry.Revision != ack.result.Revision {
		t.Fatalf("control retry result=%+v err=%v", retry, err)
	}
}

func TestFactQuantumCreateCannotReplaceParentWithAcceptedDebt(t *testing.T) {
	a, _ := quantumTestBatch(t)
	before, err := a.config.Store.GetAccount(context.Background(), a.config.Key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.reconcile(context.Background(), Reconciliation{ID: "quantum-premature-reconcile", ExpectedRevision: before.Revision, Evidence: "cannot replace frozen valid prefix", NoExternalOrders: true, Balances: before.Balances})
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("reconciliation crossed valid frozen prefix: %v", err)
	}
	// Even a stale local projection that appears caught up cannot create over a
	// just-accepted durable observation; the store lock is the final authority.
	next, err := cloneState(a.state)
	if err != nil {
		t.Fatal(err)
	}
	next.Parent = nil
	next.Child = nil
	_, err = a.persist(context.Background(), next, pg.Command{ID: "quantum-create-race", Kind: "create", ExpectedRevision: a.account.Revision}, pg.Change{RequireCaughtUp: true})
	if !errors.Is(err, pg.ErrNotReady) {
		t.Fatalf("unguarded accepted debt overwrite: %v", err)
	}
	durable, err := a.config.Store.GetAccount(context.Background(), a.config.Key)
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if decodeInternal(durable.Snapshot, &state) != nil || state.Parent == nil || state.Child == nil || durable.ProcessedSequence != 0 {
		t.Fatalf("debt guard changed durable parent: %+v", durable)
	}
}

func TestFactQuantumAndViewConfigurationBounds(t *testing.T) {
	store, key, _ := actorTestDB(t)
	cfg := DefaultConfig(store, key)
	if cfg.FactQuantumSteps != 4 || cfg.FactQuantumTime != 5*time.Millisecond || cfg.ViewCapacity != 64 {
		t.Fatalf("effective defaults not exposed: %+v", cfg)
	}
	for _, customize := range []func(*Config){func(c *Config) { c.FactQuantumSteps = -1 }, func(c *Config) { c.FactQuantumSteps = 513 }, func(c *Config) { c.FactQuantumTime = -1 }, func(c *Config) { c.FactQuantumTime = time.Second + 1 }, func(c *Config) { c.ViewCapacity = -1 }, func(c *Config) { c.ViewCapacity = 4097 }} {
		bad := cfg
		customize(&bad)
		if _, err := New(context.Background(), bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid bounds accepted: %v", err)
		}
	}
	cfg.MailboxCapacity, cfg.ViewCapacity, cfg.FactQuantumSteps, cfg.FactQuantumTime = 3, 0, 0, 0
	a, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	if cap(a.views) != 3 || a.config.FactQuantumSteps != 4 || a.config.FactQuantumTime != 5*time.Millisecond {
		t.Fatalf("custom zero defaults not normalized: view=%d cfg=%+v", cap(a.views), a.config)
	}
	// A saturated read lane cannot consume a control/ordinary slot or report
	// successful admission; publication accounting includes the separate lane.
	recorder := telemetry.New()
	bare := &Actor{config: Config{Telemetry: recorder}, views: make(chan request, 1), work: make(chan request, 1), control: make(chan request, 1)}
	bare.views <- quantumTestRequest("view", nil)
	if _, err := bare.submit(context.Background(), "view", nil, false); !errors.Is(err, ErrMailboxFull) {
		t.Fatalf("full read lane accepted: %v", err)
	}
	if len(bare.control) != 0 || len(bare.work) != 0 || recorder.Snapshot().Gauges[telemetry.ActorQueued].Value != 0 || recorder.Snapshot().Counters[telemetry.ActorRejected].Value != 1 {
		t.Fatal("read lane rejection changed admission accounting")
	}
	bare.work <- quantumTestRequest("market", marketInput{})
	if err := bare.RecoverPaper(context.Background()); !errors.Is(err, ErrMailboxFull) {
		t.Fatalf("recovery overtook retained ordinary FIFO: %v", err)
	}
	if len(bare.control) != 0 {
		t.Fatal("yielding recovery entered priority control lane")
	}
}

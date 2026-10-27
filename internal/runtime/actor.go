package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

type Actor struct {
	policyResults chan policyResult
	policyBusy    bool
	policyCancel  context.CancelFunc
	policyTimer   *time.Timer
	policyExpiry  <-chan time.Time

	config               Config
	state                State
	account              pg.Account
	owner                pg.Ownership
	transport            Transport
	control, work, facts chan request
	stop, done           chan struct{}
	admission            sync.Mutex
	next                 uint64
	closing              bool
	once                 sync.Once
	overflow             atomic.Bool
	now                  func() time.Time
	lastClock            time.Time
	elapsedPlan          string
	elapsedAt            time.Time
	elapsedBase          int64
}

func randomID(prefix string) string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(raw[:])
}

// bootstrapState validates immutable execution inputs before ownership changes,
// and reconstructs state again after the claim from the current durable account.
// A pre-claim read never supplies the snapshot written by the new owner.
func bootstrapState(c Config, account pg.Account, bundle *PolicyBundle) (State, error) {
	var state State
	if string(account.Snapshot) == "{}" && len(account.Plans) == 0 && len(account.Intents) == 0 {
		state = State{Version: StateVersion, Profile: ProfilePaper, DataSource: c.DataSource, Rules: c.Rules, Fees: c.Fees, Execution: c.Execution, BaseCurrency: c.BaseCurrency, QuoteCurrency: c.QuoteCurrency, InstrumentRevision: c.InstrumentRevision, Book: x.NewBook()}
	} else {
		if err := decodeInternal(account.Snapshot, &state); err != nil {
			return State{}, err
		}
		if state.Version != StateVersion || state.Profile != ProfilePaper || state.Book == nil || state.BaseCurrency != c.BaseCurrency || state.QuoteCurrency != c.QuoteCurrency || state.InstrumentRevision != c.InstrumentRevision || !reflect.DeepEqual(state.Rules, c.Rules) || !reflect.DeepEqual(state.Fees, c.Fees) || !reflect.DeepEqual(state.Execution, c.Execution) {
			return State{}, ErrInvalid
		}
		// Older snapshots omitted these defaults. Existing parent baselines stay
		// pinned independently of the default selected for future sessions.
		if state.Parent != nil && state.Parent.Baseline == "" {
			state.Parent.Baseline = c.Baseline
		}
		if state.DataSource == "" {
			state.DataSource = "manual"
		}
		if state.DataSource != c.DataSource {
			return State{}, ErrInvalid
		}
	}
	if !sameBundle(state.PolicyBundle, bundle) && !canReplacePolicyBundle(state, account) {
		return State{}, ErrInvalid
	}
	state.PolicyBundle = bundle
	if !terminalPolicyParent(state.Parent) && !sameBundle(state.Parent.PolicyBundle, bundle) {
		return State{}, ErrInvalid
	}
	return state, nil
}

func New(ctx context.Context, c Config) (*Actor, error) {
	if c.Profile != ProfilePaper {
		return nil, ErrProfile
	}
	if c.Store == nil || c.Lease <= 0 || c.Lease > 5*time.Minute || c.MailboxCapacity <= 0 || c.MailboxCapacity > 4096 || c.ControlCapacity <= 0 || c.ControlCapacity > 256 || c.FactCapacity <= 0 || c.FactCapacity > 4096 || c.MaxFills <= 0 || c.MaxFills > 4096 || c.MaxFacts <= 0 || c.MaxFacts > 1024 || c.BaseCurrency == "" || c.QuoteCurrency == "" || c.BaseCurrency == c.QuoteCurrency || c.InstrumentRevision == "" {
		return nil, ErrInvalid
	}
	for _, err := range []error{c.Rules.Validate(), c.Fees.Validate(), c.Execution.Validate()} {
		if err != nil {
			return nil, err
		}
	}
	if c.OwnerID == "" {
		c.OwnerID = randomID("process-")
	}
	if c.Baseline == "" {
		c.Baseline = x.BaselineScheduled
	}
	if c.DataSource == "" {
		c.DataSource = "manual"
	}
	if c.DataSource != "manual" && c.DataSource != "synthetic" && c.DataSource != "recorded" && c.DataSource != "kraken-public" {
		return nil, ErrInvalid
	}
	if err := x.ValidateBaselinePolicy(c.Baseline); err != nil {
		return nil, err
	}
	if c.Execution.DecisionInterval > 24*time.Hour.Nanoseconds() || len(c.InstrumentRevision) > 128 || len(c.Rules.Symbol) > 64 || len(c.BaseCurrency) > 32 || len(c.QuoteCurrency) > 32 || len(c.Fees.Source) > 256 {
		return nil, ErrInvalid
	}
	if c.Policy != nil {
		copied := *c.Policy
		c.Policy = &copied
	}
	bundle, err := policyBundle(c.Policy)
	if err != nil {
		return nil, err
	}
	a, err := c.Store.GetAccount(ctx, c.Key)
	if err != nil {
		return nil, err
	}
	if a.Ownership.OwnerID == c.OwnerID {
		return nil, pg.ErrOwnership
	}
	if _, err = bootstrapState(c, a, bundle); err != nil {
		return nil, err
	}
	owner, err := c.Store.ClaimOwnership(ctx, c.Key, c.OwnerID, c.Lease, c.RecoveryProof)
	if err != nil {
		return nil, err
	}
	a, err = c.Store.GetAccount(ctx, c.Key)
	if err != nil {
		return nil, err
	}
	if a.Ownership != owner {
		return nil, pg.ErrOwnership
	}
	state, err := bootstrapState(c, a, bundle)
	if err != nil {
		return nil, err
	}
	state.Book.Valid = false
	state.RecoveryRequired = false
	state.Reason = "fresh_market_and_reconciliation_required"
	if state.Child != nil {
		found := false
		for _, i := range a.Intents {
			if i.ID == state.Child.ID {
				state.Child.State = "unknown"
				state.Child.Order.State = "cancel_requested"
				found = true
				state.RecoveryRequired = true
			}
		}
		if !found && !(a.ObservedSequence > a.ProcessedSequence && state.Child.Order.Remaining.IsZero()) {
			state.Child = nil
		}
	}
	if len(a.Intents) > 0 || len(state.PendingFacts) > 0 || a.ObservedSequence > a.ProcessedSequence {
		state.RecoveryRequired = true
		state.Reason = "outstanding_outcomes_require_reconciliation"
	}
	if state.Parent != nil {
		for _, p := range a.Plans {
			if p.ID == state.Parent.ID {
				state.Parent.Revision = p.Revision
				state.Parent.Status = p.Status
			}
		}
	}
	var bootstrapPlan *pg.Plan
	// Recovery always needs an explicit resume after fresh market data and
	// reconciliation, including baseline sessions without pending inference.
	if state.Parent != nil && state.Parent.Status == "active" {
		state.Parent.Status = "paused"
		state.Parent.Revision++
		bootstrapPlan = parentRow(state.Parent)
	}
	// An old owner's pending request cannot authorize a recovered sender.
	if state.PolicyPending != nil {
		state.PolicyPending.Result, state.PolicyPending.Reason = "fallback_pause", "owner_restart"
		state.PolicyPending.ResolvedAt = time.Now().UTC()
		state.DecisionCount = max(state.DecisionCount, state.PolicyPending.Slot)
		state.LastPolicyDecision = state.PolicyPending
		state.PolicyPending = nil
	}
	for _, f := range state.UnresolvedFacts {
		if !f.Resolved {
			state.RecoveryRequired = true
		}
	}
	actor := &Actor{config: c, state: state, account: a, owner: owner, transport: newPaperTransport(c.Store, c.Key), control: make(chan request, c.ControlCapacity), work: make(chan request, c.MailboxCapacity), facts: make(chan request, c.FactCapacity), stop: make(chan struct{}), done: make(chan struct{}), now: time.Now, policyResults: make(chan policyResult, 1)}
	ready := false
	state.LastInputID = "boot-" + c.OwnerID
	state.LastInputDigest = "bootstrap"
	state.AuthorityRevision = a.Revision + 1
	actor.state = state
	change := pg.Change{Key: c.Key, Ownership: owner, Command: pg.Command{ID: state.LastInputID, Kind: "runtime_bootstrap", ExpectedRevision: a.Revision}, Ready: &ready, Market: &pg.Market{Valid: false, SnapshotID: nonempty(state.MarketID, "awaiting-snapshot"), InstrumentRevision: c.InstrumentRevision}, Snapshot: jsonBytes(state), Plan: bootstrapPlan}
	if _, err = c.Store.Apply(ctx, change); err != nil {
		return nil, err
	}
	actor.account, err = c.Store.GetAccount(ctx, c.Key)
	if err != nil {
		return nil, err
	}
	go actor.loop()
	return actor, nil
}
func nonempty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
func (a *Actor) submit(ctx context.Context, kind string, input any, priority bool) (response, error) {
	if err := ctx.Err(); err != nil {
		return response{}, err
	}
	req := request{kind: kind, input: input, ctx: ctx, reply: make(chan response, 1)}
	a.admission.Lock()
	if a.closing {
		a.admission.Unlock()
		return response{}, ErrClosed
	}
	a.next++
	req.sequence = a.next
	queue := a.work
	if priority {
		queue = a.control
	}
	if kind == "facts" {
		queue = a.facts
	}
	select {
	case queue <- req:
		a.admission.Unlock()
	default:
		a.admission.Unlock()
		if kind == "facts" {
			a.overflow.Store(true)
		}
		return response{}, ErrMailboxFull
	}
	select {
	case result := <-req.reply:
		return result, result.err
	case <-ctx.Done():
		return response{}, fmt.Errorf("%w: %v", ErrOutcomeUnknown, ctx.Err())
	case <-a.done:
		return response{}, ErrClosed
	}
}
func (a *Actor) Do(ctx context.Context, c Command) (pg.Result, error) {
	if !clientIdentity(c.ID) {
		return pg.Result{}, ErrInvalid
	}
	priority := c.Kind == "pause" || c.Kind == "cancel" || c.Kind == "deadline"
	r, err := a.submit(ctx, "command", c, priority)
	return r.result, err
}
func (a *Actor) Market(ctx context.Context, event d.MarketEvent, snapshotID string) error {
	return a.MarketFromSession(ctx, event, snapshotID, "")
}

// MarketFromSession requires a stable receipt session for trades without a
// venue trade ID. Changing book snapshot revisions are not receipt sessions.
func (a *Actor) MarketFromSession(ctx context.Context, event d.MarketEvent, snapshotID, sessionID string) error {
	_, err := a.submit(ctx, "market", marketInput{Event: event, SnapshotID: snapshotID, SessionID: sessionID}, false)
	return err
}
func (a *Actor) ObserveFill(ctx context.Context, eventID, intentID string, fill d.Fill) error {
	if !clientIdentity(eventID) || intentID == "" {
		return ErrInvalid
	}
	payload := jsonBytes(Fact{ID: eventID, IntentID: intentID, Fill: fill})
	if _, err := a.config.Store.RecordObservation(ctx, a.config.Key, pg.Observation{ID: eventID, Kind: "fill", Payload: payload}); err != nil {
		return err
	}
	_, err := a.submit(ctx, "facts", nil, true)
	return err
}
func (a *Actor) Reconcile(ctx context.Context, r Reconciliation) (pg.Result, error) {
	if !clientIdentity(r.ID) {
		return pg.Result{}, ErrInvalid
	}
	out, err := a.submit(ctx, "reconcile", r, true)
	return out.result, err
}

// Public command IDs cannot collide with reducer and paper-source receipts.
func clientIdentity(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, prefix := range []string{"boot-", "deadline-", "dispatch-market-", "market-invalid-", "clock-invalid-", "observation-", "paper-", "transport-", "runtime-"} {
		if strings.HasPrefix(id, prefix) {
			return false
		}
	}
	return true
}
func (a *Actor) View(ctx context.Context) (View, error) {
	r, err := a.submit(ctx, "view", nil, false)
	return r.view, err
}
func (a *Actor) Close(ctx context.Context) error {
	a.once.Do(func() { a.admission.Lock(); a.closing = true; close(a.stop); a.admission.Unlock() })
	select {
	case <-a.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Actor) loop() {
	defer close(a.done)
	defer func() {
		if a.policyCancel != nil {
			a.policyCancel()
		}
	}()
	interval := min(a.config.Lease/3, time.Second)
	if a.config.AutoExecute {
		interval = min(interval, time.Duration(a.config.Execution.DecisionInterval))
	}
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	if a.config.Policy != nil {
		a.policyTimer = time.NewTimer(time.Hour)
		a.policyTimer.Stop()
		defer a.policyTimer.Stop()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		// Maintenance and control must remain runnable under sustained fact ingress.
		// Each control handler still reduces previously accepted durable facts before
		// changing the parent; ordinary proposals remain revision checked.
		select {
		case <-ticker.C:
			a.maintenance()
			continue
		default:
		}
		select {
		case r := <-a.control:
			a.handle(r)
			continue
		default:
		}
		select {
		case <-a.policyExpiry:
			a.handlePolicyExpiry()
			continue
		default:
		}
		select {
		case <-a.policyExpiry:
			a.handlePolicyExpiry()
		case result := <-a.policyResults:
			a.receivePolicy(result)
		case r := <-a.facts:
			a.handle(r)
		case r := <-a.control:
			a.handle(r)
		case r := <-a.work:
			a.handle(r)
		case <-ticker.C:
			a.maintenance()
		case <-a.stop:
			for {
				select {
				case r := <-a.facts:
					a.handle(r)
				case r := <-a.control:
					a.handle(r)
				case r := <-a.work:
					a.handle(r)
				default:
					return
				}
			}
		}
	}
}
func (a *Actor) handle(req request) {
	result := response{}
	defer func() {
		if panicValue := recover(); panicValue != nil {
			a.state.RecoveryRequired = true
			a.state.Reason = "runtime_arithmetic_failure"
			a.account.Ready = false
			result.err = fmt.Errorf("%w: runtime panic contained", ErrRecoveryRequired)
		}
		req.reply <- result
	}()
	// Once admitted, bounded persistence uses an internal deadline and survives a
	// caller disappearing. An identical command ID discovers its committed result.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	control := false
	if req.kind == "command" {
		kind := req.input.(Command).Kind
		control = kind == "pause" || kind == "cancel" || kind == "deadline"
	}
	if err := a.checkClock(ctx); err != nil && req.kind != "view" && req.kind != "market" && req.kind != "reconcile" && !control {
		result.err = err
		return
	}
	if req.kind != "view" && req.kind != "market" {
		if err := a.drainFacts(ctx); err != nil && req.kind != "reconcile" && !control {
			result.err = err
			return
		}
	}
	switch req.kind {
	case "command":
		result.result, result.err = a.command(ctx, req.input.(Command))
	case "market":
		result.err = a.market(ctx, req.input.(marketInput))
	case "facts":
		result.err = a.drainFacts(ctx)
	case "reconcile":
		result.result, result.err = a.reconcile(ctx, req.input.(Reconciliation))
	case "view":
		state, err := cloneState(a.state)
		if err != nil {
			result.err = err
			return
		}
		current, err := a.config.Store.GetAccount(ctx, a.config.Key)
		if err != nil {
			result.err = err
			return
		}
		if current.Ownership != a.owner {
			result.err = pg.ErrOwnership
			return
		}
		if !current.LeaseUntil.After(a.now()) || current.ObservedSequence != current.ProcessedSequence {
			current.Ready = false
		}
		// Repository reads are fresh owned values; clone the raw snapshot as well
		// so no mutable backing slice escapes the account's serialization.
		var account pg.Account
		if err := decodeInternal(jsonBytes(current), &account); err != nil {
			result.err = err
			return
		}
		result.view = View{Account: account, State: state}
	default:
		result.err = ErrInvalid
	}
}
func (a *Actor) maintenance() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = a.checkClock(ctx)
	if err := a.config.Store.RenewOwnership(ctx, a.config.Key, a.owner, a.config.Lease); err != nil {
		a.state.RecoveryRequired = true
		a.state.Reason = "ownership_lease_lost"
		a.account.Ready = false
		return
	}
	if account, err := a.config.Store.GetAccount(ctx, a.config.Key); err == nil {
		a.account = account
	}
	_ = a.drainFacts(ctx)
	if p := a.state.Parent; p != nil && !a.now().Before(p.Deadline) && (p.Status == "active" || p.Status == "paused" || p.Status == "cancel_requested") {
		_, _ = a.command(ctx, Command{ID: fmt.Sprintf("deadline-%s-%d", p.ID, p.Revision), ExpectedRevision: a.account.Revision, Kind: "deadline"})
	}
	if a.config.AutoExecute {
		_ = a.advance(ctx)
	}
}

// advance runs under account serialization. Cadence is derived from the last
// actual decision rather than replaying missed intervals after downtime.
func (a *Actor) advance(ctx context.Context) error {
	p := a.state.Parent
	if p == nil || p.Status != "active" || !a.account.Ready || !a.state.Book.Valid || a.state.RecoveryRequired || a.overflow.Load() || !a.now().Before(p.Deadline) {
		return nil
	}
	if child := a.state.Child; child != nil && child.State == "queued" {
		_, err := a.command(ctx, Command{ID: randomID("runtime-dispatch-"), Kind: "dispatch", ExpectedRevision: a.account.Revision, IntentID: child.ID})
		return err
	}
	if a.elapsed(a.state) < a.state.NextDecision {
		return nil
	}
	if a.config.Policy != nil {
		return a.startPolicy(ctx)
	}
	if _, err := a.command(ctx, Command{ID: randomID("runtime-decision-"), Kind: "decision", Policy: p.Baseline, ExpectedRevision: a.account.Revision}); err != nil {
		return err
	}
	if child := a.state.Child; child != nil && child.State == "queued" {
		_, err := a.command(ctx, Command{ID: randomID("runtime-dispatch-"), Kind: "dispatch", ExpectedRevision: a.account.Revision, IntentID: child.ID})
		return err
	}
	return nil
}
func (a *Actor) market(ctx context.Context, input marketInput) error {
	if input.SnapshotID == "" || len(input.SnapshotID) > 128 || len(input.Event.Bids)+len(input.Event.Asks) > 512 {
		return ErrInvalid
	}
	if input.Event.Symbol != "" && input.Event.Symbol != a.state.Rules.Symbol {
		return ErrInvalid
	}
	if input.Event.Kind != "snapshot" && input.Event.Kind != "book" && input.Event.Kind != "trade" && input.Event.Kind != "gap" {
		if err := a.invalidateMarket(ctx, "unsupported_market_event"); err != nil {
			return err
		}
		return ErrInvalid
	}
	if err := a.state.Book.Apply(input.Event); err != nil {
		a.state.Book.Valid = false
		if persistErr := a.invalidateMarket(ctx, "invalid_market_event"); persistErr != nil {
			return persistErr
		}
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	a.state.MarketID = input.SnapshotID
	if len(a.state.Book.Bids)+len(a.state.Book.Asks) > 2048 {
		a.state.Book = x.NewBook()
		if err := a.invalidateMarket(ctx, "book_capacity_exceeded"); err != nil {
			return err
		}
		return ErrHistoryLimit
	}
	if !a.state.Book.Valid {
		return a.invalidateMarket(ctx, "market_invalid")
	}
	if input.Event.Kind == "trade" {
		if err := a.drainFacts(ctx); err != nil {
			return err
		}
		event := input.Event
		event.Time = a.elapsed(a.state)
		if event.Symbol == "" {
			event.Symbol = a.state.Rules.Symbol
		}
		session := input.SessionID
		if session == "" && event.TradeID != "" {
			session = "venue-trade-id"
		}
		child := a.state.Child
		if a.state.RecoveryRequired {
			child = nil
		}
		if _, err := a.transport.(*paperTransport).ObserveTrade(ctx, session, event, child, a.state.Fees); err != nil {
			if persistErr := a.invalidateMarket(ctx, "invalid_or_unrecorded_trade"); persistErr != nil {
				return persistErr
			}
			return err
		}
		return a.drainFacts(ctx)
	}
	return nil
}

func (a *Actor) invalidateMarket(ctx context.Context, reason string) error {
	a.state.Book.Valid = false
	a.state.RecoveryRequired = true
	a.state.Reason = reason
	a.account.Ready = false
	next, err := cloneState(a.state)
	if err != nil {
		return err
	}
	ready := false
	_, err = a.persist(ctx, next, pg.Command{ID: randomID("market-invalid-"), Kind: "market_invalid", ExpectedRevision: a.account.Revision}, pg.Change{Ready: &ready})
	return err
}

func (a *Actor) checkClock(ctx context.Context) error {
	now := a.now()
	previous := a.lastClock
	a.lastClock = now
	if previous.IsZero() {
		return nil
	}
	// Sub retains Go's monotonic component; Unix timestamps deliberately do not.
	drift := time.Duration(now.UnixNano()-previous.UnixNano()) - now.Sub(previous)
	if now.Before(previous) || drift > time.Second || drift < -time.Second {
		a.state.ClockDiscontinuity = true
		if err := a.invalidateMarket(ctx, "clock_discontinuity"); err != nil {
			return err
		}
		return ErrRecoveryRequired
	}
	return nil
}
func (a *Actor) persist(ctx context.Context, next State, command pg.Command, extra pg.Change) (pg.Result, error) {
	return a.persistRevision(ctx, next, command, extra, false)
}

func (a *Actor) persistRevision(ctx context.Context, next State, command pg.Command, extra pg.Change, preserveAuthority bool) (pg.Result, error) {
	if !preserveAuthority {
		next.AuthorityRevision = a.account.Revision + 1
	}
	extra.Key = a.config.Key
	extra.Ownership = a.owner
	extra.Command = command
	extra.Snapshot = jsonBytes(next)
	extra.Market = &pg.Market{Valid: next.Book.Valid, SnapshotID: nonempty(next.MarketID, "awaiting-snapshot"), InstrumentRevision: next.InstrumentRevision}
	result, err := a.config.Store.Apply(ctx, extra)
	if err != nil {
		if errors.Is(err, pg.ErrOwnership) {
			a.state.RecoveryRequired = true
			a.state.Reason = "ownership_lease_lost"
			a.account.Ready = false
		}
		return result, err
	}
	account, err := a.config.Store.GetAccount(ctx, a.config.Key)
	if err != nil {
		a.state.RecoveryRequired = true
		a.account.Ready = false
		return result, fmt.Errorf("%w: committed state reload failed: %v", ErrOutcomeUnknown, err)
	}
	a.account = account
	a.state = next
	return result, nil
}
func (a *Actor) ledger(state State) (*x.Ledger, error) {
	if state.Parent == nil {
		return nil, ErrInvalid
	}
	ledger := x.NewLedger(state.Parent.Spec)
	for _, fill := range state.Fills {
		if _, err := ledger.Apply(fill); err != nil {
			return nil, err
		}
	}
	return ledger, nil
}
func inputDigest(v any) string { sum := sha256.Sum256(jsonBytes(v)); return hex.EncodeToString(sum[:]) }
func (a *Actor) duplicate(ctx context.Context, id, digest string) (pg.Result, bool, error) {
	record, err := a.config.Store.LookupCommand(ctx, a.config.Key, id)
	if errors.Is(err, pg.ErrNotFound) {
		return pg.Result{}, false, nil
	}
	if err != nil {
		return pg.Result{}, false, err
	}
	var state State
	if err = decodeInternal(record.Snapshot, &state); err != nil {
		return pg.Result{}, true, err
	}
	if state.LastInputID != id || state.LastInputDigest != digest {
		return pg.Result{}, true, pg.ErrConflict
	}
	record.Result.Duplicate = true
	return record.Result, true, nil
}
func (a *Actor) stamp(next *State, id, digest string) {
	next.LastInputID = id
	next.LastInputDigest = digest
}

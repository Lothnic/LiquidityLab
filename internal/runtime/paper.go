package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	"github.com/liquiditylab/liquiditylab/internal/faultpoint"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

const paperTransportKind = "transport"
const paperTradeKind = "paper_trade"

// paperTradeFact freezes the result against the child visible when the unique
// source event was first observed. A retry must never rematch against a new child.
type paperTradeFact struct {
	ID          string        `json:"id"`
	InputDigest string        `json:"input_digest"`
	SourceID    string        `json:"source_id"`
	Event       d.MarketEvent `json:"event"`
	IntentID    string        `json:"intent_id,omitempty"`
	QueueAhead  d.Decimal     `json:"queue_ahead"`
	Fills       []d.Fill      `json:"fills"`
}

// paperTradeIdentity scopes venue execution identities by instrument. Receipts
// without a venue trade ID require a stable receipt-session sourceID, retained
// by ingress across retries. A reconnect must use a new receipt-session identity.
func paperTradeIdentity(sourceID string, event d.MarketEvent) (string, string) {
	identity := struct {
		Symbol, TradeID, SourceID string
		ReceiptIndex              uint64
	}{Symbol: event.Symbol, TradeID: event.TradeID}
	canonical := event
	// Time is assigned at the account boundary relative to its current parent.
	// Re-observing one source event later must retain the first frozen fill time.
	canonical.Time = 0
	if event.TradeID == "" {
		identity.SourceID = sourceID
		identity.ReceiptIndex = event.ReceiptIndex
	} else {
		// Repeated receipts of one venue trade have one economic identity.
		canonical.ReceiptIndex = 0
	}
	id := fmt.Sprintf("paper-trade-%x", sha256.Sum256(jsonBytes(identity)))
	digest := fmt.Sprintf("%x", sha256.Sum256(jsonBytes(struct {
		Identity any
		Event    d.MarketEvent
	}{identity, canonical})))
	return id, digest
}

func (p *paperTransport) storedTrade(ctx context.Context, id, digest string) (paperTradeFact, bool, error) {
	observation, err := p.store.GetObservation(ctx, p.key, id)
	if errors.Is(err, pg.ErrNotFound) {
		return paperTradeFact{}, false, nil
	}
	if err != nil {
		return paperTradeFact{}, false, err
	}
	var fact paperTradeFact
	if observation.Kind != paperTradeKind || decodeInternal(observation.Payload, &fact) != nil || fact.ID != id || fact.InputDigest != digest {
		return paperTradeFact{}, false, pg.ErrConflict
	}
	// Retrying an existing effect still requires the current paper epoch. The
	// store checks ownership before deduplication and never appends this retry.
	if _, err := p.store.RecordPaperObservation(ctx, p.key, p.owner, observation); err != nil {
		return paperTradeFact{}, false, err
	}
	return fact, true, nil
}

// ObserveTrade persists simulated queue movement and fills before account state
// changes. The serialized account owner must drain preceding observations before
// calling it for another trade. Even ignored trades are recorded for durable
// deduplication. This per-trade database cost has not been capacity benchmarked.
// No in-memory child, book, balance, or reservation is changed here.
func (p *paperTransport) ObserveTrade(ctx context.Context, sourceID string, event d.MarketEvent, child *Child, fees d.FeeConfig) (paperTradeFact, error) {
	if p == nil || p.store == nil {
		return paperTradeFact{}, ErrInvalid
	}
	account, err := p.store.GetAccount(ctx, p.key)
	if err != nil {
		return paperTradeFact{}, err
	}
	fact, _, err := p.observeTradeAt(ctx, sourceID, event, child, fees, account.Revision)
	return fact, err
}

func (p *paperTransport) observeTradeAt(ctx context.Context, sourceID string, event d.MarketEvent, child *Child, fees d.FeeConfig, revision int64) (paperTradeFact, int64, error) {
	if p == nil || p.store == nil || sourceID == "" || len(sourceID) > 128 ||
		event.Kind != "trade" || event.Invalid || event.Time < 0 ||
		(event.Side != "buy" && event.Side != "sell") || event.Price.Sign() <= 0 || event.Quantity.Sign() <= 0 ||
		len(event.Symbol) > 128 || len(event.TradeID) > 128 || len(event.Bids)+len(event.Asks) != 0 {
		return paperTradeFact{}, 0, ErrInvalid
	}
	id, digest := paperTradeIdentity(sourceID, event)
	prior, found, err := p.storedTrade(ctx, id, digest)
	if err != nil || found {
		if err != nil {
			return prior, 0, err
		}
		observation, readErr := p.store.GetObservation(ctx, p.key, id)
		return prior, observation.Sequence, readErr
	}
	fact := paperTradeFact{ID: id, InputDigest: digest, SourceID: sourceID, Event: event, QueueAhead: d.Zero, Fills: []d.Fill{}}
	if child != nil && child.Order.Intent.Passive {
		if child.ID == "" || child.ID != child.Order.OrderID || child.Order.Remaining.Sign() < 0 || child.Order.QueueAhead.Sign() < 0 {
			return paperTradeFact{}, 0, ErrInvalid
		}
		match := x.MatchPassiveTrade(child.Order, event)
		fact.IntentID, fact.QueueAhead = child.ID, match.QueueAhead
		if match.Quantity.Sign() > 0 {
			if err := fees.Validate(); err != nil {
				return paperTradeFact{}, 0, ErrInvalid
			}
			fee, err := x.FeeFor(match.Quantity, child.Order.Intent.Price, fees.Maker, fees.Currency)
			if err != nil {
				return paperTradeFact{}, 0, err
			}
			fact.Fills = append(fact.Fills, d.Fill{
				ExecutionID: id + "-fill", OrderID: child.ID, Side: child.Order.Intent.Side,
				Quantity: match.Quantity, Price: child.Order.Intent.Price,
				Fee: fee, FeeCurrency: fees.Currency, Time: event.Time, Maker: true,
			})
		}
	}
	sequence, err := p.store.RecordPaperTradeObservation(ctx, p.key, p.owner, pg.Observation{ID: id, Kind: paperTradeKind, Payload: jsonBytes(fact)}, revision)
	if err != nil {
		if errors.Is(err, pg.ErrNotReady) || errors.Is(err, pg.ErrStaleRevision) {
			return paperTradeFact{}, 0, err
		}
		if errors.Is(err, pg.ErrOwnership) {
			return paperTradeFact{}, 0, err
		}
		// A concurrent or uncertain first write may have committed a result based
		// on earlier child state. Its durable result wins over any recomputation.
		if prior, found, readErr := p.storedTrade(ctx, id, digest); readErr == nil && found {
			observation, readErr := p.store.GetObservation(ctx, p.key, id)
			return prior, observation.Sequence, readErr
		} else if errors.Is(readErr, pg.ErrConflict) {
			return paperTradeFact{}, 0, readErr
		}
		return paperTradeFact{}, 0, fmt.Errorf("%w: paper trade observation persistence: %w", ErrOutcomeUnknown, err)
	}
	if fact.IntentID != "" {
		faultpoint.Hit("trade_fact_committed")
	}
	return fact, sequence, nil
}

// paperTransport persists each simulated venue result as one durable inbox
// observation. Retries discover that fact from PostgreSQL; this object keeps no
// authoritative order or fill state.
type paperTransport struct {
	store *pg.Store
	key   pg.Key
	owner pg.Ownership
}

func newPaperTransport(store *pg.Store, key pg.Key, owner pg.Ownership) *paperTransport {
	return &paperTransport{store: store, key: key, owner: owner}
}

func (*paperTransport) Profile() Profile { return ProfilePaper }

func paperIdentity(id, prefix string) bool {
	return id != "" && len(prefix)+len(id) <= 128
}

func unknownPaperOutcome(err error) (TransportResult, error) {
	return TransportResult{
		Handoff:    PotentiallyInFlight,
		KnownFinal: false,
		Reason:     "paper_transport_observation_unknown",
	}, fmt.Errorf("%w: paper transport fact persistence or lookup: %w", ErrOutcomeUnknown, err)
}

func rejectedPaper(reason string) TransportResult {
	return TransportResult{Handoff: RejectedBeforeHandoff, KnownFinal: true, Reason: reason}
}

func (p *paperTransport) stored(ctx context.Context, id, intentID string) (transportFact, bool, error) {
	observation, err := p.store.GetObservation(ctx, p.key, id)
	if errors.Is(err, pg.ErrNotFound) {
		return transportFact{}, false, nil
	}
	if err != nil {
		return transportFact{}, false, err
	}
	if observation.Kind != paperTransportKind {
		return transportFact{}, false, pg.ErrConflict
	}
	var fact transportFact
	if err = decodeInternal(observation.Payload, &fact); err != nil {
		return transportFact{}, false, err
	}
	if fact.IntentID != intentID {
		return transportFact{}, false, pg.ErrConflict
	}
	if _, err := p.store.RecordPaperObservation(ctx, p.key, p.owner, observation); err != nil {
		return transportFact{}, false, err
	}
	return fact, true, nil
}

func (p *paperTransport) persist(ctx context.Context, observationID string, fact transportFact) (TransportResult, error) {
	payload, err := json.Marshal(fact)
	if err != nil {
		return unknownPaperOutcome(err)
	}
	if _, err = p.store.RecordPaperObservation(ctx, p.key, p.owner, pg.Observation{
		ID: observationID, Kind: paperTransportKind, Payload: payload,
	}); err != nil {
		return unknownPaperOutcome(err)
	}
	if strings.HasPrefix(observationID, "paper-cancel-") {
		faultpoint.Hit("cancel_effect_committed")
	} else {
		faultpoint.Hit("submit_effect_committed")
	}
	return fact.Outcome, nil
}

func (p *paperTransport) Handoff(ctx context.Context, submission Submission) (TransportResult, error) {
	if p == nil || p.store == nil || !paperIdentity(submission.ID, "paper-submit-") ||
		!paperIdentity(submission.ID, "paper-cancel-") || submission.OperationID == "" ||
		submission.InputDigest == "" || len(submission.OperationID) > 128 || len(submission.InputDigest) > 128 {
		return rejectedPaper("invalid_paper_submission"), ErrInvalid
	}
	submitID := "paper-submit-" + submission.ID
	prior, found, err := p.stored(ctx, submitID, submission.ID)
	if err != nil {
		if errors.Is(err, pg.ErrConflict) {
			return rejectedPaper("paper_submission_identity_conflict"), err
		}
		return unknownPaperOutcome(err)
	}
	if found {
		if prior.OperationID != submission.OperationID || prior.InputDigest != submission.InputDigest || prior.Intent == nil || !prior.Intent.Equal(submission.Intent) {
			return rejectedPaper("paper_submission_identity_conflict"), pg.ErrConflict
		}
		return prior.Outcome, nil
	}
	cancelID := "paper-cancel-" + submission.ID
	cancel, found, err := p.stored(ctx, cancelID, submission.ID)
	if err != nil {
		if errors.Is(err, pg.ErrConflict) {
			return rejectedPaper("paper_cancellation_identity_conflict"), err
		}
		return unknownPaperOutcome(err)
	}
	if found {
		if cancel.OperationID != cancelID || cancel.InputDigest != "paper-cancel" || !cancel.Outcome.KnownFinal {
			return rejectedPaper("paper_cancellation_identity_conflict"), pg.ErrConflict
		}
		return rejectedPaper("paper_child_already_cancelled"), nil
	}

	outcome := p.simulate(submission)
	fact := transportFact{
		IntentID: submission.ID, OperationID: submission.OperationID,
		InputDigest: submission.InputDigest, Outcome: outcome, Intent: &submission.Intent,
	}
	return p.persist(ctx, submitID, fact)
}

func (p *paperTransport) Cancel(ctx context.Context, childID string) (TransportResult, error) {
	if p == nil || p.store == nil || !paperIdentity(childID, "paper-cancel-") {
		return rejectedPaper("invalid_paper_cancellation"), ErrInvalid
	}
	cancelID := "paper-cancel-" + childID
	prior, found, err := p.stored(ctx, cancelID, childID)
	if err != nil {
		if errors.Is(err, pg.ErrConflict) {
			return rejectedPaper("paper_cancellation_identity_conflict"), err
		}
		return unknownPaperOutcome(err)
	}
	if found {
		if prior.OperationID != cancelID || prior.InputDigest != "paper-cancel" {
			return rejectedPaper("paper_cancellation_identity_conflict"), pg.ErrConflict
		}
		return prior.Outcome, nil
	}
	fact := transportFact{
		IntentID: childID, OperationID: cancelID, InputDigest: "paper-cancel",
		Outcome: TransportResult{
			Handoff: PotentiallyInFlight, Accepted: true, KnownFinal: true,
			VenueOrderID: "paper-" + childID, Reason: "paper_cancelled",
			Fills: []d.Fill{}, QueueAhead: d.Zero,
		},
	}
	return p.persist(ctx, cancelID, fact)
}

func (p *paperTransport) simulate(submission Submission) TransportResult {
	intent := submission.Intent
	if submission.Book == nil || !submission.Book.Valid || submission.Book.BestBid().Sign() <= 0 ||
		submission.Book.BestAsk().Sign() <= 0 || submission.Book.BestBid().Cmp(submission.Book.BestAsk()) >= 0 {
		return rejectedPaper("invalid_market_data")
	}
	if intent.Quantity.Sign() <= 0 || intent.Price.Sign() <= 0 ||
		(intent.Side != "buy" && intent.Side != "sell") {
		return rejectedPaper("invalid_order_intent")
	}
	if err := submission.Fees.Validate(); err != nil {
		return rejectedPaper("invalid_fee_assumption")
	}
	book := clonePaperBook(submission.Book)
	if intent.Passive {
		crosses := intent.Side == "buy" && intent.Price.Cmp(book.BestAsk()) >= 0 ||
			intent.Side == "sell" && intent.Price.Cmp(book.BestBid()) <= 0
		if crosses {
			return rejectedPaper("post_only_would_cross")
		}
		side := "bids"
		if intent.Side == "sell" {
			side = "asks"
		}
		return TransportResult{
			Handoff: PotentiallyInFlight, Accepted: true, KnownFinal: false,
			VenueOrderID: "paper-" + submission.ID, Reason: "paper_resting_order_accepted",
			Fills: []d.Fill{}, QueueAhead: book.Available(side, intent.Price),
		}
	}

	bookSide := "asks"
	if intent.Side == "sell" {
		bookSide = "bids"
	}
	outcome := TransportResult{
		Handoff: PotentiallyInFlight, Accepted: true, KnownFinal: true,
		VenueOrderID: "paper-" + submission.ID, Reason: "paper_ioc_complete",
		Fills: []d.Fill{}, QueueAhead: d.Zero,
	}
	remaining := intent.Quantity
	for _, price := range book.Prices(bookSide) {
		if remaining.IsZero() {
			break
		}
		if intent.Side == "buy" && price.Cmp(intent.Price) > 0 ||
			intent.Side == "sell" && price.Cmp(intent.Price) < 0 {
			break
		}
		quantity := d.Min(remaining, book.Available(bookSide, price))
		if quantity.IsZero() {
			continue
		}
		fee, err := x.FeeFor(quantity, price, submission.Fees.Taker, submission.Fees.Currency)
		if err != nil {
			return rejectedPaper("invalid_fee_assumption")
		}
		outcome.Fills = append(outcome.Fills, d.Fill{
			ExecutionID: fmt.Sprintf("paper-%s-%d", submission.ID, len(outcome.Fills)+1),
			OrderID:     submission.ID, Side: intent.Side, Quantity: quantity,
			Price: price, Fee: fee, FeeCurrency: submission.Fees.Currency,
			Time: submission.Now, Maker: false,
		})
		book.Consume(bookSide, price, quantity)
		remaining = remaining.Sub(quantity)
	}
	if remaining.Sign() > 0 {
		outcome.Reason = "paper_ioc_partial_or_unfilled"
	}
	return outcome
}

func clonePaperBook(source *x.Book) *x.Book {
	book := x.NewBook()
	book.Valid = source.Valid
	for _, side := range []string{"bids", "asks"} {
		for price, level := range source.Levels(side) {
			book.Levels(side)[price] = level
		}
		for price, debt := range source.Debt[side] {
			book.Debt[side][price] = debt
		}
	}
	return book
}

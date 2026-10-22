package replayrpc

import (
	"fmt"
	"math"

	pb "github.com/liquiditylab/liquiditylab/gen/go/liquiditylab/v1"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"github.com/liquiditylab/liquiditylab/internal/replay"
)

// decimal validates the wire spelling before invoking arbitrary precision math.
func decimal(v *pb.Decimal, required bool) (d.Decimal, error) {
	if v == nil {
		if required {
			return d.Zero, fmt.Errorf("missing decimal")
		}
		return d.Zero, nil
	}
	if v.Scale > d.MaxInputScale || len(v.Coefficient) > d.MaxInputDigits+1 {
		return d.Zero, fmt.Errorf("decimal exceeds input envelope")
	}
	x, err := d.FromCoefficient(v.Coefficient, int32(v.Scale))
	if err != nil {
		return d.Zero, err
	}
	if x.Coefficient() != v.Coefficient || uint32(x.Scale()) != v.Scale {
		return d.Zero, fmt.Errorf("decimal must use canonical coefficient and scale")
	}
	return x, nil
}

func wireDecimal(v d.Decimal) *pb.Decimal {
	return &pb.Decimal{Coefficient: v.Coefficient(), Scale: uint32(v.Scale())}
}

func side(v pb.Side) (string, error) {
	switch v {
	case pb.Side_SIDE_BUY:
		return "buy", nil
	case pb.Side_SIDE_SELL:
		return "sell", nil
	}
	return "", fmt.Errorf("side is unspecified or unknown")
}

func episode(v *pb.EpisodeSpec, limits Limits) ([]d.MarketEvent, d.Plan, d.InstrumentRules, d.FeeConfig, d.ReplayConfig, error) {
	var events []d.MarketEvent
	var plan d.Plan
	var rules d.InstrumentRules
	var fees d.FeeConfig
	config := d.DefaultConfig()
	fail := func(err error) ([]d.MarketEvent, d.Plan, d.InstrumentRules, d.FeeConfig, d.ReplayConfig, error) {
		return nil, plan, rules, fees, config, err
	}
	if v == nil || v.Plan == nil || v.Rules == nil || v.Fees == nil {
		return fail(fmt.Errorf("episode, plan, rules, and fees are required"))
	}
	if len(v.Events) == 0 || len(v.Events) > limits.MaxEvents {
		return fail(fmt.Errorf("event count outside limit"))
	}
	var err error
	plan.Side, err = side(v.Plan.Side)
	if err != nil {
		return fail(err)
	}
	plan.Symbol = v.Plan.Symbol
	plan.Deadline = v.Plan.DeadlineNs
	values := []struct {
		src *pb.Decimal
		dst *d.Decimal
	}{
		{v.Plan.Quantity, &plan.Quantity}, {v.Plan.LimitPrice, &plan.LimitPrice}, {v.Plan.MaxChildQuantity, &plan.MaxChildQuantity}, {v.Plan.QuoteBudget, &plan.QuoteBudget}, {v.Plan.BaseBalance, &plan.BaseBalance},
		{v.Rules.TickSize, &rules.TickSize}, {v.Rules.LotSize, &rules.LotSize}, {v.Rules.MinQuantity, &rules.MinQuantity}, {v.Rules.MinNotional, &rules.MinNotional},
		{v.Fees.Maker, &fees.Maker}, {v.Fees.Taker, &fees.Taker},
	}
	for _, value := range values {
		*value.dst, err = decimal(value.src, true)
		if err != nil {
			return fail(err)
		}
	}
	rules.Symbol = v.Rules.Symbol
	fees.Source = v.Fees.Source
	switch v.Fees.Currency {
	case pb.FeeCurrency_FEE_QUOTE:
		fees.Currency = "quote"
	case pb.FeeCurrency_FEE_BASE:
		fees.Currency = "base"
	default:
		return fail(fmt.Errorf("fee currency is unspecified or unknown"))
	}
	if c := v.Config; c != nil {
		if c.DecisionIntervalNs != nil {
			config.DecisionInterval = *c.DecisionIntervalNs
		}
		if c.SubmissionLatencyNs != nil {
			config.SubmissionLatency = *c.SubmissionLatencyNs
		}
		if c.CancelLatencyNs != nil {
			config.CancelLatency = *c.CancelLatencyNs
		}
		if c.SettlementAllowanceNs != nil {
			config.SettlementAllowance = *c.SettlementAllowanceNs
		}
		if c.LambdaMiss != nil {
			config.LambdaMiss = *c.LambdaMiss
		}
		if c.MaxParticipation != nil {
			config.MaxParticipation, err = decimal(c.MaxParticipation, true)
			if err != nil {
				return fail(err)
			}
		}
		if c.MinRestingAgeNs != nil {
			config.MinRestingAge = *c.MinRestingAgeNs
		}
		if c.MinMessageIntervalNs != nil {
			config.MinMessageInterval = *c.MinMessageIntervalNs
		}
		if c.FillModel != "" {
			config.FillModel = c.FillModel
		}
	}
	if config.FillModel != "conservative-v0" {
		return fail(fmt.Errorf("unsupported fill model"))
	}
	for _, err := range []error{plan.Validate(), rules.Validate(), fees.Validate(), config.Validate()} {
		if err != nil {
			return fail(err)
		}
	}
	// Bound all engine-owned history before constructing engines. Steps can still
	// be explicitly reset early; exceeding the runtime history limit poisons a batch.
	if (plan.Deadline-1)/config.DecisionInterval >= int64(limits.MaxSteps) {
		return fail(fmt.Errorf("episode exceeds decision step limit"))
	}
	levels := 0
	for _, event := range v.Events {
		if event == nil {
			return fail(fmt.Errorf("nil market event"))
		}
		if len(event.Bids)+len(event.Asks) > limits.MaxLevelsPerEvent {
			return fail(fmt.Errorf("book depth exceeds event limit"))
		}
		levels += len(event.Bids) + len(event.Asks)
		if levels > limits.MaxLevels {
			return fail(fmt.Errorf("episode book levels exceed limit"))
		}
		e := d.MarketEvent{Time: event.ReceiptTimeNs, Symbol: event.Symbol, ReceiptIndex: event.ReceiptIndex, TradeID: event.TradeId, Invalid: event.Valid != nil && !*event.Valid}
		switch event.Kind {
		case pb.EventKind_EVENT_SNAPSHOT:
			e.Kind = "snapshot"
		case pb.EventKind_EVENT_BOOK:
			e.Kind = "book"
		case pb.EventKind_EVENT_TRADE:
			e.Kind = "trade"
		case pb.EventKind_EVENT_GAP:
			e.Kind = "gap"
		default:
			return fail(fmt.Errorf("unsupported event kind"))
		}
		for _, pair := range []struct {
			src []*pb.PriceLevel
			dst *[]d.Level
		}{{event.Bids, &e.Bids}, {event.Asks, &e.Asks}} {
			for _, level := range pair.src {
				if level == nil {
					return fail(fmt.Errorf("nil book level"))
				}
				p, err := decimal(level.Price, true)
				if err != nil {
					return fail(err)
				}
				q, err := decimal(level.Quantity, true)
				if err != nil {
					return fail(err)
				}
				*pair.dst = append(*pair.dst, d.Level{Price: p, Quantity: q})
			}
		}
		e.Price, err = decimal(event.Price, e.Kind == "trade" && !e.Invalid)
		if err != nil {
			return fail(err)
		}
		e.Quantity, err = decimal(event.Quantity, e.Kind == "trade" && !e.Invalid)
		if err != nil {
			return fail(err)
		}
		if event.AggressorSide != pb.Side_SIDE_UNSPECIFIED {
			e.Side, err = side(event.AggressorSide)
			if err != nil {
				return fail(err)
			}
		} else if e.Kind == "trade" && !e.Invalid {
			return fail(fmt.Errorf("trade aggressor side required"))
		}
		events = append(events, e)
	}
	return events, plan, rules, fees, config, nil
}

func transition(id string, r replay.StepResult) (*pb.Transition, error) {
	if math.IsNaN(r.Reward) || math.IsInf(r.Reward, 0) {
		return nil, fmt.Errorf("nonfinite engine reward")
	}
	for _, v := range r.Observation {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, fmt.Errorf("nonfinite engine observation")
		}
	}
	return &pb.Transition{EnvironmentId: id, Observation: &pb.Observation{Features: r.Observation, SchemaVersion: replay.ObservationVersion, FieldNames: append([]string(nil), replay.ObservationFields...)}, Reward: r.Reward, Terminated: r.Terminated, Truncated: r.Truncated, EconomicScoreValid: r.Terminated && !r.Truncated, Status: r.Info.Status, Reason: r.Info.Reason, ReceiptTimeNs: r.Info.Time, FilledQuantity: wireDecimal(r.Info.Filled), RemainingQuantity: wireDecimal(r.Info.Remaining)}, nil
}

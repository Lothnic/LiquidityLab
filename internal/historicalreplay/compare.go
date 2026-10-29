package historicalreplay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
	"github.com/liquiditylab/liquiditylab/internal/marketarchive"
	"github.com/liquiditylab/liquiditylab/internal/marketdata"
	"github.com/liquiditylab/liquiditylab/internal/replay"
)

// ErrArchiveUnavailable distinguishes missing or unverifiable source bytes
// from a comparison which cannot run under the supplied execution assumptions.
var ErrArchiveUnavailable = errors.New("archive unavailable or invalid")

type BuildIdentity struct {
	GoVersion   string `json:"go_version"`
	Module      string `json:"module"`
	Version     string `json:"version"`
	VCSRevision string `json:"vcs_revision"`
	VCSModified string `json:"vcs_modified"`
}

type Versions struct {
	Engine      string        `json:"engine"`
	Baseline    string        `json:"baseline"`
	Compiler    string        `json:"compiler"`
	Environment string        `json:"environment"`
	Observation string        `json:"observation"`
	Build       BuildIdentity `json:"build"`
}

type Valuation struct {
	Reference string    `json:"reference"`
	TimeNS    int64     `json:"time_ns"`
	Midpoint  d.Decimal `json:"midpoint"`
}

type ScenarioResult struct {
	Label                       string        `json:"label"`
	Policy                      string        `json:"policy"`
	EstimateOnly                bool          `json:"estimate_only"`
	Simulation                  replay.Report `json:"simulation"`
	ComparisonValuation         Valuation     `json:"comparison_valuation"`
	ComparisonMarkedCostQuote   d.Decimal     `json:"comparison_marked_cost_quote"`
	NetBaseChange               d.Decimal     `json:"net_base_change"`
	NetQuoteChange              d.Decimal     `json:"net_quote_change"`
	Comparable                  bool          `json:"comparable"`
	ComparabilityReasons        []string      `json:"comparability_reasons"`
	EstimatedCostReductionQuote *d.Decimal    `json:"estimated_cost_reduction_quote"`
}

type Selection struct {
	StartTimeNS                 int64  `json:"start_time_ns"`
	RequiredEndNS               int64  `json:"required_end_ns"`
	ArchiveCoverageEndNS        int64  `json:"archive_coverage_end_ns"`
	Events                      int    `json:"events"`
	FirstSourceReceiptIndex     uint64 `json:"first_source_receipt_index"`
	LastEventSourceReceiptIndex uint64 `json:"last_event_source_receipt_index"`
	SHA256                      string `json:"sha256"`
	OmittedPreOriginGaps        int    `json:"omitted_pre_origin_gaps"`
}

type Report struct {
	Schema               string                 `json:"schema"`
	Mode                 string                 `json:"mode"`
	EstimateOnly         bool                   `json:"estimate_only"`
	SourceSessionBinding any                    `json:"source_session_binding"`
	Archive              marketarchive.Manifest `json:"archive"`
	Selection            Selection              `json:"selection"`
	ResolvedSpec         Spec                   `json:"resolved_spec"`
	ResolvedSpecSHA256   string                 `json:"resolved_spec_sha256"`
	Versions             Versions               `json:"versions"`
	Reference            ScenarioResult         `json:"reference"`
	Alternatives         []ScenarioResult       `json:"alternatives"`
	DeltaMeaning         string                 `json:"delta_meaning"`
	Limitations          []string               `json:"limitations"`
}

func buildIdentity() BuildIdentity {
	b := BuildIdentity{GoVersion: runtime.Version(), Module: "unknown", Version: "unknown", VCSRevision: "unknown", VCSModified: "unknown"}
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Path != "" {
			b.Module = info.Main.Path
		}
		if info.Main.Version != "" {
			b.Version = info.Main.Version
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				b.VCSRevision = setting.Value
			case "vcs.modified":
				b.VCSModified = setting.Value
			}
		}
	}
	return b
}

// Compare uses the first verified snapshot as time zero. Source gaps strictly
// before that snapshot's elapsed time are retained in archive provenance but
// excluded after recovery. Equal-time gaps are conservatively rejected. The
// selected interval includes every receipt at its end, preserving tape order.
func Compare(ctx context.Context, root, id string, spec Spec) (Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	end, err := spec.validate()
	if err != nil {
		return Report{}, err
	}
	recording, err := marketarchive.Load(ctx, root, id)
	if err != nil {
		return Report{}, fmt.Errorf("%w: %w", ErrArchiveUnavailable, err)
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	return compareRecording(ctx, recording, spec, end)
}

func compareRecording(ctx context.Context, recording marketarchive.Recording, spec Spec, end int64) (Report, error) {
	if end > recording.Manifest.CoverageEndNS {
		return Report{}, errors.New("sealed archive does not cover every deadline plus settlement allowance")
	}
	if spec.Rules.Symbol != recording.Manifest.Source.Symbol {
		return Report{}, errors.New("spec and archive symbols differ")
	}
	selection := Selection{RequiredEndNS: end, ArchiveCoverageEndNS: recording.Manifest.CoverageEndNS, FirstSourceReceiptIndex: recording.Manifest.FirstSnapshotSourceReceiptIndex}
	for _, gap := range recording.Manifest.Gaps {
		if gap.ElapsedNS < recording.Manifest.FirstSnapshotElapsedNS {
			selection.OmittedPreOriginGaps++
			continue
		}
		if gap.ElapsedNS-recording.Manifest.FirstSnapshotElapsedNS <= end {
			return Report{}, errors.New("selected inclusive interval contains an archive source or checksum gap")
		}
	}
	events, err := selectEvents(ctx, recording.Events, end, recording.Manifest.Source.Symbol)
	if err != nil {
		return Report{}, err
	}
	selection.Events = len(events)
	selection.LastEventSourceReceiptIndex = recording.Events[len(events)-1].SourceReceiptIndex
	tape, err := replay.NewTapeWithCoverage(events, end)
	if err != nil {
		return Report{}, err
	}
	resolved, _ := json.Marshal(spec)
	digest := sha256.Sum256(resolved)
	r := Report{
		Schema: ReportSchema, Mode: "historical-simulation-comparison", EstimateOnly: true,
		Archive: recording.Manifest, Selection: selection, ResolvedSpec: spec, ResolvedSpecSHA256: hex.EncodeToString(digest[:]),
		Versions:     Versions{Engine: "replay-engine-v1", Baseline: "scheduled-liquidity-aware-v1", Compiler: "six-action-v1", Environment: d.EnvironmentVersion, Observation: replay.ObservationVersion, Build: buildIdentity()},
		Alternatives: []ScenarioResult{}, DeltaMeaning: "estimated_cost_reduction_quote = reference comparison_marked_cost_quote minus alternative comparison_marked_cost_quote; positive means lower estimated marked cost, not realized savings",
		Limitations: []string{"All scenarios, including the reference, are simulations with explicit assumed rules and fees; venue authenticity is not independently established.", "No production source-session binding; source_session_binding is null.", "The selected interval starts at the first verified snapshot; strictly earlier source gaps may be omitted after recovery, while equal-time gaps are rejected.", "Conservative L2 queue model uses public trade prints, without market impact response; price features warm up over ten completed decision intervals.", "Comparison uses the source book midpoint frozen at each plan deadline, independently of execution timing; the engine retains its own terminal valuation."},
	}
	totalHistory := 0
	scenarios := spec.scenarios()
	for index, scenario := range scenarios {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		engine, err := replay.NewWithTape(tape, scenario.Plan, spec.Rules, spec.Fees, spec.Simulation)
		if err != nil {
			return Report{}, fmt.Errorf("scenario %q: %w", scenario.Label, err)
		}
		r.Selection.SHA256 = engine.DatasetHash
		nextSlice := int64(0)
		priorHistory := 0
		for !engine.Terminated && !engine.Truncated {
			if err := ctx.Err(); err != nil {
				return Report{}, err
			}
			if len(engine.Decisions) >= MaxDecisions || totalHistory >= MaxHistory {
				return Report{}, errors.New("comparison history or decision limit exceeded")
			}
			action, next, err := x.BaselineAction(scenario.Policy, scenario.Plan, engine.Now, engine.Ledger.Filled, nextSlice)
			if err != nil {
				return Report{}, err
			}
			nextSlice = next
			if _, err := engine.Step(action); err != nil {
				return Report{}, err
			}
			history := len(engine.Decisions) + len(engine.Orders) + len(engine.Ledger.Fills)
			totalHistory += history - priorHistory
			priorHistory = history
			if totalHistory > MaxHistory {
				return Report{}, errors.New("combined decisions, orders and fills exceed 20000")
			}
		}
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		mid, err := deadlineMidpoint(events, scenario.Plan.Deadline)
		if err != nil {
			return Report{}, err
		}
		result := ScenarioResult{Label: scenario.Label, Policy: scenario.Policy, EstimateOnly: true, Simulation: engine.Report(), ComparisonValuation: Valuation{spec.ValuationReference, scenario.Plan.Deadline, mid}, ComparisonMarkedCostQuote: engine.Ledger.MarkedCost(engine.InitialMid, mid), NetBaseChange: engine.Ledger.Base.Sub(scenario.Plan.BaseBalance), NetQuoteChange: engine.Ledger.Quote.Sub(scenario.Plan.QuoteBudget), ComparabilityReasons: []string{}}
		result.Simulation["policy"] = scenario.Policy
		result.Comparable = engine.Terminated && !engine.Truncated
		if !result.Comparable {
			result.ComparabilityReasons = append(result.ComparabilityReasons, "scenario did not terminate with valid settlement")
		}
		if index > 0 {
			result.ComparabilityReasons = append(result.ComparabilityReasons, comparisonReasons(spec.Reference, scenario, r.Reference, result)...)
			result.Comparable = len(result.ComparabilityReasons) == 0
			if result.Comparable {
				delta := r.Reference.ComparisonMarkedCostQuote.Sub(result.ComparisonMarkedCostQuote)
				result.EstimatedCostReductionQuote = &delta
			}
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return Report{}, err
		}
		if len(encoded) > MaxScenarioBytes {
			return Report{}, errors.New("scenario report exceeds 4MiB")
		}
		if index == 0 {
			r.Reference = result
		} else {
			r.Alternatives = append(r.Alternatives, result)
		}
	}
	if _, err := Encode(r); err != nil {
		return Report{}, err
	}
	return r, nil
}

func comparisonReasons(reference, alternative Scenario, base, result ScenarioResult) []string {
	reasons := []string{}
	if !base.Comparable {
		reasons = append(reasons, "reference did not terminate with valid settlement")
	}
	p, q := reference.Plan, alternative.Plan
	if p.Symbol != q.Symbol || p.Side != q.Side || !p.Quantity.Equal(q.Quantity) || p.Deadline != q.Deadline || !p.BaseBalance.Equal(q.BaseBalance) || !p.QuoteBudget.Equal(q.QuoteBudget) {
		reasons = append(reasons, "symbol, side, target quantity, deadline or initial balances differ from reference")
	}
	if base.ComparisonValuation.Reference != result.ComparisonValuation.Reference || base.ComparisonValuation.TimeNS != result.ComparisonValuation.TimeNS || !base.ComparisonValuation.Midpoint.Equal(result.ComparisonValuation.Midpoint) {
		reasons = append(reasons, "frozen comparison valuation differs from reference")
	}
	return reasons
}

func Encode(r Report) ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if len(b)+1 > MaxReportBytes {
		return nil, errors.New("comparison report exceeds 16MiB")
	}
	return append(b, '\n'), nil
}

func deadlineMidpoint(events []d.MarketEvent, deadline int64) (d.Decimal, error) {
	book := x.NewBook()
	for _, event := range events {
		if event.Time > deadline {
			break
		}
		if err := book.Apply(event); err != nil {
			return d.Zero, err
		}
	}
	if !book.Valid {
		return d.Zero, errors.New("deadline valuation lacks valid source book")
	}
	return book.Mid(), nil
}

func selectEvents(ctx context.Context, source []marketdata.Event, end int64, symbol string) ([]d.MarketEvent, error) {
	var events []d.MarketEvent
	book := x.NewBook()
	totalLevels := 0
	for _, wire := range source {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if wire.Time > end {
			break
		}
		if len(events) >= MaxEvents {
			return nil, errors.New("selected tape exceeds 20000 events")
		}
		if !wire.Valid || wire.Kind == "gap" {
			return nil, errors.New("selected inclusive interval contains invalid market data")
		}
		if wire.Symbol != symbol || wire.Time < 0 || len(events) > 0 && wire.Time < events[len(events)-1].Time {
			return nil, errors.New("selected tape identity or time mismatch")
		}
		levels := len(wire.Bids) + len(wire.Asks)
		totalLevels += levels
		if levels > MaxBookLevels || totalLevels > MaxLevels {
			return nil, errors.New("selected tape level bounds exceeded")
		}
		event := d.MarketEvent{Time: wire.Time, Kind: wire.Kind, Symbol: wire.Symbol, ReceiptIndex: wire.ReceiptIndex, Side: wire.Side, TradeID: wire.TradeID}
		var err error
		switch wire.Kind {
		case "snapshot", "book":
			if event.Bids, err = parseLevels(wire.Bids); err == nil {
				event.Asks, err = parseLevels(wire.Asks)
			}
		case "trade":
			event.Price, err = d.ParseDecimal(wire.Price)
			if err == nil {
				event.Quantity, err = d.ParseDecimal(wire.Quantity)
			}
			if err == nil && (event.Price.Sign() <= 0 || event.Quantity.Sign() <= 0 || (wire.Side != "buy" && wire.Side != "sell")) {
				err = errors.New("invalid exact trade")
			}
		default:
			err = errors.New("unsupported selected market event")
		}
		if err != nil {
			return nil, err
		}
		if err := book.Apply(event); err != nil {
			return nil, err
		}
		if !book.Valid || len(book.Bids)+len(book.Asks) > MaxBookLevels {
			return nil, errors.New("selected book is invalid or exceeds 2048 accumulated levels")
		}
		events = append(events, event)
	}
	if len(events) == 0 || events[0].Time != 0 || events[0].Kind != "snapshot" {
		return nil, errors.New("replay requires first verified snapshot at normalized zero")
	}
	return events, nil
}

func parseLevels(source [][]string) ([]d.Level, error) {
	levels := make([]d.Level, 0, len(source))
	seen := map[string]bool{}
	for _, pair := range source {
		if len(pair) != 2 {
			return nil, errors.New("book level requires two exact decimal strings")
		}
		price, err := d.ParseDecimal(pair[0])
		if err != nil || price.Sign() <= 0 {
			return nil, errors.New("invalid positive level price")
		}
		quantity, err := d.ParseDecimal(pair[1])
		if err != nil || quantity.Sign() < 0 || seen[price.String()] {
			return nil, errors.New("invalid level quantity or duplicate price")
		}
		seen[price.String()] = true
		levels = append(levels, d.Level{Price: price, Quantity: quantity})
	}
	return levels, nil
}

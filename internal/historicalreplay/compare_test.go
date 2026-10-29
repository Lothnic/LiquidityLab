package historicalreplay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"github.com/liquiditylab/liquiditylab/internal/marketarchive"
	"github.com/liquiditylab/liquiditylab/internal/marketdata"
)

func testSpec() Spec {
	p := d.DefaultPlan()
	p.Quantity, p.MaxChildQuantity = d.MustDecimal(".01"), d.MustDecimal(".01")
	p.QuoteBudget, p.BaseBalance, p.Deadline = d.MustDecimal("20"), d.Zero, d.Second
	c := d.DefaultConfig()
	c.DecisionInterval, c.SubmissionLatency, c.CancelLatency = d.Second/10, 0, 0
	c.SettlementAllowance, c.MinRestingAge, c.MinMessageInterval = d.Second/5, 0, 0
	c.MaxParticipation = d.One
	return Spec{Schema: SpecSchema, Rules: d.DefaultRules(), Fees: d.DefaultFees(), Simulation: c, ValuationReference: "deadline-midpoint", Reference: Scenario{"scheduled", p, "scheduled"}, Alternatives: []Scenario{{"liquidity", p, "liquidity-aware"}}}
}

func testRecording() marketarchive.Recording {
	return marketarchive.Recording{Manifest: marketarchive.Manifest{Source: marketarchive.Source{Symbol: "BTC/USD"}, CoverageEndNS: 2 * d.Second, FirstSnapshotElapsedNS: d.Second, FirstSnapshotSourceReceiptIndex: 5}, Events: []marketdata.Event{
		{Kind: "snapshot", Time: 0, Valid: true, Symbol: "BTC/USD", SourceReceiptIndex: 5, Bids: [][]string{{"99", "2"}}, Asks: [][]string{{"101", "2"}}},
		{Kind: "trade", Time: d.Second / 20, Valid: true, Symbol: "BTC/USD", ReceiptIndex: 1, SourceReceiptIndex: 6, Price: "99", Quantity: "2.01", Side: "sell", TradeID: "maker-print"},
		{Kind: "snapshot", Time: d.Second, Valid: true, Symbol: "BTC/USD", ReceiptIndex: 2, SourceReceiptIndex: 7, Bids: [][]string{{"109", "2"}}, Asks: [][]string{{"111", "2"}}},
	}}
}

func runRecording(t *testing.T, r marketarchive.Recording, s Spec) Report {
	t.Helper()
	end, err := s.validate()
	if err != nil {
		t.Fatal(err)
	}
	result, err := compareRecording(context.Background(), r, s, end)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func exact(t *testing.T, value any, expected string) {
	t.Helper()
	v, ok := value.(d.Decimal)
	if !ok || !v.Equal(d.MustDecimal(expected)) {
		t.Fatalf("exact value: got %v, want %s", value, expected)
	}
}

func TestCompareExactCashflowsAndCommonDeadlineValuation(t *testing.T) {
	s := testSpec()
	r := runRecording(t, testRecording(), s)
	if !r.EstimateOnly || r.SourceSessionBinding != nil || r.Mode != "historical-simulation-comparison" || r.Selection.Events != 3 || r.Selection.LastEventSourceReceiptIndex != 7 || len(r.Selection.SHA256) != 64 || len(r.ResolvedSpecSHA256) != 64 {
		t.Fatal("comparison lost provenance or estimate scope")
	}
	ref, alt := r.Reference, r.Alternatives[0]
	if !ref.Comparable || !alt.Comparable || alt.EstimatedCostReductionQuote == nil {
		t.Fatalf("valid comparable estimates rejected: %+v", alt)
	}
	exact(t, ref.Simulation["base_balance"], ".01")
	exact(t, ref.Simulation["quote_balance"], "18.98798")
	exact(t, ref.Simulation["fees_quote"], ".00202")
	exact(t, ref.ComparisonMarkedCostQuote, ".01202")
	exact(t, alt.Simulation["base_balance"], ".01")
	exact(t, alt.Simulation["quote_balance"], "19.00901")
	exact(t, alt.Simulation["fees_quote"], ".00099")
	exact(t, alt.ComparisonMarkedCostQuote, "-.00901")
	exact(t, *alt.EstimatedCostReductionQuote, ".02103")
	exact(t, alt.NetBaseChange, ".01")
	exact(t, alt.NetQuoteChange, "-.99099")
	exact(t, alt.ComparisonValuation.Midpoint, "110")
	exact(t, alt.Simulation["residual_midpoint"], "100")
	if alt.ComparisonValuation.TimeNS != d.Second {
		t.Fatal("deadline valuation used execution time")
	}
	one, err := Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Encode(runRecording(t, testRecording(), s))
	if err != nil || !bytes.Equal(one, two) {
		t.Fatal("identical input comparison is not deterministic")
	}
	if !bytes.Contains(one, []byte(`"source_session_binding":null`)) {
		t.Fatal("simulation inferred an observed session binding")
	}
}

func TestCompareQuietTailCoverageAndInclusiveTies(t *testing.T) {
	s := testSpec()
	r := testRecording()
	r.Events = r.Events[:1]
	r.Manifest.CoverageEndNS = s.Reference.Plan.Deadline + s.Simulation.SettlementAllowance
	result := runRecording(t, r, s)
	if result.Selection.Events != 1 || result.Reference.Simulation["dataset_events"] != 1 {
		t.Fatal("quiet tail fabricated an event")
	}
	r.Manifest.CoverageEndNS--
	if _, err := compareRecording(context.Background(), r, s, result.Selection.RequiredEndNS); err == nil {
		t.Fatal("short sealed coverage accepted")
	}
	r = testRecording()
	end := s.Reference.Plan.Deadline + s.Simulation.SettlementAllowance
	r.Events = append(r.Events, marketdata.Event{Kind: "book", Time: end, Valid: true, Symbol: "BTC/USD", ReceiptIndex: 3, SourceReceiptIndex: 8, Bids: [][]string{{"109", "3"}}}, marketdata.Event{Kind: "book", Time: end, Valid: true, Symbol: "BTC/USD", ReceiptIndex: 4, SourceReceiptIndex: 9, Asks: [][]string{{"111", "3"}}}, marketdata.Event{Kind: "gap", Time: end + 1, Valid: false, SourceReceiptIndex: 10})
	result = runRecording(t, r, s)
	if result.Selection.Events != 5 || result.Selection.LastEventSourceReceiptIndex != 9 {
		t.Fatal("inclusive end ties or source identity lost")
	}
}

func TestCompareGapEligibilityAndInvalidSettlement(t *testing.T) {
	s := testSpec()
	end, _ := s.validate()
	for name, change := range map[string]func(*marketarchive.Recording){
		"manifest gap at origin": func(r *marketarchive.Recording) {
			r.Manifest.Gaps = []marketarchive.Gap{{ElapsedNS: r.Manifest.FirstSnapshotElapsedNS}}
		},
		"manifest gap at endpoint": func(r *marketarchive.Recording) {
			r.Manifest.Gaps = []marketarchive.Gap{{ElapsedNS: r.Manifest.FirstSnapshotElapsedNS + end}}
		},
		"invalid event":  func(r *marketarchive.Recording) { r.Events[1].Valid = false },
		"crossed book":   func(r *marketarchive.Recording) { r.Events[0].Bids[0][0] = "102" },
		"another symbol": func(r *marketarchive.Recording) { r.Events[1].Symbol = "ETH/USD" },
	} {
		t.Run(name, func(t *testing.T) {
			r := testRecording()
			change(&r)
			if _, err := compareRecording(context.Background(), r, s, end); err == nil {
				t.Fatal("invalid selected interval accepted")
			}
		})
	}
	r := testRecording()
	r.Manifest.Gaps = []marketarchive.Gap{{ElapsedNS: r.Manifest.FirstSnapshotElapsedNS - 1}, {ElapsedNS: r.Manifest.FirstSnapshotElapsedNS + end + 1}}
	if got := runRecording(t, r, s); got.Selection.OmittedPreOriginGaps != 1 {
		t.Fatal("recovered pre-origin gap lost its explicit count")
	}
	r = testRecording()
	r.Events = r.Events[:1]
	s.Simulation.MinRestingAge = 2 * d.Second
	s.Simulation.CancelLatency = d.Second / 2
	s.Simulation.SettlementAllowance = d.Second / 10
	got := runRecording(t, r, s)
	alt := got.Alternatives[0]
	if alt.Comparable || alt.EstimatedCostReductionQuote != nil || alt.Simulation["economic_score_valid"] != false || len(alt.ComparabilityReasons) == 0 {
		t.Fatal("unresolved settlement emitted comparable cost delta")
	}
}

func TestComparisonRejectsDifferentObjectives(t *testing.T) {
	for name, change := range map[string]func(*d.Plan){
		"side":     func(p *d.Plan) { p.Side = "sell"; p.LimitPrice = d.MustDecimal("98"); p.BaseBalance = d.One },
		"target":   func(p *d.Plan) { p.Quantity = d.MustDecimal(".02") },
		"deadline": func(p *d.Plan) { p.Deadline = d.Second / 2 },
		"quote":    func(p *d.Plan) { p.QuoteBudget = d.MustDecimal("21") },
		"base":     func(p *d.Plan) { p.BaseBalance = d.One },
	} {
		t.Run(name, func(t *testing.T) {
			s := testSpec()
			change(&s.Alternatives[0].Plan)
			r := runRecording(t, testRecording(), s)
			a := r.Alternatives[0]
			if a.Comparable || a.EstimatedCostReductionQuote != nil || len(a.ComparabilityReasons) == 0 {
				t.Fatal("different objectives compared as estimated savings")
			}
		})
	}
}

func TestStrictExplicitSpecification(t *testing.T) {
	b, err := json.Marshal(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSpec(b); err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]string{{"schema"}, {"valuation_reference"}, {"fees", "source"}, {"fees", "maker"}, {"simulation", "settlement_allowance_ns"}, {"simulation", "fill_model"}, {"reference", "plan", "base_balance"}, {"reference", "policy"}, {"rules", "min_notional"}} {
		for _, null := range []bool{false, true} {
			t.Run(strings.Join(path, "/")+fmt.Sprint(null), func(t *testing.T) {
				var m map[string]any
				_ = json.Unmarshal(b, &m)
				node := m
				for _, key := range path[:len(path)-1] {
					node = node[key].(map[string]any)
				}
				if null {
					node[path[len(path)-1]] = nil
				} else {
					delete(node, path[len(path)-1])
				}
				bad, _ := json.Marshal(m)
				if _, err := ParseSpec(bad); err == nil {
					t.Fatal("missing explicit assumption accepted")
				}
			})
		}
	}
	for name, change := range map[string]func(*Spec){
		"unknown schema":      func(s *Spec) { s.Schema = "future" },
		"unknown model":       func(s *Spec) { s.Simulation.FillModel = "optimistic" },
		"unbounded decisions": func(s *Spec) { s.Simulation.DecisionInterval = 1 },
		"overflow": func(s *Spec) {
			s.Reference.Plan.Deadline = math.MaxInt64
			s.Simulation.DecisionInterval = math.MaxInt64
		},
		"empty alternatives":   func(s *Spec) { s.Alternatives = nil },
		"duplicate label":      func(s *Spec) { s.Alternatives[0].Label = s.Reference.Label },
		"unknown policy":       func(s *Spec) { s.Reference.Policy = "best-ai" },
		"empty fee provenance": func(s *Spec) { s.Fees.Source = "" },
	} {
		t.Run(name, func(t *testing.T) {
			s := testSpec()
			change(&s)
			b, _ := json.Marshal(s)
			if _, err := ParseSpec(b); err == nil {
				t.Fatal("invalid spec accepted")
			}
		})
	}
	for _, bad := range [][]byte{nil, []byte("null"), append(append([]byte(nil), b...), []byte("{}")...), []byte(strings.Replace(string(b), `"schema":`, `"unknown":0,"schema":`, 1)), bytes.Repeat([]byte(" "), MaxSpecBytes+1)} {
		if _, err := ParseSpec(bad); err == nil {
			t.Fatal("invalid JSON specification accepted")
		}
	}
}

type cancelAfter struct {
	context.Context
	remaining int
}

func (c *cancelAfter) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestSelectionBoundsAndCancellation(t *testing.T) {
	s := testSpec()
	end, _ := s.validate()
	for name, change := range map[string]func(*marketarchive.Recording){
		"event cap": func(r *marketarchive.Recording) {
			first := r.Events[0]
			r.Events = make([]marketdata.Event, MaxEvents+1)
			for i := range r.Events {
				r.Events[i] = first
			}
		},
		"level cap":       func(r *marketarchive.Recording) { r.Events[0].Bids = make([][]string, MaxBookLevels+1) },
		"duplicate price": func(r *marketarchive.Recording) { r.Events[0].Bids = append(r.Events[0].Bids, []string{"99.0", "1"}) },
		"malformed level": func(r *marketarchive.Recording) { r.Events[0].Bids[0] = []string{"99"} },
		"backward time":   func(r *marketarchive.Recording) { r.Events[2].Time = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			r := testRecording()
			change(&r)
			if _, err := compareRecording(context.Background(), r, s, end); err == nil {
				t.Fatal("unbounded or malformed selected tape accepted")
			}
		})
	}
	s.Reference.Plan.Deadline = 1000
	s.Alternatives[0].Plan.Deadline = 1000
	s.Simulation.DecisionInterval = 1
	s.Simulation.SettlementAllowance = 1
	s.Reference.Plan.LimitPrice = d.One
	s.Alternatives[0].Plan.LimitPrice = d.One
	end, _ = s.validate()
	r := testRecording()
	r.Events = r.Events[:1]
	ctx := &cancelAfter{Context: context.Background(), remaining: 20}
	if _, err := compareRecording(ctx, r, s, end); !errors.Is(err, context.Canceled) {
		t.Fatalf("execution did not honor cancellation: %v", err)
	}
}

func TestVerifiedArchivePublicInterface(t *testing.T) {
	root := t.TempDir()
	const fixture = "../../examples/recordings/kraken-btcusd-20261028/"
	m, err := marketarchive.Import(context.Background(), marketarchive.Config{RawPath: fixture + "public-capture.jsonl", ManifestPath: fixture + "public-capture.manifest.json", ArchiveRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	s := testSpec()
	s.Reference.Plan.LimitPrice = d.MustDecimal("90000")
	s.Alternatives[0].Plan.LimitPrice = s.Reference.Plan.LimitPrice
	s.Reference.Plan.QuoteBudget = d.MustDecimal("2000")
	s.Alternatives[0].Plan.QuoteBudget = s.Reference.Plan.QuoteBudget
	r, err := Compare(context.Background(), root, m.ID, s)
	if err != nil {
		t.Fatal(err)
	}
	if r.Archive.ID != m.ID || r.Archive.Normalized != m.Normalized || !r.EstimateOnly || r.Selection.RequiredEndNS > m.CoverageEndNS {
		t.Fatal("verified archive provenance lost")
	}
	p := filepath.Join(root, m.ID, "events.jsonl")
	if err := os.WriteFile(p, []byte("tampered\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Compare(context.Background(), root, m.ID, s); err == nil {
		t.Fatal("comparison trusted a tampered archive")
	}
}

func TestReadSpecRejectsSymlinkAndOversizedFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "spec.json")
	b, _ := json.Marshal(testSpec())
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSpec(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSpec(context.Background(), link); err == nil {
		t.Fatal("followed final spec symlink")
	}
	if err := os.WriteFile(p, bytes.Repeat([]byte(" "), MaxSpecBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSpec(context.Background(), p); err == nil {
		t.Fatal("oversized spec accepted")
	}
}

func TestAccumulatedBookAndTotalLevelBounds(t *testing.T) {
	levels := func(start, count int) [][]string {
		out := make([][]string, count)
		for i := range out {
			out[i] = []string{fmt.Sprint(start + i), "1"}
		}
		return out
	}
	first := marketdata.Event{Kind: "snapshot", Time: 0, Valid: true, Symbol: "BTC/USD", Bids: levels(1, 1100), Asks: [][]string{{"10000", "1"}}}
	second := marketdata.Event{Kind: "book", Time: 1, Valid: true, Symbol: "BTC/USD", Bids: levels(1101, 1000)}
	if _, err := selectEvents(context.Background(), []marketdata.Event{first, second}, 2, "BTC/USD"); err == nil || !strings.Contains(err.Error(), "2048 accumulated") {
		t.Fatalf("accumulated book cap not enforced: %v", err)
	}
	first.Bids, first.Asks = levels(1, 1024), levels(2000, 1024)
	events := make([]marketdata.Event, MaxLevels/MaxBookLevels+1)
	for i := range events {
		events[i] = first
		if i > 0 {
			events[i].Kind = "book"
		}
		events[i].Time = int64(i)
	}
	if _, err := selectEvents(context.Background(), events, int64(len(events)), "BTC/USD"); err == nil || !strings.Contains(err.Error(), "level bounds") {
		t.Fatalf("total level cap not enforced: %v", err)
	}
}

func TestComparisonHistoryIsBoundedAcrossScenarios(t *testing.T) {
	s := testSpec()
	s.Simulation.DecisionInterval, s.Simulation.SettlementAllowance = 1, 0
	s.Reference.Plan.Deadline = 3000
	s.Reference.Plan.LimitPrice = d.MustDecimal(".01")
	s.Alternatives = make([]Scenario, 7)
	for i := range s.Alternatives {
		s.Alternatives[i] = s.Reference
		s.Alternatives[i].Label = fmt.Sprintf("alternative-%d", i)
	}
	r := testRecording()
	r.Events = r.Events[:1]
	end, err := s.validate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compareRecording(context.Background(), r, s, end); err == nil || !strings.Contains(err.Error(), "history") {
		t.Fatalf("combined history cap not enforced: %v", err)
	}
}

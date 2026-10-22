package replay

import (
	"encoding/json"
	"math"
	"os"
	"strconv"
	"testing"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

type observationGoldenCase struct {
	Name  string `json:"name"`
	Input struct {
		Events []struct {
			Time     int64      `json:"time_ns"`
			Kind     string     `json:"kind"`
			Bids     [][]string `json:"bids"`
			Asks     [][]string `json:"asks"`
			Price    string     `json:"price"`
			Quantity string     `json:"quantity"`
			Side     string     `json:"side"`
			TradeID  string     `json:"trade_id"`
			Valid    *bool      `json:"valid"`
		} `json:"events"`
		Plan   map[string]json.RawMessage `json:"plan"`
		Config map[string]json.RawMessage `json:"config"`
		Fees   d.FeeConfig                `json:"fees"`
	} `json:"input"`
	ExpectedObservations struct {
		SchemaVersion string    `json:"schema_version"`
		FieldNames    []string  `json:"field_names"`
		Reset         []float64 `json:"reset"`
		Decisions     []struct {
			Action      int       `json:"action"`
			Observation []float64 `json:"observation"`
		} `json:"decisions"`
	} `json:"expected_observations"`
}

type observationGolden struct {
	Schema        string                  `json:"schema"`
	ActionMapping map[string]string       `json:"action_mapping"`
	Cases         []observationGoldenCase `json:"cases"`
}

func observationNumber(raw json.RawMessage, fallback float64) (float64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return fallback, nil
	}
	var value float64
	err := json.Unmarshal(raw, &value)
	return value, err
}

func observationDecimal(raw json.RawMessage, fallback d.Decimal) (d.Decimal, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return fallback, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return d.Decimal{}, err
	}
	return d.ParseDecimal(value)
}

func observationString(raw json.RawMessage, fallback string) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return fallback, nil
	}
	var value string
	err := json.Unmarshal(raw, &value)
	return value, err
}

func buildObservationCase(g observationGoldenCase) (*Engine, error) {
	p := d.DefaultPlan()
	var err error
	if p.Side, err = observationString(g.Input.Plan["side"], p.Side); err != nil {
		return nil, err
	}
	if p.Quantity, err = observationDecimal(g.Input.Plan["quantity"], p.Quantity); err != nil {
		return nil, err
	}
	if p.LimitPrice, err = observationDecimal(g.Input.Plan["limit_price"], p.LimitPrice); err != nil {
		return nil, err
	}
	if p.MaxChildQuantity, err = observationDecimal(g.Input.Plan["max_child_quantity"], p.MaxChildQuantity); err != nil {
		return nil, err
	}
	if p.QuoteBudget, err = observationDecimal(g.Input.Plan["quote_budget"], p.QuoteBudget); err != nil {
		return nil, err
	}
	if p.BaseBalance, err = observationDecimal(g.Input.Plan["base_balance"], p.BaseBalance); err != nil {
		return nil, err
	}
	if p.Symbol, err = observationString(g.Input.Plan["symbol"], p.Symbol); err != nil {
		return nil, err
	}
	seconds, err := observationNumber(g.Input.Plan["deadline"], float64(p.Deadline)/float64(d.Second))
	if err != nil {
		return nil, err
	}
	p.Deadline = int64(seconds * float64(d.Second))

	c := d.DefaultConfig()
	for key, target := range map[string]*int64{
		"decision_interval":    &c.DecisionInterval,
		"submission_latency":   &c.SubmissionLatency,
		"cancel_latency":       &c.CancelLatency,
		"settlement_allowance": &c.SettlementAllowance,
		"min_resting_age":      &c.MinRestingAge,
		"min_message_interval": &c.MinMessageInterval,
	} {
		value, err := observationNumber(g.Input.Config[key], float64(*target)/float64(d.Second))
		if err != nil {
			return nil, err
		}
		*target = int64(value * float64(d.Second))
	}
	if c.LambdaMiss, err = observationNumber(g.Input.Config["lambda_miss"], c.LambdaMiss); err != nil {
		return nil, err
	}
	if c.MaxParticipation, err = observationDecimal(g.Input.Config["max_participation"], c.MaxParticipation); err != nil {
		return nil, err
	}
	if c.FillModel, err = observationString(g.Input.Config["fill_model"], c.FillModel); err != nil {
		return nil, err
	}
	events := make([]d.MarketEvent, 0, len(g.Input.Events))
	for index, raw := range g.Input.Events {
		event := d.MarketEvent{
			Time: raw.Time, Kind: raw.Kind, ReceiptIndex: uint64(index), Side: raw.Side,
			TradeID: raw.TradeID, Price: d.Zero, Quantity: d.Zero,
			Invalid: raw.Valid != nil && !*raw.Valid,
		}
		if raw.Price != "" {
			event.Price, err = d.ParseDecimal(raw.Price)
			if err != nil {
				return nil, err
			}
		}
		if raw.Quantity != "" {
			event.Quantity, err = d.ParseDecimal(raw.Quantity)
			if err != nil {
				return nil, err
			}
		}
		for _, level := range raw.Bids {
			event.Bids = append(event.Bids, d.Level{Price: d.MustDecimal(level[0]), Quantity: d.MustDecimal(level[1])})
		}
		for _, level := range raw.Asks {
			event.Asks = append(event.Asks, d.Level{Price: d.MustDecimal(level[0]), Quantity: d.MustDecimal(level[1])})
		}
		events = append(events, event)
	}
	return New(events, p, d.DefaultRules(), g.Input.Fees, c)
}

func orderedFloat32(value float32) uint32 {
	bits := math.Float32bits(value)
	if bits&0x80000000 != 0 {
		return ^bits
	}
	return bits | 0x80000000
}

func compareObservation(t *testing.T, label string, got []float32, expected []float64) {
	t.Helper()
	if len(got) != ObservationSize || len(expected) != ObservationSize {
		t.Fatalf("%s observation length got=%d want=%d", label, len(got), len(expected))
	}
	for index, want64 := range expected {
		got32 := got[index]
		want32 := float32(want64)
		if math.IsNaN(float64(got32)) || math.IsInf(float64(got32), 0) {
			t.Fatalf("%s feature %s is not finite: %v", label, ObservationFields[index], got32)
		}
		gotBits, wantBits := orderedFloat32(got32), orderedFloat32(want32)
		var distance uint32
		if gotBits > wantBits {
			distance = gotBits - wantBits
		} else {
			distance = wantBits - gotBits
		}
		if distance > 2 {
			t.Errorf("%s feature %s got %.9g want %.9g (%d float32 ULPs)",
				label, ObservationFields[index], got32, want32, distance)
		}
		if index >= 16 {
			if want32 != 0 && want32 != 1 {
				t.Fatalf("%s fixture feature %s is not a boolean: %v", label, ObservationFields[index], want32)
			}
			if got32 != want32 {
				t.Errorf("%s boolean feature %s got %v want %v", label, ObservationFields[index], got32, want32)
			}
		}
	}
}

func TestPythonGoldenObservations(t *testing.T) {
	data, err := os.ReadFile("../../conformance/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden observationGolden
	if err = json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	if golden.Schema != "liquiditylab-python-reference-v1" {
		t.Fatalf("unexpected golden schema %q", golden.Schema)
	}
	wantActions := []string{"HOLD", "CANCEL", "PASSIVE_SMALL", "PASSIVE_NORMAL", "AGGRESSIVE_SMALL", "AGGRESSIVE_NORMAL"}
	if len(golden.ActionMapping) != len(wantActions) {
		t.Fatalf("action mapping has %d entries", len(golden.ActionMapping))
	}
	for legacy, semantic := range wantActions {
		if got := golden.ActionMapping[strconv.Itoa(legacy)]; got != semantic {
			t.Fatalf("legacy action %d maps to %q, want %q", legacy, got, semantic)
		}
	}
	for _, g := range golden.Cases {
		t.Run(g.Name, func(t *testing.T) {
			vectors := g.ExpectedObservations
			if vectors.SchemaVersion != ObservationVersion {
				t.Fatalf("observation schema %q, want %q", vectors.SchemaVersion, ObservationVersion)
			}
			if len(vectors.FieldNames) != len(ObservationFields) {
				t.Fatalf("observation field count %d, want %d", len(vectors.FieldNames), len(ObservationFields))
			}
			for index, field := range ObservationFields {
				if vectors.FieldNames[index] != field {
					t.Fatalf("observation field %d is %q, want %q", index, vectors.FieldNames[index], field)
				}
			}
			engine, err := buildObservationCase(g)
			if err != nil {
				t.Fatal(err)
			}
			reset, err := engine.Reset()
			if err != nil {
				t.Fatal(err)
			}
			compareObservation(t, "reset", reset.Observation, vectors.Reset)
			if len(engine.Decisions) != 0 {
				t.Fatal("reset unexpectedly made decisions")
			}
			for index, expected := range vectors.Decisions {
				action, err := d.ActionFromLegacy(expected.Action)
				if err != nil {
					t.Fatalf("decision %d action: %v", index, err)
				}
				result, err := engine.Step(action)
				if err != nil {
					t.Fatalf("decision %d step: %v", index, err)
				}
				if len(engine.Decisions) != index+1 || int(engine.Decisions[index].Action)-1 != expected.Action {
					t.Fatalf("decision %d action mapping mismatch: got %v want %d", index, engine.Decisions, expected.Action)
				}
				compareObservation(t, "decision "+strconv.Itoa(index), result.Observation, expected.Observation)
			}
			if len(engine.Decisions) != len(vectors.Decisions) {
				t.Fatalf("decision count %d, want %d", len(engine.Decisions), len(vectors.Decisions))
			}
		})
	}
}

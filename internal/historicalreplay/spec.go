// Package historicalreplay compares bounded simulated scenarios on one verified
// public recording. It does not establish live session bindings or savings.
package historicalreplay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
	x "github.com/liquiditylab/liquiditylab/internal/execution"
)

const (
	SpecSchema       = "liquiditylab.historical-compare.spec.v1"
	ReportSchema     = "liquiditylab.historical-compare.report.v1"
	MaxSpecBytes     = 64 << 10
	MaxEvents        = 20000
	MaxLevels        = 1000000
	MaxBookLevels    = 2048
	MaxDecisions     = 10000
	MaxHistory       = 20000
	MaxScenarioBytes = 4 << 20
	MaxReportBytes   = 16 << 20
)

type Scenario struct {
	Label  string `json:"label"`
	Plan   d.Plan `json:"plan"`
	Policy string `json:"policy"`
}

type Spec struct {
	Schema             string            `json:"schema"`
	Rules              d.InstrumentRules `json:"rules"`
	Fees               d.FeeConfig       `json:"fees"`
	Simulation         d.ReplayConfig    `json:"simulation"`
	ValuationReference string            `json:"valuation_reference"`
	Reference          Scenario          `json:"reference"`
	Alternatives       []Scenario        `json:"alternatives"`
}

func ParseSpec(b []byte) (Spec, error) {
	var spec Spec
	if len(b) == 0 || len(b) > MaxSpecBytes {
		return spec, errors.New("spec must contain 1..64KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return spec, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return spec, errors.New("spec requires exactly one JSON object")
	}
	fields, err := required(b, "schema", "rules", "fees", "simulation", "valuation_reference", "reference", "alternatives")
	if err != nil {
		return spec, err
	}
	for _, item := range []struct {
		key    string
		fields []string
	}{
		{"rules", []string{"symbol", "tick_size", "lot_size", "min_quantity", "min_notional"}},
		{"fees", []string{"maker", "taker", "currency", "source"}},
		{"simulation", []string{"decision_interval_ns", "submission_latency_ns", "cancel_latency_ns", "settlement_allowance_ns", "lambda_miss", "max_participation", "min_resting_age_ns", "min_message_interval_ns", "fill_model"}},
	} {
		if _, err := required(fields[item.key], item.fields...); err != nil {
			return spec, fmt.Errorf("%s: %w", item.key, err)
		}
	}
	var alternatives []json.RawMessage
	if err := json.Unmarshal(fields["alternatives"], &alternatives); err != nil {
		return spec, err
	}
	for _, raw := range append([]json.RawMessage{fields["reference"]}, alternatives...) {
		item, err := required(raw, "label", "plan", "policy")
		if err != nil {
			return spec, err
		}
		if _, err := required(item["plan"], "side", "quantity", "limit_price", "deadline_ns", "max_child_quantity", "quote_budget", "base_balance", "symbol"); err != nil {
			return spec, fmt.Errorf("plan: %w", err)
		}
	}
	_, err = spec.validate()
	return spec, err
}

func required(b []byte, keys ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return nil, err
	}
	for _, key := range keys {
		v, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return nil, fmt.Errorf("explicit %s is required", key)
		}
	}
	return fields, nil
}

func (s Spec) scenarios() []Scenario { return append([]Scenario{s.Reference}, s.Alternatives...) }

func (s Spec) validate() (int64, error) {
	if s.Schema != SpecSchema || len(s.Alternatives) < 1 || len(s.Alternatives) > 7 {
		return 0, errors.New("spec requires its supported schema, a reference and 1..7 alternatives")
	}
	for _, err := range []error{s.Rules.Validate(), s.Fees.Validate(), s.Simulation.Validate()} {
		if err != nil {
			return 0, err
		}
	}
	if strings.TrimSpace(s.Fees.Source) == "" || len(s.Fees.Source) > 256 {
		return 0, errors.New("explicit bounded fee source is required")
	}
	if s.Simulation.FillModel != "conservative-v0" || s.ValuationReference != "deadline-midpoint" {
		return 0, errors.New("supported assumptions are conservative-v0 and explicit deadline-midpoint valuation")
	}
	labels := map[string]bool{}
	end := int64(0)
	for _, scenario := range s.scenarios() {
		if len(scenario.Label) < 1 || len(scenario.Label) > 64 || strings.TrimSpace(scenario.Label) != scenario.Label || strings.ContainsAny(scenario.Label, "\r\n\x00") || labels[scenario.Label] {
			return 0, errors.New("scenario labels must be unique bounded text")
		}
		labels[scenario.Label] = true
		if err := scenario.Plan.Validate(); err != nil {
			return 0, err
		}
		if err := x.ValidateBaselinePolicy(scenario.Policy); err != nil {
			return 0, err
		}
		if scenario.Plan.Symbol != s.Rules.Symbol {
			return 0, errors.New("scenario and instrument symbols differ")
		}
		deadline := scenario.Plan.Deadline
		decisions := (deadline-1)/s.Simulation.DecisionInterval + 1
		if decisions > MaxDecisions {
			return 0, errors.New("scenario exceeds 10000 possible decisions")
		}
		if s.Simulation.SettlementAllowance > math.MaxInt64-deadline {
			return 0, errors.New("deadline plus settlement overflows")
		}
		coverage := deadline + s.Simulation.SettlementAllowance
		if s.Simulation.SubmissionLatency > math.MaxInt64-coverage || s.Simulation.CancelLatency > math.MaxInt64-coverage {
			return 0, errors.New("scenario timer addition overflows")
		}
		end = max(end, coverage)
	}
	return end, nil
}

func ReadSpec(ctx context.Context, path string) (Spec, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Spec{}, err
	}
	f, err := openSpec(path)
	if err != nil {
		return Spec{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Spec{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxSpecBytes {
		return Spec{}, errors.New("spec must be a regular file of at most 64KiB")
	}
	var b bytes.Buffer
	buffer := make([]byte, 4096)
	for {
		if err := ctx.Err(); err != nil {
			return Spec{}, err
		}
		n, err := f.Read(buffer)
		b.Write(buffer[:n])
		if b.Len() > MaxSpecBytes {
			return Spec{}, errors.New("spec exceeds 64KiB")
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return Spec{}, err
		}
	}
	return ParseSpec(b.Bytes())
}

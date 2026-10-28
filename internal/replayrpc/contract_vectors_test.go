package replayrpc

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	pb "github.com/liquiditylab/liquiditylab/gen/go/liquiditylab/v1"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	"github.com/liquiditylab/liquiditylab/internal/replay"
	"google.golang.org/grpc/codes"
)

type contractDecimalVector struct {
	Name string
	Wire *struct {
		Coefficient string
		Scale       uint32
	}
	Required *bool
	Valid    bool
	Text     string
}

type contractVectors struct {
	SchemaVersion string `json:"schema_version"`
	EngineVersion string `json:"engine_version"`
	Observation   struct {
		Version    string
		FieldNames []string `json:"field_names"`
	}
	InputDecimals  []contractDecimalVector `json:"input_decimals"`
	OutputDecimals []contractDecimalVector `json:"output_decimals"`
	Times          []struct {
		Name        string
		Seconds     string
		Valid       bool
		Nanoseconds int64
	}
	LegacyFloatTimes []struct {
		Name        string
		Value       float64
		Nanoseconds int64
	} `json:"legacy_float_times"`
	Actions []struct {
		Legacy int
		Wire   int32
		Name   string
	}
	InvalidLegacyActions []int `json:"invalid_legacy_actions"`
	Enums                map[string]struct {
		Values       map[string]int32
		RejectedWire []int32 `json:"rejected_wire"`
	}
}

func readContractVectors(t *testing.T) contractVectors {
	t.Helper()
	data, err := os.ReadFile("../../conformance/contracts.json")
	if err != nil {
		t.Fatal(err)
	}
	var families map[string]json.RawMessage
	if err := json.Unmarshal(data, &families); err != nil {
		t.Fatal(err)
	}
	required := []string{"schema_version", "engine_version", "observation", "input_decimals", "output_decimals", "times", "legacy_float_times", "actions", "invalid_legacy_actions", "enums"}
	if len(families) != len(required) {
		t.Fatal("missing or unknown contract fixture family")
	}
	for _, family := range required {
		if _, exists := families[family]; !exists {
			t.Fatalf("missing contract fixture family %q", family)
		}
	}
	var vectors contractVectors
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if vectors.SchemaVersion != "phase1-contracts-v1" {
		t.Fatalf("unsupported fixture schema %q", vectors.SchemaVersion)
	}
	if vectors.EngineVersion == "" || vectors.Observation.Version == "" {
		t.Fatal("missing environment or observation version")
	}
	namedFamilies := map[string][]string{
		"input_decimals": nil, "output_decimals": nil, "times": nil,
		"legacy_float_times": nil, "actions": nil,
		"observation_fields": vectors.Observation.FieldNames,
	}
	for _, vector := range vectors.InputDecimals {
		namedFamilies["input_decimals"] = append(namedFamilies["input_decimals"], vector.Name)
	}
	for _, vector := range vectors.OutputDecimals {
		namedFamilies["output_decimals"] = append(namedFamilies["output_decimals"], vector.Name)
	}
	for _, vector := range vectors.Times {
		namedFamilies["times"] = append(namedFamilies["times"], vector.Name)
	}
	for _, vector := range vectors.LegacyFloatTimes {
		namedFamilies["legacy_float_times"] = append(namedFamilies["legacy_float_times"], vector.Name)
	}
	legacyActions := make(map[int]bool)
	for _, vector := range vectors.Actions {
		namedFamilies["actions"] = append(namedFamilies["actions"], vector.Name)
		if legacyActions[vector.Legacy] || vector.Legacy < 0 || vector.Legacy > 5 {
			t.Fatal("duplicate or invalid legacy action vector")
		}
		legacyActions[vector.Legacy] = true
	}
	if len(legacyActions) != 6 || len(vectors.InvalidLegacyActions) == 0 {
		t.Fatal("missing action vectors")
	}
	invalidLegacy := make(map[int]bool)
	for _, legacy := range vectors.InvalidLegacyActions {
		if invalidLegacy[legacy] {
			t.Fatal("duplicate invalid legacy action vector")
		}
		invalidLegacy[legacy] = true
	}
	for family, names := range namedFamilies {
		if len(names) == 0 {
			t.Fatalf("empty contract fixture family %q", family)
		}
		seen := make(map[string]bool)
		for _, name := range names {
			if name == "" || seen[name] {
				t.Fatalf("missing or duplicate name in %s: %q", family, name)
			}
			seen[name] = true
		}
	}
	if len(vectors.Enums) != 4 {
		t.Fatal("missing or unknown enum family")
	}
	for _, name := range []string{"Action", "Side", "EventKind", "FeeCurrency"} {
		enum, exists := vectors.Enums[name]
		if !exists || len(enum.Values) == 0 || len(enum.RejectedWire) == 0 {
			t.Fatalf("missing or empty enum family %q", name)
		}
		values := make(map[int32]bool)
		for _, value := range enum.Values {
			if values[value] {
				t.Fatalf("duplicate enum number in %s", name)
			}
			values[value] = true
		}
		rejected := make(map[int32]bool)
		for _, value := range enum.RejectedWire {
			if rejected[value] {
				t.Fatalf("duplicate rejected enum vector in %s", name)
			}
			rejected[value] = true
		}
	}
	return vectors
}

func TestContractTimeFinePrecisionRegression(t *testing.T) {
	got, err := d.SecondsToNanos("0.00000000150000000000000000000000000000000000000000000000001")
	if err != nil || got != 2 {
		t.Fatalf("fine precision time: got %d, %v; want 2", got, err)
	}
}

func TestContractVectors(t *testing.T) {
	vectors := readContractVectors(t)
	t.Run("input_decimals", func(t *testing.T) {
		for _, vector := range vectors.InputDecimals {
			t.Run(vector.Name, func(t *testing.T) {
				var wire *pb.Decimal
				if vector.Wire != nil {
					wire = &pb.Decimal{Coefficient: vector.Wire.Coefficient, Scale: vector.Wire.Scale}
				}
				required := vector.Required == nil || *vector.Required
				value, err := decimal(wire, required)
				if (err == nil) != vector.Valid {
					t.Fatalf("valid=%v: %v", vector.Valid, err)
				}
				if vector.Valid && value.String() != vector.Text {
					t.Fatalf("got %q, want %q", value.String(), vector.Text)
				}
			})
		}
	})
	t.Run("output_decimals", func(t *testing.T) {
		for _, vector := range vectors.OutputDecimals {
			// The Go boundary emits derived values; Python owns output decoding.
			if !vector.Valid {
				continue
			}
			t.Run(vector.Name, func(t *testing.T) {
				value, err := d.ParseInternalDecimal(vector.Text)
				if err != nil {
					t.Fatal(err)
				}
				wire := wireDecimal(value)
				if wire.Coefficient != vector.Wire.Coefficient || wire.Scale != vector.Wire.Scale {
					t.Fatalf("wireDecimal(%q) = %v, want %v", vector.Text, wire, vector.Wire)
				}
			})
		}
	})
	t.Run("seconds", func(t *testing.T) {
		for _, vector := range vectors.Times {
			t.Run(vector.Name, func(t *testing.T) {
				got, err := d.SecondsToNanos(vector.Seconds)
				if (err == nil) != vector.Valid || vector.Valid && got != vector.Nanoseconds {
					t.Fatalf("got %d, %v; valid=%v want=%d", got, err, vector.Valid, vector.Nanoseconds)
				}
			})
		}
	})
	t.Run("identity_and_observation", func(t *testing.T) {
		if vectors.EngineVersion != d.EnvironmentVersion || vectors.Observation.Version != replay.ObservationVersion || !reflect.DeepEqual(vectors.Observation.FieldNames, replay.ObservationFields) {
			t.Fatal("environment or observation identity differs from shared contract")
		}
		result, err := transition("contract", replay.StepResult{Observation: make([]float32, len(vectors.Observation.FieldNames))})
		if err != nil || result.Observation.SchemaVersion != vectors.Observation.Version || !reflect.DeepEqual(result.Observation.FieldNames, vectors.Observation.FieldNames) {
			t.Fatalf("wire observation differs: %v, %v", result, err)
		}
	})
	t.Run("actions", func(t *testing.T) {
		for _, vector := range vectors.Actions {
			action, err := d.ActionFromLegacy(vector.Legacy)
			if err != nil || int32(action) != vector.Wire || pb.Action_name[vector.Wire] != vector.Name {
				t.Fatalf("legacy action %d changed: %v, %v", vector.Legacy, action, err)
			}
		}
		for _, legacy := range vectors.InvalidLegacyActions {
			if _, err := d.ActionFromLegacy(legacy); err == nil {
				t.Fatalf("invalid legacy action %d accepted", legacy)
			}
		}
	})
	t.Run("enum_rpc_rejection", func(t *testing.T) {
		maps := map[string]map[string]int32{"Action": pb.Action_value, "Side": pb.Side_value, "EventKind": pb.EventKind_value, "FeeCurrency": pb.FeeCurrency_value}
		for name, vector := range vectors.Enums {
			if !reflect.DeepEqual(maps[name], vector.Values) {
				t.Fatalf("%s enum names/numbers changed", name)
			}
			for _, value := range vector.RejectedWire {
				t.Run(name+"/"+pb.Action(value).String(), func(t *testing.T) {
					s := newTestServer(t, DefaultLimits())
					if name == "Action" {
						created := create(t, s, "contract", 1)
						_, err := s.StepBatch(context.Background(), stepRequest(created, "bad-enum", 1, pb.Action(value)))
						wantCode(t, err, codes.InvalidArgument)
						return
					}
					spec := fixture()
					switch name {
					case "Side":
						spec.Plan.Side = pb.Side(value)
					case "EventKind":
						spec.Events[0].Kind = pb.EventKind(value)
					case "FeeCurrency":
						spec.Fees.Currency = pb.FeeCurrency(value)
					default:
						t.Fatalf("unknown enum family %q", name)
					}
					_, err := s.CreateBatch(context.Background(), &pb.CreateBatchRequest{RequestId: "bad-enum", Count: 1, Episode: spec})
					wantCode(t, err, codes.InvalidArgument)
				})
			}
		}
	})
}

package domain

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"testing"
)

func TestLegacyFloatTimerCompatibility(t *testing.T) {
	raw, err := os.ReadFile("../../conformance/time-compatibility.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Schema string
		Order  []string `json:"v1_equal_time_order"`
		Cases  []struct {
			Name    string
			Start   string `json:"start_seconds"`
			Latency string `json:"latency_seconds"`
			Receipt string `json:"receipt_seconds"`
			Legacy  string `json:"legacy_arrival_relation"`
			V1      string `json:"v1_arrival_relation"`
		}
	}
	if json.Unmarshal(raw, &fixture) != nil || fixture.Schema != "legacy-float-timer-compatibility-v1" || len(fixture.Cases) != 3 || !reflect.DeepEqual(fixture.Order, []string{"receipt", "arrival", "cancel"}) {
		t.Fatal("invalid compatibility fixture")
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			legacy := make([]float64, 3)
			nanos := make([]int64, 3)
			for i, input := range []string{c.Start, c.Latency, c.Receipt} {
				var err error
				legacy[i], err = strconv.ParseFloat(input, 64)
				if err != nil {
					t.Fatal(err)
				}
				nanos[i], err = SecondsToNanos(input)
				if err != nil {
					t.Fatal(err)
				}
			}
			actual := "equal"
			if legacy[0]+legacy[1] < legacy[2] {
				actual = "before"
			}
			if legacy[0]+legacy[1] > legacy[2] {
				actual = "after"
			}
			if actual != c.Legacy || c.V1 != "equal" || nanos[0]+nanos[1] != nanos[2] {
				t.Fatalf("legacy=%s want=%s; v1 timer=%d receipt=%d", actual, c.Legacy, nanos[0]+nanos[1], nanos[2])
			}
		})
	}
}

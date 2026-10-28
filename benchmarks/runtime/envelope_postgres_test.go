package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestPostgresEnvelopeHTTPAndPolicyFallback(t *testing.T) {
	if os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN") == "" {
		t.Skip("set explicit development PostgreSQL DSN for envelope integration")
	}
	for _, fallback := range []string{"baseline", "pause"} {
		t.Run(fallback, func(t *testing.T) {
			c := testConfig()
			c.Schema = "ll_bench_envelope_" + strconv.FormatInt(time.Now().UnixNano(), 36)
			c.Output = filepath.Join(t.TempDir(), "envelope.json")
			c.ReadMode = "http"
			c.Warmup = 100 * time.Millisecond
			c.Duration = 500 * time.Millisecond
			c.MarketRate = 10
			c.ReadRate = 20
			c.Queue = 64
			c.MockPolicyDelay = 100 * time.Millisecond
			c.PolicyTimeout = 20 * time.Millisecond
			c.PolicyFallback = fallback
			if err := execute(c); err != nil {
				data, _ := os.ReadFile(c.Output)
				t.Fatalf("run: %v; report: %s", err, data)
			}
			data, err := os.ReadFile(c.Output)
			if err != nil {
				t.Fatal(err)
			}
			var r report
			if err = json.Unmarshal(data, &r); err != nil {
				t.Fatal(err)
			}
			if !r.Valid || !r.FinancialAudit.Valid || r.Policy.Kind != "controlled_local_grpc" || r.Policy.Quiescence != "client_calls_drained" || r.Policy.Quiescent.InFlight != 0 {
				t.Fatalf("invalid envelope report: valid=%v policy=%+v", r.Valid, r.Policy)
			}
			if r.Policy.Quiescent.Failed == 0 {
				t.Fatal("delayed worker did not produce observed client errors")
			}
			fallbackCount := uint64(0)
			for _, counter := range r.QuiescentTelemetry.Counters {
				if counter.Name == "policy_fallbacks" {
					fallbackCount = counter.Value
				}
			}
			if fallbackCount == 0 {
				t.Fatal("no durable policy fallback observed")
			}
			if fallback == "baseline" && r.FinancialAudit.Filled.IsZero() {
				t.Fatal("baseline fallback never exercised an actual fill")
			}
			if fallback == "pause" && (r.Final.State.LastPolicyDecision == nil || r.Final.State.LastPolicyDecision.Result != "fallback_pause") {
				t.Fatal("pause fallback missing from durable final audit")
			}
			if r.Measurement.Views.Completed != r.Measurement.Views.Offered || len(r.Measurement.RouteCounts) != 3 {
				t.Fatalf("HTTP mix incomplete: %+v", r.Measurement.RouteCounts)
			}
			for _, sample := range r.Measurement.Samples {
				if sample.Kind == "view" && (sample.ResponseBytes == 0 || sample.HTTPStatus != 200 || sample.ServiceBoundary != "HTTP full response and JSON validation") {
					t.Fatalf("HTTP response was not consumed: %+v", sample)
				}
			}
			// Retain this unique schema and all accounts; never reset or drop them.
		})
	}
}

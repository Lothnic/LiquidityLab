package main

import "testing"

func TestLatencyGroupsRetainSeparateKindsAndAllOutcomes(t *testing.T) {
	ptr := func(n int64) *int64 { return &n }
	samples := []sample{
		{Kind: "market", IntendedNS: 0, AttemptNS: 1, AdmittedNS: ptr(2), CallNS: ptr(4), CompletedNS: ptr(10), Outcome: "completed"},
		{Kind: "view", IntendedNS: 0, AttemptNS: 1, AdmittedNS: ptr(2), CallNS: ptr(4), CompletedNS: ptr(100), Outcome: "completed"},
		{Kind: "market", IntendedNS: 0, AttemptNS: 2, Outcome: "runner_rejected"},
		{Kind: "view", IntendedNS: 0, AttemptNS: 1, AdmittedNS: ptr(2), CallNS: ptr(4), CompletedNS: ptr(1000), Outcome: "outcome_unknown"},
	}
	got := latencies(samples)
	if got["service_call_success"].Count != 2 || got["service_call_all_returns"].Count != 3 || got["generator_lag"].Count != 4 {
		t.Fatal("combined distributions omitted offered or failed work")
	}
	if got["market_offered_to_success"].Count != 1 || got["market_offered_to_success"].P99 != 10 || got["view_offered_to_success"].Count != 1 || got["view_offered_to_success"].P99 != 100 {
		t.Fatal("market and view successes were mixed")
	}
	if got["view_service_call_all_returns"].Count != 2 || got["view_service_call_all_returns"].Max != 996 || got["market_generator_lag"].Count != 2 {
		t.Fatal("per-kind returns and rejected demand were lost")
	}
}

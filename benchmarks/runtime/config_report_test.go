package main

import (
	"testing"
	"time"

	ar "github.com/liquiditylab/liquiditylab/internal/runtime"
)

func TestConfigReportIncludesEffectiveReductionAndViewBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config ar.Config
		views  int
		steps  int
		budget time.Duration
	}{
		{"inherited", ar.Config{MailboxCapacity: 7}, 7, 4, 5 * time.Millisecond},
		{"explicit", ar.Config{MailboxCapacity: 7, ViewCapacity: 11, FactQuantumSteps: 2, FactQuantumTime: time.Millisecond}, 11, 2, time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cfgInfo(tc.config)
			if got["view_capacity"] != tc.views || got["fact_quantum_steps"] != tc.steps || got["fact_quantum_time_ns"] != int64(tc.budget) {
				t.Fatalf("effective scheduling config missing: %+v", got)
			}
		})
	}
}

package runtime

import (
	"strings"
	"time"

	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
	"github.com/liquiditylab/liquiditylab/internal/telemetry"
)

func (a *Actor) metricStart() time.Time {
	if a.config.Telemetry == nil {
		return time.Time{}
	}
	return time.Now()
}

func (a *Actor) observeElapsed(id telemetry.Distribution, started time.Time) {
	if a.config.Telemetry != nil && !started.IsZero() {
		a.config.Telemetry.Observe(id, uint64(max(0, time.Since(started).Nanoseconds())))
	}
}

// recordDecisionCommit runs at acknowledged persistence, before account reload.
// Only process-local start times can produce latency samples. Recovered pending
// requests therefore never fabricate elapsed time across owners or restarts.
func (a *Actor) recordDecisionCommit(next State, command pg.Command) {
	if pending := a.state.PolicyPending; pending != nil && next.PolicyPending == nil && next.LastPolicyDecision != nil && next.LastPolicyDecision.RequestID == pending.RequestID {
		a.config.Telemetry.Add(telemetry.PolicyDecisions, 1)
		if strings.HasPrefix(next.LastPolicyDecision.Result, "fallback_") {
			a.config.Telemetry.Add(telemetry.PolicyFallbacks, 1)
		}
		a.observeElapsed(telemetry.DecisionCommitNS, a.policyStartedAt)
		a.policyStartedAt = time.Time{}
	} else if command.Kind == "decision" && next.DecisionCount > a.state.DecisionCount {
		a.observeElapsed(telemetry.DecisionCommitNS, a.decisionStartedAt)
	}
}

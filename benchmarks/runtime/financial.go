package main

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	d "github.com/liquiditylab/liquiditylab/internal/domain"
	ar "github.com/liquiditylab/liquiditylab/internal/runtime"
	pg "github.com/liquiditylab/liquiditylab/internal/store/postgres"
)

type financialAudit struct {
	Valid              bool                 `json:"valid"`
	Issues             []string             `json:"issues"`
	ExpectedTotals     map[string]d.Decimal `json:"expected_totals_from_initial_and_fills"`
	Filled             d.Decimal            `json:"snapshot_fill_quantity"`
	DurableFilled      d.Decimal            `json:"durable_execution_quantity"`
	Remaining          d.Decimal            `json:"remaining_plan_quantity"`
	SnapshotExecutions int                  `json:"snapshot_execution_count"`
	DurableExecutions  int                  `json:"durable_execution_count"`
}

// auditFinancial deliberately computes conservation directly from signed cash
// flows, without invoking the execution ledger used by the actor itself.
func auditFinancial(initial []pg.Balance, v ar.View, durable []d.Fill) financialAudit {
	out := financialAudit{Valid: true, Issues: []string{}, ExpectedTotals: map[string]d.Decimal{}, SnapshotExecutions: len(v.State.Fills), DurableExecutions: len(durable)}
	fail := func(reason string) { out.Valid = false; out.Issues = append(out.Issues, reason) }
	for _, balance := range initial {
		out.ExpectedTotals[balance.Currency] = balance.Total
	}
	if v.State.Parent == nil {
		fail("missing final parent")
		return out
	}
	plan := v.State.Parent.Spec
	if plan.Validate() != nil {
		fail("invalid final parent plan")
		return out
	}
	executions := map[string]d.Fill{}
	for _, fill := range v.State.Fills {
		if fill.Validate() != nil {
			fail("invalid snapshot fill")
			continue
		}
		if _, seen := executions[fill.ExecutionID]; seen {
			fail("duplicate snapshot execution identity")
			continue
		}
		executions[fill.ExecutionID] = fill
		if fill.Side != plan.Side {
			fail("fill side differs from parent")
		}
		if fill.Side == "buy" && fill.Price.Cmp(plan.LimitPrice) > 0 || fill.Side == "sell" && fill.Price.Cmp(plan.LimitPrice) < 0 {
			fail("fill price breaches parent limit")
		}
		out.Filled = out.Filled.Add(fill.Quantity)
		baseDelta := fill.Quantity
		quoteDelta := fill.Quantity.Mul(fill.Price).Neg()
		if fill.Side == "sell" {
			baseDelta = baseDelta.Neg()
			quoteDelta = quoteDelta.Neg()
		}
		if fill.FeeCurrency == "base" {
			baseDelta = baseDelta.Sub(fill.Fee)
		} else {
			quoteDelta = quoteDelta.Sub(fill.Fee)
		}
		out.ExpectedTotals[v.State.BaseCurrency] = out.ExpectedTotals[v.State.BaseCurrency].Add(baseDelta)
		out.ExpectedTotals[v.State.QuoteCurrency] = out.ExpectedTotals[v.State.QuoteCurrency].Add(quoteDelta)
	}
	if out.Filled.Cmp(plan.Quantity) > 0 {
		fail("filled quantity exceeds parent quantity")
	}
	out.Remaining = plan.Quantity.Sub(out.Filled)
	switch v.State.Parent.Status {
	case "completed":
		if !out.Remaining.IsZero() {
			fail("completed parent retains remaining quantity")
		}
	case "cancelled", "expired":
		if out.Remaining.IsZero() {
			fail("fully filled parent is not completed")
		}
	default:
		fail("final parent status is not terminal")
	}
	if out.Filled.Sign() > 0 && !v.State.Parent.Started {
		fail("unstarted parent has fills")
	}
	durableIDs := map[string]bool{}
	for _, fill := range durable {
		if durableIDs[fill.ExecutionID] {
			fail("duplicate durable execution identity")
		}
		durableIDs[fill.ExecutionID] = true
		out.DurableFilled = out.DurableFilled.Add(fill.Quantity)
		if snapshot, ok := executions[fill.ExecutionID]; !ok || !snapshot.Equal(fill) {
			fail("durable execution differs from snapshot")
		}
	}
	if len(durable) != len(v.State.Fills) || !out.Filled.Equal(out.DurableFilled) {
		fail("snapshot and durable filled quantities/counts differ")
	}
	seenBalances := map[string]bool{}
	for _, balance := range v.Account.Balances {
		if seenBalances[balance.Currency] {
			fail("duplicate balance currency")
		}
		seenBalances[balance.Currency] = true
		want, ok := out.ExpectedTotals[balance.Currency]
		if !ok || !balance.Total.Equal(want) || !balance.Total.Equal(balance.Free.Add(balance.Locked)) || balance.Total.Sign() < 0 || balance.Free.Sign() < 0 || balance.Locked.Sign() < 0 {
			fail("balance does not conserve initial amount plus fills and fees")
		}
		if !balance.Locked.IsZero() || !balance.Free.Equal(balance.Total) {
			fail("final balance retains locked funds")
		}
	}
	if len(seenBalances) != len(out.ExpectedTotals) {
		fail("final currency set differs from initial currency set")
	}
	return out
}

func validateFinancial(ctx context.Context, conn *pgx.Conn, schema string, initial []pg.Balance, v ar.View) financialAudit {
	rows, err := conn.Query(ctx, "SELECT detail FROM "+pgx.Identifier{schema, "executions"}.Sanitize()+" WHERE tenant_id=$1 AND account_id=$2 AND venue='paper' ORDER BY execution_id LIMIT 4097", v.Account.Key.TenantID, v.Account.Key.AccountID)
	if err != nil {
		return financialAudit{Valid: false, Issues: []string{"durable execution read unavailable"}}
	}
	defer rows.Close()
	fills := []d.Fill{}
	for rows.Next() {
		var raw json.RawMessage
		var fill d.Fill
		if err = rows.Scan(&raw); err != nil {
			return financialAudit{Valid: false, Issues: []string{"durable execution row unavailable"}}
		}
		if err = json.Unmarshal(raw, &fill); err != nil {
			return financialAudit{Valid: false, Issues: []string{"durable execution payload invalid"}}
		}
		fills = append(fills, fill)
	}
	if rows.Err() != nil || len(fills) > 4096 {
		return financialAudit{Valid: false, Issues: []string{"durable execution read failed or exceeded bound"}}
	}
	return auditFinancial(initial, v, fills)
}

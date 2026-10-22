package replay

func (e *Engine) Report() Report {
	mid := e.lastMid
	if e.frozenMid != nil {
		mid = *e.frozenMid
	}
	cost := e.Ledger.MarkedCost(e.InitialMid, mid)
	var objective any
	if e.Terminated {
		objective = -10000*cost.Quo(e.Plan.Quantity.Mul(e.InitialMid)).Float64() - e.Config.LambdaMiss*e.Ledger.Remaining().Quo(e.Plan.Quantity).Float64()
	}
	return Report{"schema_version": "report-v1", "compiler_version": "six-action-v1", "environment_version": "LiquidityLab-SpotExecution-v1", "dataset_sha256": e.DatasetHash, "dataset_events": len(e.events), "events_processed": e.index, "mode": "historical-simulation", "fill_model": e.Config.FillModel, "observation_version": ObservationVersion, "status": e.Status, "terminated": e.Terminated, "truncated": e.Truncated, "economic_score_valid": e.Terminated, "plan": e.Plan, "instrument_assumption": e.Rules, "fee_assumption": e.Fees, "config": e.Config, "time_ns": e.Now, "initial_midpoint": e.InitialMid, "residual_midpoint": mid, "filled_quantity": e.Ledger.Filled, "remaining_quantity": e.Ledger.Remaining(), "completion_fraction": e.Ledger.Filled.Quo(e.Plan.Quantity), "average_price": e.Ledger.AveragePrice(), "gross_notional": e.Ledger.Notional, "fees_quote": e.Ledger.FeesQuote, "fees_base": e.Ledger.FeesBase, "net_base_change": e.Ledger.Base.Sub(e.Plan.BaseBalance), "base_balance": e.Ledger.Base, "quote_balance": e.Ledger.Quote, "marked_cost_quote": cost, "reward_sum": e.rewardSum, "terminal_objective": objective, "fills": e.Ledger.Fills, "decisions": e.Decisions, "orders": e.Orders, "limitations": []string{"Synthetic/account fee assumptions require verification", "No market impact response", "L2 queue approximation; trade prints only", "Price features warm up over ten completed decision intervals"}}
}

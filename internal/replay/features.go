package replay

import x "github.com/liquiditylab/liquiditylab/internal/execution"

func (e *Engine) Observe() []float32 {
	observation, err := x.BuildObservation(x.FeatureSnapshot{
		Plan: e.Plan, Ledger: e.Ledger, Book: e.Book, Child: e.Child, Now: e.Now,
		ReferenceMid: e.lastMid, DecisionMids: e.decisionMids, TradeFlow: e.tradeFlow,
		AccountValid: true,
	})
	if err != nil {
		panic(err)
	}
	return observation
}

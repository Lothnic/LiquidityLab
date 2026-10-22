package execution

import (
	"fmt"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

// ReservationFor returns the maximum compiled child debit, including the larger
// configured maker/taker fee when that fee is charged in the debited currency.
// Currency kind is quote for buys and base for sells; the caller binds metadata.
func ReservationFor(intent d.OrderIntent, fees d.FeeConfig) (kind string, amount d.Decimal, err error) {
	defer func() {
		if recover() != nil {
			kind = ""
			amount = d.Zero
			err = fmt.Errorf("reservation arithmetic exceeds envelope")
		}
	}()
	if err = fees.Validate(); err != nil {
		return "", d.Zero, err
	}
	if intent.Quantity.Sign() <= 0 || intent.Price.Sign() <= 0 || (intent.Side != "buy" && intent.Side != "sell") {
		return "", d.Zero, fmt.Errorf("invalid reservation intent")
	}
	kind, amount = "base", intent.Quantity
	if intent.Side == "buy" {
		kind = "quote"
		amount = amount.Mul(intent.Price)
	}
	if fees.Currency == kind {
		amount = amount.Mul(d.One.Add(d.Max(fees.Maker, fees.Taker)))
	}
	return kind, amount, nil
}

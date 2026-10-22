package execution

import (
	"fmt"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

// FeeFor is the shared simulator fee rule; confirmed venue fees remain facts.
func FeeFor(quantity, price, rate d.Decimal, currency string) (fee d.Decimal, err error) {
	defer func() {
		if recover() != nil {
			fee = d.Zero
			err = fmt.Errorf("fee arithmetic exceeds envelope")
		}
	}()
	if quantity.Sign() <= 0 || price.Sign() <= 0 || rate.Sign() < 0 || rate.Cmp(d.One) >= 0 || (currency != "base" && currency != "quote") {
		return d.Zero, fmt.Errorf("invalid fee inputs")
	}
	fee = quantity.Mul(rate)
	if currency == "quote" {
		fee = fee.Mul(price)
	}
	return fee, nil
}

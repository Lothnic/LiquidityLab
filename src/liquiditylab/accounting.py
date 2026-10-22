"""Exact spot ledger; duplicate executions are idempotent."""

from decimal import Decimal

from .contracts import ZERO, D, Fill, Plan


class Ledger:
    def __init__(self, plan: Plan):
        self.plan = plan
        self.base = plan.base_balance
        self.quote = plan.quote_budget
        self.filled = ZERO
        self.notional = ZERO
        self.fees_quote = ZERO
        self.fees_base = ZERO
        self.fills: list[Fill] = []
        self._executions: dict[str, Fill] = {}

    @property
    def remaining(self) -> Decimal:
        return self.plan.quantity - self.filled

    @property
    def average_price(self) -> Decimal:
        return self.notional / self.filled if self.filled else ZERO

    def apply(self, fill: Fill) -> bool:
        previous = self._executions.get(fill.execution_id)
        if previous is not None:
            if previous != fill:
                raise ValueError("conflicting duplicate execution")
            return False
        if fill.side != self.plan.side or fill.quantity > self.remaining:
            raise ValueError("fill exceeds parent obligation or side")
        if (fill.side == "buy" and fill.price > self.plan.limit_price) or (
            fill.side == "sell" and fill.price < self.plan.limit_price
        ):
            raise ValueError("fill breaches hard price boundary")
        quote_fee = fill.fee if fill.fee_currency == "quote" else ZERO
        base_fee = fill.fee if fill.fee_currency == "base" else ZERO
        notional = fill.quantity * fill.price
        base = self.base + (fill.quantity if fill.side == "buy" else -fill.quantity) - base_fee
        quote = self.quote + (-notional if fill.side == "buy" else notional) - quote_fee
        if base < 0 or quote < 0:
            raise ValueError("fill would create a negative balance")
        self.base, self.quote = base, quote
        self.filled += fill.quantity
        self.notional += notional
        self.fees_quote += quote_fee
        self.fees_base += base_fee
        self.fills.append(fill)
        self._executions[fill.execution_id] = fill
        return True

    def marked_cost(self, initial_mid: Decimal, residual_mid: Decimal) -> Decimal:
        sign = D(1) if self.plan.side == "buy" else D(-1)
        execution_cost = sign * (self.notional - self.filled * initial_mid)
        # Convert base fees at their fill price, rather than a later midpoint.
        fees = sum(
            (f.fee * f.price if f.fee_currency == "base" else f.fee for f in self.fills), ZERO
        )
        return execution_cost + fees + sign * self.remaining * (residual_mid - initial_mid)

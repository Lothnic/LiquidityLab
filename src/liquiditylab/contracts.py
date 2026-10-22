"""Versioned domain contracts. Monetary values are Decimal, features alone use floats."""

from dataclasses import dataclass, field
from decimal import ROUND_CEILING, ROUND_FLOOR, Decimal
from enum import IntEnum
from typing import Any

D = Decimal
ZERO = D("0")


def decimal(value: Any) -> Decimal:
    result = D(str(value))
    if not result.is_finite():
        raise ValueError("monetary values must be finite")
    return result


def floor_step(value: Decimal, step: Decimal) -> Decimal:
    return (value / step).to_integral_value(rounding=ROUND_FLOOR) * step


def ceil_step(value: Decimal, step: Decimal) -> Decimal:
    return (value / step).to_integral_value(rounding=ROUND_CEILING) * step


class Action(IntEnum):
    HOLD = 0
    CANCEL = 1
    PASSIVE_SMALL = 2
    PASSIVE_NORMAL = 3
    AGGRESSIVE_SMALL = 4
    AGGRESSIVE_NORMAL = 5


@dataclass(frozen=True)
class InstrumentRules:
    symbol: str = "BTC/USD"
    tick_size: Decimal = D("0.01")
    lot_size: Decimal = D("0.001")
    min_quantity: Decimal = D("0.001")
    min_notional: Decimal = D("0.01")

    def __post_init__(self):
        for name in ("tick_size", "lot_size", "min_quantity", "min_notional"):
            object.__setattr__(self, name, decimal(getattr(self, name)))
            if getattr(self, name) <= 0:
                raise ValueError(f"{name} must be positive")


@dataclass(frozen=True)
class FeeConfig:
    maker: Decimal = D("0.001")
    taker: Decimal = D("0.002")
    currency: str = "quote"
    source: str = "synthetic-assumption"

    def __post_init__(self):
        for name in ("maker", "taker"):
            object.__setattr__(self, name, decimal(getattr(self, name)))
            if not ZERO <= getattr(self, name) < 1:
                raise ValueError("fee rates must be in [0,1)")
        if self.currency not in ("base", "quote"):
            raise ValueError("fee currency must be base or quote")


@dataclass(frozen=True)
class Plan:
    side: str = "buy"
    quantity: Decimal = D("0.1")
    limit_price: Decimal = D("102")
    deadline: float = 30.0
    max_child_quantity: Decimal = D("0.1")
    quote_budget: Decimal = D("1000")
    base_balance: Decimal = D("1")
    symbol: str = "BTC/USD"

    def __post_init__(self):
        for name in (
            "quantity",
            "limit_price",
            "max_child_quantity",
            "quote_budget",
            "base_balance",
        ):
            object.__setattr__(self, name, decimal(getattr(self, name)))
        if self.side not in ("buy", "sell"):
            raise ValueError("side must be buy or sell")
        if min(self.quantity, self.limit_price, self.max_child_quantity) <= 0:
            raise ValueError("quantity, limit and max child must be positive")
        if min(self.quote_budget, self.base_balance) < 0 or not 0 < self.deadline < float("inf"):
            raise ValueError("balances must be nonnegative and deadline finite/positive")


@dataclass(frozen=True)
class ReplayConfig:
    decision_interval: float = 1.0
    submission_latency: float = 0.05
    cancel_latency: float = 0.05
    settlement_allowance: float = 30.0
    lambda_miss: float = 100.0
    max_participation: Decimal = D("0.25")
    min_resting_age: float = 2.0
    min_message_interval: float = 0.1
    fill_model: str = "conservative-v0"

    def __post_init__(self):
        object.__setattr__(self, "max_participation", decimal(self.max_participation))
        if not ZERO < self.max_participation <= 1:
            raise ValueError("participation must be in (0,1]")
        if not all(
            0 <= x < float("inf")
            for x in (
                self.submission_latency,
                self.cancel_latency,
                self.settlement_allowance,
                self.lambda_miss,
                self.min_resting_age,
                self.min_message_interval,
            )
        ) or not 0 < self.decision_interval < float("inf"):
            raise ValueError("invalid timing or reward configuration")


@dataclass(frozen=True)
class OrderIntent:
    side: str
    quantity: Decimal
    price: Decimal
    passive: bool


@dataclass
class ChildOrder:
    order_id: str
    intent: OrderIntent
    submitted_at: float
    arrival_at: float
    remaining: Decimal
    state: str = "submitting"
    queue_ahead: Decimal = ZERO
    cancel_at: float | None = None


@dataclass(frozen=True)
class Fill:
    execution_id: str
    order_id: str
    side: str
    quantity: Decimal
    price: Decimal
    fee: Decimal
    fee_currency: str
    time: float
    maker: bool = False

    def __post_init__(self):
        for name in ("quantity", "price", "fee"):
            object.__setattr__(self, name, decimal(getattr(self, name)))
        if self.side not in ("buy", "sell") or self.fee_currency not in ("base", "quote"):
            raise ValueError("invalid fill side or fee currency")
        if self.quantity <= 0 or self.price <= 0 or self.fee < 0:
            raise ValueError("invalid fill amount")


@dataclass(frozen=True)
class CompileResult:
    intent: OrderIntent | None = None
    cancel: bool = False
    reason: str = "hold"
    details: dict = field(default_factory=dict)

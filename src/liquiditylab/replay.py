"""Deterministic receipt-ordered replay with simulated, isolated spot orders."""

import hashlib
import json
import math
from collections import deque
from dataclasses import asdict
from decimal import Decimal

import numpy as np

from .accounting import Ledger
from .book import Book
from .compiler import compile_action
from .contracts import (
    ZERO,
    Action,
    ChildOrder,
    D,
    FeeConfig,
    Fill,
    InstrumentRules,
    Plan,
    ReplayConfig,
    decimal,
)

OBSERVATION_VERSION = "compact-v1"
OBSERVATION_SIZE = 20
OBSERVATION_FIELDS = (
    "remaining_fraction", "time_remaining_fraction", "filled_fraction",
    "average_fill_to_mid_ratio", "reserved_fraction", "free_quote_fraction",
    "signed_limit_distance", "spread_bps", "bid_depth_to_target", "ask_depth_to_target",
    "recent_mid_return", "recent_log_return_std", "cumulative_trade_flow_to_target",
    "signed_child_price_distance", "child_age_fraction", "child_remaining_fraction",
    "has_child", "market_valid", "simulated_account_valid", "child_pending",
)


def synthetic_events():
    events = [
        {
            "time": 0.0,
            "kind": "snapshot",
            "bids": [["99.99", "2"]],
            "asks": [["100.01", "2"]],
            "valid": True,
        }
    ]
    for t in range(1, 66):
        events.append(
            {
                "time": float(t),
                "kind": "trade",
                "price": "99.99",
                "quantity": "0.4",
                "side": "sell",
                "trade_id": f"s{t}",
            }
        )
    return events


def serializable(value):
    if isinstance(value, Decimal):
        return str(value)
    if isinstance(value, dict):
        return {k: serializable(v) for k, v in value.items()}
    if isinstance(value, (tuple, list)):
        return [serializable(v) for v in value]
    return value


class Replay:
    def __init__(self, events, plan=None, rules=None, fees=None, config=None):
        self.plan = plan or Plan()
        self.rules = rules or InstrumentRules()
        self.fees = fees or FeeConfig()
        self.config = config or ReplayConfig()
        if self.plan.symbol != self.rules.symbol:
            raise ValueError("plan and instrument symbol differ")
        self.events = [dict(event) for event in events]
        previous = -1.0
        for i, event in enumerate(self.events):
            event["time"] = float(event.get("time", event.get("ts", -1)))
            event["kind"] = event.get("kind", event.get("type"))
            if not math.isfinite(event["time"]) or event["time"] < max(previous, 0):
                raise ValueError("events must preserve nonnegative receipt order")
            if event["kind"] not in ("snapshot", "book", "trade", "gap"):
                raise ValueError("unsupported market event kind")
            if event.get("symbol", self.plan.symbol) != self.plan.symbol:
                raise ValueError("recording contains another symbol")
            event.setdefault("receipt_index", i)
            previous = event["time"]
        if not self.events or self.events[0]["time"] != 0 or self.events[0]["kind"] != "snapshot":
            raise ValueError("episode must begin with a snapshot at receipt time zero")
        if previous < self.plan.deadline + self.config.settlement_allowance:
            raise ValueError("recording must cover deadline plus settlement allowance")
        self.dataset_hash = hashlib.sha256(
            json.dumps(self.events, sort_keys=True, default=str).encode()
        ).hexdigest()
        self.reset()

    def reset(self):
        self.book = Book()
        self.ledger = Ledger(self.plan)
        self.child = None
        self.now = 0.0
        self._index = 0
        self._order_counter = 0
        self._fill_counter = 0
        self._seen_trades = set()
        self._last_message = -math.inf
        self._previous_cost = ZERO
        self._reward_sum = 0.0
        self._last_mid = ZERO
        self._frozen_mid = None
        self._done = False
        self.terminated = False
        self.truncated = False
        self.status = "running"
        self.reason = "reset"
        self.decisions = []
        self.orders = []
        self._trade_flow = ZERO
        self._advance(0.0)
        if not self.book.valid:
            raise ValueError("initial snapshot is invalid or crossed")
        self.initial_mid = self.book.mid
        self._last_mid = self.initial_mid
        self._decision_mids = deque([self.initial_mid], maxlen=11)
        return self.observation(), self.info()

    def _event(self, event):
        if event["kind"] in ("snapshot", "book", "gap"):
            self.book.apply(event)
            if self.book.valid:
                self._last_mid = self.book.mid
            else:
                self._truncate("invalid_market_data")
            return
        if not event.get("valid", True):
            self._truncate("invalid_market_data")
            return
        identity = event.get("trade_id", f"receipt:{event['receipt_index']}")
        if identity in self._seen_trades:
            return
        self._seen_trades.add(identity)
        quantity, price = decimal(event["quantity"]), decimal(event["price"])
        if quantity <= 0 or price <= 0 or event.get("side") not in ("buy", "sell"):
            raise ValueError("invalid trade")
        self._trade_flow += quantity if event["side"] == "buy" else -quantity
        child = self.child
        if child is None or child.state not in ("open", "cancel_requested"):
            return
        qualifying = (child.intent.side == "buy" and event["side"] == "sell") or (
            child.intent.side == "sell" and event["side"] == "buy"
        )
        if not qualifying or price != child.intent.price:
            return
        advance = min(quantity, child.queue_ahead)
        child.queue_ahead -= advance
        remaining_volume = quantity - advance
        if remaining_volume > 0:
            self._fill(min(remaining_volume, child.remaining), child.intent.price, maker=True)

    def _fill(self, quantity, price, maker):
        child = self.child
        if quantity <= 0 or child is None:
            return
        rate = self.fees.maker if maker else self.fees.taker
        fee = quantity * rate * (price if self.fees.currency == "quote" else D(1))
        self._fill_counter += 1
        fill = Fill(
            f"sim-{self._fill_counter}",
            child.order_id,
            self.plan.side,
            quantity,
            price,
            fee,
            self.fees.currency,
            self.now,
            maker,
        )
        self.ledger.apply(fill)
        child.remaining -= quantity
        if child.remaining == 0:
            child.state = "filled"
            self.child = None

    def _arrive(self):
        child = self.child
        if child is None:
            return
        if not self.book.valid:
            child.state = "rejected"
            self.child = None
            return
        intent = child.intent
        if intent.passive:
            crosses = (intent.side == "buy" and intent.price >= self.book.best_ask) or (
                intent.side == "sell" and intent.price <= self.book.best_bid
            )
            if crosses:
                child.state = "rejected_post_only"
                self.child = None
                return
            side = "bids" if intent.side == "buy" else "asks"
            child.queue_ahead = self.book.available(side, intent.price)
            child.state = "cancel_requested" if child.cancel_at is not None else "open"
            return
        side = "asks" if intent.side == "buy" else "bids"
        prices = sorted(getattr(self.book, side), reverse=intent.side == "sell")
        for price in prices:
            if (intent.side == "buy" and price > intent.price) or (
                intent.side == "sell" and price < intent.price
            ):
                break
            quantity = min(child.remaining, self.book.available(side, price))
            self.book.consume(side, price, quantity)
            self._fill(quantity, price, maker=False)
            if self.child is None:
                break
        if self.child is not None:
            self.child.state = "ioc_expired"
            self.child = None

    def _advance(self, target, *, stop_when_settled=False, stop_on_completion=False):
        while not self._done:
            if stop_when_settled and self.child is None:
                return
            if stop_on_completion and self.ledger.remaining == 0 and self.child is None:
                return
            market_time = (
                self.events[self._index]["time"] if self._index < len(self.events) else math.inf
            )
            child = self.child
            arrival = child.arrival_at if child and child.state == "submitting" else math.inf
            # A submitting order may also have cancellation pending.
            if child and child.state == "cancel_requested" and self.now < child.arrival_at:
                arrival = child.arrival_at
            cancel = child.cancel_at if child and child.cancel_at is not None else math.inf
            next_time = min(market_time, arrival, cancel)
            if next_time > target or next_time == math.inf:
                break
            self.now = next_time
            # Receipt events win ties, then arrival, then cancellation: a cancel race can fill.
            if market_time == next_time:
                event = self.events[self._index]
                self._index += 1
                self._event(event)
            elif arrival == next_time:
                self._arrive()
            elif self.child is not None:
                self.child.state = "cancelled"
                self.child = None
        if not self._done:
            self.now = target

    def _cancel(self):
        if self.child is not None and self.child.cancel_at is None:
            self.child.cancel_at = self.now + self.config.cancel_latency
            # Keep submission state until arrival so the timer cannot be lost.
            if self.child.state != "submitting":
                self.child.state = "cancel_requested"
            self._last_message = self.now

    def _truncate(self, reason):
        self._done = self.truncated = True
        self.status = reason
        self.reason = reason

    def _settle(self, reason):
        self._frozen_mid = self._last_mid
        self.status = "settling"
        self._cancel()
        if self.child is not None:
            until = self.now + self.config.settlement_allowance
            self._advance(until, stop_when_settled=True)
        if self.truncated:
            return
        if self.child is not None:
            self._truncate("unresolved_settlement")
            return
        self._done = self.terminated = True
        self.status = "completed" if self.ledger.remaining == 0 else "deadline_partial"
        self.reason = reason

    def step(self, action):
        if self._done:
            raise RuntimeError("episode has ended; call reset")
        result = compile_action(
            action,
            plan=self.plan,
            rules=self.rules,
            fees=self.fees,
            config=self.config,
            ledger=self.ledger,
            book=self.book,
            child=self.child,
            now=self.now,
        )
        if (result.cancel or result.intent is not None) and (
            self.now - self._last_message < self.config.min_message_interval
        ):
            self.reason = "message_budget"
        else:
            self.reason = result.reason
            if result.cancel:
                self._cancel()
            elif result.intent is not None:
                self._order_counter += 1
                intent = result.intent
                self.child = ChildOrder(
                    f"child-{self._order_counter}",
                    intent,
                    self.now,
                    self.now + self.config.submission_latency,
                    intent.quantity,
                )
                self.orders.append(self.child)
                self._last_message = self.now
        self.decisions.append(
            {"time": self.now, "action": int(Action(action)), "reason": self.reason}
        )
        self._advance(
            min(self.now + self.config.decision_interval, self.plan.deadline),
            stop_on_completion=True,
        )
        if not self._done and (self.ledger.remaining == 0 or self.now >= self.plan.deadline):
            self._settle("target_filled" if self.ledger.remaining == 0 else "deadline")
        midpoint = self._frozen_mid if self._frozen_mid is not None else self._last_mid
        if not self.truncated:
            self._decision_mids.append(midpoint)
        cost = self.ledger.marked_cost(self.initial_mid, midpoint)
        reward = -10000.0 * float(
            (cost - self._previous_cost) / (self.plan.quantity * self.initial_mid)
        )
        if self.terminated:
            reward -= self.config.lambda_miss * float(self.ledger.remaining / self.plan.quantity)
        self._previous_cost = cost
        self._reward_sum += reward
        return self.observation(), reward, self.terminated, self.truncated, self.info()

    def observation(self):
        q = self.plan.quantity
        mid = self._last_mid or self.book.mid or D(1)
        child = self.child
        remaining = self.ledger.remaining
        reserved = child.remaining if child else ZERO
        sign = D(1) if self.plan.side == "buy" else D(-1)
        mids = np.asarray([float(value) for value in self._decision_mids], dtype=np.float64)
        recent_return = float(mid / self._decision_mids[0] - 1)
        log_returns = np.diff(np.log(mids))
        recent_volatility = float(np.std(log_returns)) if len(log_returns) > 1 else 0.0
        values = [
            remaining / q,
            max(self.plan.deadline - self.now, 0) / self.plan.deadline,
            self.ledger.filled / q,
            self.ledger.average_price / mid if self.ledger.filled else 0,
            reserved / q,
            self.ledger.quote / max(self.plan.quote_budget, D(1)),
            sign * (self.plan.limit_price - mid) / mid,
            (self.book.best_ask - self.book.best_bid) / mid * 10000,
            self.book.visible("bids") / q,
            self.book.visible("asks") / q,
            recent_return,
            recent_volatility,
            self._trade_flow / q,
            sign * (child.intent.price - mid) / mid if child else 0,
            max(self.now - child.arrival_at, 0) / self.plan.deadline if child else 0,
            reserved / q,
            float(child is not None),
            float(self.book.valid),
            1.0,
            float(child is not None and child.state in ("submitting", "cancel_requested")),
        ]
        return np.clip(np.asarray(values, dtype=np.float64), -1e6, 1e6).astype(np.float32)

    def info(self):
        return {
            "status": self.status,
            "reason": self.reason,
            "time": self.now,
            "filled": str(self.ledger.filled),
            "remaining": str(self.ledger.remaining),
            "provisional": not self.terminated,
            "report": self.report() if self._done else None,
        }

    def report(self):
        midpoint = self._frozen_mid if self._frozen_mid is not None else self._last_mid
        cost = (
            self.ledger.marked_cost(self.initial_mid, midpoint)
            if hasattr(self, "initial_mid")
            else ZERO
        )
        objective = (
            (
                -10000 * float(cost / (self.plan.quantity * self.initial_mid))
                - self.config.lambda_miss * float(self.ledger.remaining / self.plan.quantity)
            )
            if self.terminated
            else None
        )
        return serializable(
            {
                "schema_version": "report-v0",
                "compiler_version": "six-action-v0",
                "dataset_sha256": self.dataset_hash,
                "dataset_events": len(self.events),
                "events_processed": self._index,
                "mode": "historical-simulation",
                "fill_model": self.config.fill_model,
                "observation_version": OBSERVATION_VERSION,
                "status": self.status,
                "terminated": self.terminated,
                "truncated": self.truncated,
                "economic_score_valid": self.terminated,
                "plan": asdict(self.plan),
                "instrument_assumption": asdict(self.rules),
                "fee_assumption": asdict(self.fees),
                "config": asdict(self.config),
                "time": self.now,
                "initial_midpoint": getattr(self, "initial_mid", ZERO),
                "residual_midpoint": midpoint,
                "filled_quantity": self.ledger.filled,
                "remaining_quantity": self.ledger.remaining,
                "completion_fraction": self.ledger.filled / self.plan.quantity,
                "average_price": self.ledger.average_price,
                "gross_notional": self.ledger.notional,
                "fees_quote": self.ledger.fees_quote,
                "fees_base": self.ledger.fees_base,
                "net_base_change": self.ledger.base - self.plan.base_balance,
                "base_balance": self.ledger.base,
                "quote_balance": self.ledger.quote,
                "marked_cost_quote": cost,
                "reward_sum": self._reward_sum,
                "terminal_objective": objective,
                "fills": [asdict(f) for f in self.ledger.fills],
                "decisions": self.decisions,
                "orders": [asdict(o) for o in self.orders],
                "limitations": [
                    "Synthetic/account fee assumptions require verification",
                    "No market impact response",
                    "L2 queue approximation; trade prints only",
                    "Zero feature warmup; return/volatility slots reserved",
                ],
            }
        )

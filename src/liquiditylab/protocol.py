"""Exact, versioned conversion at the Go replay boundary.

Legacy tape seconds import with nearest nanosecond, ties to even. Money never
passes through binary floating point. Decimal tuple operations avoid the
caller's ambient Decimal precision.
"""

import re
from decimal import Decimal
from numbers import Integral

from .contracts import FeeConfig, InstrumentRules, Plan, ReplayConfig
from .v1 import execution_pb2 as pb

MAX_TIME_NS = (1 << 63) - 1
GO_ENV_ID = "LiquidityLab-SpotExecution-v1"
_WRITTEN_SECONDS = re.compile(r"[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?")


def encode_decimal(value):
    if isinstance(value, (float, bool)):
        raise ValueError("wire money must be Decimal, a decimal string, or an integer")
    number = Decimal(value)
    if not number.is_finite():
        raise ValueError("wire money must be finite")
    sign, digits, exponent = number.as_tuple()
    # Bound before allocating exponent-sized strings, including zero encodings.
    if len(digits) > 128 or exponent < -36 or exponent > 128:
        raise ValueError("wire money exceeds 128 digits or scale 36")
    coefficient = "".join(str(digit) for digit in digits).lstrip("0") or "0"
    if coefficient == "0":
        return pb.Decimal(coefficient="0", scale=0)
    if exponent > 0:
        coefficient += "0" * exponent
    scale = max(0, -exponent)
    while scale and coefficient.endswith("0"):
        coefficient, scale = coefficient[:-1], scale - 1
    if len(coefficient) > 128:
        raise ValueError("wire money exceeds 128 digits")
    return pb.Decimal(coefficient=("-" if sign else "") + coefficient, scale=scale)


def decode_decimal(value):
    # Derived reports have the core's wider internal precision envelope.
    coefficient = value.coefficient
    digits = coefficient.removeprefix("-")
    if (not digits or len(digits) > 512 or not digits.isascii() or not digits.isdecimal()
            or value.scale > 144 or value.scale < 0
            or len(digits) > 1 and digits.startswith("0")
            or coefficient.startswith("-") and digits == "0"
            or value.scale and digits.endswith("0")):
        raise ValueError("malformed decimal from replay service")
    return Decimal((int(coefficient.startswith("-")), tuple(map(int, digits)), -value.scale))


def seconds_to_ns(value):
    written = str(value)
    if _WRITTEN_SECONDS.fullmatch(written) is None:
        raise ValueError("time must use ASCII decimal or scientific notation")
    number = Decimal(written)
    if not number.is_finite() or number < 0 or number > Decimal("9223372036.854775807"):
        raise ValueError("time must be nonnegative and fit signed nanoseconds")
    sign, digits, exponent = number.as_tuple()
    if len(digits) > 128 or exponent < -128 or exponent > 128:
        raise ValueError("unsupported time precision")
    coefficient = int("".join(map(str, digits)))
    power = exponent + 9
    if power >= 0:
        result = coefficient * 10 ** power
    else:
        divisor = 10 ** -power
        result, remainder = divmod(coefficient, divisor)
        if 2 * remainder > divisor or 2 * remainder == divisor and result % 2:
            result += 1
    if result > MAX_TIME_NS:
        raise ValueError("time overflows signed nanoseconds")
    return result


def action_to_wire(value):
    if isinstance(value, bool) or not isinstance(value, Integral) or not 0 <= value <= 5:
        raise ValueError("action must be an integer in [0,5]")
    return int(value) + 1


def _side(value):
    if value not in ("buy", "sell"):
        raise ValueError("side must be buy or sell")
    return pb.SIDE_BUY if value == "buy" else pb.SIDE_SELL


def episode_spec(events, plan=None, rules=None, fees=None, config=None):
    plan, rules = plan or Plan(), rules or InstrumentRules()
    fees, config = fees or FeeConfig(), config or ReplayConfig()
    spec = pb.EpisodeSpec(
        plan=pb.Plan(side=_side(plan.side), deadline_ns=seconds_to_ns(plan.deadline),
                     symbol=plan.symbol, **{name: encode_decimal(getattr(plan, name)) for name in
                     ("quantity", "limit_price", "max_child_quantity", "quote_budget", "base_balance")}),
        rules=pb.InstrumentRules(symbol=rules.symbol, **{
            name: encode_decimal(getattr(rules, name)) for name in
            ("tick_size", "lot_size", "min_quantity", "min_notional")}),
        fees=pb.FeeConfig(maker=encode_decimal(fees.maker), taker=encode_decimal(fees.taker),
                          currency=pb.FEE_BASE if fees.currency == "base" else pb.FEE_QUOTE,
                          source=fees.source),
        config=pb.ReplayConfig(lambda_miss=config.lambda_miss,
                              max_participation=encode_decimal(config.max_participation),
                              fill_model=config.fill_model, **{
            name + "_ns": seconds_to_ns(getattr(config, name)) for name in
            ("decision_interval", "submission_latency", "cancel_latency", "settlement_allowance",
             "min_resting_age", "min_message_interval")}),
    )
    kinds = {"snapshot": pb.EVENT_SNAPSHOT, "book": pb.EVENT_BOOK,
             "trade": pb.EVENT_TRADE, "gap": pb.EVENT_GAP}
    for index, raw in enumerate(events):
        kind = raw.get("kind", raw.get("type"))
        if kind not in kinds:
            raise ValueError(f"unsupported event kind {kind!r}")
        if "time_ns" in raw:
            timestamp = raw["time_ns"]
            if isinstance(timestamp, bool) or not isinstance(timestamp, int):
                raise ValueError("time_ns must be an integer")
            if not 0 <= timestamp <= MAX_TIME_NS:
                raise ValueError("time_ns out of range")
        else:
            timestamp = seconds_to_ns(raw.get("time", raw.get("ts", -1)))
        valid = raw.get("valid", not raw.get("invalid", False))
        if not isinstance(valid, bool):
            raise ValueError("event validity must be boolean")
        event = spec.events.add(receipt_time_ns=timestamp, kind=kinds[kind],
                                symbol=raw.get("symbol", plan.symbol),
                                receipt_index=raw.get("receipt_index", index), valid=valid,
                                trade_id=str(raw.get("trade_id", "")),
                                exchange_timestamp=str(raw.get("exchange_timestamp", "")),
                                receipt_session_id=str(raw.get("receipt_session_id", "")))
        for side in ("bids", "asks"):
            for level in raw.get(side, ()):
                price, quantity = ((level["price"], level["quantity"])
                                   if isinstance(level, dict) else level)
                getattr(event, side).add(price=encode_decimal(price),
                                         quantity=encode_decimal(quantity))
        if kind == "trade":
            event.price.CopyFrom(encode_decimal(raw.get("price", "0")))
            event.quantity.CopyFrom(encode_decimal(raw.get("quantity", "0")))
            # Invalid frames carry no fabricated trade semantics.
            if valid or raw.get("side") in ("buy", "sell"):
                event.aggressor_side = _side(raw.get("side"))
    return spec

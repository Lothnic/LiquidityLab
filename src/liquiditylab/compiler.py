"""Pure v0 six-action compiler. No policy can bypass these boundaries."""

import math

from .contracts import Action, CompileResult, D, OrderIntent, ceil_step, floor_step


def compile_action(action, *, plan, rules, fees, config, ledger, book, child, now):
    action = Action(action)
    if action == Action.HOLD:
        return CompileResult(reason="hold")
    if action == Action.CANCEL:
        return CompileResult(cancel=child is not None, reason="policy_cancel")
    if not book.valid or now >= plan.deadline:
        return CompileResult(reason="market_invalid_or_deadline")
    passive = action in (Action.PASSIVE_SMALL, Action.PASSIVE_NORMAL)
    small = action in (Action.PASSIVE_SMALL, Action.AGGRESSIVE_SMALL)
    if plan.side == "buy":
        price = min(book.best_bid if passive else book.best_ask + rules.tick_size, plan.limit_price)
        price = floor_step(price, rules.tick_size)
    else:
        price = max(book.best_ask if passive else book.best_bid - rules.tick_size, plan.limit_price)
        price = ceil_step(price, rules.tick_size)
    if price <= 0:
        return CompileResult(reason="invalid_tick_price")
    slices = math.ceil(plan.deadline / 15)
    quantity = plan.quantity / D(slices) / (D(2) if small else D(1))
    rate = max(fees.maker, fees.taker)  # reserve against the conservative configured fee
    if plan.side == "buy":
        balance_quantity = ledger.quote / (price * (1 + rate if fees.currency == "quote" else 1))
    else:
        balance_quantity = ledger.base / (1 + rate if fees.currency == "base" else 1)
    depth_side = "bids" if plan.side == "buy" and passive else "asks"
    if not passive:
        depth_side = "asks" if plan.side == "buy" else "bids"
    elif plan.side == "sell":
        depth_side = "asks"
    envelope = book.visible(depth_side) * config.max_participation
    quantity = floor_step(
        min(quantity, ledger.remaining, plan.max_child_quantity, balance_quantity, envelope),
        rules.lot_size,
    )
    if quantity < rules.min_quantity or quantity * price < rules.min_notional:
        return CompileResult(reason="below_minimum_or_no_free_balance")
    desired = OrderIntent(plan.side, quantity, price, passive)
    if child is not None:
        if child.state in ("submitting", "cancel_requested"):
            return CompileResult(reason="awaiting_order_final_state")
        if child.intent == desired:
            return CompileResult(reason="existing_order_matches")
        if now - child.arrival_at < config.min_resting_age:
            return CompileResult(reason="minimum_resting_age")
        return CompileResult(cancel=True, reason="cancel_before_replace")
    return CompileResult(intent=desired, reason="submit_passive" if passive else "submit_ioc")

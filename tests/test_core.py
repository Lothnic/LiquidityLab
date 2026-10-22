from decimal import Decimal as D

import pytest

from liquiditylab.accounting import Ledger
from liquiditylab.book import Book
from liquiditylab.contracts import FeeConfig, Fill, InstrumentRules, Plan, ReplayConfig
from liquiditylab.replay import Replay


def test_buy_quote_fee_updates_exact_spot_balances_and_cost():
    plan = Plan(side="buy", quantity="0.2", limit_price="100", quote_budget="50", base_balance="0")
    ledger = Ledger(plan)
    fill = Fill("e1", "o1", "buy", "0.1", "99.5", "0.0199", "quote", 1.0)
    assert ledger.apply(fill) is True
    assert (ledger.base, ledger.quote) == (D("0.1"), D("40.0301"))
    assert ledger.filled == D("0.1") and ledger.average_price == D("99.5")
    assert ledger.fees_quote == D("0.0199")
    assert ledger.marked_cost(D("100"), D("101")) == D("0.0699")


def test_sell_base_fee_updates_exact_spot_balances_and_cost():
    plan = Plan(side="sell", quantity="0.2", limit_price="100", quote_budget="0", base_balance="0.3")
    ledger = Ledger(plan)
    fill = Fill("e1", "o1", "sell", "0.1", "101", "0.0001", "base", 1.0)
    assert ledger.apply(fill) is True
    assert (ledger.base, ledger.quote) == (D("0.1999"), D("10.1"))
    assert ledger.fees_base == D("0.0001")
    assert ledger.marked_cost(D("100"), D("102")) == D("-0.2899")


def test_execution_application_is_idempotent_but_conflicts_are_rejected():
    ledger = Ledger(Plan())
    fill = Fill("same", "o1", "buy", "0.01", "100", "0", "quote", 1.0)
    assert ledger.apply(fill) is True
    before = (ledger.base, ledger.quote, ledger.filled, len(ledger.fills))
    assert ledger.apply(fill) is False
    assert (ledger.base, ledger.quote, ledger.filled, len(ledger.fills)) == before
    with pytest.raises(ValueError, match="conflicting duplicate"):
        ledger.apply(Fill("same", "o1", "buy", "0.01", "100", "0.01", "quote", 1.0))


@pytest.mark.parametrize("fill, message", [
    (Fill("e", "o", "buy", "0.2", "100", "0", "quote", 0), "exceeds"),
    (Fill("e", "o", "buy", "0.1", "102.01", "0", "quote", 0), "boundary"),
])
def test_ledger_rejects_obligation_and_price_boundary_violations(fill, message):
    with pytest.raises(ValueError, match=message):
        Ledger(Plan()).apply(fill)


def test_ledger_rejects_negative_spot_balances():
    ledger = Ledger(Plan(side="sell", quantity="0.1", limit_price="100", base_balance="0.05"))
    with pytest.raises(ValueError, match="negative balance"):
        ledger.apply(Fill("e", "o", "sell", "0.1", "101", "0", "quote", 0))


def _events(*tail):
    return [{"time": 0, "kind": "snapshot", "bids": [["99", "2"]],
             "asks": [["101", "2"]]}, *tail]


def _config(**overrides):
    values = dict(decision_interval=1, submission_latency=0, cancel_latency=0,
                  settlement_allowance=1, min_message_interval=0)
    values.update(overrides)
    return ReplayConfig(**values)


def _replay(events, *, side="buy", quantity="0.1", deadline=2, **kwargs):
    plan = Plan(side=side, quantity=quantity, limit_price="101" if side == "buy" else "99",
                max_child_quantity=quantity, deadline=deadline, quote_budget="1000", base_balance="1")
    return Replay(events, plan=plan, config=_config(), **kwargs)


def test_book_consumption_debt_survives_updates_until_explicit_removal():
    book = Book()
    book.apply(_events()[0])
    book.consume("asks", D("101"), D("1"))
    book.apply({"kind": "book", "asks": [["101", "2"]]})
    book.apply({"kind": "snapshot", "bids": [["99", "2"]], "asks": [["101", "2"]]})
    assert book.available("asks", D("101")) == D("1")
    book.apply({"kind": "book", "asks": [["101", "0"]]})
    book.apply({"kind": "snapshot", "bids": [["99", "2"]], "asks": [["101", "2"]]})
    assert book.available("asks", D("101")) == D("2")


def test_passive_order_waits_for_qualifying_trade_after_queue_ahead():
    events = _events(
        {"time": 1, "kind": "trade", "side": "sell", "price": "99", "quantity": "2", "trade_id": "q"},
        {"time": 2, "kind": "trade", "side": "sell", "price": "99", "quantity": "0.2", "trade_id": "f"},
        {"time": 3, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["101", "2"]]},
    )
    replay = _replay(events, quantity="0.1", deadline=2, fees=FeeConfig(maker="0", taker="0"))
    _, _, terminated, truncated, _ = replay.step(2)  # PASSIVE_SMALL, queue ahead is 2
    assert not truncated
    assert replay.ledger.filled == D("0")  # same-time queue print is before order arrival only if receipt wins
    # A subsequent qualifying print crosses the observed queue and fills passively.
    if not terminated:
        replay.step(0)
    assert replay.ledger.filled == D("0.05")
    assert replay.ledger.fills[0].maker is True


def test_invalid_gap_truncates_and_never_claims_economic_score():
    replay = _replay(_events(
        {"time": 1, "kind": "gap"},
        {"time": 3, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["101", "2"]]},
    ))
    _, _, terminated, truncated, info = replay.step(0)
    assert truncated and not terminated
    assert info["report"]["economic_score_valid"] is False


def test_deadline_settlement_and_reward_reconcile_to_report_objective():
    events = _events(
        {"time": 1, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["100", "2"]]},
        {"time": 2, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["100", "2"]]},
        {"time": 3, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["100", "2"]]},
    )
    replay = _replay(events, quantity="0.1", deadline=2, fees=FeeConfig(maker="0", taker="0"))
    _, reward, terminated, truncated, info = replay.step(5)
    assert terminated and not truncated
    report = info["report"]
    assert report["status"] == "completed"
    assert report["reward_sum"] == pytest.approx(reward)
    assert reward == pytest.approx(report["terminal_objective"])


def test_reset_reproduces_same_action_trace_and_observation():
    events = _events(
        {"time": 1, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["100", "2"]]},
        {"time": 2, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["100", "2"]]},
        {"time": 3, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["100", "2"]]},
    )
    replay = _replay(events, quantity="0.1", deadline=2, fees=FeeConfig(maker="0", taker="0"))
    first = replay.step(5)
    replay.reset()
    second = replay.step(5)
    assert (first[1:4]) == (second[1:4])
    assert (first[0] == second[0]).all()
    assert first[4]["report"] == second[4]["report"]


def test_repeated_ioc_cannot_reuse_same_size_ask_update_but_remove_readd_can():
    events = _events(
        {"time": 1, "kind": "book", "asks": [["101", "0.1"]]},
        {"time": 1.5, "kind": "book", "asks": [["101", "0"]]},
        {"time": 1.6, "kind": "book", "asks": [["101", "0.1"]]},
        {"time": 4, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["101", "2"]]},
    )
    events[0]["asks"] = [["101", "0.1"], ["102", "2"]]
    plan = Plan(quantity="0.3", limit_price="101", max_child_quantity="0.1", deadline=3)
    replay = Replay(events, plan=plan, config=_config(max_participation=D("1")),
                    fees=FeeConfig(maker="0", taker="0"))
    replay.step(5)
    assert replay.ledger.filled == D("0.1")
    replay.step(5)
    assert replay.ledger.filled == D("0.1")  # a same-size update cannot replenish consumed liquidity
    assert len(replay.orders) == 2  # an IOC attempt is emitted but the consumed level stays unavailable
    replay.step(5)
    assert replay.ledger.filled == D("0.2")  # explicit remove/re-add permits fresh displayed size
    assert len(replay.orders) == 3


def test_observations_do_not_include_future_book_events():
    def tape(future_ask):
        return _events(
            {"time": 1, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["101", "2"]]},
            {"time": 2, "kind": "snapshot", "bids": [["99", "2"]], "asks": [[future_ask, "2"]]},
            {"time": 4, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["101", "2"]]},
        )
    first = _replay(tape("101"), deadline=3)
    second = _replay(tape("120"), deadline=3)
    assert (first.observation() == second.observation()).all()
    first_t1 = first.step(0)[0]
    second_t1 = second.step(0)[0]
    assert (first_t1 == second_t1).all()
    assert first.book.best_ask == second.book.best_ask == D("101")


def test_settlement_cancel_race_fills_once_freezes_mid_and_ignores_later_gap():
    events = _events(
        {"time": 1.5, "kind": "trade", "side": "sell", "price": "99", "quantity": "2.1", "trade_id": "race"},
        {"time": 1.75, "kind": "snapshot", "bids": [["109", "2"]], "asks": [["111", "2"]]},
        {"time": 1.8, "kind": "gap"},
        {"time": 2, "kind": "snapshot", "bids": [["109", "2"]], "asks": [["111", "2"]]},
    )
    plan = Plan(quantity="0.2", limit_price="99", max_child_quantity="0.2", deadline=1)
    config = _config(cancel_latency=0.5, settlement_allowance=1)
    replay = Replay(events, plan=plan, config=config, fees=FeeConfig(maker="0", taker="0"))
    _, _, terminated, truncated, info = replay.step(3)
    assert terminated and not truncated
    assert replay.ledger.filled == D("0.1")
    assert len(replay.ledger.fills) == 1
    assert info["report"]["residual_midpoint"] == "100"
    assert info["report"]["economic_score_valid"] is True


def test_cancel_latency_beyond_allowance_truncates_without_terminal_score():
    events = _events(
        {"time": 1.5, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["101", "2"]]},
        {"time": 2, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["101", "2"]]},
    )
    plan = Plan(quantity="0.2", limit_price="99", deadline=1, max_child_quantity="0.2")
    replay = Replay(events, plan=plan, config=_config(cancel_latency=0.5, settlement_allowance=0.25))
    _, _, terminated, truncated, info = replay.step(3)
    assert truncated and not terminated
    assert info["report"]["economic_score_valid"] is False
    assert info["report"]["terminal_objective"] is None


def test_compiler_floors_below_minimum_instead_of_rounding_up():
    events = _events({"time": 3, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["101", "2"]]})
    plan = Plan(quantity="0.15", limit_price="99", max_child_quantity="0.15", deadline=2)
    rules = InstrumentRules(lot_size="0.1", min_quantity="0.1", min_notional="0.01")
    replay = Replay(events, plan=plan, rules=rules, config=_config())
    replay.step(2)  # half-size passive slice is 0.075 and floors to zero
    assert replay.orders == []
    assert replay.reason == "below_minimum_or_no_free_balance"


def test_buy_quote_fee_reserve_and_sell_base_fee_cap_bound_order_size():
    events = _events({"time": 3, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["100", "2"]]})
    buy_fees = FeeConfig(maker="0.01", taker="0.01", currency="quote")
    buy_plan = Plan(quantity="0.1", limit_price="101", max_child_quantity="0.1",
                    quote_budget="10.05", base_balance="0", deadline=2)
    buy = Replay(events, plan=buy_plan, fees=buy_fees, config=_config())
    buy.step(5)
    buy_size = buy.orders[0].intent.quantity
    assert buy_size * D("100.01") * D("1.01") <= D("10.05")

    sell_fees = FeeConfig(maker="0.01", taker="0.01", currency="base")
    sell_plan = Plan(side="sell", quantity="0.1", limit_price="99", max_child_quantity="0.1",
                     quote_budget="0", base_balance="0.1005", deadline=2)
    sell = Replay(events, plan=sell_plan, fees=sell_fees, config=_config())
    sell.step(5)
    sell_size = sell.orders[0].intent.quantity
    assert sell_size * D("1.01") <= D("0.1005")


def test_passive_replacement_waits_until_minimum_resting_age():
    events = _events(
        {"time": 1, "kind": "snapshot", "bids": [["98", "2"]], "asks": [["101", "2"]]},
        {"time": 5, "kind": "snapshot", "bids": [["98", "2"]], "asks": [["101", "2"]]},
    )
    plan = Plan(quantity="0.2", limit_price="100", max_child_quantity="0.2", deadline=4)
    replay = Replay(events, plan=plan, config=_config(min_resting_age=1.5))
    replay.step(3)
    first_order = replay.child.order_id
    replay.step(3)
    assert replay.reason == "minimum_resting_age"
    assert replay.child.order_id == first_order
    assert len(replay.orders) == 1
    replay.step(3)
    assert replay.reason == "cancel_before_replace"
    assert len(replay.orders) == 1


def test_completion_inside_decision_interval_stops_before_later_gap():
    events = _events(
        {"time": 0.5, "kind": "gap"},
        {"time": 3, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["101", "2"]]},
    )
    plan = Plan(quantity="0.05", limit_price="101", max_child_quantity="0.05", deadline=2)
    replay = Replay(events, plan=plan, config=_config(submission_latency=0.05),
                    fees=FeeConfig(maker="0", taker="0"))
    _, _, terminated, truncated, info = replay.step(5)
    assert terminated and not truncated
    assert replay.now == pytest.approx(0.05)
    assert replay.ledger.filled == D("0.05")
    assert info["report"]["economic_score_valid"] is True
    assert info["report"]["status"] == "completed"


def test_buy_limit_below_tick_returns_noop_instead_of_zero_price_order():
    events = [{"time": 0, "kind": "snapshot", "bids": [["0.005", "2"]],
               "asks": [["0.02", "2"]]},
              {"time": 3, "kind": "snapshot", "bids": [["0.005", "2"]],
               "asks": [["0.02", "2"]]}]
    plan = Plan(quantity="0.1", limit_price="0.005", max_child_quantity="0.1", deadline=2)
    replay = Replay(events, plan=plan, config=_config())
    replay.step(3)
    assert replay.orders == []
    assert replay.reason == "invalid_tick_price"

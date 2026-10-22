"""Regenerate Python-reference golden vectors; run from the liquiditylab root.

Usage: uv run python conformance/regenerate.py
All fixture times use integer nanoseconds; report time fields are normalized too.
"""

import json
from pathlib import Path

from liquiditylab.contracts import FeeConfig, Plan, ReplayConfig
from liquiditylab.policies import run_policy
from liquiditylab.replay import Replay, synthetic_events


ROOT = Path(__file__).resolve().parent


def ns_events(events):
    return [
        {**{key: value for key, value in event.items() if key != "time"},
         "time_ns": int(round(event["time"] * 1_000_000_000))}
        for event in events
    ]


def ns_report(report):
    result = json.loads(json.dumps(report))
    result["time_ns"] = int(round(result.pop("time") * 1_000_000_000))
    for decision in result["decisions"]:
        decision["time_ns"] = int(round(decision.pop("time") * 1_000_000_000))
    for order in result["orders"]:
        for key in ("submitted_at", "arrival_at", "cancel_at"):
            if order.get(key) is not None:
                order[key + "_ns"] = int(round(order.pop(key) * 1_000_000_000))
    for fill in result["fills"]:
        fill["time_ns"] = int(round(fill.pop("time") * 1_000_000_000))
    return result


def report_case(name, events, plan, *, actions=None, policy=None, config=None, fees=None):
    config = config or ReplayConfig(
        decision_interval=1, submission_latency=0, cancel_latency=0,
        settlement_allowance=1, min_message_interval=0,
    )
    fees = fees or FeeConfig(maker="0", taker="0")
    replay = Replay(events, plan=plan, config=config, fees=fees)
    if policy:
        report = run_policy(replay, policy)
    else:
        replay.reset()
        for action in actions:
            if replay.terminated or replay.truncated:
                break
            replay.step(action)
        report = replay.report()
    return {
        "name": name,
        "input": {
            "events": ns_events(events),
            "plan": json.loads(json.dumps(plan.__dict__, default=str)),
            "actions": actions,
            "policy": policy,
            "config": json.loads(json.dumps(config.__dict__, default=str)),
            "fees": json.loads(json.dumps(fees.__dict__, default=str)),
        },
        "expected_report": ns_report(report),
    }


def main():
    buy_plan = Plan(**json.loads((ROOT.parent / "examples/buy-plan.json").read_text()))
    buy = report_case(
        "full_buy", synthetic_events(), buy_plan, policy="scheduled",
        config=ReplayConfig(), fees=FeeConfig(),
    )

    partial_events = [
        {"time": 0, "kind": "snapshot", "bids": [["99", "0.04"]], "asks": [["101", "2"]]},
        {"time": 1, "kind": "snapshot", "bids": [["99", "0.04"]], "asks": [["101", "2"]]},
        {"time": 2, "kind": "snapshot", "bids": [["99", "0.04"]], "asks": [["101", "2"]]},
        {"time": 3, "kind": "snapshot", "bids": [["99", "0.04"]], "asks": [["101", "2"]]},
    ]
    partial = report_case(
        "partial_sell", partial_events,
        Plan(side="sell", quantity="0.1", limit_price="99", max_child_quantity="0.1",
             deadline=2, quote_budget="0", base_balance="0.1"),
        actions=[5, 0], config=ReplayConfig(decision_interval=1, submission_latency=0,
            cancel_latency=0, settlement_allowance=1, min_message_interval=0,
            max_participation="1"),
    )

    race_events = [
        {"time": 0, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["101", "2"]]},
        {"time": 1.5, "kind": "trade", "side": "sell", "price": "99", "quantity": "2.1", "trade_id": "race"},
        {"time": 1.75, "kind": "snapshot", "bids": [["109", "2"]], "asks": [["111", "2"]]},
        {"time": 1.8, "kind": "gap"},
        {"time": 2, "kind": "snapshot", "bids": [["109", "2"]], "asks": [["111", "2"]]},
    ]
    race = report_case(
        "cancel_race_frozen_reference_late_gap", race_events,
        Plan(quantity="0.2", limit_price="99", max_child_quantity="0.2", deadline=1),
        actions=[3], config=ReplayConfig(decision_interval=1, submission_latency=0,
            cancel_latency=0.5, settlement_allowance=1, min_message_interval=0),
    )

    gap_events = [
        {"time": 0, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["101", "2"]]},
        {"time": 1, "kind": "gap"},
        {"time": 3, "kind": "snapshot", "bids": [["99", "2"]], "asks": [["101", "2"]]},
    ]
    gap = report_case(
        "feed_gap_invalidates_episode", gap_events,
        Plan(quantity="0.1", limit_price="101", max_child_quantity="0.1", deadline=2,
             quote_budget="1000", base_balance="1"),
        actions=[0],
    )

    (ROOT / "golden.json").write_text(json.dumps({
        "schema": "liquiditylab-python-reference-v1",
        "time_unit": "nanoseconds",
        "decimal_encoding": "base-10 strings",
        "action_mapping": {"0": "HOLD", "1": "CANCEL", "2": "PASSIVE_SMALL",
            "3": "PASSIVE_NORMAL", "4": "AGGRESSIVE_SMALL", "5": "AGGRESSIVE_NORMAL"},
        "cases": [buy, partial, race, gap],
    }, indent=2) + "\n")


if __name__ == "__main__":
    main()

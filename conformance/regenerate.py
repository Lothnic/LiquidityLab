"""Regenerate Python-reference golden vectors; run from the liquiditylab root.

Usage: uv run python conformance/regenerate.py
All fixture times use integer nanoseconds; report time fields are normalized too.
"""

import json
from decimal import ROUND_HALF_EVEN, Decimal, localcontext
from pathlib import Path

from liquiditylab.contracts import FeeConfig, Plan, ReplayConfig
from liquiditylab.policies import LiquidityAwarePolicy, ScheduledPolicy
from liquiditylab.replay import (
    OBSERVATION_FIELDS,
    OBSERVATION_VERSION,
    Replay,
    synthetic_events,
)

ROOT = Path(__file__).resolve().parent


def ns_events(events):
    return [
        {**{key: value for key, value in event.items() if key != "time"},
         "time_ns": ns(event["time"])}
        for event in events
    ]


def ns(value):
    """Convert decimal-written seconds to nearest integer ns, ties to even."""
    with localcontext() as context:
        context.prec = 50
        scaled = Decimal(str(value)) * Decimal(1_000_000_000)
        return int(scaled.to_integral_value(rounding=ROUND_HALF_EVEN))


def ns_report(report):
    result = json.loads(json.dumps(report))
    result["time_ns"] = ns(result.pop("time"))
    for decision in result["decisions"]:
        decision["time_ns"] = ns(decision.pop("time"))
    for order in result["orders"]:
        for key in ("submitted_at", "arrival_at", "cancel_at"):
            if order.get(key) is not None:
                order[key + "_ns"] = ns(order.pop(key))
    for fill in result["fills"]:
        fill["time_ns"] = ns(fill.pop("time"))
    return result


def report_case(name, events, plan, *, actions=None, policy=None, config=None, fees=None):
    config = config or ReplayConfig(
        decision_interval=1, submission_latency=0, cancel_latency=0,
        settlement_allowance=1, min_message_interval=0,
    )
    fees = fees or FeeConfig(maker="0", taker="0")
    replay = Replay(events, plan=plan, config=config, fees=fees)
    observation, _ = replay.reset()
    expected_observations = {
        "schema_version": OBSERVATION_VERSION,
        "field_names": list(OBSERVATION_FIELDS),
        "reset": [float(value) for value in observation],
        "decisions": [],
    }
    policy_impl = None
    if policy == "scheduled":
        policy_impl = ScheduledPolicy()
    elif policy == "liquidity-aware":
        policy_impl = LiquidityAwarePolicy()
    if policy_impl is not None:
        while not (replay.terminated or replay.truncated):
            action = policy_impl.act(replay)
            observation, _, _, _, _ = replay.step(action)
            expected_observations["decisions"].append({
                "observation": [float(value) for value in observation],
            })
        report = replay.report()
        report["policy"] = policy
    else:
        for action in actions:
            if replay.terminated or replay.truncated:
                break
            observation, _, _, _, _ = replay.step(action)
            expected_observations["decisions"].append({
                "observation": [float(value) for value in observation],
            })
        report = replay.report()
    expected_observations["decisions"] = [
        {"action": decision["action"], **vector}
        for decision, vector in zip(
            report["decisions"], expected_observations["decisions"], strict=True
        )
    ]
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
        "expected_observations": expected_observations,
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

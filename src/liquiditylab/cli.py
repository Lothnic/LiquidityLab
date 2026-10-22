"""Local execution-environment CLI. No command submits a real order."""

import argparse
import asyncio
import json
from dataclasses import replace
from pathlib import Path

from .contracts import FeeConfig, InstrumentRules, Plan, ReplayConfig
from .policies import run_policy
from .replay import Replay, synthetic_events


def load_events(path):
    rows = [json.loads(line) for line in Path(path).read_text().splitlines() if line.strip()]
    if rows and ("kind" in rows[0] or "type" in rows[0]) and ("time" in rows[0] or "ts" in rows[0]):
        return rows
    from .data.kraken import normalize_recording

    return normalize_recording(Path(path))


def main(argv=None):
    parser = argparse.ArgumentParser(description="LiquidityLab public data and simulated replay")
    subs = parser.add_subparsers(dest="command", required=True)
    demo = subs.add_parser("demo", help="run a deterministic synthetic episode")
    demo.add_argument("--side", choices=["buy", "sell"], default="buy")
    replay = subs.add_parser("replay", help="replay canonical JSONL or raw Kraken capture")
    replay.add_argument("tape", type=Path)
    replay.add_argument("--plan", type=Path)
    replay.add_argument("--rules", type=Path, help="instrument rules JSON; defaults are synthetic")
    replay.add_argument("--fees", type=Path, help="fee configuration JSON; defaults are synthetic")
    replay.add_argument("--horizon", type=float)
    replay.add_argument("--settlement", type=float, default=30.0)
    for command in (demo, replay):
        command.add_argument(
            "--policy", choices=["scheduled", "liquidity-aware"], default="scheduled"
        )
        command.add_argument("--output", type=Path)
    capture = subs.add_parser("capture", help="record Kraken public book and trades")
    capture.add_argument("--symbol", default="BTC/USD")
    capture.add_argument("--duration", type=float, default=60)
    capture.add_argument("--depth", type=int, default=10)
    capture.add_argument("--output", type=Path, required=True)
    normalize = subs.add_parser("normalize", help="convert raw capture to replay JSONL")
    normalize.add_argument("tape", type=Path)
    normalize.add_argument("--output", type=Path, required=True)
    inspect = subs.add_parser("inspect", help="summarize a recording without simulating")
    inspect.add_argument("tape", type=Path)
    args = parser.parse_args(argv)
    try:
        if args.command == "capture":
            from .data.kraken import capture as capture_public

            result = asyncio.run(
                capture_public(args.symbol, args.output, args.duration, args.depth)
            )
            if result.get("book_messages", 0) == 0:
                print(json.dumps(result, indent=2))
                parser.exit(
                    1,
                    "liquiditylab: capture produced no valid book messages; inspect its manifest\n",
                )
        elif args.command in ("normalize", "inspect"):
            events = load_events(args.tape)
            if args.command == "normalize":
                args.output.parent.mkdir(parents=True, exist_ok=True)
                args.output.write_text("".join(json.dumps(event) + "\n" for event in events))
            result = {
                "events": len(events),
                "duration": events[-1]["time"] if events else 0,
                "gaps": sum(e["kind"] == "gap" for e in events),
                "output": str(args.output) if args.command == "normalize" else None,
            }
        else:
            if args.command == "demo":
                plan = Plan(side=args.side, limit_price="102" if args.side == "buy" else "101")
                events, config = synthetic_events(), ReplayConfig()
                rules, fees = InstrumentRules(), FeeConfig()
            else:
                plan = Plan(**json.loads(args.plan.read_text())) if args.plan else Plan()
                if args.horizon is not None:
                    plan = replace(plan, deadline=args.horizon)
                events = load_events(args.tape)
                config = ReplayConfig(settlement_allowance=args.settlement)
                rules = (
                    InstrumentRules(**json.loads(args.rules.read_text()))
                    if args.rules
                    else InstrumentRules()
                )
                fees = FeeConfig(**json.loads(args.fees.read_text())) if args.fees else FeeConfig()
            result = run_policy(
                Replay(events, plan=plan, rules=rules, fees=fees, config=config), args.policy
            )
            if args.output:
                args.output.parent.mkdir(parents=True, exist_ok=True)
                args.output.write_text(json.dumps(result, indent=2) + "\n")
        print(json.dumps(result, indent=2))
    except (ValueError, OSError, RuntimeError, json.JSONDecodeError) as exc:
        parser.exit(2, f"liquiditylab: {exc}\n")


if __name__ == "__main__":
    main()

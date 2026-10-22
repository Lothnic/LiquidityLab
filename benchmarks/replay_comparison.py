#!/usr/bin/env python3
"""Bounded, deterministic Python-reference vs Go replay benchmark.

This measures the public Python client and Go process plus gRPC RPC path. It is
not a measurement of pure Go core time or of end-to-end training overhead.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.metadata
import json
import os
import platform
import re
import subprocess
import sys
import time
from decimal import Decimal, InvalidOperation
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from liquiditylab.contracts import Plan, ReplayConfig  # noqa: E402
from liquiditylab.go_backend import GoReplayBatch  # noqa: E402
from liquiditylab.replay import Replay, synthetic_events  # noqa: E402


def percentile(values: list[float], pct: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    rank = (len(ordered) - 1) * pct
    lo, hi = int(rank), min(int(rank) + 1, len(ordered) - 1)
    return ordered[lo] + (ordered[hi] - ordered[lo]) * (rank - lo)


def latency_summary(values: list[float]) -> dict[str, float | None]:
    return {
        "count": len(values),
        "p50_ms": percentile(values, 0.50) * 1000 if values else None,
        "p95_ms": percentile(values, 0.95) * 1000 if values else None,
        "p99_ms": percentile(values, 0.99) * 1000 if values else None,
    }


def cpu_model() -> str | None:
    try:
        for line in Path("/proc/cpuinfo").read_text().splitlines():
            if line.lower().startswith(("model name", "hardware")) and ":" in line:
                return line.split(":", 1)[1].strip()
    except OSError:
        pass
    return platform.processor() or None


def projection(report: dict) -> dict:
    """Exact semantic outcome; serialization-specific hashes/scale are omitted."""

    def numeric(value):
        try:
            result = Decimal(str(value))
        except (InvalidOperation, ValueError):
            return value
        if not result.is_finite():
            return value
        return format(result.normalize(), "f")

    def canonical(value):
        if isinstance(value, dict):
            return {key: canonical(item) for key, item in sorted(value.items())}
        if isinstance(value, list):
            return [canonical(item) for item in value]
        if isinstance(value, str) and re.fullmatch(
            r"[-+]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][-+]?\d+)?", value
        ):
            return numeric(value)
        return value

    keys = (
        "status",
        "terminated",
        "truncated",
        "economic_score_valid",
        "events_processed",
        "filled_quantity",
        "remaining_quantity",
        "marked_cost_quote",
        "terminal_objective",
        "fills",
    )
    return {key: canonical(report.get(key)) for key in keys}


def run_python(
    env_count: int, episodes_per_env: int, events: list[dict], plan: Plan, config: ReplayConfig
) -> dict:
    init_start = time.perf_counter()
    envs = [Replay(events, plan=plan, config=config) for _ in range(env_count)]
    construct_seconds = time.perf_counter() - init_start
    reset_start = time.perf_counter()
    for env in envs:
        env.reset()
    initial_reset_seconds = time.perf_counter() - reset_start
    init_seconds = construct_seconds + initial_reset_seconds
    step_latencies: list[float] = []
    reset_latencies: list[float] = []
    outcomes: list[dict] = []
    episode_counts = [0] * env_count
    wall_start = time.perf_counter()
    while min(episode_counts) < episodes_per_env:
        for index, env in enumerate(envs):
            if episode_counts[index] >= episodes_per_env:
                continue
            start = time.perf_counter()
            _, _, terminated, truncated, info = env.step(0)
            step_latencies.append(time.perf_counter() - start)
            if terminated or truncated:
                report = info["report"]
                outcomes.append(projection(report))
                episode_counts[index] += 1
                if episode_counts[index] < episodes_per_env:
                    start = time.perf_counter()
                    env.reset()
                    reset_latencies.append(time.perf_counter() - start)
    wall_seconds = time.perf_counter() - wall_start
    return {
        "backend": "python-reference",
        "batch_size": 1,
        "scalar_envs": env_count,
        "episodes_per_env": episodes_per_env,
        "initialization_seconds": init_seconds,
        "construct_or_upload_seconds": construct_seconds,
        "initial_reset_seconds": initial_reset_seconds,
        "step_call_latency": latency_summary(step_latencies),
        "reset_call_latency": latency_summary(reset_latencies),
        "env_transitions": len(step_latencies),
        "total_episodes": len(outcomes),
        "wall_seconds_including_report_validation": wall_seconds,
        "env_transitions_per_second": len(step_latencies) / wall_seconds,
        "outcomes": outcomes,
        "all_outcomes_valid": all(
            x["terminated"] and not x["truncated"] and x["economic_score_valid"] for x in outcomes
        ),
    }


def run_go(
    batch_size: int,
    episodes_per_env: int,
    events: list[dict],
    plan: Plan,
    config: ReplayConfig,
    binary: str,
) -> dict:
    init_start = time.perf_counter()
    batch = GoReplayBatch(events, plan=plan, config=config, count=batch_size, binary=binary)
    create_upload_seconds = time.perf_counter() - init_start
    reset_start = time.perf_counter()
    initial = batch.reset()
    initial_reset_seconds = time.perf_counter() - reset_start
    init_seconds = create_upload_seconds + initial_reset_seconds
    if len(initial) != batch_size or any(item[2] or item[3] for item in initial):
        batch.close()
        raise RuntimeError("Go initial reset did not produce one valid live state per environment")

    step_latencies: list[float] = []
    reset_latencies: list[float] = []
    outcomes: list[dict] = []
    episode_counts = [0] * batch_size
    active = list(range(batch_size))
    wall_start = time.perf_counter()
    try:
        while min(episode_counts) < episodes_per_env:
            start = time.perf_counter()
            transitions = batch.step([0] * len(active), indices=active)
            step_latencies.append(time.perf_counter() - start)
            ended = []
            for index, item in zip(active, transitions, strict=True):
                if item[3] or (item[2] and not item[4]["economic_score_valid"]):
                    raise RuntimeError(f"Go produced invalid terminal transition for env {index}")
                if item[2]:
                    ended.append(index)
            if ended:
                reports = batch.reports(ended)
                for report in reports:
                    outcomes.append(projection(report))
                for index in ended:
                    episode_counts[index] += 1
                reset_indices = [i for i in ended if episode_counts[i] < episodes_per_env]
                active = [i for i in range(batch_size) if episode_counts[i] < episodes_per_env]
                if reset_indices:
                    start = time.perf_counter()
                    resets = batch.reset(indices=reset_indices)
                    reset_latencies.append(time.perf_counter() - start)
                    if any(item[2] or item[3] for item in resets):
                        raise RuntimeError("Go reset returned terminal/truncated state")
        wall_seconds = time.perf_counter() - wall_start
    finally:
        batch.close()
    expected = batch_size * episodes_per_env
    return {
        "backend": "go-process-plus-grpc-rpc",
        "batch_size": batch_size,
        "episodes_per_env": episodes_per_env,
        "initialization_seconds": init_seconds,
        "process_create_upload_seconds": create_upload_seconds,
        "initial_reset_rpc_seconds": initial_reset_seconds,
        "step_rpc_latency": latency_summary(step_latencies),
        "step_rpc_latency_per_env_transition": latency_summary(
            [value / batch_size for value in step_latencies]
        ),
        "reset_rpc_latency": latency_summary(reset_latencies),
        "env_transitions": batch_size * episodes_per_env * 30,
        "total_episodes": len(outcomes),
        "wall_seconds_including_report_validation": wall_seconds,
        "env_transitions_per_second": batch_size * episodes_per_env * 30 / wall_seconds,
        "outcomes": outcomes,
        "all_outcomes_valid": len(outcomes) == expected
        and all(
            x["terminated"] and not x["truncated"] and x["economic_score_valid"] for x in outcomes
        ),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, help="explicit liquiditylabd executable")
    parser.add_argument("--episodes-per-env", type=int, default=2)
    parser.add_argument(
        "--repetitions", type=int, default=1, help="complete timed passes; bounded to 1..3"
    )
    parser.add_argument(
        "--max-seconds", type=float, default=50.0, help="hard wall budget, at most 55 seconds"
    )
    parser.add_argument(
        "--output", type=Path, default=ROOT / "benchmarks/results/reference-host.json"
    )
    args = parser.parse_args()
    if not 1 <= args.episodes_per_env <= 4 or not 1 <= args.repetitions <= 3:
        parser.error("episodes-per-env must be 1..4 and repetitions 1..3")
    if not 1 <= args.max_seconds <= 55:
        parser.error("max-seconds must be in [1,55]")
    binary = str(Path(args.binary).resolve(strict=True))
    if not os.access(binary, os.X_OK):
        parser.error("binary must be executable")

    events = synthetic_events()
    plan, config = Plan(deadline=30.0), ReplayConfig()
    tape_bytes = json.dumps(events, sort_keys=True, separators=(",", ":")).encode()
    tape_sha = hashlib.sha256(tape_bytes).hexdigest()
    run_start = time.perf_counter()

    # Warm each implementation and each RPC batch size. Warm data are excluded.
    for count in (1, 8, 32):
        warm = GoReplayBatch(events, plan=plan, config=config, count=count, binary=binary)
        try:
            warm.reset()
            for _ in range(30):
                transitions = warm.step([0] * count)
            if not all(item[2] and item[4]["economic_score_valid"] for item in transitions):
                raise RuntimeError("Go warm-up episode was not economically valid")
        finally:
            warm.close()
    reference = Replay(events, plan=plan, config=config)
    reference.reset()
    for _ in range(30):
        _, _, terminated, truncated, info = reference.step(0)
    if not terminated or truncated or not info["report"]["economic_score_valid"]:
        raise RuntimeError("Python warm-up episode was not economically valid")

    passes = []
    for repetition in range(args.repetitions):
        for count in (1, 8, 32):
            if time.perf_counter() - run_start >= args.max_seconds:
                raise TimeoutError("benchmark exceeded the configured wall budget")
            py = run_python(count, args.episodes_per_env, events, plan, config)
            go = run_go(count, args.episodes_per_env, events, plan, config, binary)
            parity = py["outcomes"] == go["outcomes"]
            passes.append(
                {
                    "repetition": repetition + 1,
                    "env_count": count,
                    "python_reference": py,
                    "go_batch": go,
                    "exact_terminal_outcome_parity": parity,
                }
            )
            if not py["all_outcomes_valid"] or not go["all_outcomes_valid"] or not parity:
                raise RuntimeError(f"invalid or non-equivalent outcomes at batch size {count}")

    result = {
        "schema": "liquiditylab-replay-benchmark-v1",
        "measurement_scope": (
            "Python reference vs Go process plus gRPC RPC. RPC wall time includes client, "
            "serialization, transport, server dispatch, and replay core; it is not pure core "
            "throughput and does not establish training-overhead percentage."
        ),
        "host": {
            "platform": platform.platform(),
            "os": platform.system(),
            "release": platform.release(),
            "machine": platform.machine(),
            "processor": cpu_model(),
            "cpu_count_logical": os.cpu_count(),
            "python": sys.version,
            "go": subprocess.run(
                ["go", "version"], check=True, capture_output=True, text=True, timeout=5
            ).stdout.strip(),
            "packages": {
                name: importlib.metadata.version(name)
                for name in ("liquiditylab", "numpy", "gymnasium", "grpcio", "protobuf")
            },
        },
        "binary": {
            "path": binary,
            "sha256": hashlib.sha256(Path(binary).read_bytes()).hexdigest(),
            "size_bytes": Path(binary).stat().st_size,
        },
        "tape": {
            "source": "liquiditylab.replay.synthetic_events",
            "sha256_canonical_json": tape_sha,
            "events": len(events),
            "first_event": events[0],
            "last_event": events[-1],
            "plan": {
                "deadline_seconds": plan.deadline,
                "quantity": str(plan.quantity),
                "limit_price": str(plan.limit_price),
                "symbol": plan.symbol,
            },
            "replay_config": {
                "decision_interval_seconds": config.decision_interval,
                "settlement_allowance_seconds": config.settlement_allowance,
                "fill_model": config.fill_model,
            },
            "action_stream": "fixed policy action 0 (HOLD); Go wire action 1",
            "expected_decisions_per_episode": 30,
        },
        "repetitions": args.repetitions,
        "episodes_per_env_per_pass": args.episodes_per_env,
        "batch_sizes": [1, 8, 32],
        "max_wall_seconds": args.max_seconds,
        "elapsed_seconds": time.perf_counter() - run_start,
        "passes": passes,
        "validity": {
            "all_python_outcomes_valid": all(
                p["python_reference"]["all_outcomes_valid"] for p in passes
            ),
            "all_go_outcomes_valid": all(p["go_batch"]["all_outcomes_valid"] for p in passes),
            "exact_terminal_outcome_parity": all(
                p["exact_terminal_outcome_parity"] for p in passes
            ),
        },
    }
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(result, indent=2, sort_keys=True) + "\n")
    print(
        json.dumps(
            {
                "output": str(args.output),
                "elapsed_seconds": result["elapsed_seconds"],
                "validity": result["validity"],
            },
            indent=2,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

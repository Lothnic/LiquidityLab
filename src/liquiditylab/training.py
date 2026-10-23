"""Optional offline PPO training; every generated bundle starts unvalidated.

Run ``python -m liquiditylab.training --help``. PyTorch/SB3 are imported only
when training is requested, leaving replay and recording lightweight.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from dataclasses import asdict
from datetime import UTC, datetime
from importlib.metadata import version
from pathlib import Path

import gymnasium as gym

from .contracts import FeeConfig, InstrumentRules, Plan, ReplayConfig
from .env import LiquidityEnv
from .replay import OBSERVATION_FIELDS, OBSERVATION_SIZE, OBSERVATION_VERSION, serializable


class InvalidTrainingEpisode(RuntimeError):
    """An unresolved trajectory must not enter ordinary PPO bootstrapping."""


class TrainingGuard(gym.Wrapper):
    """Abort before the learner can consume an invalid truncated transition."""

    def step(self, action):
        transition = self.env.step(action)
        _, _, _, truncated, info = transition
        if truncated:
            reason = info.get("status", info.get("reason", "unknown"))
            raise InvalidTrainingEpisode(
                f"Invalid replay episode ({reason}); training halted. "
                "Reconcile the recording/simulator before retrying."
            )
        return transition


def _digest(path: Path) -> str:
    result = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            result.update(chunk)
    return result.hexdigest()


def _implementation_digest() -> str:
    result = hashlib.sha256()
    package = Path(__file__).parent
    for path in sorted(package.rglob("*.py")):
        result.update(str(path.relative_to(package)).encode())
        result.update(b"\0")
        result.update(path.read_bytes())
    return result.hexdigest()


def train_policy(
    tape_path: Path,
    plan_path: Path | None,
    output_dir: Path,
    total_timesteps: int = 2048,
    seed: int = 0,
    rules_path: Path | None = None,
    fees_path: Path | None = None,
    *,
    backend: str = "python-reference",
    replay_binary: Path | None = None,
    replay_endpoint: str | None = None,
    num_envs: int = 1,
    max_batch_operations: int = 100000,
) -> dict:
    """Train on an explicit tape and save an unvalidated, replay-only bundle.

    This entrypoint does not perform a holdout promotion evaluation, connect
    to a private account, or make a trained policy eligible for live trading.
    """
    if isinstance(total_timesteps, bool) or not isinstance(total_timesteps, int) or total_timesteps < 8:
        raise ValueError("total_timesteps must be an integer of at least 8")
    if isinstance(seed, bool) or not isinstance(seed, int) or seed < 0:
        raise ValueError("seed must be a nonnegative integer")
    if backend not in ("python-reference", "go"):
        raise ValueError("backend must be 'python-reference' or 'go'")
    if (isinstance(max_batch_operations, bool) or not isinstance(max_batch_operations, int)
            or not 1 <= max_batch_operations <= 1000000):
        raise ValueError("max_batch_operations must be in [1,1000000]")
    if isinstance(num_envs, bool) or not isinstance(num_envs, int) or not 1 <= num_envs <= 32:
        raise ValueError("num_envs must be an integer in [1,32]")
    if backend == "python-reference" and num_envs != 1:
        raise ValueError("num_envs greater than one is supported only by the Go backend")
    if backend == "python-reference" and (replay_binary is not None or replay_endpoint is not None):
        raise ValueError("replay binary and endpoint options require --backend go")
    if replay_binary is not None and replay_endpoint is not None:
        raise ValueError("select replay_binary or replay_endpoint, not both")
    if backend == "go" and replay_endpoint is not None:
        raise ValueError(
            "Go training through a remote endpoint has no trustworthy binary provenance; "
            "use an owned local --replay-binary to create a bundle"
        )
    tape_path, output_dir = Path(tape_path), Path(output_dir)
    if output_dir.exists():
        raise ValueError("Use a new bundle directory; existing bundles are immutable")
    from .cli import load_events

    events = load_events(tape_path)
    if any(event.get("kind", event.get("type")) == "gap" or not event.get("valid", True)
           for event in events):
        raise ValueError("Training tape contains gaps or invalid events; select a valid interval")
    plan = Plan(**json.loads(Path(plan_path).read_text())) if plan_path else Plan()
    rules = InstrumentRules(**json.loads(Path(rules_path).read_text())) if rules_path else InstrumentRules()
    fees = FeeConfig(**json.loads(Path(fees_path).read_text())) if fees_path else FeeConfig()
    config = ReplayConfig()
    implementation_digest = _implementation_digest()
    lockfile = Path(__file__).resolve().parents[2] / "uv.lock"
    # Validate the tape and contracts before importing a large optional runtime.
    binary_path = None
    binary_digest = None
    go_env_id = None
    if backend == "go":
        from .protocol import GO_ENV_ID as go_env_id

        binary_value = replay_binary or os.environ.get("LIQUIDITYLAB_REPLAY_BINARY")
        if not binary_value:
            raise ValueError("Go training requires --replay-binary; see docs/go-replay.md")
        binary_path = Path(binary_value).expanduser().resolve()
        if not binary_path.is_file() or not os.access(binary_path, os.X_OK):
            raise ValueError(f"replay executable unavailable: {binary_path}")
        binary_digest = _digest(binary_path)
        from .go_vector import make_go_vec_env

        env = make_go_vec_env(
            events,
            plan,
            rules,
            fees,
            config,
            num_envs=num_envs,
            binary=binary_path,
            max_operations=max_batch_operations,
        )
    else:
        env = TrainingGuard(LiquidityEnv(events, plan=plan, rules=rules, fees=fees, config=config))
    try:
        from stable_baselines3 import PPO
    except ImportError as exc:
        env.close()
        raise RuntimeError("PPO requires optional dependencies. Run: uv sync --extra rl") from exc

    rollout_steps = min(128, max(8, total_timesteps))
    batch_size = min(64, rollout_steps)
    try:
        model = PPO("MlpPolicy", env, seed=seed, gamma=1.0, n_steps=rollout_steps,
                    batch_size=batch_size, n_epochs=4, device="cpu", verbose=0)
        model.learn(total_timesteps=total_timesteps)
        if binary_path is not None and _digest(binary_path) != binary_digest:
            raise RuntimeError("Go replay binary changed during training; refusing bundle provenance")
        # Create only after training succeeds: failure cannot leave a usable bundle.
        output_dir.mkdir(parents=True, exist_ok=False)
        model.save(output_dir / "policy")
        manifest = {
            "schema": "liquiditylab.policy-bundle.v0",
            "created_at": datetime.now(UTC).isoformat(),
            "status": "unvalidated",
            "live_eligible": False,
            "evaluation": "No holdout evaluation or promotion has been performed",
            "algorithm": "PPO",
            "seed": seed,
            "requested_timesteps": total_timesteps,
            "actual_timesteps": model.num_timesteps,
            "backend": backend,
            "environment": go_env_id if backend == "go" else "LiquidityLab-SpotExecution-v0",
            "observation_version": OBSERVATION_VERSION,
            "observation_size": OBSERVATION_SIZE,
            "observation_fields": list(OBSERVATION_FIELDS),
            "action_version": "six-actions-v0",
            "compiler_version": "six-action-v1" if backend == "go" else "six-action-v0",
            "action_compatibility": {
                "policy_action_values": list(range(6)),
                "go_wire_enum_values": list(range(1, 7)),
                "mapping": "wire enum value = policy action value + 1",
            },
            "feature_normalization": "fixed scaling defined by observation version; no fitted scaler",
            "reward_version": "marked-cost-bps-with-terminal-residual-v0",
            "dataset_sha256": _digest(tape_path),
            "implementation_sha256": implementation_digest,
            "dependency_lock_sha256": _digest(lockfile) if lockfile.exists() else None,
            "policy_sha256": _digest(output_dir / "policy.zip"),
            "dataset": str(tape_path),
            "plan": serializable(asdict(plan)),
            "instrument_rules": serializable(asdict(rules)),
            "fees": serializable(asdict(fees)),
            "replay_config": serializable(asdict(config)),
            "dependencies": {name: version(name) for name in
                             (("liquiditylab", "gymnasium", "numpy", "stable-baselines3", "torch",
                               "grpcio", "protobuf") if backend == "go" else
                              ("liquiditylab", "gymnasium", "numpy", "stable-baselines3", "torch"))},
        }
        if backend == "go":
            manifest["go_replay"] = {
                "environment": go_env_id,
                "provenance": "owned-local-process-binary",
                "binary_path": str(binary_path),
                "binary_sha256": binary_digest,
                "num_envs": num_envs,
                "max_batch_operations": max_batch_operations,
            }
        (output_dir / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
        return manifest
    finally:
        env.close()


def main(argv=None):
    parser = argparse.ArgumentParser(description="Train an unvalidated offline execution policy")
    parser.add_argument("tape", type=Path)
    parser.add_argument("--plan", type=Path)
    parser.add_argument("--rules", type=Path)
    parser.add_argument("--fees", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--timesteps", type=int, default=2048)
    parser.add_argument("--seed", type=int, default=0)
    parser.add_argument("--backend", choices=["python-reference", "go"], default="python-reference")
    parser.add_argument("--replay-binary", type=Path, help="owned Go replay executable")
    parser.add_argument("--replay-endpoint", help="local Go replay endpoint (cannot publish without binary provenance)")
    parser.add_argument("--num-envs", type=int, default=1, help="parallel replay environments (Go backend)")
    parser.add_argument("--max-batch-operations", type=int, default=100000,
                        help="Go mutation identity budget (1..1000000; increases memory use)")
    args = parser.parse_args(argv)
    try:
        manifest = train_policy(
            args.tape,
            args.plan,
            args.output,
            args.timesteps,
            args.seed,
            args.rules,
            args.fees,
            backend=args.backend,
            replay_binary=args.replay_binary,
            replay_endpoint=args.replay_endpoint,
            num_envs=args.num_envs,
            max_batch_operations=args.max_batch_operations,
        )
    except (ValueError, OSError, RuntimeError) as exc:
        parser.exit(2, f"liquiditylab training: {exc}\n")
    print(json.dumps(manifest, indent=2))


if __name__ == "__main__":
    main()

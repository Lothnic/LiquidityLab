"""Optional offline PPO training; every generated bundle starts unvalidated.

Run ``python -m liquiditylab.training --help``. PyTorch/SB3 are imported only
when training is requested, leaving replay and recording lightweight.
"""

from __future__ import annotations

import argparse
import hashlib
import json
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
) -> dict:
    """Train on an explicit tape and save an unvalidated, replay-only bundle.

    This entrypoint does not perform a holdout promotion evaluation, connect
    to a private account, or make a trained policy eligible for live trading.
    """
    if isinstance(total_timesteps, bool) or not isinstance(total_timesteps, int) or total_timesteps < 8:
        raise ValueError("total_timesteps must be an integer of at least 8")
    if isinstance(seed, bool) or not isinstance(seed, int) or seed < 0:
        raise ValueError("seed must be a nonnegative integer")
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
    env = TrainingGuard(LiquidityEnv(events, plan=plan, rules=rules, fees=fees, config=config))
    try:
        from stable_baselines3 import PPO
    except ImportError as exc:
        env.close()
        raise RuntimeError("PPO requires optional dependencies. Run: uv sync --extra rl") from exc

    rollout_steps = min(128, max(8, total_timesteps))
    batch_size = min(64, rollout_steps)
    model = PPO("MlpPolicy", env, seed=seed, gamma=1.0, n_steps=rollout_steps,
                batch_size=batch_size, n_epochs=4, device="cpu", verbose=0)
    try:
        model.learn(total_timesteps=total_timesteps)
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
            "environment": "LiquidityLab-SpotExecution-v0",
            "observation_version": OBSERVATION_VERSION,
            "observation_size": OBSERVATION_SIZE,
            "observation_fields": list(OBSERVATION_FIELDS),
            "action_version": "six-actions-v0",
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
                             ("liquiditylab", "gymnasium", "numpy", "stable-baselines3", "torch")},
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
    args = parser.parse_args(argv)
    try:
        manifest = train_policy(args.tape, args.plan, args.output, args.timesteps, args.seed,
                                args.rules, args.fees)
    except (ValueError, OSError, RuntimeError) as exc:
        parser.exit(2, f"liquiditylab training: {exc}\n")
    print(json.dumps(manifest, indent=2))


if __name__ == "__main__":
    main()

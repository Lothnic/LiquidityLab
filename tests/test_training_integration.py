"""Optional CPU PPO roundtrip; skipped when the RL extra is not installed."""

import hashlib
import json

import numpy as np
import pytest

from liquiditylab import LiquidityEnv
from liquiditylab.replay import synthetic_events
from liquiditylab.training import train_policy


def test_cpu_policy_bundle_can_be_trained_and_reloaded(tmp_path):
    sb3 = pytest.importorskip("stable_baselines3")
    tape = tmp_path / "tape.jsonl"
    tape.write_text("".join(json.dumps(e) + "\n" for e in synthetic_events()))
    bundle = tmp_path / "bundle"
    manifest = train_policy(tape, None, bundle, total_timesteps=8, seed=7)
    assert manifest["status"] == "unvalidated"
    assert manifest["live_eligible"] is False
    assert manifest["actual_timesteps"] == 8
    assert manifest["policy_sha256"] == hashlib.sha256((bundle / "policy.zip").read_bytes()).hexdigest()
    assert len(manifest["implementation_sha256"]) == 64
    assert len(manifest["observation_fields"]) == manifest["observation_size"]
    policy = sb3.PPO.load(bundle / "policy.zip", device="cpu")
    env = LiquidityEnv()
    observation, _ = env.reset(seed=7)
    action, _ = policy.predict(observation, deterministic=True)
    observation, reward, _, _, _ = env.step(int(action))
    assert np.isfinite(observation).all() and np.isfinite(reward)
    env.close()

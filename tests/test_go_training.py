"""Go VecEnv safety checks and a binary-gated PPO integration roundtrip."""

import hashlib
import json
import os
from pathlib import Path

import numpy as np
import pytest

from liquiditylab.replay import OBSERVATION_SIZE, synthetic_events
from liquiditylab.training import InvalidTrainingEpisode, train_policy


def _require_sb3():
    pytest.importorskip("stable_baselines3")
    pytest.importorskip("grpc")
    pytest.importorskip("google.protobuf")


def test_go_vec_env_batches_steps_and_autoresets_with_terminal_observation(monkeypatch):
    _require_sb3()
    import liquiditylab.go_backend as backend
    from liquiditylab.go_vector import make_go_vec_env

    class FakeBatch:
        def __init__(self, events, *args, count, **kwargs):
            self.count = count
            self.step_calls = []
            self.reset_calls = []

        def reset(self, indices=None, seeds=None):
            indices = list(range(self.count)) if indices is None else list(indices)
            self.reset_calls.append((indices, list(seeds)))
            return [
                (np.full(OBSERVATION_SIZE, 10 + i, dtype=np.float32), 0.0, False, False,
                 {"status": "running", "economic_score_valid": False, "reset_id": len(self.reset_calls)})
                for i in indices
            ]

        def step(self, actions):
            self.step_calls.append(list(actions))
            return [
                (np.full(OBSERVATION_SIZE, 1, dtype=np.float32), 2.0, True, False,
                 {"status": "settled", "economic_score_valid": True}),
                (np.full(OBSERVATION_SIZE, 2, dtype=np.float32), 3.0, False, False,
                 {"status": "running", "economic_score_valid": False}),
            ]

        def close(self):
            pass

    monkeypatch.setattr(backend, "GoReplayBatch", FakeBatch)
    env = make_go_vec_env([], num_envs=2, binary="unused")
    try:
        initial = env.reset()
        assert initial.shape == (2, OBSERVATION_SIZE)
        env.step_async(np.asarray([0, 1], dtype=np.int64))
        observations, rewards, dones, infos = env.step_wait()
        assert env._batch.step_calls == [[0, 1]]
        assert env._batch.reset_calls[-1][0] == [0]
        assert np.all(observations[0] == 10)
        assert np.all(observations[1] == 2)
        assert rewards.tolist() == [2.0, 3.0]
        assert dones.tolist() == [True, False]
        assert np.all(infos[0]["terminal_observation"] == 1)
        assert infos[0]["TimeLimit.truncated"] is False
        assert env.reset_infos[0]["reset_id"] == 2
    finally:
        env.close()


def test_go_vec_env_aborts_truncated_transition_before_return(monkeypatch):
    _require_sb3()
    import liquiditylab.go_backend as backend
    from liquiditylab.go_vector import make_go_vec_env

    class TruncatedBatch:
        def __init__(self, events, *args, count, **kwargs):
            self.count = count

        def reset(self, indices=None, seeds=None):
            return [(np.zeros(OBSERVATION_SIZE, dtype=np.float32), 0.0, False, False, {})]

        def step(self, actions):
            return [(np.zeros(OBSERVATION_SIZE, dtype=np.float32), 0.0, False, True,
                     {"status": "unresolved_settlement", "economic_score_valid": False})]

        def close(self):
            pass

    monkeypatch.setattr(backend, "GoReplayBatch", TruncatedBatch)
    env = make_go_vec_env([], num_envs=1, binary="unused")
    try:
        env.reset()
        env.step_async(np.asarray([0], dtype=np.int64))
        with pytest.raises(InvalidTrainingEpisode, match="training halted before PPO consumed it"):
            env.step_wait()
    finally:
        env.close()


def test_go_ppo_bundle_roundtrip_when_binary_is_configured(tmp_path):
    binary = os.environ.get("LIQUIDITYLAB_REPLAY_BINARY")
    if not binary:
        pytest.skip("set LIQUIDITYLAB_REPLAY_BINARY to run the Go integration check")
    _require_sb3()
    import stable_baselines3 as sb3
    binary_path = Path(binary).expanduser().resolve()
    if not binary_path.is_file() or not os.access(binary_path, os.X_OK):
        pytest.skip(f"Go replay binary is unavailable: {binary_path}")

    tape = tmp_path / "tape.jsonl"
    tape.write_text("".join(json.dumps(event) + "\n" for event in synthetic_events()))
    bundle = tmp_path / "bundle"
    manifest = train_policy(
        tape,
        None,
        bundle,
        total_timesteps=16,
        seed=7,
        backend="go",
        replay_binary=binary_path,
        num_envs=2,
    )
    assert manifest["environment"] == "LiquidityLab-SpotExecution-v1"
    assert manifest["backend"] == "go"
    assert manifest["status"] == "unvalidated"
    assert manifest["live_eligible"] is False
    assert manifest["go_replay"]["provenance"] == "owned-local-process-binary"
    assert manifest["go_replay"]["binary_sha256"] == hashlib.sha256(binary_path.read_bytes()).hexdigest()
    assert manifest["action_compatibility"]["go_wire_enum_values"] == [1, 2, 3, 4, 5, 6]

    policy = sb3.PPO.load(bundle / "policy.zip", device="cpu")
    from liquiditylab.go_backend import GoLiquidityEnv

    env = GoLiquidityEnv(synthetic_events(), binary=binary_path)
    try:
        observation, _ = env.reset(seed=7)
        action, _ = policy.predict(observation, deterministic=True)
        observation, reward, _, truncated, _ = env.step(int(action))
        assert not truncated
        assert np.isfinite(observation).all() and np.isfinite(reward)
    finally:
        env.close()


def test_go_training_rejects_remote_endpoint_without_binary_provenance(tmp_path):
    with pytest.raises(ValueError, match="no trustworthy binary provenance"):
        train_policy(
            tmp_path / "unused.jsonl",
            None,
            tmp_path / "bundle",
            total_timesteps=8,
            backend="go",
            replay_endpoint="127.0.0.1:12345",
        )

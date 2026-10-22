"""The training adapter must refuse transitions with unresolved economics."""

import pytest

from liquiditylab.contracts import Action, Plan, ReplayConfig
from liquiditylab.env import LiquidityEnv
from liquiditylab.training import InvalidTrainingEpisode, TrainingGuard, train_policy


def tape(extra=()):
    return [{"time": 0, "kind": "snapshot", "bids": [["99", "1"]],
             "asks": [["101", "1"]]}, *extra,
            {"time": 40, "kind": "book", "bids": [], "asks": []}]


def test_gap_aborts_before_learner_receives_truncated_transition():
    env = TrainingGuard(LiquidityEnv(tape([{"time": 0.5, "kind": "gap"}]),
                                    plan=Plan(deadline=2)))
    env.reset(seed=7)
    with pytest.raises(InvalidTrainingEpisode, match="training halted"):
        env.step(Action.HOLD)


def test_unresolved_cancellation_aborts_training():
    env = TrainingGuard(LiquidityEnv(tape(), plan=Plan(deadline=1),
                                    config=ReplayConfig(cancel_latency=10, settlement_allowance=0.1)))
    env.reset(seed=7)
    with pytest.raises(InvalidTrainingEpisode, match="unresolved_settlement"):
        env.step(Action.PASSIVE_NORMAL)


def test_known_partial_completion_remains_a_valid_training_outcome():
    env = TrainingGuard(LiquidityEnv(tape(), plan=Plan(deadline=1)))
    env.reset(seed=7)
    _, reward, terminated, truncated, _ = env.step(Action.HOLD)
    assert terminated and not truncated
    assert reward == -100.0


@pytest.mark.parametrize("timesteps", [0, -1, 7, 3.5, True])
def test_training_rejects_invalid_budget_before_loading_optional_runtime(tmp_path, timesteps):
    with pytest.raises(ValueError, match="at least 8"):
        train_policy(tmp_path / "missing.jsonl", None, tmp_path / "bundle", timesteps)


def test_existing_bundle_cannot_be_overwritten(tmp_path):
    with pytest.raises(ValueError, match="immutable"):
        train_policy(tmp_path / "missing.jsonl", None, tmp_path, 8)

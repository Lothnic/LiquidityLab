import numpy as np
import pytest

from liquiditylab import Action, LiquidityEnv, Plan
from liquiditylab.replay import OBSERVATION_FIELDS, OBSERVATION_SIZE


def test_price_features_use_only_completed_decision_intervals():
    events = [
        {"time": 0, "kind": "snapshot", "bids": [["99", "1"]], "asks": [["101", "1"]]},
        {"time": 1, "kind": "snapshot", "bids": [["100", "1"]], "asks": [["102", "1"]]},
        {"time": 2, "kind": "snapshot", "bids": [["102", "1"]], "asks": [["104", "1"]]},
        {"time": 40, "kind": "book", "bids": [], "asks": []},
    ]
    env = LiquidityEnv(events, plan=Plan(deadline=3))
    obs, _ = env.reset(seed=3)
    assert len(OBSERVATION_FIELDS) == OBSERVATION_SIZE
    assert obs[10] == 0 and obs[11] == 0
    obs, *_ = env.step(Action.HOLD)
    assert obs[10] == pytest.approx(0.01)
    assert obs[11] == 0
    obs, *_ = env.step(Action.HOLD)
    assert obs[10] == pytest.approx(0.03)
    assert obs[11] == pytest.approx(np.std([np.log(101 / 100), np.log(103 / 101)]))

import gymnasium as gym
import numpy as np
from gymnasium.utils.env_checker import check_env

from liquiditylab import ENV_ID, LiquidityEnv


def test_registered_environment_has_valid_gymnasium_contract():
    env = gym.make(ENV_ID)
    observation, info = env.reset(seed=123)
    assert env.action_space.contains(0)
    assert env.observation_space.contains(observation)
    assert observation.shape == (20,)
    assert observation.dtype == np.float32
    assert "status" in info
    result = env.step(0)
    assert len(result) == 5
    next_observation, reward, terminated, truncated, step_info = result
    assert env.observation_space.contains(next_observation)
    assert isinstance(reward, float)
    assert isinstance(terminated, bool) and isinstance(truncated, bool)
    assert "reason" in step_info
    env.close()


def test_liquidity_env_passes_gymnasium_checker():
    check_env(LiquidityEnv(), skip_render_check=True)

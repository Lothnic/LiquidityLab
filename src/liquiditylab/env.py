"""Gymnasium wrapper for receipt-ordered replay."""

import gymnasium as gym
import numpy as np

from .contracts import Action
from .replay import OBSERVATION_SIZE, Replay, synthetic_events


class LiquidityEnv(gym.Env):
    metadata = {"render_modes": ["ansi"], "render_fps": 1}

    def __init__(
        self, events=None, plan=None, rules=None, fees=None, config=None, render_mode=None
    ):
        super().__init__()
        if render_mode not in (None, "ansi"):
            raise ValueError("only ansi rendering is supported")
        self.render_mode = render_mode
        self.replay = Replay(
            events if events is not None else synthetic_events(), plan, rules, fees, config
        )
        self.action_space = gym.spaces.Discrete(len(Action))
        self.observation_space = gym.spaces.Box(
            -1e6, 1e6, shape=(OBSERVATION_SIZE,), dtype=np.float32
        )

    def reset(self, *, seed=None, options=None):
        super().reset(seed=seed)
        return self.replay.reset()

    def step(self, action):
        if not self.action_space.contains(action):
            raise ValueError("action must be an integer in [0,5]")
        return self.replay.step(action)

    def render(self):
        return str(self.replay.info())

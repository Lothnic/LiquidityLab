"""Bounded execution environment with isolated simulated balances."""

from gymnasium.envs.registration import register, registry

ENV_ID = "LiquidityLab-SpotExecution-v0"
if ENV_ID not in registry:
    register(id=ENV_ID, entry_point="liquiditylab.env:LiquidityEnv")

from .contracts import Action, FeeConfig, InstrumentRules, Plan, ReplayConfig  # noqa: E402
from .env import LiquidityEnv  # noqa: E402

__all__ = [
    "ENV_ID",
    "Action",
    "FeeConfig",
    "InstrumentRules",
    "Plan",
    "ReplayConfig",
    "LiquidityEnv",
]

"""Bounded execution environment with isolated simulated balances."""

from gymnasium.envs.registration import register, registry

ENV_ID = "LiquidityLab-SpotExecution-v0"
if ENV_ID not in registry:
    register(id=ENV_ID, entry_point="liquiditylab.env:LiquidityEnv")
GO_ENV_ID = "LiquidityLab-SpotExecution-v1"
if GO_ENV_ID not in registry:
    register(id=GO_ENV_ID, entry_point="liquiditylab.go_backend:GoLiquidityEnv")

from .contracts import Action, FeeConfig, InstrumentRules, Plan, ReplayConfig  # noqa: E402
from .env import LiquidityEnv  # noqa: E402

__all__ = [
    "ENV_ID",
    "GO_ENV_ID",
    "Action",
    "FeeConfig",
    "InstrumentRules",
    "Plan",
    "ReplayConfig",
    "LiquidityEnv",
]

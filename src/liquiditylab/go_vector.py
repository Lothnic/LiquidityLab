"""Lazy Stable-Baselines3 VecEnv adapter for one sequenced Go replay batch."""

from __future__ import annotations

import gymnasium as gym
import numpy as np


def make_go_vec_env(
    events,
    plan=None,
    rules=None,
    fees=None,
    config=None,
    *,
    num_envs=1,
    endpoint=None,
    binary=None,
    rpc_timeout=5.0,
):
    """Create an SB3 VecEnv without importing SB3 or the Go backend at module load."""
    try:
        from stable_baselines3.common.vec_env import VecEnv
    except ImportError as exc:
        raise RuntimeError(
            "Go training requires Stable-Baselines3. Run: uv sync --extra rl --extra go"
        ) from exc

    try:
        from .go_backend import GoReplayBatch
    except ImportError as exc:
        raise RuntimeError("Go replay requires the optional dependencies. Run: uv sync --extra go") from exc
    from .replay import OBSERVATION_SIZE
    from .training import InvalidTrainingEpisode

    if isinstance(num_envs, bool) or not isinstance(num_envs, int) or not 1 <= num_envs <= 32:
        raise ValueError("num_envs must be an integer in [1,32]")

    class GoReplayVecEnv(VecEnv):
        def __init__(self):
            self._batch = GoReplayBatch(
                events,
                plan,
                rules,
                fees,
                config,
                count=num_envs,
                endpoint=endpoint,
                binary=binary,
                rpc_timeout=rpc_timeout,
            )
            self._actions = None
            self._closed = False
            self.render_mode = None
            self._last_infos = [{} for _ in range(num_envs)]
            super().__init__(
                num_envs,
                gym.spaces.Box(
                    -1e6,
                    1e6,
                    shape=(OBSERVATION_SIZE,),
                    dtype=np.float32,
                ),
                gym.spaces.Discrete(6),
            )

        def reset(self):
            if any(self._options):
                raise ValueError("Go replay reset options are unsupported; select a new tape")
            seeds = [index if seed is None else int(seed)
                     for index, seed in enumerate(self._seeds)]
            transitions = self._batch.reset(seeds=seeds)
            observations = []
            reset_infos = []
            for observation, _, terminated, truncated, info in transitions:
                if terminated or truncated:
                    raise InvalidTrainingEpisode(
                        "Go replay reset returned a completed or invalid episode"
                    )
                observations.append(observation)
                reset_infos.append(dict(info))
            self.reset_infos = reset_infos
            self._last_infos = [dict(info) for info in reset_infos]
            self._seeds = [None] * self.num_envs
            self._options = [{} for _ in range(self.num_envs)]
            return np.stack(observations).astype(np.float32, copy=False)

        def step_async(self, actions):
            if self._closed:
                raise RuntimeError("Go replay VecEnv is closed")
            if self._actions is not None:
                raise RuntimeError("step_async called before the prior step_wait")
            actions = np.asarray(actions)
            if actions.shape != (self.num_envs,):
                raise ValueError(f"expected one discrete action per environment, got {actions.shape}")
            self._actions = actions.copy()

        def step_wait(self):
            if self._actions is None:
                raise RuntimeError("step_wait called without step_async")
            actions, self._actions = self._actions, None
            transitions = self._batch.step(actions.tolist())
            observations = []
            rewards = np.empty(self.num_envs, dtype=np.float32)
            dones = np.empty(self.num_envs, dtype=np.bool_)
            infos = []
            completed = []
            for index, (observation, reward, terminated, truncated, info) in enumerate(transitions):
                info = dict(info)
                if truncated:
                    reason = info.get("status", info.get("reason", "unknown"))
                    raise InvalidTrainingEpisode(
                        f"Invalid Go replay episode ({reason}); training halted before PPO consumed it"
                    )
                if terminated and not info.get("economic_score_valid", False):
                    reason = info.get("status", info.get("reason", "unknown"))
                    raise InvalidTrainingEpisode(
                        f"Invalid Go replay terminal outcome ({reason}); training halted"
                    )
                observations.append(observation)
                rewards[index] = reward
                dones[index] = terminated
                if terminated:
                    info["terminal_observation"] = observation.copy()
                    info["TimeLimit.truncated"] = False
                    completed.append(index)
                else:
                    info["TimeLimit.truncated"] = False
                infos.append(info)

            if completed:
                seeds = [index if self._seeds[index] is None else int(self._seeds[index])
                         for index in completed]
                reset_transitions = self._batch.reset(indices=completed, seeds=seeds)
                for index, (observation, _, terminated, truncated, reset_info) in zip(
                    completed, reset_transitions, strict=True
                ):
                    if terminated or truncated:
                        raise InvalidTrainingEpisode(
                            "Go replay auto-reset returned a completed or invalid episode"
                        )
                    observations[index] = observation
                    self.reset_infos[index] = dict(reset_info)
                    self._last_infos[index] = dict(reset_info)
                    self._seeds[index] = None
            for index, info in enumerate(infos):
                self._last_infos[index] = info
            return (
                np.stack(observations).astype(np.float32, copy=False),
                rewards,
                dones,
                infos,
            )

        def close(self):
            if self._closed:
                return
            self._closed = True
            self._batch.close()

        def get_attr(self, attr_name, indices=None):
            indices = list(self._get_indices(indices))
            if attr_name != "render_mode":
                raise AttributeError(attr_name)
            return [self.render_mode for _ in indices]

        def set_attr(self, attr_name, value, indices=None):
            raise NotImplementedError("Go engine state changes require explicit RPC commands")

        def env_method(self, method_name, *method_args, indices=None, **method_kwargs):
            raise NotImplementedError("Go replay VecEnv exposes no per-environment Python methods")

        def env_is_wrapped(self, wrapper_class, indices=None):
            return [False for _ in self._get_indices(indices)]

    return GoReplayVecEnv()

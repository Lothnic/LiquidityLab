"""Transparent baselines using the same six actions as learned policies."""

from .contracts import Action


class ScheduledPolicy:
    """Submit one normal IOC every 15 seconds."""

    def __init__(self):
        self.next_slice = 0.0

    def act(self, replay):
        if replay.now + 1e-9 < self.next_slice:
            return Action.HOLD
        self.next_slice += 15.0
        return Action.AGGRESSIVE_NORMAL


class LiquidityAwarePolicy:
    """Rest early; use bounded IOC orders when behind a linear schedule."""

    def act(self, replay):
        elapsed = replay.now / replay.plan.deadline
        filled = float(replay.ledger.filled / replay.plan.quantity)
        if elapsed - filled > 0.1 or elapsed >= 0.75:
            return Action.AGGRESSIVE_NORMAL
        return Action.PASSIVE_NORMAL


def run_policy(replay, name="scheduled"):
    if name not in ("scheduled", "liquidity-aware"):
        raise ValueError("unknown baseline policy")
    policy = ScheduledPolicy() if name == "scheduled" else LiquidityAwarePolicy()
    replay.reset()
    while not (replay.terminated or replay.truncated):
        replay.step(policy.act(replay))
    report = replay.report()
    report["policy"] = name
    return report

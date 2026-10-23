"""Local, replay-only PPO inference worker.

The worker returns semantic actions and has no exchange credentials or
order-submission capability. Its gRPC and PPO dependencies load on demand.
"""

from .worker import (
    InferenceError,
    InferenceWorker,
    load_worker,
    observation_schema_sha256,
    serve,
)

__all__ = [
    "InferenceError",
    "InferenceWorker",
    "load_worker",
    "observation_schema_sha256",
    "serve",
]

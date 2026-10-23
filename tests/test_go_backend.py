import os

import numpy as np
import pytest

grpc = pytest.importorskip("grpc")

from gymnasium.utils.env_checker import check_env  # noqa: E402

from liquiditylab.go_backend import (  # noqa: E402
    GoLiquidityEnv,
    GoReplayBatch,
    ReplayServiceError,
)
from liquiditylab.replay import synthetic_events  # noqa: E402


@pytest.fixture
def replay_binary():
    path = os.environ.get("LIQUIDITYLAB_REPLAY_BINARY")
    if not path:
        pytest.skip("build liquiditylabd and set LIQUIDITYLAB_REPLAY_BINARY for integration checks")
    return path


def test_real_gym_contract_and_report(replay_binary):
    with GoLiquidityEnv(binary=replay_binary) as env:
        check_env(env, skip_render_check=True)
        env.reset(seed=17)
        for _ in range(40):
            _, _, terminated, truncated, _ = env.step(5)
            if terminated or truncated:
                break
        assert terminated and not truncated
        report = env._batch.reports()[0]
        assert report["economic_score_valid"] is True
        assert report["filled_quantity"] == "0.1"


def test_batch_state_isolation_and_lost_response_retry(replay_binary):
    with GoReplayBatch(synthetic_events(), count=2, binary=replay_binary) as batch:
        before = batch.reset()
        results = batch.step([5, 0])
        assert results[0][-1]["filled"] == "0.05"
        assert results[1][-1]["filled"] == "0"
        np.testing.assert_array_equal(before[1][0][[0, 2]], results[1][0][[0, 2]])
        batch.reset()
        original = batch._stub.StepBatch
        requests = []

        class LostResponse(grpc.RpcError):
            def code(self):
                return grpc.StatusCode.UNAVAILABLE

            def details(self):
                return "injected response loss after real server mutation"

        def lose_once(request, timeout):
            requests.append(request.SerializeToString())
            response = original(request, timeout=timeout)
            if len(requests) == 1:
                raise LostResponse()
            return response

        batch._stub.StepBatch = lose_once
        results = batch.step([5, 0])
        assert requests[0] == requests[1]
        assert results[0][-1]["filled"] == "0.05"
        assert results[0][-1]["time_ns"] == 1_000_000_000


def test_worker_death_aborts_batch(replay_binary):
    with GoReplayBatch(synthetic_events(), binary=replay_binary, rpc_timeout=0.2) as batch:
        batch.reset()
        batch._owner._process.terminate()
        batch._owner._process.wait(timeout=5)
        with pytest.raises(ReplayServiceError, match="cannot advance"):
            batch.step([0])
        with pytest.raises(ReplayServiceError, match="unresolved"):
            batch.step([0])


def test_malformed_monetary_response_invalidates_batch(replay_binary):
    with GoReplayBatch(synthetic_events(), binary=replay_binary) as batch:
        batch.reset()
        original = batch._stub.StepBatch

        def corrupt(request, timeout):
            response = original(request, timeout=timeout)
            response.transitions[0].filled_quantity.coefficient = "bad"
            return response

        batch._stub.StepBatch = corrupt
        with pytest.raises(ReplayServiceError, match="malformed"):
            batch.step([0])
        with pytest.raises(ReplayServiceError, match="unresolved"):
            batch.step([0])


def test_selected_operation_budget_fails_explicitly(replay_binary):
    with GoReplayBatch(synthetic_events(), binary=replay_binary, max_operations=1) as batch:
        batch.reset()
        with pytest.raises(ReplayServiceError, match="RESOURCE_EXHAUSTED"):
            batch.step([0])

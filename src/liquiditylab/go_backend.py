"""Gymnasium v1 and a bounded client for the authoritative Go replay core.

An RPC timeout never causes an unsequenced second step. The client retries the
same operation identity and fails the trajectory if its outcome remains unknown.
Install ``liquiditylab[go]`` and provide a built replay binary or a local endpoint.
"""

from __future__ import annotations

import json
import math
import os
import subprocess
import tempfile
import threading
import uuid
from pathlib import Path

import grpc
import gymnasium as gym
import numpy as np

from .protocol import GO_ENV_ID, action_to_wire, decode_decimal, encode_decimal, episode_spec
from .replay import OBSERVATION_FIELDS, OBSERVATION_SIZE, OBSERVATION_VERSION, synthetic_events
from .v1 import execution_pb2 as pb
from .v1 import execution_pb2_grpc as rpc


class ReplayServiceError(RuntimeError):
    """Replay failed; no replacement episode or policy fallback is permitted."""


class GoReplayProcess:
    """Own one explicitly selected Go binary and a private local socket."""

    def __init__(self, binary, startup_timeout=10.0, max_operations=100000):
        if not math.isfinite(startup_timeout) or not 0 < startup_timeout <= 60:
            raise ValueError("startup_timeout must be in (0,60]")
        if isinstance(max_operations, bool) or not isinstance(max_operations, int) \
                or not 1 <= max_operations <= 1000000:
            raise ValueError("max_operations must be in [1,1000000]")
        path = Path(binary).expanduser().resolve()
        if not path.is_file() or not os.access(path, os.X_OK):
            raise ValueError(f"replay executable unavailable: {path}")
        self._directory = tempfile.TemporaryDirectory(prefix="liquiditylab-replay-")
        self.endpoint = "unix:" + str(Path(self._directory.name) / "service.sock")
        self._log = open(Path(self._directory.name) / "server.log", "w+b")
        self._process = None
        try:
            self._process = subprocess.Popen(
                [str(path), "replay", "--listen", self.endpoint,
                 "--max-operations-per-batch", str(max_operations)],
                stdin=subprocess.DEVNULL, stdout=self._log, stderr=self._log,
            )
            with grpc.insecure_channel(self.endpoint) as channel:
                grpc.channel_ready_future(channel).result(timeout=startup_timeout)
            if self._process.poll() is not None:
                raise ReplayServiceError("Go replay process exited during startup")
        except Exception as exc:
            self._log.seek(0)
            detail = self._log.read(4096).decode(errors="replace")
            self.close()
            raise ReplayServiceError(f"Go replay startup failed: {detail}") from exc

    def close(self):
        if self._process is not None and self._process.poll() is None:
            self._process.terminate()
            try:
                self._process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self._process.kill()
                self._process.wait(timeout=5)
        self._log.close()
        self._directory.cleanup()

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()


def _local_endpoint(endpoint):
    if endpoint.startswith("unix:/"):
        return endpoint
    host, sep, port = endpoint.rpartition(":")
    if not sep or host not in ("127.0.0.1", "localhost", "[::1]"):
        raise ValueError("replay endpoint must be a local TCP address or absolute Unix socket")
    if not port.isdecimal() or not 0 < int(port) <= 65535:
        raise ValueError("invalid replay TCP port")
    return endpoint


class GoReplayBatch:
    """One tape upload, isolated handles, and serialized sequenced batch calls."""

    def __init__(self, events, plan=None, rules=None, fees=None, config=None, *, count=1,
                 endpoint=None, binary=None, rpc_timeout=5.0, max_operations=100000):
        if isinstance(count, bool) or not isinstance(count, int) or not 1 <= count <= 32:
            raise ValueError("batch count must be in [1,32]")
        if not math.isfinite(rpc_timeout) or not 0 < rpc_timeout <= 60:
            raise ValueError("rpc_timeout must be in (0,60]")
        if endpoint is not None and binary is not None:
            raise ValueError("select endpoint or binary")
        spec = episode_spec(events, plan, rules, fees, config)
        request = pb.CreateBatchRequest(request_id=uuid.uuid4().hex, count=count, episode=spec)
        if request.ByteSize() > 8 * 1024 * 1024:
            raise ValueError("episode request exceeds the 8MiB message limit")
        self._owner = None
        self._channel = None
        self._closed = False
        self._uncertain = False
        self._lock = threading.RLock()
        self._timeout = rpc_timeout
        self._last = []
        try:
            if endpoint is None:
                binary = binary or os.environ.get("LIQUIDITYLAB_REPLAY_BINARY")
                if not binary:
                    raise ValueError("provide binary or endpoint; see docs/go-replay.md")
                self._owner = GoReplayProcess(binary, max_operations=max_operations)
                endpoint = self._owner.endpoint
            self._channel = grpc.insecure_channel(_local_endpoint(endpoint), options=(
                ("grpc.max_send_message_length", 8 * 1024 * 1024),
                ("grpc.max_receive_message_length", 8 * 1024 * 1024),
                ("grpc.enable_retries", 0),
            ))
            self._stub = rpc.ReplayServiceStub(self._channel)
            created = self._call(self._stub.CreateBatch, request)
            self.batch_id, self.handles = created.batch_id, tuple(created.environment_ids)
            self.engine_version = created.engine_version
            self.engine_instance_id = created.engine_instance_id
            self._sequence = created.next_sequence
            if (self.engine_version != GO_ENV_ID or created.observation_version != OBSERVATION_VERSION
                    or len(self.handles) != count or len(set(self.handles)) != count
                    or not self.batch_id or not self.engine_instance_id or self._sequence != 1):
                raise ReplayServiceError("incompatible Go replay contract")
        except Exception:
            self.close()
            raise

    def _call(self, method, request):
        # Application sequencing owns retries; gRPC transport retries are disabled.
        for attempt in range(2):
            try:
                return method(request, timeout=self._timeout)
            except grpc.RpcError as exc:
                transient = exc.code() in (grpc.StatusCode.UNAVAILABLE,
                                            grpc.StatusCode.DEADLINE_EXCEEDED)
                if transient and attempt == 0:
                    continue
                self._uncertain = True
                raise ReplayServiceError(
                    f"Replay RPC {exc.code().name}: {exc.details()}; "
                    f"request={request.request_id}, sequence={getattr(request, 'sequence', 0)}. "
                    "This batch cannot advance after an unresolved call."
                ) from exc

    def _ready(self):
        if self._closed or self._uncertain:
            raise ReplayServiceError("replay batch is closed or has an unresolved operation")

    def _indices(self, indices):
        result = list(range(len(self.handles))) if indices is None else list(indices)
        if (not result or len(set(result)) != len(result)
                or any(isinstance(i, bool) or not isinstance(i, int)
                       or not 0 <= i < len(self.handles) for i in result)):
            raise ValueError("select distinct valid environment indices")
        return result

    def _transitions(self, result, indices):
        try:
            return self._decode_transitions(result, indices)
        except Exception as exc:
            # The server may have advanced even when its response is malformed.
            self._uncertain = True
            if isinstance(exc, ReplayServiceError):
                raise
            raise ReplayServiceError("malformed replay response; batch invalidated") from exc

    def _decode_transitions(self, result, indices):
        expected = [self.handles[index] for index in indices]
        if (result.batch_id != self.batch_id or result.sequence != self._sequence
                or [item.environment_id for item in result.transitions] != expected):
            self._uncertain = True
            raise ReplayServiceError("batch response identity mismatch")
        output = []
        for item in result.transitions:
            observation = item.observation
            values = np.asarray(observation.features, dtype=np.float32)
            if (observation.schema_version != OBSERVATION_VERSION
                    or tuple(observation.field_names) != OBSERVATION_FIELDS
                    or values.shape != (OBSERVATION_SIZE,) or not np.isfinite(values).all()
                    or not math.isfinite(item.reward)
                    or item.terminated and item.truncated
                    or item.economic_score_valid != item.terminated):
                self._uncertain = True
                raise ReplayServiceError("invalid observation, reward, or settlement contract")
            info = {"status": item.status, "reason": item.reason,
                    "time_ns": item.receipt_time_ns,
                    "filled": str(decode_decimal(item.filled_quantity)),
                    "remaining": str(decode_decimal(item.remaining_quantity)),
                    "provisional": not item.economic_score_valid,
                    "economic_score_valid": item.economic_score_valid,
                    "engine_version": self.engine_version,
                    "engine_instance_id": self.engine_instance_id}
            if item.HasField("report"):
                info["report"] = json.loads(item.report.detail_json)
            output.append((values, item.reward, item.terminated, item.truncated, info))
        self._sequence += 1
        self._last = output
        return output

    def reset(self, indices=None, seeds=None):
        with self._lock:
            self._ready()
            indices = self._indices(indices)
            seeds = [0] * len(indices) if seeds is None else list(seeds)
            if len(seeds) != len(indices) or any(
                    isinstance(seed, bool) or not isinstance(seed, int)
                    or not 0 <= seed < 1 << 64 for seed in seeds):
                raise ValueError("one nonnegative uint64 seed is required per environment")
            request = pb.ResetBatchRequest(batch_id=self.batch_id, request_id=uuid.uuid4().hex,
                                          sequence=self._sequence, resets=[
                pb.EnvironmentReset(environment_id=self.handles[i], seed=seed)
                for i, seed in zip(indices, seeds, strict=True)])
            return self._transitions(self._call(self._stub.ResetBatch, request), indices)

    def step(self, actions, indices=None):
        with self._lock:
            self._ready()
            indices = self._indices(indices)
            actions = list(actions)
            if len(actions) != len(indices):
                raise ValueError("one action is required per selected environment")
            request = pb.StepBatchRequest(batch_id=self.batch_id, request_id=uuid.uuid4().hex,
                                         sequence=self._sequence, actions=[
                pb.EnvironmentAction(environment_id=self.handles[i], action=action_to_wire(action))
                for i, action in zip(indices, actions, strict=True)])
            return self._transitions(self._call(self._stub.StepBatch, request), indices)

    def reports(self, indices=None):
        """Read ended-episode reports outside the hot step; this does not advance state."""
        with self._lock:
            self._ready()
            indices = self._indices(indices)
            try:
                result = self._stub.GetReports(pb.GetReportsRequest(
                    batch_id=self.batch_id, environment_ids=[self.handles[i] for i in indices]),
                    timeout=self._timeout)
            except grpc.RpcError as exc:
                raise ReplayServiceError(f"Report RPC {exc.code().name}: {exc.details()}") from exc
            if len(result.reports) != len(indices):
                raise ReplayServiceError("report count differs from requested handles")
            output = []
            for report in result.reports:
                detail = json.loads(report.detail_json)
                if (detail.get("economic_score_valid") != report.economic_score_valid
                        or detail.get("status") != report.status
                        or decode_decimal(report.filled_quantity)
                        != decode_decimal(encode_decimal(detail["filled_quantity"]))
                        or decode_decimal(report.remaining_quantity)
                        != decode_decimal(encode_decimal(detail["remaining_quantity"]))):
                    raise ReplayServiceError("typed report differs from report details")
                output.append(detail)
            return output

    def close(self):
        with self._lock:
            if self._closed:
                return
            try:
                if self._channel is not None and hasattr(self, "batch_id") and not self._uncertain:
                    response = self._call(self._stub.CloseBatch, pb.CloseBatchRequest(
                        batch_id=self.batch_id, request_id=uuid.uuid4().hex, sequence=self._sequence))
                    if not response.closed or response.sequence != self._sequence:
                        raise ReplayServiceError("invalid close acknowledgement")
            finally:
                self._closed = True
                if self._channel is not None:
                    self._channel.close()
                if self._owner is not None:
                    self._owner.close()

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()


class GoLiquidityEnv(gym.Env):
    metadata = {"render_modes": ["ansi"], "render_fps": 1}

    def __init__(self, events=None, plan=None, rules=None, fees=None, config=None,
                 render_mode=None, *, endpoint=None, binary=None, rpc_timeout=5.0,
                 max_operations=100000):
        super().__init__()
        if render_mode not in (None, "ansi"):
            raise ValueError("only ansi rendering is supported")
        self.render_mode = render_mode
        self.action_space = gym.spaces.Discrete(6)
        self.observation_space = gym.spaces.Box(-1e6, 1e6, shape=(OBSERVATION_SIZE,), dtype=np.float32)
        self._batch = GoReplayBatch(events if events is not None else synthetic_events(),
                                    plan, rules, fees, config, endpoint=endpoint, binary=binary,
                                    rpc_timeout=rpc_timeout, max_operations=max_operations)
        self._info = {}

    def reset(self, *, seed=None, options=None):
        super().reset(seed=seed)
        if options:
            raise ValueError("episode replacement requires a new batch")
        observation, _, _, _, self._info = self._batch.reset(seeds=[0 if seed is None else seed])[0]
        return observation, dict(self._info)

    def step(self, action):
        if not self.action_space.contains(action):
            raise ValueError("action must be an integer in [0,5]")
        result = self._batch.step([action])[0]
        self._info = result[-1]
        return result

    def render(self):
        return str(self._info)

    def report(self):
        return self._batch.reports([0])[0]

    def close(self):
        self._batch.close()

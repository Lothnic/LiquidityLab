"""Pinned local inference worker contract and optional Go/PPO integration."""

import hashlib
import json
import os
import socket
import stat
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

import numpy as np
import pytest

from liquiditylab.inference.worker import (
    BusyInference,
    InferenceError,
    _validate_local_address,
    load_worker,
    observation_schema_sha256,
    start_server,
)
from liquiditylab.replay import OBSERVATION_FIELDS, OBSERVATION_SIZE, OBSERVATION_VERSION


def _require_grpc():
    pytest.importorskip("grpc")
    pytest.importorskip("google.protobuf")


def _bundle(tmp_path, *, manifest_overrides=None, model_bytes=b"fake-policy-zip"):
    bundle = tmp_path / "bundle"
    bundle.mkdir()
    policy = bundle / "policy.zip"
    policy.write_bytes(model_bytes)
    manifest = {
        "schema": "liquiditylab.policy-bundle.v0",
        "status": "unvalidated",
        "live_eligible": False,
        "backend": "go",
        "environment": "LiquidityLab-SpotExecution-v1",
        "observation_version": OBSERVATION_VERSION,
        "observation_size": OBSERVATION_SIZE,
        "observation_fields": list(OBSERVATION_FIELDS),
        "action_version": "six-actions-v0",
        "action_compatibility": {
            "policy_action_values": [0, 1, 2, 3, 4, 5],
            "go_wire_enum_values": [1, 2, 3, 4, 5, 6],
            "mapping": "wire enum value = policy action value + 1",
        },
        "policy_sha256": hashlib.sha256(policy.read_bytes()).hexdigest(),
        "go_replay": {
            "environment": "LiquidityLab-SpotExecution-v1",
            "provenance": "owned-local-process-binary",
            "binary_path": "/work/bin/liquiditylabd",
            "binary_sha256": "a" * 64,
        },
    }
    if manifest_overrides:
        manifest.update(manifest_overrides)
    (bundle / "manifest.json").write_text(json.dumps(manifest), encoding="utf-8")
    return bundle, manifest


class FakeModel:
    def __init__(self, action=4, *, entered=None, release=None):
        self.action = action
        self.entered = entered
        self.release = release
        self.calls = []

    def predict(self, observation, deterministic):
        self.calls.append((observation.copy(), deterministic))
        if self.entered:
            self.entered.set()
        if self.release:
            assert self.release.wait(2)
        return np.asarray([self.action]), None


class FixedOutputModel:
    def __init__(self, output):
        self.output = output

    def predict(self, observation, deterministic):
        return np.asarray(self.output), None


@pytest.fixture
def protocol():
    _require_grpc()
    from liquiditylab.v1 import execution_pb2 as pb
    from liquiditylab.v1 import execution_pb2_grpc as pb_grpc

    return pb, pb_grpc


def _request(pb, policy_sha, *, action=None, expiry_ns=None):
    binding = pb.DecisionBinding(
        request_id="request-1",
        tenant_id="tenant-1",
        account_id="account-1",
        session_id="session-1",
        session_revision=9,
        account_revision=17,
        policy_sha256=policy_sha,
        observation_version=OBSERVATION_VERSION,
        observation_schema_sha256=observation_schema_sha256(),
        market_snapshot_id="snapshot-101",
        expires_at_unix_ns=expiry_ns or time.time_ns() + 150_000_000,
    )
    observation = pb.Observation(
        features=[float(i) / 10 for i in range(OBSERVATION_SIZE)],
        schema_version=OBSERVATION_VERSION,
        field_names=OBSERVATION_FIELDS,
    )
    return pb.DecisionRequest(binding=binding, observation=observation)


def _free_local_port():
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()
    return port


def test_schema_hash_matches_canonical_version_and_order():
    canonical = json.dumps(
        {"version": OBSERVATION_VERSION, "fields": list(OBSERVATION_FIELDS)},
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
    ).encode("utf-8")
    assert observation_schema_sha256() == hashlib.sha256(canonical).hexdigest()


def test_bundle_verification_and_policyzip_digest(tmp_path):
    bundle, manifest = _bundle(tmp_path)
    model = FakeModel()
    worker = load_worker(bundle, model=model)
    assert worker.policy_sha256 == manifest["policy_sha256"]
    assert worker.policy_info() == {
        "policy_sha256": manifest["policy_sha256"],
        "observation_version": OBSERVATION_VERSION,
        "field_names": list(OBSERVATION_FIELDS),
        "promotion_status": "unvalidated",
        "live_eligible": False,
    }

    (bundle / "policy.zip").write_bytes(b"changed-policy")
    with pytest.raises(InferenceError, match="SHA-256"):
        load_worker(bundle, model=FakeModel())


def test_bundle_loader_rejects_symlinked_policy_file(tmp_path):
    bundle, _ = _bundle(tmp_path)
    target = tmp_path / "actual-policy.zip"
    (bundle / "policy.zip").rename(target)
    (bundle / "policy.zip").symlink_to(target)
    with pytest.raises(InferenceError, match="requires manifest"):
        load_worker(bundle, model=FakeModel())


def test_bundle_loader_rejects_symlinked_bundle_directory(tmp_path):
    bundle, _ = _bundle(tmp_path)
    alias = tmp_path / "bundle-link"
    alias.symlink_to(bundle, target_is_directory=True)
    with pytest.raises(InferenceError, match="requires manifest"):
        load_worker(alias, model=FakeModel())


@pytest.mark.parametrize(
    ("overrides", "message"),
    [
        ({"schema": "liquiditylab.policy-bundle.v0-old"}, "schema"),
        ({"backend": "python-reference"}, "Go replay v1"),
        ({"go_replay": None}, "provenance"),
        ({"observation_fields": list(reversed(OBSERVATION_FIELDS))}, "fields or order"),
        ({"action_compatibility": {"policy_action_values": [0, 1]}}, "action compatibility"),
        ({"live_eligible": True}, "live-eligible"),
    ],
)
def test_bundle_rejects_incompatible_or_forged_manifest(tmp_path, overrides, message):
    bundle, _ = _bundle(tmp_path, manifest_overrides=overrides)
    with pytest.raises(InferenceError, match=message):
        load_worker(bundle, model=FakeModel())


def test_policy_actions_map_zero_through_five_to_wire_one_through_six(tmp_path, protocol):
    pb, pb_grpc = protocol
    bundle, manifest = _bundle(tmp_path)
    worker = load_worker(bundle, model=FakeModel())
    server, executor, address = start_server(worker, f"127.0.0.1:{_free_local_port()}")
    channel = __import__("grpc").insecure_channel(address)
    stub = pb_grpc.PolicyServiceStub(channel)
    try:
        for policy_action, wire_action in enumerate(range(1, 7)):
            worker.model.action = policy_action
            request = _request(pb, manifest["policy_sha256"])
            response = stub.Predict(request, timeout=1.0)
            assert response.action == wire_action
            assert response.binding.SerializeToString() == request.binding.SerializeToString()
            assert response.mode == "unvalidated"
        info = stub.GetPolicyInfo(
            pb.PolicyInfoRequest(policy_sha256=manifest["policy_sha256"]), timeout=1.0
        )
        assert info.promotion_status == "unvalidated"
        assert not info.live_eligible
        assert tuple(info.field_names) == OBSERVATION_FIELDS
    finally:
        channel.close()
        server.stop(0).wait(timeout=2)
        executor.shutdown(wait=True, cancel_futures=True)


@pytest.mark.parametrize(
    "mutation, message",
    [
        ("policy", "policy SHA-256"),
        ("schema_hash", "schema hash"),
        ("schema_version", "version"),
        ("fields", "fields or order"),
        ("short", "exactly 20"),
        ("nan", "non-finite"),
        ("expired", "expired"),
        ("too_far", "validity window"),
    ],
)
def test_predict_rejects_mismatched_observations_and_bindings(
    tmp_path, protocol, mutation, message
):
    pb, _ = protocol
    bundle, manifest = _bundle(tmp_path)
    worker = load_worker(bundle, model=FakeModel())
    request = _request(pb, manifest["policy_sha256"])
    if mutation == "policy":
        request.binding.policy_sha256 = "b" * 64
    elif mutation == "schema_hash":
        request.binding.observation_schema_sha256 = "b" * 64
    elif mutation == "schema_version":
        request.observation.schema_version = "compact-v0"
    elif mutation == "fields":
        request.observation.field_names[0] = "wrong-first-field"
    elif mutation == "short":
        del request.observation.features[-1]
    elif mutation == "nan":
        request.observation.features[0] = float("nan")
    elif mutation == "expired":
        request.binding.expires_at_unix_ns = time.time_ns() - 1
    elif mutation == "too_far":
        request.binding.expires_at_unix_ns = time.time_ns() + 500_000_000
    with pytest.raises(InferenceError, match=message):
        worker.predict_action(
            request.binding,
            request.observation,
            request_bytes=request.ByteSize(),
            is_active=lambda: True,
            time_remaining=lambda: 1.0,
        )
    assert worker.model.calls == []


def test_worker_enforces_message_size_and_cancel_before_model_start(tmp_path, protocol):
    pb, _ = protocol
    bundle, manifest = _bundle(tmp_path)
    model = FakeModel()
    worker = load_worker(bundle, model=model, max_request_bytes=512)
    request = _request(pb, manifest["policy_sha256"])
    with pytest.raises(InferenceError, match="maximum message size"):
        worker.predict_action(
            request.binding,
            request.observation,
            request_bytes=513,
            is_active=lambda: True,
            time_remaining=lambda: 1.0,
        )
    with pytest.raises(InferenceError, match="cancelled"):
        worker.predict_action(
            request.binding,
            request.observation,
            request_bytes=1,
            is_active=lambda: False,
            time_remaining=lambda: 1.0,
        )
    assert model.calls == []


@pytest.mark.parametrize("output", [[1, 2], [6], [-1], [float("nan")], [1.5]])
def test_worker_rejects_invalid_policy_outputs(tmp_path, protocol, output):
    pb, _ = protocol
    bundle, manifest = _bundle(tmp_path)
    worker = load_worker(bundle, model=FixedOutputModel(output))
    request = _request(pb, manifest["policy_sha256"])
    with pytest.raises(InferenceError, match="policy"):
        worker.predict_action(
            request.binding,
            request.observation,
            request_bytes=request.ByteSize(),
            is_active=lambda: True,
            time_remaining=lambda: 1.0,
        )


def test_worker_has_one_nonblocking_inference_slot(tmp_path, protocol):
    pb, _ = protocol
    bundle, manifest = _bundle(tmp_path)
    entered, release = threading.Event(), threading.Event()
    model = FakeModel(entered=entered, release=release)
    worker = load_worker(bundle, model=model)
    request = _request(pb, manifest["policy_sha256"])

    def run_first():
        return worker.predict_action(
            request.binding,
            request.observation,
            request_bytes=request.ByteSize(),
            is_active=lambda: True,
            time_remaining=lambda: 1.0,
        )

    with ThreadPoolExecutor(max_workers=2) as pool:
        first = pool.submit(run_first)
        assert entered.wait(1)
        with pytest.raises(BusyInference, match="capacity"):
            run_first()
        release.set()
        assert first.result(timeout=2) == 5


def test_server_only_binds_unix_or_loopback(tmp_path):
    assert _validate_local_address("127.0.0.1:50051")[0] == "127.0.0.1:50051"
    assert (
        _validate_local_address("unix:/tmp/liquiditylab-test.sock")[0]
        == "unix:/tmp/liquiditylab-test.sock"
    )
    for address in ("0.0.0.0:50051", "192.0.2.10:50051", "localhost:50051"):
        with pytest.raises(ValueError, match="local|loopback"):
            _validate_local_address(address)
    with pytest.raises(ValueError, match="existing"):
        _validate_local_address(f"unix:{tmp_path}")


def test_unix_server_socket_has_owner_only_permissions(tmp_path):
    _require_grpc()
    bundle, _ = _bundle(tmp_path)
    worker = load_worker(bundle, model=FakeModel())
    socket_path = tmp_path / "inference.sock"
    server, executor, _ = start_server(worker, f"unix:{socket_path}")
    try:
        assert stat.S_IMODE(socket_path.stat().st_mode) == 0o600
    finally:
        server.stop(0).wait(timeout=2)
        executor.shutdown(wait=True, cancel_futures=True)
        socket_path.unlink(missing_ok=True)


def test_ppo_bundle_roundtrip_when_go_binary_is_configured(tmp_path, protocol):
    binary = os.environ.get("LIQUIDITYLAB_REPLAY_BINARY")
    if not binary:
        pytest.skip("set LIQUIDITYLAB_REPLAY_BINARY to run the Go PPO integration check")
    pytest.importorskip("stable_baselines3")
    pytest.importorskip("torch")
    _require_grpc()
    from liquiditylab.go_backend import GoLiquidityEnv
    from liquiditylab.replay import synthetic_events
    from liquiditylab.training import train_policy

    binary_path = Path(binary).expanduser().resolve()
    if not binary_path.is_file() or not os.access(binary_path, os.X_OK):
        pytest.skip(f"Go replay binary is unavailable: {binary_path}")
    tape = tmp_path / "tape.jsonl"
    tape.write_text(
        "".join(json.dumps(event) + "\n" for event in synthetic_events()), encoding="utf-8"
    )
    bundle = tmp_path / "bundle"
    manifest = train_policy(
        tape,
        None,
        bundle,
        total_timesteps=16,
        seed=11,
        backend="go",
        replay_binary=binary_path,
        num_envs=2,
    )
    assert manifest["status"] == "unvalidated" and manifest["live_eligible"] is False
    assert manifest["go_replay"]["provenance"] == "owned-local-process-binary"

    worker = load_worker(bundle, max_validity_ns=10_000_000_000)
    assert worker.policy_sha256 == hashlib.sha256((bundle / "policy.zip").read_bytes()).hexdigest()
    assert worker.policy_info()["live_eligible"] is False
    env = GoLiquidityEnv(synthetic_events(), binary=binary_path)
    try:
        observation, _ = env.reset(seed=11)
        pb, pb_grpc = protocol
        request = _request(pb, manifest["policy_sha256"], expiry_ns=time.time_ns() + 5_000_000_000)
        request.observation.features[:] = observation.tolist()
        server, executor, address = start_server(worker, f"127.0.0.1:{_free_local_port()}")
        channel = __import__("grpc").insecure_channel(address)
        try:
            response = pb_grpc.PolicyServiceStub(channel).Predict(request, timeout=1.0)
        finally:
            channel.close()
            server.stop(0).wait(timeout=2)
            executor.shutdown(wait=True, cancel_futures=True)
        assert 1 <= response.action <= 6
        assert response.binding.SerializeToString() == request.binding.SerializeToString()
        assert response.mode == "unvalidated"
        action = int(response.action) - 1
        next_observation, reward, _, truncated, _ = env.step(action)
        assert not truncated and np.isfinite(next_observation).all() and np.isfinite(reward)
    finally:
        env.close()

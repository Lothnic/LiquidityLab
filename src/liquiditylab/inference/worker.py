"""Strict local policy inference for the versioned PolicyService contract."""

from __future__ import annotations

import hashlib
import ipaddress
import json
import math
import os
import re
import signal
import stat
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from typing import Any, Callable

from ..replay import OBSERVATION_FIELDS, OBSERVATION_SIZE, OBSERVATION_VERSION

GO_ENV_ID = "LiquidityLab-SpotExecution-v1"
_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
_EXPECTED_ACTION_COMPATIBILITY = {
    "policy_action_values": [0, 1, 2, 3, 4, 5],
    "go_wire_enum_values": [1, 2, 3, 4, 5, 6],
    "mapping": "wire enum value = policy action value + 1",
}
_WIRE_ACTION_MIN = 1
_WIRE_ACTION_MAX = 6
_DEFAULT_MAX_REQUEST_BYTES = 8192
_DEFAULT_MAX_VALIDITY_NS = 200_000_000


class InferenceError(ValueError):
    """A bundle or inference request violates the frozen worker contract."""


def _canonical_json(value: Any) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode(
        "utf-8"
    )


def observation_schema_sha256() -> str:
    """Hash the canonical observation version and ordered field list."""
    canonical = {"version": OBSERVATION_VERSION, "fields": list(OBSERVATION_FIELDS)}
    return hashlib.sha256(_canonical_json(canonical)).hexdigest()


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _require_hash(value: Any, label: str) -> str:
    if not isinstance(value, str) or not _SHA256_RE.fullmatch(value):
        raise InferenceError(f"{label} must be a lowercase SHA-256 digest")
    return value


def _load_manifest(bundle_dir: Path) -> tuple[dict[str, Any], Path, str]:
    manifest_path = bundle_dir / "manifest.json"
    model_path = bundle_dir / "policy.zip"
    if (
        bundle_dir.is_symlink()
        or manifest_path.is_symlink()
        or model_path.is_symlink()
        or not manifest_path.is_file()
        or not model_path.is_file()
    ):
        raise InferenceError("policy bundle requires manifest.json and policy.zip")
    try:
        if manifest_path.stat().st_size > 1_048_576:
            raise InferenceError("policy bundle manifest exceeds 1 MiB")
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise InferenceError("policy bundle manifest is unreadable JSON") from exc
    if not isinstance(manifest, dict):
        raise InferenceError("policy bundle manifest must be an object")
    if manifest.get("schema") != "liquiditylab.policy-bundle.v0":
        raise InferenceError("unsupported policy bundle schema")
    if manifest.get("backend") != "go" or manifest.get("environment") != GO_ENV_ID:
        raise InferenceError("inference requires a Go replay v1 policy bundle")
    replay = manifest.get("go_replay")
    if not isinstance(replay, dict) or replay.get("environment") != GO_ENV_ID:
        raise InferenceError("policy bundle lacks Go replay v1 provenance")
    if replay.get("provenance") != "owned-local-process-binary":
        raise InferenceError("policy bundle lacks owned Go binary provenance")
    if not isinstance(replay.get("binary_path"), str) or not replay["binary_path"]:
        raise InferenceError("policy bundle lacks its Go binary path provenance")
    if not Path(replay["binary_path"]).is_absolute():
        raise InferenceError("Go replay binary provenance path must be absolute")
    _require_hash(replay.get("binary_sha256"), "Go replay binary SHA-256")

    if manifest.get("observation_version") != OBSERVATION_VERSION:
        raise InferenceError("policy observation version is incompatible")
    if manifest.get("observation_size") != OBSERVATION_SIZE:
        raise InferenceError("policy observation length is incompatible")
    if manifest.get("observation_fields") != list(OBSERVATION_FIELDS):
        raise InferenceError("policy observation fields or order are incompatible")
    if manifest.get("action_version") != "six-actions-v0":
        raise InferenceError("policy action version is incompatible")
    if manifest.get("action_compatibility") != _EXPECTED_ACTION_COMPATIBILITY:
        raise InferenceError("policy action compatibility mapping is invalid")

    status = manifest.get("status")
    if not isinstance(status, str) or not status or len(status) > 64:
        raise InferenceError("policy status is missing or invalid")
    if manifest.get("live_eligible") is not False:
        raise InferenceError("live-eligible bundles are not supported by this worker")

    expected_model_sha = _require_hash(manifest.get("policy_sha256"), "policy SHA-256")
    model_sha = _sha256(model_path)
    if model_sha != expected_model_sha:
        raise InferenceError("policy.zip SHA-256 does not match the manifest")
    return manifest, model_path, model_sha


def _load_ppo(model_path: Path):
    try:
        import torch
        from stable_baselines3 import PPO
    except ImportError as exc:
        raise RuntimeError(
            "PPO inference requires optional dependencies; install liquiditylab[rl]"
        ) from exc
    torch.set_num_threads(1)
    try:
        torch.set_num_interop_threads(1)
    except RuntimeError:
        # Torch permits this setting only before its parallel work starts. The
        # worker still caps intra-op execution, its dominant CPU fanout.
        pass
    return PPO.load(model_path, device="cpu")


class InferenceWorker:
    """A single-flight deterministic PPO worker pinned to one verified bundle."""

    def __init__(
        self,
        manifest: dict[str, Any],
        model: Any,
        model_sha256: str,
        *,
        max_request_bytes: int = _DEFAULT_MAX_REQUEST_BYTES,
        max_validity_ns: int = _DEFAULT_MAX_VALIDITY_NS,
    ) -> None:
        if (
            isinstance(max_request_bytes, bool)
            or not isinstance(max_request_bytes, int)
            or not 256 <= max_request_bytes <= 1_048_576
        ):
            raise ValueError("max_request_bytes must be in [256,1048576]")
        if (
            isinstance(max_validity_ns, bool)
            or not isinstance(max_validity_ns, int)
            or not 1 <= max_validity_ns <= 10_000_000_000
        ):
            raise ValueError("max_validity_ns must be in [1,10000000000]")
        self.manifest = manifest
        self.model = model
        self.policy_sha256 = model_sha256
        self.max_request_bytes = max_request_bytes
        self.max_validity_ns = max_validity_ns
        self._inference_lock = threading.Lock()

    @property
    def promotion_status(self) -> str:
        # Report the bundle's status for inspection, but never infer eligibility.
        return self.manifest["status"]

    def policy_info(self) -> dict[str, Any]:
        return {
            "policy_sha256": self.policy_sha256,
            "observation_version": OBSERVATION_VERSION,
            "field_names": list(OBSERVATION_FIELDS),
            "promotion_status": self.promotion_status,
            "live_eligible": False,
        }

    def predict_action(
        self,
        binding: Any,
        observation: Any,
        *,
        request_bytes: int,
        is_active: Callable[[], bool],
        time_remaining: Callable[[], float | None],
        now_ns: Callable[[], int] = time.time_ns,
    ) -> int:
        """Validate and return a wire enum action; binding is never modified."""
        if (
            isinstance(request_bytes, bool)
            or not isinstance(request_bytes, int)
            or request_bytes < 0
            or request_bytes > self.max_request_bytes
        ):
            raise InferenceError("decision request exceeds maximum message size")
        required_strings = (
            "request_id",
            "tenant_id",
            "account_id",
            "session_id",
            "policy_sha256",
            "observation_version",
            "observation_schema_sha256",
            "market_snapshot_id",
        )
        for name in required_strings:
            value = getattr(binding, name, "")
            if not isinstance(value, str) or not value or len(value.encode("utf-8")) > 256:
                raise InferenceError(f"decision binding {name} is missing or too long")
        if binding.session_revision <= 0 or binding.account_revision <= 0:
            raise InferenceError("decision binding revisions must be positive")
        if binding.policy_sha256 != self.policy_sha256:
            raise InferenceError("decision policy SHA-256 does not match the pinned bundle")
        if binding.observation_version != OBSERVATION_VERSION:
            raise InferenceError("decision observation version is incompatible")
        if binding.observation_schema_sha256 != observation_schema_sha256():
            raise InferenceError("decision observation schema hash is incompatible")
        now = now_ns()
        expires = binding.expires_at_unix_ns
        if expires <= now:
            raise InferenceError("decision request has expired")
        if expires - now > self.max_validity_ns:
            raise InferenceError("decision expiry exceeds the configured validity window")
        remaining = time_remaining()
        if remaining is None or not math.isfinite(remaining) or remaining <= 0:
            raise InferenceError("decision RPC requires an active explicit deadline")
        if not is_active():
            raise InferenceError("decision RPC was cancelled before inference")

        if observation.schema_version != OBSERVATION_VERSION:
            raise InferenceError("observation version is incompatible")
        if tuple(observation.field_names) != OBSERVATION_FIELDS:
            raise InferenceError("observation fields or order are incompatible")
        if len(observation.features) != OBSERVATION_SIZE:
            raise InferenceError(f"observation must contain exactly {OBSERVATION_SIZE} features")
        try:
            features = [float(value) for value in observation.features]
        except (TypeError, ValueError, OverflowError) as exc:
            raise InferenceError("observation contains a malformed feature") from exc
        if not all(math.isfinite(value) for value in features):
            raise InferenceError("observation contains a non-finite feature")

        if not self._inference_lock.acquire(blocking=False):
            raise BusyInference("inference worker is at capacity")
        try:
            # Recheck after waiting for validation; requests must not begin after
            # their caller has timed out or cancelled.
            remaining = time_remaining()
            if (
                remaining is None
                or not math.isfinite(remaining)
                or remaining <= 0
                or not is_active()
            ):
                raise InferenceError("decision RPC expired or was cancelled before model start")
            if expires <= now_ns():
                raise InferenceError("decision request expired before model start")
            import numpy as np

            vector = np.asarray(features, dtype=np.float32).reshape(1, OBSERVATION_SIZE)
            action, _ = self.model.predict(vector, deterministic=True)
            action_array = np.asarray(action)
            if action_array.size != 1:
                raise InferenceError("policy must return exactly one semantic action")
            action_value = action_array.item()
            if isinstance(action_value, bool) or not isinstance(action_value, (int, float)):
                raise InferenceError("policy returned a non-integer semantic action")
            if (
                not math.isfinite(float(action_value))
                or int(action_value) != action_value
                or not 0 <= int(action_value) <= 5
            ):
                raise InferenceError("policy action must be an integer in [0,5]")
            remaining = time_remaining()
            if (
                not is_active()
                or remaining is None
                or not math.isfinite(remaining)
                or remaining <= 0
                or expires <= now_ns()
            ):
                raise InferenceError("decision expired during inference")
            wire_action = int(action_value) + 1
            if not _WIRE_ACTION_MIN <= wire_action <= _WIRE_ACTION_MAX:
                raise InferenceError("semantic action failed wire mapping")
            return wire_action
        finally:
            self._inference_lock.release()


class BusyInference(InferenceError):
    """The worker has no spare inference slot; callers should fall back."""


def load_worker(
    bundle_dir: str | Path,
    *,
    model: Any | None = None,
    model_loader: Callable[[Path], Any] | None = None,
    max_request_bytes: int = _DEFAULT_MAX_REQUEST_BYTES,
    max_validity_ns: int = _DEFAULT_MAX_VALIDITY_NS,
) -> InferenceWorker:
    """Verify Go-v1 provenance and policy bytes before loading the frozen PPO."""
    # Keep the final path component visible to _load_manifest so it can reject
    # a symlinked bundle directory instead of resolving through it first.
    directory = Path(bundle_dir).expanduser().absolute()
    manifest, model_path, model_sha = _load_manifest(directory)
    if model is None:
        loader = model_loader or _load_ppo
        model = loader(model_path)
    return InferenceWorker(
        manifest,
        model,
        model_sha,
        max_request_bytes=max_request_bytes,
        max_validity_ns=max_validity_ns,
    )


def _grpc_module():
    try:
        import grpc

        from ..v1 import execution_pb2 as pb
        from ..v1 import execution_pb2_grpc as pb_grpc
    except ImportError as exc:
        raise RuntimeError(
            "gRPC inference requires optional dependencies; install liquiditylab[go]"
        ) from exc
    return grpc, pb, pb_grpc


def _servicer(worker: InferenceWorker):
    grpc, pb, pb_grpc = _grpc_module()

    class PolicyService(pb_grpc.PolicyServiceServicer):
        def Predict(self, request, context):
            try:
                if request.ByteSize() > worker.max_request_bytes:
                    raise InferenceError("decision request exceeds maximum message size")
                if not request.HasField("binding") or not request.HasField("observation"):
                    raise InferenceError("decision request requires binding and observation")
                wire_action = worker.predict_action(
                    request.binding,
                    request.observation,
                    request_bytes=request.ByteSize(),
                    is_active=context.is_active,
                    time_remaining=context.time_remaining,
                )
                response = pb.DecisionResponse()
                response.binding.CopyFrom(request.binding)
                response.action = wire_action
                response.mode = worker.promotion_status
                return response
            except BusyInference as exc:
                context.abort(grpc.StatusCode.RESOURCE_EXHAUSTED, str(exc))
            except InferenceError as exc:
                context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(exc))
            except Exception as exc:
                context.abort(grpc.StatusCode.INTERNAL, f"inference failed: {type(exc).__name__}")

        def GetPolicyInfo(self, request, context):
            if request.policy_sha256 != worker.policy_sha256:
                context.abort(grpc.StatusCode.NOT_FOUND, "requested policy is not loaded")
            info = worker.policy_info()
            response = pb.PolicyInfoResponse(
                policy_sha256=info["policy_sha256"],
                observation_version=info["observation_version"],
                field_names=info["field_names"],
                promotion_status=info["promotion_status"],
                live_eligible=False,
            )
            return response

    return PolicyService()


def _validate_local_address(address: str) -> tuple[str, Path | None]:
    if not isinstance(address, str) or not address:
        raise ValueError("listen address is required")
    if address.startswith("unix:"):
        path = Path(address[5:]).expanduser().absolute()
        if not path.is_absolute() or str(path) == "/" or "\0" in str(path):
            raise ValueError("Unix socket path must name a file")
        try:
            path.lstat()
        except FileNotFoundError:
            pass
        else:
            raise ValueError("refusing to replace an existing socket or file")
        if len(os.fsencode(path)) > 103:
            raise ValueError("Unix socket path is too long")
        return "unix:" + str(path), path
    host, sep, port_text = address.rpartition(":")
    if not sep or not port_text.isdecimal():
        raise ValueError("listen address must be unix:/path or a loopback host:port")
    host = host.strip("[]")
    try:
        ip = ipaddress.ip_address(host)
    except ValueError as exc:
        raise ValueError("TCP inference listener must use a numeric loopback address") from exc
    if not ip.is_loopback:
        raise ValueError("inference listener is local-only; non-loopback addresses are rejected")
    port = int(port_text)
    if not 0 <= port <= 65535:
        raise ValueError("listen port must be in [0,65535]")
    target_host = f"[{host}]" if ":" in host else host
    return f"{target_host}:{port}", None


def start_server(worker: InferenceWorker, listen: str = "unix:/tmp/liquiditylab-inference.sock"):
    """Start a bounded gRPC server on Unix or a numeric loopback address."""
    grpc, _, pb_grpc = _grpc_module()
    target, socket_path = _validate_local_address(listen)
    executor = ThreadPoolExecutor(max_workers=1, thread_name_prefix="liquiditylab-inference")
    server = grpc.server(
        executor,
        maximum_concurrent_rpcs=1,
        options=(
            ("grpc.max_receive_message_length", worker.max_request_bytes),
            ("grpc.max_send_message_length", 4096),
        ),
    )
    pb_grpc.add_PolicyServiceServicer_to_server(_servicer(worker), server)
    old_umask = os.umask(0o177)
    try:
        bound_port = server.add_insecure_port(target)
        if not bound_port:
            raise RuntimeError(f"could not bind local inference listener {target}")
        server.start()
    except BaseException:
        server.stop(0)
        executor.shutdown(wait=False, cancel_futures=True)
        raise
    finally:
        os.umask(old_umask)
    if socket_path is not None:
        try:
            os.chmod(socket_path, stat.S_IRUSR | stat.S_IWUSR)
        except OSError:
            server.stop(0).wait(timeout=2)
            executor.shutdown(wait=True, cancel_futures=True)
            raise
        active_target = target
    else:
        host, _, port_text = target.rpartition(":")
        active_target = f"{host}:{bound_port}" if port_text == "0" else target
    return server, executor, active_target


def serve(
    bundle_dir: str | Path,
    listen: str = "unix:/tmp/liquiditylab-inference.sock",
    *,
    max_request_bytes: int = _DEFAULT_MAX_REQUEST_BYTES,
) -> None:
    """Load one immutable bundle and serve until graceful process shutdown."""
    worker = load_worker(bundle_dir, max_request_bytes=max_request_bytes)
    server, executor, _ = start_server(worker, listen)
    socket_path = Path(listen[5:]).expanduser().absolute() if listen.startswith("unix:") else None
    socket_identity = None
    if socket_path is not None:
        socket_stat = socket_path.lstat()
        socket_identity = (socket_stat.st_dev, socket_stat.st_ino)
    previous = {}

    def stop(signum, _frame):
        server.stop(grace=5)

    for signum in (signal.SIGINT, signal.SIGTERM):
        previous[signum] = signal.signal(signum, stop)
    try:
        server.wait_for_termination()
    finally:
        server.stop(grace=5).wait(timeout=6)
        executor.shutdown(wait=True, cancel_futures=True)
        if socket_path is not None:
            try:
                current = socket_path.lstat()
            except FileNotFoundError:
                pass
            else:
                if (
                    stat.S_ISSOCK(current.st_mode)
                    and (current.st_dev, current.st_ino) == socket_identity
                ):
                    socket_path.unlink()
        for signum, handler in previous.items():
            signal.signal(signum, handler)


def main(argv: list[str] | None = None) -> int:
    import argparse

    parser = argparse.ArgumentParser(description="Serve a pinned replay-only PPO policy locally")
    parser.add_argument("bundle", type=Path)
    parser.add_argument("--listen", default="unix:/tmp/liquiditylab-inference.sock")
    parser.add_argument("--max-request-bytes", type=int, default=_DEFAULT_MAX_REQUEST_BYTES)
    args = parser.parse_args(argv)
    serve(args.bundle, args.listen, max_request_bytes=args.max_request_bytes)
    return 0

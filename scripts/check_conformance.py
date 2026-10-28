"""Check generated contracts, Python reference vectors, and both runtimes.

Run with the locked dev, go, codegen, and rl extras installed. Go codegen plugins
must be available as documented in docs/protocol.md. No fixture is rewritten.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import signal
import subprocess
import sys
import tempfile
import time
import xml.etree.ElementTree as ET
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def run_step(name, command, env, timeout):
    """Own the entire command process group, including on interruption."""
    started = time.monotonic_ns()
    result = {"name": name, "status": "failed"}
    process = subprocess.Popen(command, cwd=ROOT, env=env, start_new_session=True)
    try:
        try:
            code = process.wait(timeout=timeout)
            result.update(status="passed" if code == 0 else "failed", exit_code=code)
        except subprocess.TimeoutExpired:
            result["status"] = "timed_out"
    finally:
        # A failed parent can leave a worker alive in our process group.
        # Always close that group, including when the parent already exited.
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        if process.poll() is None:
            try:
                process.wait(timeout=1)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.wait(timeout=5)
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        result["elapsed_ns"] = time.monotonic_ns() - started
    return result


def check_python_results(path):
    """Optional dependency skips cannot silently pass the contract gate."""
    suites = list(ET.parse(path).getroot().iter("testsuite"))
    totals = {key: sum(int(s.attrib.get(key, 0)) for s in suites)
              for key in ("tests", "failures", "errors", "skipped")}
    if not totals["tests"] or any(totals[key] for key in ("failures", "errors", "skipped")):
        raise ValueError("Python contract gate requires passing tests without skips")
    return totals


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", type=Path, help="new optional JSON evidence file (0600)")
    parser.add_argument("--step-timeout", type=int, default=180,
                        help="seconds per command, 1..600 (default 180)")
    args = parser.parse_args()
    if not 1 <= args.step_timeout <= 600:
        parser.error("step timeout must be within 1..600 seconds")
    if os.name != "posix":
        parser.error("this local harness requires POSIX process-group supervision")
    output = None
    if args.report:
        try:
            descriptor = os.open(args.report, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            output = os.fdopen(descriptor, "w")
        except OSError:
            parser.error("report must be a fresh writable path in an existing directory")
    report = {"schema": "liquiditylab-contract-check-v1", "status": "failed", "steps": [],
              "step_timeout_seconds": args.step_timeout, "python_version": sys.version.split()[0],
              "limits": ["Contract/replay conformance only; no venue, capacity, or policy-quality gate.",
                         "Go race coverage here is domain/execution/replay/RPC; not the full product suite."]}
    try:
        env = dict(os.environ, OMP_NUM_THREADS="1", MKL_NUM_THREADS="1",
                   OPENBLAS_NUM_THREADS="1", LIQUIDITYLAB_PYTHON=sys.executable, GOFLAGS="")
        for key, command in (("revision", ["git", "rev-parse", "HEAD"]),
                             ("dirty", ["git", "status", "--porcelain"])):
            value = subprocess.run(command, cwd=ROOT, capture_output=True, text=True,
                                   timeout=5, check=True).stdout.strip()
            report.setdefault("source", {})[key] = bool(value) if key == "dirty" else value
        with tempfile.TemporaryDirectory(prefix="liquiditylab-contract-check-") as temp:
            binary = Path(temp) / "liquiditylabd"
            junit = Path(temp) / "python.xml"
            env["LIQUIDITYLAB_REPLAY_BINARY"] = str(binary)
            steps = [
                ("generated-bindings", [sys.executable, "scripts/generate_protocol.py", "--check"]),
                ("python-reference-vectors", [sys.executable, "conformance/regenerate.py", "--check"]),
                ("go-contract-replay-race", ["go", "test", "-race", "-count=1", "./internal/domain",
                                            "./internal/execution", "./internal/replay",
                                            "./internal/replayrpc"]),
                ("owned-replay-build", ["go", "build", "-buildvcs=true", "-tags=", "-race=false",
                                         "-o", str(binary),
                                         "./cmd/liquiditylabd"]),
                ("python-suite", [sys.executable, "-m", "pytest", "-q", f"--junitxml={junit}"]),
            ]
            for name, command in steps:
                print(f"Conformance: {name}", flush=True)
                result = run_step(name, command, env, args.step_timeout)
                report["steps"].append(result)
                if result["status"] != "passed":
                    return 1
            report["python_tests"] = check_python_results(junit)
            report["replay_binary_sha256"] = hashlib.sha256(binary.read_bytes()).hexdigest()
            report["fixture_sha256"] = {
                name: hashlib.sha256((ROOT / "conformance" / name).read_bytes()).hexdigest()
                for name in ("golden.json", "contracts.json", "time-compatibility.json")
            }
            report["status"] = "passed"
            print("Contract conformance passed; generated files and fixtures unchanged.", flush=True)
            return 0
    except (OSError, ValueError, ET.ParseError, subprocess.SubprocessError):
        print("Conformance setup or result validation failed.", file=sys.stderr)
        return 1
    finally:
        if output is not None:
            with output:
                json.dump(report, output, indent=2)
                output.write("\n")
                output.flush()
                os.fsync(output.fileno())


if __name__ == "__main__":
    raise SystemExit(main())

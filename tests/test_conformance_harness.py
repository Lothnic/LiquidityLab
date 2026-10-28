"""The conformance command must disclose missing checks and bounded failures."""

import os
import runpy
import subprocess
import sys
import time
from pathlib import Path

import pytest

HARNESS = runpy.run_path(str(Path(__file__).resolve().parents[1] / "scripts/check_conformance.py"))


@pytest.mark.parametrize("attribute", ["skipped", "failures", "errors"])
def test_conformance_rejects_incomplete_python_gate(tmp_path, attribute):
    result = tmp_path / "results.xml"
    result.write_text(f'<testsuites><testsuite tests="1" {attribute}="1"/></testsuites>')
    with pytest.raises(ValueError, match="without skips"):
        HARNESS["check_python_results"](result)


def test_conformance_requires_actual_tests(tmp_path):
    result = tmp_path / "results.xml"
    result.write_text('<testsuites><testsuite tests="0"/></testsuites>')
    with pytest.raises(ValueError):
        HARNESS["check_python_results"](result)
    result.write_text('<testsuites><testsuite tests="3" skipped="0"/></testsuites>')
    assert HARNESS["check_python_results"](result)["tests"] == 3


def test_conformance_command_failure_is_not_success():
    result = HARNESS["run_step"]("failure", [sys.executable, "-c", "raise SystemExit(7)"],
                                 os.environ.copy(), 2)
    assert result["status"] == "failed" and result["exit_code"] == 7


def test_conformance_command_timeout_is_bounded():
    result = HARNESS["run_step"]("timeout", [sys.executable, "-c", "import time; time.sleep(10)"],
                                 os.environ.copy(), 0.05)
    assert result["status"] == "timed_out" and result["elapsed_ns"] < 3_000_000_000


def test_conformance_cleans_workers_after_parent_exit(tmp_path):
    pid_file = tmp_path / "owned-worker.pid"
    code = ("import subprocess,sys;from pathlib import Path;"
            "p=subprocess.Popen([sys.executable,'-c','import time;time.sleep(30)']);"
            "Path(sys.argv[1]).write_text(str(p.pid))")
    result = HARNESS["run_step"]("parent-exit", [sys.executable, "-c", code, str(pid_file)],
                                 os.environ.copy(), 2)
    assert result["status"] == "passed"
    pid = int(pid_file.read_text())
    status = Path(f"/proc/{pid}/stat")
    # Orphaned zombies await the host reaper but cannot execute further work.
    deadline = time.monotonic() + 1
    while time.monotonic() < deadline:
        try:
            if status.read_text().rsplit(")", 1)[1].split()[0] == "Z":
                return
        except FileNotFoundError:
            return
        time.sleep(0.005)
    pytest.fail("owned worker survived command-group cleanup")


def test_conformance_refuses_existing_evidence_before_running_commands(tmp_path):
    output = tmp_path / "keep.json"
    output.write_text("original evidence")
    script = Path(__file__).resolve().parents[1] / "scripts/check_conformance.py"
    result = subprocess.run([sys.executable, str(script), "--report", str(output)],
                            capture_output=True, text=True, timeout=3)
    assert result.returncode != 0 and output.read_text() == "original evidence"
    assert "Conformance:" not in result.stdout

"""Check resolved log bounds without starting containers or contacting a daemon."""

import json
import os
import subprocess
from pathlib import Path


def test_every_compose_service_has_bounded_logs():
    root = Path(__file__).resolve().parents[1]
    result = subprocess.run(
        [
            "docker",
            "compose",
            "--profile",
            "prepare",
            "-f",
            str(root / "deployment" / "compose.yaml"),
            "config",
            "--format",
            "json",
        ],
        env={**os.environ, "PAPER_MODE": "resume", "DOCKER_CONTEXT": "default"},
        capture_output=True,
        text=True,
        timeout=30,
        check=True,
    )
    services = json.loads(result.stdout)["services"]
    assert set(services) == {"db", "train", "worker", "paper", "edge"}
    for name, service in services.items():
        assert service["logging"] == {
            "driver": "json-file",
            "options": {"max-size": "10m", "max-file": "3"},
        }, name

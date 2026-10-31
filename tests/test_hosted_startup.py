"""Hosted cold starts must never create or reset a retained paper account."""

import importlib.util
from pathlib import Path

import pytest

_PATH = Path(__file__).resolve().parents[1] / "deployment" / "hosted" / "serve.py"
_SPEC = importlib.util.spec_from_file_location("hosted_serve", _PATH)
serve = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(serve)


@pytest.fixture
def env():
    return {
        "LIQUIDITYLAB_DATABASE_URL": "postgres://test:private-password@host/db",
        "LIQUIDITYLAB_API_TOKEN": "private-test-token-01234567890123456789",
    }


def test_cold_start_only_resumes_and_keeps_credentials_out_of_argv(env):
    port, command = serve.configuration(env)
    assert port == 10000
    assert command[1:3] == ["paper", "resume"]
    assert "--initial-base" not in command
    assert "--initial-quote" not in command
    assert env["LIQUIDITYLAB_DATABASE_URL"] not in " ".join(command)
    assert env["LIQUIDITYLAB_API_TOKEN"] not in " ".join(command)
    assert "--policy-target" not in command
    assert command[command.index("--source") + 1] == "kraken"
    assert command[command.index("--capture-output") + 1].startswith("/tmp/liquiditylab-kraken-")


def test_create_is_refused_on_hosted_restart(env):
    env["PAPER_MODE"] = "create"
    with pytest.raises(ValueError, match="only resumes"):
        serve.configuration(env)


@pytest.mark.parametrize("port", ["8080", "0", "80", "65536", "10000;anything"])
def test_proxy_rejects_private_port_collision_and_invalid_ports(env, port):
    env["PORT"] = port
    with pytest.raises(ValueError):
        serve.configuration(env)


@pytest.mark.parametrize("schema", ["public", "pg_catalog", "information_schema", "a;drop"])
def test_system_or_invalid_schema_is_refused(env, schema):
    env["PAPER_SCHEMA"] = schema
    with pytest.raises(ValueError):
        serve.configuration(env)


def test_partial_rl_configuration_is_refused(env):
    env["LIQUIDITYLAB_POLICY_TARGET"] = "unix:/missing-worker.sock"
    with pytest.raises(ValueError, match="does not enable an RL worker"):
        serve.configuration(env)


@pytest.mark.parametrize("key", ["LIQUIDITYLAB_DATABASE_URL", "LIQUIDITYLAB_API_TOKEN"])
def test_missing_credentials_fail_without_printing_other_secrets(env, key):
    del env[key]
    with pytest.raises(ValueError) as error:
        serve.configuration(env)
    assert "private-password" not in str(error.value)
    assert "private-test-token" not in str(error.value)

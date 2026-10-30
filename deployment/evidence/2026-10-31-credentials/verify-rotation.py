"""Read-only credential probes; stop/recreate ONLY the isolated restored fixture."""
from pathlib import Path
import datetime
import hashlib
import json
import os
import secrets
import ssl
import stat
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

W = Path(__file__).resolve().parent
R = W.parents[1] / "outputs/liquiditylab"
V = json.loads((W / "environment.json").read_text())
assert V["COMPOSE_PROJECT_NAME"] == "liquiditylab-restore-20261031-b"
CMD = ["docker", "--context", "default", "compose", "--project-name", V["COMPOSE_PROJECT_NAME"],
       "--file", str(R / "deployment/compose.yaml"), "--file", str(R / "deployment/compose.tls.yaml"),
       "--file", str(W / "fixture.compose.json")]
ENV = {**os.environ, **V}
CTX = ssl.create_default_context(cafile=str(W / "test-ca.pem"))
ACCOUNT = "/v1/accounts/synthetic-paper-daemon/demo"
BASE = "https://127.0.0.1:18884"
OUT = W / "rotation"
OUT.mkdir(mode=0o700, exist_ok=False)
os.umask(0o077)
old_token = (W / "api_token").read_text().strip()
private_values = [old_token, (W / "test-key.pem").read_text().strip(),
                  (W.parents[0] / "paper-backup-drill-20261031/restored-final/postgres_password").read_text().strip()]
result = {"observed_at_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
          "project": V["COMPOSE_PROJECT_NAME"], "valid_trading_commands_issued": 0,
          "transport": "HTTPS; hostname and explicit local test CA verified"}

def compose(label, *args):
    p = subprocess.run([*CMD, *args], env=ENV, capture_output=True, timeout=150)
    (OUT / (label + ".private.log")).write_bytes(p.stdout + p.stderr)
    assert p.returncode == 0, f"{label}: Docker exit {p.returncode}; inspect private log"

def request(path, token=None):
    headers = {"Authorization": "Bearer " + token} if token is not None else {}
    req = urllib.request.Request(BASE + path, headers=headers)
    try:
        with urllib.request.urlopen(req, context=CTX, timeout=8) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        return error.code, None

def ready(token):
    for _ in range(60):
        status, account = request(ACCOUNT, token)
        if status == 200 and account["ready"] and not account["recovery_required"]:
            return account
        time.sleep(0.5)
    raise AssertionError("Account did not become ready")

def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()

reports = [("completed", "container-ppo-handoff-20261030-b", 3569),
           ("cancelled", "container-ppo-handoff-loss-20261030-c", 3736)]

def inspect_reports(token):
    retained = {}
    for name, plan, revision in reports:
        status, report = request(ACCOUNT + "/sessions/" + plan + "?as_of_revision=" + str(revision), token)
        expected = json.loads((W.parents[0] / "paper-backup-drill-20261031" / ("before-stop-" + name + "-report.json")).read_text())
        assert status == 200 and report == expected
        retained[name] = {"revision": revision, "canonical_sha256": hashlib.sha256(canonical(report)).hexdigest(),
                          "exact_original_report": True}
    return retained

try:
    compose("start", "up", "-d", "--no-build", "--pull", "never", "--wait", "--wait-timeout", "120", "db", "worker", "paper", "edge")
    before = ready(old_token)
    result["before"] = {"old_token_account_http": 200, "revision": before["revision"], "reports": inspect_reports(old_token)}
    compose("stop-edge", "stop", "edge")
    compose("stop-paper", "stop", "paper")
    paper_id = subprocess.check_output([*CMD, "ps", "--all", "--quiet", "paper"], env=ENV, text=True).strip()
    p = subprocess.run(["docker", "--context", "default", "inspect", "--format", "{{json .State}}", paper_id], capture_output=True, text=True, check=True)
    state = json.loads(p.stdout)
    assert not state["Running"] and state["ExitCode"] == 0
    log = subprocess.check_output(["docker", "--context", "default", "logs", paper_id], stderr=subprocess.STDOUT, text=True)
    assert "known_final" in log
    result["old_process"] = {"container_id": paper_id, "running": False, "exit_code": 0, "known_final_logged": True}
    assert stat.S_IMODE(W.stat().st_mode) == 0o700
    assert stat.S_IMODE((W / "api_token").stat().st_mode) == 0o600
    new_token = secrets.token_hex(32)
    private_values.append(new_token)
    fd, tmp_name = tempfile.mkstemp(prefix=".api_token.", dir=W)
    try:
        with os.fdopen(fd, "w") as stream:
            stream.write(new_token + "\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(tmp_name, W / "api_token")
        directory_fd = os.open(W, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        Path(tmp_name).unlink(missing_ok=True)
    compose("recreate", "up", "-d", "--force-recreate", "--no-build", "--pull", "never", "--wait", "--wait-timeout", "120", "paper", "edge")
    after = ready(new_token)
    assert request(ACCOUNT, old_token)[0] == 401
    assert request(ACCOUNT)[0] == 401
    assert request("/v1/accounts/synthetic-paper-daemon/wrong-account", new_token)[0] == 404
    assert request("/healthz")[0] == 200
    for field in ("balances", "reserved", "plan", "child", "fills", "fill_count", "policy_config", "policy_runtime"):
        assert before[field] == after[field], field
    assert after["child"] is None and after["plan_status"] == "cancelled"
    assert all(item["amount"] == "0" for item in after["reserved"])
    result["after"] = {"new_token_account_http": 200, "old_token_account_http": 401,
                       "unauthenticated_account_http": 401, "valid_token_wrong_account_http": 404,
                       "health_http": 200, "ready": True, "recovery_required": False,
                       "revision": after["revision"], "balances": after["balances"], "reserved": after["reserved"],
                       "plan_status": after["plan_status"], "plan_id": after["plan"]["id"], "child": after["child"],
                       "reports": inspect_reports(new_token), "economic_projection_unchanged": True,
                       "policy_config_and_runtime_unchanged": True}
    for name, account in (("before", before), ("after", after)):
        (OUT / (name + "-account.json")).write_text(json.dumps(account, indent=2) + "\n")
finally:
    compose("final-stop", "stop", "edge", "paper", "worker", "db")

result["fixture_services_stopped_volumes_retained"] = True
result["completed_at_utc"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
encoded = json.dumps(result, indent=2) + "\n"
for value in private_values:
    assert value not in encoded, "Private value in public result"
(OUT / "verification.json").write_text(encoded)
print(encoded)

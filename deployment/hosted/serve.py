"""Run the baseline paper daemon behind a public proxy on Render's PORT.

The account must be created deliberately before deploying with PAPER_MODE=resume.
Neither a cold start nor a failed resume can create or reset an account.
"""

import os
import re
import signal
import subprocess
import sys
import tempfile
import time
from pathlib import Path


def configuration(env):
    port = env.get("PORT", "10000")
    if not re.fullmatch(r"[0-9]{1,5}", port) or not 1024 <= int(port) <= 65535:
        raise ValueError("PORT must be a nonprivileged numeric TCP port")
    if int(port) == 8080:
        raise ValueError("PORT must differ from the private daemon port 8080")
    schema = env.get("PAPER_SCHEMA", "hosted_paper_demo")
    account = env.get("PAPER_ACCOUNT", "demo")
    if not re.fullmatch(r"[a-z][a-z0-9_]{0,62}", schema):
        raise ValueError("PAPER_SCHEMA must be an isolated SQL identifier")
    if schema in {"public", "information_schema"} or schema.startswith("pg_"):
        raise ValueError("PAPER_SCHEMA must not be a system or public schema")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", account):
        raise ValueError("PAPER_ACCOUNT must be an account identifier")
    if env.get("PAPER_MODE", "resume") != "resume":
        raise ValueError("Hosted startup only resumes a deliberately created account")
    if not env.get("LIQUIDITYLAB_DATABASE_URL"):
        raise ValueError("LIQUIDITYLAB_DATABASE_URL is required")
    token = env.get("LIQUIDITYLAB_API_TOKEN", "")
    if not 32 <= len(token) <= 4096 or any(c in token for c in " \t\r\n"):
        raise ValueError("A private LIQUIDITYLAB_API_TOKEN is required")
    if any(env.get(key) for key in (
        "LIQUIDITYLAB_POLICY_TARGET", "LIQUIDITYLAB_POLICY_SHA256",
        "LIQUIDITYLAB_POLICY_FALLBACK", "LIQUIDITYLAB_POLICY_ALLOW_UNVALIDATED",
    )):
        raise ValueError("This baseline deployment does not enable an RL worker")
    command = [
        "/usr/local/bin/liquiditylabd", "paper", "resume",
        "--listen", "127.0.0.1:8080", "--schema", schema, "--account", account,
        "--source", "synthetic", "--web-dir", "/app/web", "--duration", "24h",
    ]
    return int(port), command


def proxy_config(port, directory):
    return f"""worker_processes 1;
pid {directory}/nginx.pid;
error_log stderr warn;
events {{ worker_connections 128; }}
http {{
  access_log off;
  server_tokens off;
  client_body_temp_path {directory}/body;
  proxy_temp_path {directory}/proxy;
  fastcgi_temp_path {directory}/fastcgi;
  uwsgi_temp_path {directory}/uwsgi;
  scgi_temp_path {directory}/scgi;
  client_header_timeout 10s;
  client_body_timeout 10s;
  keepalive_timeout 30s;
  server {{
    listen 0.0.0.0:{port};
    client_max_body_size 64k;
    location / {{
      proxy_pass http://127.0.0.1:8080;
      proxy_http_version 1.1;
      proxy_set_header Host $host;
      proxy_buffering off;
      proxy_connect_timeout 5s;
      proxy_send_timeout 15s;
      proxy_read_timeout 65s;
    }}
  }}
}}
"""


def main():
    try:
        port, command = configuration(os.environ)
    except ValueError as error:
        print(f"Hosted startup refused: {error}", file=sys.stderr, flush=True)
        return 1
    stopping = False
    children = []

    def stop(signum, frame):
        nonlocal stopping
        stopping = True

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    with tempfile.TemporaryDirectory(prefix="liquiditylab-proxy-") as directory:
        config = Path(directory) / "nginx.conf"
        config.write_text(proxy_config(port, directory))
        try:
            children.append(subprocess.Popen(command))
            children.append(subprocess.Popen([
                "nginx", "-c", str(config), "-g", "daemon off;",
            ]))
            print("Hosted synthetic paper demo starting; RL disabled", flush=True)
            while not stopping and all(child.poll() is None for child in children):
                time.sleep(0.2)
            result = 0 if stopping else 1
        finally:
            # Stop accepting HTTP before asking Go to drain durable account work.
            for child in reversed(children):
                if child.poll() is None:
                    child.terminate()
            deadline = time.monotonic() + 25
            for child in reversed(children):
                try:
                    child.wait(timeout=max(0.1, deadline - time.monotonic()))
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait()
    return result


if __name__ == "__main__":
    sys.exit(main())

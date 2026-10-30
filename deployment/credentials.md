# Beta account credentials

This beta uses one invite-only bearer token for one paper account on one daemon
instance. The token is an account credential, not an invitation system or a
multi-account role. Provision a separate daemon and account for each invitee.
The operator must deliberately choose and approve a private channel for delivery
to the intended person. Never put the token in URLs, shell history, command
arguments, logs, screenshots, or repository files. Chat or email may be used
only when the operator has approved that specific channel for this credential.

Anyone with Docker access on the host is trusted: they can inspect containers,
mounts, environment and secret files. Restrict host and repository access to
the operators who administer this instance.

## Initialize an instance

From the repository root, initialize the two local Compose secrets once:

```sh
deployment/demo.sh init
```

The script creates `deployment/secrets/api_token` and
`deployment/secrets/postgres_password` only when absent. Each is 32 random bytes
encoded as hex. It uses `umask 077`; the `deployment/secrets` directory and
files must remain owner-only and are ignored by Git. Re-running `init` preserves
existing files. It does not rotate either credential.

The paper entrypoint reads the API token from the mounted file at process start,
then exports it to the Go daemon. It does not reload that value. The daemon
accepts the token for its configured account; use a separate daemon/account for
each invitee. The TLS edge forwards the `Authorization` header to Go. The
The built frontend contains no token. It keeps the entered token in component
memory for that tab and clears it on disconnect. There is no JWT/session refresh or
non-disruptive token rotation mechanism.

Use the existing instance's Docker context, Compose project, schema, account,
policy bundle and secrets. For the base local HTTP profile, `PAPER_PORT` is
loopback-only. To use the optional TLS profile, set its file paths and port in
the operator shell; the path variables are not credential values:

```sh
export DOCKER_CONTEXT=default
export COMPOSE_PROJECT_NAME=liquiditylab-paper-beta
export PAPER_TLS_CERTIFICATE=/absolute/private/path/fullchain.pem
export PAPER_TLS_PRIVATE_KEY=/absolute/private/path/privkey.pem
export PAPER_TLS_PORT=8443
export PAPER_MODE=resume

docker compose -p "$COMPOSE_PROJECT_NAME" \
  -f deployment/compose.yaml -f deployment/compose.tls.yaml config --quiet
docker compose -p "$COMPOSE_PROJECT_NAME" \
  -f deployment/compose.yaml -f deployment/compose.tls.yaml up -d db worker
docker compose -p "$COMPOSE_PROJECT_NAME" \
  -f deployment/compose.yaml -f deployment/compose.tls.yaml up -d \
  --force-recreate --wait --wait-timeout 120 paper edge
```

The TLS overlay replaces the base HTTP publication with
`127.0.0.1:${PAPER_TLS_PORT}:8443`; it does not publish a public endpoint. Keep
using the same overlay for subsequent operations on that instance. Without the
overlay, the base profile publishes HTTP only to `127.0.0.1:${PAPER_PORT}`.
Do not send a bearer token over an unverified connection. A local self-signed
test certificate is for explicitly trusting test clients, not a public beta
certificate.

## Rotate a token

Plan a maintenance window and tell the invitee when their connection will stop.
Stopping the paper daemon is disruptive: graceful shutdown cancels an active
plan and tries to settle it to known finality. It does not merely pause an
active plan. A terminal cancelled plan cannot be restarted; create a new plan if
trading is approved again. Inspect the account afterward and use the recovery
procedure if shutdown did not reach known finality. If the daemon crashes,
recovery leaves the existing plan paused and requires an explicit resume action.
A later `resume` startup recovers the account but never starts or resumes trading
automatically.

Use the same Compose project and profile currently in service. Stop edge first,
then paper. Do not run `down -v` or remove any retained volumes:

```sh
export DOCKER_CONTEXT=default
export COMPOSE_PROJECT_NAME=liquiditylab-paper-beta
export PAPER_MODE=resume

# Base HTTP profile:
docker compose -p "$COMPOSE_PROJECT_NAME" -f deployment/compose.yaml stop edge paper

# For a TLS deployment, use both Compose files instead:
# docker compose -p "$COMPOSE_PROJECT_NAME" -f deployment/compose.yaml \
#   -f deployment/compose.tls.yaml stop edge paper
```

After both services have stopped, replace only the API token using a private
temporary file in the same directory. The checks refuse an unexpectedly
permissive directory or temporary file. The rename is atomic because both paths
are on the same filesystem. This does not change the PostgreSQL password. The
example uses GNU `stat -c` and applies to Linux hosts.

```sh
set -eu
umask 077
secrets=deployment/secrets
test "$(stat -c %a "$secrets")" = 700
tmp=$(mktemp "$secrets/.api_token.XXXXXX")
trap 'rm -f -- "$tmp"' EXIT HUP INT TERM
openssl rand -hex 32 > "$tmp"
chmod 600 "$tmp"
test "$(stat -c %a "$tmp")" = 600
mv -f -- "$tmp" "$secrets/api_token"
trap - EXIT HUP INT TERM
```

Do not print the new token. Recreate paper and edge so the mounted secret and
Go startup environment use the replacement. Leave worker, bundle, database,
account and recordings in place:

```sh
# Base HTTP profile:
PAPER_MODE=resume docker compose -p "$COMPOSE_PROJECT_NAME" \
  -f deployment/compose.yaml up -d --force-recreate \
  --wait --wait-timeout 120 paper edge

# TLS deployment (retain the PAPER_TLS_* path/port settings above):
# PAPER_MODE=resume docker compose -p "$COMPOSE_PROJECT_NAME" \
#   -f deployment/compose.yaml -f deployment/compose.tls.yaml up -d \
#   --force-recreate --wait --wait-timeout 120 paper edge
```

There is no non-disruptive rotation. Once the old paper process has stopped, its
instance cannot use the old credential. After recreation, requests with the old
token should receive HTTP 401. Do not copy a compromised token into a rollback file
or redistribute it. If an existing private operator record contains the old
token, protect or remove it according to the applicable retention policy; do not
restore a compromised credential.

## Revoke access

To revoke access without issuing a replacement, stop `edge` and `paper` as
above and leave the API offline. To keep the service available while withholding
access from the former invitee, rotate to a fresh token, recreate the services,
and keep the replacement private until a new invitation is deliberately
approved. Once the old Go process has stopped, that instance cannot use the old
credential; after recreation, the old token should receive HTTP 401.

## Verify the change

Check the health endpoint, then make read-only account requests with the old
token and new token. A revoked or otherwise invalid token should receive HTTP
401; a valid token for the wrong account should receive HTTP 404. The new token
should receive HTTP 200 for the configured account. Check readiness and retained
session reports before inviting anyone. Do not send commands as part of credential
verification. For a TLS instance use its verified HTTPS hostname/port; for a
local test CA, configure an explicit CA file in the client rather than disabling
certificate verification.

The following pattern reads a token from its private file inside the Python
process. It never puts the token in a shell variable or command-line argument
and prints only the HTTP status. For the old-token check, use an approved
verification tool that keeps the old token in memory; do not make a disk backup
for this purpose.

```sh
python3 - <<'PY'
from pathlib import Path
from urllib.error import HTTPError
from urllib.request import Request, urlopen

token = Path("deployment/secrets/api_token").read_text().strip()
request = Request(
    "https://localhost:8443/v1/accounts/synthetic-paper-daemon/demo",
    headers={"Authorization": "Bearer " + token},
)
try:
    with urlopen(request, timeout=5) as response:
        print("account HTTP", response.status)
except HTTPError as error:
    print("account HTTP", error.code)
PY
```

Use the host and port matching the certificate. For a test certificate, pass a
client SSL context configured with the explicit test CA. Do not print response
headers, request objects, token values, or exceptions that may include them.
Verify the account response is ready, `recovery_required` is false, and its
balances, child/reservation state and retained reports match the expected
account. A healthy HTTP endpoint alone does not establish account readiness.

## PostgreSQL password is separate

`postgres_password` is read by the database container only when its data volume
is initialized. Changing that file does not change the password of the role in
an existing database. The paper entrypoint reads the file when Go starts, so
changing it without a coordinated PostgreSQL role-password change can prevent
connections. Treat database password rotation as a separate, reviewed
maintenance procedure that changes the database role and mounted secret
together. Do not place the password in SQL literals, shell arguments, logs, or
URLs. This guide does not perform or claim a database-password rotation.

This package has no cohort provisioning, user directory, role hierarchy,
refreshable sessions, public domain or provider configuration. TLS, browser
review, certificate renewal and hosted access remain separate deployment work.

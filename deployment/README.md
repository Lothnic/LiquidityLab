# Portable paper + PPO demo

Run from the repository root with Docker Engine, Compose v2 and OpenSSL:

```sh
deployment/demo.sh init
deployment/demo.sh build
deployment/demo.sh prepare
deployment/demo.sh create
```

`prepare` runs real CPU PPO against an owned Go replay process using the checked-in
synthetic training tape. It publishes a Go-provenance bundle into the named bundle
volume at `/bundle/policy` and refuses to reuse an existing policy directory,
including an incomplete training attempt. The `/bundle` mount itself always
exists; training requires its `policy` child to be absent. The bundle is unvalidated,
live-ineligible, and has no demonstrated strategy value. Preparation is an actual
training job, potentially slow; successful packaging alone is not training evidence.

Open http://127.0.0.1:8080. Obtain the bearer token locally from
`deployment/secrets/api_token` and enter it in the UI's connection flow. The token
is scoped by Go to the configured paper account; no credential is baked into the
UI or images. Protect this directory and do not paste its contents into logs.
Docker operators can inspect container environments and volumes.

The first startup explicitly creates a synthetic account. Repeating `create`
against an existing account fails rather than resetting balances. After stopping:

```sh
deployment/demo.sh stop
deployment/demo.sh resume
```

PostgreSQL, the immutable bundle, and recordings survive `stop`. Never run
`docker compose down -v` against retained demo data. Keep the same schema, account,
bundle and secrets for resume; recovery can pause an active session and require
fresh market readiness and reconciliation. Use the UI's controls to inspect and
recover it. A daemon run is bounded to 24 hours; it has no automatic restart loop.
Use explicit `resume` after shutdown. Back up the database and bundle together
before upgrading; this package supplies no automated backup or migration rollback.

Configuration is supplied as shell environment variables when invoking the script:
`PAPER_SOURCE=synthetic` (default) or `kraken`, `PAPER_ACCOUNT=demo`,
`PAPER_SCHEMA=paper_beta`, `PAPER_DURATION=24h`, `PAPER_PORT=8080`.
`INITIAL_BASE=1` and `INITIAL_QUOTE=10000` apply only to `create`.
Kraken uses public market data only and produces bounded new recordings per run;
synthetic mode provides a deterministic offline paper feed. No exchange key is used.

Go serves the built UI and owns account state, risk, simulated execution and durable
audit. Python only proposes PPO actions over a private Unix socket. A shared volume
and the same numeric UID preserve its owner-only permissions. Worker and training
have no network access or published ports. PostgreSQL has no published port. An
nginx sidecar shares Go's network namespace because Go only accepts loopback HTTP;
it forwards port 8081 to Go's loopback port 8080. The only published port is bound
to the Docker host's loopback. For remote hosting, place authenticated TLS ingress
on the same host or use an SSH tunnel. No provider, domain, TLS certificate or
public deployment is configured by this package.

Policy startup must pass Go's pinned hash/schema checks. Fallback is explicitly
`pause`: worker failure or stale proposals pause the session. Worker readiness
checks socket connectivity only; Go performs the policy handshake. The worker
refuses to replace stale socket files. If a crash leaves one, stop all services,
then remove only the stale socket with:

```sh
cd deployment
PAPER_MODE=resume docker compose run --rm --no-deps --entrypoint python worker \
  -c 'from pathlib import Path; Path("/policy-run/inference.sock").unlink(missing_ok=True)'
cd ..
deployment/demo.sh resume
```

Never do that while worker or paper is running. No hot reload or bundle replacement
is supported. Named volumes are single-host durability, not high availability.
Image tags are versioned but not digest pinned; dependency locks are retained.
The package has not established target-host latency, capacity or strategy quality.

Compose enforces per-container CPU, memory and PID limits (ordinary Engine
Compose settings, not Swarm reservations): PostgreSQL 1 CPU/512 MiB/128 PIDs,
training 2 CPUs/2 GiB/128 PIDs, worker 1 CPU/1 GiB/64 PIDs, Go 1 CPU/1 GiB/128
PIDs, nginx 0.5 CPU/128 MiB/64 PIDs. Override these using the service prefix
`DB`, `TRAIN`, `WORKER`, `PAPER`, or `EDGE` followed by `_CPUS`, `_MEMORY`, or
`_PIDS`, for example `WORKER_MEMORY=2g deployment/demo.sh resume`. These caps
bound resource use; they do not establish inference latency, throughput, or
model quality. Budget host memory for concurrently running services.

Training is limited to `TRAIN_TIMEOUT=30m` wall time by GNU timeout, followed by
a 30 second termination grace period. Override it explicitly for a slower host.
Timeout, OOM, or interrupted training may leave an incomplete `/bundle/policy`;
preparation refuses to overwrite it. Inspect and retain diagnostic artifacts,
and prepare a fresh project/volume rather than deleting a bundle pinned by an
account. Kraken runs use a fresh random directory under `/recordings`; their
`raw.jsonl` files do not exist before Go opens them.

Build/runtime baseline pins are Node 26.5.0, Python 3.12.11, uv 0.13.5,
Go 1.26.8 and PostgreSQL 18.6. PostgreSQL 18's official image stores its
versioned data below `/var/lib/postgresql` (default `18/docker`); the database
volume mounts that parent path. Existing PostgreSQL 17 volumes require an
explicit supported upgrade or dump/restore, not a direct image-tag swap.

On 2026-10-30 an isolated local Compose run built both images, trained the actual
2048-step CPU PPO bundle against four Go replay environments, and served the UI
and scoped API on host loopback port 18882. PostgreSQL and the worker were healthy.
This run accepted the plan create/start commands, but did **not** demonstrate a
policy action or fill: the first decision paused with `expired`, and two explicit
operator resumes paused with `source_facts_pending`. These guards were preserved.
Stopping only that project's worker and issuing one authorized operator resume
produced `fallback_pause` with `worker_unavailable`. Cancel reached
`known_final: true`. A complete Compose stop/resume retained the cancelled session,
its pinned report and policy digest, BTC `1`, USD `10000`, and the empty fill list.
The terminal plan stayed cancelled; this does not establish active-session recovery
or fill retention. Command acceptance does not imply policy proposal acceptance.

Curated credential-free responses, command bodies/receipts, revision-pinned reports,
image IDs, effective resource limits and SHA-256 digests are in
[`evidence/2026-10-30`](evidence/2026-10-30/). Build/training success and safe failure
handling are demonstrated; successful container policy execution, target-host
latency/capacity and strategy quality remain unproved. No public host was deployed.

Docker context selection depends on the host. Inspect `docker context ls` and use
a running engine; this machine's active Desktop context referenced a missing
socket, while `docker --context default info` succeeded. For that local run every
script invocation used `DOCKER_CONTEXT=default`,
`COMPOSE_PROJECT_NAME=liquiditylab-rlbeta-20261030`, and `PAPER_PORT=18882`.
Select your own working context/project/port rather than changing global context
or sharing another deployment's volumes.

The final UI image was rebuilt from UI source commit `f3d49e0`, then explicitly
resumed against the same retained account and volumes. Go source was unchanged
from `91bf17a`; deployment packaging was uncommitted at build time, so this is
working-tree execution evidence, not a clean source release. Distinct final
container metadata and health/account responses are retained beside the original
records. The bundle provenance records PPO, seed, requested/actual timesteps and
the manifest's dataset, dependency-lock and implementation digests directly.

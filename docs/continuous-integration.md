# Continuous integration validation

`.github/workflows/validate.yml` runs read-only pull-request checks for the Go service, Python/replay contracts, and trader interface. It uses only synthetic fixtures and a throwaway PostgreSQL service; it has no deployment, publication, or release step and requires no repository secrets.

## Jobs

- **Backend, Python, and replay conformance:** One job provisions PostgreSQL 18.6, Go 1.26.8, and Python 3.12.11, then installs locked `dev`, `go`, `codegen`, and CPU `rl` extras. Before `go test -race -count=1 ./...`, it builds the ordinary replay daemon and exports its path plus the exact `.venv/bin/python` path for Go/Python boundary integration tests. The explicit `LIQUIDITYLAB_TEST_POSTGRES_DSN` targets the service over TCP at `127.0.0.1:5432` with a CI-only development account. Pinned Protobuf generators are added to `PATH`; `scripts/check_conformance.py` then checks generated bindings and fixture drift, runs focused Go race conformance, builds its owned replay daemon, and requires the Python suite to pass without skips. Compiler parallelism and `GOMAXPROCS` are capped at two to bound runner load.
- **Frontend:** Node.js 26.5.0 runs `npm ci`, `npm test`, and `npm run build` in `apps/web/`, using the committed package lock.

The workflow uses full commit-SHA pins for checkout and setup actions. The pinned releases and PostgreSQL image tag were checked against their official release/tag pages on 29 October 2026: [actions/checkout v4.2.2](https://github.com/actions/checkout/releases/tag/v4.2.2), [actions/setup-go v6.3.0](https://github.com/actions/setup-go/releases/tag/v6.3.0), [actions/setup-python v6.3.0](https://github.com/actions/setup-python/releases/tag/v6.3.0), [actions/setup-node v6.1.0](https://github.com/actions/setup-node/releases/tag/v6.1.0), [astral-sh/setup-uv v10.3.2](https://github.com/astral-sh/setup-uv/releases/tag/v10.3.2), and the [official PostgreSQL image's 18.6 tags](https://hub.docker.com/_/postgres/tags/).

Database tests create randomized schemas. Some integration paths intentionally leave their test schema behind; the entire PostgreSQL service is ephemeral and is discarded when the backend job ends. Other tests clean up their own schemas.

This configuration has not been run by GitHub Actions. A local workflow-file review does not establish passing CI; actual command results and a remote run remain separate evidence.

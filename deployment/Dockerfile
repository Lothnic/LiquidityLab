FROM node:26.5.0-bookworm-slim AS web
WORKDIR /build
COPY apps/web/package*.json ./
RUN npm ci
COPY apps/web/ ./
RUN npm run build

FROM golang:1.26.8-bookworm AS go
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY gen/ gen/
COPY migrations/ migrations/
RUN CGO_ENABLED=0 go build -trimpath -o /out/liquiditylabd ./cmd/liquiditylabd

FROM python:3.12.11-slim-bookworm AS worker
RUN pip install --no-cache-dir uv==0.13.5
WORKDIR /app
COPY pyproject.toml uv.lock ./
COPY src/ src/
COPY examples/training-tape.jsonl examples/buy-plan.json examples/
COPY --from=go /out/liquiditylabd /usr/local/bin/liquiditylabd
RUN uv sync --locked --extra go --extra rl --no-dev \
 && mkdir /bundle /policy-run \
 && chown 10001:10001 /bundle /policy-run
ENV PATH="/app/.venv/bin:$PATH" OMP_NUM_THREADS=1 MKL_NUM_THREADS=1
USER 10001:10001
ENTRYPOINT ["python"]
CMD ["-m", "liquiditylab.inference", "/bundle/policy", "--listen", "unix:/policy-run/inference.sock"]

FROM debian:bookworm-slim AS paper
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates gosu python3 \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir /bundle /policy-run /recordings \
 && chown 10001:10001 /bundle /policy-run /recordings
COPY --from=go /out/liquiditylabd /usr/local/bin/liquiditylabd
COPY --from=web /build/dist /app/web
COPY deployment/paper-entrypoint.sh /usr/local/bin/paper-entrypoint
ENTRYPOINT ["/usr/local/bin/paper-entrypoint"]

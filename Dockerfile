# Lightweight baseline paper demo for Render. The full PPO Compose package lives
# in deployment/Dockerfile and is deliberately independent of this profile.
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

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates nginx-light python3 \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 10001 --create-home app
COPY --from=go /out/liquiditylabd /usr/local/bin/liquiditylabd
COPY --from=web /build/dist /app/web
COPY deployment/hosted/serve.py /app/serve.py
USER 10001:10001
ENV PORT=10000 GOMEMLIMIT=256MiB GOMAXPROCS=1
EXPOSE 10000
ENTRYPOINT ["python3", "/app/serve.py"]

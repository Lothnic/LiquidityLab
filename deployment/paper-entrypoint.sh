#!/bin/sh
set -eu
# Read secrets before dropping privilege; never place them in argv or logs.
LIQUIDITYLAB_API_TOKEN=$(cat /run/secrets/api_token)
password=$(cat /run/secrets/postgres_password)
export LIQUIDITYLAB_API_TOKEN
export LIQUIDITYLAB_DATABASE_URL="postgres://liquiditylab:${password}@db:5432/liquiditylab?sslmode=disable"
unset password
export LIQUIDITYLAB_POLICY_SHA256
LIQUIDITYLAB_POLICY_SHA256=$(python3 -c 'import json; print(json.load(open("/bundle/policy/manifest.json"))["policy_sha256"])')
mode=${PAPER_MODE:?PAPER_MODE must explicitly be create or resume}
case "$mode" in
  create) set -- --initial-base "${INITIAL_BASE:-1}" --initial-quote "${INITIAL_QUOTE:-10000}" ;;
  resume) set -- ;;
  *) echo 'PAPER_MODE must be create or resume' >&2; exit 2 ;;
esac
source=${PAPER_SOURCE:-synthetic}
if [ "$source" = kraken ]; then
  capture_dir=$(gosu 10001:10001 mktemp -d /recordings/kraken-XXXXXXXXXXXX)
  set -- "$@" --capture-output "$capture_dir/raw.jsonl"
fi
exec gosu 10001:10001 liquiditylabd paper "$mode" "$@" \
  --schema "${PAPER_SCHEMA:-paper_beta}" --account "${PAPER_ACCOUNT:-demo}" \
  --source "$source" --duration "${PAPER_DURATION:-24h}" \
  --listen 127.0.0.1:8080 --web-dir /app/web \
  --policy-target unix:/policy-run/inference.sock --policy-allow-unvalidated \
  --policy-profile paper --policy-fallback pause --policy-timeout 100ms

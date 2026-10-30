#!/bin/sh
set -eu
cd "$(dirname "$0")"
command=${1:-help}
# Compose interpolation needs a mode even for build/down; startup always sets it.
export PAPER_MODE=resume
case "$command" in
  init)
    umask 077
    mkdir -p secrets
    for name in api_token postgres_password; do
      if [ ! -e "secrets/$name" ]; then
        # Hex passwords require no URL escaping in the PostgreSQL connection URI.
        openssl rand -hex 32 > "secrets/$name"
      fi
    done
    echo 'Secrets initialized in deployment/secrets (existing secrets preserved).'
    ;;
  build) docker compose build ;;
  prepare)
    # Never overwrite a bundle which might be pinned by durable accounts.
    docker compose --profile prepare run --rm --no-deps --entrypoint python train -c \
      'from pathlib import Path; import sys; sys.exit("Bundle directory already exists; preserve it for resume or inspect an incomplete training attempt." if Path("/bundle/policy").exists() else 0)'
    docker compose --profile prepare run --rm --no-deps train
    ;;
  create|resume)
    export PAPER_MODE="$command"
    docker compose up -d db worker
    # Recreate both shared-network services together when switching modes.
    docker compose up -d --force-recreate paper edge
    ;;
  stop) docker compose down ;;
  logs) docker compose logs --tail 100 paper worker edge ;;
  *) echo 'Usage: deployment/demo.sh init|build|prepare|create|resume|stop|logs'; exit 2 ;;
esac

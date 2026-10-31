# Free hosted baseline paper demo

This profile uses Vercel for the React desk, Render Free for the Go daemon and
nginx ingress, and Neon Free for PostgreSQL. It uses Kraken public BTC/USD prices
and deterministic baselines; balances, orders and fills are simulated. Python in
the image only supervises processes; PPO
training and inference are not enabled. This is a demo, not the hosted RL beta.

The root `Dockerfile` builds the independent small image. The full local PPO
package remains in `deployment/Dockerfile` and `deployment/compose.yaml`.

## Runtime configuration

Set these as Render runtime secrets/configuration, never Vercel frontend values:

- `LIQUIDITYLAB_DATABASE_URL`: the Neon **direct** PostgreSQL connection string
  with TLS. Do not use the transaction-pooling endpoint: the store sets a
  per-connection search path.
- `LIQUIDITYLAB_API_TOKEN`: a unique random bearer credential, at least 32 bytes,
  privately delivered to the intended demo user.
- `PAPER_SCHEMA=hosted_kraken_demo`, `PAPER_ACCOUNT=kraken-demo`, `PAPER_MODE=resume`.
- Render supplies `PORT`; the default is `10000`. Go stays on `127.0.0.1:8080`,
  with a non-root nginx process accepting external requests on `PORT`.

The account must be created deliberately **once**, before the hosted service is
started. Use an ordinary binary from the same source revision, export the two
secrets privately, and run:

```sh
liquiditylabd paper create \
  --listen 127.0.0.1:8089 --schema hosted_kraken_demo --account kraken-demo \
  --source kraken --web-dir apps/web/dist --capture-output /tmp/kraken-init.jsonl \
  --initial-base 1 --initial-quote 10000 --duration 30s
```

This grants only synthetic balances. Repeating creation refuses an existing
account. Every hosted startup uses `paper resume`; failed startup never falls
back to creation. Existing balances, deadlines and history remain authoritative,
and recovered plans can require an explicit trader resume.

The duration includes database migration and initialization. A short local run
against a remote database can finish with an uncertain shutdown even after the
account was created. Inspect its retained balances and obligations before
continuing with resume; never repeat creation to clear that state.

## Deploy

Use only free plans. `render.yaml` documents the service settings; the CLI can
also create the Docker service directly from the deployed Git branch. Keep
automatic deployment disabled until a tested revision is selected deliberately.
The service health path is `/healthz`.

After Render reports its actual service URL, configure Vercel rewrites for
`/v1/:path*` and `/healthz` to that exact HTTPS origin. Do not guess the backend
hostname: bearer credentials must only be forwarded to the verified deployment.
Deploy `apps/web` as Vite, with `npm run build` and output directory `dist`.
The browser keeps credentials in tab memory. No account token or database
credential belongs in `VITE_*` variables, frontend code, or rewrite URLs.

Verify the public frontend, proxy health, rejection of unauthenticated API calls,
and authenticated readiness through the Vercel origin. Then stop/restart the
backend and verify that account balances and session reports are retained.

## Free-tier limits

Render Free can sleep after 15 minutes without inbound requests; the next
visitor may wait for cold startup. Neon compute and storage allowances are
finite. Synthetic source observations, ownership renewal and audit writes
consume database resources while the daemon runs. Inspect consumption and
retain backups; there is no automatic deletion of financial history.

The Go daemon has a 24-hour maximum lifetime. The wrapper exits if Go or nginx
exits and forwards shutdown to both processes. Any platform restart follows the
same resume path. This does not establish continuous market reliability.

Kraken captures use bounded temporary files on Render's ephemeral filesystem.
They are not durable and are removed on a graceful shutdown. A capture limit or
feed interruption can leave the account unready and require recovery. The prior
synthetic account and its history remain in the separate `hosted_paper_demo`
schema. No replay archive is attached in this profile. `/healthz`
checks HTTP availability only; separately inspect account `ready` and
`recovery_required`. A successful container smoke test is not a hosted capacity
or soak result.

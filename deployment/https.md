# Certificate-based HTTPS ingress

The optional overlay provides TLS ingress using the existing nginx image and
private Go listener. It accepts operator-provided PEM certificate/key files;
hosting, DNS, certificate issuance/renewal and public-network rules remain an
explicit later deployment step. This profile still publishes only on host
loopback. No public deployment is created by these instructions.

Use Docker Compose 2.24.4 or later. The overlay's `!override` replaces the base
HTTP port mapping, so selecting HTTPS does not also publish the plaintext port.
See [Docker's merge rules](https://docs.docker.com/reference/compose-file/merge/#replace-value).

## Configure and resume

Obtain a certificate valid for the eventual hostname through your chosen
certificate provider. Supply the leaf certificate followed by any intermediate
certificates in `PAPER_TLS_CERTIFICATE`, and its matching PEM private key in
`PAPER_TLS_PRIVATE_KEY`. Keep both outside Git; restrict private-key access to
the operator and container engine. nginx reads runtime secrets, with no key in
the frontend build or application image. See
[nginx's certificate-chain guidance](https://nginx.org/en/docs/http/configuring_https_servers.html#chains).

From the repository root, after normal image/bundle preparation:

```sh
export PAPER_TLS_CERTIFICATE=/absolute/private/path/fullchain.pem
export PAPER_TLS_PRIVATE_KEY=/absolute/private/path/privkey.pem
export PAPER_TLS_PORT=8443
export PAPER_MODE=resume

docker compose --file deployment/compose.yaml \
  --file deployment/compose.tls.yaml config --quiet

docker compose --file deployment/compose.yaml \
  --file deployment/compose.tls.yaml up --detach db worker

docker compose --file deployment/compose.yaml \
  --file deployment/compose.tls.yaml up --detach --force-recreate \
  --wait --wait-timeout 120 paper edge
```

Use the deployment's original `DOCKER_CONTEXT`, `COMPOSE_PROJECT_NAME`, account,
schema, bundle and secrets. For first creation only, deliberately select
`PAPER_MODE=create` with explicit initial balances. Resume preserves retained
balances; changing transport does not create a replacement account.

Open the hostname matching the certificate over HTTPS. The current mapping is
`127.0.0.1:8443`, suitable for a local check or SSH tunnel. A hosted release must
deliberately arrange its external TLS/network ingress and verify that only the
intended HTTPS endpoint is reachable; do not expose the Go or database ports.
The bearer token remains account-scoped. TLS does not provision or revoke tokens.

The profile enables TLS 1.2/1.3 and sends HSTS. It has no plaintext listener or
automatic HTTP redirect. The edge healthcheck validates configuration and
certificate readability; it does not prove a successful client handshake,
account readiness or certificate expiry monitoring. Check HTTPS, authentication
and account readiness separately before inviting users.

## Verification and certificate renewal

Verify the chain/hostname with a trusted client, `/healthz`, unauthenticated
account rejection, authenticated account access, and retained session reports.
Send credentials only over the verified TLS connection. Do not disable
certificate verification for a hosted check. A local self-signed test may use an
explicit test CA; it is not a production certificate.

Missing or mismatched certificate files must fail startup. They must not cause a
fallback to plaintext ingress. Also inspect `docker compose ... config` and
container port bindings to confirm the original HTTP mapping was removed.

After certificate renewal, recreate the edge using the same overlay so runtime
file mounts resolve the replacement files. Validate the new certificate and
expiry from the client. Recheck authentication and readiness; keep a rollback
decision and the prior certificate outside the repository. Automated renewal,
expiry alerts and chosen-host verification remain release gates.

Use both Compose files for later `logs`, `stop`, `up` and certificate changes.
`demo.sh resume` selects the base HTTP profile; it does not remember this overlay.

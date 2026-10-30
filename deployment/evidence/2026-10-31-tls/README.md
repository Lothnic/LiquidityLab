# Local certificate-based HTTPS verification

The [optional TLS overlay](../../https.md) was applied to an isolated restored
terminal account with exact retained images, fresh credentials and a locally
generated self-signed test certificate. Its only published endpoint was host
loopback `127.0.0.1:18884` to TLS port 8443. The base HTTP publication was replaced;
Go and PostgreSQL were not published directly.

A client explicitly trusting that test CA verified the hostname and retrieved
HTTP 200 health, HTTP 401 unauthenticated account rejection, HTTP 200 authenticated
account access, and both exact revision-pinned reports (50 fills plus one fill).
All responses included the configured HSTS header. A 65,537-byte command request
was rejected with HTTP 413 at ingress; no valid trading command was issued.
System trust rejected the self-signed certificate and a trusted client rejected
a wrong hostname. Explicit TLS 1.2 and 1.3 connections succeeded; TLS 1.1 received
a server protocol-version alert. Missing certificate configuration, missing
runtime certificate files and a mismatched private key were refused.

The first test helper selected an internal Docker bridge. This engine left its
host publication unbound, so the initial host connection failed. The fixture was
moved to a normal bridge with the same loopback-only publication before the
successful client checks. Process/configuration health was not substituted for
an observed HTTPS connection.

The services were explicitly stopped after verification; restored volumes were
retained. The original local demonstration was not reconfigured. Private keys,
tokens, passwords and secret file contents are absent from this evidence.

This is local transport/configuration verification, not public deployment,
production certificate issuance, browser review, renewal automation, credential
lifecycle, availability or capacity evidence. Those hosted release gates remain
open. See the operator guide for explicit certificate and profile handling.

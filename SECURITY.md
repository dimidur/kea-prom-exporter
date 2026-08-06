# Security

## Reporting a vulnerability

Please use
[GitHub's private vulnerability reporting](https://github.com/dimidur/kea-prom-exporter/security/advisories/new)
rather than a public issue.

## What this exporter touches

It holds a credential for Kea's **control socket**, which is worth being
explicit about: that socket is an administrative interface. This exporter only
ever issues two read commands -- `statistic-get-all` and `status-get` -- but
the credential it holds is not inherently read-only — anything else able to
read that credential can drive the control socket fully.

- **Prefer `--kea-password-file` / `KEA_PASSWORD_FILE`** over the inline password
  flag or env var. An environment variable is visible to anything that can run
  `docker inspect`; a mounted file is not.
- **Kea's HTTP control socket is plaintext** unless you have configured
  `socket-type: https`. Keep the exporter and Kea on the same trusted segment, or
  on the same host.
- **Give Kea a dedicated control-socket account** if your deployment supports it,
  rather than reusing an administrative one.

## Exposure of the metrics endpoint

`/metrics` carries no credentials, but it does describe your DHCP estate: subnet
identifiers, pool sizes, lease counts, and HA topology and state. Bind it to an
internal address and let your scraper reach it there rather than publishing it.

## Hardening in the shipped image

- Static binary on distroless `static` — no shell, no package manager, no libc.
  It carries only a CA bundle, a `/etc/passwd` entry, `/tmp` and tzdata.
- **Runs as uid 65532 (`nonroot`)**, a dedicated identity with its own passwd
  entry rather than the shared `nobody`. Kubernetes `runAsNonRoot` and
  PodSecurity `restricted` accept it without a `securityContext` override.
  A bind-mounted password file must be readable by that uid.
- No capabilities required; the process makes one outbound HTTP connection and
  binds one port.
- CI runs `govulncheck` and `gosec` on every push, so a dependency advisory or a
  new unsafe pattern fails the build rather than shipping.

Images up to and including **v0.2.0** were built `FROM scratch` and carried no
CA certificates, so an HTTPS control socket could not be verified at all
(`x509: certificate signed by unknown authority`). Images built after v0.2.0
ship a CA bundle. If you are pinned to v0.2.0 or earlier and need
`socket-type: https`, upgrade rather than mounting certificates into the image.

## Supported versions

The most recent release. This is a small project — fixes go into a new release
rather than being backported.

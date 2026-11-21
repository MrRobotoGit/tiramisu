# Security Policy

## Reporting a vulnerability

Please do not open a public issue for security problems. Report them privately through
[GitHub Security Advisories](https://github.com/MrRobotoGit/tiramisu/security/advisories/new).
Include the version (`/metrics` → `version`), how to reproduce the problem and its impact.

You will get an answer within a few days. Fixes are released as a new version and credited in the
release notes unless you prefer otherwise.

## Supported versions

Only the latest release receives security fixes.

## Scope

Tiramisu is designed to run on a trusted home network. Sign-in for `:9080` (Control Panel,
Dashboard, APIs, MCP) is optional and off by default; turn it on in **Control Panel → Access**
before exposing the port beyond your LAN, together with HTTPS: Tiramisu can serve it itself
(**Access → HTTPS**, with a local CA you trust on each device or your own certificate), or a
reverse proxy can. With HTTPS on, the Plex/Jellyfin webhooks and calls from the host itself stay
on plain HTTP by design. The engine runs in-process and opens no HTTP port of its own (the
`:8090` API of earlier versions is gone since v1.13.0). Do not expose `:9080` to the internet
directly: use a VPN or a reverse proxy.

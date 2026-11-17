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

Tiramisu is designed to run on a trusted home network. The Control Panel, the Dashboard and the APIs
on `:9080` and `:8090` have no authentication; do not expose them to the internet.

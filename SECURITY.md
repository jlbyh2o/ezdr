# Security Policy

EZDR manages hypervisors, replication, and failover, so security reports are
taken seriously.

## Reporting a vulnerability

Please **do not** open a public issue. Instead, report vulnerabilities
privately through GitHub:
[Report a vulnerability](https://github.com/jlbyh2o/ezdr/security/advisories/new).

Include as much detail as you can: affected component (client, portal, or web
UI), version or commit, steps to reproduce, and the potential impact.

You can expect an acknowledgment within a few days. Fixes will be coordinated
with you before any public disclosure.

## Supported versions

Until version 1.0, only the latest release is supported: security fixes go
into a new release rather than into older ones.

## Verifying releases

Release checksums are signed with EZDR's release key, and files and images
carry GitHub build provenance. See
[Verify a release](docs/deployment.md#verify-a-release).

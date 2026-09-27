# EZDR (Easy Disaster Recovery)

EZDR is a self-hosted, open source tool for setting up and managing disaster
recovery between [Proxmox VE](https://www.proxmox.com/en/proxmox-virtual-environment)
hosts that use ZFS storage.

A lightweight client runs on each Proxmox host, and a web portal ties them
together. From the portal you can:

- Inventory VMs, containers, storage, and networking on each host.
- Build a DR plan that maps storage, networks, and public DNS records between
  sites.
- Replicate workloads with [zrepl](https://zrepl.github.io/) over an encrypted
  site-to-site WireGuard tunnel.
- Monitor replication health and recovery point objectives.
- Run test failovers, failovers, and failbacks, including switching DNS records
  (Cloudflare first).

## Status

EZDR is in early development. Phases 1–3 are complete: the portal, sign-in
with TOTP, enrolling hosts over WireGuard, host inventory with replication
readiness, and DR plans (guests, mappings, schedules, retention, startup
order, and validation). Replication and failover are not implemented yet. See the [architecture document](docs/architecture.md) for
the design and roadmap.

## Quick start

1. Deploy the portal with Docker Compose on a server outside the sites it
   protects ([deployment guide](docs/deployment.md)).
2. Open the portal, create the administrator with the setup code from the
   portal's log, and set up two-factor authentication.
3. Select **Add host** and run the command it shows on each Proxmox VE host.

## Documentation

- [Documentation index](docs/README.md)
- [Deploying EZDR](docs/deployment.md)
- [Architecture](docs/architecture.md)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, commit
conventions, and the DCO sign-off requirement. Report security issues as
described in [SECURITY.md](SECURITY.md).

## License

EZDR is licensed under the [GNU Affero General Public License v3.0](LICENSE).

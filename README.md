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

EZDR is in the early design phase and is not yet usable. See the
[architecture document](docs/architecture.md) for the intended design and
roadmap.

## Documentation

- [Documentation index](docs/README.md)
- [Architecture](docs/architecture.md)

## License

EZDR is licensed under the [GNU Affero General Public License v3.0](LICENSE).

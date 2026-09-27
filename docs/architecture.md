# EZDR Architecture

> **Status:** Draft. This document describes the intended high-level design.
> Implementation details will be captured in separate documents as they are
> decided.

## 1. Overview

EZDR makes it easy to set up, monitor, and execute disaster recovery (DR) for
Proxmox VE hosts that use ZFS storage. It consists of two pieces:

- **Portal** – a self-hosted web application that serves as the control plane.
  Operators use it to review inventory, build DR plans, monitor replication,
  and trigger test failovers, failovers, and failbacks.
- **Client** – an agent installed on each Proxmox host. It inventories the
  host, configures networking and replication according to the DR plan, reports
  status to the portal, and carries out failover actions.

ZFS replication is handled by [zrepl](https://zrepl.github.io/). EZDR does not
reimplement snapshotting or send/receive; it generates zrepl configuration,
observes zrepl's status, and orchestrates everything around it (VM
configuration, network mapping, startup order, and failover workflows).

### 1.1 Goals

- Guided setup: install a client, enroll it, and build a DR plan from inventory
  in the portal.
- Safe, observable replication of VM and container disks via ZFS and zrepl.
- One-click **test failover**, **failover**, and **failback**.
- Public DNS records switched as part of failover and failback, so services
  remain reachable when they move between sites.
- Secure by default: encrypted transport, mutual authentication, and a DR copy
  that a compromised primary cannot destroy.
- Easy to self-host: a small portal deployable on a single VPS.

### 1.2 Non-goals (for now)

- Storage backends other than ZFS.
- Automatic (unattended) failover. Failover is always initiated by a human.
- Multi-tenancy. A portal instance serves a single organization.
- Hosting EZDR as a service. Each user runs their own portal.

### 1.3 Scope of the first release

| Area | First release | Designed to allow later |
| --- | --- | --- |
| Topology | One primary host to one DR host | Many-to-one, one-to-many, clusters |
| Hosts | Standalone Proxmox VE nodes | Proxmox VE clusters |
| Storage | ZFS (zvols and datasets) | — |
| Workloads | QEMU VMs and LXC containers | — |
| Tenancy | Single organization | — |

The data model should not assume a strict 1:1 pairing, even though the first
release only exposes that topology. A DR plan links a set of workloads on a
source host to a target host, so additional topologies can be added without a
redesign.

## 2. System context

```text
                         ┌───────────────────────────┐
                         │          Portal           │
                         │   (self-hosted, e.g. VPS) │
                         │                           │
                         │  Web UI · API · Database  │
                         └─────────────┬─────────────┘
                                       │
                     control plane (VPN, client-initiated)
                    ┌──────────────────┴──────────────────┐
                    │                                     │
          ┌─────────┴─────────┐                 ┌─────────┴─────────┐
          │   Primary host    │                 │      DR host      │
          │  Proxmox VE + ZFS │   data plane    │  Proxmox VE + ZFS │
          │                   │◄───────────────►│                   │
          │  EZDR client      │  site-to-site   │  EZDR client      │
          │  zrepl (source)   │      VPN        │  zrepl (pull)     │
          └───────────────────┘                 └───────────────────┘
```

Two separate network paths exist:

- **Control plane:** each client to the portal. Carries enrollment, inventory,
  plans, status, and commands. Low bandwidth.
- **Data plane:** primary host to DR host directly. Carries ZFS replication
  traffic. High bandwidth. Replication data never passes through the portal.

## 3. Components

### 3.1 Portal

Responsibilities:

- User authentication (with MFA) and an audit log of all actions.
- Client enrollment: issuing one-time enrollment tokens and client certificates.
- Storing inventory reported by clients.
- DR plan builder: selecting workloads and defining storage and network
  mappings between hosts.
- Distributing desired state (plans) to clients.
- Displaying replication health, snapshot age, recovery point objective (RPO)
  compliance, and alerts.
- Sending alerts for replication failures, RPO violations, and clients that
  stop reporting, via email (SMTP) and generic webhooks.
- Initiating test failover, failover, and failback workflows.
- Acting as the coordination point for the site-to-site VPN (exchanging peer
  public keys and endpoints between clients).

Deployment: a single container image with an embedded SQLite database and a
data volume, plus a Compose file for easy self-hosting. It must be hosted
**outside** the sites it protects; a portal running at the primary site would
fail in the same disaster.

### 3.2 Client

A single binary running as a systemd service on each Proxmox host. It is
published as a `.deb` package in an EZDR apt repository, with a one-line install
script that adds the repository and installs the package. Updates arrive
through normal `apt upgrade`.

The same binary provides a local command-line interface (`ezdr`) for status
checks and break-glass failover.

Responsibilities:

- **Bootstrap:** verify prerequisites (Proxmox VE version, ZFS pools) and
  install required tools (zrepl, WireGuard tools) from their official
  repositories.
- **Enrollment:** exchange a one-time token for a client certificate and
  establish the control-plane VPN to the portal.
- **Inventory:** report VMs, containers, disks, ZFS pools and datasets, Proxmox
  storage definitions, and host network configuration (interfaces, bridges,
  VLANs). Inventory is gathered through the Proxmox API (`pvesh` or the local
  REST API) and ZFS tooling rather than by parsing files directly.
- **Reconciliation:** receive the desired state from the portal and apply it:
  site-to-site VPN, zrepl configuration, and VM/container configuration
  replication.
- **Status reporting:** report zrepl job status, latest snapshot per dataset,
  replication lag, and errors.
- **Failover actions:** execute test failover, failover, and failback steps on
  the local host.

The client footprint on the hypervisor should be small and predictable. Every
change it makes to the host should be documented and reversible.

### 3.3 zrepl

zrepl performs snapshotting, pruning, and incremental ZFS send/receive.

- The **primary** runs a `source` job that takes snapshots and serves them.
- The **DR host** runs a `pull` job that fetches snapshots from the primary.

Pull mode is chosen deliberately: the primary has no ability to delete or
modify data on the DR host. If the primary is compromised (for example, by
ransomware), existing snapshots on the DR host remain intact.

Encrypted datasets are replicated with raw sends so data stays encrypted in
transit and at rest on the DR host.

## 4. Networking

### 4.1 Control-plane VPN

Each client initiates a WireGuard tunnel to the portal. Because connections are
outbound from the client, hosts behind NAT or restrictive firewalls work
without inbound port forwarding. Only the portal needs a publicly reachable
endpoint.

### 4.2 Site-to-site VPN

The primary and DR clients establish a direct WireGuard tunnel between each
other, using keys and endpoints exchanged through the portal. zrepl traffic runs
over this tunnel.

At least one side must be reachable by the other (a public IP or port forward).
Relaying replication traffic through the portal is not planned, as it would put
bulk data on the VPS and defeat the separation of control and data planes.

### 4.3 WireGuard management

EZDR manages WireGuard directly rather than building on a mesh product such as
Headscale or NetBird. The portal generates and distributes peer configuration;
each client writes its own interface configuration and never shares its private
key. This keeps the self-hosted footprint to a single service and avoids
installing a third-party VPN agent on hypervisors. The trade-off is limited NAT
traversal, which is why one side of the site-to-site link must be reachable.

## 5. DR plan

A DR plan is created in the portal once inventory from both hosts is available.
It defines:

- **Protected workloads:** which VMs and containers are replicated.
- **Storage mapping:** source Proxmox storage ID and ZFS dataset to target
  storage ID and dataset (for example, `local-zfs` on the primary maps to
  `tank-dr` on the DR host).
- **Network mapping:** source bridge and VLAN to target bridge and VLAN (for
  example, `vmbr0` maps to `vmbr1`).
- **Replication schedule and retention:** snapshot interval and how many
  snapshots to keep on each side.
- **Recovery settings:** startup order and delays, and optionally which
  workloads to include in a test failover.
- **DNS records:** public DNS records attached to each workload, with a
  production value and a failover value (see [DNS failover](#51-dns-failover)).

Workloads keep their internal IP addresses on failover. The DR site is expected
to provide the same subnets and VLANs (mapped through the network mapping
above), so guests boot with unchanged configuration. Public IP addresses differ
between sites, which is why public DNS records must be switched. The plan model
should leave room for optional per-workload re-addressing in a later release.

The portal validates the plan against inventory (enough free space, target
storage and bridges exist) before sending it to clients.

### 5.1 DNS failover

A common layout, and the one EZDR is designed around first, is:

- Each site's router holds that site's public IP addresses and forwards traffic
  to workloads on internal VLANs.
- Both sites use the same VLANs and internal subnets, so a failed-over workload
  keeps its internal IP address.
- Only the public IP address changes, because it belongs to the other site's
  router. Public DNS records must be updated to point at the DR site.

EZDR manages those DNS records:

- **Per-workload records:** each protected workload lists the DNS records that
  point at it (for example, an `A` record for `app.example.com`), each with a
  production value and a failover value. `TXT` records that contain IP
  addresses (such as SPF) can be attached in the same way.
- **Provider interface:** DNS changes go through a provider interface. The
  first provider is Cloudflare; others (Route 53, PowerDNS, and so on) can be
  added by contributors.
- **Scoped credentials:** the portal stores a Cloudflare API token limited to
  DNS edit on the selected zones, encrypted at rest. The token is never sent to
  clients.
- **TTL checks:** for DNS-only (unproxied) records, resolvers cache the old
  value until the TTL expires. The portal warns when a protected record's TTL
  is above a configurable threshold (default 300 seconds). Proxied records take
  effect almost immediately.
- **Drift detection:** the portal periodically reads the managed records and
  alerts if they don't match the expected values for the plan's current state
  (for example, a manual edit or a partially applied switch).

Items that EZDR cannot manage, but that operators must prepare for failover:

- Port forwards and firewall rules on the DR router must mirror the primary's.
- Reverse DNS (PTR) records are controlled by the ISP, not the DNS provider. Mail
  servers in particular need PTR records for the DR public IPs set up in
  advance, or outgoing mail may be rejected after failover.

## 6. Key workflows

### 6.1 Enrollment and inventory

1. Operator creates an enrollment token in the portal.
2. Operator installs the client on a host and provides the token.
3. Client checks prerequisites and installs required tools.
4. Client enrolls, receives its certificate, and brings up the control-plane
   VPN.
5. Client sends a full inventory, then sends updates when the host changes.

### 6.2 Plan activation and replication

1. Operator builds and saves a DR plan.
2. Portal sends the desired state to both clients.
3. Clients establish the site-to-site VPN.
4. Clients generate and apply zrepl configuration.
5. The DR client pulls the initial full replication, then incremental updates
   on schedule.
6. The primary client regularly exports VM and container configuration to the
   DR client, which stores it (not yet registered with Proxmox) alongside the
   replicated data.
7. Both clients report replication status to the portal.

Replication continues even if the portal is unreachable. The portal is needed
to change plans and view status, not for replication to run.

### 6.3 Test failover

Verifies that recovery works without interrupting replication.

1. DR client clones the latest replicated snapshots.
2. DR client registers temporary VMs and containers using the clones, with
   storage and network mappings applied, attached to an isolated network.
3. Workloads start in the defined order; results are reported to the portal.
4. When the test ends, the temporary workloads and clones are destroyed.

Test failover never changes production DNS records.

### 6.4 Failover

Always initiated by an operator in the portal. When the portal itself is
unreachable, an operator with root access to the DR host can run failover
locally through the client CLI (for example, `ezdr failover --plan <name>`).
Break-glass failover relies on existing root access to the hypervisor; the
client records the action and syncs the audit entry to the portal once it is
reachable again.

1. If the primary is still reachable, stop its workloads and perform a final
   replication (planned failover). Otherwise, use the latest snapshot
   (unplanned failover).
2. Stop replication for the plan.
3. Promote the replicated datasets on the DR host.
4. Register VMs and containers from the stored configuration, applying the
   storage and network mappings.
5. Start workloads in the defined order.
6. Mark the plan as failed over so the original primary does not start its
   copies of the same workloads if it comes back (preventing split-brain).
7. Pause for the operator to verify that services are running at the DR site.
8. On the operator's confirmation, switch the workloads' DNS records to their
   failover values and verify them through the provider API.

During break-glass failover the portal is unavailable, so DNS is not switched
automatically. The CLI prints the exact records and values to change, and the
operator updates them in the Cloudflare dashboard. The DR client already has
this information from the plan; it never holds DNS provider credentials.

### 6.5 Failback

Returns workloads to the original primary once it is repaired.

1. Reverse replication direction: the DR host becomes the source and the
   original primary pulls changes. If a common snapshot still exists,
   replication is incremental; otherwise a full resync is required.
2. Once in sync, perform a planned failover back to the original primary,
   including the confirmation step that switches DNS records back to their
   production values.
3. Restore the original replication direction.

Failback is the most complex workflow and is scheduled last, but the data model
and zrepl configuration should account for it from the start.

## 7. Security model

The portal can direct actions on hypervisors, which makes it a high-value
target. The design limits both the likelihood and the impact of a compromise.

- **Enrollment:** one-time, expiring tokens. After enrollment, clients and
  portal authenticate each other with mutual TLS (mTLS) certificates.
- **Client-initiated connections:** clients connect out to the portal; the
  portal never needs inbound access to hosts.
- **Declarative control, not remote execution:** the portal sends desired state
  and a fixed set of named actions (for example, "start test failover for
  plan X"). The client validates every request and rejects anything outside
  its known actions. There is no arbitrary command execution.
- **Pull-based replication:** a compromised primary cannot delete DR snapshots.
- **Least-privilege data plane:** zrepl on each side is limited to the datasets
  in the plan.
- **Portal hardening:** MFA for users, audit log of all actions, secrets
  encrypted at rest.
- **DNS provider credentials stay on the portal:** API tokens are scoped to DNS
  edit on selected zones and are never distributed to clients.
- **Human-initiated failover:** no automatic failover, reducing the risk of
  split-brain or an attacker-triggered failover.

## 8. Technology choices

| Area | Choice | Rationale |
| --- | --- | --- |
| Client language | Go | Single static binary, easy to ship to Debian-based Proxmox, strong networking libraries. |
| Client distribution | `.deb` in an apt repository, plus an install script | Proxmox-native installs and updates through `apt`; the script makes first installs a one-liner. |
| Replication | zrepl | Mature ZFS replication with snapshot management, pruning, and resumable sends. |
| VPN | WireGuard, managed directly by EZDR | Built into the Linux kernel on Proxmox VE; no extra VPN service to self-host. |
| Portal backend | Go API | Shares code and types with the client. |
| Portal frontend | React single-page app (TypeScript, Vite) | Largest ecosystem and contributor pool; built assets are embedded in the Go binary. |
| Portal database | SQLite | Enough for a single organization; no separate database service; backups are a file copy. |
| Portal packaging | Container image and Compose file | Simple to self-host on a VPS. |
| Alerting | Email (SMTP) and generic webhooks | Webhooks cover Slack, Discord, Teams, and custom tooling. |
| DNS provider | Cloudflare, behind a provider interface | Widely used and has a complete API; the interface allows other providers later. |
| License | AGPL-3.0 | Changes to a hosted portal must be shared; matches Proxmox VE's license. |

## 9. Roadmap

| Phase | Deliverable |
| --- | --- |
| 1 | Portal skeleton, user authentication, client enrollment, control-plane VPN |
| 2 | Inventory: workloads, storage, and network reported to and displayed in the portal |
| 3 | DR plan builder with storage and network mapping, per-workload DNS records, and validation |
| 4 | Site-to-site VPN, zrepl configuration, configuration replication, and status reporting |
| 5 | Test failover |
| 6 | Failover (planned and unplanned), including break-glass local failover, Cloudflare DNS switching, and DNS drift detection |
| 7 | Failback |
| Later | Additional topologies, Proxmox VE clusters, disk-based initial seeding, per-workload re-addressing on failover, more alerting channels, more DNS providers |

## 10. Deferred features

These were considered and intentionally left out of the first release:

- **Disk-based initial seeding:** in the first release, the initial full
  replication runs over the site-to-site VPN, relying on zrepl's resumable
  sends. Seeding from a locally attached disk may be added if large datasets
  over slow links prove to be a problem.
- **Re-addressing on failover:** workloads keep their IP addresses in the first
  release (see [DR plan](#5-dr-plan)).
- **Additional alerting channels:** ntfy, Healthchecks.io, and similar
  integrations. Many can already be reached through generic webhooks.

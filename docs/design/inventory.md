# Design: Inventory

> **Status:** Approved for phase 2. Covers how clients collect and report host
> inventory, and how the portal stores and shows it. See
> [Architecture](../architecture.md) for the overall design and
> [Enrollment and control plane](enrollment.md) for the client API.

## 1. Summary

- The client reads inventory from the **local Proxmox VE API** using a
  dedicated **read-only API token**, and from `zfs` for dataset details.
- It collects **every 60 seconds** and sends the inventory to the portal only
  when it **changed**, plus whenever the command stream reconnects.
- A **Refresh** button in the portal asks a host for an immediate update. It is
  the first **action** sent over the command stream, which later phases
  (test failover, failover) reuse.
- Each guest disk gets a **replication readiness** verdict, which the phase 3
  plan builder uses.
- The portal keeps **only the latest inventory** per host, with the time it
  last changed.
- Guest notes and cloud-init credentials are **never sent** to the portal.

## 2. Reading from Proxmox VE

### 2.1 Read-only API token

Enrollment creates a Proxmox user and API token with read-only access:

| Item | Value |
| --- | --- |
| User | `ezdr@pve` (no password; cannot sign in to the web UI) |
| Token | `ezdr@pve!inventory`, with privilege separation |
| Role | `PVEAuditor` on `/`, granted to both the user and the token |
| Secret | `/etc/ezdr/pve-token`, readable only by root |

`PVEAuditor` can read guest configuration, storage, and network settings but
cannot change anything. With privilege separation, a token's effective
permissions are the intersection of its own and its user's, so the role is
granted to both. Later phases that change host configuration (such as
registering guests during failover) use local tools as root, not this token.

Enrollment lists the token among its planned changes. Hosts enrolled before
inventory existed get the token when the upgraded client first starts; the
client logs that it created it. `ezdr unenroll` removes the token and user.

### 2.2 Trusting the local API's certificate

The client calls `https://127.0.0.1:8006`. It accepts the connection only if
the certificate presented is byte-for-byte one of the node's own certificate
files, `/etc/pve/local/pveproxy-ssl.pem` (a custom or ACME certificate) or
`/etc/pve/local/pve-ssl.pem` (the default). This works for every certificate
setup without trusting anything else.

### 2.3 Sources

| Data | Source |
| --- | --- |
| Host facts | `GET /nodes/{node}/status`, `GET /version`, `/sys/module/zfs/version` |
| Guests | `GET /nodes/{node}/qemu`, `GET /nodes/{node}/lxc`, and each guest's `.../config` |
| Storage | `GET /storage` and `GET /nodes/{node}/storage` |
| ZFS pools | `GET /nodes/{node}/disks/zfs` |
| ZFS datasets and zvols | `zfs list -Hp -t filesystem,volume -o name,type,used,refer,volsize,compression,encryption` |
| Host network | `GET /nodes/{node}/network` |

The API does not expose per-dataset properties, so `zfs list` (read-only) is
used for those.

## 3. Content

### 3.1 Collected

- **Host:** hostname, Proxmox VE version, kernel, ZFS version, CPU model and
  count, total memory.
- **Guests (VMs and containers):** ID, type, name, status, cores, memory,
  start at boot, startup order, template flag, tags, lock state, and:
  - **Disks:** configuration key (such as `scsi0`, `rootfs`, `mp0`), storage
    ID, volume, size, the ZFS dataset or zvol it maps to, and a readiness
    verdict (section 4).
  - **NICs:** key (such as `net0`), bridge, VLAN tag, MAC address, model,
    firewall flag.
  - **Passthrough:** whether the guest uses PCI or USB passthrough.
- **Storage:** ID, type, content types, ZFS pool (for `zfspool` storage),
  total, used, available, active and shared flags.
- **ZFS pools:** name, health, size, allocated, free, fragmentation.
- **ZFS datasets and zvols:** name, type, used, referenced, volume size,
  compression, encryption.
- **Host network:** interfaces with type (bridge, physical, bond, VLAN),
  active and autostart flags, bridge ports, VLAN awareness, VLAN ID and raw
  device, bond members, address, and gateway.

### 3.2 Never collected

- Guest notes (`description`) and interface comments, which are free text and
  sometimes hold passwords or internal details.
- Cloud-init credentials: `cipassword` and `sshkeys`.

The client builds the inventory from an explicit list of fields, so new or
unknown configuration keys are never sent by accident.

## 4. Replication readiness

Each guest disk gets one of these verdicts:

| Verdict | When | Meaning |
| --- | --- | --- |
| **Replicable** | Storage type is `zfspool` | Replicated with zrepl in later phases. |
| **Not replicable** | Any other storage type, a passthrough device (`/dev/...`), or a container bind mount of a host path | Can't be protected by EZDR. The reason names the storage and type, or the host path. |
| **Not needed** | Cloud-init drive; CD/DVD drive; detached (`unusedN`) disk | Regenerated from configuration (cloud-init), not guest data (ISO media), or not attached to the guest. |

A guest is **ready** when all its disks are replicable or not needed. It gets
a **warning** (not a failure) when it uses PCI or USB passthrough, because the
DR host's hardware may differ, and when a CD/DVD drive references an ISO that
must also exist at the DR site.

## 5. Reporting

- The client collects every 60 seconds and hashes the inventory (SHA-256 of
  its deterministic protobuf encoding, excluding the collection time).
- It calls `ReportInventory` when the hash changes, when a new command stream
  opens, and when the portal sends a **Refresh inventory** action.
- Collection problems (for example, one guest's configuration could not be
  read) are reported in the inventory's `warnings` list rather than failing
  the whole report.

### 5.1 Actions on the command stream

The `Subscribe` stream gains a third message type, **action**, alongside
desired state and heartbeats:

- Each action has an ID and a kind. Phase 2 defines one kind:
  **refresh inventory**.
- The client reports the outcome with `AckAction` (succeeded or failed, with
  a message).
- The portal only sends actions to hosts with an open stream. Asking an offline
  host to refresh returns an error in the UI.

## 6. Portal storage and API

- A `host_inventory` table stores, per host, the latest inventory (protobuf),
  its hash, when it was collected, when it last changed, and when it was
  received. Removing a host removes its inventory.
- `HostService` gains `GetHostInventory` and `RefreshInventory`.
- `ListHosts` gains per-host counts: guests, and guests that aren't ready.

## 7. User interface

- The host list links each host to a **host detail page** and shows guest and
  readiness counts.
- The host detail page has **Guests**, **Storage**, and **Network** sections,
  shows when inventory was last updated, and has a **Refresh** button.
- Guests that aren't ready show the reason next to the affected disk.

## 8. Phase 2 scope

In scope: everything above.

Out of scope (later phases): inventory history, ZFS snapshot listings
(phase 4, with replication status), and inventory-driven alerts.

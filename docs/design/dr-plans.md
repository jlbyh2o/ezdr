# Design: DR plans

> **Status:** Draft for phase 3. Covers what a disaster recovery (DR) plan
> contains, how it is validated, and how it is built in the portal. Applying a
> plan (zrepl configuration and replication) is phase 4. See
> [Architecture](../architecture.md) and [Inventory](inventory.md).

## 1. Summary

- A **DR plan** links one **primary host** to one **DR host** and protects
  **individually selected guests**.
- It maps **storage** (source storage to a DR ZFS storage) and **networks**
  (bridge to bridge; VLAN tags are kept), and names the isolated bridge used
  for test failovers.
- The **snapshot interval** and **retention** are configurable per plan, with
  presets. Retention is set separately for the primary (default 24 hours) and
  the DR host.
- The **startup order** starts from each guest's Proxmox settings and can be
  adjusted in the plan.
- **DNS records** are attached to guests by hand. Connecting to Cloudflare, and
  switching records, come in phase 6.
- The portal **validates** plans against both hosts' latest inventories and
  shows errors (which block activation) and warnings.
- Plans can **adopt an existing zrepl setup** (phase 4). The receive location
  and snapshot prefix can be set to match it, so the first sync is incremental.
- In phase 3, plans are **drafts**; activation arrives in phase 4.

## 2. Plan contents

| Part | Contents |
| --- | --- |
| General | Name, description, primary host, DR host |
| Guests | Selected guests from the primary's inventory, by VMID |
| Storage mapping | For each source storage used by protected disks: the DR ZFS storage and the receive dataset (section 4) |
| Network mapping | For each source bridge used by protected NICs: the DR bridge. Plus the DR bridge used for test failovers |
| Schedule | Snapshot interval |
| Retention | Tiers kept on the primary, and tiers kept on the DR host (section 5) |
| Startup order | Per guest: order and delay after starting (section 6) |
| DNS records | Per guest: records with production and failover values (section 7) |
| Advanced | Receive dataset overrides and snapshot prefix, for adopting existing zrepl setups (section 8) |

### 2.1 Rules

- A guest can belong to only one plan.
- A host can be the primary of several plans (for example, to give some guests
  a shorter snapshot interval) and the DR host of several plans.
- A plan's primary and DR hosts must be different hosts.
- Guests keep their VMIDs at the DR site. The VMIDs must be free on the DR
  host.

## 3. Mapping suggestions

When a plan is created, the portal suggests mappings from the inventories:

- **Storage:** a DR ZFS storage with the same ID; otherwise the DR host's only
  ZFS storage; otherwise left for the user to choose.
- **Networks:** a DR bridge with the same name; otherwise the DR host's only
  VLAN-aware bridge without physical ports, or its only VLAN-aware bridge;
  otherwise left for the user to choose.
- **Test failover bridge:** a DR bridge with no physical ports that isn't used
  by the network mapping, if there is exactly one.

Suggestions are always shown for review, never applied silently.

## 4. Where replicas are stored

zrepl receives each source dataset under a **receive dataset** on the DR host,
keeping the source path: `<receive dataset>/<source dataset>`. For example,
with receive dataset `tank-dr/ezdr/pve1`, the source zvol
`rpool/data/vm-201-disk-0` is received as
`tank-dr/ezdr/pve1/rpool/data/vm-201-disk-0`.

- The default receive dataset is `<DR storage's pool>/ezdr/<primary node name>`.
  Replicas stay separate from the DR host's own guests, and several primaries
  can share one DR pool.
- It can be changed per storage mapping, for example to match an existing
  zrepl job's `root_fs` (section 8).
- How replicas become usable Proxmox volumes at failover (by clone for test
  failover, by rename or a dedicated Proxmox storage for real failover) is
  designed in phases 5 and 6.

## 5. Schedule and retention

- **Interval:** from 1 minute to 24 hours. The default is 15 minutes.
- **Retention** is a list of tiers, in zrepl's grid form. Each tier keeps one
  snapshot per period for a number of periods; the first tier can keep every
  snapshot. The primary and DR host each have their own list.

| Preset | DR host keeps | zrepl grid |
| --- | --- | --- |
| Balanced (default) | Every snapshot for 24 hours, daily for 14 days, weekly for 8 weeks | `1x24h(keep=all) \| 14x1d \| 8x7d` |
| Hourly and daily | Every snapshot for 1 hour, hourly for 24 hours, daily for 14 days | `1x1h(keep=all) \| 24x1h \| 14x1d` |
| Minimal | Every snapshot for 24 hours, daily for 7 days | `1x24h(keep=all) \| 7x1d` |
| Extended | Balanced, plus monthly for 12 months | `1x24h(keep=all) \| 14x1d \| 8x7d \| 12x30d` |

- The primary's default is every snapshot for 24 hours (`1x24h(keep=all)`).
  zrepl keeps bookmarks of the last replicated snapshot, so incremental sends
  keep working even after the primary prunes.
- Presets fill in the tiers, which can then be edited freely.
- Retention only ever applies to snapshots with the plan's prefix. EZDR never
  prunes other snapshots, such as Proxmox snapshots or manual ones, on either
  host, and never prunes a primary snapshot that hasn't been replicated yet
  (zrepl's `not_replicated` rule). These rules are always added and can't be
  turned off.

## 6. Startup order

- When a guest is added, its order and delay come from its Proxmox `startup`
  setting (`order=N,up=S`). Guests without one are placed after the others,
  in VMID order.
- The plan's order can be changed without touching the primary's Proxmox
  configuration.
- At failover (phase 6), guests start in ascending order, waiting each guest's
  delay before starting the next. Guests with the same order start together.

## 7. DNS records

Each protected guest can have DNS records, entered by hand in phase 3:

| Field | Example |
| --- | --- |
| Name | `app.example.com` |
| Type | `A`, `AAAA`, `CNAME`, or `TXT` |
| Production value | `203.0.113.10` |
| Failover value | `198.51.100.10` |

Phase 6 connects to Cloudflare, checks records and TTLs, and switches them
during failover and failback.

## 8. Adopting an existing zrepl setup

Hosts may already replicate with a hand-written zrepl configuration. Taking
over should not resend everything. The plan supports this with two advanced
settings:

- **Receive dataset** per storage mapping, set to the existing job's
  `root_fs`, so existing replicas are reused.
- **Snapshot prefix**, set to the existing job's prefix (for example,
  `zrepl_`), so existing snapshots count toward retention and serve as the
  base for incremental sends. The default prefix is `ezdr_`.

Phase 4 designs the takeover itself: detecting an existing zrepl
configuration, replacing it safely, and verifying the first incremental sync.
Known requirements:

- zrepl names its replication cursors and holds after the job. When EZDR's
  jobs replace existing ones, the old job's holds must be released, or they
  would keep old snapshots forever.
- Send options must stay consistent with the existing chain. EZDR's defaults
  are compressed, large-block, and embedded-data sends (raw sends for
  encrypted datasets); changing large-block settings mid-chain can break
  incremental receives.
- Existing jobs often replicate a whole pool with exclusions. EZDR replicates
  only the disks of protected guests, so other datasets under the pool stop
  being replicated after takeover. The takeover lists them first.

## 9. Validation

Validation runs whenever a plan is viewed or saved, against the latest
inventories of both hosts.

**Errors** (block activation):

- A protected guest no longer exists on the primary.
- A protected disk isn't replicable (for example, it's on LVM storage).
- A source storage or bridge used by protected guests isn't mapped, or its
  mapping names storage or a bridge that doesn't exist on the DR host.
- A DR storage in the mapping isn't ZFS storage.
- A protected VMID is already used by a guest on the DR host.
- The guest is protected by another plan.
- The retention lists are empty, or the primary's retention doesn't keep at
  least one snapshot interval.
- Either host has no inventory yet.

**Warnings**:

- The DR pool's free space is less than the protected disks' used space plus
  20%.
- A guest uses VLAN tags, but its mapped DR bridge isn't VLAN-aware.
- No test failover bridge is set, or it has physical ports.
- Readiness warnings from inventory (passthrough, ISO media).
- The primary has guests that aren't in any plan.
- Either host's inventory is more than 30 minutes old.

## 10. Portal storage and API

- **Storage:** a `plans` table holds each plan's specification as protobuf.
  A `plan_guests` table records (primary host, VMID) pairs with a uniqueness
  constraint, which enforces "one plan per guest". Removing a host removes
  plans that use it, after confirmation in the UI.
- **API:** `PlanService` with `ListPlans`, `GetPlan`, `CreatePlan`,
  `UpdatePlan`, `DeletePlan`, and `SuggestPlan` (proposed mappings and
  startup order for a host pair and set of guests). `GetPlan` returns the
  validation results with the plan.
- Creating, changing, and deleting plans are recorded in the audit log.

## 11. User interface

- The guest picker has a **Select all replicable** shortcut, for setups that
  protect everything except a few guests.
- A **Plans** page lists plans with their hosts, number of guests, interval,
  and validation state.
- The **plan editor** has sections for hosts, guests, mappings, schedule and
  retention, startup order, and DNS records, with validation results shown
  alongside and updated as the plan changes.
- Host detail pages show which plan protects each guest.

## 12. Phase 3 scope

In scope: everything above.

Out of scope: applying plans and replication (phase 4), test failover
(phase 5), failover and DNS switching (phase 6), Cloudflare integration
(phase 6), and taking over existing zrepl configurations (phase 4).

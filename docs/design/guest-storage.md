# Design: Guest storage figures

> **Status:** Approved. Covers showing how much storage each guest has
> allocated and uses, on the overview and the plan page. See
> [UI](ui.md) (sections 3.1 and 4) and [Inventory](inventory.md).

## 1. Summary

- Each guest shows its **allocated** size, the sum of its disks' configured
  sizes, as the main figure: in its row on the overview chart and in the
  plan page's guest list.
- Its details add the data it **uses on the primary** and the size of its
  **replica on the DR host**, snapshots included: in the row's tooltip and
  in the hover card of its line.
- Each plan section on the overview shows the plan's **total allocated**
  size, and the plan page's guest list a total of the selected guests.
- The hosts already report everything needed. Only the portal and the web
  UI change: no client update.

## 2. Figures

All three come from the inventories the portal stores.

| Figure | Source | Notes |
| --- | --- | --- |
| Allocated | Sum of `Disk.size_bytes` of the guest's disks | Known for every storage type. Disks without a size (passthrough devices, bind mounts) add nothing. |
| Used | Sum of `ZfsDataset.referenced_bytes` of the disks' datasets, on the guest's host | The live data after compression, without snapshots. ZFS disks only; when some disks aren't on ZFS, the figure says so ("ZFS disks only"). |
| On the DR host | Sum of `ZfsDataset.used_bytes` of the guest's replicas, found with `plan.ReplicaPaths` in the DR host's inventory | Includes the snapshots kept on the DR host: what the guest costs there. Shown for plans that aren't drafts; "not replicated yet" while a replica is missing. |

Allocated sizes change on the overview as soon as a disk is added or
resized (a structural change sends a new inventory). Used and replica sizes
can be up to about 15 minutes old: the inventory's hash excludes usage, so
clients send usage changes only with their periodic resend. The details
don't claim more precision than that ("about").

## 3. Overview

- **Guest rows:** a small muted size at the right of the row, before the
  gear, such as "32 GiB". The name truncates first. The same figure shows on
  the guest's row on the primary and on the DR host. Unconfigured,
  unprotected, and test-copy rows show theirs too.
- **Row tooltip:** a line such as "Disks: 32 GiB allocated · 12 GiB used ·
  about 18 GiB on the DR host".
- **Hover card** (a guest's line): rows "Allocated", "Used", and "On DR
  host".
- **Plan section header:** the plan's total allocated after its name, such
  as "Web and API · 96 GiB". The status label truncates first.

### API

- `OverviewGuest` gains `allocated_bytes`, `used_bytes`, and
  `used_zfs_only` (true when some sized disks aren't on ZFS), filled from
  the host's inventory for every guest.
- `OverviewGuestReplication` gains `replica_bytes` (0 while no replica
  exists), filled from the DR host's inventory.
- The chart computes section totals from the guest figures.

## 4. Plan page

The Guests section's table gets a **Size** column: the allocated size,
with the used size beneath it in muted text. Its description adds the
total of the selected guests, such as "4 of 12 guests on pve1 selected ·
96 GiB allocated". The page computes these from the primary's inventory it
already loads; the replica size stays on the overview.

## 5. Out of scope

- Replica sizes on the plan page (needs another request or an extended
  status response).
- Usage history or growth trends.
- Warnings when the DR host is short of space for a plan. A possible
  follow-up: compare the plan's total with the DR pool's free space in
  validation.

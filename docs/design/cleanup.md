# Design: Cleaning up replicated data

> **Status:** Approved. Covers deleting a plan's replicated data when the plan
> is deleted, removing what a takeover's old zrepl jobs left behind, and
> preventing two plans from sharing replica paths. See
> [Replication](replication.md) (section 5, takeover) and
> [DR plans](dr-plans.md).

## 1. Summary

- Deactivating or deleting a plan **never deletes data** today: replicas
  stay on the DR host, and the plan's snapshots, bookmarks, and zrepl holds
  stay on the primary, using space on both hosts indefinitely.
- The **Delete plan** dialog gets a checkbox, **"Also delete its replicated
  data"** (off by default). Checking it loads a preview of what would be
  removed on each host and how much space that frees. Deleting then runs the
  cleanup on both hosts before the plan's record is removed.
- A completed **takeover** page gets **"Clean up what the old jobs left"**:
  snapshots on datasets the old jobs replicated but the plan doesn't (such
  as the pool root and cloud-init volumes), and their stale replicas on the
  DR host. Also with a preview and a confirmation.
- Plan **validation blocks** replica paths that another plan also uses
  (different primaries, the same receive dataset, the same disk names), so a
  cleanup can never delete another plan's data.
- EZDR only ever deletes **snapshots with the plan's prefix and replicas
  under the plan's receive datasets**. The guests' live disks on the primary
  are never destroyed.

## 2. Deleting a plan's data

### 2.1 What is removed

| Host | Removed | Kept |
| --- | --- | --- |
| DR host | Each of the plan's replicas (`<receive dataset>/<source dataset>`) with its snapshots and zrepl holds; zrepl placeholder datasets left empty by that | The receive dataset itself, anything not under a replica path of the plan |
| Primary | On each of the plan's disks: snapshots and bookmarks whose names start with the plan's prefix, and zrepl's holds and cursor bookmarks for the plan's jobs | The disks themselves, snapshots with other names |

Holds are released with `zrepl zfs-abstraction release-all --job <job>` for
the plan's pull job (DR host) and source job (primary). The command works
for jobs that no longer exist.

### 2.2 Preconditions and safety checks

- The plan must be **deactivated** (draft), as for deleting today, so its
  jobs are gone and nothing replicates into the replicas. A plan with a
  running takeover can't be deleted (unchanged).
- **Both hosts must be online** when the deletion starts. If a host goes
  offline during it, the step waits and resends, like other runners.
- The preview, and each host again before deleting, **refuses a replica**
  that:
  - another plan maps to (equal path, or one nested in the other);
  - has clones (for example, a leftover test failover copy);
  - is used by a Proxmox storage or a guest configuration on the DR host.
- The primary skips snapshots that have holds EZDR didn't place (for
  example, a backup tool's) and reports them.
- Confirmation: the typed plan name, as for deleting today.

### 2.3 Flow

1. The dialog calls a preview RPC. The portal asks both hosts for the
   plan's datasets (new client action: exists, used space, space used by
   snapshots, matching snapshots and bookmarks, holds, clones, and whether
   a storage or guest uses them) and returns a list per host with the
   problems that would block deletion.
2. Deleting with the checkbox starts a runner with persisted steps, like
   the failover runners: **Release holds and delete snapshots on the
   primary**, **Delete replicas on the DR host**, **Delete the plan**. Each
   step is safe to repeat and resumes after a portal restart.
3. While it runs, the plan shows **"Deleting data"** and can't be edited or
   activated. If a step fails, the plan stays with the error and a
   **Retry** button; its data may be partly deleted.
4. Each host reports what it deleted; the audit log records it.

Without the checkbox, deleting works as today.

## 3. Takeover leftovers

The takeover preflight already stores the datasets the old jobs replicated
that the plan no longer does ("dropped datasets"). Once a takeover is
**completed**, its page offers **"Clean up what the old jobs left"**:

- **Primary:** on each dropped dataset, delete the snapshots and bookmarks
  with the old jobs' prefix. Datasets are never destroyed.
- **DR host:** for each dropped dataset's stale replica under the old
  `root_fs`, delete its snapshots. Delete the replica dataset itself only
  if it has no children (a replica of the pool root is the parent of every
  other replica, so only its snapshots go).
- **Skipped** (reported in the preview): a dataset that any remaining zrepl
  job on that host still covers, anything with clones, and anything used by
  a storage or guest.
- The same preview, confirmation, runner, and audit as section 2. Once it
  has run, the button is replaced with a summary of what was removed.

A failover that finds no cloud-init replica already creates a new
cloud-init volume, so removing stale cloud-init replicas is safe.

## 4. Validation: shared replica paths

- A plan's replica paths are `<receive dataset>/<source dataset>` for each
  of its disks. Validation reports an **error**, naming the other plan and
  the dataset, when a replica path equals or is nested in (either way) a
  replica path of another plan with the same DR host, or when a receive
  dataset of either plan lies at or under a replica path of the other.
  Sharing a receive dataset is fine as long as the replica paths differ.
- Two plans on the same primary already can't share disks (they can't share
  guests), so in practice this catches plans from different primaries with
  the same receive dataset and the same pool and disk names.
- Existing plans that collide show the error but keep running; the error
  blocks activating and applying changes, as other validation errors do.

## 5. Out of scope

- Finding replicas that no plan owns any more (for example, from plans
  deleted before this feature). Those are removed by hand.
- Deleting data when deactivating or pausing (only when deleting).
- Removing zrepl certificate files for peers no plan uses.
